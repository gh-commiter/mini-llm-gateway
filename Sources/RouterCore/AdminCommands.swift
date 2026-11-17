import Foundation

// minigw gw user|bind|project|member|grant|core backup|core audit|usage|usage-events: the owner's commands of the 0.6.0 core, thin calls of
// the admin API (the app forwards the core's routes to the helper). A port of client/serve_core_cli.go: the same arguments, help,
// output, requests and exit codes; client/testdata/admin-commands.json is asserted here and in client/admin_parity_test.go.
// The glue that connects to the app is Sources/MiniGW/RouterCommand.swift.

/// A command that ends with `minigw: <message>` and the exit code.
public struct AdminCLIError: Error, Equatable {
    public var message: String
    public var code: Int32
    public init(_ message: String, code: Int32 = 1) { self.message = message; self.code = code }
}

/// The flags of the owner commands (Go's coreOpts).
public struct AdminOpts {
    public var reason = "", team = ""
    public var days = "7", user = "", project = "", model = "", claudeProbe = "", provider = "", device = "", folder = "", sort = ""
    public var json = false, csv = false, history = false, explain = false
    public init(days: String = "7", user: String = "", project: String = "", model: String = "", claudeProbe: String = "",
                provider: String = "", device: String = "", folder: String = "", sort: String = "", json: Bool = false, csv: Bool = false, history: Bool = false, explain: Bool = false, reason: String = "", team: String = "") {
        self.reason = reason; self.team = team
        self.days = days; self.user = user; self.project = project; self.model = model; self.claudeProbe = claudeProbe
        self.provider = provider; self.device = device; self.folder = folder; self.sort = sort; self.json = json; self.csv = csv; self.history = history; self.explain = explain
    }
}

public final class AdminCommands {
    public static let help = """
        minigw gw export --team <name> [--project slug ...] [--with-private] <file.zip>
          minigw gw import --source <name> <file.zip> [--dry-run] [--map old=new]
          minigw gw kb review [<project>|--team <team>] | accept <id> | reject <id> [--reason text]
          minigw gw user list | add <name> | rename <user> <name> | activate <user> | deactivate <user>
          minigw gw bind list | <device id> <user> | remove <device id>        which user a device belongs to
          minigw gw project list | add <slug> <name> | archive <project> | unarchive <project>
          minigw gw project remote list <project> | add <project> <url> | remove <project> <url>   git remotes registered for a project
          minigw gw team add <name> | list | rename <team> <name> | archive <team>
          minigw gw team path <team> <label>                    free-text path label of a team ("-" clears it)
          minigw gw team member add|remove <team> <user>
          minigw gw team project add|remove <team> <project>
          minigw gw team access set <team> <project> <user> <role> | remove <team> <project> <user> | list <team>
          minigw gw member <project> list [--explain] | set <user> <role> | remove <user>   role: reader, developer, admin
          minigw gw grant <source> list | add <reader> <class> | remove <reader> <class>   class: structure, data
          minigw gw core backup [name]                          a consistent copy of the core database into <config>/core/backups (gateway running)
          minigw gw core restore <file|name> [--yes]            only with the gateway stopped; a name is looked up in <config>/core/backups
          minigw gw core audit                                  the audit trail of the core
          minigw gw project-sessions --project <p> [--days n] [--model m] [--device d] [--json]
          minigw gw usage-events [--days n] [--user u] [--project p] [--model m]   model invocations per day, model and user (core; all of them)
          minigw gw quota [--claude-probe=on|off]               subscription quota and provider credit, with source, time and staleness
          minigw gw quota --history [--days N] [--provider P] [--json]   recorded probes (default 7 days; no probe)
        (<user> and <project>: the name or slug, or the number.)
        """

    /// status 0 = no answer.
    public typealias Call = (_ method: String, _ path: String, _ body: Data?) -> (status: Int, data: Data)

    let call: Call
    let out: (String) -> Void
    public var zone = TimeZone.current
    public var now: () -> Date = { Date() }

    public init(call: @escaping Call, out: @escaping (String) -> Void) { self.call = call; self.out = out }

    func say(_ s: String) { out(s + "\n") }
    func fail(_ s: String) -> AdminCLIError { AdminCLIError(s) }
    func clean(_ s: String) -> String { ProjectText.clean(s) }

