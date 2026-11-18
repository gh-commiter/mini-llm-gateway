import Foundation

// minigw project list|link|unlink|show, minigw note list|read|push|edit|rm and minigw usage: the member's side of the gateway's
// projects (0.6.0). A port of client/project_cmd.go: the same arguments, help text, output, errors and exit codes. The commands
// work on a ProjectEnv so that tests can run them against a fake gateway with a temporary state file; the thin glue that builds
// the real one is Sources/MiniGW/Project.swift.

public struct ProjectConn: Sendable {
    public var url: String
    public var token: String
    public init(url: String, token: String) { self.url = url; self.token = token }
}

/// status 0 = no answer from the gateway.
public struct ProjectReply: Sendable {
    public var status: Int
    public var data: Data
    public var retryAfter: String?
    public init(status: Int, data: Data = Data(), retryAfter: String? = nil) { self.status = status; self.data = data; self.retryAfter = retryAfter }
}

public final class ProjectEnv {

    public static let usageHelp = "minigw usage [--project slug] [--days n] [--json]   your model usage (a developer sees own events in a project, an admin all of it)"
    public static let runningHelp = "minigw running [--json]                    your requests that run on the gateway now: model, folder, time, steps, last step"
    public static let projectHelp = """
        minigw project list [--archived] [--path <prefix>] [--json]   the projects you can see (role, and what a grant gives you); --path: only those of your teams whose path label equals the prefix or starts with prefix/
        minigw project link [<slug>]                link this folder (and its subfolders) to a project you are a member of; without a slug the project is found by this folder's git remote
        minigw project remote list [<slug>]         the git remotes registered for a project (default: the linked one)
        minigw project remote add <url>|--here [<slug>]   register a git remote (project admin); --here: this folder's origin
        minigw project remote remove <url> [<slug>]
        minigw project depends [<slug>]             the projects and teams this project depends on (those you may see) (default: the linked one); a search of the project also covers them
        minigw project unlink                       remove the link that applies here
        minigw project show                         the link that applies here, and why
        minigw project sessions [<project>] [--days n] [--model m] [--device d] [--phase p] [--activity a] [--task t] [--json]
        """
    public static let noteHelp = """
        minigw note list [--class c] [--limit n] [--after cursor] [--json]   notes of the linked project
        minigw note read <id> [--json]
        minigw note push [--class structure|data|private] [--title t] [--key k] <file|->   new note (default class data)
        minigw note edit <id> [--class c] [--title t] [--revision n] [<file|->]
        minigw note rm <id> [--revision n]        (inside an agent run only note list and note read work, on the run's own project)
           --project <slug> only confirms the project this folder is linked to; classes: structure, data, private
           (private = visible to you and the project admins)
           --team <team> on any of these: the team's own notes instead of the linked project (every team member reads, a project admin of the team writes; structure and data only)
        """
    static let privateNote = "private = visible to you and the project admins"
    static let agentRefusal = "inside an agent run only minigw kb search, note list and note read work (on the run's own project); link, push, edit, rm and project commands are not available"
    public static let maxNoteBody = 64 << 10
    static let maxNoteTitle = 256
    static let pushRetries = 3
    static let apiProjects = "/minigw/v1/projects"

    public typealias Send = (ProjectConn, _ method: String, _ path: String, _ headers: [String: String], _ body: Data?) -> ProjectReply

    public var connect: () -> (ProjectConn, String)  // the routed connection and the gateway's identity (only when needed)
    public var state: String  // projects.json
    public var cwd: String
    public var stdin: () -> Data  // at most maxNoteBody + 1 bytes
    public var out: (String) -> Void
    public var err: (String) -> Void
    public var agent: Bool
    public var runConn: ProjectConn?  // inside an agent run: the run port and its token; only note list|read work, on the run's own project
    public var send: Send
    public var sleep: (TimeInterval) -> Void
    public var zone = TimeZone.current
    /// The folder's raw remote.origin.url, "" = none; nil = ask git (tests inject it).
    public var origin: ((String) -> String)?
    /// Test seam, per instance so that parallel tests do not interfere: runs after the export destination is opened and checked; nil in production.
    public var exportHook: (() -> Void)?
    /// kb hooks (KBHook.swift), per instance so that parallel tests do not interfere; nil = the real thing.
    public var gateway: (() -> String)?  // the default profile's gateway id, "" = not connected
    public var gitState: ((String) -> (head: String, status: String))?  // HEAD and the status hash of the linked folder
    public var now: (() -> Date)?
    public var gitTimeout: TimeInterval?  // seconds for both git calls of a hook, nil = 2

    public init(connect: @escaping () -> (ProjectConn, String), state: String, cwd: String, stdin: @escaping () -> Data,
                out: @escaping (String) -> Void, err: @escaping (String) -> Void, agent: Bool, runConn: ProjectConn? = nil,
                send: @escaping Send, sleep: @escaping (TimeInterval) -> Void = { Thread.sleep(forTimeInterval: $0) }) {
        self.connect = connect; self.state = state; self.cwd = cwd; self.stdin = stdin; self.out = out; self.err = err
        self.agent = agent; self.runConn = runConn; self.send = send; self.sleep = sleep
    }

    func say(_ s: String) { out(s + "\n") }

    // MARK: entry points

