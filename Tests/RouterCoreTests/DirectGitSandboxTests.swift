import Foundation
import Testing
@testable import RouterCore

// 0.6.0 (#112): in direct work mode an agent can commit in the client's repository. Real processes under the sandbox mechanisms of this Mac:
// Codex's permission profile through `codex sandbox` (the profile Codex.args builds for a direct-mode folder). Claude CLI's own sandbox was run
// for real once with the generated settings (see the report); here its generated rules are checked. Copilot cannot run git (no commands, GIT_DIR=/dev/null)
// and keeps the Seatbelt profile that denies `.git`; Grok has no `.git` rule in its sandbox, the client's guard (GitGuardTests) is its only one.

private func run(_ exe: String, _ args: [String]) -> (status: Int32, out: String) {
    let p = Process(), pipe = Pipe()
    p.executableURL = URL(fileURLWithPath: exe); p.arguments = args; p.standardOutput = pipe; p.standardError = pipe
    guard (try? p.run()) != nil else { return (-1, "cannot run \(exe)") }
    var data = Data()
    let reader = Thread { data = pipe.fileHandleForReading.readDataToEndOfFile() }
    reader.start()
    p.waitUntilExit()
    while reader.isExecuting { usleep(5_000) }
    return (p.terminationStatus, String(decoding: data, as: UTF8.self))
}

private func profileConfig(_ a: [String]) -> [String] {
    var o: [String] = []
    for (i, x) in a.enumerated() where x == "-c" && i + 1 < a.count && (a[i + 1].hasPrefix("default_permissions") || a[i + 1].hasPrefix("permissions.")) { o += ["-c", a[i + 1]] }
    return o
}

private let gitEnv = "export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0 GIT_AUTHOR_NAME=n GIT_AUTHOR_EMAIL=a@b GIT_COMMITTER_NAME=n GIT_COMMITTER_EMAIL=a@b; "

/// <base>/repo: a repository with one commit and a submodule `sub`, plus <base>/tmp for the agent's TMPDIR.
private struct Repo {
    let base: URL, repo: URL, tmp: URL
    init() throws {
        let fm = FileManager.default
        base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("direct-git-" + UUID().uuidString, isDirectory: true)
        repo = base.appendingPathComponent("repo"); tmp = base.appendingPathComponent("tmp")
        let up = base.appendingPathComponent("up")
        for d in [repo, tmp, up] { try fm.createDirectory(at: d, withIntermediateDirectories: true) }
        func sh(_ cmd: String, _ cwd: URL) -> Bool { run("/bin/sh", ["-c", gitEnv + "cd '\(cwd.path)' && " + cmd]).status == 0 }
        #expect(sh("git init -q && echo a > a && git add a && git commit -qm up", up))
        #expect(sh("git init -q && echo hi > a.txt && git add a.txt && git commit -qm init && git branch x && git -c protocol.file.allow=always submodule add -q '\(up.path)' sub && git commit -qm sub && git init -q nest && echo n > nest/n && git -C nest add n && git -C nest commit -qm nested", repo))
    }
    func cleanup() { try? FileManager.default.removeItem(at: base) }
    func head() -> String { run("/bin/sh", ["-c", gitEnv + "cd '\(repo.path)' && git log --oneline | wc -l"]).out.trimmingCharacters(in: .whitespacesAndNewlines) }
}