    /// One admin request; an error status ends the command with the API's message (Go's adminClient.do).
    @discardableResult
    func request(_ method: String, _ path: String, _ body: [String: Any]? = nil) throws -> JDict {
        let r = call(method, path, body.map { Data(ProjectEnv.kbJSON($0).utf8) })
        guard r.status != 0 else { throw fail("lost the connection to Mini Gateway") }
        if r.status >= 300 { throw fail(clean(JSONValue.parse(r.data)?.dict["error"]?.dict["message"]?.string ?? "")) }
        return JSONValue.parse(r.data)?.dict ?? JDict()
    }

    /// The answer as plain Foundation objects (keys as they are).
    func requestRaw(_ method: String, _ path: String) throws -> [String: Any] {
        let r = call(method, path, nil)
        guard r.status != 0 else { throw fail("lost the connection to Mini Gateway") }
        if r.status >= 300 { throw fail(clean(JSONValue.parse(r.data)?.dict["error"]?.dict["message"]?.string ?? "")) }
        return (try? JSONSerialization.jsonObject(with: r.data) as? [String: Any]) ?? [:]
    }

    /// A key as Go's decoding finds it (any case).
    func ci(_ d: [String: Any], _ k: String) -> Any? { d.first { $0.key.caseInsensitiveCompare(k) == .orderedSame }?.value }

    func userID(_ ref: String) throws -> Int64 {
        let n = Int64(ref) ?? 0
        for u in try request("GET", "/users")["users"]?.array ?? [] {
            let d = u.dict
            if d["id"]?.int ?? 0 == n || (d["name"]?.string ?? "").caseInsensitiveCompare(ref) == .orderedSame { return d["id"]?.int ?? 0 }
        }
        throw fail("no user " + GoText.quote(ref) + " (minigw gw user list)")
    }

    func projectID(_ ref: String) throws -> Int64 {
        let n = Int64(ref) ?? 0
        for p in try request("GET", "/projects")["projects"]?.array ?? [] {
            let d = p.dict
            if d["id"]?.int ?? 0 == n || d["slug"]?.string == ref { return d["id"]?.int ?? 0 }
        }
        throw fail("no project " + GoText.quote(ref) + " (minigw gw project list)")
    }