    public func project(_ args: [String]) throws { try runProject(args) }
    public func note(_ args: [String]) throws { try runNote(args) }
    public func usage(_ args: [String]) throws { try runUsage(args) }
    public func running(_ args: [String]) throws { try runRunning(args) }

    // MARK: gateway answers

    /// Section 6's answers as short sentences. 403 forbidden is printed plainly.
    static func gatewayError(_ st: Int, _ data: Data, _ retryAfter: String?) -> ProjectError {
        var e = gatewayAnswer(st, data, retryAfter)
        e.gatewayStatus = st
        return e
    }
    static func gatewayAnswer(_ st: Int, _ data: Data, _ retryAfter: String?) -> ProjectError {
        let e = JSONValue.parse(data)?.dict["error"]?.dict ?? JDict()
        let type = e["type"]?.string ?? "", message = e["message"]?.string ?? ""
        switch st {
        case 0: return ProjectError("no answer from the gateway")
        case 400: return ProjectError("rejected: " + ProjectText.oneLine(message))
        case 403:
            switch type {
            case "forbidden": return ProjectError("not allowed: your role in this project does not permit it (a grant alone only lets you read; linking needs a direct role)")
            case "archived": return ProjectError("the project is archived: nobody can write to it")
            default:
                let m = ProjectText.oneLine(message)
                if m.hasPrefix("no project access:") { return ProjectError(m) }  // the gateway names the condition
                return ProjectError("no project access: this device is not approved or not bound to a user on the gateway (ask the gateway owner)")
            }
        case 404: return ProjectError("not found or not visible")
        case 409:
            if type == "revision_mismatch" {
                return ProjectError("the note has changed: its current revision is \(e["revision"]?.int ?? 0) (read it again, then retry)")
            }
            return ProjectError("this idempotency key was already used for another note")
        case 413:
            if type == "pending_limit" { return ProjectError(ProjectText.oneLine(message)) }
            return ProjectError("too large (title up to \(maxNoteTitle) bytes, body up to \(maxNoteBody >> 10) KiB, project limits apply)")
        case 429:
            let wait = retryAfter.flatMap { GoFlags.parseInt($0) }.map { "\($0) seconds" } ?? "a minute"
            return ProjectError("too many requests: try again in " + wait)
        case 503: return ProjectError("the gateway's project store is not available")
        default: return ProjectError("gateway answered HTTP \(st)")
        }
    }

    /// A note outside the linked project's scope answers like one that does not exist; the hint is client-side text only (as Go noteScopeHint).
    static let noteScopeText = "a note of a team needs --team <team>; a note of another project needs a folder linked to that project (a search of a team or another project shows the ids)"
    static func scopeHint(_ e: ProjectError, scoped: Bool) -> ProjectError {
        scoped || e.message != "not found or not visible" ? e : ProjectError(e.message + "\n" + noteScopeText, gatewayStatus: e.gatewayStatus)
    }

    /// gatewayError, with one clear line for a run that cannot reach its run port.
    func gwError(_ r: ProjectReply) -> ProjectError {
        if r.status == 0 && runConn != nil {
            return ProjectError("the notes are not reachable from this sandbox (it has no access to the gateway's run port)")
        }
        return Self.gatewayError(r.status, r.data, r.retryAfter)
    }

    func request(_ c: ProjectConn, _ method: String, _ path: String, _ headers: [String: String] = [:], _ body: Data? = nil) -> ProjectReply {
        send(c, method, path, headers, body)
    }

    /// The gateway's version (/health/ready) when it is below 0.6.0, else nil. A gateway that reports none is older still.
    func olderGatewayHint(_ c: ProjectConn) -> String? {
        let r = request(c, "GET", "/health/ready")
        guard r.status == 200, let o = JSONValue.parse(r.data)?.dict else { return nil }
        let v = o["version"]?.string ?? ""
        guard v.allSatisfy({ $0.isASCII && ($0.isNumber || $0 == ".") }) else { return nil }
        if v.isEmpty { return "hint: this gateway is older than 0.6.0 and has no projects (update the gateway)" }
        guard Self.versionLess(v, "0.6.0") else { return nil }
        return "hint: this gateway runs \(v), older than 0.6.0: it has no projects yet (update the gateway)"
    }

    /// versionLess of the Go client: a < b by numbers (0.5.9 < 0.5.10); a missing or non-numeric part is 0.
    static func versionLess(_ a: String, _ b: String) -> Bool {
        let pa = a.split(separator: ".", omittingEmptySubsequences: false).map { Int($0) ?? 0 }, pb = b.split(separator: ".", omittingEmptySubsequences: false).map { Int($0) ?? 0 }
        for i in 0..<max(pa.count, pb.count) {
            let x = i < pa.count ? pa[i] : 0, y = i < pb.count ? pb[i] : 0
            if x != y { return x < y }
        }
        return false
    }

    func loadState() throws -> ProjectState { try Projects.load(state) }

    func stamp(_ d: Date?) -> String {
        guard let d else { return "0001-01-01 00:00" }
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX"); f.timeZone = zone; f.dateFormat = "yyyy-MM-dd HH:mm"
        return f.string(from: d)
    }

    // MARK: project

