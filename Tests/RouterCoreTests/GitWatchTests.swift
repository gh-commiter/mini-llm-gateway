import Foundation
import Testing
@testable import RouterCore

// #116 B1: client/testdata/git-watch.json is asserted here and in client/gitwatch_test.go.
private struct Case: Decodable { let name: String; let start: GitWatch.State; let end: GitWatch.State; let commits: [GitWatch.Commit]; let lines: [String]; let reset: [String] }

@Test func gitWatchReportMatchesTheSharedTable() throws {
    let root = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
    struct File: Decodable { let cases: [Case] }
    let f = try JSONDecoder().decode(File.self, from: Data(contentsOf: root.appendingPathComponent("client/testdata/git-watch.json")))
    #expect(!f.cases.isEmpty)
    for c in f.cases {
        let r = GitWatch.report(start: c.start, end: c.end, commits: c.commits, isProtected: { Sync.isProtected($0) })
        #expect(r.lines == c.lines && r.reset == c.reset, "\(c.name): \(r)")
    }
}

/// Runs a shell in `dir` with the same git the product uses (Worktree.git, not the /usr/bin shim). A non-zero exit of the shell is a test issue with its output, unless `fails`
/// says it is expected: a silent `&&` chain that stopped early hid the cause of a flake.
private func git(_ dir: String, _ args: String, fails: Bool = false, sourceLocation: SourceLocation = #_sourceLocation) -> String {
    let p = Process(), pipe = Pipe()
    p.executableURL = URL(fileURLWithPath: "/bin/sh")
    let gitDir = (Worktree.git as NSString).deletingLastPathComponent
    p.arguments = ["-c", "export PATH='\(gitDir)':\"$PATH\" GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_AUTHOR_NAME=n GIT_AUTHOR_EMAIL=a@b GIT_COMMITTER_NAME=n GIT_COMMITTER_EMAIL=a@b; cd '\(dir)' && \(args)"]
    p.standardOutput = pipe; p.standardError = pipe
    try? p.run()
    let d = pipe.fileHandleForReading.readDataToEndOfFile(); p.waitUntilExit()
    let out = String(decoding: d, as: UTF8.self)
    if p.terminationStatus != 0, !fails { Issue.record("shell exited \(p.terminationStatus): \(args)\n\(out)", sourceLocation: sourceLocation) }
    return out
}

// A real repository: what an agent does through the share is reported, the staged protected path leaves the index, the work tree, the commit and MERGE_HEAD stay.
@Test func gitWatchRealRepository() throws {
    let fm = FileManager.default
    let root = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("gw-" + UUID().uuidString)
    defer { try? fm.removeItem(at: root); try? fm.removeItem(atPath: root.path + "-state") }
    try fm.createDirectory(at: root, withIntermediateDirectories: true)
    _ = git(root.path, "git init -q -b main && echo a > a.txt && git add a.txt && git commit -qm init && git branch side")
    let w = try #require(GitWatch.begin(root.path, stateDir: root.path + "-state"))
    _ = git(root.path, "echo 'echo hi' > .envrc && git add .envrc && git commit -qm envrc && git branch feature && mkdir .vscode && echo '{}' > .vscode/tasks.json && git add .vscode/tasks.json")
    let side = git(root.path, "git rev-parse side").trimmingCharacters(in: .whitespacesAndNewlines)
    try Data((side + "\n").utf8).write(to: root.appendingPathComponent(".git/MERGE_HEAD"))
    let out = w.finish().joined(separator: "\n")
    for want in ["1 new commit(s)", "new branches: feature", "committed a protected path: .envrc", "staged a protected path: .vscode/tasks.json", "merge, cherry-pick or revert is in progress"] {
        #expect(out.contains(want), "missing \(want) in \(out)")
    }
    #expect(git(root.path, "git ls-files -s .vscode/tasks.json").isEmpty, "still in the index")
    #expect(fm.fileExists(atPath: root.appendingPathComponent(".vscode/tasks.json").path))
    #expect(git(root.path, "git log --oneline").contains("envrc"))
    #expect(fm.fileExists(atPath: root.appendingPathComponent(".git/MERGE_HEAD").path))
    // nothing to watch: not a repository, a gitdir: file
    let plain = root.appendingPathComponent("plain"); try fm.createDirectory(at: plain, withIntermediateDirectories: true)
    #expect(GitWatch.begin(plain.path, stateDir: root.path + "-state") == nil)
    try Data("gitdir: ../elsewhere\n".utf8).write(to: plain.appendingPathComponent(".git"))
    #expect(GitWatch.begin(plain.path, stateDir: root.path + "-state") == nil)
}