    public func run(_ pos: [String], _ o: AdminOpts) throws {
        let usage = fail(Self.help.trimmingCharacters(in: .whitespacesAndNewlines))
        func need(_ lo: Int, _ hi: Int) throws { if pos.count < lo || pos.count > hi { throw usage } }
        func s(_ n: Int64) -> String { String(n) }
        switch pos[0] {
        case "kb": try kb(pos, reason: o.reason, team: o.team)
        case "team": try team(pos)
        case "user":
            try need(2, 4)
            switch (pos[1], pos.count) {
            case ("list", 2):
                for u in try request("GET", "/users")["users"]?.array ?? [] {
                    let d = u.dict
                    var state = d["active"]?.bool == true ? "" : "  (deactivated)"
                    if d["owner"]?.bool == true { state += "  (owner)" }
                    say(clean("\(d["id"]?.int ?? 0)  \(d["name"]?.string ?? "")\(state)"))
                }
            case ("add", 3):
                try request("POST", "/users", ["name": pos[2]])
                say("user " + clean(pos[2]) + " added")
            case ("rename", 4):
                try request("PATCH", "/users/" + s(try userID(pos[2])), ["name": pos[3]])
            case ("activate", 3), ("deactivate", 3):
                try request("PATCH", "/users/" + s(try userID(pos[2])), ["active": pos[1] == "activate"])
                if pos[1] == "deactivate" { say("deactivated: the user's runs and capabilities end (the epoch grew)") }
            default: throw usage
            }
        case "bind":
            switch (pos.count, pos[1...].first ?? "") {
            case (2, "list"):
                for b in try request("GET", "/bindings")["bindings"]?.array ?? [] {
                    say("\(b.dict["device"]?.string ?? "")  user \(b.dict["user"]?.int ?? 0)")
                }
            case (3, "remove"):
                try request("DELETE", "/bindings/" + ProjectText.pathEscape(pos[2]))
            case (3, _) where Self.validDeviceID(pos[1]):
                try request("PUT", "/bindings/" + ProjectText.pathEscape(pos[1]), ["user": try userID(pos[2])])
                let listed = try request("GET", "/devices")["devices"]?.array ?? []
                // A device may be bound before the owner allows it; without the list entry projects still refuse.
                if !listed.contains(where: { $0.dict["id"]?.string == pos[1] }) {
                    say("warning: this device is not on the device list yet: minigw gw devices allow " + clean(pos[1]))
                }
            default: throw usage
            }
        case "project":
            try need(2, 5)
            switch (pos[1], pos.count) {
            case ("list", 2):
                for p in try request("GET", "/projects")["projects"]?.array ?? [] {
                    let d = p.dict
                    say(clean("\(d["id"]?.int ?? 0)  \(d["slug"]?.string ?? "")  \(d["name"]?.string ?? "")\(d["archived"]?.bool == true ? "  (archived)" : "")"))
                }
            case ("add", 4):
                try request("POST", "/projects", ["slug": pos[2], "name": pos[3]])
                say("project " + clean(pos[2]) + " added")
            case ("archive", 3), ("unarchive", 3):
                try request("PATCH", "/projects/" + s(try projectID(pos[2])), ["archived": pos[1] == "archive"])
            case ("remote", 4) where pos[2] == "list", ("remote", 5) where pos[2] == "add" || pos[2] == "remove":
                let path = "/projects/" + s(try projectID(pos[3])) + "/remotes"
                if pos[2] == "list" {
                    let list = try request("GET", path)["remotes"]?.array.compactMap(\.string) ?? []
                    for v in list { say(clean(v)) }
                    if list.isEmpty { say("no remotes registered") }
                    break
                }
                let r = try request("POST", path, [pos[2]: [ProjectEnv.stripUserinfo(pos[4])]])
                let named = r[pos[2] == "add" ? "added" : "removed"]?.array.compactMap(\.string) ?? []
                guard named.count == 1 else { throw fail("the gateway did not name the remote") }
                say(clean(pos[2] == "add" ? "registered \(named[0]) for project \(pos[3])" : "removed \(named[0]) from project \(pos[3])"))
            default: throw usage
            }
        case "member":
            try need(3, 5)
            let p = s(try projectID(pos[1]))
            switch (pos[2], pos.count) {
            case ("list", 3):
                for m in try request("GET", "/projects/" + p + "/members")["members"]?.array ?? [] {
                    let d = m.dict
                    say(clean("\(d["user"]?.int ?? 0)  \(d["name"]?.string ?? "")  \(d["role"]?.string ?? "")"))
                    if o.explain { say(clean("  origins: " + ProjectEnv.originText(d["origins"]?.array ?? [], roles: true))) }
                }
            case ("set", 5):
                memberFeedback(try request("PUT", "/projects/" + p + "/members/" + s(try userID(pos[3])), ["role": pos[4]]), direct: pos[4])
            case ("remove", 4):
                memberFeedback(try request("DELETE", "/projects/" + p + "/members/" + s(try userID(pos[3]))), direct: "")
            default: throw usage
            }
        case "grant":
            try need(3, 5)
            let src = try projectID(pos[1])
            switch (pos[2], pos.count) {
            case ("list", 3):
                for g in try request("GET", "/grants")["grants"]?.array ?? [] where (g.dict["source"]?.int ?? 0) == src {
                    say("project \(g.dict["reader"]?.int ?? 0) reads \(g.dict["class"]?.string ?? "") of \(pos[1])")
                }
            case ("add", 5):
                try request("POST", "/grants", ["source": src, "reader": try projectID(pos[3]), "class": pos[4]])
            case ("remove", 5):
                try request("DELETE", "/grants?source=\(src)&reader=\(try projectID(pos[3]))&class=" + ProjectText.queryEscape(pos[4]))
            default: throw usage
            }
        case "usage":  // the report of the new events (the folder and device counters: usage --legacy)
            guard pos.count == 1, !(o.json && o.csv) else { throw fail(UsageReport.help.trimmingCharacters(in: .whitespacesAndNewlines)) }
            if !o.device.isEmpty || !o.folder.isEmpty { throw fail("--device and --folder belong to the old counters: use minigw gw usage --legacy") }
            if !o.user.isEmpty || !o.project.isEmpty { throw fail("--user and --project belong to minigw gw usage-events") }
            if !o.sort.isEmpty && (o.sort != "failed" || o.json || o.csv) { throw fail(UsageReport.help.trimmingCharacters(in: .whitespacesAndNewlines)) }
            var q = ["days": o.days]
            if !o.provider.isEmpty { q["provider"] = o.provider }
            if !o.model.isEmpty { q["model"] = o.model }
            let r = call("GET", "/usage/events" + ProjectText.query(q), nil)
            guard r.status != 0 else { throw fail("lost the connection to Mini Gateway") }
            if r.status >= 300 { throw fail(clean(JSONValue.parse(r.data)?.dict["error"]?.dict["message"]?.string ?? "")) }
            if o.json { say(String(decoding: r.data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)); break }
            guard let (rows, gaps) = UsageReport.rows(r.data) else { throw fail("the usage answer is not understood") }
            out(o.csv ? UsageReport.csv(rows) : UsageReport.text(rows, gaps, sortFailed: o.sort == "failed"))
        case "project-sessions":
            try need(1, 1)
            guard !o.project.isEmpty else { throw fail("--project required") }
            var q = ["project": s(try projectID(o.project)), "days": o.days, "model": o.model, "device": o.device]
            let page = try SessionHistory.collect { after in
                q["after"] = after
                return try self.requestRaw("GET", "/usage/sessions" + ProjectText.query(q))
            }
            out(o.json ? try SessionHistory.json(page) + "\n" : SessionHistory.text(page))
        case "usage-events":
            try need(1, 1)
            var q = ["days": o.days]
            if !o.user.isEmpty { q["user"] = s(try userID(o.user)) }
            if !o.project.isEmpty { q["project"] = s(try projectID(o.project)) }
            if !o.model.isEmpty { q["model"] = o.model }
            printUsage(try request("GET", "/usage/events" + ProjectText.query(q)))
        case "quota":
            try need(1, 1)
            if o.history {
                guard o.claudeProbe.isEmpty else { throw fail("--history cannot be combined with --claude-probe") }
                var q = ["days": o.days]
                if !o.provider.isEmpty { q["provider"] = o.provider }
                let r = call("GET", "/quota/history" + ProjectText.query(q), nil)
                guard r.status != 0 else { throw fail("lost the connection to Mini Gateway") }
                if r.status >= 300 { throw fail(clean(JSONValue.parse(r.data)?.dict["error"]?.dict["message"]?.string ?? "")) }
                if r.status == 206 {
                    try? FileHandle.standardError.write(contentsOf: Data("quota history capped: newest 1000 probes; gaps capped at 1000\n".utf8))
                }
                if o.json { say(String(decoding: r.data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)) }
                else { out(QuotaHistory.text(r.data)) }
                break
            }
            if !o.claudeProbe.isEmpty {
                guard o.claudeProbe == "on" || o.claudeProbe == "off" else { throw fail("--claude-probe: on or off") }
                try request("PATCH", "/quota", ["claudeProbe": o.claudeProbe == "on"])
            }
            let r = try request("GET", "/quota")
            say("claude probe: " + (r["claudeprobe"]?.bool == true ? "on (runs claude -p /usage, at most every 10 minutes)" : "off (minigw gw quota --claude-probe=on turns it on)"))
            for q in r["quota"]?.array ?? [] {
                let d = q.dict
                var val = "-"
                if case .num(let n)? = d["value"] { val = GoText.f(n.doubleValue) + " " + (d["unit"]?.string ?? "") }
                var line = ProjectText.pad(d["name"]?.string ?? "", 28) + " " + ProjectText.pad(val, 14) + " " + (d["source"]?.string ?? "")
                if let t = d["observedat"]?.string.flatMap(QuotaTime.parse) { line += "  observed " + ProjectText.stamp(t, zone) }
                if let t = d["resetsat"]?.string.flatMap(QuotaTime.parse) { line += "  resets " + ProjectText.stamp(t, zone) }
                else if let t = d["resetstext"]?.string, !t.isEmpty { line += "  resets " + t }
                if d["stale"]?.bool == true { line += "  (stale)" }
                if let n = d["note"]?.string, !n.isEmpty { line += "  " + n }
                if let n = d["reason"]?.string, !n.isEmpty { line += "  unavailable: " + n }
                say(clean(line))
            }
        case "core":
            try need(2, 3)
            switch pos[1] {
            case "backup":
                var name = "core-" + Self.stamp(now()) + ".db"
                if pos.count == 3 {
                    name = pos[2]
                    if name.contains(where: { "/\\:".contains($0) }) {
                        throw fail("a backup is kept in the core folder's backups folder (a copy anywhere else would be readable by agents): give a name, not a path")
                    }
                }
                say("backup written to " + ((try request("POST", "/core/backup", ["name": name]))["file"]?.string ?? ""))
            case "audit" where pos.count == 2:
                var after: Int64 = 0
                while true {
                    let r = try requestRaw("GET", "/audit?after=\(after)&limit=500")  // Detail keeps its keys as they are
                    let events = ci(r, "events") as? [[String: Any]] ?? []
                    for e in events {
                        let id = (ci(e, "id") as? NSNumber)?.int64Value ?? 0
                        say(clean("\(id)  \(ci(e, "time") as? String ?? "")  \(ci(e, "actor") as? String ?? "")  \(ci(e, "action") as? String ?? "")  \(ci(e, "subject") as? String ?? "")  \(GoText.v(ci(e, "detail")))"))
                    }
                    if events.isEmpty { break }
                    after = (ci(r, "next") as? NSNumber)?.int64Value ?? 0
                }
            default: throw usage
            }
        default: break
        }
    }

