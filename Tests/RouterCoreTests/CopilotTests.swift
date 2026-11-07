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
    let ro = Copilot.args(agent: true, model: nil, write: false, commands: false, network: false, session: "s1")
    #expect(!ro.contains { forbidden.contains($0) || $0 == "--add-dir" })  // M3: koren klona je delovna mapa
    #expect(rules(ro).allow == ["read"] && Set(rules(ro).deny) == ["memory", "write", "shell", "url"])
    for f in ["--disallow-temp-dir", "--disable-builtin-mcps", "--no-ask-user", "--no-auto-update", "--disable-client-telemetry",
              "--secret-env-vars=COPILOT_GITHUB_TOKEN,GH_TOKEN,GITHUB_TOKEN"] { #expect(ro.contains(f)) }
    #expect(ro[ro.firstIndex(of: "--session-id")! + 1] == "s1" && !ro.contains("-p") && !ro.contains("--model"))  // navodilo na stdin

    let w = Copilot.args(agent: true, model: "gpt-5.2", write: true, commands: false, network: true, session: "s")
    #expect(rules(w).allow == ["read", "write", "url"] && rules(w).deny.contains("shell") && !w.contains { forbidden.contains($0) })
    #expect(w[w.firstIndex(of: "--model")! + 1] == "gpt-5.2")

    let c = Copilot.args(agent: true, model: nil, write: true, commands: true, network: false, session: "s")
    #expect(rules(c).allow.contains("shell") && rules(c).deny.contains("shell(gh:*)") && rules(c).deny.contains("shell(security:*)"))
    let cro = Copilot.args(agent: true, model: nil, write: false, commands: true, network: false, session: "s")
    #expect(rules(cro).allow == ["read"] && rules(cro).deny.contains("shell"))  // ukazi brez pisanja (ali --ro): ne

    let chat = Copilot.args(agent: false, model: nil, write: true, commands: true, network: true, session: "s")
    #expect(rules(chat).allow.isEmpty && Set(rules(chat).deny) == ["memory", "read", "write", "shell", "url"])  // brez orodij: nič
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
                                   "COPILOT_PROVIDER_API_KEY_COMMAND": "touch /tmp/x", "COPILOT_CLI_DIST_DIR": "/evil",
                                   "COPILOT_CUSTOM_INSTRUCTIONS_DIRS": "/evil", "GH_TOKEN": "user", "GITHUB_TOKEN": "user",
                                   "CLAUDE_CODE_X": "1", "SSH_AUTH_SOCK": "/s", "COPILOT_HOME": "/home/u/.copilot", "PATH": "/bin"],
                                  home: URL(fileURLWithPath: "/req/home"), token: "tok", extra: ["TMPDIR": "/t/"])
    #expect(env.keys.filter { $0.hasPrefix("COPILOT_") }.sorted() == ["COPILOT_AUTO_UPDATE", "COPILOT_GITHUB_TOKEN", "COPILOT_HOME"])
    #expect(env["GH_TOKEN"] == nil && env["GITHUB_TOKEN"] == nil && env["ANTHROPIC_BASE_URL"] == nil && env["CLAUDE_CODE_X"] == nil
            && env["SSH_AUTH_SOCK"] == nil)
    #expect(env["COPILOT_HOME"] == "/req/home" && env["COPILOT_GITHUB_TOKEN"] == "tok" && env["PATH"] == "/bin" && env["TMPDIR"] == "/t/"
            && env["COPILOT_AUTO_UPDATE"] == "false")
    #expect(Copilot.validToken("github_pat_11ABC_x") && !Copilot.validToken("gho_abc") && !Copilot.validToken("github_pat_a b"))

    #expect(Copilot.event(Data(#"{"type":"assistant.message","data":{"content":"Hi!","toolRequests":[]}}"#.utf8)).text == "Hi!")
    #expect(Copilot.event(Data(#"{"type":"assistant.message","data":{"content":"","toolRequests":[{}]}}"#.utf8)).text == nil)
    #expect(Copilot.event(Data(#"{"type":"tool.execution_start","data":{"toolName":"view"}}"#.utf8)).step == "view")
    #expect(Copilot.event(Data("not json".utf8)).text == nil)
}

