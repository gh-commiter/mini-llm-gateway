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

@Test(.enabled(if: ProcessInfo.processInfo.environment["MG_REAL_CLAUDE_GIT"] == "1"), arguments: [true, false])
func aClaudeAgentCommitsInADirectModeFolderAndCannotPlantAHook(identity: Bool) async throws {
    guard let exe = await ClaudeCLI.executable("") else { try Test.cancel("claude is not installed") }
    let base = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("mgw-direct-git-real-" + UUID().uuidString.lowercased())
    let repo = base.appendingPathComponent("repo")
    try FileManager.default.createDirectory(at: repo, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: base) }
    #expect(sh("git init -q && echo x > a.txt && git add a.txt && git -c user.name=Owner -c user.email=o@example.com commit -qm init" + (identity ? " && git config user.name Owner && git config user.email o@example.com" : ""), repo.path).status == 0)
    var cfg = ClaudeCLIConfig(); cfg.allowLocalTools = true
    let args = cfg.agentArgs(cwd: repo, allTools: ["Bash"], folders: AllowedFolders(), checkedPaths: [])
        + ["-p", "--output-format", "json", "--model", "haiku", "--strict-mcp-config", "--setting-sources", ""]
    var env = ProcessInfo.processInfo.environment
    for k in env.keys where k.hasPrefix("ANTHROPIC_") || k.hasPrefix("CLAUDE_CODE_") || k == "CLAUDECODE" || k.hasPrefix("GIT_") { env[k] = nil }
    env["PATH"] = (env["PATH"] ?? "") + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
    env.merge(ClaudeCLIConfig.sandboxGitEnv) { $1 }
    let ask = "Run these as separate Bash calls, one simple command each (no chaining, no echo, no redirection of exit codes): 1) printf hi > b.txt  2) git add b.txt  3) git commit -m 'add b'  4) printf x > .git/hooks/pre-commit. Report the exact output of each."
    let r = ClaudeSeatbelt.capture(exe, args, env: env, cwd: repo.path, input: ask, timeout: 240, log: base)
    print("[direct-git identity=\(identity)] author: \(sh("git log -1 --format=%an,%ae", repo.path).out); claude said: \((r.out.components(separatedBy: "\"result\":").last ?? "").prefix(900))")
    let log = sh("git log --format=%s", repo.path).out
    #expect(log.hasPrefix("add b"), "no commit: git log: \(log); claude: \(r.out.prefix(600)) \(r.err.prefix(200))")
    #expect(!FileManager.default.fileExists(atPath: repo.appendingPathComponent(".git/hooks/pre-commit").path), "a hook was planted")
}

/// The launch of a sandboxed agent run (fake claude): git gets no user and system configuration (the sandbox hides ~/.gitconfig, which git treats as fatal) and no
/// inherited GIT_* variable; the sandbox's denyWrite closes nested `.git` entries but not the folder's own `.git`, which a commit writes (a real run: index.lock refused).
@Test func aSandboxedAgentRunGetsASafeGitEnvironment() async throws {
    let base = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("mgw-direct-git-env-" + UUID().uuidString.lowercased(), isDirectory: true)
    let project = base.appendingPathComponent("proj"), bin = base.appendingPathComponent("bin")
    for d in [project, bin] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
    defer { try? FileManager.default.removeItem(at: base) }
    let exe = bin.appendingPathComponent("claude")
    try Data("""
        #!/bin/sh
        [ "$1" = --version ] && { echo '2.1.0 (Claude Code)'; exit 0; }
        cat > /dev/null
        env | grep '^GIT_' | sort > env.txt
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
    // the sandbox settings
    var cfg = ClaudeCLIConfig(); cfg.allowLocalTools = true
    let a = cfg.agentArgs(cwd: project, allTools: [], folders: AllowedFolders(), checkedPaths: [])
    let st = try #require(try JSONSerialization.jsonObject(with: Data(a[a.firstIndex(of: "--settings")! + 1].utf8)) as? [String: Any])
    let deny = try #require(((st["sandbox"] as? [String: Any])?["filesystem"] as? [String: Any])?["denyWrite"] as? [String])
    #expect(deny.contains(project.path + "/*/**/.git") && !deny.contains(project.path + "/**/.git") && !deny.contains(project.path + "/.git"))
}