    private func projectSessions(_ args: [String]) throws {
        guard let fl = try? GoFlags.parse([("days", .int), ("model", .string), ("device", .string), ("json", .bool), ("phase", .string), ("activity", .string), ("task", .string)], args), fl.positional.count <= 1 else { throw ProjectError("usage: minigw project sessions [<project>] [--days n] [--model m] [--device d] [--phase p] [--activity a] [--task t] [--json]") }
        let c: ProjectConn, project: String
        if let ref = fl.positional.first { (c, _) = connect(); project = ref }
        else { let linked = try linkedProject(""); c = linked.0; project = String(linked.1.project) }
        var q = ["days": fl.set.contains("days") ? fl.string("days") : "7", "model": fl.string("model"), "device": fl.string("device")]
        for k in ["phase", "activity", "task"] where !fl.string(k).isEmpty { q[k] = fl.string(k) }
        let page = try SessionHistory.collect { after in
            q["after"] = after.isEmpty ? nil : after  // only a continuation carries a cursor (as the Go client)
            let r = self.request(c, "GET", Self.apiProjects + "/" + ProjectText.queryEscape(project) + "/sessions" + ProjectText.query(q))
            guard r.status == 200 else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
            guard let obj = try JSONSerialization.jsonObject(with: r.data) as? [String: Any] else { throw ProjectError("the sessions answer is not understood") }
            return obj
        }
        out(fl.bool("json") ? try SessionHistory.json(page) + "\n" : SessionHistory.text(page))
    }

    private func runProject(_ args: [String]) throws {
        if agent { throw ProjectError(Self.agentRefusal) }
        guard let sub = args.first else { throw ProjectError("usage:\n" + Self.projectHelp) }
        if sub == "sessions" { try projectSessions(Array(args.dropFirst())); return }
        guard let fl = try? GoFlags.parse([("archived", .bool), ("json", .bool), ("path", .string), ("here", .bool)], Array(args.dropFirst())) else {
            throw ProjectError("usage:\n" + Self.projectHelp)
        }
        let pos = fl.positional
        switch sub {
        case "list":
            let (c, gw) = connect()
            var q: [String: String] = [:]
            if fl.bool("archived") { q["archived"] = "1" }
            if !fl.string("path").isEmpty { q["path"] = fl.string("path") }
            let r = request(c, "GET", Self.apiProjects + ProjectText.query(q))
            guard r.status == 200 else {
                var e = Self.gatewayError(r.status, r.data, r.retryAfter)
                if r.status == 404, let hint = olderGatewayHint(c) { e = ProjectError(e.message + "\n" + hint) }  // a gateway older than 0.6.0 has no projects
                throw e
            }
            if fl.bool("json") { say(ProjectText.trim(String(decoding: r.data, as: UTF8.self))); return }
            let list = JSONValue.parse(r.data)?.array ?? []
            let ps = (try? loadState()) ?? ProjectState()
            let here = try? Projects.resolve(ps, gateway: gw, cwd: cwd)
            for p in list.map(\.dict) {
                var access = p["role"]?.string ?? ""
                if let origins = p["origins"]?.array, !origins.isEmpty { access += " (from " + Self.originText(origins, roles: false) + ")" }
                let granted = (p["granted"]?.array ?? []).compactMap(\.string)
                if !granted.isEmpty {
                    if !access.isEmpty { access += ", " }
                    access += "reads " + granted.joined(separator: "+") + " by grant"
                }
                var line = ProjectText.pad(p["slug"]?.string ?? "", 24) + " " + ProjectText.pad(access, 28) + " " + ProjectText.clean(p["name"]?.string ?? "")
                if p["archived"]?.bool == true { line += "  (archived)" }
                if let h = here, h.state != .none, h.link.project == p["id"]?.int ?? 0 { line += "  (linked here)" }
                let pending = p["pendingReview"]?.int ?? 0
                if pending == 1 { line += "  (1 submission waits for review: minigw kb review \(p["slug"]?.string ?? ""))" }
                else if pending > 1 { line += "  (\(pending) submissions wait for review: minigw kb review \(p["slug"]?.string ?? ""))" }
                say(ProjectText.clean(line))
            }
            if list.isEmpty { say("no projects: ask the gateway owner to add you to one") }
        case "link":
            guard pos.count <= 1 else { throw ProjectError("usage: minigw project link [<slug>]") }
            try link(pos.first ?? "")
        case "remote": try remote(pos, here: fl.bool("here"))
        case "depends": try depends(pos)
        case "unlink":
            guard pos.isEmpty else { throw ProjectError("usage: minigw project unlink") }
            try unlink()
        case "show": try show()
        default: throw ProjectError("unknown command minigw project " + sub + "\n" + Self.projectHelp)
        }
    }

    func link(_ slugArg: String) throws {
        var slug = slugArg
        guard slug.isEmpty || Projects.isSlug(slug) else { throw ProjectError("not a project slug (lower case letters, digits and -): " + slug) }
        let (c, gw) = connect()
        var st = try loadState()
        let real = try Projects.realFolder(cwd)
        for l in st.links where l.gateway == gw && Projects.samePath(real, l.path) {  // a deeper folder may be linked on its own; the same one not twice
            throw ProjectError("this folder is already linked to \(l.slug): run minigw project unlink first")
        }
        if slug.isEmpty { slug = try findByRemote(c, real) }
        let r = request(c, "POST", Self.apiProjects + "/" + ProjectText.pathEscape(slug) + "/link-check")
        guard r.status == 200 else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
        let p = JSONValue.parse(r.data)?.dict ?? JDict()
        let id = p["id"]?.int ?? 0, pslug = p["slug"]?.string ?? ""
        guard id != 0 else { throw ProjectError("the gateway sent no project") }
        st.links.append(ProjectLink(gateway: gw, project: id, slug: pslug, path: real))
        do { try Projects.save(state, st) } catch { throw ProjectError("cannot save the link: " + String(describing: error)) }
        say("linked \(real) to project \(pslug) (id \(id))")
        let h = Projects.readHint(real)
        if !h.isEmpty && h != pslug {
            err("minigw: warning: \(Projects.hintFile) says project \(h); runs here are refused until you remove or fix it\n")
        }
    }

