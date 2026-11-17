import Foundation

// The registered git identity of a project: minigw project link without a slug, minigw project remote list|add|remove and
// minigw kb faq. A port of the matching parts of client/project_cmd.go and client/kb_cmd.go: the same texts and requests
// (client/testdata/project-commands.json and teams-kb-submissions.json are asserted here and in Go). The gateway normalizes
// the remote; this side only removes the credentials of a URL and reads the folder's origin.

extension ProjectEnv {
    /// The credentials of a URL remote; they never leave this computer.
    static func stripUserinfo(_ raw: String) -> String {
        let s = ProjectText.trim(raw)
        guard let re = try? NSRegularExpression(pattern: #"^([A-Za-z][A-Za-z0-9+.-]*://)[^/@]*@"#) else { return s }
        return re.stringByReplacingMatches(in: s, range: NSRange(s.startIndex..., in: s), withTemplate: "$1")
    }

    /// The folder's raw remote.origin.url from the repository's own config (no insteadOf rewrite, no hooks). Git missing, no origin
    /// or an untrusted checkout all give "".
    static func gitOrigin(_ dir: String) -> String {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        p.arguments = ["git", "config", "--local", "--get", "remote.origin.url"]
        p.currentDirectoryURL = URL(fileURLWithPath: dir)
        var env = ProcessInfo.processInfo.environment
        env["GIT_TERMINAL_PROMPT"] = "0"
        p.environment = env
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = FileHandle.nullDevice
        p.standardInput = FileHandle.nullDevice
        let done = DispatchSemaphore(value: 0)
        p.terminationHandler = { _ in done.signal() }
        do { try p.run() } catch { return "" }
        if done.wait(timeout: .now() + 10) == .timedOut { p.terminate(); return "" }
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        guard p.terminationStatus == 0 else { return "" }
        return ProjectText.trim(String(decoding: data, as: UTF8.self))
    }

    /// The folder's origin without credentials, or the error "no git remote origin ...".
    func folderRemote(_ dir: String) throws -> String {
        let raw = Self.stripUserinfo((origin ?? Self.gitOrigin)(dir))
        if raw.isEmpty { throw ProjectError("no git remote origin in this folder") }
        return raw
    }

    /// The gateway names the caller's project registered for the folder's origin.
    func findByRemote(_ c: ProjectConn, _ dir: String) throws -> String {
        let hint = "; run minigw project link <slug>"
        let raw: String
        do { raw = try folderRemote(dir) } catch let e as ProjectError { throw ProjectError(e.message + hint) }
        let body = Data(Self.kbJSON(["remote": raw]).utf8)
        let r = request(c, "POST", Self.apiProjects + "/by-remote", [:], body)
        if r.status == 400 { throw ProjectError("the git remote origin of this folder is not a remote URL minigw can use" + hint) }
        guard r.status == 200 else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
        guard let o = JSONValue.parse(r.data)?.dict else { throw Self.malformed }
        let identity = o["identity"]?.string ?? ""
        let slugs = (o["projects"]?.array ?? []).map { $0.dict["slug"]?.string ?? "" }
        switch slugs.count {
        case 0: throw ProjectError("no project of yours is registered for this git remote (\(identity))" + hint)
        case 1:
            say("found project \(slugs[0]) by git remote \(identity)")
            return slugs[0]
        default: throw ProjectError("this git remote matches several of your projects (\(slugs.joined(separator: ", ")))" + hint)
        }
    }

    /// A project given by slug (or id) to its id.
    func projectIDOf(_ c: ProjectConn, _ ref: String) throws -> Int64 {
        let r = request(c, "GET", Self.apiProjects + "/" + ProjectText.pathEscape(ref))
        guard r.status == 200 else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
        return JSONValue.parse(r.data)?.dict["id"]?.int ?? 0
    }

    /// minigw project remote list [<slug>] | add <url>|--here [<slug>] | remove <url> [<slug>]
    func remote(_ pos: [String], here: Bool) throws {
        let usage = ProjectError("usage:\n" + Self.projectHelp)
        guard let op = pos.first, ["list", "add", "remove"].contains(op), !here || op == "add" else { throw usage }
        var rest = Array(pos.dropFirst())
        var raw = ""
        if (op == "add" && here) || op == "list" {
            guard rest.count <= 1 else { throw usage }
        } else {
            guard (1...2).contains(rest.count) else { throw usage }
            raw = Self.stripUserinfo(rest[0]); rest.removeFirst()
        }
        let c: ProjectConn, project: Int64
        var slug = ""
        if rest.count == 1 {
            (c, _) = connect()
            slug = rest[0]
            project = try projectIDOf(c, slug)
        } else {
            let linked = try linkedProject("")
            c = linked.0; project = linked.1.project; slug = linked.1.slug
        }
        let path = "\(Self.apiProjects)/\(project)/remotes"
        if op == "list" {
            let r = request(c, "GET", path)
            guard r.status == 200 else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
            let list = (JSONValue.parse(r.data)?.dict["remotes"]?.array ?? []).compactMap(\.string)
            for id in list { say(ProjectText.clean(id)) }
            if list.isEmpty { say("no remotes registered") }
            return
        }
        if here { raw = try folderRemote(try Projects.realFolder(cwd)) }
        let r = request(c, "POST", path, [:], Data(Self.kbJSON([op: [raw]]).utf8))
        guard r.status == 200 else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
        let named = (JSONValue.parse(r.data)?.dict[op == "add" ? "added" : "removed"]?.array ?? []).compactMap(\.string)
        guard named.count == 1 else { throw ProjectError("the gateway did not name the remote") }
        say(ProjectText.clean(op == "add" ? "registered \(named[0]) for project \(slug)" : "removed \(named[0]) from project \(slug)"))
    }

    /// minigw kb faq [<project>] [--json]; the pages are followed (at most 20).
    func kbFAQ(_ args: [String]) throws {
        let usage = ProjectError("usage: minigw kb faq [<project>] [--json]")
        guard let fl = try? GoFlags.parse([("json", .bool)], args), fl.positional.count <= 1 else { throw usage }
        let c: ProjectConn, project: Int64
        if let ref = fl.positional.first {
            (c, _) = connect()
            project = try projectIDOf(c, ref)
        } else {
            let linked = try linkedProject("")
            c = linked.0; project = linked.1.project
        }
        var entries: [JDict] = []
        var raw: [[String: Any]] = []
        var after = ""
        for _ in 0..<20 {
            var path = "\(Self.apiProjects)/\(project)/kb/faq"
            if !after.isEmpty { path += "?after=" + ProjectText.queryEscape(after) }
            let r = request(c, "GET", path)
            guard r.status == 200 else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
            guard case .obj(let pg)? = JSONValue.parse(r.data) else { throw Self.malformed }
            entries += (pg["entries"]?.array ?? []).map(\.dict)
            let obj = try JSONSerialization.jsonObject(with: r.data) as? [String: Any]
            raw += obj?["entries"] as? [[String: Any]] ?? []
            after = pg["next"]?.string ?? ""
            if after.isEmpty { break }
        }
        if fl.bool("json") { say(ProjectText.jsonText(Self.kbJSON(["entries": raw]))); return }
        for v in entries {
            say(ProjectText.clean("\(v["id"]?.int ?? 0)  \(v["title"]?.string ?? "")  \(v["class"]?.string ?? "")  \(stamp(v["updated"]?.string.flatMap(ProjectText.parseTime)))"))
            say(ProjectText.clean(v["answer"]?.string ?? ""))
        }
        if entries.isEmpty { say("no faq entries") }
    }
}
