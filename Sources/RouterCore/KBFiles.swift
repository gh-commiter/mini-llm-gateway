import Foundation
import CryptoKit

extension ProjectEnv {
    static func markdown(_ d: JDict) -> Data {
        let title = d["title"]?.string ?? "", body = d["body"]?.string ?? ""
        let hash = SHA256.hash(data: Data(("[" + GoJSON.string(title) + "," + GoJSON.string(body) + "]").utf8)).map { String(format: "%02x", $0) }.joined()
        let fields: [(String, String)] = [
            ("id", String(d["id"]?.int ?? 0)), ("title", title), ("class", d["class"]?.string ?? ""),
            ("kind", d["provenance"]?.dict["sourcekind"]?.string ?? ""), ("revision", String(d["revision"]?.int ?? 0)),
            ("author", d["author"]?.string ?? ""), ("created", d["created"]?.string ?? ""),
            ("updated", d["updated"]?.string ?? ""), ("hash", hash)
        ]
        return Data(("---\n" + fields.map { $0.0 + ": " + GoJSON.string($0.1) + "\n" }.joined() + "---\n" + body).utf8)
    }

    func kbFiles(_ args: [String]) throws {
        if agent { throw ProjectError(Self.agentRefusal) }
        let usage = ProjectError("usage:\n" + Self.kbHelp)
        guard let fl = try? GoFlags.parse([("class", .string), ("submit", .bool)], args), fl.positional.count == 3 else { throw usage }
        let pos = fl.positional
        guard ["export", "import"].contains(pos[0]) else { throw usage }
        if pos[0] == "export" && (fl.set.contains("class") || fl.bool("submit")) { throw ProjectError("export takes no import options") }
        let (c, _) = connect()
        func request(_ method: String, _ path: String, _ body: Data? = nil) throws -> JDict {
            while true {
                let r = send(c, method, path, [:], body)
                if r.status == 429 {
                    let seconds = Int(r.retryAfter ?? "") ?? 60
                    sleep(TimeInterval((1...60).contains(seconds) ? seconds : 60))
                    continue
                }
                guard (200...299).contains(r.status) else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
                guard let d = JSONValue.parse(r.data)?.dict else { throw Self.malformed }
                return d
            }
        }
        let p = try request("GET", Self.apiProjects + "/" + ProjectText.pathEscape(pos[1]))
        guard let id = p["id"]?.int else { throw Self.malformed }
        let base = "\(Self.apiProjects)/\(id)"
        let root = URL(fileURLWithPath: pos[2], relativeTo: URL(fileURLWithPath: cwd, isDirectory: true)).standardizedFileURL
        let fm = FileManager.default
        if pos[0] == "export" {
            var notes: [JDict] = [], after = ""
            repeat {
                let pg = try request("GET", base + "/notes?limit=100&after=" + ProjectText.queryEscape(after))
                for n in pg["notes"]?.array ?? [] {
                    notes.append(try request("GET", base + "/notes/\(n.dict["id"]?.int ?? 0)"))
                }
                after = pg["next"]?.string ?? ""
            } while !after.isEmpty
            try fm.createDirectory(at: root, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            guard try fm.contentsOfDirectory(atPath: root.path).isEmpty else { throw ProjectError("export directory must be empty") }
            func write(_ name: String, _ data: Data) throws {
                let url = root.appendingPathComponent(name)
                // O_EXCL refuses existing files and links. Permissions apply before content is written.
                let fd = open(url.path, O_WRONLY | O_CREAT | O_EXCL, 0o600)
                guard fd >= 0 else { throw ProjectError("cannot create " + url.path) }
                let handle = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
                try handle.write(contentsOf: data)
                try handle.close()
            }
            var index = "# " + pos[1] + "\n\n"
            for n in notes {
                let name = "\(n["id"]?.int ?? 0).md"
                try write(name, Self.markdown(n))
                index += "- [\((n["title"]?.string ?? "").replacingOccurrences(of: "\n", with: " "))](\(name))\n"
            }
            try write("INDEX.md", Data(index.utf8))
            say("export \(notes.count) notes")
            return
        }
        let attrs = try fm.attributesOfItem(atPath: root.path)
        let isDir = attrs[.type] as? FileAttributeType == .typeDirectory
        var files: [URL] = []
        if isDir {
            var walkError: Error?
            guard let walk = fm.enumerator(at: root, includingPropertiesForKeys: [.isRegularFileKey, .isSymbolicLinkKey, .isDirectoryKey], options: [], errorHandler: { _, e in walkError = e; return false }) else { throw usage }
            for case let file as URL in walk {
                let v = try file.resourceValues(forKeys: [.isRegularFileKey, .isSymbolicLinkKey, .isDirectoryKey])
                if v.isSymbolicLink == true { throw ProjectError("not a regular file: " + file.path) }
                if v.isDirectory == true { continue }
                if file.lastPathComponent.lowercased() == "index.md" || file.pathExtension.lowercased() != "md" { continue }
                guard v.isRegularFile == true else { throw ProjectError("not a regular file: " + file.path) }
                files.append(file)
            }
            if let walkError { throw walkError }
        } else {
            guard attrs[.type] as? FileAttributeType == .typeRegular, root.pathExtension.lowercased() == "md" else { throw ProjectError("import requires a Markdown file or directory") }
            files = [root]
        }
        for file in files.sorted(by: { $0.path < $1.path }) {
            let handle = try FileHandle(forReadingFrom: file)
            let data = try handle.read(upToCount: 96 << 10) ?? Data()
            try handle.close()
            guard data.count < 96 << 10 else { throw ProjectError("too large") }
            guard let text = String(data: data, encoding: .utf8) else { throw ProjectError("the note must be text (UTF-8)") }
            let rel = isDir ? String(file.path.dropFirst(root.path.count + 1)) : file.lastPathComponent
            let payload = try JSONSerialization.data(withJSONObject: ["markdown": text, "path": rel, "class": fl.string("class"), "submit": fl.bool("submit")])
            guard payload.count <= 512 << 10 else { throw ProjectError("too large") }
            let r = try request("POST", base + "/kb/import", payload)
            var line = (r["action"]?.string ?? "") + " " + (r["key"]?.string ?? "")
            if let reason = r["reason"]?.string, !reason.isEmpty { line += ": " + reason }
            say(ProjectText.clean(line))
        }
    }
}

extension AdminCommands {
    public func bundle(_ args: [String]) throws {
        let usage = fail(Self.help.trimmingCharacters(in: .whitespacesAndNewlines))
        guard let command = args.first, ["export", "import"].contains(command) else { throw usage }
        var team = "", projects: [String] = [], mapping: [String: String] = [:], file = ""
        var withPrivate = false, dry = false, i = 1
        while i < args.count {
            let arg = args[i]
            if arg == "--with-private" { withPrivate = true }
            else if arg == "--dry-run" { dry = true }
            else if arg.hasPrefix("--") {
                let kv = String(arg.dropFirst(2)).split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false).map(String.init)
                let name = kv[0]
                guard ["team", "project", "map"].contains(name) else { throw usage }
                let value: String
                if kv.count == 2 { value = kv[1] }
                else { i += 1; guard i < args.count else { throw usage }; value = args[i] }
                if name == "team" { team = value }
                if name == "project" { projects.append(value) }
                if name == "map" {
                    let pair = value.split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false).map(String.init)
                    guard pair.count == 2, !pair[0].isEmpty, !pair[1].isEmpty, mapping[pair[0]] == nil else { throw usage }
                    mapping[pair[0]] = pair[1]
                }
            } else {
                guard file.isEmpty else { throw usage }; file = arg
            }
            i += 1
        }
        guard !file.isEmpty else { throw usage }
        let path = URL(fileURLWithPath: file).standardizedFileURL.path
        let body: [String: Any]
        if command == "export" {
            guard !team.isEmpty, !dry, mapping.isEmpty else { throw usage }
            body = ["team": team, "projects": projects, "with_private": withPrivate, "path": path]
        } else {
            guard team.isEmpty, projects.isEmpty, !withPrivate else { throw usage }
            body = ["path": path, "dry_run": dry, "map": mapping]
        }
        let r = try request("POST", "/" + command, body)
        for line in r["lines"]?.array ?? [] { say(clean(line.string ?? "")) }
    }
}