    private func unlink() throws {
        let (_, gw) = connect()
        var st = try loadState()
        let res = try Projects.resolve(st, gateway: gw, cwd: cwd)
        guard res.state != .none else { throw ProjectError("this folder is not linked to a project") }
        st.links = st.links.filter { !($0.gateway == gw && $0.path == res.link.path) }
        do { try Projects.save(state, st) } catch { throw ProjectError("cannot save: " + String(describing: error)) }
        say("unlinked \(res.link.path) from project \(res.link.slug)")
    }

    private func show() throws {
        let (_, gw) = connect()
        let st = try loadState()
        let res = try Projects.resolve(st, gateway: gw, cwd: cwd)
        say("folder:   " + res.realPath)
        switch res.state {
        case .none:
            say("project:  none (not linked)")
            if !res.hint.isEmpty {
                say("hint:     \(Projects.hintFile) in \(res.hintDir) suggests project \(res.hint); run minigw project link \(res.hint)")
            }
        default:
            say("project:  \(res.link.slug) (id \(res.link.project))")
            say("linked:   " + res.link.path + " (the nearest linked folder above or at this one)")
            if res.hint.isEmpty {
                say("hint:     no " + Projects.hintFile + " in the linked folder")
            } else if res.state == .hintDiff {
                say("hint:     \(Projects.hintFile) says \(res.hint) but the link is \(res.link.slug): runs here are refused; run minigw project unlink, link again or remove the hint")
            } else {
                say("hint:     " + Projects.hintFile + " agrees")
            }
        }
        say("gateway: " + gw)
    }

    /// The project of the current folder for the note commands; --project only confirms it.
    func linkedProject(_ flagSlug: String) throws -> (ProjectConn, ProjectLink) {
        let (c, gw) = connect()
        let st = try loadState()
        let res = try Projects.resolve(st, gateway: gw, cwd: cwd)
        switch res.state {
        case .none: throw ProjectError(Self.notLinkedMessage(res))
        case .hintDiff: throw ProjectError(Projects.hintDiffMessage(res))
        case .ok: break
        }
        if !flagSlug.isEmpty && flagSlug != res.link.slug {
            throw ProjectError("this folder is linked to \(res.link.slug), not \(flagSlug): --project only names the linked project (link the other checkout instead)")
        }
        return (c, res.link)
    }

    // MARK: note

    struct NoteView {
        var id: Int64 = 0, revision: Int64 = 0
        var cls = "", title = "", author = ""
        var body: String?
        var updated: Date?
        init(_ d: JDict) {
            id = d["id"]?.int ?? 0; revision = d["revision"]?.int ?? 0
            cls = d["class"]?.string ?? ""; title = d["title"]?.string ?? ""; author = d["author"]?.string ?? ""
            body = d["body"]?.string
            updated = d["updated"]?.string.flatMap(ProjectText.parseTime)
        }
    }

    func readInput(_ file: String) throws -> String {
        var b: Data
        if file == "-" { b = stdin() } else {
            let fd = open(file, O_RDONLY)
            guard fd >= 0 else { throw ProjectError("open \(file): " + String(cString: strerror(errno)).lowercasedFirst) }
            let h = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
            b = (try? h.read(upToCount: Self.maxNoteBody + 1)) ?? Data()
            try? h.close()
        }
        if b.count > Self.maxNoteBody { throw ProjectError("too large: a note body is at most \(Self.maxNoteBody >> 10) KiB") }
        let s = String(decoding: b, as: UTF8.self)
        guard Data(s.utf8) == b else { throw ProjectError("the note must be text (UTF-8)") }
        return s
    }

    /// The first non-empty line, at most maxNoteTitle bytes, else the file name.
    func defaultTitle(_ body: String, _ file: String) -> String {
        for l in body.split(separator: "\n", omittingEmptySubsequences: false) {
            var t = ProjectText.trim(String(l))
            if !t.isEmpty {
                while t.utf8.count > Self.maxNoteTitle { t.unicodeScalars.removeLast() }
                return t
            }
        }
        return file != "-" ? (file as NSString).lastPathComponent : "note"
    }

