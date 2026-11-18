import CryptoKit
import Foundation

/// Direct mode lets the agent write git's own data (Sync.gitDirWritable), so the owner always sees what it did in the repository and is warned about what git could
/// carry into the work tree later (#116 B1): at the start of the share the client takes the state of the repository, at the end it compares (HEAD, refs, index,
/// merge state), prints one summary line, warns about protected paths in new commits or in the index (the staged ones it resets, never touching the work tree) and
/// about a merge, cherry-pick or revert the agent left in progress. The Go client does the same (gitwatch.go); client/testdata/git-watch.json is asserted by both.
public struct GitWatch: Sendable {
    public struct State: Codable, Equatable, Sendable {
        public var head = ""                  // branch HEAD is on ("" = detached or unborn)
        public var headId = ""                // object id HEAD points to, also when it is detached ("" = unborn)
        public var refs: [String: String] = [:]   // refname → object id
        public var index: [String: String] = [:]  // path → "mode id stage"
        public var pending: [String] = []     // MERGE_HEAD, CHERRY_PICK_HEAD, REVERT_HEAD that exist
        public var cached: [String]?          // end state only: paths where the index differs from HEAD (staged, not committed); nil = all
        /// sha256 of `git ls-files -s -z`. `partial` (a start state read back from the store): `index` holds only the protected paths, so the staged paths are told only
        /// for those, and a changed `indexHash` says that the index changed.
        public var indexHash = ""
        public var partial = false
        /// end state only: what the check could not read in full (output over the limit, a git command that failed). Never a clean result: the report says so.
        public var incomplete: [String] = []
        public init() {}
        enum CodingKeys: String, CodingKey { case head, headId, refs, index, pending, cached, indexHash, partial, incomplete }
        public init(from d: Decoder) throws {
            let c = try d.container(keyedBy: CodingKeys.self)
            head = try c.decodeIfPresent(String.self, forKey: .head) ?? ""
            headId = try c.decodeIfPresent(String.self, forKey: .headId) ?? ""
            refs = try c.decodeIfPresent([String: String].self, forKey: .refs) ?? [:]
            index = try c.decodeIfPresent([String: String].self, forKey: .index) ?? [:]
            pending = try c.decodeIfPresent([String].self, forKey: .pending) ?? []
            cached = try c.decodeIfPresent([String].self, forKey: .cached)
            indexHash = try c.decodeIfPresent(String.self, forKey: .indexHash) ?? ""
            partial = try c.decodeIfPresent(Bool.self, forKey: .partial) ?? false
            incomplete = try c.decodeIfPresent([String].self, forKey: .incomplete) ?? []
        }
    }
    public struct Commit: Codable, Equatable, Sendable {
        public var id: String; public var files: [String]
        public var modes: [String]?  // "path old->new": the file mode or type changed in this commit (0.7.1)
        public init(id: String, files: [String], modes: [String]? = nil) { self.id = id; self.files = files; self.modes = modes }
    }

    static let names = 20  // names shown per list
    static let keepIncomplete: TimeInterval = 7 * 24 * 3600  // how long a baseline that could not be compared in full is kept
    static let maxCommitsDefault = 200, maxOutputDefault = 1 << 20
    static func list(_ l: [String]) -> String {
        let s = l.sorted()
        return s.count > names ? s.prefix(names).joined(separator: ", ") + " and \(s.count - names) more" : s.joined(separator: ", ")
    }

