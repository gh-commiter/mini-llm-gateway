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

private func git(_ dir: String, _ args: String) -> String {
    let p = Process(), pipe = Pipe()
    p.executableURL = URL(fileURLWithPath: "/bin/sh")
    p.arguments = ["-c", "export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_AUTHOR_NAME=n GIT_AUTHOR_EMAIL=a@b GIT_COMMITTER_NAME=n GIT_COMMITTER_EMAIL=a@b; cd '\(dir)' && \(args)"]
    p.standardOutput = pipe; p.standardError = pipe
    try? p.run()
    let d = pipe.fileHandleForReading.readDataToEndOfFile(); p.waitUntilExit()
    return String(decoding: d, as: UTF8.self)
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
    #expect(GitWatch.recover(root.path, dir: dir, selfPID: getpid() + 1, alive: { _ in true }).isEmpty, "a live run is not reported")
    let lines = GitWatch.recover(root.path, dir: dir, selfPID: getpid() + 1, alive: { _ in false }).joined(separator: "\n")
    for want in ["from the previous run that did not finish: git in this folder", "1 new commit(s)", "new branches: feature", "committed a protected path: .envrc"] { #expect(lines.contains(want), "missing \(want) in \(lines)") }
    #expect(!fm.fileExists(atPath: file))
    #expect(GitWatch.recover(root.path, dir: dir, selfPID: getpid() + 1, alive: { _ in false }).isEmpty, "reported once")
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
    let bad: [(String, Data)] = [("version", data { $0.version = 2 }), ("root", data { $0.root = "/elsewhere" }), ("ref name", data { $0.refs = ["../x": id] }),
        ("ref id", data { $0.refs = ["refs/heads/x": "zz"] }), ("entry", data { $0.protected = [".envrc": "bad"] }), ("pending", data { $0.pending = ["HEAD"] }),
        ("hash", data { $0.indexHash = "short" }), ("unknown field", Data(#"{"version":1,"root":"/some/folder","extra":1}"#.utf8)), ("not json", Data("nope".utf8)),
        ("too large", Data(count: GitWatch.maxFile + 1))]
    for (name, d) in bad { #expect(GitWatch.parse(d, root: root).0 == nil, "\(name) accepted") }
    try fm.createDirectory(atPath: dir, withIntermediateDirectories: true)
    try Data("not json".utf8).write(to: URL(fileURLWithPath: GitWatch.file(root, dir: dir, pid: 99999)))
    let l = GitWatch.recover(root, dir: dir, selfPID: 1, alive: { _ in false })
    #expect(l.count == 1 && l[0].contains("ignoring a damaged git watch state file"), "\(l)")
    #expect(!fm.fileExists(atPath: GitWatch.file(root, dir: dir, pid: 99999)))
}
