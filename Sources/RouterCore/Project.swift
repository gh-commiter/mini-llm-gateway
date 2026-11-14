import Foundation

// Project links of the Mac client (0.6.0): which gateway project a folder tree belongs to. A port of client/project.go with
// the same behaviour and the same file format. The state is private to this user and outside every repository:
// ~/Library/Application Support/minigw/projects.json, which is where the Go client keeps it on macOS too
// (os.UserConfigDir) and next to the Swift client's own shared-folders file. A link is explicit, per folder (the folder's
// real path); .minigw-project is only a hint that is shown to the user and never trusted as a link.
// Parity with Go is asserted by the shared fixtures client/testdata/project-*.json (Tests/RouterCoreTests/ProjectTests.swift).

public struct ProjectError: Error, CustomStringConvertible {
    public let message: String
    public init(_ message: String) { self.message = message }
    public var description: String { message }
}

/// One folder tree linked to one project of one gateway. `path` is the canonical real path of the folder; `slug` is for display
/// only (the project id is what counts). `gateway` is the gateway's public key (base64url), not the profile name.
public struct ProjectLink: Equatable, Codable, Sendable {
    public var gateway: String
    public var project: Int64
    public var slug: String
    public var path: String
    public init(gateway: String, project: Int64, slug: String, path: String) {
        self.gateway = gateway; self.project = project; self.slug = slug; self.path = path
    }
    // Go leaves missing fields at their zero value
    public init(from d: Decoder) throws {
        let c = try d.container(keyedBy: Key.self)
        gateway = try c.decodeIfPresent(String.self, forKey: .gateway) ?? ""
        project = try c.decodeIfPresent(Int64.self, forKey: .project) ?? 0
        slug = try c.decodeIfPresent(String.self, forKey: .slug) ?? ""
        path = try c.decodeIfPresent(String.self, forKey: .path) ?? ""
    }
    enum Key: String, CodingKey { case gateway, project, slug, path }
}

public struct ProjectState: Equatable, Sendable {
    public var version = 0
    public var links: [ProjectLink] = []
    public init(links: [ProjectLink] = []) { self.links = links }
}

enum ProjectStateFile: Decodable {
    case state(ProjectState)
    init(from d: Decoder) throws {
        let c = try d.container(keyedBy: Key.self)
        var s = ProjectState()
        s.version = try c.decodeIfPresent(Int.self, forKey: .version) ?? 0
        s.links = try c.decodeIfPresent([ProjectLink].self, forKey: .links) ?? []
        self = .state(s)
    }
    enum Key: String, CodingKey { case version, links }
}

public enum Projects {
    public static let hintFile = ".minigw-project"

    /// ^[a-z0-9][a-z0-9-]{1,62}$ (a slug; also what a hint may contain).
    public static func isSlug(_ s: String) -> Bool {
        let u = Array(s.unicodeScalars)
        guard (2...63).contains(u.count) else { return false }
        func lower(_ c: Unicode.Scalar) -> Bool { ("a"..."z").contains(c) || ("0"..."9").contains(c) }
        return lower(u[0]) && u.dropFirst().allSatisfy { lower($0) || $0 == "-" }
    }

    public static func statePath() -> String {
        FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("minigw/projects.json").path
    }

    // MARK: state file