    /// The default name of a backup: time.Now().UTC().Format("20060102T150405Z").
    static func stamp(_ d: Date) -> String {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX"); f.timeZone = TimeZone(identifier: "UTC"); f.dateFormat = "yyyyMMdd'T'HHmmss'Z'"
        return f.string(from: d)
    }

    /// validDeviceID: a device ID as the refusal prints it (base64url of the 32 bytes of the public key, 43 characters).
    public static func validDeviceID(_ id: String) -> Bool {
        guard id.utf8.count == 43, id.utf8.allSatisfy({ ($0 >= 0x41 && $0 <= 0x5a) || ($0 >= 0x61 && $0 <= 0x7a) || ($0 >= 0x30 && $0 <= 0x39) || $0 == 0x2d || $0 == 0x5f }) else { return false }
        // 43 characters carry 258 bits: the last character's two spare bits must be zero (Go: the ID must re-encode to itself).
        let tail = "AEIMQUYcgkosw048".contains(id.last!)
        return tail
    }

    /// printUsage with the admin view: one line per day, model and user, the totals, the gaps.
    func printUsage(_ r: JDict) {
        var ev: Int64 = 0, tin: Int64 = 0, tout: Int64 = 0
        let days = (r["days"]?.array ?? []).map(\.dict)
        for d in days {
            let name = d["username"]?.string ?? ""
            var who = "  user " + (name.isEmpty ? "-" : name)
            if let p = d["project"]?.int, p != 0 { who += "  project \(p)" }
            var extra = ""
            if let n = d["nestedevents"]?.int, n > 0 { extra += ", \(n) nested" }
            if let n = d["eventswithouttokens"]?.int, n > 0 { extra += ", \(n) without token counts" }
            if let n = d["failedevents"]?.int, n > 0 { extra += ", \(n) failed" }
            let (e, i, o) = (d["events"]?.int ?? 0, d["tokensin"]?.int ?? 0, d["tokensout"]?.int ?? 0)
            say(clean("\(d["day"]?.string ?? "")  \(ProjectText.pad(d["model"]?.string ?? "", 24))\(who)  \(e) events, \(i) in, \(o) out\(extra)"))
            ev += e; tin += i; tout += o
        }
        if days.isEmpty { say("no events") } else { say("total: \(ev) events, \(tin) in, \(tout) out (every event once, nested ones included)") }
        for g in (r["gaps"]?.array ?? []).map(\.dict) {
            say("\(g["count"]?.int ?? 0) events lost between \(ProjectText.stamp(g["from"]?.string.flatMap(ProjectText.parseTime), zone)) and \(ProjectText.stamp(g["to"]?.string.flatMap(ProjectText.parseTime), zone))")
        }
    }
}