    /// The lines for the owner and the staged protected paths to reset. Pure: the fixture asserts it.
    public static func report(start: State, end: State, commits: [Commit], isProtected: (String) -> Bool) -> (lines: [String], reset: [String]) {
        func short(_ r: String) -> String { r.hasPrefix("refs/heads/") ? String(r.dropFirst(11)) : r.hasPrefix("refs/tags/") ? String(r.dropFirst(10)) : r }
        var newBranches: [String] = [], moved: [String] = [], newTags: [String] = [], movedTags: [String] = [], other: [String] = [], replaced: [String] = [], deleted: [String] = []
        for (r, id) in end.refs {
            let old = start.refs[r]
            if r.hasPrefix("refs/replace/"), old != id { replaced.append(String(r.dropFirst(13))) }
            if r.hasPrefix("refs/heads/") { if old == nil { newBranches.append(short(r)) } else if old != id { moved.append(short(r)) } }
            else if r.hasPrefix("refs/tags/") { if old == nil { newTags.append(short(r)) } else if old != id { movedTags.append(short(r)) } }  // a moved tag can hide a commit that is already in the history
            else if old != id { other.append(r) }  // every other namespace: stash, notes, replace, remotes, ...
        }
        for r in start.refs.keys where end.refs[r] == nil { deleted.append(short(r)) }
        func notCommitted(_ p: String) -> Bool { end.cached == nil || end.cached!.contains(p) }  // a path the agent committed is in the index too
        func counted(_ p: String) -> Bool { !start.partial || isProtected(p) }  // a partial start state knows only the protected paths
        var staged: [String] = []
        for (p, v) in end.index where counted(p) && start.index[p] != v && notCommitted(p) { staged.append(p) }
        for p in start.index.keys where end.index[p] == nil && counted(p) && notCommitted(p) { staged.append(p) }
        var parts: [String] = []
        if end.head != start.head { parts.append(end.head.isEmpty ? "HEAD is detached" : "HEAD now on " + end.head) }
        else if end.head.isEmpty, end.headId != start.headId { parts.append("detached HEAD moved") }  // no branch moves with it: only the object id says so
        if !commits.isEmpty { parts.append("\(commits.count) new commit(s)") }
        for (what, l) in [("new branches", newBranches), ("moved branches", moved), ("new tags", newTags), ("moved tags", movedTags), ("other refs changed", other), ("deleted refs", deleted)] where !l.isEmpty { parts.append(what + ": " + list(l)) }
        if !staged.isEmpty && !start.partial { parts.append("\(staged.count) staged path(s)") } else if start.partial && start.indexHash != end.indexHash { parts.append("the index changed") }
        var lines: [String] = []
        if !parts.isEmpty { lines.append("minigw: git in this folder, changed by the agent: " + parts.joined(separator: "; ")) }
        // 0.7.1: a mode or type change (a symbolic link that became a regular file, the executable bit) in a new commit or in the index is named, whatever else the run did
        var modeChanged: [String] = []
        for c in commits { for m in c.modes ?? [] where !modeChanged.contains(m) { modeChanged.append(m) } }
        if !start.partial {
            for (p, v) in end.index {
                guard let sv = start.index[p] else { continue }
                let sf = sv.split(separator: " ").map(String.init), ef = v.split(separator: " ").map(String.init)
                if sf.count == 3, ef.count == 3, sf[2] == ef[2], sf[0] != ef[0] {
                    let m = p + " " + sf[0] + "->" + ef[0]
                    if !modeChanged.contains(m) { modeChanged.append(m) }
                }
            }
        }
        if !modeChanged.isEmpty {
            lines.append("minigw: WARNING: the agent changed the mode or type of " + list(modeChanged) + " (120000 is a symbolic link, 100755 an executable file): review it before git checkout, reset --hard, restore, stash pop or your next commit")
        }
        var committed: [String] = []
        for c in commits { for f in c.files where isProtected(f) && !committed.contains(f) { committed.append(f) } }
        if !committed.isEmpty { lines.append("minigw: WARNING: the agent committed a protected path: " + list(committed) + " - review before git checkout, reset --hard, restore, stash pop or your next commit") }
        var reset = staged.filter(isProtected).sorted()
        if !reset.isEmpty, !end.incomplete.isEmpty {  // the staged paths come from a read that failed in part: the index is left alone
            lines.append("minigw: WARNING: a protected path looks staged: " + list(reset) + " - NOT removed from the index, because the check of git was incomplete; look at git status before your next commit")
            reset = []
        } else if !reset.isEmpty { lines.append("minigw: WARNING: the agent staged a protected path: " + list(reset) + " - removed from the index for you (your files were not touched)") }
        let dashed = end.refs.filter { r, id in start.refs[r] != id && r.split(separator: "/").contains { $0.hasPrefix("-") } }.keys.map { $0 }
        if !dashed.isEmpty {  // the guard refuses them as loose refs, packed-refs is writable: a script of yours could read such a name as an option
            lines.append("minigw: WARNING: the agent created a ref with a name that starts with \"-\" (" + list(dashed) + "): a script of yours could take it for an option - review it, then git update-ref -d <name>")
        }
        if !replaced.isEmpty { lines.append("minigw: WARNING: the agent created or changed a replacement ref (" + list(replaced) + "): git shows other content than the real commits while it exists - review it, then git replace -d <name>") }
        if !end.incomplete.isEmpty { lines.append("minigw: WARNING: the check of git in this folder is INCOMPLETE (" + end.incomplete.joined(separator: "; ") + "): the absence of other warnings is not a clean result - review the repository before git checkout, reset --hard, restore, stash pop or your next commit") }
        let pending = end.pending.filter { !start.pending.contains($0) }
        if !pending.isEmpty { lines.append("minigw: WARNING: a merge, cherry-pick or revert is in progress, started by the agent (" + pending.joined(separator: ", ") + "): finish it or cancel it (git merge --abort) after you have reviewed it") }
        return (lines, reset)
    }