/// Lažen copilot (v peskovniku: zapisovati sme le v COPILOT_HOME zahteve): v odgovoru pove argumente, mapo, okolje in
/// stanje seje; šteje zavoje v session-state/<id>, podtakne povezavo in hooks.json (ne smeta ostati za naslednji zavoj).
private func fakeCopilot(_ dir: URL, _ extra: String = "") throws -> URL {
    let exe = dir.appendingPathComponent("copilot")
    try Data("""
    #!/bin/sh
    id=""; prev=""; for a in "$@"; do [ "$prev" = --session-id ] && id="$a"; prev="$a"; done
    st="$COPILOT_HOME/session-state/$id"; mkdir -p "$st"
    n=$(cat "$st/count" 2>/dev/null || echo 0); echo $((n + 1)) > "$st/count"
    hooks=no; [ -e "$COPILOT_HOME/hooks.json" ] && hooks=yes
    link=no; [ -L "$st/link" ] && link=yes
    echo '{}' > "$COPILOT_HOME/hooks.json"; ln -s /etc "$st/link"
    stdin=$(cat | grep -c 'read a.txt')
    \(extra)
    echo '{"type":"tool.execution_start","data":{"toolName":"view"}}'
    echo '{"type":"assistant.message","data":{"content":"first","toolRequests":[{}]}}'
    printf '{"type":"assistant.message","data":{"content":"ARGS=%s CWD=%s HOME=%s TOKEN=%s STDIN=%s TURN=%s HOOKS=%s LINK=%s ALLOW=%s"}}\\n' \\
      "$(echo "$@" | tr ' ' '|')" "$(pwd)" "$COPILOT_HOME" "$COPILOT_GITHUB_TOKEN" "$stdin" "$((n + 1))" "$hooks" "$link" "${COPILOT_ALLOW_ALL:-}"
    """.utf8).write(to: exe)
    chmod(exe.path, 0o755)
    return exe
}

