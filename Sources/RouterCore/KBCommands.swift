import Foundation
import CoreFoundation

extension ProjectEnv {
    public static let kbHelp = """
        minigw kb export <project> <dir>
        minigw kb import <project> <dir|file.md> [--class c] [--submit]
        minigw kb mcp [--project slug] [--print-config]
        minigw kb search [<project>] <query> [--json]
        minigw kb submit <project> [--class structure|data|private] [--kind finding|faq] [--title t] [--key k] [--replaces id --revision n] <file|->   faq: the title is the question, the text a short answer (up to 2 KiB)
        minigw kb faq [<project>] [--json]   the project's questions and answers
        minigw kb submissions [<project>] [--json]   your submissions (all projects if omitted)
        """
    public static let kbAdminHelp = """
        minigw gw kb rebuild-search
        minigw gw kb review [<project>]   pending submissions, including bodies
        minigw gw kb accept <id>
        minigw gw kb reject <id> [--reason text]
        """
    static func submissionLine(_ d: JDict) -> String {
        ProjectText.clean("submission \(d["id"]?.int ?? 0)  project \(d["project"]?.int ?? 0)  \(d["state"]?.string ?? "")  \(d["class"]?.string ?? "")  \(d["title"]?.string ?? "")\(kindSuffix(d))")
    }
    /// A faq is marked at the end of the first line; a finding prints as before.
    static func kindSuffix(_ d: JDict) -> String { d["entrykind"]?.string == "faq" ? "  (faq)" : "" }
    static func submissionResult(_ d: JDict) -> String { "submission \(d["id"]?.int ?? 0) \(d["state"]?.string ?? "")" }

    static func kbJSON(_ value: Any) -> String {
        if let d = value as? [String: Any] { return "{" + d.keys.sorted().map { GoJSON.string($0) + ":" + kbJSON(d[$0]!) }.joined(separator: ",") + "}" }
        if let a = value as? [Any] { return "[" + a.map(kbJSON).joined(separator: ",") + "]" }
        if let s = value as? String { return GoJSON.string(s) }
        if let n = value as? MCPNumber { return n.raw }  // digits unchanged (also above 2^53)
        if let n = value as? NSNumber { return CFGetTypeID(n) == CFBooleanGetTypeID() ? (n.boolValue ? "true" : "false") : n.stringValue }
        return "null"
    }

    public func kb(_ args: [String]) throws {
        if args.first == "export" || args.first == "import" { try kbFiles(args); return }
        if args.first == "search" { try kbSearch(Array(args.dropFirst())); return }
        if agent { throw ProjectError(Self.agentRefusal) }
        if args.first == "faq" { try kbFAQ(Array(args.dropFirst())); return }
        let usage = ProjectError("usage:\n" + Self.kbHelp)
        guard let fl = try? GoFlags.parse([("class", .string), ("kind", .string), ("title", .string), ("key", .string), ("replaces", .int), ("revision", .int), ("json", .bool)], args) else { throw usage }
        let pos = fl.positional
        guard pos.count == 3 && pos[0] == "submit" || (1...2).contains(pos.count) && pos[0] == "submissions" else { throw usage }
        let kind = fl.string("kind")
        if !kind.isEmpty && kind != "finding" && kind != "faq" { throw usage }
        let (c, _) = connect()
        func request(_ method: String, _ path: String, _ headers: [String: String] = [:], _ body: Data? = nil) throws -> Data {
            let r = send(c, method, path, headers, body)
            guard (200...299).contains(r.status) else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
            return r.data
        }
        var project: Int64 = 0
        if pos.count > 1 {
            let b = try request("GET", Self.apiProjects + "/" + ProjectText.pathEscape(pos[1]))
            project = JSONValue.parse(b)?.dict["id"]?.int ?? 0
        }
        if pos[0] == "submit" {
            let body = try readInput(pos[2])
            let title = fl.string("title").isEmpty ? defaultTitle(body, pos[2]) : fl.string("title")
            if title.utf8.count > Self.maxNoteTitle { throw ProjectError("too large: a title is at most \(Self.maxNoteTitle) bytes") }
            var key = fl.string("key")
            if key.isEmpty {
                var g = SystemRandomNumberGenerator()
                key = "mgw-" + (0..<16).map { _ in String(format: "%02x", UInt8.random(in: 0...255, using: &g)) }.joined()
            }
            let cls = fl.set.contains("class") ? fl.string("class") : "data"
            var fields: [String: Any] = ["class": cls, "title": title, "body": body, "replaces": fl.int("replaces"), "revision": fl.int("revision")]
            if !kind.isEmpty { fields["entryKind"] = kind }
            let payload = try JSONSerialization.data(withJSONObject: fields)
            var r = ProjectReply(status: 0)
            for _ in 0...Self.pushRetries {
                r = send(c, "POST", "\(Self.apiProjects)/\(project)/kb/submissions", ["Idempotency-Key": key], payload)
                if r.status != 0 { break }; sleep(1)
            }
            if r.status == 0 { throw ProjectError("no answer from the gateway; repeat with --key \(key) (safe: no duplicate), or check with minigw kb submissions") }
            guard r.status == 201 else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
            if fl.bool("json") { say(ProjectText.jsonText(ProjectText.trim(String(decoding: r.data, as: UTF8.self)))) }
            else { say(Self.submissionResult(JSONValue.parse(r.data)?.dict ?? JDict())) }
            return
        }
        var projects = [project]
        if project == 0 {
            let b = try request("GET", Self.apiProjects + "?archived=1")
            guard case .arr(let ps)? = JSONValue.parse(b) else { throw Self.malformed }
            projects = ps.map { $0.dict["id"]?.int ?? 0 }
        }
        var raw: [[String: Any]] = []
        var all: [JSONValue] = []
        for pid in projects {
            var after: Int64 = 0
            repeat {
                let b = try request("GET", "\(Self.apiProjects)/\(pid)/kb/submissions?after=\(after)")
                guard case .obj(let pg)? = JSONValue.parse(b) else { throw Self.malformed }
                all += pg["submissions"]?.array ?? []
                let obj = try JSONSerialization.jsonObject(with: b) as? [String: Any]
                raw += obj?["submissions"] as? [[String: Any]] ?? []
                after = pg["next"]?.int ?? 0
            } while after != 0
        }
        if fl.bool("json") { say(ProjectText.jsonText(Self.kbJSON(["submissions": raw]))); return }
        for v in all { say(Self.submissionLine(v.dict)) }
        if all.isEmpty { say("no submissions") }
    }