    let root: String
    let start: State
    var maxCommits = GitWatch.maxCommitsDefault  // new commits whose paths are checked (a test lowers it)
    var maxOutput = GitWatch.maxOutputDefault    // bytes of one git command's output (a test lowers it)
    var file: String?  // the stored copy of the start state (store below)
    var dir = GitWatch.stateDir()  // where it is stored (a test uses its own folder)

    /// --no-replace-objects: the watch reads the real objects, never what a refs/replace entry says they are.
    /// Every git call of the watch also has Worktree.guardrails (core.commitGraph=false, core.multiPackIndex=false, no hooks, no fsmonitor, no external diff) and, with `safe`,
    /// GIT_CONFIG_NOSYSTEM and GIT_CONFIG_GLOBAL=/dev/null (Worktree.safeEnv). The agent can write objects/info/commit-graph* and the multi-pack-index (git gc needs the first),
    /// and a forged one would show a commit's files as another's. The Go watch has the same (gitWatchPins); client/testdata/git-watch.json (pins) is asserted by both.
    static let env = ["GIT_OPTIONAL_LOCKS": "0", "GIT_NO_REPLACE_OBJECTS": "1", "GIT_EXTERNAL_DIFF": ""]
    static let pins = ["--no-replace-objects", "--no-optional-locks", "-c", "core.attributesFile=/dev/null", "-c", "core.quotePath=false", "-c", "diff.external="]  // Worktree.guardrails: no hooks, no fsmonitor

    static func git(_ root: String, maxOutput: Int = GitWatch.maxOutputDefault, _ args: [String], input: Data? = nil) -> (ok: Bool, out: String) {
        let r = Worktree.run(pins + args, cwd: root, env: env, safe: true, hooks: false, input: input, timeout: 20, trim: false)
        // ok is false when git failed or the output was longer than the limit (the part is returned): a caller never takes it for the whole
        return (r.status == 0 && r.out.utf8.count <= maxOutput, String(decoding: Array(r.out.utf8.prefix(maxOutput)), as: UTF8.self))
    }