    private func runNote(_ args: [String]) throws {
        guard let sub = args.first else { throw ProjectError("usage:\n" + Self.noteHelp) }
        if agent && !(runConn != nil && (sub == "list" || sub == "read")) { throw ProjectError(Self.agentRefusal) }
        let usage = ProjectError("usage:\n" + Self.noteHelp)
        guard let fl = try? GoFlags.parse([("class", .string), ("title", .string), ("limit", .int), ("after", .string), ("json", .bool),
                                           ("project", .string), ("revision", .int64), ("key", .string), ("team", .string)], Array(args.dropFirst())) else { throw usage }
        var pos = fl.positional
        guard ["list", "read", "push", "edit", "rm"].contains(sub) else { throw ProjectError("unknown command minigw note " + sub + "\n" + Self.noteHelp) }
        var id: Int64 = 0
        if ["read", "edit", "rm"].contains(sub) {
            guard let first = pos.first else { throw usage }
            guard let v = GoFlags.parseInt(first), v >= 1 else { throw ProjectError("not a note id: " + first) }
            id = v
            pos.removeFirst()
        }
        let asJSON = fl.bool("json")
        let cls = fl.string("class"), title = fl.string("title")
        let c: ProjectConn
        var base = "/minigw/v1/notes"  // inside a run: the run's own project, named nowhere
        let team = fl.string("team")
        if !team.isEmpty && (agent || runConn != nil) { throw ProjectError(Self.teamAgentRefusal) }
        if !team.isEmpty && !fl.string("project").isEmpty { throw ProjectError(Self.teamProjectUsage) }
        if let rc = runConn {
            if !fl.string("project").isEmpty { throw ProjectError("inside an agent run the project is the run's own: --project is not accepted") }
            c = rc
        } else if !team.isEmpty {
            (c, _) = connect()
            base = "\(Self.apiProjects)/\(try teamSegment(c, team))/notes"
        } else {
            let (cc, link) = try linkedProject(fl.string("project"))
            c = cc
            base = "\(Self.apiProjects)/\(link.project)/notes"
        }
        func one(_ method: String, _ body: Data? = nil) throws -> Data {
            let r = request(c, method, "\(base)/\(id)", [:], body)
            guard (200...299).contains(r.status) else { throw Self.scopeHint(gwError(r), scoped: runConn != nil || !team.isEmpty) }
            return r.data
        }
        func emit(_ data: Data, _ f: (NoteView) -> Void) {
            if asJSON { say(ProjectText.jsonText(ProjectText.trim(String(decoding: data, as: UTF8.self)))); return }
            f(NoteView(JSONValue.parse(data)?.dict ?? JDict()))
        }
        func privacy(_ cl: String) {
            guard cl == "private" else { return }
            if asJSON { err(Self.privateNote + "\n") } else { say(Self.privateNote) }
        }
        switch sub {
        case "list":
            guard pos.isEmpty else { throw usage }
            var q: [String: String] = [:]
            if !cls.isEmpty { q["class"] = cls }
            if fl.set.contains("limit") { q["limit"] = String(fl.int("limit")) }
            if !fl.string("after").isEmpty { q["after"] = fl.string("after") }
            let r = request(c, "GET", base + ProjectText.query(q))
            guard r.status == 200 else { throw gwError(r) }
            if asJSON { say(ProjectText.jsonText(ProjectText.trim(String(decoding: r.data, as: UTF8.self)))); return }
            let pg = JSONValue.parse(r.data)?.dict ?? JDict()
            let notes = (pg["notes"]?.array ?? []).map { NoteView($0.dict) }
            for n in notes {
                say(ProjectText.clean("\(ProjectText.pad(String(n.id), 6)) \(ProjectText.pad(n.cls, 9)) r\(ProjectText.pad(String(n.revision), 3)) \(stamp(n.updated))  \(ProjectText.pad(n.author, 12)) \(n.title)"))
            }
            if notes.isEmpty { say("no notes") }
            if let next = pg["next"]?.string, !next.isEmpty { say("more: minigw note list --after " + next) }
        case "read":
            emit(try one("GET")) { n in
                let body = n.body ?? ""
                say(ProjectText.clean("note \(n.id)  \(n.cls)  revision \(n.revision)  by \(n.author)  \(stamp(n.updated))\n\(n.title)\n"))
                out(ProjectText.clean(body.replacingOccurrences(of: "\t", with: "    ")))
                if !body.hasSuffix("\n") { say("") }
            }
        case "push":
            guard pos.count == 1 else { throw usage }
            let body = try readInput(pos[0])
            let cl = cls.isEmpty ? "data" : cls
            let t = title.isEmpty ? defaultTitle(body, pos[0]) : title
            if t.utf8.count > Self.maxNoteTitle { throw ProjectError("too large: a title is at most \(Self.maxNoteTitle) bytes") }
            var k = fl.string("key")
            if k.isEmpty {
                var g = SystemRandomNumberGenerator()
                k = "mgw-" + (0..<16).map { _ in String(format: "%02x", UInt8.random(in: 0...255, using: &g)) }.joined()
            }
            let payload = Data(GoJSON.object(["class": cl, "title": t, "body": body]).utf8)
            var r = ProjectReply(status: 0)
            for _ in 0...Self.pushRetries {  // a lost answer: the same key makes the retry safe (the gateway replays)
                r = request(c, "POST", base, ["Idempotency-Key": k], payload)
                if r.status != 0 { break }
                self.sleep(1)
            }
            if r.status == 0 {
                throw ProjectError("no answer from the gateway: the note may have been created; repeat with --key \(k) (safe: no duplicate), or check with minigw note list")
            }
            guard r.status == 201 else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
            emit(r.data) { n in say("note \(n.id) created (\(n.cls), revision \(n.revision))") }
            privacy(cl)
        case "edit":
            guard pos.count <= 1 else { throw usage }
            var cur = NoteView(JDict())
            if fl.int64("revision") == 0 || pos.isEmpty || cls.isEmpty || !fl.set.contains("title") {  // the current note supplies what the flags do not
                cur = NoteView(JSONValue.parse(try one("GET"))?.dict ?? JDict())
            }
            var n: [String: Any] = ["revision": cur.revision, "class": cur.cls, "title": cur.title, "body": cur.body ?? ""]
            if fl.set.contains("revision") { n["revision"] = fl.int64("revision") }
            if !cls.isEmpty { n["class"] = cls }
            if fl.set.contains("title") { n["title"] = title }
            if pos.count == 1 { n["body"] = try readInput(pos[0]) }
            let data = try one("PUT", Data(GoJSON.object(n).utf8))
            emit(data) { v in say("note \(v.id) updated (\(v.cls), revision \(v.revision))") }
            if let cl = n["class"] as? String, cl == "private" { privacy(cl) }
        case "rm":
            guard pos.isEmpty else { throw usage }
            var rev = fl.int64("revision")
            if !fl.set.contains("revision") { rev = NoteView(JSONValue.parse(try one("GET"))?.dict ?? JDict()).revision }
            let r = request(c, "DELETE", "\(base)/\(id)?revision=\(rev)")
            guard r.status == 204 else { throw Self.scopeHint(Self.gatewayError(r.status, r.data, r.retryAfter), scoped: runConn != nil || !team.isEmpty) }
            say(asJSON ? "{\"deleted\":\(id)}" : "note \(id) deleted")
        default: break
        }
    }

