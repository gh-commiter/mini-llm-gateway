import CryptoKit
import Foundation

/// Direct mode lets the agent write git's own data (Sync.gitDirWritable), so the owner always sees what it did in the repository and is warned about what git could
/// carry into the work tree later (#116 B1): at the start of the share the client takes the state of the repository, at the end it compares (HEAD, refs, index,
/// merge state), prints one summary line, warns about protected paths in new commits or in the index (the staged ones it resets, never touching the work tree) and
/// about a merge, cherry-pick or revert the agent left in progress. The Go client does the same (gitwatch.go); client/testdata/git-watch.json is asserted by both.
public struct GitWatch: Sendable {
    public struct State: Codable, Equatable, Sendable {
        public var head = ""                  // branch HEAD is on ("" = detached or unborn)
        public var refs: [String: String] = [:]   // refname → object id
        public var index: [String: String] = [:]  // path → "mode id stage"
        public var pending: [String] = []     // MERGE_HEAD, CHERRY_PICK_HEAD, REVERT_HEAD that exist
        public var cached: [String]?          // end state only: paths where the index differs from HEAD (staged, not committed); nil = all
        /// sha256 of `git ls-files -s -z`. `partial` (a start state read back from the store): `index` holds only the protected paths, so the staged paths are told only
        /// for those, and a changed `indexHash` says that the index changed.
        public var indexHash = ""
        public var partial = false
        public init() {}
        enum CodingKeys: String, CodingKey { case head, refs, index, pending, cached, indexHash, partial }
        public init(from d: Decoder) throws {
            let c = try d.container(keyedBy: CodingKeys.self)
            head = try c.decodeIfPresent(String.self, forKey: .head) ?? ""
            refs = try c.decodeIfPresent([String: String].self, forKey: .refs) ?? [:]
            index = try c.decodeIfPresent([String: String].self, forKey: .index) ?? [:]
            pending = try c.decodeIfPresent([String].self, forKey: .pending) ?? []
            cached = try c.decodeIfPresent([String].self, forKey: .cached)
            indexHash = try c.decodeIfPresent(String.self, forKey: .indexHash) ?? ""
            partial = try c.decodeIfPresent(Bool.self, forKey: .partial) ?? false
        }
    }
    public struct Commit: Codable, Equatable, Sendable { public var id: String; public var files: [String] }

    static let names = 20  // names shown per list
    static func list(_ l: [String]) -> String {
        let s = l.sorted()
        return s.count > names ? s.prefix(names).joined(separator: ", ") + " and \(s.count - names) more" : s.joined(separator: ", ")
    }

