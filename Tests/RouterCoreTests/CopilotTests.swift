import CryptoKit
import Foundation
import Testing
@testable import RouterCore

private let forbidden = ["--allow-all-tools", "--allow-all-paths", "--allow-all-urls", "--allow-all", "--yolo", "--sandbox", "--trust-folder"]

/// Pari --allow-tool / --deny-tool iz argumentov.
private func rules(_ a: [String]) -> (allow: [String], deny: [String]) {
    var allow: [String] = [], deny: [String] = []
    for (i, x) in a.enumerated() where i + 1 < a.count {
        if x == "--allow-tool" { allow.append(a[i + 1]) }
        if x == "--deny-tool" { deny.append(a[i + 1]) }
    }
    return (allow, deny)
}

@Test func copilotArgsReadOnlyByDefaultAndNeverAllowAll() {
    let ro = Copilot.args(prompt: "hi", extraDirs: [], agent: true, model: nil, write: false, commands: false, network: false, session: "s1")
    #expect(!ro.contains { forbidden.contains($0) })
    #expect(rules(ro).allow == ["read"] && Set(rules(ro).deny) == ["memory", "write", "shell", "url"])
    for f in ["--disallow-temp-dir", "--disable-builtin-mcps", "--no-ask-user", "--no-auto-update"] { #expect(ro.contains(f)) }
    #expect(ro[ro.firstIndex(of: "--session-id")! + 1] == "s1" && ro[ro.firstIndex(of: "-p")! + 1] == "hi" && !ro.contains("--model"))

    let w = Copilot.args(prompt: "x", extraDirs: ["/wt"], agent: true, model: "gpt-5.2", write: true, commands: false, network: true, session: "s")
    #expect(rules(w).allow == ["read", "write", "url"] && rules(w).deny.contains("shell") && !w.contains { forbidden.contains($0) })
    #expect(w[w.firstIndex(of: "--model")! + 1] == "gpt-5.2" && w[w.firstIndex(of: "--add-dir")! + 1] == "/wt")

    let c = Copilot.args(prompt: "x", extraDirs: [], agent: true, model: nil, write: true, commands: true, network: false, session: "s")
    #expect(rules(c).allow.contains("shell") && rules(c).deny.contains("shell(gh:*)") && rules(c).deny.contains("shell(security:*)"))

    let chat = Copilot.args(prompt: "x", extraDirs: [], agent: false, model: nil, write: true, commands: true, network: true, session: "s")
    #expect(rules(chat).allow.isEmpty && Set(rules(chat).deny) == ["memory", "read", "write", "shell", "url"])  // brez orodij: nič

    let big = Copilot.args(prompt: String(repeating: "a", count: Copilot.maxPrompt + 10) + "END", extraDirs: [], agent: false, model: nil,
                           write: false, commands: false, network: false, session: "s")
    #expect(big[1].count == Copilot.maxPrompt && big[1].hasSuffix("END"))  // ARG_MAX: konec pogovora ostane
}

@Test func copilotModelsEnvAndEvents() throws {
    #expect(Copilot.match("claude-copilot") == .some(nil) && Copilot.match("claude-copilot-gpt-5.2") == .some("gpt-5.2"))
    #expect(Copilot.match("claude-codex") == .none && Copilot.match("claude-copilot-") == .none)
    var c = CopilotConfig()
    c.models = "gpt-5.2, claude-sonnet-4.5, --yolo, bad name, auto"
    #expect(c.modelList == ["gpt-5.2", "claude-sonnet-4.5", "auto"])  // le veljavna imena (nikoli zastavica)
    let old = try JSONDecoder().decode(CopilotConfig.self, from: Data(#"{"enabled":true}"#.utf8))
    #expect(old.enabled && !old.write && !old.commands && !old.network)

    let env = Copilot.environment(["COPILOT_ALLOW_ALL": "true", "COPILOT_ASSISTED_APPROVAL": "1", "ANTHROPIC_BASE_URL": "x",
                                   "CLAUDE_CODE_X": "1", "SSH_AUTH_SOCK": "/s", "COPILOT_HOME": "/home/u/.copilot", "PATH": "/bin"],
                                  home: URL(fileURLWithPath: "/scratch/copilot"), extra: ["TMPDIR": "/t/"])
    #expect(env["COPILOT_ALLOW_ALL"] == nil && env["COPILOT_ASSISTED_APPROVAL"] == nil && env["ANTHROPIC_BASE_URL"] == nil
            && env["CLAUDE_CODE_X"] == nil && env["SSH_AUTH_SOCK"] == nil)
    #expect(env["COPILOT_HOME"] == "/scratch/copilot" && env["PATH"] == "/bin" && env["TMPDIR"] == "/t/" && env["COPILOT_AUTO_UPDATE"] == "false")

    #expect(Copilot.event(Data(#"{"type":"assistant.message","data":{"content":"Hi!","toolRequests":[]}}"#.utf8)).text == "Hi!")
    #expect(Copilot.event(Data(#"{"type":"assistant.message","data":{"content":"","toolRequests":[{}]}}"#.utf8)).text == nil)
    #expect(Copilot.event(Data(#"{"type":"tool.execution_start","data":{"toolName":"view"}}"#.utf8)).step == "view")
    #expect(Copilot.event(Data("not json".utf8)).text == nil)
}

/// Lažen copilot: zapiše argumente in okolje, izpiše dogodke JSONL; odgovor = zadnje sporočilo agenta.
private func fakeCopilot(_ dir: URL) throws -> URL {
    let exe = dir.appendingPathComponent("copilot")
    try Data("""
    #!/bin/sh
    for a in "$@"; do printf '%s\\n' "$a"; done > "\(dir.path)/args"
    env > "\(dir.path)/env"
    pwd > "\(dir.path)/cwd"
    echo '{"type":"session.start","data":{"sessionId":"x"}}'
    echo '{"type":"tool.execution_start","data":{"toolName":"view"}}'
    echo '{"type":"assistant.message","data":{"content":"first","toolRequests":[{}]}}'
    echo '{"type":"assistant.message","data":{"content":"FAKE ANSWER","toolRequests":[]}}'
    """.utf8).write(to: exe)
    chmod(exe.path, 0o755)
    return exe
}

@Test func copilotRunsThroughTheRouterWithFakeBinary() async throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("copilot-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let proj = root.appendingPathComponent("proj"), bin = root.appendingPathComponent("bin")
    for d in [proj, bin] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
    var c = RouterConfig()
    c.copilot.enabled = true; c.copilot.executable = try fakeCopilot(bin).path; c.copilot.models = "gpt-5.2"
    c.claudeCLI.allowedRoots = proj.path
    c.scratchFolder = root.appendingPathComponent("scratch").path
    let r = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    let models = await r.modelList()
    let ids = (models["data"] as? [[String: Any]] ?? []).compactMap { $0["id"] as? String }
    #expect(ids.contains("claude-copilot") && ids.contains("claude-copilot-gpt-5.2"))

    func ask(_ model: String, session: String) async -> FakeWriter {
        var q = request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok", "x-claude-mini-session": session, "x-claude-mini-cwd": proj.path],
                        body: #"{"model":"\#(model)","max_tokens":5,"tools":[{"name":"x","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"read a.txt"}]}"#)
        q.peer = "127.0.0.1"
        let w = FakeWriter()
        await r.handle(q, writer: w)
        return w
    }
    let w = await ask("claude-copilot-gpt-5.2", session: UUID().uuidString)
    #expect(w.status == 200 && w.text.contains("FAKE ANSWER") && !w.text.contains("first"))
    let args = try String(contentsOf: bin.appendingPathComponent("args"), encoding: .utf8).split(separator: "\n").map(String.init)
    #expect(!args.contains { forbidden.contains($0) } && rules(args).deny.contains("write") && rules(args).deny.contains("shell"))
    #expect(args[args.firstIndex(of: "--model")! + 1] == "gpt-5.2")
    #expect(try String(contentsOf: bin.appendingPathComponent("cwd"), encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines) == proj.path)
    let env = try String(contentsOf: bin.appendingPathComponent("env"), encoding: .utf8)
    #expect(env.contains("COPILOT_HOME=" + (Sync.real(c.scratch("copilot"))?.path ?? "")) || env.contains("COPILOT_HOME=" + c.scratch("copilot").path))
    #expect(!env.contains("COPILOT_ALLOW_ALL"))
    #expect(await ask("claude-copilot-bad name", session: UUID().uuidString).status == 400)
}

@Test func copilotWorktreeNeedsWritingAndAdminAPI() async throws {
    var c = RouterConfig()
    c.copilot.enabled = true; c.copilot.write = false
    let r = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    let session = UUID().uuidString
    _ = await Router.shares.register(session: session, cwd: "/p", key: nil, device: nil,
                                     tree: ShareHub.Tree(repo: URL(fileURLWithPath: "/nonexistent/x.git"), worktrees: URL(fileURLWithPath: "/nonexistent/w"), sub: ""))
    var q = request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok", "x-claude-mini-session": session, "x-claude-mini-cwd": "/p"],
                    body: #"{"model":"claude-copilot","max_tokens":5,"tools":[{"name":"x","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}"#)
    q.peer = "127.0.0.1"
    let w = FakeWriter()
    await r.handle(q, writer: w)
    #expect(w.status == 400 && w.text.contains("Copilot may not write"))

    let app = FakeApp()
    #expect(await admin(app.router(), "GET", "/agents/copilot").text.contains(#""commands":false"#))
    #expect(await admin(app.router(), "PATCH", "/agents/copilot", body: #"{"enabled":true,"write":true,"models":["gpt-5.2"]}"#).status == 200)
    #expect(app.config.copilot.enabled && app.config.copilot.write && app.config.copilot.models == "gpt-5.2")
    #expect(await admin(app.router(), "PATCH", "/agents/copilot", body: #"{"models":["--yolo"]}"#).status == 400)
    #expect(await admin(app.router(), "PATCH", "/agents/copilot", body: #"{"allowAll":true}"#).status == 400)
}

/// Ukazi lupine (z dovoljenjem): ves copilot v sandbox-exec: piše v mapo seje in COPILOT_HOME, ne v domačo mapo.
@Test func copilotCommandsRunInTheSandbox() async throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("copilot-sb-" + UUID().uuidString)
    let outside = URL(fileURLWithPath: NSHomeDirectory()).appendingPathComponent(".minigw-copilot-test-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(at: outside) }
    let proj = root.appendingPathComponent("proj"), bin = root.appendingPathComponent("bin")
    for d in [proj, bin] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
    let exe = bin.appendingPathComponent("copilot")
    try Data("""
    #!/bin/sh
    echo in > inside.txt
    echo home > "$COPILOT_HOME/state.txt"
    echo out > "\(outside.path)"
    echo '{"type":"assistant.message","data":{"content":"done","toolRequests":[]}}'
    """.utf8).write(to: exe)
    chmod(exe.path, 0o755)
    var c = RouterConfig()
    c.copilot.enabled = true; c.copilot.executable = exe.path; c.copilot.write = true; c.copilot.commands = true
    c.claudeCLI.allowedRoots = proj.path
    c.scratchFolder = root.appendingPathComponent("scratch").path
    let r = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    var q = request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok", "x-claude-mini-session": UUID().uuidString, "x-claude-mini-cwd": proj.path],
                    body: #"{"model":"claude-copilot","max_tokens":5,"tools":[{"name":"x","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"go"}]}"#)
    q.peer = "127.0.0.1"
    let w = FakeWriter()
    await r.handle(q, writer: w)
    #expect(w.status == 200 && w.text.contains("done"))
    #expect(FileManager.default.fileExists(atPath: proj.appendingPathComponent("inside.txt").path))
    #expect(FileManager.default.fileExists(atPath: c.scratch("copilot").appendingPathComponent("state.txt").path))
    #expect(!FileManager.default.fileExists(atPath: outside.path))  // domača mapa: ne
}