@Test func codexDirectProfileLetsTheAgentCommitAndNothingElseInGit() throws {
    let exe = try #require(["/opt/homebrew/bin/codex", "/usr/local/bin/codex"].first { FileManager.default.isExecutableFile(atPath: $0) }, "codex is installed on the Mac that runs this gate")
    let r = try Repo(); defer { r.cleanup() }
    let out = r.base.appendingPathComponent("o.txt")
    func cfg(write: Bool = true, direct: Bool = true) -> [String] {
        profileConfig(Codex.args(cwd: r.repo, extraDirs: [], agent: true, output: out, write: write, deny: [], tmp: r.tmp.path, directGit: direct))
    }
    func under(_ c: [String], _ cmd: String) -> (status: Int32, out: String) { run(exe, ["sandbox"] + c + ["--", "/bin/sh", "-c", gitEnv + "export TMPDIR='\(r.tmp.path)/'; cd '\(r.repo.path)' && " + cmd]) }
    let w = cfg()
    // what an agent does: add, commit, branch, tag, checkout, stash, merge of a local branch, and no commit inside the submodule (G-2)
    for cmd in ["echo more >> a.txt && git add a.txt && git commit -qm agent", "git branch feature", "git tag v1", "git checkout -q -b topic", "echo again >> a.txt && git stash -q && git stash list | grep -q stash",
                "git checkout -q master 2>/dev/null || git checkout -q main", "git merge -q --no-edit x", "git gc -q", "git repack -a -d -q && git prune --expire=now"] {
        let o = under(w, cmd)
        #expect(o.status == 0, "\(cmd): \(o)")
    }
    #expect(r.head() == "3", "init, sub and the agent's commit: \(r.head())")  // merge of x was a fast-forward, nothing more
    // what can run code or redirect git of the owner is refused, also in the submodule's git dir
    let git = r.repo.appendingPathComponent(".git")
    let denied = ["echo '[core]' >> .git/config", "echo x > .git/hooks/pre-commit", "mkdir -p .git/info && echo x > .git/info/attributes", "mkdir -p .git/objects/info && echo /x > .git/objects/info/alternates",
                  "mkdir .git/rebase-merge && echo 'exec x' > .git/rebase-merge/git-rebase-todo", "mkdir .git/rebase-apply", "mkdir .git/sequencer", "echo x > .git/shallow", "echo x > .git/commondir", "mkdir .git/worktrees",
                  "echo s > sub/s.txt && git -C sub add s.txt", "echo m > nest/m && git -C nest add m",  // round 3 (G-2): a commit inside a submodule or a nested clone is closed, the watch audits one repository
                  "echo '[core]' >> nest/.git/config", "echo x > nest/.git/hooks/pre-commit", "mkdir -p nest/.git/info && echo x > nest/.git/info/attributes", "mv nest/.git nest/.g",
                  "echo '[core]' >> .git/modules/sub/config", "echo x > .git/modules/sub/hooks/pre-commit", "mkdir -p .git/modules/sub/info && echo x > .git/modules/sub/info/attributes"]
    for cmd in denied {
        let o = under(w, cmd)
        #expect(o.status != 0, "\(cmd): \(o)")
    }
    for p in ["../nest/.git/hooks/pre-commit", "../nest/.git/info/attributes", "hooks/pre-commit", "info/attributes", "objects/info/alternates", "rebase-merge", "rebase-apply", "sequencer", "shallow", "commondir", "worktrees", "modules/sub/hooks/pre-commit", "modules/sub/info/attributes"] {
        #expect(!FileManager.default.fileExists(atPath: git.appendingPathComponent(p).path), "\(p)")
    }
    let config = (try? String(contentsOf: git.appendingPathComponent("config"), encoding: .utf8)) ?? ""
    #expect(!config.hasSuffix("[core]\n") && !((try? String(contentsOf: git.appendingPathComponent("modules/sub/config"), encoding: .utf8)) ?? "").hasSuffix("[core]\n"))
    // another letter case of the same folder (a case-insensitive volume): still the same refusal
    if FileManager.default.fileExists(atPath: r.repo.appendingPathComponent(".GIT").path) {
        #expect(under(w, "echo '[core]' >> .GIT/config").status != 0 && under(w, "echo x > .Git/hooks/pre-commit").status != 0)
    }
    // #113: renaming a folder that holds a refused path (to write there and move it back) is refused too
    for cmd in ["mv .git .g", "mv .git/modules .git/m", "mv .git/modules/sub .git/modules/sub2", "mv .git/hooks .git/hooks2", "mv .git/info .git/info2"] {
        let o = under(w, cmd)
        #expect(o.status != 0, "\(cmd): \(o)")
        #expect(FileManager.default.fileExists(atPath: git.appendingPathComponent("modules/sub/config").path), "\(cmd)")
    }
    // read-only (--minigw-ro): nothing is writable, not even git's own data
    #expect(under(cfg(write: false), "git add a.txt").status != 0 && under(cfg(write: false), "echo x > .git/refs/heads/z").status != 0)
    // without direct mode (a sync copy, the default): .git stays read-only as it was
    let before = r.head()
    #expect(under(cfg(direct: false), "echo x >> a.txt && git add a.txt && git commit -qm no").status != 0 && r.head() == before)
}

