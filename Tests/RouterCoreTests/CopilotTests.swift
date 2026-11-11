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
    let ro = Copilot.args(agent: true, model: nil, write: false, network: false, session: "s1")
    #expect(!ro.contains { forbidden.contains($0) || $0 == "--add-dir" })  // M3: koren klona je delovna mapa
    #expect(rules(ro).allow == ["read"] && Set(rules(ro).deny) == ["memory", "write", "shell", "shell(gh:*)", "shell(security:*)", "url"])
    for f in ["--disallow-temp-dir", "--disable-builtin-mcps", "--no-ask-user", "--no-auto-update", "--disable-client-telemetry",
              "--secret-env-vars=COPILOT_GITHUB_TOKEN,GH_TOKEN,GITHUB_TOKEN"] { #expect(ro.contains(f)) }
    #expect(ro[ro.firstIndex(of: "--session-id")! + 1] == "s1" && !ro.contains("-p") && !ro.contains("--model"))  // navodilo na stdin

    let w = Copilot.args(agent: true, model: "gpt-5.2", write: true, network: true, session: "s")  // #81 H1: lupine nikoli
    #expect(rules(w).allow == ["read", "write", "url"] && rules(w).deny.contains("shell") && !w.contains { forbidden.contains($0) })
    #expect(w[w.firstIndex(of: "--model")! + 1] == "gpt-5.2")

    let chat = Copilot.args(agent: false, model: nil, write: true, network: true, session: "s")
    #expect(rules(chat).allow.isEmpty && Set(rules(chat).deny) == ["memory", "read", "write", "shell", "shell(gh:*)", "shell(security:*)", "url"])  // brez orodij: nič
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
                                   "CLAUDE_CODE_X": "1", "SSH_AUTH_SOCK": "/s", "COPILOT_HOME": "/home/u/.copilot", "PATH": "/bin",
                                   "GITHUB_COPILOT_PROMPT_MODE_REPO_HOOKS": "true", "GITHUB_COPILOT_PROMPT_MODE_WORKSPACE_MCP": "true",
                                   "COPILOT_GITHUB_TOKEN": "user", "COPILOT_HMAC_KEY": "k", "COPILOT_PROVIDERS_CONFIG": "/evil"],
                                  home: URL(fileURLWithPath: "/req/home"), extra: ["TMPDIR": "/t/"])
    #expect(env.keys.filter { $0.hasPrefix("COPILOT_") }.sorted() == ["COPILOT_AUTO_UPDATE", "COPILOT_HOME"])  // #81: brez žetona
    #expect(!env.keys.contains { $0.hasPrefix("GITHUB_COPILOT_") })  // M1: hooki in MCP repozitorija v načinu -p
    #expect(env["GH_TOKEN"] == nil && env["GITHUB_TOKEN"] == nil && env["ANTHROPIC_BASE_URL"] == nil && env["CLAUDE_CODE_X"] == nil
            && env["SSH_AUTH_SOCK"] == nil)
    #expect(env["COPILOT_HOME"] == "/req/home" && env["PATH"] == "/bin" && env["TMPDIR"] == "/t/" && env["COPILOT_AUTO_UPDATE"] == "false")

    #expect(Copilot.event(Data(#"{"type":"assistant.message","data":{"content":"Hi!","toolRequests":[]}}"#.utf8)).text == "Hi!")
    #expect(Copilot.event(Data(#"{"type":"assistant.message","data":{"content":"","toolRequests":[{}]}}"#.utf8)).text == nil)
    #expect(Copilot.event(Data(#"{"type":"tool.execution_start","data":{"toolName":"view"}}"#.utf8)).step == "view")
    #expect(Copilot.event(Data("not json".utf8)).text == nil)
}

