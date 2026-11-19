import Foundation
import CoreFoundation

extension ProjectEnv {
    public static let kbHelp = """
        minigw kb export <project>|--team <team> <dir>
        minigw kb import <project>|--team <team> <dir|file.md> [--class c] [--submit]
        minigw kb mcp [--project slug] [--print-config]
        minigw kb search [<project>|--team <team>] <query> [--json]   a project search also covers its team's notes (as far as you may read them); a hit from there ends with "from team:<name>", a read-only copy from an upstream gateway with "from hub:<name> project:<slug>"
        minigw kb submit <project>|--team <team> [--class structure|data] [--kind finding|faq] [--title t] [--key k] [--replaces id --revision n] <file|->   faq: the title is the question, the text a short answer (up to 2 KiB); private is for projects only
        minigw kb faq [<project>|--team <team>] [--json]   the project's (or the team's) questions and answers
        minigw kb submissions [<project>|--team <team>] [--json]   your submissions (all projects and your teams' own notes if omitted)
        minigw kb hooks on|off|status [<project>]   the plugin's hooks that ask a Claude Code session to hand findings in (off by default, per project, on this computer)
        minigw kb review [<project>|--team <team>]   project admins: the pending submissions you may review (never your own), including bodies
        minigw kb accept <id> [--version <hash>]   project admins: publish a submission of another member; --version: the whole version line kb review showed (without it: only in a terminal, after you confirm the text)
        minigw kb reject <id> [--reason text]
        """
    public static let kbAdminHelp = """
        minigw gw kb history <id> [--revision n] [--json]
        minigw gw kb restore <id> --revision n [--class c] [--current n]
        minigw gw kb rebuild-search
        minigw gw kb review [<project>|--team <team>]   pending submissions, including bodies
        minigw gw kb accept <id> [--version <hash>]   --version: the whole version line gw kb review showed (without it: only in a terminal, after you confirm the text)
        minigw gw kb reject <id> [--reason text]
        """
    static let teamProjectUsage = "usage: give either a project or --team <team>"
    static let teamAgentRefusal = "inside an agent run the project is the run's own: --team is not accepted"
    /// Where a team's own notes live: ids above this are a team's notes (the team id is the difference).
    static let segmentBase: Int64 = 1 << 40
    /// Where a note, hit or submission belongs: "project <id>", or "team <name>" for a team's own notes (never the hidden id:
    /// an unknown name prints "team"). The only place that prints a project column.
    static func subjectOf(_ project: Int64, _ name: String) -> String {
        guard project > segmentBase else { return "project \(project)" }
        return name.isEmpty ? "team" : "team " + name
    }
    /// subjectOf for a submission, whose team name comes from a map of team ids to names.
    static func teamSubject(_ project: Int64, _ names: [Int64: String]) -> String { subjectOf(project, names[project - segmentBase] ?? "") }
    static func submissionLine(_ d: JDict, _ names: [Int64: String] = [:]) -> String {
        ProjectText.clean("submission \(d["id"]?.int ?? 0)  \(teamSubject(d["project"]?.int ?? 0, names))  \(d["state"]?.string ?? "")  \(d["class"]?.string ?? "")  \(d["title"]?.string ?? "")\(kindSuffix(d))")
    }
    /// A faq is marked at the end of the first line; a finding prints as before.
    static func kindSuffix(_ d: JDict) -> String { d["entrykind"]?.string == "faq" ? "  (faq)" : "" }
    /// How far back a rejected submission with the same title is still mentioned after a submit.
    static let rejectedHintDays = 30.0
    /// After a submit: a submission with the same title was rejected in the last rejectedHintDays days, and why. The author's own rows
    /// carry no hash or body, so only the title can be compared. Best effort: any failure says nothing. Twin of projectEnv.rejectedHint.
    func rejectedHint(_ c: ProjectConn, _ project: Int64, _ title: String) {
        let now = self.now?() ?? Date()
        var found: JDict?
        var after: Int64 = 0
        for _ in 0..<10 {
            let r = send(c, "GET", "\(Self.apiProjects)/\(project)/kb/submissions?after=\(after)", [:], nil)
            guard r.status == 200, case .obj(let pg)? = JSONValue.parse(r.data) else { return }
            for v in pg["submissions"]?.array ?? [] {
                let d = v.dict
                guard d["state"]?.string == "rejected", !(d["reason"]?.string ?? "").isEmpty, d["title"]?.string == title,
                      let stamp = d["provenance"]?.dict["received"]?.string,
                      let at = ISO8601DateFormatter().date(from: stamp.replacingOccurrences(of: "\\.[0-9]+", with: "", options: .regularExpression)),
                      now.timeIntervalSince(at) < Self.rejectedHintDays * 86400 else { continue }
                if found == nil || (d["id"]?.int ?? 0) > (found?["id"]?.int ?? 0) { found = d }
            }
            after = pg["next"]?.int ?? 0
            if after == 0 { break }
        }
        if let why = found?["reason"]?.string { say(ProjectText.clean("an earlier submission with this title was rejected: " + why)) }
    }
    static func submissionResult(_ d: JDict) -> String { "submission \(d["id"]?.int ?? 0) \(d["state"]?.string ?? "")" }