    static func state(_ root: String, maxOutput: Int = GitWatch.maxOutputDefault) -> State? {
        var st = stat()
        guard lstat(root + "/.git", &st) == 0, st.st_mode & S_IFMT == S_IFDIR else { return nil }  // a gitdir: file is not watched
        let refs = git(root, maxOutput: maxOutput, ["for-each-ref", "--format=%(refname) %(objectname)"])  // every namespace: heads, tags, stash, notes, replace, packed refs too
        guard refs.ok || !refs.out.isEmpty else { return nil }
        var s = State()
        if !refs.ok { s.incomplete.append("refs not read in full") }
        for l in refs.out.split(separator: "\n") { let p = l.split(separator: " ", maxSplits: 1).map(String.init); if p.count == 2 { s.refs[p[0]] = p[1] } }
        let h = git(root, maxOutput: maxOutput, ["symbolic-ref", "-q", "--short", "HEAD"])
        if h.ok { s.head = h.out.trimmingCharacters(in: .whitespacesAndNewlines) }
        let hid = git(root, maxOutput: maxOutput, ["rev-parse", "-q", "--verify", "HEAD"])  // also when HEAD is detached: no ref names that commit
        if hid.ok { s.headId = hid.out.trimmingCharacters(in: .whitespacesAndNewlines) }
        let ls = git(root, maxOutput: maxOutput, ["ls-files", "-s", "-z"])
        if !ls.ok { s.incomplete.append("index not read in full") }
        let idx = ls.out
        s.indexHash = SHA256.hash(data: Data(idx.utf8)).map { String(format: "%02x", $0) }.joined()
        for e in idx.split(separator: "\0") {
            let parts = e.split(separator: "\t", maxSplits: 1).map(String.init)
            if parts.count == 2 { let f = parts[0].split(separator: " ").map(String.init); if f.count == 3 { s.index[parts[1]] = f.joined(separator: " ") } }
        }
        let cached = git(root, maxOutput: maxOutput, ["diff", "--cached", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv", "-z"])
        if !cached.ok { s.incomplete.append("staged paths not read in full") }
        s.cached = cached.out.split(separator: "\0").map(String.init)
        for n in ["MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"] where lstat(root + "/.git/" + n, &st) == 0 { s.pending.append(n) }
        return s
    }

    public static func begin(_ root: String, stateDir: String = GitWatch.stateDir()) -> GitWatch? {
        recoverAtStart(root, stateDir: stateDir)  // a run that did not finish (X2)
        guard let st = state(root) else { return nil }
        var w = GitWatch(root: root, start: st)
        w.dir = stateDir
        w.file = w.save()
        if w.file == nil { try? FileHandle.standardError.write(contentsOf: Data("minigw: git watch: the start state of this repository could not be stored (too many refs or no writable settings folder): if this client is killed, the report of what the agent did is lost\n".utf8)) }
        return w
    }

    /// The runs of this folder that did not finish, reported at the start of every share (not only a direct-mode one that watches the repository itself).
    public static func recoverAtStart(_ root: String, stateDir: String = GitWatch.stateDir()) {
        for l in recover(root, dir: stateDir, selfPID: getpid(), alive: Self.pidAlive) { try? FileHandle.standardError.write(contentsOf: Data((l + "\n").utf8)) }
    }

    /// The comparison of the start state with the repository now; the staged protected paths are taken out of the index (plumbing: the work tree is not touched).
    func lines() -> [String] { compare().lines }

    /// The lines and whether the check was complete. The history is read from every ref and from HEAD itself (a detached HEAD is on no ref), minus what the start state
    /// already had; merge commits are diffed against each parent (-m), so the paths a merge brings in show up.
    /// restore false (the share did not drain: a request may still be writing): nothing is taken out of the index, the staged protected paths are only reported.
    func compare(incomplete: [String] = [], restore: Bool = true) -> (lines: [String], complete: Bool) {
        guard var end = Self.state(root, maxOutput: maxOutput) else {  // the start was watchable, so this is not "nothing to watch": damaged refs, a removed .git
            return (["minigw: WARNING: the check of git in this folder is INCOMPLETE (the repository could not be read at the end" + incomplete.map { "; " + $0 }.joined() + "): review the repository before git checkout, reset --hard, restore, stash pop or your next commit"], false)
        }
        end.incomplete += incomplete
        let tips = Self.tips(end), old = Self.tips(start)
        var commits: [Commit] = []
        if !tips.isEmpty {
            // Framing that a file name cannot forge: the commit ids come alone (rev-list), then each commit's names alone (diff-tree, NUL-separated, raw, no header at all).
            let r = Self.git(root, maxOutput: maxOutput, ["rev-list", "-n", String(maxCommits + 1)] + tips + (old.isEmpty ? [] : ["--not"] + old))
            if !r.ok { end.incomplete.append("history not read in full") }
            var ids = r.out.split(whereSeparator: \.isWhitespace).map(String.init)
            if ids.count > maxCommits {
                ids = Array(ids.prefix(maxCommits))
                end.incomplete.append("more than \(maxCommits) new commits: only the first \(maxCommits) were checked")
            }
            for id in ids {
                // -m: a merge is diffed against each parent; --root: the first commit has files too
                let n = Self.git(root, maxOutput: maxOutput, ["diff-tree", "-m", "-r", "-z", "--raw", "--no-renames", "--root", "--no-commit-id", "--no-ext-diff", id])
                if !n.ok { end.incomplete.append("files of a commit not read in full") }
                let (files, modes) = Self.parseRaw(n.out)
                commits.append(Commit(id: id, files: files, modes: modes))
            }
        }
        var (lines, reset) = Self.report(start: start, end: end, commits: commits, isProtected: { Sync.isProtected($0) })
        if !reset.isEmpty, !restore {
            lines.append("minigw: the staged protected paths were NOT removed from the index (a request of the agent was still running): run git reset -- " + reset.joined(separator: " "))
        } else if !reset.isEmpty, !resetIndex(reset) { lines.append("minigw: could not remove the staged protected paths from the index: run git reset -- " + reset.joined(separator: " ")) }
        return (lines, end.incomplete.isEmpty)
    }

    /// The output of `git diff-tree -z --name-only --no-commit-id`: the names, NUL-separated and raw (a quote, a newline or a control character in a name is not C-quoted), each
    /// once (-m lists a merge's names once per parent).
    static func parseNames(_ out: String) -> [String] {
        var names: [String] = []
        for part in out.split(separator: "\0", omittingEmptySubsequences: true) where !names.contains(String(part)) { names.append(String(part)) }
        return names
    }

    /// The output of `git diff-tree -z --raw --no-commit-id`: ":oldmode newmode oldid newid STATUS" NUL path NUL per entry. The names as parseNames gives them, and the entries whose
    /// mode or type changed (both modes present and different: a typechange T, or the executable bit) as "path old->new".
    static func parseRaw(_ out: String) -> (files: [String], modes: [String]) {
        var files: [String] = [], modes: [String] = []
        let toks = out.split(separator: "\0", omittingEmptySubsequences: false).map(String.init)
        var i = 0
        while i + 1 < toks.count {
            guard toks[i].hasPrefix(":") else { i += 1; continue }
            let f = toks[i].dropFirst().split(separator: " ").map(String.init), path = toks[i + 1]
            i += 2
            if !path.isEmpty, !files.contains(path) { files.append(path) }
            let special = { (m: String) in m == "120000" || m == "100755" }  // a link or an executable file, also added or deleted (a rename with a type change is a delete and an add)
            if f.count >= 5, f[0] != f[1], (f[0] != "000000" && f[1] != "000000") || special(f[0]) || special(f[1]) {
                let m = path + " " + f[0] + "->" + f[1]
                if !modes.contains(m) { modes.append(m) }
            }
        }
        return (files, modes)
    }

    /// The object ids to start the history from: every ref and HEAD.
    static func tips(_ st: State) -> [String] { Array(Set(st.refs.values).union(st.headId.isEmpty ? [] : [st.headId])).sorted() }

    /// At the end of the share: prints to stderr, then forgets the stored start state, unless the check was incomplete: then the file stays and the next start in the folder checks
    /// again (`recover`). `drained` false: the requests of the share did not finish in time, the agent may still be writing.
    @discardableResult public func finish(drained: Bool = true) -> [String] {
        let r = compare(incomplete: drained ? [] : ["requests of the agent were still running"], restore: drained)
        for l in r.lines { try? FileHandle.standardError.write(contentsOf: Data((l + "\n").utf8)) }
        if r.complete, let file { try? FileManager.default.removeItem(atPath: file) }
        return r.lines
    }

    /// The end report under the folder's exclusive lock: waits (up to `wait`) for the requests that hold it, so that nothing can write while the repository is read and the index
    /// restored. nil lock or no lock in time: the check runs anyway and says that requests were still running.
    @discardableResult public func finish(under lock: FolderLock?, wait: TimeInterval) -> [String] {
        lock?.with(exclusive: true, wait: wait, { finish() }) ?? finish(drained: false)
    }

    // MARK: X2 the stored start state (#116)
    // One file per folder and process in the client's state folder (~/Library/Application Support/minigw/gitwatch, 0700; the files 0600), never inside the repository: the ids
    // of HEAD and refs, a hash of the index and the index entries of the protected paths, no file contents. A stored file is untrusted on read: at most 4 MB, strict parse,
    // plausible names and ids, else it is ignored with one line. The Go client keeps the same (gitwatch_store.go).

    struct Stored: Codable, Equatable {
        var version = 1, root = "", pid = 0, head = "", indexHash = ""
        var started: String?  // procStart of the process that wrote the file; absent in a file of an older client
        var headId: String?  // absent in a file of an older client
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
    /// When the process started ("sec.usec", sysctl; "" = not known): a reused process id is another process.
    static func procStart(_ pid: pid_t) -> String {
        var info = kinfo_proc(), size = MemoryLayout<kinfo_proc>.stride
        var mib: [Int32] = [CTL_KERN, KERN_PROC, KERN_PROC_PID, pid]
        guard sysctl(&mib, 4, &info, &size, nil, 0) == 0, size > 0 else { return "" }
        let t = info.kp_proc.p_un.__p_starttime
        return "\(t.tv_sec).\(t.tv_usec)"
    }
    /// Whether the process that wrote the state file still runs: the id exists and, when the start time was stored and can be read, it is the same process.
    static func pidAlive(_ pid: pid_t, _ started: String) -> Bool {
        guard kill(pid, 0) == 0 || errno == EPERM else { return false }
        let now = procStart(pid)
        return started.isEmpty || now.isEmpty || now == started
    }

    func save() -> String? {
        guard start.refs.count <= Self.maxRefs else { return nil }
        var s = Stored(); s.root = root; s.pid = Int(getpid()); s.started = Self.procStart(getpid()); s.head = start.head; s.headId = start.headId.isEmpty ? nil : start.headId; s.refs = start.refs; s.pending = start.pending; s.indexHash = start.indexHash
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
              Set(o.keys).isSubset(of: ["version", "root", "pid", "head", "headId", "started", "refs", "pending", "indexHash", "protected"]),
              let s = try? JSONDecoder().decode(Stored.self, from: data) else { return (nil, "not valid") }
        guard s.version == 1, s.root == root, s.refs.count <= maxRefs, s.protected.count <= maxProtected, s.pending.count <= 3, s.indexHash.count == 64,
              s.indexHash.allSatisfy({ $0.isHexDigit }) else { return (nil, "unexpected content") }
        func okName(_ n: String) -> Bool { !n.isEmpty && n.utf8.count <= 1024 && !n.contains(where: { $0 == "\0" || $0 == "\n" || $0 == "\r" }) && !n.hasPrefix("-") }
        func okID(_ i: String) -> Bool { (i.count == 40 || i.count == 64) && i.allSatisfy { $0.isHexDigit && !$0.isUppercase } }
        guard (s.headId ?? "").isEmpty || okID(s.headId!) else { return (nil, "bad head id") }
        guard s.head.utf8.count <= 1024, !s.head.contains(where: { $0 == "\0" || $0 == "\n" || $0 == "\r" }) else { return (nil, "bad head") }
        for (r, id) in s.refs where !(r.hasPrefix("refs/") && okName(r) && okID(id)) { return (nil, "bad ref") }
        for (p, v) in s.protected {
            let f = v.split(separator: " ", omittingEmptySubsequences: false).map(String.init)
            guard okName(p), f.count == 3, f[0].count == 6, f[0].allSatisfy({ ("0"..."7").contains($0) }), okID(f[1]), f[2].count == 1, ("0"..."3").contains(f[2].first!) else { return (nil, "bad index entry") }
        }
        guard s.pending.allSatisfy({ ["MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"].contains($0) }) else { return (nil, "bad state name") }
        var st = State(); st.head = s.head; st.headId = s.headId ?? ""; st.refs = s.refs; st.index = s.protected; st.pending = s.pending; st.indexHash = s.indexHash; st.partial = true
        return (st, "")
    }

    /// Files of runs that did not finish (their process is gone) in this folder: compare and report, prefixed, then remove them.
    static func recover(_ root: String, dir: String, selfPID: pid_t, alive: (pid_t, String) -> Bool, maxCommits: Int = GitWatch.maxCommitsDefault, maxOutput: Int = GitWatch.maxOutputDefault) -> [String] {
        let fm = FileManager.default, prefix = key(root) + "-"
        var out: [String] = []
        for name in ((try? fm.contentsOfDirectory(atPath: dir)) ?? []).sorted() where name.hasPrefix(prefix) && name.hasSuffix(".json") {
            guard let pid = pid_t(name.dropFirst(prefix.count).dropLast(5)), pid != selfPID else { continue }
            let f = dir + "/" + name
            var data = Data()
            if let h = FileHandle(forReadingAtPath: f) { data = h.readData(ofLength: maxFile + 1); try? h.close() }
            let started = ((try? JSONSerialization.jsonObject(with: data)) as? [String: Any])?["started"] as? String ?? ""  // lenient: a damaged file has none, the id alone decides
            if alive(pid, started) { continue }
            let (st, why) = parse(data, root: root)
            guard let st else { try? fm.removeItem(atPath: f); out.append("minigw: ignoring a damaged git watch state file (\(why))"); continue }
            var w = GitWatch(root: root, start: st); w.maxCommits = maxCommits; w.maxOutput = maxOutput
            let r = w.compare()
            for l in r.lines { out.append(l.replacingOccurrences(of: "minigw: ", with: "minigw: from the previous run that did not finish: ", options: [], range: l.range(of: "minigw: "))) }
            // Only a complete comparison removes the baseline; an incomplete one is tried again at the next start, for a week at most.
            let age = ((try? fm.attributesOfItem(atPath: f))?[.modificationDate] as? Date).map { -$0.timeIntervalSinceNow } ?? .infinity
            if r.complete || age > Self.keepIncomplete { try? fm.removeItem(atPath: f) }
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
