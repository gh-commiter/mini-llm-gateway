import Foundation

/// `minigw gw labels eval --file <examples.jsonl> [--show-errors]` (Go: client/serve_labels_cli.go; shared cases in client/testdata/admin-commands.json).
/// The file is read here, the gateway classifies (the model lives there) and answers numbers; the example text never comes back: the misses are
/// listed from this process's own copy, only with --show-errors, only to this terminal.
extension AdminCommands {
    static let evalMaxFile = 1 << 20
    static let evalMaxExamples = 500
    static let evalShowLen = 120

    func labelsCommand(_ pos: [String], _ o: AdminOpts) throws {
        guard pos.count == 2, pos[1] == "eval", !o.file.isEmpty else { throw fail("minigw gw labels eval --file <examples.jsonl> [--show-errors]") }
        guard let handle = FileHandle(forReadingAtPath: o.file) else { throw fail("labels eval: cannot open the file") }
        defer { try? handle.close() }
        let data: Data
        do { data = try handle.read(upToCount: Self.evalMaxFile + 1) ?? Data() } catch { throw fail("labels eval: cannot read the file") }  // nil = an empty file
        if data.count > Self.evalMaxFile { throw fail("labels eval: the file is larger than 1 MiB") }
        guard String(data: data, encoding: .utf8) != nil else { throw fail("labels eval: the file is not valid UTF-8") }
        var examples: [[String: Any]] = []
        var lines: [Int] = []  // the file line of each example
        for (i, raw) in data.split(separator: 0x0a, omittingEmptySubsequences: false).enumerated() {
            let text = String(decoding: raw, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            if text.isEmpty { continue }
            if examples.count == Self.evalMaxExamples { throw fail("labels eval: at most \(Self.evalMaxExamples) examples") }
            let bad = fail("labels eval: line \(i + 1): one JSON object with text, phase, activity")
            guard !Self.trailingComma(text), let d = (try? JSONSerialization.jsonObject(with: Data(text.utf8))) as? [String: Any] else { throw bad }
            var f: [String: String] = [:]
            for (k, v) in d {
                guard ["text", "phase", "activity"].contains(k.lowercased()) else { throw bad }
                if v is NSNull { continue }
                guard let s = v as? String else { throw bad }
                f[k.lowercased()] = s
            }
            let (t, phase, activity) = (f["text"] ?? "", f["phase"] ?? "", f["activity"] ?? "")
            if t.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { throw fail("labels eval: line \(i + 1): text is required") }
            if phase.isEmpty && activity.isEmpty { throw fail("labels eval: line \(i + 1): needs a phase or an activity") }
            examples.append(["text": Decide.capInput(t), "phase": phase, "activity": activity])
            lines.append(i + 1)
        }
        if examples.isEmpty { throw fail("labels eval: the file has no examples") }
        let body: [String: Any] = ["examples": examples]
        if ProjectEnv.kbJSON(body).utf8.count > Self.evalMaxFile { throw fail("labels eval: the examples are larger than the 1 MiB the gateway takes: shorten the texts") }
        let r = try request("POST", "/usage/labels/eval", body)
        let e = r["eval"]?.dict ?? JDict()
        out(Self.evalText(e))
        let wrong = e["wrong"]?.array ?? []
        if !o.showErrors {
            if !wrong.isEmpty { say("misses: \(wrong.count) (--show-errors lists them with their text; the text goes to this terminal only)") }
            return
        }
        say("misses at 2 of 3 (file line, kind, hand label -> model):")
        for w in wrong {
            let d = w.dict
            let line = Int(d["line"]?.int ?? 0)
            guard line >= 1, line <= examples.count else { continue }
            let got = (d["got"]?.string ?? "").isEmpty ? "-" : d["got"]!.string!
            say(clean("  line \(lines[line - 1])  \(d["kind"]?.string ?? "")  \(d["expected"]?.string ?? "") -> \(got)"))
            say("    " + Self.evalSnippet(examples[line - 1]["text"] as? String ?? ""))
        }
        if wrong.count == 100 { say("  (the first 100)") }
    }

    /// Foundation accepts a comma before a closing bracket; Go's decoder (and so the gateway's own files) does not: one rule for both clients.
    static func trailingComma(_ text: String) -> Bool {
        var inString = false, escaped = false, comma = false
        for c in text.unicodeScalars {
            if inString {
                if escaped { escaped = false } else if c == "\\" { escaped = true } else if c == "\"" { inString = false }
                continue
            }
            switch c {
            case "\"": inString = true; comma = false
            case ",": comma = true
            case "}", "]": if comma { return true }
            case " ", "\t", "\n", "\r": break
            default: comma = false
            }
        }
        return false
    }

    /// One clean line of an example for the terminal.
    static func evalSnippet(_ text: String) -> String {
        let t = ProjectText.clean(text).split(whereSeparator: { $0.isWhitespace }).joined(separator: " ")
        let scalars = Array(t.unicodeScalars)
        return scalars.count > evalShowLen ? String(String.UnicodeScalarView(scalars.prefix(evalShowLen))) + "…" : t
    }

    static func evalPct(_ n: Int64, _ of: Int64) -> String { of == 0 ? "-" : "\((n * 100 + of / 2) / of)%" }

    /// The numbers, in the format of the Go client.
    static func evalText(_ e: JDict) -> String {
        var b = "\(ProjectText.clean(e["backend"]?.string ?? "")): \(e["examples"]?.int ?? 0) examples, \(e["run"]?.int ?? 0) run, \(e["notrun"]?.int ?? 0) not run\n"
        for name in ["phase", "activity"] {
            let k = e[name]?.dict ?? JDict()
            let scored = k["scored"]?.int ?? 0
            b += "\(name): \(scored) hand-labelled\n"
            for (label, key) in [("3 of 3 agree", "t10"), ("2 of 3 agree", "t067")] {
                let c = k[key]?.dict ?? JDict()
                let (decided, correct) = (c["decided"]?.int ?? 0, c["correct"]?.int ?? 0)
                b += "  \(label): coverage \(evalPct(decided, scored)) (\(decided) of \(scored)), accuracy \(evalPct(correct, decided)) (\(correct) of \(decided))\n"
            }
            let m = (e["mistakes"]?.dict[name]?.array) ?? []
            if !m.isEmpty {
                b += "  most frequent mistakes at 2 of 3 (hand label -> model):\n"
                for c in m {
                    let d = c.dict
                    b += "    \(ProjectText.clean(d["expected"]?.string ?? "")) -> \(ProjectText.clean(d["got"]?.string ?? ""))  \(d["n"]?.int ?? 0)\n"
                }
            }
        }
        return b + "This is agreement with your labels on these examples, not calibrated accuracy. Nothing was stored except one internal usage event for the model time.\n"
    }
}