extension ProjectText {
    /// time.Format("2006-01-02 15:04") in the zone; the zero time for a missing one.
    static func stamp(_ d: Date?, _ zone: TimeZone) -> String {
        guard let d else { return "0001-01-01 00:00" }
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX"); f.timeZone = zone; f.dateFormat = "yyyy-MM-dd HH:mm"
        return f.string(from: d)
    }
}

/// Go's fmt for the few verbs the commands use.
enum GoText {
    /// strconv.Quote for the printable ASCII and UTF-8 text a name is made of.
    static func quote(_ s: String) -> String {
        var o = "\""
        for u in s.unicodeScalars {
            switch u {
            case "\"": o += "\\\""
            case "\\": o += "\\\\"
            case "\n": o += "\\n"
            case "\r": o += "\\r"
            case "\t": o += "\\t"
            case "\u{7}": o += "\\a"
            case "\u{8}": o += "\\b"
            case "\u{c}": o += "\\f"
            case "\u{b}": o += "\\v"
            default:
                if u.value < 0x20 || u.value == 0x7f { o += String(format: "\\x%02x", u.value) }
                else if u.properties.generalCategory == .control || u.properties.generalCategory == .format || u.properties.generalCategory == .lineSeparator
                    || u.properties.generalCategory == .paragraphSeparator { o += u.value < 0x10000 ? String(format: "\\u%04x", u.value) : String(format: "\\U%08x", u.value) }
                else { o.unicodeScalars.append(u) }
            }
        }
        return o + "\""
    }

