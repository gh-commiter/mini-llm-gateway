import Foundation
import Testing
@testable import RouterCore

// 0.7.0: the real Claude CLI, launched as the gateway launches a sandboxed (direct mode) run: the arguments of `agentArgs` and the git environment of the run.
// Gated (it uses the owner's subscription): MG_REAL_CLAUDE_GIT=1.

private func sh(_ cmd: String, _ cwd: String) -> (status: Int32, out: String) {
    let p = Process(), pipe = Pipe()
    p.executableURL = URL(fileURLWithPath: "/bin/sh"); p.arguments = ["-c", cmd]; p.currentDirectoryURL = URL(fileURLWithPath: cwd)
    p.standardOutput = pipe; p.standardError = pipe
    var env = ProcessInfo.processInfo.environment
    for k in env.keys where k.hasPrefix("GIT_") { env[k] = nil }
    env["GIT_CONFIG_GLOBAL"] = "/dev/null"; env["GIT_CONFIG_NOSYSTEM"] = "1"
    p.environment = env
    guard (try? p.run()) != nil else { return (-1, "") }
    let data = pipe.fileHandleForReading.readDataToEndOfFile()
    p.waitUntilExit()
    return (p.terminationStatus, String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines))
}

@Test(.enabled(if: ProcessInfo.processInfo.environment["MG_REAL_CLAUDE_GIT"] == "1"), .blocking, arguments: [true, false])
func aClaudeAgentCommitsInADirectModeFolderAndCannotPlantAHook(identity: Bool) async throws {
    guard let exe = await ClaudeCLI.executable("") else { try Test.cancel("claude is not installed") }
    let base = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("mgw-direct-git-real-" + UUID().uuidString.lowercased())
    let repo = base.appendingPathComponent("repo")
    try FileManager.default.createDirectory(at: repo, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: base) }
    #expect(sh("git init -q && echo x > a.txt && git add a.txt && git -c user.name=Owner -c user.email=o@example.com commit -qm init" + (identity ? " && git config user.name Owner && git config user.email o@example.com" : ""), repo.path).status == 0)
    var cfg = ClaudeCLIConfig(); cfg.allowLocalTools = true
    let args = cfg.agentArgs(cwd: repo, allTools: ["Bash"], folders: AllowedFolders(), checkedPaths: [], directGit: true)
        + ["-p", "--output-format", "json", "--model", "haiku", "--strict-mcp-config", "--setting-sources", ""]
    var env = ProcessInfo.processInfo.environment
    for k in env.keys where k.hasPrefix("ANTHROPIC_") || k.hasPrefix("CLAUDE_CODE_") || k == "CLAUDECODE" || k.hasPrefix("GIT_") { env[k] = nil }
    env["PATH"] = (env["PATH"] ?? "") + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
    env.merge(ClaudeCLIConfig.sandboxGitEnv(mount: false)) { $1 }
    let ask = "Run these as separate Bash calls, one simple command each (no chaining, no echo, no redirection of exit codes): 1) printf hi > b.txt  2) git add b.txt  3) git commit -m 'add b'  4) printf x > .git/hooks/pre-commit  5) printf x > .git/COMMONDIR  6) printf x > .git/objects/info/ALTERNATES. Report the exact output of each."
    let r = ClaudeSeatbelt.capture(exe, args, env: env, cwd: repo.path, input: ask, timeout: 240, log: base)
    print("[direct-git identity=\(identity)] author: \(sh("git log -1 --format=%an,%ae", repo.path).out); claude said: \((r.out.components(separatedBy: "\"result\":").last ?? "").prefix(900))")
    let log = sh("git log --format=%s", repo.path).out
    #expect(log.hasPrefix("add b"), "no commit: git log: \(log); claude: \(r.out.prefix(600)) \(r.err.prefix(200))")
    #expect(!FileManager.default.fileExists(atPath: repo.appendingPathComponent(".git/hooks/pre-commit").path), "a hook was planted")
    for p in ["commondir", "objects/info/alternates"] { #expect(!FileManager.default.fileExists(atPath: repo.appendingPathComponent(".git/" + p).path), "\(p) was written (a case variant)") }
}

