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
    defer { try? fm.removeItem(at: root) }
    try fm.createDirectory(at: root, withIntermediateDirectories: true)
    _ = git(root.path, "git init -q -b main && echo a > a.txt && git add a.txt && git commit -qm init && git branch side")
    let w = try #require(GitWatch.begin(root.path))
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
    #expect(GitWatch.begin(plain.path) == nil)
    try Data("gitdir: ../elsewhere\n".utf8).write(to: plain.appendingPathComponent(".git"))
    #expect(GitWatch.begin(plain.path) == nil)
}