    /// The lines for the owner and the staged protected paths to reset. Pure: the fixture asserts it.
    public static func report(start: State, end: State, commits: [Commit], isProtected: (String) -> Bool) -> (lines: [String], reset: [String]) {
        func short(_ r: String) -> String { r.hasPrefix("refs/heads/") ? String(r.dropFirst(11)) : r.hasPrefix("refs/tags/") ? String(r.dropFirst(10)) : r }
        var newBranches: [String] = [], moved: [String] = [], newTags: [String] = [], deleted: [String] = []
        for (r, id) in end.refs {
            let old = start.refs[r]
            if r.hasPrefix("refs/heads/") { if old == nil { newBranches.append(short(r)) } else if old != id { moved.append(short(r)) } }
            else if r.hasPrefix("refs/tags/"), old == nil { newTags.append(short(r)) }
        }
        for r in start.refs.keys where end.refs[r] == nil { deleted.append(short(r)) }
        func notCommitted(_ p: String) -> Bool { end.cached == nil || end.cached!.contains(p) }  // a path the agent committed is in the index too
        func counted(_ p: String) -> Bool { !start.partial || isProtected(p) }  // a partial start state knows only the protected paths
        var staged: [String] = []
        for (p, v) in end.index where counted(p) && start.index[p] != v && notCommitted(p) { staged.append(p) }
        for p in start.index.keys where end.index[p] == nil && counted(p) && notCommitted(p) { staged.append(p) }
        var parts: [String] = []
        if end.head != start.head { parts.append(end.head.isEmpty ? "HEAD is detached" : "HEAD now on " + end.head) }
        if !commits.isEmpty { parts.append("\(commits.count) new commit(s)") }
        for (what, l) in [("new branches", newBranches), ("moved branches", moved), ("new tags", newTags), ("deleted refs", deleted)] where !l.isEmpty { parts.append(what + ": " + list(l)) }
        if !staged.isEmpty && !start.partial { parts.append("\(staged.count) staged path(s)") } else if start.partial && start.indexHash != end.indexHash { parts.append("the index changed") }
        var lines: [String] = []
        if !parts.isEmpty { lines.append("minigw: git in this folder, changed by the agent: " + parts.joined(separator: "; ")) }
        var committed: [String] = []
        for c in commits { for f in c.files where isProtected(f) && !committed.contains(f) { committed.append(f) } }
        if !committed.isEmpty { lines.append("minigw: WARNING: the agent committed a protected path: " + list(committed) + " - review before git checkout, reset --hard, restore, stash pop or your next commit") }
        let reset = staged.filter(isProtected).sorted()
        if !reset.isEmpty { lines.append("minigw: WARNING: the agent staged a protected path: " + list(reset) + " - removed from the index for you (your files were not touched)") }
        let pending = end.pending.filter { !start.pending.contains($0) }
        if !pending.isEmpty { lines.append("minigw: WARNING: a merge, cherry-pick or revert is in progress, started by the agent (" + pending.joined(separator: ", ") + "): finish it or cancel it (git merge --abort) after you have reviewed it") }
        return (lines, reset)
    }

    let root: String
    let start: State
    var file: String?  // the stored copy of the start state (store below)
    var dir = GitWatch.stateDir()  // where it is stored (a test uses its own folder)

    static let pins = ["--no-optional-locks", "-c", "core.attributesFile=/dev/null", "-c", "core.quotePath=false", "-c", "diff.external="]  // Worktree.guardrails: no hooks, no fsmonitor

    static func git(_ root: String, _ args: [String], input: Data? = nil) -> (ok: Bool, out: String) {
        let r = Worktree.run(pins + args, cwd: root, env: ["GIT_OPTIONAL_LOCKS": "0"], safe: true, hooks: false, input: input, timeout: 20, trim: false)
        return (r.status == 0, String(r.out.prefix(1 << 20)))
    }