    func searchLine(_ d: JDict) -> String {
        ProjectText.clean("\(d["id"]?.int ?? 0)  \(d["title"]?.string ?? "")  project \(d["project"]?.int ?? 0)  \(d["class"]?.string ?? "")  \(stamp(d["updated"]?.string.flatMap(ProjectText.parseTime)))\(Self.kindSuffix(d))\n\(d["snippet"]?.string ?? "")")
    }
    func kbSearch(_ args: [String]) throws {
        let usage = ProjectError("usage: minigw kb search [<project>] <query> [--json]")
        guard let fl = try? GoFlags.parse([("json", .bool)], args), (1...2).contains(fl.positional.count) else { throw usage }
        let pos = fl.positional
        let c: ProjectConn
        var base = "/minigw/v1/kb/search"
        if let rc = runConn {
            guard pos.count == 1 else { throw ProjectError("inside an agent run the project is the run's own: a project argument is not accepted") }
            c = rc
        } else {
            if agent { throw ProjectError(Self.agentRefusal) }
            if pos.count == 1 {
                let (cc, link) = try linkedProject(""); c = cc
                base = "\(Self.apiProjects)/\(link.project)/kb/search"
            } else {
                let (cc, _) = connect(); c = cc
                let r = request(c, "GET", Self.apiProjects + "/" + ProjectText.pathEscape(pos[0]), [:], nil)
                guard r.status == 200 else { throw gwError(r) }
                guard let id = JSONValue.parse(r.data)?.dict["id"]?.int else { throw Self.malformed }
                base = "\(Self.apiProjects)/\(id)/kb/search"
            }
        }
        let r = request(c, "GET", base + "?q=" + ProjectText.queryEscape(pos.last!), [:], nil)
        guard r.status == 200 else { throw gwError(r) }
        if fl.bool("json") { say(ProjectText.jsonText(ProjectText.trim(String(decoding: r.data, as: UTF8.self)))); return }
        guard let hits = JSONValue.parse(r.data)?.dict["hits"]?.array else { throw Self.malformed }
        for h in hits { say(searchLine(h.dict)) }
        if hits.isEmpty { say("no hits") }
    }
}

extension AdminCommands {
    func kb(_ pos: [String], reason: String) throws {
        let usage = fail("usage:\n" + ProjectEnv.kbAdminHelp)
        guard pos.count >= 2 else { throw usage }
        if pos.count == 2 && pos[1] == "rebuild-search" { _ = try request("POST", "/kb/search/rebuild"); say("search index rebuilt"); return }
        if pos[1] == "review" && pos.count <= 3 {
            let project = pos.count == 3 ? try projectID(pos[2]) : 0
            var after: Int64 = 0
            var count = 0
            repeat {
                let pg = try request("GET", "/kb/submissions?project=\(project)&after=\(after)")
                for v in pg["submissions"]?.array ?? [] {
                    let full = try request("GET", "/kb/submissions/\(v.dict["id"]?.int ?? 0)")
                    say(ProjectEnv.submissionLine(full))
                    say(clean("writer \(full["user"]?.int ?? 0)  device \(full["provenance"]?.dict["device"]?.string ?? "")  replaces \(full["replaces"]?.int ?? 0)  revision \(full["revision"]?.int ?? 0)"))
                    if let body = full["body"]?.string { say(clean(body)) }; count += 1
                }
                after = pg["next"]?.int ?? 0
            } while after != 0
            if count == 0 { say("no submissions") }; return
        }
        if pos.count == 3 && ["accept", "reject"].contains(pos[1]), let id = Int64(pos[2]), id > 0 {
            let v = try request("POST", "/kb/submissions/\(id)/\(pos[1])", ["reason": reason])
            say(ProjectEnv.submissionResult(v)); return
        }
        throw usage
    }
}