@Test func claudeSandboxedModeKeepsGitDataWritableAndFileToolsOut() throws {
    var local = ClaudeCLIConfig(); local.allowLocalTools = true
    var restricted = ClaudeCLIConfig(); restricted.allowLocalTools = false; restricted.tools = "Read,Edit,Write"
    let d = URL(fileURLWithPath: "/private/tmp/minigw-direct-git-claude")
    func arg(_ a: [String], _ flag: String) throws -> String { try #require(a.firstIndex(of: flag).map { a[$0 + 1] }) }
    func settings(_ a: [String]) throws -> [String: Any] {
        let json = try arg(a, "--settings")
        return try #require(try JSONSerialization.jsonObject(with: Data(json.utf8)) as? [String: Any])
    }
    let a = local.agentArgs(cwd: d, allTools: [], folders: AllowedFolders(), checkedPaths: [])
    let fsRules = try #require((try settings(a)["sandbox"] as? [String: Any])?["filesystem"] as? [String: Any])
    let deny = try #require(fsRules["denyWrite"] as? [String])
    // no rule names the whole `.git` of the folder or the index, objects, refs, HEAD; every closed name is there (a real `claude -p` committed under these)
    for g in ClaudeCLIConfig.directGitDenies { #expect(deny.contains(d.path + "/.git/" + g), "\(g)") }
    for w in ["index", "index.lock", "objects", "refs", "HEAD", "logs", "packed-refs"] { #expect(!deny.contains(d.path + "/.git/" + w) && !deny.contains(d.path + "/.git"), "\(w)") }
    let tools = try arg(a, "--disallowedTools")
    #expect(tools.contains("Edit(/\(d.path)/.git/**)") && tools.contains("Edit(/\(d.path)/**/.git/**)"), "the file tools stay out of .git")
    // the restricted mode (no Bash): the same, and a read-only run has no git data rule to lift
    let ra = restricted.agentArgs(cwd: d, allTools: [], folders: AllowedFolders(), checkedPaths: [])
    let rtools = try arg(ra, "--disallowedTools")
    #expect(rtools.contains("Edit(/\(d.path)/.git/**)") && rtools.contains("Edit(/\(d.path)/**/.git/**)"))
}

@Test func claudeRulesCoverTheFoldersAboveTheClosedGitPaths() throws {
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("rules-" + UUID().uuidString, isDirectory: true)
    defer { try? fm.removeItem(at: base) }
    let git = base.appendingPathComponent(".git")
    for d in ["modules/sub/hooks", "modules/sub/info"] { try fm.createDirectory(at: git.appendingPathComponent(d), withIntermediateDirectories: true) }
    try Data("c".utf8).write(to: git.appendingPathComponent("modules/sub/config"))
    try Data("ref: refs/heads/x".utf8).write(to: git.appendingPathComponent("modules/sub/HEAD"))
    try fm.createDirectory(at: base.appendingPathComponent("a/b/.git/hooks"), withIntermediateDirectories: true)
    var local = ClaudeCLIConfig(); local.allowLocalTools = true
    let a = local.agentArgs(cwd: base, allTools: [], folders: AllowedFolders(), checkedPaths: [])
    let json = try #require(a.firstIndex(of: "--settings").map { a[$0 + 1] })
    let st = try #require(try JSONSerialization.jsonObject(with: Data(json.utf8)) as? [String: Any])
    let deny = try #require(((st["sandbox"] as? [String: Any])?["filesystem"] as? [String: Any])?["denyWrite"] as? [String])
    // literal paths: Claude's sandbox then also refuses `mv .git/modules m` (a real run: the globs alone did not)
    for p in ["config", "hooks", "info"] { #expect(deny.contains(git.path + "/modules/sub/" + p), "\(p)") }
    // a nested repository found at launch: its closed paths literally (the globs name entries only)
    // (G-2: the whole git directory of a nested repository)
    #expect(deny.contains(base.path + "/a/b/.git"))
    #expect(ClaudeCLIConfig.directGitDenies.contains("modules"))
    #expect(ClaudeCLIConfig.nestedGitDirs(base.path) == [base.path + "/a/b/.git"])
    // worktree mode: the one shared list, config.worktree, worktrees and the submodule folders included
    let wt = local.worktreeArgs(root: base, maven: base, cwd: base, allTools: [], network: false)
    let wjson = try #require(wt.firstIndex(of: "--settings").map { wt[$0 + 1] })
    let wst = try #require(try JSONSerialization.jsonObject(with: Data(wjson.utf8)) as? [String: Any])
    let wdeny = try #require(((wst["sandbox"] as? [String: Any])?["filesystem"] as? [String: Any])?["denyWrite"] as? [String])
    for g in ClaudeCLIConfig.gitDenies { #expect(wdeny.contains(git.path + "/" + g), "\(g)") }
    #expect(wdeny.contains(git.path + "/modules/sub/config") && wdeny.contains(git.path + "/modules/**/hooks"))
}

// #116 B4: a folder whose path cannot go into a rule keeps no `.git` Edit denies, so the sandboxed branch does not use it (an allowed folder) or switches the file tools off (the session folder).
@Test func theSandboxedBranchDropsFoldersThatCannotGoIntoARule() {
    var local = ClaudeCLIConfig(); local.allowLocalTools = true
    let a = local.agentArgs(cwd: URL(fileURLWithPath: "/tmp/ok"), allTools: [], folders: AllowedFolders(), checkedPaths: ["/tmp/bad (1)", "/tmp/fine"])
    #expect(!a.contains("/tmp/bad (1)") && a.contains("/tmp/fine"))
    let b = local.agentArgs(cwd: URL(fileURLWithPath: "/tmp/p (1)"), allTools: [], folders: AllowedFolders(), checkedPaths: [])
    let denied = b.firstIndex(of: "--disallowedTools").map { b[$0 + 1] } ?? ""
    #expect(["Edit", "Write", "MultiEdit", "NotebookEdit"].allSatisfy { denied.contains($0) }, "\(denied)")
}