    static func state(_ root: String) -> State? {
        var st = stat()
        guard lstat(root + "/.git", &st) == 0, st.st_mode & S_IFMT == S_IFDIR else { return nil }  // a gitdir: file is not watched
        let refs = git(root, ["for-each-ref", "--format=%(refname) %(objectname)"])
        guard refs.ok else { return nil }
        var s = State()
        for l in refs.out.split(separator: "\n") { let p = l.split(separator: " ", maxSplits: 1).map(String.init); if p.count == 2 { s.refs[p[0]] = p[1] } }
        let h = git(root, ["symbolic-ref", "-q", "--short", "HEAD"])
        if h.ok { s.head = h.out.trimmingCharacters(in: .whitespacesAndNewlines) }
        let idx = git(root, ["ls-files", "-s", "-z"]).out
        s.indexHash = SHA256.hash(data: Data(idx.utf8)).map { String(format: "%02x", $0) }.joined()
        for e in idx.split(separator: "\0") {
            let parts = e.split(separator: "\t", maxSplits: 1).map(String.init)
            if parts.count == 2 { let f = parts[0].split(separator: " ").map(String.init); if f.count == 3 { s.index[parts[1]] = f.joined(separator: " ") } }
        }
        s.cached = git(root, ["diff", "--cached", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv", "-z"]).out.split(separator: "\0").map(String.init)
        for n in ["MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"] where lstat(root + "/.git/" + n, &st) == 0 { s.pending.append(n) }
        return s
    }

    public static func begin(_ root: String, stateDir: String = GitWatch.stateDir()) -> GitWatch? {
        for l in recover(root, dir: stateDir, selfPID: getpid(), alive: Self.pidAlive) { try? FileHandle.standardError.write(contentsOf: Data((l + "\n").utf8)) }  // a run that did not finish (X2)
        guard let st = state(root) else { return nil }
        var w = GitWatch(root: root, start: st)
        w.dir = stateDir
        w.file = w.save()
        return w
    }

    /// The comparison of the start state with the repository now; the staged protected paths are taken out of the index (plumbing: the work tree is not touched).
    func lines() -> [String] {
        guard let end = Self.state(root) else { return [] }
        let tips = Array(end.refs.values), old = Array(start.refs.values)
        var commits: [Commit] = []
        if !tips.isEmpty {
            let out = Self.git(root, ["log", "--no-renames", "--name-only", "--format=@@%H", "-n", "200"] + tips + (old.isEmpty ? [] : ["--not"] + old)).out
            for l in out.split(separator: "\n", omittingEmptySubsequences: true) {
                if l.hasPrefix("@@") { commits.append(Commit(id: String(l.dropFirst(2)), files: [])) } else if !commits.isEmpty { commits[commits.count - 1].files.append(String(l)) }
            }
        }
        var (lines, reset) = Self.report(start: start, end: end, commits: commits, isProtected: { Sync.isProtected($0) })
        if !reset.isEmpty, !resetIndex(reset) { lines.append("minigw: could not remove the staged protected paths from the index: run git reset -- " + reset.joined(separator: " ")) }
        return lines
    }

    /// At the end of the share: prints to stderr, then forgets the stored start state.
    @discardableResult public func finish() -> [String] {
        let lines = self.lines()
        for l in lines { try? FileHandle.standardError.write(contentsOf: Data((l + "\n").utf8)) }
        if let file { try? FileManager.default.removeItem(atPath: file) }
        return lines
    }

    // MARK: X2 the stored start state (#116)
    // One file per folder and process in the client's state folder (~/Library/Application Support/minigw/gitwatch, 0700; the files 0600), never inside the repository: the ids
    // of HEAD and refs, a hash of the index and the index entries of the protected paths, no file contents. A stored file is untrusted on read: at most 4 MB, strict parse,
    // plausible names and ids, else it is ignored with one line. The Go client keeps the same (gitwatch_store.go).

    struct Stored: Codable, Equatable {
        var version = 1, root = "", pid = 0, head = "", indexHash = ""
        var refs: [String: String] = [:], protected: [String: String] = [:]
        var pending: [String] = []
    }
    static let maxFile = 4 << 20, maxRefs = 20000, maxProtected = 1000

    public static func stateDir() -> String { FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0].appendingPathComponent("minigw/gitwatch").path }
    static func key(_ root: String) -> String {
        let r = Sync.real(URL(fileURLWithPath: root))?.path ?? root
        return SHA256.hash(data: Data(r.utf8)).prefix(16).map { String(format: "%02x", $0) }.joined()
    }
    static func file(_ root: String, dir: String, pid: pid_t) -> String { dir + "/\(key(root))-\(pid).json" }
    static func pidAlive(_ pid: pid_t) -> Bool { kill(pid, 0) == 0 || errno == EPERM }

    func save() -> String? {
        guard start.refs.count <= Self.maxRefs else { return nil }
        var s = Stored(); s.root = root; s.pid = Int(getpid()); s.head = start.head; s.refs = start.refs; s.pending = start.pending; s.indexHash = start.indexHash
        for (p, v) in start.index where Sync.isProtected(p) && s.protected.count < Self.maxProtected { s.protected[p] = v }
        guard let data = try? JSONEncoder().encode(s), data.count <= Self.maxFile else { return nil }
        let f = Self.file(root, dir: dir, pid: getpid()), fm = FileManager.default
        guard (try? fm.createDirectory(atPath: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])) != nil,
              fm.createFile(atPath: f + ".tmp", contents: data, attributes: [.posixPermissions: 0o600]), rename(f + ".tmp", f) == 0 else { return nil }
        return f
    }