    // MARK: usage

    /// GET /minigw/v1/usage. The project is --project, else the linked project of this folder, else none (then your own events over
    /// all your projects).
    private func runUsage(_ args: [String]) throws {
        if agent { throw ProjectError(Self.agentRefusal) }
        guard let fl = try? GoFlags.parse([("project", .string), ("days", .int), ("json", .bool)], args), fl.positional.isEmpty else {
            throw ProjectError("usage:\n" + Self.usageHelp)
        }
        let (c, gw) = connect()
        var slug = fl.string("project")
        if slug.isEmpty, let st = try? loadState(), let res = try? Projects.resolve(st, gateway: gw, cwd: cwd), res.state == .ok { slug = res.link.slug }
        let days = fl.set.contains("days") ? fl.int("days") : 7
        var q = ["days": String(days)]
        if !slug.isEmpty { q["project"] = slug }
        let r = request(c, "GET", "/minigw/v1/usage" + ProjectText.query(q))
        guard r.status == 200 else {
            if r.status == 404 && !slug.isEmpty {
                throw ProjectError("no usage to show for project " + slug + ": not found, not visible to you, or you are not a developer or admin there")
            }
            throw Self.gatewayError(r.status, r.data, r.retryAfter)
        }
        if fl.bool("json") { say(ProjectText.trim(String(decoding: r.data, as: UTF8.self))); return }
        say(slug.isEmpty ? "your own usage, all projects" : "project " + slug)
        printUsage(JSONValue.parse(r.data)?.dict ?? JDict())
    }

    // MARK: running

    /// GET /minigw/v1/running: the requests of this device that run on the gateway now (the same as the Go client's running).
    private func runRunning(_ args: [String]) throws {
        if agent { throw ProjectError(Self.agentRefusal) }
        guard let fl = try? GoFlags.parse([("json", .bool)], args), fl.positional.isEmpty else {
            throw ProjectError("usage: minigw running [--json]")
        }
        let (c, _) = connect()
        let r = request(c, "GET", "/minigw/v1/running")
        if r.status == 404 { throw ProjectError("this gateway is too old for minigw running: update it") }
        guard r.status == 200 else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
        if fl.bool("json") { say(ProjectText.trim(String(decoding: r.data, as: UTF8.self))); return }
        let list = (JSONValue.parse(r.data)?.dict["running"]?.array ?? []).map(\.dict)
        if list.isEmpty { say("nothing of yours is running on the gateway"); return }
        for x in list {
            let n = x["steps"]?.int ?? 0
            var line = "\(ProjectText.oneLine(x["model"]?.string ?? ""))  \(ProjectText.oneLine(x["folder"]?.string ?? ""))  running \(Self.goDuration(Int(x["elapsedSec"]?.int ?? 0)))  \(n) \(n == 1 ? "step" : "steps")"
            if let last = x["lastStep"]?.string, !last.isEmpty {
                line += "  last: \(RunningRegistry.shortLine(last, 160)) (\(Self.goDuration(Int(x["stepAgoSec"]?.int ?? 0))) ago)"
            }
            say(line)
        }
    }

    /// Go's time.Duration.String for whole seconds: 12s, 6m12s, 1h2m3s.
    static func goDuration(_ sec: Int) -> String {
        let h = sec / 3600, m = sec % 3600 / 60, s = sec % 60
        return h > 0 ? "\(h)h\(m)m\(s)s" : m > 0 ? "\(m)m\(s)s" : "\(s)s"
    }

