import Foundation
import Testing
@testable import RouterCore

// The Mac client's owner commands (minigw gw user|bind|project|member|grant|core|usage-events|quota), proven against the Go client: the
// shared fixture client/testdata/admin-commands.json is asserted here and in client/admin_parity_test.go. Everything is fake: the
// gateway's answers are the fixture's, nothing touches the app, the keychain or a real folder.

private let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()

private func fixtureJSON(_ name: String) throws -> [String: Any] {
    try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("client/testdata/" + name))) as! [String: Any]
}

@Suite struct AdminCommandParity {
    @Test func everyCommandMatchesTheGoOutput() throws {
        let cases = try fixtureJSON("admin-commands.json")["cases"] as! [[String: Any]]
        #expect(cases.count >= 60)
        for c in cases {
            let name = c["name"] as! String
            let responses = c["responses"] as! [[String: Any]]
            nonisolated(unsafe) var requests: [[String]] = []
            nonisolated(unsafe) var outText = ""
            let cmd = AdminCommands(call: { method, path, body in
                requests.append([method, path, body.map { String(decoding: $0, as: UTF8.self) } ?? ""])
                guard let r = responses.first(where: { ($0["match"] as! String) == method + " " + path }) else {
                    return (599, Data(#"{"error":{"message":"no such response in the fixture"}}"#.utf8))
                }
                return (r["status"] as! Int, Data((r["body"] as! String).utf8))
            }, out: { outText += $0 })
            cmd.zone = TimeZone(identifier: "UTC")!
            let o = c["opts"] as! [String: Any]
            func str(_ k: String) -> String? { o[k] as? String }
            var exit: Int32 = 0, errText = ""
            do {
                var opts = AdminOpts(days: str("days") ?? "7", user: str("user") ?? "", project: str("project") ?? "",
                                     model: str("model") ?? "", claudeProbe: str("claudeProbe") ?? "", team: str("team") ?? "",
                                     phase: str("phase") ?? "", activity: str("activity") ?? "", task: str("task") ?? "", labelSource: str("labelSource") ?? "")
                opts.showErrors = o["showErrors"] as? Bool ?? false
                if let content = c["file"] as? String {  // labels eval: the examples file
                    let dir = FileManager.default.temporaryDirectory.appendingPathComponent("admin-case-" + UUID().uuidString)
                    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
                    opts.file = dir.appendingPathComponent("examples.jsonl").path
                    try Data(content.utf8).write(to: URL(fileURLWithPath: opts.file))
                }
                try cmd.run(c["args"] as! [String], opts)
            } catch let e as AdminCLIError { exit = e.code; errText = "minigw: " + e.message + "\n" }
            let want = c["want"] as! [String: Any]
            #expect(outText == want["out"] as! String, "\(name): stdout")
            #expect(errText == want["err"] as! String, "\(name): stderr")
            #expect(Int(exit) == want["exit"] as! Int, "\(name): exit")
            #expect(requests == want["requests"] as! [[String]], "\(name): requests")
        }
    }

    /// gw kb history and restore: the "owner" cases of client/testdata/note-history.json (the Go client's output, byte for byte).
    @Test func ownerNoteHistoryAndRestoreMatchTheGoOutput() throws {
        let cases = try fixtureJSON("note-history.json")["owner"] as! [[String: Any]]
        #expect(cases.count >= 10)
        for c in cases {
            let name = c["name"] as! String
            let responses = c["responses"] as! [[String: Any]]
            nonisolated(unsafe) var requests: [[String]] = []
            nonisolated(unsafe) var outText = ""
            let cmd = AdminCommands(call: { method, path, body in
                requests.append([method, path, body.map { String(decoding: $0, as: UTF8.self) } ?? ""])
                guard let r = responses.first(where: { ($0["match"] as! String) == method + " " + path }) else {
                    return (599, Data(#"{"error":{"message":"no such response in the fixture"}}"#.utf8))
                }
                return (r["status"] as! Int, Data((r["body"] as! String).utf8))
            }, out: { outText += $0 })
            cmd.zone = TimeZone(identifier: "UTC")!
            let o = c["opts"] as! [String: Any]
            cmd.isTerminal = { o["tty"] as? Bool == true }
            nonisolated(unsafe) var answers = (c["stdin"] as? String ?? "").split(separator: "\n", omittingEmptySubsequences: false).map(String.init)
            cmd.readAnswer = { answers.isEmpty ? nil : answers.removeFirst() }
            var opts = AdminOpts()
            opts.json = o["json"] as? Bool ?? false
            opts.revision = (o["revision"] as? NSNumber)?.int64Value ?? 0
            opts.current = (o["current"] as? NSNumber)?.int64Value ?? 0
            opts.hasCurrent = o["current"] != nil
            opts.classes = o["class"] as? String ?? ""
            var exit: Int32 = 0, errText = ""
            do { try cmd.run(c["args"] as! [String], opts) } catch let e as AdminCLIError { exit = e.code; errText = "minigw: " + e.message + "\n" }
            let want = c["want"] as! [String: Any]
            #expect(outText == want["out"] as! String, "\(name): stdout")
            #expect(errText == want["err"] as! String, "\(name): stderr")
            #expect(Int(exit) == want["exit"] as! Int, "\(name): exit")
            #expect(requests == want["requests"] as! [[String]], "\(name): requests")
        }
        // the owner's commands take neither --team nor --project
        for team in [true, false] {
            nonisolated(unsafe) var sent = 0
            let cmd = AdminCommands(call: { _, _, _ in sent += 1; return (599, Data()) }, out: { _ in })
            var opts = AdminOpts()
            if team { opts.team = "garden" } else { opts.project = "orchard" }
            do { try cmd.run(["kb", "history", "7"], opts); Issue.record("no usage error") } catch let e as AdminCLIError { #expect(e.code == 1 && e.message.contains("usage:")) }
            #expect(sent == 0)
        }
    }

    /// labels eval: the file's own limits are checked before anything is sent, and an error names the line, never its text.
    @Test func labelsEvalChecksTheFileBeforeSendingAnything() throws {
        let marker = "LINE-TEXT-MARKER-3321"
        let line = #"{"text":"alpha","phase":"do"}"# + "\n"
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("labels-eval-" + UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        func file(_ content: Data) throws -> String {
            let p = dir.appendingPathComponent(UUID().uuidString).path
            try content.write(to: URL(fileURLWithPath: p))
            return p
        }
        let cases: [(String, Data, String)] = [
            ("file over 1 MiB", Data((String(repeating: " ", count: 1 << 20) + line).utf8), "the file is larger than 1 MiB"),
            ("over 500 lines", Data(String(repeating: line, count: 501).utf8), "at most 500 examples"),
            ("bad line", Data((line + #"{"text":"\#(marker)","phase":"do",}"# + "\n").utf8), "line 2: one JSON object"),
            ("unknown key", Data((#"{"text":"\#(marker)","phase":"do","note":1}"# + "\n").utf8), "line 1: one JSON object"),
            ("no text", Data((#"{"phase":"do"}"# + "\n").utf8), "line 1: text is required"),
            ("no label", Data((line + #"{"text":"\#(marker)"}"# + "\n").utf8), "line 2: needs a phase or an activity"),
            ("invalid UTF-8", Data(line.utf8) + Data([0x7b, 0x22, 0x74, 0x22, 0x3a, 0x22, 0xff, 0x22, 0x7d, 0x0a]), "not valid UTF-8"),
            ("empty", Data("\n\n".utf8), "no examples"),
            ("empty file", Data(), "no examples"),
            ("examples over 1 MiB", Data(String(repeating: #"{"text":"\#(String(repeating: "<", count: 1990))","phase":"do"}"# + "\n", count: 500).utf8), "larger than the 1 MiB")]
        for (name, content, want) in cases {
            nonisolated(unsafe) var sent = 0
            nonisolated(unsafe) var outText = ""
            let cmd = AdminCommands(call: { _, _, _ in sent += 1; return (599, Data("{}".utf8)) }, out: { outText += $0 })
            var o = AdminOpts()
            o.file = try file(content)
            do { try cmd.run(["labels", "eval"], o); Issue.record("\(name): no error") } catch let e as AdminCLIError {
                #expect(e.code == 1 && e.message.contains(want) && !e.message.contains(marker) && !outText.contains(marker), "\(name): \(e.message)")
            }
            #expect(sent == 0, "\(name): something was sent")
        }
        for (o, want) in [(AdminOpts(), "--file"), ({ var o = AdminOpts(); o.file = dir.appendingPathComponent("missing").path; return o }(), "cannot open the file")] {
            let cmd = AdminCommands(call: { _, _, _ in (599, Data()) }, out: { _ in })
            do { try cmd.run(["labels", "eval"], o); Issue.record("no error") } catch let e as AdminCLIError { #expect(e.message.contains(want)) }
        }
    }

    @Test func theDefaultBackupNameIsAUTCStamp() {
        nonisolated(unsafe) var body = ""
        let cmd = AdminCommands(call: { _, _, b in body = String(decoding: b ?? Data(), as: UTF8.self); return (200, Data(#"{"file":"/x/core.db"}"#.utf8)) }, out: { _ in })
        cmd.now = { Date(timeIntervalSince1970: 1_700_000_000) }
        try? cmd.run(["core", "backup"], AdminOpts())
        #expect(body == #"{"name":"core-20231114T221320Z.db"}"#)
    }

    @Test func goFloatsAndMaps() {
        #expect(GoText.g(1_000_000) == "1e+06")
        #expect(GoText.g(123456) == "123456")
        #expect(GoText.g(1234567.5) == "1.2345675e+06")
        #expect(GoText.g(2.5) == "2.5")
        #expect(GoText.g(0.0001) == "0.0001")
        #expect(GoText.g(0.00001) == "1e-05")
        #expect(GoText.f(1e21) == "1000000000000000000000")
        #expect(GoText.f(0.5) == "0.5")
        #expect(GoText.f(100) == "100")
        #expect(GoText.v(["b": 1, "a": [true, "x"]] as [String: Any]) == "map[a:[true x] b:1]")
    }

    @Test func aDeviceIDIsTheKeyEncodedAsGoChecksIt() {
        #expect(AdminCommands.validDeviceID(String(repeating: "A", count: 43)))
        #expect(!AdminCommands.validDeviceID(String(repeating: "A", count: 42) + "B"))  // spare bits set
        #expect(!AdminCommands.validDeviceID("short"))
    }
}

@Suite struct CoreRestoreOffline {
    @Test func refusedWhileTheAppRuns() {
        let why = CoreRestore.check(appRunning: true)
        #expect(why?.contains("quit the app first") == true && why?.contains("Nothing was changed") == true)
        #expect(CoreRestore.check(appRunning: false) == nil)
    }

    @Test func theHelperIsStartedInRestoreModeOnly() {
        let a = CoreRestore.argv(binary: "/h/minigw-core", store: "/s", file: "b.db", yes: true)
        #expect(a == ["/h/minigw-core", "restore", "-store", "/s", "-yes", "--", "b.db"])  // never the socket mode: no -store before "restore"
        #expect(CoreRestore.argv(binary: "/h/minigw-core", store: "/s", file: "-x", yes: false) == ["/h/minigw-core", "restore", "-store", "/s", "--", "-x"])
    }
}