/// #81: začasna ~/.copilot/config.json v obliki copilota (vrstice »//«, ključi, ki se ne smejo kopirati); nikoli prava prijava.
private func copilotLogin(_ root: URL) throws -> URL {
    let f = root.appendingPathComponent("copilot-home/config.json")
    try FileManager.default.createDirectory(at: f.deletingLastPathComponent(), withIntermediateDirectories: true)
    try Data("""
    // This file is managed automatically.
    // User settings belong in settings.json.
    {"loggedInUsers": [{"host": "https://github.com", "login": "octo-cat"}, {"host": "corp.ghe.com", "login": "octo_corp", "kind": "user"}],
     "lastLoggedInUser": {"host": "https://github.com", "login": "octo-cat"},
     "trustedFolders": ["/"], "installedPlugins": [{"name": "evil", "cachePath": "/tmp/evil"}], "storeTokenPlaintext": false}
    """.utf8).write(to: f)
    return f
}

private let copilotPointer = #"{"lastLoggedInUser":{"host":"https:\/\/github.com","login":"octo-cat"},"#
    + #""loggedInUsers":[{"host":"https:\/\/github.com","login":"octo-cat"},{"host":"corp.ghe.com","kind":"user","login":"octo_corp"}]}"#

/// #81: le loggedInUsers in lastLoggedInUser, preverjena oblika; žeton v navadnem besedilu ali tuja oblika = razlog (kot Go).
@Test func copilotLoginPointerWhitelist() throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("copilot-login-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let f = try copilotLogin(root)
    let ok = Copilot.login(f)
    #expect(ok.problem == nil && ok.config.map { String(decoding: $0, as: UTF8.self) } == copilotPointer)
    let user = #"{"host":"github.com","login":"u"}"#
    let cases: [(String, String?)] = [
        (#"{"loggedInUsers":[\#(user)]}"#, nil),
        (#"{"loggedInUsers":[\#(user)],"authTokens":{"github.com:u":"gho_x"}}"#, "plain text"),
        (#"{"loggedInUsers":[\#(user)],"copilotTokens":{"x":"y"}}"#, "plain text"),
        (#"{"loggedInUsers":[]}"#, "not signed in"),
        (#"{"trustedFolders":["/"]}"#, "not signed in"),
        ("not json", "expected form"),
        (#"{"loggedInUsers":[{"host":"github.com","login":"u","token":"x"}]}"#, "expected form"),
        (#"{"loggedInUsers":[{"host":"github.com/../x","login":"u"}]}"#, "expected form"),
        (#"{"loggedInUsers":[{"host":"file:///etc","login":"u"}]}"#, "expected form"),
        (#"{"loggedInUsers":[{"host":"github.com","login":"-u"}]}"#, "expected form"),
        (#"{"loggedInUsers":[{"host":"github.com","login":"u;rm"}]}"#, "expected form"),
        (#"{"loggedInUsers":[{"host":"github.com"}]}"#, "expected form"),
        (#"{"loggedInUsers":[{"host":"github.com","login":"u","kind":7}]}"#, "expected form"),
        (#"{"loggedInUsers":[{"host":"github.com","login":"u","kind":"User"}]}"#, "expected form"),  // L4: le [a-z][a-z0-9-]
        (#"{"loggedInUsers":[{"host":"github.com","login":"u","derivedFrom":"github.com:u"}]}"#, "expected form"),
        (#"{"loggedInUsers":[{"host":"github.com","login":"u","kind":"enterprise-user","derivedFrom":"oauth"}]}"#, nil),
        (#"{"loggedInUsers":[{"host":"github.com","login":"u","kind":"a\nb"}]}"#, "expected form"),
        (#"{"loggedInUsers":[\#(user)],"lastLoggedInUser":{"host":"","login":"u"}}"#, "expected form"),
    ]
    for (body, want) in cases {
        try Data(body.utf8).write(to: f)
        let r = Copilot.login(f)
        #expect(want == nil ? r.problem == nil && r.config != nil : r.problem?.contains(want!) == true && r.config == nil, "\(body): \(r.problem ?? "")")
    }
    try FileManager.default.removeItem(at: f)
    try Data(#"{"loggedInUsers":[\#(user)]}"#.utf8).write(to: root.appendingPathComponent("elsewhere.json"))
    try FileManager.default.createSymbolicLink(at: f, withDestinationURL: root.appendingPathComponent("elsewhere.json"))
    #expect(Copilot.login(f).problem?.contains("not signed in") == true)  // povezava: ne sledi
    #expect(Copilot.login(root.appendingPathComponent("none.json")).problem?.contains("copilot login") == true)
}

/// #81: peskovnik Copilota sme brati ~/Library/Keychains (prijava v Keychainu), ne pisati vanj; drugo ostane skrito.
/// Bere ga le drevo procesov copilota (H1: Copilot nima ukazov).
/// Začasna domača mapa, nikoli pravi Keychain.
@Test func copilotSandboxReadsOnlyTheKeychainFolder() throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("copilot-kc-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let home = root.appendingPathComponent("home"), proj = root.appendingPathComponent("proj")
    let kc = home.appendingPathComponent("Library/Keychains"), ssh = home.appendingPathComponent(".ssh")
    for d in [kc, ssh, proj] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
    try Data("kc".utf8).write(to: kc.appendingPathComponent("login.keychain-db"))
    try Data("key".utf8).write(to: ssh.appendingPathComponent("id"))
    func run(_ profile: String) throws -> String {
        let p = Process(), out = Pipe()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/sandbox-exec")
        p.arguments = ["-p", profile, "/bin/sh", "-c",
                       #"cat "$1/login.keychain-db"; cat "$2/id"; echo w > "$1/new" && echo W; rm -f "$1/login.keychain-db" && echo R; true"#, "sh", kc.path, ssh.path]
        p.standardOutput = out; p.standardError = FileHandle.nullDevice
        try p.run(); p.waitUntilExit()
        return String(decoding: out.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
    }
    let base = try #require(Native.sandboxProfile(dir: proj.path, home: home.path, network: false, writable: [proj.path]))
    #expect(Copilot.keychainRule(home: home.path).contains("(subpath \"\(kc.path)\")"))
    #expect(try run(base + Copilot.keychainRule(home: home.path)) == "kc")  // ne .ssh, ne pisanje, ne brisanje
    #expect(try run(base) == "")  // brez pravila Keychain skrit
    #expect(FileManager.default.fileExists(atPath: kc.appendingPathComponent("login.keychain-db").path))
}

/// Lažen copilot (v peskovniku: zapisovati sme le v COPILOT_HOME zahteve): v odgovoru pove argumente, mapo, okolje in
/// stanje seje; šteje zavoje v session-state/<id>/events.jsonl, podtakne povezavo, checkpoints in hooks.json (ne smejo
/// ostati za naslednji zavoj). `--version` (priprava paketa, brez peskovnika) zapiše, ali je imel žeton.
private func fakeCopilot(_ dir: URL, _ extra: String = "") throws -> URL {
    let exe = dir.appendingPathComponent("copilot")
    try Data("""
    #!/bin/sh
    if [ "$1" = --version ]; then echo "token=${COPILOT_GITHUB_TOKEN:-none}" >> "\(dir.path)/version.log"; exit 0; fi
    id=""; prev=""; for a in "$@"; do [ "$prev" = --session-id ] && id="$a"; prev="$a"; done
    st="$COPILOT_HOME/session-state/$id"; mkdir -p "$st"
    n=$(cat "$st/events.jsonl" 2>/dev/null || echo 0); echo $((n + 1)) > "$st/events.jsonl"
    hooks=no; [ -e "$COPILOT_HOME/hooks.json" ] && hooks=yes
    link=no; [ -L "$st/link" ] && link=yes
    [ -e "$st/checkpoints" ] && hooks=checkpoints
    echo '{}' > "$COPILOT_HOME/hooks.json"; ln -s /etc "$st/link"; mkdir -p "$st/checkpoints"
    stdin=$(cat | grep -c 'read a.txt')
    \(extra)
    echo '{"type":"tool.execution_start","data":{"toolName":"view"}}'
    echo '{"type":"assistant.message","data":{"content":"first","toolRequests":[{}]}}'
    printf '{"type":"assistant.message","data":{"content":"ARGS=%s;CWD=%s;HOME=%s;TOKEN=%s;STDIN=%s;TURN=%s;HOOKS=%s;LINK=%s;ALLOW=%s;CONFIG=%s;FSMON=%s;"}}\\n' \\
      "$(echo "$@" | tr ' ' '|')" "$(pwd)" "$COPILOT_HOME" "$COPILOT_GITHUB_TOKEN$GH_TOKEN$GITHUB_TOKEN" "$stdin" "$((n + 1))" "$hooks" "$link" "${COPILOT_ALLOW_ALL:-}" \\
      "$(/usr/bin/base64 < "$COPILOT_HOME/config.json" | tr -d '\\n')" "$GIT_CONFIG_KEY_0"
    """.utf8).write(to: exe)
    chmod(exe.path, 0o755)
    return exe
}

private func field(_ text: String, _ name: String) -> String {
    guard let r = text.range(of: name + "=") else { return "" }
    return String(text[r.upperBound...].prefix { $0 != ";" && $0 != "\"" })
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
    let r = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]), copilotConfig: try copilotLogin(root))
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
    #expect(field(w.text, "CWD") == proj.path && field(w.text, "TOKEN").isEmpty && field(w.text, "ALLOW").isEmpty)  // #81: brez žetona
    // #81: svež config.json le s kazalcem na prijavo (ne trustedFolders, installedPlugins)
    #expect(field(w.text, "FSMON") == "core.fsmonitor")  // #81 N1: tudi v načinu direct git brez .git/config odjemalca
    #expect(Data(base64Encoded: field(w.text, "CONFIG")).map { String(decoding: $0, as: UTF8.self) } == copilotPointer)
    let home = field(w.text, "HOME")
    let req = (Sync.real(c.scratch("copilot/req")) ?? c.scratch("copilot/req")).path
    #expect(home.hasPrefix(req + "/") && !FileManager.default.fileExists(atPath: home))  // svež, v Scratch (R2), po zagonu izbrisan
    // R7: priprava paketa (v peskovniku, brez prijave).
    #expect(!FileManager.default.fileExists(atPath: bin.appendingPathComponent("version.log").path))  // #26: v peskovniku (zunaj ne piše)
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
    let r = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]), copilotConfig: URL(fileURLWithPath: "/nonexistent/.copilot/config.json"))  // L5
    let session = UUID().uuidString
    _ = await Router.shares.register(session: session, cwd: "/p", key: nil, device: nil,
                                     tree: ShareHub.Tree(repo: URL(fileURLWithPath: "/nonexistent/x.git"), worktrees: URL(fileURLWithPath: "/nonexistent/w"), sub: ""))
    var q = request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok", "x-claude-mini-session": session, "x-claude-mini-cwd": "/p"],
                    body: #"{"model":"claude-copilot","max_tokens":5,"tools":[{"name":"x","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}"#)
    q.peer = "127.0.0.1"
    let w = FakeWriter()
    await r.handle(q, writer: w)
    #expect(!w.text.contains("Copilot may not write"))  // F2: brez pisanja v klonu le bere (ni 400)

    let app = FakeApp()
    #expect(await admin(app.router(), "GET", "/agents/copilot").text.contains(#""commands":false"#))
    // #81 H1: ukazov ni: PATCH 400 z razlogom; GET commands:false in razlog tudi, ko je shranjen true
    let cmd = await admin(app.router(), "PATCH", "/agents/copilot", body: #"{"write":true,"commands":true}"#)
    #expect(cmd.status == 400 && cmd.text.contains("Copilot commands are not available") && !app.config.copilot.commands)
    app.config.copilot.commands = true
    let stored = await admin(app.router(), "GET", "/agents/copilot")
    #expect(stored.text.contains(#""commands":false"#) && stored.text.contains(#""commandsUnavailable":"Copilot commands are not available"#))
    app.config.copilot.commands = false
    #expect(await admin(app.router(), "PATCH", "/agents/copilot", body: #"{"enabled":true,"write":true,"models":["gpt-5.2"]}"#).status == 200)
    #expect(app.config.copilot.enabled && app.config.copilot.write && app.config.copilot.models == "gpt-5.2")
    #expect(await admin(app.router(), "PATCH", "/agents/copilot", body: #"{"models":["--yolo"]}"#).status == 400)
    #expect(await admin(app.router(), "PATCH", "/agents/copilot", body: #"{"allowAll":true}"#).status == 400)
    // #81 (D1): brez žetona; GET in status povesta, zakaj Copilot ni na voljo (FakeApp: prijave ni)
    #expect(await admin(app.router(), "PATCH", "/agents/copilot", body: #"{"token":"github_pat_secret1"}"#).status == 400 && app.keys.isEmpty)
    let got = await admin(app.router(), "GET", "/agents/copilot")
    #expect(!got.text.contains("hasToken") && got.text.contains(#""loginProblem":"Copilot is not signed in"#) && got.text.contains("not available: Copilot is not signed in"))
    #expect(await admin(app.router(), "GET", "/status").text.contains(#""copilot":"Copilot is not signed in"#))
}

/// Peskovnik copilota: piše v mapo seje in COPILOT_HOME zahteve, ne v domačo mapo. #81 H1: shranjen commands=true ne da
/// orodja shell. Brez prijave copilot login nič (503, x-should-retry: false).
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
    echo '{"type":"assistant.message","data":{"content":"WROTE='$inside$out$cache' TOKEN='$COPILOT_GITHUB_TOKEN' ARGS='"$(echo "$@" | tr ' ' '|')"'"}}'
    exit 0
    """)
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
    let login = try copilotLogin(root)
    let w = await ask(Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]), copilotConfig: login))
    #expect(w.status == 200 && w.text.contains("WROTE=011") && w.text.contains("TOKEN= ARGS="), "\(w.text)")  // mapa da; domača mapa, ~/Library/Caches ne
    #expect(FileManager.default.fileExists(atPath: proj.appendingPathComponent("inside.txt").path) && !FileManager.default.fileExists(atPath: outside.path))
    let args = field(w.text, "ARGS").split(separator: "|").map(String.init)
    #expect(rules(args).allow.contains("write") && !rules(args).allow.contains("shell") && rules(args).deny.contains("shell"), "\(args)")
    // #81 (D1): brez uporabne prijave ni Copilota (ne z ukazi ne brez), z razlogom; nikoli žeton
    c.copilot.commands = false
    let none = await ask(Router(config: c, token: "tok", secrets: FakeSecrets(values: ["copilot-token": "github_pat_old"]),
                                copilotConfig: root.appendingPathComponent("none/config.json")))
    #expect(none.status == 503 && none.text.contains("copilot login") && none.text.lowercased().contains("x-should-retry: false"))
    try Data(#"{"loggedInUsers":[{"host":"github.com","login":"u"}],"authTokens":{"github.com:u":"gho_secret"}}"#.utf8).write(to: login)
    let plain = await ask(Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]), copilotConfig: login))
    #expect(plain.status == 503 && plain.text.contains("plain text") && !plain.text.contains("gho_secret"))
}

/// R9: mapa seje, ki vsebuje paket copilot (~/Library/Caches/copilot), je zavrnjena (pisanje bi ga spremenilo).
@Test func copilotRefusesFolderWithItsPackageCache() async throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("copilot-r9-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let bin = root.appendingPathComponent("bin")
    try FileManager.default.createDirectory(at: bin, withIntermediateDirectories: true)
    let caches = try #require(Sync.real(URL(fileURLWithPath: NSHomeDirectory() + "/Library/Caches")))
    var c = RouterConfig()
    c.copilot.enabled = true; c.copilot.executable = try fakeCopilot(bin).path; c.copilot.write = true
    c.claudeCLI.allowedRoots = caches.path
    c.scratchFolder = root.appendingPathComponent("scratch").path
    let r = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]), copilotConfig: try copilotLogin(root))
    var q = request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok", "x-claude-mini-session": UUID().uuidString, "x-claude-mini-cwd": caches.path],
                    body: #"{"model":"claude-copilot","max_tokens":5,"tools":[{"name":"x","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"go"}]}"#)
    q.peer = "127.0.0.1"
    let w = FakeWriter()
    await r.handle(q, writer: w)
    #expect(w.status == 400, "\(w.text)")  // N2: resolveCwd jo zavrne že pred pravilom R9 (to ostane za mape ShareHub)
}

/// #26: copilot --version (priprava paketa) v peskovniku brez omrežja: piše le v svojo začasno mapo (in paket), ne drugam;
/// dovoljena mapa, ki vsebuje paket Copilot, je zavrnjena.
@Test func copilotPrepareIsSandboxedAndCacheFoldersRefused() async throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("prep-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let req = root.appendingPathComponent("req"), tmp = req.appendingPathComponent("t"), outside = root.appendingPathComponent("outside")
    for d in [tmp, outside] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
    let exe = root.appendingPathComponent("copilot")
    try Data("""
    #!/bin/sh
    echo in > "$PWD/inside.txt"
    echo out > "\(outside.path)/x.txt" 2>/dev/null
    /usr/bin/curl -s -m 2 -o /dev/null https://example.com && echo net > "$PWD/net.txt"
    exit 0
    """.utf8).write(to: exe)
    chmod(exe.path, 0o755)
    let box = try #require(Native.sandboxProfile(dir: tmp.path, network: false, hidden: [], runTmp: req.path, writable: []))
    Copilot.prepare(exe.path, base: ["PATH": "/usr/bin:/bin"], home: req.appendingPathComponent("p"), cwd: tmp, sandbox: box)
    #expect(FileManager.default.fileExists(atPath: tmp.appendingPathComponent("inside.txt").path))
    #expect(!FileManager.default.fileExists(atPath: outside.appendingPathComponent("x.txt").path))
    #expect(!FileManager.default.fileExists(atPath: tmp.appendingPathComponent("net.txt").path))  // brez omrežja

    #expect(RouterConfig.overlapsCopilotCache(RouterConfig.copilotCache.appendingPathComponent("pkg").path))
    #expect(RouterConfig.overlapsCopilotCache(NSHomeDirectory() + "/Library/Caches") && !RouterConfig.overlapsCopilotCache(root.path))
    var c = RouterConfig()
    c.scratchFolder = root.path  // Scratch drugje: zavrne le pravilo za paket Copilot
    let app = FakeApp(c)
    let w = await admin(app.router(), "PATCH", "/agents/claude", body: #"{"folders":"~/Library/Caches"}"#)
    #expect(w.status == 400 && w.text.contains("Copilot's package cache"))
    // N2: delovna mapa ne v paketu ali okoli njega; Claude ga ne sme pisati (denyWrite), tudi v načinu worktree
    #expect(ClaudeCLI.resolveCwd("~/Library/Caches", roots: "~") == nil && ClaudeCLI.resolveCwd(root.path, roots: root.path) != nil)
    var cli = ClaudeCLIConfig()
    cli.allowLocalTools = true
    let cache = RouterConfig.copilotCache.path.replacingOccurrences(of: "/", with: "\\/")
    for args in [cli.agentArgs(cwd: root, allTools: []),
                 cli.worktreeArgs(root: root, maven: root, cwd: root, allTools: [], network: false)] {
        let s = args[args.firstIndex(of: "--settings")! + 1]
        #expect(s.contains(#""denyWrite":["#) && s.contains(cache), "\(s)")
    }
}