    /// One line per day, model and user, the totals (each event once; nested events are inside them), and the gaps.
    func printUsage(_ r: JDict) {
        var ev: Int64 = 0, tin: Int64 = 0, tout: Int64 = 0
        let days = (r["days"]?.array ?? []).map(\.dict)
        for d in days {
            let user = d["userName"]?.string ?? ""
            let who = user.isEmpty ? "" : "  " + user
            var extra = ""
            if let n = d["nestedEvents"]?.int, n > 0 { extra += ", \(n) nested" }
            if let n = d["eventsWithoutTokens"]?.int, n > 0 { extra += ", \(n) without token counts" }
            if let n = d["failedEvents"]?.int, n > 0 { extra += ", \(n) failed" }
            let (e, i, o) = (d["events"]?.int ?? 0, d["tokensIn"]?.int ?? 0, d["tokensOut"]?.int ?? 0)
            say(ProjectText.clean("\(d["day"]?.string ?? "")  \(ProjectText.pad(d["model"]?.string ?? "", 24))\(who)  \(e) events, \(i) in, \(o) out\(extra)"))
            ev += e; tin += i; tout += o
        }
        if days.isEmpty { say("no events") } else { say("total: \(ev) events, \(tin) in, \(tout) out (every event once, nested ones included)") }
        for g in (r["gaps"]?.array ?? []).map(\.dict) {
            say("\(g["count"]?.int ?? 0) events lost between \(stamp(g["from"]?.string.flatMap(ProjectText.parseTime))) and \(stamp(g["to"]?.string.flatMap(ProjectText.parseTime)))")
        }
    }
}

// MARK: small helpers shared with the Go behaviour

enum ProjectText {
    /// A character clean removes: C0 except \n, C1, the line separators and every format character.
    static func isTerminalControl(_ u: Unicode.Scalar) -> Bool {
        u != "\n" && (u.value < 0x20 || (0x7f...0x9f).contains(u.value) || u.value == 0x2028 || u.value == 0x2029
                         || u.properties.generalCategory == .format)
    }

    /// A gateway's text without control characters (except \n), so that it cannot steer the terminal; the same set as the client's clean().
    static func clean(_ s: String) -> String {
        String(String.UnicodeScalarView(s.unicodeScalars.filter { !isTerminalControl($0) }))
    }

    /// The gateway's JSON with those characters escaped (\uXXXX) inside every string: the JSON stays valid and a program
    /// that parses it gets the text as sent, while a person's terminal sees only ASCII.
    static func jsonText(_ s: String) -> String {
        var o = String.UnicodeScalarView()
        var inString = false, escaped = false
        for u in s.unicodeScalars {
            if !inString {
                if u == "\"" { inString = true }
                o.append(u)
                continue
            }
            if escaped { o.append(u); escaped = false; continue }
            switch u {
            case "\\": o.append(u); escaped = true
            case "\"": o.append(u); inString = false
            default:
                if !isTerminalControl(u) { o.append(u) }
                else if u.value > 0xffff {
                    let v = u.value - 0x10000
                    o.append(contentsOf: String(format: "\\u%04x\\u%04x", 0xd800 + (v >> 10), 0xdc00 + (v & 0x3ff)).unicodeScalars)
                } else { o.append(contentsOf: String(format: "\\u%04x", u.value).unicodeScalars) }
            }
        }
        return String(o)
    }

    static func oneLine(_ s: String) -> String { clean(s).replacingOccurrences(of: "\n", with: " ") }
    static func trim(_ s: String) -> String { s.trimmingCharacters(in: .whitespacesAndNewlines) }

    /// fmt's %-Ns: padded on the right to N characters (runes, as Go counts them).
    static func pad(_ s: String, _ n: Int) -> String { s + String(repeating: " ", count: max(0, n - s.unicodeScalars.count)) }

    /// url.QueryEscape
    static func queryEscape(_ s: String) -> String {
        var o = ""
        for b in s.utf8 {
            switch b {
            case 0x41...0x5a, 0x61...0x7a, 0x30...0x39, 0x2d, 0x5f, 0x2e, 0x7e: o.unicodeScalars.append(Unicode.Scalar(b))
            case 0x20: o += "+"
            default: o += String(format: "%%%02X", b)
            }
        }
        return o
    }

    /// "?" + url.Values.Encode(): keys sorted, "" for no values.
    static func query(_ q: [String: String]) -> String {
        q.isEmpty ? "" : "?" + q.keys.sorted().map { queryEscape($0) + "=" + queryEscape(q[$0]!) }.joined(separator: "&")
    }

    /// url.PathEscape (a slug is validated first, so this only matters for the contract's sake).
    static func pathEscape(_ s: String) -> String {
        var o = ""
        for b in s.utf8 {
            switch b {
            case 0x41...0x5a, 0x61...0x7a, 0x30...0x39, 0x2d, 0x5f, 0x2e, 0x7e, 0x24, 0x26, 0x2b, 0x3a, 0x3d, 0x40: o.unicodeScalars.append(Unicode.Scalar(b))
            default: o += String(format: "%%%02X", b)
            }
        }
        return o
    }

    /// RFC 3339 with or without fractional seconds (time.Time's JSON).
    static func parseTime(_ s: String) -> Date? {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = f.date(from: s) { return d }
        f.formatOptions = [.withInternetDateTime]
        return f.date(from: s)
    }
}

/// The parts of a parsed JSON document the commands read; absent or mistyped fields read as the zero value, as Go's decoding into a
/// struct whose errors are ignored does (keys are matched case-insensitively there too).
enum JSONValue {
    case obj([String: JSONValue]), arr([JSONValue]), str(String), num(NSNumber), bool(Bool), null

