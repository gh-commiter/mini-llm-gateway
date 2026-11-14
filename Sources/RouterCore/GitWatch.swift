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
        public init() {}
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
        var staged: [String] = []
        for (p, v) in end.index where start.index[p] != v && notCommitted(p) { staged.append(p) }
        for p in start.index.keys where end.index[p] == nil && notCommitted(p) { staged.append(p) }
        var parts: [String] = []
        if end.head != start.head { parts.append(end.head.isEmpty ? "HEAD is detached" : "HEAD now on " + end.head) }
        if !commits.isEmpty { parts.append("\(commits.count) new commit(s)") }
        for (what, l) in [("new branches", newBranches), ("moved branches", moved), ("new tags", newTags), ("deleted refs", deleted)] where !l.isEmpty { parts.append(what + ": " + list(l)) }
        if !staged.isEmpty { parts.append("\(staged.count) staged path(s)") }
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
        for e in git(root, ["ls-files", "-s", "-z"]).out.split(separator: "\0") {
            let parts = e.split(separator: "\t", maxSplits: 1).map(String.init)
            if parts.count == 2 { let f = parts[0].split(separator: " ").map(String.init); if f.count == 3 { s.index[parts[1]] = f.joined(separator: " ") } }
        }
        s.cached = git(root, ["diff", "--cached", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv", "-z"]).out.split(separator: "\0").map(String.init)
        for n in ["MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"] where lstat(root + "/.git/" + n, &st) == 0 { s.pending.append(n) }
        return s
    }

    public static func begin(_ root: String) -> GitWatch? { state(root).map { GitWatch(root: root, start: $0) } }

    /// At the end of the share: prints to stderr; puts the start state of the staged protected paths back into the index (plumbing: the work tree is not touched).
    @discardableResult public func finish() -> [String] {
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
        for l in lines { try? FileHandle.standardError.write(contentsOf: Data((l + "\n").utf8)) }
        return lines
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