    /// %v of a JSON-decoded value (map[string]any, []any, float64, string, bool, nil).
    static func v(_ x: Any?) -> String {
        switch x {
        case nil, is NSNull: return "<nil>"
        case let s as String: return s
        case let n as NSNumber: return CFGetTypeID(n) == CFBooleanGetTypeID() ? (n.boolValue ? "true" : "false") : g(n.doubleValue)
        case let a as [Any]: return "[" + a.map { v($0) }.joined(separator: " ") + "]"
        case let m as [String: Any]:
            return "map[" + m.keys.sorted { Array($0.utf8).lexicographicallyPrecedes(Array($1.utf8)) }.map { $0 + ":" + v(m[$0]) }.joined(separator: " ") + "]"
        default: return "<nil>"
        }
    }

    /// strconv.FormatFloat(f, 'g', -1, 64), what %v prints for a float64: %e from an exponent below -4 or from 6.
    static func g(_ x: Double) -> String {
        if x == 0 { return x.sign == .minus ? "-0" : "0" }
        let (digits, dp) = shortest(abs(x))  // 0.d1d2… x 10^dp
        let sign = x < 0 ? "-" : ""
        let exp = dp - 1
        if exp < -4 || exp >= 6 {  // the shortest form decides at six digits
            var m = String(digits.first!)
            if digits.count > 1 { m += "." + digits.dropFirst() }
            return sign + m + "e" + (exp < 0 ? "-" : "+") + (abs(exp) < 10 ? "0" : "") + String(abs(exp))
        }
        return sign + fixed(digits, dp)
    }

    /// strconv.FormatFloat(f, 'f', -1, 64).
    static func f(_ x: Double) -> String {
        if x == 0 { return x.sign == .minus ? "-0" : "0" }
        return (x < 0 ? "-" : "") + fixed(shortest(abs(x)).0, shortest(abs(x)).1)
    }

    private static func fixed(_ digits: String, _ dp: Int) -> String {
        if dp <= 0 { return "0." + String(repeating: "0", count: -dp) + digits }
        if dp >= digits.count { return digits + String(repeating: "0", count: dp - digits.count) }
        return String(digits.prefix(dp)) + "." + digits.dropFirst(dp)
    }

    /// The shortest digits that read back as x, and the decimal point position (x = 0.digits × 10^dp).
    private static func shortest(_ x: Double) -> (String, Int) {
        let d = "\(x)"  // Swift prints the shortest round-trip form, with or without an exponent
        var mant = d, e = 0
        if let r = d.firstIndex(where: { $0 == "e" || $0 == "E" }) {
            mant = String(d[..<r]); e = Int(d[d.index(after: r)...]) ?? 0
        }
        let parts = mant.split(separator: ".", omittingEmptySubsequences: false)
        var ip = String(parts[0]), fp = parts.count > 1 ? String(parts[1]) : ""
        var dp = ip.count + e
        var all = ip + fp
        while all.hasPrefix("0") && all.count > 1 { all.removeFirst(); dp -= 1 }
        while all.hasSuffix("0") && all.count > 1 { all.removeLast() }
        ip = ""; fp = ""
        return (all, dp)
    }
}