/// The launch of a sandboxed agent run (fake claude): git gets no user and system configuration (the sandbox hides ~/.gitconfig, which git treats as fatal) and no
/// inherited GIT_* variable; the sandbox's denyWrite closes nested `.git` entries but not the folder's own `.git`, which a commit writes (a real run: index.lock refused).
@Test(.blocking) func aSandboxedAgentRunGetsASafeGitEnvironment() async throws {
    let base = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("mgw-direct-git-env-" + UUID().uuidString.lowercased(), isDirectory: true)
    let project = base.appendingPathComponent("proj"), bin = base.appendingPathComponent("bin")
    for d in [project, bin] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
    defer { try? FileManager.default.removeItem(at: base) }
    #expect(sh("git init -q", project.path).status == 0)  // #187: the folder is a repository
    let exe = bin.appendingPathComponent("claude")
    try Data("""
        #!/bin/sh
        [ "$1" = --version ] && { echo '2.1.0 (Claude Code)'; exit 0; }
        cat > /dev/null
        env | grep '^GIT_' | sort > env.txt
        git rev-parse --show-toplevel > harness-git.txt 2>&1 || echo "no repository" >> harness-git.txt
        (. "$CLAUDE_ENV_FILE"; git rev-parse --show-toplevel) > bash-git.txt 2>&1
        echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
        echo '{"type":"result","subtype":"success","usage":{"output_tokens":3}}'

        """.utf8).write(to: exe)
    try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: exe.path)
    var c = RouterConfig()
    c.scratchFolder = base.appendingPathComponent("scratch").path
    c.claudeCLI.enabled = true; c.claudeCLI.executable = exe.path; c.claudeCLI.allowLocalTools = true; c.claudeCLI.allowedRoots = project.path
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    let h = ["Authorization": "Bearer tok", "x-claude-mini-session": UUID().uuidString, "x-claude-mini-cwd": project.path]
    let w = FakeWriter()
    await router.handle(request("POST", "/v1/messages", headers: h, body: #"{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}"#), writer: w)
    #expect(w.status == 200, "\(w.text.prefix(300))")
    let env = (try? String(contentsOf: project.appendingPathComponent("env.txt"), encoding: .utf8)) ?? ""
    #expect(env.contains("GIT_CONFIG_GLOBAL=/dev/null") && env.contains("GIT_CONFIG_NOSYSTEM=1") && env.contains("GIT_CONFIG_KEY_0=core.hooksPath") && env.contains("GIT_CONFIG_VALUE_0=/dev/null"), "\(env)")
    // #187: the harness has no repository (GIT_DIR=/dev/null), its Bash (CLAUDE_ENV_FILE ahead of each command) finds the folder's own
    #expect(env.contains("GIT_DIR=/dev/null\n") && env.contains("GIT_CEILING_DIRECTORIES=" + base.path + "\n"), "\(env)")
    let harness = (try? String(contentsOf: project.appendingPathComponent("harness-git.txt"), encoding: .utf8)) ?? ""
    let bash = (try? String(contentsOf: project.appendingPathComponent("bash-git.txt"), encoding: .utf8)) ?? ""
    #expect(harness.contains("no repository") && bash == project.path + "\n", "harness: \(harness) bash: \(bash)")
    // the sandbox settings: the folder's own `.git` is closed for the shell, except in the session folder of a direct-mode mount whose `.git` is a real directory
    func deny(_ dir: URL, directGit: Bool, extra: [String] = []) throws -> [String] {
        var cfg = ClaudeCLIConfig(); cfg.allowLocalTools = true
        let a = cfg.agentArgs(cwd: dir, allTools: [], folders: AllowedFolders(), checkedPaths: extra, directGit: directGit)
        let st = try #require(try JSONSerialization.jsonObject(with: Data(a[a.firstIndex(of: "--settings")! + 1].utf8)) as? [String: Any])
        return try #require(((st["sandbox"] as? [String: Any])?["filesystem"] as? [String: Any])?["denyWrite"] as? [String])
    }
    let real = base.appendingPathComponent("real"), linked = base.appendingPathComponent("linked"), none = base.appendingPathComponent("none")
    for d in [real.appendingPathComponent(".git"), linked, none] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
    try Data("gitdir: ../real/.git/worktrees/linked\n".utf8).write(to: linked.appendingPathComponent(".git"))
    func closed(_ d: [String], _ dir: URL) -> Bool { d.contains(dir.path + "/**/.git") && !d.contains(dir.path + "/*/**/.git") }
    func opened(_ d: [String], _ dir: URL) -> Bool { d.contains(dir.path + "/*/**/.git") && !d.contains(dir.path + "/**/.git") }
    let dr = try deny(real, directGit: true), dlocal = try deny(real, directGit: false)
    #expect(opened(dr, real))  // the mount's real .git directory
    #expect(closed(dlocal, real))  // a local session folder
    let dn = try deny(none, directGit: true), dl = try deny(linked, directGit: true)
    #expect(closed(dn, none) && closed(dl, linked))  // missing, or a gitdir: file
    let both = try deny(none, directGit: true, extra: [real.path])  // an allowed folder is never opened, even with a real .git
    #expect(closed(both, real) && closed(both, none))
    // what stays closed inside an opened .git
    let o = dr
    for g in ["config", "config.worktree", "hooks", "info", "commondir", "worktrees", "modules", "objects/info/alternates", "shallow", "grafts", "rebase-merge", "rebase-apply", "sequencer", "remotes", "branches", "refs/replace"] {
        #expect(o.contains(real.path + "/.git/" + g), "\(g)")
    }
}

/// B1: a linked worktree (its `.git` is a file) is a local folder, never opened; the real claude cannot point it at a directory of its own. Gated like the test above.
@Test(.enabled(if: ProcessInfo.processInfo.environment["MG_REAL_CLAUDE_GIT"] == "1"), .blocking)
func aClaudeAgentCannotRewriteTheGitFileOfALinkedWorktree() async throws {
    guard let exe = await ClaudeCLI.executable("") else { try Test.cancel("claude is not installed") }
    let base = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("mgw-direct-git-wt-" + UUID().uuidString.lowercased())
    let main = base.appendingPathComponent("main"), wt = base.appendingPathComponent("wt")
    try FileManager.default.createDirectory(at: main, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: base) }
    #expect(sh("git init -q && echo x > a.txt && git add a.txt && git -c user.name=O -c user.email=o@example.com commit -qm init && git worktree add -q '\(wt.path)'", main.path).status == 0)
    let before = try String(contentsOf: wt.appendingPathComponent(".git"), encoding: .utf8)
    var cfg = ClaudeCLIConfig(); cfg.allowLocalTools = true
    for direct in [false, true] {  // a local folder, and (a gitdir: file stays closed) a direct-mode mount
        let args = cfg.agentArgs(cwd: wt, allTools: ["Bash"], folders: AllowedFolders(), checkedPaths: [], directGit: direct)
            + ["-p", "--output-format", "json", "--model", "haiku", "--strict-mcp-config", "--setting-sources", ""]
        var env = ProcessInfo.processInfo.environment
        for k in env.keys where k.hasPrefix("ANTHROPIC_") || k.hasPrefix("CLAUDE_CODE_") || k == "CLAUDECODE" || k.hasPrefix("GIT_") { env[k] = nil }
        env["PATH"] = (env["PATH"] ?? "") + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
        env.merge(ClaudeCLIConfig.sandboxGitEnv(mount: false)) { $1 }
        let r = ClaudeSeatbelt.capture(exe, args, env: env, cwd: wt.path, input: "Run as one simple Bash call: printf 'gitdir: x' > .git   and report the exact output.", timeout: 240, log: base)
        print("[direct-git wt direct=\(direct)] \((r.out.components(separatedBy: "\"result\":").last ?? "").prefix(300))")
        #expect((try? String(contentsOf: wt.appendingPathComponent(".git"), encoding: .utf8)) == before, "the .git file was rewritten (direct=\(direct))")
    }
}