    static func kbJSON(_ value: Any) -> String {
        if let d = value as? [String: Any] { return "{" + d.keys.sorted().map { GoJSON.string($0) + ":" + kbJSON(d[$0]!) }.joined(separator: ",") + "}" }
        if let a = value as? [Any] { return "[" + a.map(kbJSON).joined(separator: ",") + "]" }
        if let s = value as? String { return GoJSON.string(s) }
        if let n = value as? MCPNumber { return n.raw }  // digits unchanged (also above 2^53)
        if let n = value as? NSNumber { return CFGetTypeID(n) == CFBooleanGetTypeID() ? (n.boolValue ? "true" : "false") : n.stringValue }
        return "null"
    }

    /// --team: the id of the team's own notes, which the notes, search and submission routes take in place of a project id.
    func teamSegment(_ c: ProjectConn, _ team: String) throws -> Int64 { try teamInfo(c, team).0 }
    /// teamSegment with the team's name as the gateway spells it.
    func teamInfo(_ c: ProjectConn, _ team: String) throws -> (Int64, String) {
        let r = request(c, "GET", "/minigw/v1/teams/" + ProjectText.pathEscape(team), [:], nil)
        guard r.status == 200 else { throw gwError(r) }
        let d = JSONValue.parse(r.data)?.dict
        guard let segment = d?["segment"]?.int else { throw Self.malformed }
        if segment == 0 { throw ProjectError("this gateway has no team notes: update it") }
        return (segment, d?["name"]?.string ?? "")
    }

    public func kb(_ args: [String]) throws {
        if args.first == "export" || args.first == "import" { try kbFiles(args); return }
        if args.first == "search" { try kbSearch(Array(args.dropFirst())); return }
        if args.first == "hook" { kbHook(Array(args.dropFirst())); return }  // before the agent refusal: a hook inside an agent run is silent
        if args.first == "hooks" { try kbHooks(Array(args.dropFirst())); return }
        if agent { throw ProjectError(Self.agentRefusal) }
        if args.first == "faq" { try kbFAQ(Array(args.dropFirst())); return }
        let usage = ProjectError("usage:\n" + Self.kbHelp)
        guard let fl = try? GoFlags.parse([("class", .string), ("kind", .string), ("title", .string), ("key", .string), ("replaces", .int), ("revision", .int), ("json", .bool), ("team", .string), ("reason", .string), ("version", .string)], args) else { throw usage }
        var pos = fl.positional
        let team = fl.string("team")
        if pos.first == "accept" || pos.first == "reject" { try kbDecide(pos, team, fl.string("reason"), fl.string("version")); return }
        if !team.isEmpty {
            guard pos.first == "submit" || pos.first == "submissions" || pos.first == "review" else { throw usage }
            guard pos[0] == "submit" ? pos.count == 2 : pos.count == 1 else { throw ProjectError(Self.teamProjectUsage) }
            if pos[0] == "submit" { pos = ["submit", "", pos[1]] }
        } else {
            guard pos.count == 3 && pos[0] == "submit" || (1...2).contains(pos.count) && (pos[0] == "submissions" || pos[0] == "review") else { throw usage }
        }
        let kind = fl.string("kind")
        if !kind.isEmpty && kind != "finding" && kind != "faq" { throw usage }
        let (c, gw) = connect()
        func request(_ method: String, _ path: String, _ headers: [String: String] = [:], _ body: Data? = nil) throws -> Data {
            let r = send(c, method, path, headers, body)
            guard (200...299).contains(r.status) else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
            return r.data
        }
        var project: Int64 = 0
        var names: [Int64: String] = [:]  // team id -> name, for the lines of submissions to a team's own notes
        if !team.isEmpty {
            let (id, name) = try teamInfo(c, team)
            project = id; names[id - Self.segmentBase] = name
        } else if pos.count > 1 {
            let b = try request("GET", Self.apiProjects + "/" + ProjectText.pathEscape(pos[1]))
            project = JSONValue.parse(b)?.dict["id"]?.int ?? 0
        }
        if pos[0] == "review" { try kbReview(c, project, names); return }
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
            recordSubmitted(gw, project)
            if fl.bool("json") { say(ProjectText.jsonText(ProjectText.trim(String(decoding: r.data, as: UTF8.self)))) }
            else {
                say(Self.submissionResult(JSONValue.parse(r.data)?.dict ?? JDict()))
                rejectedHint(c, project, title)
            }
            return
        }
        var projects = [project]
        if project == 0 {
            let b = try request("GET", Self.apiProjects + "?archived=1")
            guard case .arr(let ps)? = JSONValue.parse(b) else { throw Self.malformed }
            projects = ps.map { $0.dict["id"]?.int ?? 0 }
            // The teams you are on: your submissions to their own notes too (a gateway without teams just has none).
            let tr = send(c, "GET", "/minigw/v1/teams", [:], nil)
            if tr.status == 200, case .arr(let ts)? = JSONValue.parse(tr.data) {
                for t in ts {
                    let id = t.dict["segment"]?.int ?? 0
                    if id > Self.segmentBase { projects.append(id); names[id - Self.segmentBase] = t.dict["name"]?.string ?? "" }
                }
            }
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
        for v in all {
            say(Self.submissionLine(v.dict, names))
            if v.dict["state"]?.string == "rejected", let why = v.dict["reason"]?.string, !why.isEmpty { say(ProjectText.clean("    reason: " + why)) }  // why it was rejected (#163)
        }
        if all.isEmpty { say("no submissions") }
    }