// X2: a client that was killed (the end of the share never ran) leaves its stored start state; the next start in the folder reports it, prefixed, and removes it.
@Test func gitWatchSurvivesAKilledClient() throws {
    let fm = FileManager.default
    let root = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("gk-" + UUID().uuidString)
    let dir = root.path + "-state"
    defer { try? fm.removeItem(at: root); try? fm.removeItem(atPath: dir) }
    try fm.createDirectory(at: root, withIntermediateDirectories: true)
    _ = git(root.path, "git init -q -b main && echo a > a.txt && git add a.txt && git commit -qm init")
    let w = try #require(GitWatch.begin(root.path, stateDir: dir))
    let file = try #require(w.file)
    let fileMode = try fm.attributesOfItem(atPath: file)[.posixPermissions] as? Int, dirMode = try fm.attributesOfItem(atPath: dir)[.posixPermissions] as? Int
    #expect(fileMode == 0o600 && dirMode == 0o700)
    #expect(!file.hasPrefix(root.path + "/"))
    // the agent works, the client is killed: no finish
    _ = git(root.path, "echo 'echo hi' > .envrc && git add .envrc && git commit -qm envrc && git branch feature")
    #expect(GitWatch.recover(root.path, dir: dir, selfPID: getpid() + 1, alive: { _, _ in true }).isEmpty, "a live run is not reported")
    let lines = GitWatch.recover(root.path, dir: dir, selfPID: getpid() + 1, alive: { _, _ in false }).joined(separator: "\n")
    for want in ["from the previous run that did not finish: git in this folder", "1 new commit(s)", "new branches: feature", "committed a protected path: .envrc"] { #expect(lines.contains(want), "missing \(want) in \(lines)") }
    #expect(!fm.fileExists(atPath: file))
    #expect(GitWatch.recover(root.path, dir: dir, selfPID: getpid() + 1, alive: { _, _ in false }).isEmpty, "reported once")
}

@Test func gitWatchIgnoresADamagedStateFile() throws {
    let fm = FileManager.default
    let dir = fm.temporaryDirectory.appendingPathComponent("gws-" + UUID().uuidString).path
    defer { try? fm.removeItem(atPath: dir) }
    let root = "/some/folder", id = String(repeating: "a", count: 40), h = String(repeating: "b", count: 64)
    func data(_ mutate: (inout GitWatch.Stored) -> Void) -> Data {
        var s = GitWatch.Stored(); s.root = root; s.head = "main"; s.refs = ["refs/heads/main": id]; s.indexHash = h; mutate(&s)
        return try! JSONEncoder().encode(s)
    }
    #expect(GitWatch.parse(data { _ in }, root: root).0 != nil)
    #expect(GitWatch.parse(data { $0.headId = id }, root: root).0?.headId == id && GitWatch.parse(data { $0.headId = "zz" }, root: root).0 == nil)
    let bad: [(String, Data)] = [("version", data { $0.version = 2 }), ("root", data { $0.root = "/elsewhere" }), ("ref name", data { $0.refs = ["../x": id] }),
        ("ref id", data { $0.refs = ["refs/heads/x": "zz"] }), ("entry", data { $0.protected = [".envrc": "bad"] }), ("pending", data { $0.pending = ["HEAD"] }),
        ("hash", data { $0.indexHash = "short" }), ("unknown field", Data(#"{"version":1,"root":"/some/folder","extra":1}"#.utf8)), ("not json", Data("nope".utf8)),
        ("too large", Data(count: GitWatch.maxFile + 1))]
    for (name, d) in bad { #expect(GitWatch.parse(d, root: root).0 == nil, "\(name) accepted") }
    try fm.createDirectory(atPath: dir, withIntermediateDirectories: true)
    try Data("not json".utf8).write(to: URL(fileURLWithPath: GitWatch.file(root, dir: dir, pid: 99999)))
    let l = GitWatch.recover(root, dir: dir, selfPID: 1, alive: { _, _ in false })
    #expect(l.count == 1 && l[0].contains("ignoring a damaged git watch state file"), "\(l)")
    #expect(!fm.fileExists(atPath: GitWatch.file(root, dir: dir, pid: 99999)))
}

// MARK: review of 0.6.0 (findings 2, 4, 5, 6, 7), the same scenarios as client/gitwatch_test.go

private func watched(_ name: String, _ setup: String = "git init -q -b main && echo a > a.txt && git add a.txt && git commit -qm init") throws -> (root: URL, state: String) {
    let fm = FileManager.default
    let root = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("gw-\(name)-" + UUID().uuidString)
    try fm.createDirectory(at: root, withIntermediateDirectories: true)
    _ = git(root.path, setup)
    return (root, root.path + "-state")
}

@Test func gitWatchReportsReplacementRefsAndReadsWithoutThem() throws {
    for packed in [false, true] {
        let (root, state) = try watched("repl"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
        let w = try #require(GitWatch.begin(root.path, stateDir: state))
        let head = git(root.path, "git rev-parse HEAD").trimmingCharacters(in: .whitespacesAndNewlines)
        _ = git(root.path, "echo 'echo hi' > .envrc && git add .envrc && tree=$(git write-tree) && repl=$(git commit-tree $tree -m replacement) && git reset -q && rm .envrc && git update-ref refs/replace/\(head) $repl" + (packed ? " && git pack-refs --all" : ""))
        let out = w.finish().joined(separator: "\n")
        for want in ["replacement ref (\(head))", "committed a protected path: .envrc", "other refs changed: refs/replace/\(head)"] { #expect(out.contains(want), "missing \(want) in \(out)") }
    }
    // a new commit with a protected path hidden by a replacement of it with the old commit
    let (root, state) = try watched("hide"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
    let w = try #require(GitWatch.begin(root.path, stateDir: state))
    _ = git(root.path, "old=$(git rev-parse HEAD) && echo 'echo hi' > .envrc && git add .envrc && git commit -qm envrc && git update-ref refs/replace/$(git rev-parse HEAD) $old")
    let out = w.finish().joined(separator: "\n")
    for want in ["1 new commit(s)", "committed a protected path: .envrc", "replacement ref ("] { #expect(out.contains(want), "missing \(want) in \(out)") }
}

@Test func gitWatchSeesACommitOnADetachedHead() throws {
    for atStart in [false, true] {
        let (root, state) = try watched("det"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
        if atStart { _ = git(root.path, "git checkout -q --detach") }
        let w = try #require(GitWatch.begin(root.path, stateDir: state))
        _ = git(root.path, "git checkout -q --detach && mkdir .claude && echo '{}' > .claude/settings.json && git add .claude/settings.json && git commit -qm settings")
        let out = w.finish().joined(separator: "\n")
        for want in ["1 new commit(s)", "committed a protected path: .claude/settings.json"] + (atStart ? ["detached HEAD moved"] : []) { #expect(out.contains(want), "missing \(want) in \(out)") }
    }
}

@Test func gitWatchSeesWhatAMergeBringsIn() throws {
    let (root, state) = try watched("merge"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
    _ = git(root.path, "git checkout -q -b side && echo 'echo hi' > .envrc && git add .envrc && git commit -qm side && git checkout -q main && echo b > b.txt && git add b.txt && git commit -qm b")
    let w = try #require(GitWatch.begin(root.path, stateDir: state))
    _ = git(root.path, "git merge -q --no-ff --no-edit side")
    let out = w.finish().joined(separator: "\n")
    for want in ["1 new commit(s)", "committed a protected path: .envrc"] { #expect(out.contains(want), "missing \(want) in \(out)") }
}

// An incomplete check never changes the owner's index: the staged paths came from a read that failed in part (a committed .envrc looked staged, 0.7.36 flake).
@Test func gitWatchLeavesTheIndexAloneWhenTheCheckIsIncomplete() {
    var s = GitWatch.State(), e = GitWatch.State()
    s.refs = ["refs/heads/main": "a"]; s.index = ["a.txt": "100644 aaaa 0"]
    e.refs = ["refs/heads/main": "b"]; e.index = ["a.txt": "100644 aaaa 0", ".envrc": "100644 bbbb 0"]; e.cached = [".envrc"]; e.incomplete = ["history not read in full"]
    let r = GitWatch.report(start: s, end: e, commits: [], isProtected: { Sync.isProtected($0) })
    #expect(r.reset.isEmpty)
    #expect(r.lines.contains { $0.contains("a protected path looks staged: .envrc - NOT removed from the index") })
    e.incomplete = []
    #expect(GitWatch.report(start: s, end: e, commits: [], isProtected: { Sync.isProtected($0) }).reset == [".envrc"])
}

// End to end: HEAD points at a branch that does not exist (diff --cached then lists the whole index, .envrc included) and the tip commit is corrupt (rev-list fails).
@Test func gitWatchKeepsACommittedPathInTheIndexWhenTheHistoryCannotBeRead() throws {
    let (root, state) = try watched("inc1"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
    let w = try #require(GitWatch.begin(root.path, stateDir: state))
    _ = git(root.path, "echo 'echo hi' > .envrc && git add .envrc && git commit -qm envrc && git symbolic-ref HEAD refs/heads/ghost && h=$(git rev-parse main) && f=.git/objects/$(echo $h | cut -c1-2)/$(echo $h | cut -c3-) && chmod u+w $f && echo junk > $f")
    let out = w.finish().joined(separator: "\n")
    #expect(out.contains("history not read in full") && out.contains("NOT removed from the index"), "\(out)")
    #expect(git(root.path, "git ls-files -s .envrc").contains(".envrc"), "the watcher took a path out of the index: \(out)")
}

@Test func gitWatchSaysWhenALimitWasReached() throws {
    var (root, state) = try watched("lim1"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
    var w = try #require(GitWatch.begin(root.path, stateDir: state))
    w.maxCommits = 3
    var file = try #require(w.file)
    _ = git(root.path, "echo 'echo hi' > .envrc && git add .envrc && git commit -qm envrc && for i in 1 2 3 4; do git commit -q --allow-empty -m more; done")
    var out = w.finish().joined(separator: "\n")
    #expect(out.contains("INCOMPLETE") && out.contains("more than 3 new commits"), "\(out)")
    #expect(FileManager.default.fileExists(atPath: file), "the stored start state is gone after an incomplete check")
    // the output limit: the index listing is cut
    (root, state) = try watched("lim2"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
    w = try #require(GitWatch.begin(root.path, stateDir: state))
    file = try #require(w.file)
    _ = git(root.path, "for i in $(seq 1 30); do echo x > file-with-a-long-name-$i.txt; done; git add .")
    w.maxOutput = 300
    out = w.finish().joined(separator: "\n")
    #expect(out.contains("INCOMPLETE") && out.contains("index not read in full"), "\(out)")
    #expect(FileManager.default.fileExists(atPath: file))
    // complete: no word about it, the file is removed
    (root, state) = try watched("lim3"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
    w = try #require(GitWatch.begin(root.path, stateDir: state))
    file = try #require(w.file)
    _ = git(root.path, "git commit -q --allow-empty -m one")
    out = w.finish().joined(separator: "\n")
    #expect(!out.contains("INCOMPLETE") && !FileManager.default.fileExists(atPath: file))
}

// The end report waits for the requests that hold the folder lock: what a running request stages is in the report and restored.
@Test func gitWatchEndReportWaitsForTheFolderLock() throws {
    let (root, state) = try watched("drain"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
    let lockDir = root.path + "-locks"; defer { try? FileManager.default.removeItem(atPath: lockDir) }
    let lock = FolderLock(id: [UInt64.random(in: 1...UInt64.max), 1], dir: lockDir)
    let w = try #require(GitWatch.begin(root.path, stateDir: state))
    let started = DispatchSemaphore(value: 0), release = DispatchSemaphore(value: 0)
    let running = Thread {  // a request in flight
        _ = lock.with(exclusive: false) { started.signal(); release.wait(); return 1 }
    }
    running.start(); started.wait()
    let done = DispatchSemaphore(value: 0)
    nonisolated(unsafe) var lines: [String] = []
    Thread { lines = w.finish(under: lock, wait: 10); done.signal() }.start()
    #expect(done.wait(timeout: .now() + 0.3) == .timedOut, "the end report did not wait for the running request")
    _ = git(root.path, "echo 'echo hi' > .envrc && git add .envrc")  // the running request finishes its write
    release.signal()
    #expect(done.wait(timeout: .now() + 10) == .success)
    #expect(lines.joined(separator: "\n").contains("staged a protected path: .envrc"), "\(lines)")
    #expect(git(root.path, "git ls-files -s .envrc").isEmpty, "the staged path was not restored")
}

// MARK: round 2 (H3-H6 and the closing gate), the same scenarios as client/gitwatch_test.go

@Test func gitWatchDrainTimeoutRestoresNothing() throws {
    let (root, state) = try watched("dt"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
    let lockDir = root.path + "-locks"; defer { try? FileManager.default.removeItem(atPath: lockDir) }
    let lock = FolderLock(id: [UInt64.random(in: 1...UInt64.max), 2], dir: lockDir)
    let w = try #require(GitWatch.begin(root.path, stateDir: state))
    let file = try #require(w.file)
    let started = DispatchSemaphore(value: 0), release = DispatchSemaphore(value: 0)
    Thread { _ = lock.with(exclusive: false) { started.signal(); release.wait(); return 1 } }.start(); started.wait()
    _ = git(root.path, "echo 'echo hi' > .envrc && git add .envrc")
    let out = w.finish(under: lock, wait: 0.2).joined(separator: "\n")
    release.signal()
    for want in ["INCOMPLETE", "requests of the agent were still running", "a protected path looks staged: .envrc", "NOT removed from the index"] { #expect(out.contains(want), "missing \(want) in \(out)") }
    #expect(!git(root.path, "git ls-files -s .envrc").isEmpty, "the index was restored without the lock")
    #expect(FileManager.default.fileExists(atPath: file), "the baseline is gone")
}

@Test func gitWatchUnreadableAtTheEndWarnsAndKeepsTheBaseline() throws {
    let (root, state) = try watched("ue"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
    let w = try #require(GitWatch.begin(root.path, stateDir: state))
    let file = try #require(w.file)
    try Data("garbage without a header\nnot a ref\n".utf8).write(to: root.appendingPathComponent(".git/packed-refs"))
    let out = w.finish().joined(separator: "\n")
    #expect(out.contains("INCOMPLETE") && out.contains("could not be read at the end"), "\(out)")
    #expect(FileManager.default.fileExists(atPath: file))
}

@Test func gitWatchRecoveryKeepsAnIncompleteBaseline() throws {
    let (root, state) = try watched("rk"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
    let w = try #require(GitWatch.begin(root.path, stateDir: state))
    let file = try #require(w.file)
    _ = git(root.path, "for i in 1 2 3 4 5; do git commit -q --allow-empty -m more; done")
    var out = GitWatch.recover(root.path, dir: state, selfPID: getpid() + 1, alive: { _, _ in false }, maxCommits: 3).joined(separator: "\n")
    #expect(out.contains("from the previous run that did not finish") && out.contains("INCOMPLETE"), "\(out)")
    #expect(FileManager.default.fileExists(atPath: file), "the baseline was removed after an incomplete comparison")
    out = GitWatch.recover(root.path, dir: state, selfPID: getpid() + 1, alive: { _, _ in false }).joined(separator: "\n")
    #expect(!out.contains("INCOMPLETE") && out.contains("5 new commit(s)"), "\(out)")
    #expect(!FileManager.default.fileExists(atPath: file), "the baseline stays after a complete comparison")
}

@Test func gitWatchReportsAMovedTag() throws {
    let (root, state) = try watched("mt"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
    _ = git(root.path, "git commit -q --allow-empty -m second && git tag v1 HEAD~1")
    let w = try #require(GitWatch.begin(root.path, stateDir: state))
    _ = git(root.path, "git tag -f v1 HEAD")
    #expect(w.finish().joined(separator: "\n").contains("moved tags: v1"))
}

// The end report closes the writes: after closeWrites a write that gets the lock is refused with 503, a read is not, and the lock is still the one the running requests hold.
@Test func folderLockRefusesWritesAfterTheyAreClosed() throws {
    let lockDir = FileManager.default.temporaryDirectory.appendingPathComponent("fl-" + UUID().uuidString).path; defer { try? FileManager.default.removeItem(atPath: lockDir) }
    let lock = FolderLock(id: [UInt64.random(in: 1...UInt64.max), 3], dir: lockDir)
    func ask(_ exclusive: Bool) -> Int { lock.respond(exclusive: exclusive, rootOK: { true }) { DAVResponse(200) }.status }
    #expect(ask(true) == 200 && ask(false) == 200)
    lock.closeWrites()
    #expect(ask(true) == 503 && ask(false) == 200)
    #expect(lock.respond(exclusive: false, rootOK: { false }) { DAVResponse(200) }.status == 403)
}

// MARK: round 3 (G-1, G-3, G-4, G-5), the same scenarios as client/gitwatch_test.go

private func fixtureRoot() -> URL { URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent() }

@Test func gitWatchHasTheSharedPins() throws {
    struct File: Decodable { struct Pins: Decodable { let args: [String]; let env: [String] }; let pins: Pins }
    let f = try JSONDecoder().decode(File.self, from: Data(contentsOf: fixtureRoot().appendingPathComponent("client/testdata/git-watch.json")))
    let args = GitWatch.pins + Worktree.guardrails  // every call of the watch runs through Worktree.run with safe: true
    let env = GitWatch.env.merging(Worktree.safeEnv) { $1 }
    for a in f.pins.args.map({ $0.replacingOccurrences(of: "{null}", with: "/dev/null") }) {
        let ok = args.contains(a) || zip(args, args.dropFirst()).contains { $0 == "-c" && $1 == a }
        #expect(ok, "the watch does not run git with \(a)")
    }
    for e in f.pins.env.map({ $0.replacingOccurrences(of: "{null}", with: "/dev/null") }) {
        let kv = e.split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false).map(String.init)
        #expect(env[kv[0]] == kv[1], "the watch does not run git with \(e) in its environment")
    }
}

@Test func gitWatchNamesParseRaw() throws {
    struct Log: Decodable { let name: String; let out: String; let files: [String] }
    struct File: Decodable { let logs: [Log] }
    let f = try JSONDecoder().decode(File.self, from: Data(contentsOf: fixtureRoot().appendingPathComponent("client/testdata/git-watch.json")))
    #expect(!f.logs.isEmpty)
    for l in f.logs { #expect(GitWatch.parseNames(l.out) == l.files, "\(l.name)") }
}

// Astra 5b90d47 finding 1: a name that begins with the commit-header marker is a name, not a commit.
@Test func gitWatchSeesAProtectedNameThatStartsWithTheMarker() throws {
    let (root, state) = try watched("marker"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
    let w = try #require(GitWatch.begin(root.path, stateDir: state))
    _ = git(root.path, "mkdir -p '@@payload/.claude' && echo x > 00.txt && echo '{}' > '@@payload/.claude/settings.json' && git add . && git commit -qm marker")
    let out = w.finish().joined(separator: "\n")
    #expect(out.contains("1 new commit(s)") && out.contains("committed a protected path: @@payload/.claude/settings.json"), "\(out)")
}

@Test func gitWatchSeesAProtectedPathWithAQuoteInItsName() throws {
    let (root, state) = try watched("quote"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
    let w = try #require(GitWatch.begin(root.path, stateDir: state))
    _ = git(root.path, "mkdir -p 'a\"b/.claude' && echo '{}' > 'a\"b/.claude/settings.json' && git add . && git commit -qm quoted")
    let out = w.finish().joined(separator: "\n")
    #expect(out.contains("a\"b/.claude/settings.json"), "\\(out)")
}

@Test func gitWatchRecoveryDoesNotTrustAReusedProcessID() throws {
    let (root, state) = try watched("reuse"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
    let w = try #require(GitWatch.begin(root.path, stateDir: state))
    let file = try #require(w.file)
    _ = git(root.path, "git branch feature")
    #expect(!GitWatch.procStart(getpid()).isEmpty)
    #expect(GitWatch.recover(root.path, dir: state, selfPID: getpid() + 1, alive: GitWatch.pidAlive).isEmpty, "this very process is still running")
    // the same process id, but it started at another time: another process
    let other = try String(contentsOfFile: file, encoding: .utf8).replacingOccurrences(of: "\"started\":\"", with: "\"started\":\"other-")
    try other.write(toFile: file, atomically: true, encoding: .utf8)
    let out = GitWatch.recover(root.path, dir: state, selfPID: getpid() + 1, alive: GitWatch.pidAlive).joined(separator: "\n")
    #expect(out.contains("new branches: feature"), "\\(out)")
}

@Test func gitWatchSaysWhenTheStartStateIsNotStored() throws {
    let (root, state) = try watched("nostore"); defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(atPath: state) }
    try Data().write(to: URL(fileURLWithPath: state))  // the state folder is a file: nothing can be stored
    let w = try #require(GitWatch.begin(root.path, stateDir: state))
    #expect(w.file == nil)
}

@Test func parseRawNamesTheModeAndTypeChanges() {
    let out = ":120000 100644 aaa bbb T\0link\0:100755 100644 ccc ddd M\0run.sh\0:000000 100644 0000 eee A\0new.txt\0:100644 100644 fff ggg M\0same.txt\0:100644 000000 hhh 0000 D\0plain.txt\0:120000 000000 iii 0000 D\0old-link\0:000000 100755 0000 jjj A\0new.sh\0"
    let (files, modes) = GitWatch.parseRaw(out)
    #expect(files == ["link", "run.sh", "new.txt", "same.txt", "plain.txt", "old-link", "new.sh"])
    #expect(modes == ["link 120000->100644", "run.sh 100755->100644", "old-link 120000->000000", "new.sh 000000->100755"])
}