    /// A missing file is an empty state; a file that cannot be read or parsed is an error, never an empty state.
    public static func load(_ path: String) throws -> ProjectState {
        var probe = stat()
        if stat(path, &probe) != 0 && errno == ENOENT { return ProjectState() }
        let data: Data
        do { data = try Data(contentsOf: URL(fileURLWithPath: path)) } catch {
            throw ProjectError("cannot read " + path + ": " + error.localizedDescription)
        }
        if String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines) == "null" { return ProjectState() }
        do {
            guard case .state(let s) = try JSONDecoder().decode(ProjectStateFile.self, from: data) else { return ProjectState() }
            return s
        } catch { throw ProjectError("cannot read " + path + ": " + String(describing: error)) }
    }

    /// The bytes of encoding/json's MarshalIndent(state, "", "  ") plus a newline, so both clients write the same file.
    public static func encode(_ st: ProjectState) -> Data {
        var s = "{\n  \"version\": 1,\n  \"links\": "
        if st.links.isEmpty { s += "null\n}\n" } else {
            s += "[\n" + st.links.map {
                "    {\n      \"gateway\": \(GoJSON.string($0.gateway)),\n      \"project\": \($0.project),\n"
                    + "      \"slug\": \(GoJSON.string($0.slug)),\n      \"path\": \(GoJSON.string($0.path))\n    }"
            }.joined(separator: ",\n") + "\n  ]\n}\n"
        }
        return Data(s.utf8)
    }

    public static func save(_ path: String, _ st: ProjectState) throws {
        let dir = (path as NSString).deletingLastPathComponent
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        var tmp = Array((dir + "/." + (path as NSString).lastPathComponent + "-XXXXXX").utf8CString)
        let fd = mkstemp(&tmp)  // 0600
        guard fd >= 0 else { throw ProjectError(String(cString: strerror(errno)).lowercasedFirst) }
        let name = String(cString: tmp)
        var ok = false
        defer { if !ok { unlink(name) } }
        let h = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
        do { try h.write(contentsOf: encode(st)); try h.synchronize(); try h.close() } catch { throw ProjectError(error.localizedDescription) }
        guard rename(name, path) == 0 else { throw ProjectError(String(cString: strerror(errno)).lowercasedFirst) }
        ok = true
    }

    // MARK: folders

    /// The gateway's identity: its public key, so a renamed or second profile of the same gateway shares its links. A local plain
    /// profile without a key is told apart by its address.
    public static func gatewayID(url: String, key: String?) -> String { key ?? "url:" + url }

    /// The canonical real path of a folder (symlinks resolved).
    public static func realFolder(_ dir: String) throws -> String {
        let abs = dir.hasPrefix("/") ? dir : FileManager.default.currentDirectoryPath + "/" + dir
        guard let p = realpath(abs, nil) else {
            throw ProjectError("cannot resolve " + dir + ": " + String(cString: strerror(errno)).lowercasedFirst)
        }
        defer { free(p) }
        return String(cString: p)
    }

    static func fileID(_ p: String) -> (dev: UInt64, ino: UInt64)? {
        var st = stat()
        return stat(p, &st) == 0 ? (UInt64(bitPattern: Int64(st.st_dev)), UInt64(st.st_ino)) : nil
    }

    /// The same name, or the same folder by identity (case-insensitive volumes): by name first, then by (device, inode).
    static func samePath(_ a: String, _ b: String) -> Bool {
        if a == b { return true }
        guard let x = fileID(a), let y = fileID(b) else { return false }
        return x == y
    }

    static func parent(_ dir: String) -> String { (dir as NSString).deletingLastPathComponent.isEmpty ? "/" : (dir as NSString).deletingLastPathComponent }

    /// The slug in a .minigw-project file (one line); "" if there is none or it is not a plain file with a valid slug. It only ever
    /// feeds a suggestion or a check; it never creates a link.
    public static func readHint(_ dir: String) -> String {
        let p = dir + "/" + hintFile
        var st = stat()
        guard lstat(p, &st) == 0, (st.st_mode & S_IFMT) == S_IFREG, st.st_size <= 256, let b = FileManager.default.contents(atPath: p) else { return "" }
        let text = String(decoding: b, as: UTF8.self).replacingOccurrences(of: "\r", with: "")
        let line = (text.split(separator: "\n", maxSplits: 1, omittingEmptySubsequences: false).first.map(String.init) ?? "")
            .trimmingCharacters(in: .whitespacesAndNewlines)
        return isSlug(line) ? line : ""
    }

    public enum Applies: Sendable { case none, ok, hintDiff }  // no link / the link applies / link and a hint with another slug: refused

    public struct Resolution: Sendable {
        public var state = Applies.none
        public var link = ProjectLink(gateway: "", project: 0, slug: "", path: "")
        public var hint = ""
        public var hintDir = ""
        public var realPath = ""
    }

    /// The project of a folder (contracts section 3): resolve symlinks, take the stored link of this gateway whose path is the folder
    /// or its nearest ancestor. The hint is read only from the linked root; without a link from the folder and its ancestors up to the
    /// repository (.git) or the filesystem root.
    public static func resolve(_ st: ProjectState, gateway: String, cwd: String) throws -> Resolution {
        let real = try realFolder(cwd)
        var res = Resolution()
        res.realPath = real
        let links = st.links.filter { $0.gateway == gateway }
        var dir = real
        while true {
            if let l = links.first(where: { samePath(dir, $0.path) }) {
                res.link = l; res.state = .ok
                let h = readHint(dir)
                if !h.isEmpty {
                    res.hint = h; res.hintDir = dir
                    if h != l.slug { res.state = .hintDiff }
                }
                return res
            }
            let up = parent(dir)
            if up == dir { break }
            dir = up
        }
        dir = real
        while true {
            let h = readHint(dir)
            if !h.isEmpty { res.hint = h; res.hintDir = dir; break }
            var s = stat()
            if lstat(dir + "/.git", &s) == 0 || parent(dir) == dir { break }  // the repository root (or the filesystem root) is the last folder looked at
            dir = parent(dir)
        }
        return res
    }

    public static func hintDiffMessage(_ r: Resolution) -> String {
        "this folder is linked to \(r.link.slug) but \(hintFile) says \(r.hint): run minigw project unlink, link again or remove the hint"
    }

    /// The header line for the agent requests of a run in cwd, or "". A link sends the project id; link + hint with another slug is an
    /// error (the request is not sent); no link continues without a project and warns with the hint's suggestion. Not used inside an
    /// agent run: the gateway inherits the project.
    public static func headerFor(cwd: String, gateway: String, statePath: String, agent: Bool,
                                 warn: (String) -> Void = { FileHandle.standardError.write(Data($0.utf8)) }) throws -> String {
        if agent { return "" }
        let st = try load(statePath)
        guard let res = try? resolve(st, gateway: gateway, cwd: cwd) else { return "" }  // a folder that cannot be resolved has no link
        switch res.state {
        case .ok: return "x-minigw-project: \(res.link.project)"
        case .hintDiff: throw ProjectError(hintDiffMessage(res))
        case .none: break
        }
        if !res.hint.isEmpty {
            warn("minigw: this folder suggests project \(res.hint) (\(hintFile)); run minigw project link \(res.hint). Continuing without a project.\n")
        }
        return ""
    }

    // MARK: the local proxy's header rule (client/proxy.go rewriteHead)

    /// The request line and headers (without the final blank line) with every X-Minigw-Project header removed, whatever its case,
    /// and, when the run has a project, the one minigw resolved. The agent CLI's own settings (ANTHROPIC_CUSTOM_HEADERS from a
    /// repository) can therefore never choose the project of a request.
    /// 0.6.0 sec-fix F1, fail closed: nil (close the connection) when the head has a line the gateway's parsers could read differently
    /// from this one: a line starting with SP/HT (obs-fold), a bare CR or LF inside a line, a line without a colon or a header name that is not an
    /// RFC 7230 token (`X-Minigw-Project : 7`, a no-break space). The name is compared exactly, as the receiving parser does.
    public static func rewriteHead(_ head: Data, project: String) -> Data? {
        let crlf = Data("\r\n".utf8)
        var lines: [Data] = []
        var rest = head[...]
        while let r = rest.range(of: crlf) { lines.append(Data(rest[..<r.lowerBound])); rest = rest[r.upperBound...] }
        lines.append(Data(rest))
        let name = Array("x-minigw-project".utf8)
        func lowered(_ d: Data) -> [UInt8] { d.map { $0 >= 65 && $0 <= 90 ? $0 + 32 : $0 } }
        var out: [Data] = []
        for (i, l) in lines.enumerated() {
            if l.contains(0x0a) || l.contains(0x0d) { return nil }
            if i > 0 {
                guard l.first != 0x20, l.first != 0x09, let colon = l.firstIndex(of: UInt8(ascii: ":")),
                      colon > l.startIndex, l[..<colon].allSatisfy(isTokenByte) else { return nil }
                if lowered(l)[..<(colon - l.startIndex)].elementsEqual(name) { continue }
            }
            out.append(l)
            if i == 0 && !project.isEmpty { out.append(Data(("X-Minigw-Project: " + project).utf8)) }
        }
        return Data(out.enumerated().reduce(into: [UInt8]()) { $0 += ($1.offset > 0 ? Array(crlf) : []) + Array($1.element) })
    }

    /// RFC 7230 tchar.
    static func isTokenByte(_ b: UInt8) -> Bool {
        (0x30...0x39).contains(b) || (0x41...0x5a).contains(b) || (0x61...0x7a).contains(b) || "!#$%&'*+-.^_`|~".utf8.contains(b)
    }

    /// The first request's header block of one local connection, until it is complete (the gateway serves one request per connection),
    /// rewritten with rewriteHead; everything after it passes unchanged. F2: not complete after 64 KiB, or a head rewriteHead refuses:
    /// nil, the caller closes the connection (nothing is passed on unrewritten).
    public final class HeadRewriter: @unchecked Sendable {
        private var head = Data()
        private var done = false
        private let project: String
        public init(project: String) { self.project = project }

        /// The bytes to send on (empty while the header block is incomplete); nil = close the connection.
        public func feed(_ chunk: Data) -> Data? {
            if done { return chunk }
            head.append(chunk)
            // R4: the limit is on the offset of the terminator, not on what has arrived: a head of at most 64 KiB (before CRLFCRLF) is
            // accepted however it is chunked; the terminator of one that is longer is refused even if it comes in the chunk that crosses 64 KiB
            let limit = 64 << 10
            guard let end = head.range(of: Data("\r\n\r\n".utf8)) else { return head.count >= limit + 4 ? nil : Data() }
            guard end.lowerBound - head.startIndex <= limit else { return nil }
            done = true
            defer { head = Data() }
            guard let rewritten = Projects.rewriteHead(head[..<end.lowerBound], project: project) else { return nil }
            return rewritten + head[end.lowerBound...]
        }
    }
}

