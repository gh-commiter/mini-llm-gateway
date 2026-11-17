import Foundation
import CryptoKit

extension ProjectEnv {
    static func markdown(_ d: JDict) -> Data {
        let title = d["title"]?.string ?? "", body = d["body"]?.string ?? ""
        let hash = SHA256.hash(data: Data(("[" + GoJSON.string(title) + "," + GoJSON.string(body) + "]").utf8)).map { String(format: "%02x", $0) }.joined()
        let fields: [(String, String)] = [
            ("id", String(d["id"]?.int ?? 0)), ("title", title), ("class", d["class"]?.string ?? ""),
            ("kind", d["provenance"]?.dict["sourcekind"]?.string ?? ""),
            ("entry_kind", (d["entrykind"]?.string).flatMap { $0.isEmpty ? nil : $0 } ?? "finding"), ("revision", String(d["revision"]?.int ?? 0)),
            ("author", d["author"]?.string ?? ""), ("created", d["created"]?.string ?? ""),
            ("updated", d["updated"]?.string ?? ""), ("hash", hash)
        ]
        return Data(("---\n" + fields.map { $0.0 + ": " + GoJSON.string($0.1) + "\n" }.joined() + "---\n" + body).utf8)
    }

    /// The note's bytes: the file is opened once without following a link and checked on that descriptor.
    func readNoteFile(_ path: String, root: Int32? = nil) throws -> Data {
        var parent = root
        var held: [Int32] = []
        defer { for fd in held.reversed() { close(fd) } }
        let parts = path.split(separator: "/").map(String.init)
        if let root {
            parent = root
            for part in parts.dropLast() {
                guard part != "..", part != "." else { throw ProjectError("invalid import path") }
                let next = openat(parent!, part, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_NONBLOCK)
                guard next >= 0 else { throw ProjectError("not a directory: " + path) }
                held.append(next)
                parent = next
            }
        }
        let fd = parent.map { openat($0, parts.last ?? "", O_RDONLY | O_NOFOLLOW | O_NONBLOCK) }
            ?? open(path, O_RDONLY | O_NOFOLLOW | O_NONBLOCK)
        guard fd >= 0 else { throw ProjectError("not a regular file: " + path) }
        defer { close(fd) }
        var st = stat()
        guard fstat(fd, &st) == 0, st.st_mode & S_IFMT == S_IFREG else { throw ProjectError("not a regular file: " + path) }
        let limit = 96 << 10
        var data = Data(), buf = [UInt8](repeating: 0, count: 16 << 10)
        while true {
            let n = read(fd, &buf, min(buf.count, limit - data.count + 1))
            if n < 0 { throw ProjectError("cannot read: " + path) }
            if n == 0 { return data }
            data.append(buf, count: n)
            if data.count >= limit { throw ProjectError("too large") }
        }
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
            // The destination is opened once, never through a link, and held: the check and every file go through this descriptor.
            let dirFD = open(root.path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW)
            guard dirFD >= 0 else { throw ProjectError("export destination must be a directory, not a symbolic link") }
            defer { close(dirFD) }
            let listFD = dup(dirFD)
            guard listFD >= 0, let dir = fdopendir(listFD) else { throw ProjectError("cannot read export directory") }
            var empty = true
            while let e = readdir(dir) {
                let name = withUnsafePointer(to: &e.pointee.d_name) { $0.withMemoryRebound(to: CChar.self, capacity: 256) { String(cString: $0) } }
                if name != "." && name != ".." { empty = false; break }
            }
            closedir(dir)
            guard empty else { throw ProjectError("export directory must be empty") }
            exportHook?()
            func write(_ name: String, _ data: Data) throws {
                // O_EXCL refuses existing files and links. Permissions apply before content is written.
                let fd = openat(dirFD, name, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW, 0o600)
                guard fd >= 0 else { throw ProjectError("cannot create " + root.appendingPathComponent(name).path) }
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
        // The enumerator returns resolved paths (/private/var/…) also for a root given through a symbolic link (/var/…): compare resolved
        // paths, or the relative path (the stable import key) would start in the middle of a folder name.
        let rootPath = root.resolvingSymlinksInPath().path
        let rootFD = isDir ? open(rootPath, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_NONBLOCK) : -1
        defer { if rootFD >= 0 { close(rootFD) } }
        if isDir && rootFD < 0 { throw ProjectError("cannot open import folder") }
        var files: [(url: URL, rel: String)] = []
        if isDir {
            var walkError: Error?
            guard let walk = fm.enumerator(at: root, includingPropertiesForKeys: [.isRegularFileKey, .isSymbolicLinkKey, .isDirectoryKey], options: [], errorHandler: { _, e in walkError = e; return false }) else { throw usage }
            var visited = 0
            for case let file as URL in walk {
                visited += 1
                if visited > 10000 { throw ProjectError("too many entries") }
                if walk.level > 32 { throw ProjectError("import folder is too deep") }
                let v = try file.resourceValues(forKeys: [.isRegularFileKey, .isSymbolicLinkKey, .isDirectoryKey])
                if v.isSymbolicLink == true { throw ProjectError("not a regular file: " + file.path) }
                if v.isDirectory == true { continue }
                if file.lastPathComponent.lowercased() == "index.md" || file.pathExtension.lowercased() != "md" { continue }
                guard v.isRegularFile == true else { throw ProjectError("not a regular file: " + file.path) }
                let full = file.resolvingSymlinksInPath().path
                guard full.hasPrefix(rootPath + "/") else { throw ProjectError("not inside the import folder: " + file.path) }
                let rel = String(full.dropFirst(rootPath.count + 1))
                if rel.filter({ $0 == "/" }).count > 32 { throw ProjectError("import folder is too deep") }
                if files.count >= 10000 { throw ProjectError("too many files") }
                files.append((file, rel))
            }
            if let walkError { throw walkError }
        } else {
            guard root.pathExtension.lowercased() == "md" else { throw ProjectError("import requires a Markdown file or directory") }
            files = [(root, root.lastPathComponent)]
        }
        for (file, rel) in files.sorted(by: { $0.url.path < $1.url.path }) {
            let data = try readNoteFile(isDir ? rel : file.path, root: isDir ? rootFD : nil)
            guard let text = String(data: data, encoding: .utf8) else { throw ProjectError("the note must be text (UTF-8)") }
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
        var team = "", source = "", projects: [String] = [], mapping: [String: String] = [:], file = ""
        var withPrivate = false, dry = false, i = 1
        while i < args.count {
            let arg = args[i]
            if arg == "--with-private" { withPrivate = true }
            else if arg == "--dry-run" { dry = true }
            else if arg.hasPrefix("--") {
                let kv = String(arg.dropFirst(2)).split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false).map(String.init)
                let name = kv[0]
                guard ["team", "project", "map", "source"].contains(name) else { throw usage }
                let value: String
                if kv.count == 2 { value = kv[1] }
                else { i += 1; guard i < args.count else { throw usage }; value = args[i] }
                if name == "team" { team = value }
                if name == "source" { source = value }
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
            guard !team.isEmpty, source.isEmpty, !dry, mapping.isEmpty else { throw usage }
            body = ["team": team, "projects": projects, "with_private": withPrivate, "path": path]
        } else {
            guard team.isEmpty, projects.isEmpty, !withPrivate, !source.isEmpty else { throw usage }
            body = ["path": path, "source": source, "dry_run": dry, "map": mapping]
        }
        let r = try request("POST", "/" + command, body)
        for line in r["lines"]?.array ?? [] { say(clean(line.string ?? "")) }
    }
}