    /// The pending submissions a project admin may review (project 0: all of them), as gw kb review prints them.
    func kbReview(_ c: ProjectConn, _ project: Int64, _ known: [Int64: String]) throws {
        func request(_ path: String) throws -> JDict {
            let r = send(c, "GET", path, [:], nil)
            guard r.status == 200 else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
            return JSONValue.parse(r.data)?.dict ?? JDict()
        }
        var names = known, teamsAsked = false, count = 0, after: Int64 = 0
        repeat {
            let pg = try request("\(Self.apiProjects)/\(project)/kb/review?after=\(after)")
            for v in pg["submissions"]?.array ?? [] {
                let full = try request("\(Self.apiProjects)/0/kb/review/\(v.dict["id"]?.int ?? 0)")
                let pid = full["project"]?.int ?? 0
                if pid > Self.segmentBase && (names[pid - Self.segmentBase] ?? "").isEmpty && !teamsAsked {
                    teamsAsked = true  // the notes of a team print as "team <name>": the teams you are on, asked once
                    let tr = send(c, "GET", "/minigw/v1/teams", [:], nil)
                    if tr.status == 200, case .arr(let ts)? = JSONValue.parse(tr.data) {
                        for t in ts {
                            let id = t.dict["segment"]?.int ?? 0
                            if id > Self.segmentBase { names[id - Self.segmentBase] = t.dict["name"]?.string ?? "" }
                        }
                    }
                }
                Self.printReview(say, full, names)
                count += 1
            }
            after = pg["next"]?.int ?? 0
        } while after != 0
        if count == 0 { say("no submissions") }
    }

    /// One submission as gw kb review and kb review print it: its line, the writer, and the text.
    static func printReview(_ say: (String) -> Void, _ full: JDict, _ names: [Int64: String]) {
        say(submissionLine(full, names))
        let hash = full["reviewHash"]?.string ?? ""
        say(ProjectText.clean("writer \(full["authorName"]?.string.flatMap { $0.isEmpty ? nil : $0 } ?? String(full["user"]?.int ?? 0))  device \(full["provenance"]?.dict["device"]?.string ?? "")  replaces \(full["replaces"]?.int ?? 0)  revision \(full["revision"]?.int ?? 0)"))
        if !hash.isEmpty { say(ProjectText.clean("version " + hash)) }  // on its own line, to copy whole into kb accept --version
        if let n = full["hidden"]?.int, n > 0 { say("contains hidden characters: \(n) removed from the text shown; it cannot be accepted (reject it and ask for the text without them)") }
        if let body = full["body"]?.string { say(ProjectText.reviewText(body)) }
    }

    static let changedSinceRead = "the submission changed since you read it: review it again"
    static let acceptNeedsTag = "not accepted: without a terminal, name the version you read: --version <hash> (kb review shows it)"
    static let versionWhole = "--version takes the whole version line of kb review (64 hex characters), not a part of it"
    static let acceptDeclined = "not accepted"

