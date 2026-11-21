import Foundation

// minigw note history|restore (members) and minigw gw kb history|restore (the owner): saved versions of a note, and restoring one as a
// new revision. A port of client/note_history_cmd.go: both command families use this one function, as in Go. Every printed value goes
// through ProjectText.clean; the requests, the confirmation rules and the texts are the Go client's (fixture client/testdata/note-history.json).

extension ProjectEnv {
    static let historyTail = "some revisions are not listed: not kept (from before 0.8.6, or dropped from a long series of edits) or not readable by you"
    static let restoreNeedsCurrent = "not restored: without a terminal, name the current revision: --current <n>"

    /// A time of these commands, to the minute, always with its zone: UTC, or the offset from it (Go's historyTime).
    static func historyTime(_ d: Date?, _ zone: TimeZone) -> String {
        guard let d else { return "0001-01-01 00:00 UTC" }
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX"); f.timeZone = zone; f.dateFormat = "yyyy-MM-dd HH:mm"
        let off = zone.secondsFromGMT(for: d)
        if off == 0 { return f.string(from: d) + " UTC" }
        let a = abs(off)
        return f.string(from: d) + String(format: " %@%02d:%02d", off < 0 ? "-" : "+", a / 3600, a % 3600 / 60)
    }

    /// written <time> by <who>  replaced <time> by <who>: written is when this text became the note's text, replaced when something else did
    /// (the names are the ones the gateway gave).
    static func versionWho(_ v: JDict, _ zone: TimeZone) -> String {
        func time(_ k: String) -> String { historyTime(v[k]?.string.flatMap(ProjectText.parseTime), zone) }
        func word(_ k: String, _ none: String) -> String { let s = v[k]?.string ?? ""; return s.isEmpty ? none : s }
        return "written \(time("updated")) by \(word("writtenBy", "unknown"))  replaced \(time("saved")) by \(word("replacedBy", "a review or import"))"
    }

    static let forbiddenRoleText = "not allowed: your role in this project does not permit it (a grant alone only lets you read; linking needs a direct role)"

    /// `request` sends one call and returns the answer's body, or throws the gateway's error as it should be printed.
    static func noteHistoryCommand(_ sub: String, _ path: String, id: Int64, revision: Int64, hasRevision: Bool, current: Int64, hasCurrent: Bool, cls: String, asJSON: Bool,
                                   terminal: Bool, out: (String) -> Void, answer: () -> String?, zone: TimeZone,
                                   request: (String, String, Data?) throws -> Data) throws {
        func say(_ s: String) { out(ProjectText.clean(s) + "\n") }
        func json(_ d: Data) { say(ProjectText.jsonText(ProjectText.trim(String(decoding: d, as: UTF8.self)))) }
        if hasRevision && revision < 1 || sub == "restore" && !hasRevision { throw ProjectError("--revision must be positive") }
        if hasCurrent && current < 1 { throw ProjectError("--current must be positive") }
        if sub == "history" && !hasRevision {
            let data = try request("GET", path + "/history", nil)
            if asJSON { json(data); return }
            let h = JSONValue.parse(data)?.dict ?? JDict()
            let n = h["note"]?.dict ?? JDict()
            say("note \(n["id"]?.int ?? 0)  revision \(n["revision"]?.int ?? 0) (current)  \(n["class"]?.string ?? "")  \(ProjectText.oneLine(n["title"]?.string ?? ""))")
            let rows = h["history"]?.array ?? []
            for r in rows {
                let v = r.dict
                say("revision \(v["revision"]?.int ?? 0)  \(versionWho(v, zone))  \(v["class"]?.string ?? "")  \(v["entryKind"]?.string ?? "")  \(v["bytes"]?.int ?? 0) bytes  \(ProjectText.oneLine(v["title"]?.string ?? ""))")
            }
            if rows.isEmpty { say("no saved revisions you may read") }
            // every saved revision lies below the current one: the list is complete when it holds all of 1..current-1
            if Int64(rows.count) != (n["revision"]?.int ?? 0) - 1 { say(historyTail) }
            return
        }
        let cur = NoteView(JSONValue.parse(try request("GET", path, nil))?.dict ?? JDict())
        if revision == cur.revision {
            if sub == "restore" { throw ProjectError("revision \(revision) is the current text: nothing to restore") }
            if path.hasPrefix("/kb/") { throw ProjectError("revision \(revision) is the current text, not a saved version") }
            throw ProjectError("revision \(revision) is the current text: \(ProjectEnv.mg) note read \(id)")
        }
        let data = try request("GET", path + "/history/" + String(revision), nil)
        if sub == "history" && asJSON { json(data); return }
        let v = JSONValue.parse(data)?.dict ?? JDict()
        let vClass = v["class"]?.string ?? ""
        if sub == "history" {
            say("note \(id)  revision \(v["revision"]?.int ?? 0) of \(cur.revision)  \(vClass)  \(v["entryKind"]?.string ?? "")  \(versionWho(v, zone))")
            say(ProjectText.oneLine(v["title"]?.string ?? ""))
            let body = v["body"]?.string ?? ""
            out(ProjectText.clean(body.replacingOccurrences(of: "\t", with: "    ")))
            if !body.hasSuffix("\n") { say("") }
            return
        }
        let target = cls.isEmpty ? cur.cls : cls
        if target != cur.cls && target != vClass { throw ProjectError("class must be the current or saved class") }
        let expected = hasCurrent ? current : cur.revision
        // the plan is printed where it is a question (before the prompt, or before the refusal naming the current revision) and with
        // the success: never before a request that is refused, so it cannot read as if it happened
        func plan() {
            say("restore note \(id) to the text of revision \(revision) (now revision \(cur.revision), becomes \(cur.revision + 1))")
            say(target == cur.cls ? "class: " + target + " (kept)" : "class: " + cur.cls + " -> " + target)
        }
        if !hasCurrent {
            plan()
            if !terminal { throw ProjectError(restoreNeedsCurrent) }
            out("Restore? [y/N] ")
            let a = (answer() ?? "").trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
            if a != "y" && a != "yes" { throw ProjectError("not restored") }
        }
        let payload = Data("{\"revision\":\(revision),\"expectedRevision\":\(expected),\"class\":\(GoJSON.string(target))}".utf8)
        let data2: Data
        do { data2 = try request("POST", path + "/restore", payload) } catch let e as ProjectError where e.gatewayStatus == 403 && e.message == forbiddenRoleText {
            throw ProjectError("not allowed: restoring a saved version needs the project admin role (or the gateway owner)")
        }
        if hasCurrent { plan() }
        let n = NoteView(JSONValue.parse(data2)?.dict ?? JDict())
        var line = "restored revision \(revision) as revision \(n.revision); the note is \(n.cls)"
        if target != cur.cls { line += " (class changed from \(cur.cls) to \(target))" }
        else if vClass != cur.cls { line += " (current class kept; use --class \(vClass) to restore the saved class)" }
        say(line)
    }
}