/// 0.7.0: the length of the arguments of claude for a run with ten allowed folders (Mac: ARG_MAX is 1 MiB for arguments and environment together; the rules ride on argv). Pure function:
/// the ceiling is half of it (measured at this commit: about 314,000 characters for ten folders of 120 characters, ~31,000 per folder, mostly the sandbox denyWrite list that exists since 0.6.0), so a growth of the rules (one rule per git entry, tool and case broke the command line on Windows at 0a8e83b) is seen here too.
@Test func theArgumentsOfAClaudeRunWithTenAllowedFoldersStayShort() {
    var cfg = ClaudeCLIConfig(); cfg.allowLocalTools = true
    let long = (0..<10).map { "/Users/someone/Development/projects/client-\($0)/a-long-folder-name-" + String(repeating: "x", count: 60) }
    for allow in [true, false] {
        cfg.allowLocalTools = allow
        let a = cfg.agentArgs(cwd: URL(fileURLWithPath: "/Users/someone/Development/projects/session-" + String(repeating: "s", count: 60)), allTools: ["Bash", "Read", "Edit", "Write"], folders: AllowedFolders(), checkedPaths: long)
        let n = a.reduce(0) { $0 + $1.utf8.count + 1 }
        print("[argv-length] allowLocalTools=\(allow), ten allowed folders of 120 characters: \(n) characters")
        #expect(n < 524_288, "\(n)")
    }
}

/// 0.7.1: the environment of a sandboxed run: git ignores the executable bit only on a direct-mode mount (its files all show as -rwx------); a local folder keeps its real modes.
@Test func theGitEnvironmentIgnoresTheExecutableBitOnlyOnAMount() {
    func pairs(_ e: [String: String]) -> [String: String] {
        Dictionary(uniqueKeysWithValues: (0..<(Int(e["GIT_CONFIG_COUNT"] ?? "0") ?? 0)).map { (e["GIT_CONFIG_KEY_\($0)"] ?? "", e["GIT_CONFIG_VALUE_\($0)"] ?? "") })
    }
    let mount = ClaudeCLIConfig.sandboxGitEnv(mount: true), local = ClaudeCLIConfig.sandboxGitEnv(mount: false)
    #expect(pairs(mount)["core.fileMode"] == "false" && pairs(local)["core.fileMode"] == nil)
    for e in [mount, local] {
        #expect(e["GIT_CONFIG_GLOBAL"] == "/dev/null" && e["GIT_CONFIG_NOSYSTEM"] == "1" && pairs(e)["core.hooksPath"] == "/dev/null" && pairs(e)["core.fsmonitor"] == "false")
        #expect(e.keys.filter { $0.hasPrefix("GIT_CONFIG_KEY_") }.count == Int(e["GIT_CONFIG_COUNT"]!))
    }
}