    /// confirmAccept of Go: with --version only when it is exactly the hash (all 64 hex); without it only in a terminal,
    /// after a "y". nil = send; otherwise the refusal, and nothing is sent.
    static func confirmAccept(_ hash: String, _ version: String, terminal: Bool, ask: (String) -> Void, answer: () -> String?) -> String? {
        if !version.isEmpty { return version.count != 64 ? versionWhole : version == hash ? nil : changedSinceRead }
        if !terminal { return acceptNeedsTag }
        ask("Accept this text? [y/N] ")
        let a = (answer() ?? "").trimmingCharacters(in: .whitespaces).lowercased()
        return a == "y" || a == "yes" ? nil : acceptDeclined
    }

    /// A project admin's kb accept|reject <id>: the gateway finds the submission's project and decides who may. An accept
    /// first reads the submission, prints what it accepts and sends the version it read (refused when the text changed).
    func kbDecide(_ pos: [String], _ team: String, _ reason: String, _ version: String) throws {
        guard pos.count == 2, team.isEmpty, let id = Int64(pos[1]), id > 0 else { throw ProjectError("usage:\n" + Self.kbHelp) }
        let (c, _) = connect()
        var fields: [String: Any] = ["reason": reason]
        if pos[0] == "accept" {
            let g = send(c, "GET", "\(Self.apiProjects)/0/kb/review/\(id)", [:], nil)
            guard g.status == 200 else { throw Self.gatewayError(g.status, g.data, g.retryAfter) }
            let full = JSONValue.parse(g.data)?.dict ?? JDict()
            Self.printReview(say, full, [:])
            let hash = full["reviewHash"]?.string ?? ""
            if let refusal = Self.confirmAccept(hash, version, terminal: isTerminal(), ask: { out($0) }, answer: readAnswer) { throw ProjectError(refusal) }
            fields["reviewHash"] = hash
        }
        let r = send(c, "POST", "\(Self.apiProjects)/0/kb/review/\(id)/\(pos[0])", [:], try JSONSerialization.data(withJSONObject: fields, options: [.sortedKeys]))
        guard r.status == 200 else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
        say(Self.submissionResult(JSONValue.parse(r.data)?.dict ?? JDict()))
    }