private func field(_ text: String, _ name: String) -> String {
    guard let r = text.range(of: name + "=") else { return "" }
    return String(text[r.upperBound...].prefix { $0 != " " && $0 != "\"" })
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
    let r = Router(config: c, token: "tok", secrets: FakeSecrets(values: [Copilot.tokenAccount: "github_pat_test"]))
    let models = await r.modelList()
    let ids = (models["data"] as? [[String: Any]] ?? []).compactMap { $0["id"] as? String }
    #expect(ids.contains("claude-copilot") && ids.contains("claude-copilot-gpt-5.2"))

    func ask(_ model: String, session: String, history: String = "") async -> FakeWriter {
        var q = request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok", "x-claude-mini-session": session, "x-claude-mini-cwd": proj.path],
                        body: #"{"model":"\#(model)","max_tokens":5,"tools":[{"name":"x","input_schema":{"type":"object"}}],"messages":[\#(history){"role":"user","content":"read a.txt"}]}"#)
        q.peer = "127.0.0.1"
        let w = FakeWriter()
        await r.handle(q, writer: w)
        return w
    }
    let w = await ask("claude-copilot-gpt-5.2", session: UUID().uuidString)
    #expect(w.status == 200 && w.text.contains("ARGS=") && !w.text.contains("first"))
    let args = field(w.text, "ARGS").split(separator: "|").map(String.init)
    #expect(!args.contains { forbidden.contains($0) } && rules(args).deny.contains("write") && rules(args).deny.contains("shell"))
    #expect(args[args.firstIndex(of: "--model")! + 1] == "gpt-5.2")
    #expect(field(w.text, "STDIN") == "1" && !args.contains { $0.contains("read a.txt") })  // navodilo na stdin, ne v argumentih
    #expect(field(w.text, "CWD") == proj.path && field(w.text, "TOKEN") == "github_pat_test" && field(w.text, "ALLOW").isEmpty)
    let home = field(w.text, "HOME")
    #expect(home.hasPrefix("/private/tmp/minigw-copilot-") && !FileManager.default.fileExists(atPath: home))  // svež, po zagonu izbrisan
    #expect(field(w.text, "TURN") == "1" && field(w.text, "HOOKS") == "no")

    // Naslednji zavoj iste seje: stanje seje ostane (brez povezave), podtaknjen hooks.json ne (C2).
    let answer = w.text.components(separatedBy: "\"text\":\"").dropFirst().first?.components(separatedBy: "\"").first ?? ""
    let history = #"{"role":"user","content":"read a.txt"},{"role":"assistant","content":"\#(answer)"},"#
    let w2 = await ask("claude-copilot-gpt-5.2", session: UUID().uuidString, history: history)
    #expect(w2.status == 200 && field(w2.text, "TURN") == "2" && field(w2.text, "HOOKS") == "no" && field(w2.text, "LINK") == "no")
    #expect(field(w2.text, "HOME") != home)

    #expect(await ask("claude-copilot-bad name", session: UUID().uuidString).status == 404)
    #expect(await ask("claude-copilot-unlisted", session: UUID().uuidString).status == 404)  // L1
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
    // Žeton: le fine-grained PAT, le za pisanje (GET pove le hasToken).
    #expect(await admin(app.router(), "PATCH", "/agents/copilot", body: #"{"token":"gho_classic"}"#).status == 400)
    let set = await admin(app.router(), "PATCH", "/agents/copilot", body: #"{"token":"github_pat_secret1"}"#)
    #expect(set.status == 200 && set.text.contains(#""hasToken":true"#) && app.keys[Copilot.tokenAccount] == "github_pat_secret1")
    let got = await admin(app.router(), "GET", "/agents/copilot")
    #expect(got.text.contains(#""hasToken":true"#) && !got.text.contains("github_pat_secret1"))
    #expect(await admin(app.router(), "PATCH", "/agents/copilot", body: #"{"token":""}"#).status == 200 && app.keys[Copilot.tokenAccount] == nil)
}

/// Ukazi lupine (z dovoljenjem): le z namenskim žetonom; pišejo v mapo seje in COPILOT_HOME zahteve, ne v domačo mapo.
/// Brez ukazov in brez žetona: prijava GitHub CLI (lažni gh, nikoli pravi).
@Test func copilotCommandsRunInTheSandbox() async throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("copilot-sb-" + UUID().uuidString)
    let outside = URL(fileURLWithPath: NSHomeDirectory()).appendingPathComponent(".minigw-copilot-test-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(at: outside) }
    let proj = root.appendingPathComponent("proj"), bin = root.appendingPathComponent("bin")
    for d in [proj, bin] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
    let exe = try fakeCopilot(bin, """
    echo in > inside.txt; inside=$?
    echo out > "\(outside.path)" 2>/dev/null; out=$?
    mkdir -p "$HOME/Library/Caches/copilot/pkg-minigw-test" 2>/dev/null; cache=$?
    echo '{"type":"assistant.message","data":{"content":"WROTE='$inside$out$cache' TOKEN='$COPILOT_GITHUB_TOKEN'"}}'
    exit 0
    """)
    let gh = bin.appendingPathComponent("gh")
    try Data("#!/bin/sh\n[ \"$*\" = 'auth token --hostname github.com' ] && echo gho_Test123\n".utf8).write(to: gh)
    chmod(gh.path, 0o755)
    let realGh = Copilot.gh
    Copilot.gh = gh.path
    defer { Copilot.gh = realGh }
    #expect(Copilot.ghToken() == "gho_Test123")
    var c = RouterConfig()
    c.copilot.enabled = true; c.copilot.executable = exe.path; c.copilot.write = true; c.copilot.commands = true
    c.claudeCLI.allowedRoots = proj.path
    c.scratchFolder = root.appendingPathComponent("scratch").path
    func ask(_ r: Router) async -> FakeWriter {
        var q = request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok", "x-claude-mini-session": UUID().uuidString, "x-claude-mini-cwd": proj.path],
                        body: #"{"model":"claude-copilot","max_tokens":5,"tools":[{"name":"x","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"go"}]}"#)
        q.peer = "127.0.0.1"
        let w = FakeWriter()
        await r.handle(q, writer: w)
        return w
    }
    // H1: ukazi brez namenskega žetona: zavrnjeno (žeton GitHub CLI ima vse pravice uporabnika).
    let refused = await ask(Router(config: c, token: "tok", secrets: FakeSecrets(values: [:])))
    #expect(refused.status == 503 && refused.text.contains("Copilot Requests"))
    let w = await ask(Router(config: c, token: "tok", secrets: FakeSecrets(values: [Copilot.tokenAccount: "github_pat_cmd"])))
    #expect(w.status == 200 && w.text.contains("WROTE=011") && w.text.contains("TOKEN=github_pat_cmd"))  // mapa da; domača mapa, ~/Library/Caches ne
    #expect(FileManager.default.fileExists(atPath: proj.appendingPathComponent("inside.txt").path) && !FileManager.default.fileExists(atPath: outside.path))
    // Brez ukazov: prijava GitHub CLI (v peskovniku je copilot ne najde sam).
    if Copilot.tokenVars.allSatisfy({ (ProcessInfo.processInfo.environment[$0] ?? "").isEmpty }) {
        c.copilot.commands = false
        let g = await ask(Router(config: c, token: "tok", secrets: FakeSecrets(values: [:])))
        #expect(g.status == 200 && g.text.contains("TOKEN=gho_Test123"))
    }
}