    static func parse(_ d: Data) -> JSONValue? {
        (try? JSONSerialization.jsonObject(with: d, options: [.fragmentsAllowed])).map(wrap)
    }

    private static func wrap(_ x: Any) -> JSONValue {
        switch x {
        case let d as [String: Any]:
            var m: [String: JSONValue] = [:]
            for (k, v) in d { m[k.lowercased()] = wrap(v) }
            return .obj(m)
        case let a as [Any]: return .arr(a.map(wrap))
        case let s as String: return .str(s)
        case let n as NSNumber: return CFGetTypeID(n) == CFBooleanGetTypeID() ? .bool(n.boolValue) : .num(n)
        default: return .null
        }
    }

    var dict: JDict { if case .obj(let m) = self { return JDict(m) }; return JDict() }
    var array: [JSONValue] { if case .arr(let a) = self { return a }; return [] }
    var string: String? { if case .str(let s) = self { return s }; return nil }
    var int: Int64? { if case .num(let n) = self { return n.int64Value }; return nil }
    var bool: Bool? { if case .bool(let b) = self { return b }; return nil }
}

/// A JSON object whose keys are found case-insensitively ("userName" as "username").
struct JDict: ExpressibleByDictionaryLiteral {
    let m: [String: JSONValue]
    init(_ m: [String: JSONValue] = [:]) { self.m = m }
    init(dictionaryLiteral elements: (String, JSONValue)...) { m = Dictionary(elements, uniquingKeysWith: { $1 }) }
    subscript(k: String) -> JSONValue? { m[k.lowercased()] }
}

/// Go's flag package as parseFlags uses it: -f and --f, "-f=v" and "-f v", "--" ends the flags, flags and positionals in any order.
struct GoFlags {
    enum Kind { case bool, string, int, int64 }
    var values: [String: String] = [:]
    var set: Set<String> = []
    var positional: [String] = []

    func bool(_ n: String) -> Bool { values[n] == "true" }
    func string(_ n: String) -> String { values[n] ?? "" }
    func int(_ n: String) -> Int { Int(values[n].flatMap { Self.parseInt($0) } ?? 0) }
    func int64(_ n: String) -> Int64 { values[n].flatMap { Self.parseInt($0) } ?? 0 }

    struct Failure: Error {}

    static func parse(_ specs: [(String, Kind)], _ args: [String]) throws -> GoFlags {
        var r = GoFlags()
        var args = args[...]
        while true {
            // one fs.Parse: flags up to the first positional (or "--")
            while let s = args.first, s.count >= 2, s.hasPrefix("-") {
                var name = s.dropFirst()
                args = args.dropFirst()
                if name.hasPrefix("-") {
                    name = name.dropFirst()
                    if name.isEmpty { break }  // "--": the flags end here
                }
                guard let c = name.first, c != "-", c != "=" else { throw Failure() }
                var value: String?
                if let eq = name.firstIndex(of: "=") { value = String(name[name.index(after: eq)...]); name = name[..<eq] }
                guard let kind = specs.first(where: { $0.0 == name })?.1 else { throw Failure() }  // unknown, or -h/-help
                let n = String(name)
                switch kind {
                case .bool:
                    if let v = value {
                        guard let b = parseBool(v) else { throw Failure() }
                        r.values[n] = b ? "true" : "false"
                    } else { r.values[n] = "true" }
                default:
                    if value == nil, let next = args.first { value = next; args = args.dropFirst() }
                    guard let v = value else { throw Failure() }
                    if kind != .string { guard parseInt(v) != nil else { throw Failure() } }
                    r.values[n] = v
                }
                r.set.insert(n)
            }
            guard let first = args.first else { return r }
            r.positional.append(first)
            args = args.dropFirst()
        }
    }

    static func parseBool(_ s: String) -> Bool? {
        switch s {
        case "1", "t", "T", "TRUE", "true", "True": return true
        case "0", "f", "F", "FALSE", "false", "False": return false
        default: return nil
        }
    }

    /// strconv.ParseInt(s, 0, 64): optional sign, 0x/0b/0o prefixes, a leading 0 for octal, "_" between digits.
    static func parseInt(_ s: String) -> Int64? {
        var t = Substring(s)
        var neg = false
        if t.hasPrefix("+") || t.hasPrefix("-") { neg = t.hasPrefix("-"); t = t.dropFirst() }
        var radix = 10
        if t.hasPrefix("0x") || t.hasPrefix("0X") { radix = 16; t = t.dropFirst(2) }
        else if t.hasPrefix("0b") || t.hasPrefix("0B") { radix = 2; t = t.dropFirst(2) }
        else if t.hasPrefix("0o") || t.hasPrefix("0O") { radix = 8; t = t.dropFirst(2) }
        else if t.count > 1 && t.hasPrefix("0") { radix = 8; t = t.dropFirst() }
        if radix != 10 || s.contains("_") {
            guard !t.hasPrefix("_"), !t.hasSuffix("_"), !t.contains("__") else { return nil }
            t = Substring(t.replacingOccurrences(of: "_", with: ""))
        }
        guard !t.isEmpty, let v = Int64(neg ? "-" + t : String(t), radix: radix) else { return nil }
        return v
    }
}