    func searchLine(_ d: JDict) -> String {
        ProjectText.clean("\(d["id"]?.int ?? 0)  \(ProjectText.oneLine(d["title"]?.string ?? ""))  \(Self.subjectOf(d["project"]?.int ?? 0, d["team"]?.string ?? ""))  \(d["class"]?.string ?? "")  \(stamp(d["updated"]?.string.flatMap(ProjectText.parseTime)))\(Self.kindSuffix(d))\(d["from"]?.string.flatMap { $0.isEmpty ? nil : "  from " + $0 } ?? "")\n\(d["snippet"]?.string ?? "")")
    }
    func kbSearch(_ args: [String]) throws {
        let usage = ProjectError("usage: minigw kb search [<project>|--team <team>] <query> [--json]")
        guard let fl = try? GoFlags.parse([("json", .bool), ("team", .string)], args), (1...2).contains(fl.positional.count) else { throw usage }
        let pos = fl.positional
        let c: ProjectConn
        var base = "/minigw/v1/kb/search"
        if !fl.string("team").isEmpty {
            if agent || runConn != nil { throw ProjectError(Self.teamAgentRefusal) }
            guard pos.count == 1 else { throw ProjectError(Self.teamProjectUsage) }
            let (cc, _) = connect(); c = cc
            base = "\(Self.apiProjects)/\(try teamSegment(c, fl.string("team")))/kb/search"
        } else if let rc = runConn {
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
        if let note = JSONValue.parse(r.data)?.dict["note"]?.string, !note.isEmpty { say(ProjectText.clean(note)) }  // the hub's hits were left out
        if hits.isEmpty { say("no hits") }
        else if !fl.string("team").isEmpty { say("read a team's note with: minigw note read <id> --team " + ProjectText.clean(fl.string("team"))) }
        else if pos.count == 2 { say("read a note of project " + ProjectText.clean(pos[0]) + " from a folder linked to it: minigw note read <id>") }
    }
}

extension AdminCommands {
    /// A 404 or 405 on the owner's note history routes from a gateway older than 0.8.7 becomes a sentence that names the gateway's version
    /// (GET /status) and the update; nil when the path is another one or the version is not older (Go's adminClient.olderNoteHistory).
    func olderNoteHistory(_ path: String) -> String? {
        guard path.range(of: #"^/kb/notes/[0-9]+/(history(/[0-9]+)?|restore)$"#, options: .regularExpression) != nil else { return nil }
        let r = call("GET", "/status", nil)
        guard r.status == 200, let d = JSONValue.parse(r.data)?.dict, let v = d["Version"]?.string ?? d["version"]?.string,
              !v.isEmpty, v.allSatisfy({ $0.isASCII && ($0.isNumber || $0 == ".") }), ProjectEnv.versionLess(v, "0.8.7") else { return nil }
        return "this gateway is \(v); \(path.hasSuffix("/restore") ? "note restore" : "note history") needs 0.8.7 or newer: update the gateway (/minigw:gw update there)"
    }

    func kb(_ pos: [String], reason: String, team: String = "", version: String = "", opts: AdminOpts = AdminOpts()) throws {
        let usage = fail("usage:\n" + ProjectEnv.kbAdminHelp)
        guard pos.count >= 2 else { throw usage }
        if pos.count == 3 && (pos[1] == "history" || pos[1] == "restore") {  // the owner's saved versions of any note: no project or team selection
            guard team.isEmpty && opts.project.isEmpty, let id = Int64(pos[2]), id > 0 else { throw usage }
            do {
                try ProjectEnv.noteHistoryCommand(pos[1], "/kb/notes/" + pos[2], id: id, revision: opts.revision, current: opts.current, hasCurrent: opts.hasCurrent, cls: opts.classes, asJSON: opts.json,
                                                  terminal: isTerminal(), out: { out($0) }, answer: readAnswer, zone: zone, request: { method, path, body in
                    let r = call(method, path, body)
                    guard r.status != 0 else { throw fail("lost the connection to Mini Gateway") }
                    if r.status >= 300 {
                        if r.status == 404 || r.status == 405, let hint = olderNoteHistory(path) { throw fail(hint) }
                        throw fail(clean(JSONValue.parse(r.data)?.dict["error"]?.dict["message"]?.string ?? ""))
                    }
                    return r.data
                })
            } catch let e as ProjectError { throw fail(clean(e.message)) }
            return
        }
        if pos.count == 2 && pos[1] == "rebuild-search" { _ = try request("POST", "/kb/search/rebuild"); say("search index rebuilt"); return }
        if pos[1] == "review" && pos.count <= 3 && (team.isEmpty || pos.count == 2) {
            var project = pos.count == 3 ? try projectID(pos[2]) : 0
            if !team.isEmpty { project = ProjectEnv.segmentBase + (try teamID(team)) }
            var names: [Int64: String]?  // the notes of a team print as "team <name>"; asked for only when one shows up
            func teamNames() throws -> [Int64: String] {
                if let n = names { return n }
                var m: [Int64: String] = [:]
                for t in try request("GET", "/teams")["teams"]?.array ?? [] { m[t.dict["id"]?.int ?? 0] = t.dict["name"]?.string ?? "" }
                names = m; return m
            }
            var after: Int64 = 0
            var count = 0
            repeat {
                let pg = try request("GET", "/kb/submissions?project=\(project)&after=\(after)")
                for v in pg["submissions"]?.array ?? [] {
                    let full = try request("GET", "/kb/submissions/\(v.dict["id"]?.int ?? 0)")
                    ProjectEnv.printReview(say, full, (full["project"]?.int ?? 0) > ProjectEnv.segmentBase ? try teamNames() : [:]); count += 1
                }
                after = pg["next"]?.int ?? 0
            } while after != 0
            if count == 0 { say("no submissions") }; return
        }
        if pos.count == 3 && ["accept", "reject"].contains(pos[1]), let id = Int64(pos[2]), id > 0 {
            var fields: [String: Any] = ["reason": reason]
            if pos[1] == "accept" {  // what is accepted, and the version read: the gateway refuses a text changed since
                let full = try request("GET", "/kb/submissions/\(id)")
                ProjectEnv.printReview(say, full, [:])
                let hash = full["reviewHash"]?.string ?? ""
                if let refusal = ProjectEnv.confirmAccept(hash, version, terminal: isTerminal(), ask: { out($0) }, answer: readAnswer) { throw fail(refusal) }
                fields["reviewHash"] = hash
            }
            let v = try request("POST", "/kb/submissions/\(id)/\(pos[1])", fields)
            say(ProjectEnv.submissionResult(v)); return
        }
        throw usage
    }
}