extension String {
    /// Go's error texts start in lower case ("no such file or directory"); strerror's do not.
    var lowercasedFirst: String { prefix(1).lowercased() + dropFirst() }
}

/// encoding/json's string and number rules for the few documents the clients write (projects.json, the note requests).
enum GoJSON {
    static func string(_ s: String) -> String {
        var o = "\""
        for u in s.unicodeScalars {
            switch u.value {
            case 0x22: o += "\\\""
            case 0x5c: o += "\\\\"
            case 0x0a: o += "\\n"
            case 0x0d: o += "\\r"
            case 0x09: o += "\\t"
            case 0x08: o += "\\b"
            case 0x0c: o += "\\f"
            case 0..<0x20, 0x3c, 0x3e, 0x26, 0x2028, 0x2029:
                o += "\\u" + String(format: "%04x", u.value)
            default: o.unicodeScalars.append(u)
            }
        }
        return o + "\""
    }

    /// json.Marshal of a map: keys sorted, values String, Bool or Int64.
    static func object(_ m: [String: Any]) -> String {
        "{" + m.keys.sorted().map { k in
            string(k) + ":" + ((m[k] as? String).map(string) ?? (m[k] as? Bool).map { $0 ? "true" : "false" } ?? String(m[k] as? Int64 ?? 0))
        }.joined(separator: ",") + "}"
    }
}