    /// Strict. nil = the file is ignored (the reason is in the second part).
    static func parse(_ data: Data, root: String) -> (State?, String) {
        if data.count > maxFile { return (nil, "too large") }
        guard let o = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              Set(o.keys).isSubset(of: ["version", "root", "pid", "head", "refs", "pending", "indexHash", "protected"]),
              let s = try? JSONDecoder().decode(Stored.self, from: data) else { return (nil, "not valid") }
        guard s.version == 1, s.root == root, s.refs.count <= maxRefs, s.protected.count <= maxProtected, s.pending.count <= 3, s.indexHash.count == 64,
              s.indexHash.allSatisfy({ $0.isHexDigit }) else { return (nil, "unexpected content") }
        func okName(_ n: String) -> Bool { !n.isEmpty && n.utf8.count <= 1024 && !n.contains(where: { $0 == "\0" || $0 == "\n" || $0 == "\r" }) && !n.hasPrefix("-") }
        func okID(_ i: String) -> Bool { (i.count == 40 || i.count == 64) && i.allSatisfy { $0.isHexDigit && !$0.isUppercase } }
        guard s.head.utf8.count <= 1024, !s.head.contains(where: { $0 == "\0" || $0 == "\n" || $0 == "\r" }) else { return (nil, "bad head") }
        for (r, id) in s.refs where !(r.hasPrefix("refs/") && okName(r) && okID(id)) { return (nil, "bad ref") }
        for (p, v) in s.protected {
            let f = v.split(separator: " ", omittingEmptySubsequences: false).map(String.init)
            guard okName(p), f.count == 3, f[0].count == 6, f[0].allSatisfy({ ("0"..."7").contains($0) }), okID(f[1]), f[2].count == 1, ("0"..."3").contains(f[2].first!) else { return (nil, "bad index entry") }
        }
        guard s.pending.allSatisfy({ ["MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"].contains($0) }) else { return (nil, "bad state name") }
        var st = State(); st.head = s.head; st.refs = s.refs; st.index = s.protected; st.pending = s.pending; st.indexHash = s.indexHash; st.partial = true
        return (st, "")
    }

    /// Files of runs that did not finish (their process is gone) in this folder: compare and report, prefixed, then remove them.
    static func recover(_ root: String, dir: String, selfPID: pid_t, alive: (pid_t) -> Bool) -> [String] {
        let fm = FileManager.default, prefix = key(root) + "-"
        var out: [String] = []
        for name in ((try? fm.contentsOfDirectory(atPath: dir)) ?? []).sorted() where name.hasPrefix(prefix) && name.hasSuffix(".json") {
            guard let pid = pid_t(name.dropFirst(prefix.count).dropLast(5)), pid != selfPID, !alive(pid) else { continue }
            let f = dir + "/" + name
            var data = Data()
            if let h = FileHandle(forReadingAtPath: f) { data = h.readData(ofLength: maxFile + 1); try? h.close() }
            try? fm.removeItem(atPath: f)
            let (st, why) = parse(data, root: root)
            guard let st else { out.append("minigw: ignoring a damaged git watch state file (\(why))"); continue }
            for l in GitWatch(root: root, start: st).lines() { out.append(l.replacingOccurrences(of: "minigw: ", with: "minigw: from the previous run that did not finish: ", options: [], range: l.range(of: "minigw: "))) }
        }
        return out
    }

    /// git update-index --index-info: mode, id, stage, path; id 0 removes the entry.
    func resetIndex(_ paths: [String]) -> Bool {
        var input = ""
        for p in paths {
            if p.contains("\n") || p.contains("\0") { return false }
            if let v = start.index[p] { let f = v.split(separator: " "); input += "\(f[0]) \(f[1]) \(f[2])\t\(p)\n" } else { input += "0 \(String(repeating: "0", count: 40))\t\(p)\n" }
        }
        return Self.git(root, ["update-index", "--index-info"], input: Data(input.utf8)).ok
    }
}
