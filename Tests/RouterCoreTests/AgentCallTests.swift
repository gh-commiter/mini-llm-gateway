import Foundation
import Testing
@testable import RouterCore

/// GET na vrata izvajanja; -1 = povezava ni uspela (vrata zaprta).
private func get(_ run: AgentRun, _ path: String, token: String?) async -> Int {
    var r = URLRequest(url: URL(string: "http://127.0.0.1:\(run.port)\(path)")!)
    if let token { r.setValue("Bearer " + token, forHTTPHeaderField: "Authorization") }
    let s = URLSession(configuration: .ephemeral)
    guard let (_, resp) = try? await s.data(for: r) else { return -1 }
    return (resp as? HTTPURLResponse)?.statusCode ?? 0
}

/// Vrata izvajanja: le žeton izvajanja in tri poti; zahteva dobi napravo in naslov klicatelja; po stop() zaprta.
@Test func agentRunListener() async throws {
    var c = RouterConfig()
    c.claudeCLI.askOthers = true; c.claudeCLI.allowLocalTools = true
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    var caller = request("POST", "/v1/messages", headers: ["x-claude-mini-session": "s1", "x-claude-mini-cwd": "/client"])
    caller.peer = "10.0.0.9"
    caller.device = Noise.Device(id: "dev1", name: "phone")
    let run = try #require(await router.startAgentRun(caller, kind: "claude", dir: nil))
    run.started(getpid())  // M2: ta proces je »agent« izvajanja
    #expect(run.env["MINIGW_AGENT_URL"] == "http://127.0.0.1:\(run.port)" && run.env["MINIGW_AGENT_AUTH_FILE"] == run.authFile.path)
    // M3: žeton le v datoteki 0600, ne v okolju; po koncu izvajanja izbrisan (preverjeno spodaj po stop)
    #expect(FileManager.default.contents(atPath: run.authFile.path) == Data(run.token.utf8) && !run.env.values.contains(run.token))
    #expect((try? FileManager.default.attributesOfItem(atPath: run.authFile.path)[.posixPermissions] as? Int) == 0o600)
    #expect(await get(run, "/v1/models", token: nil) == 401)
    #expect(await get(run, "/v1/models", token: "tok") == 401)  // žeton prehoda tu ne velja
    for p in ["/admin/v1/settings", "/minigw/share/next", "/health/live"] { #expect(await get(run, p, token: run.token) == 404) }
    #expect(await get(run, "/v1/models", token: run.token) == 403)  // neznana oddaljena naprava: kot izvirna zahteva
    run.stop()
    #expect(!FileManager.default.fileExists(atPath: run.authFile.path))
    #expect(await get(run, "/v1/models", token: run.token) == -1)

    // Loopback klicatelj: modeli; stikalo izklopljeno: ni vrat.
    let local = try #require(await router.startAgentRun(request("POST", "/v1/messages"), kind: "claude", dir: nil))
    defer { local.stop() }
    #expect(await get(local, "/v1/models", token: local.token) == 403)  // M2: proces agenta še ne teče
    local.started(getpid())
    #expect(await get(local, "/v1/models", token: local.token) == 200)
    local.running.withLock { $0 = AgentRun.maxNested }  // M1: vsa mesta izvajanja zasedena
    #expect(await get(local, "/v1/models", token: local.token) == 429)
    local.running.withLock { $0 = 0 }
    let other = Process()  // M2: proces zunaj drevesa izvajanja
    other.executableURL = URL(fileURLWithPath: "/bin/sleep")
    other.arguments = ["30"]
    try other.run()
    defer { other.terminate() }
    local.started(other.processIdentifier)
    #expect(await get(local, "/v1/models", token: local.token) == 403)
    #expect(await router.startAgentRun(caller, kind: "codex", dir: nil) == nil)
}

@Test func agentRunChain() async throws {
    var c = RouterConfig()
    c.claudeCLI.askOthers = true; c.claudeCLI.allowLocalTools = true; c.codex.askOthers = true
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    #expect(AgentRun.refusal("claude") == nil)
    let r1 = try #require(await router.startAgentRun(request("POST", "/v1/messages"), kind: "claude", dir: nil))
    defer { r1.stop() }
    try await AgentRun.$current.withValue(r1) {
        #expect(AgentRun.refusal("codex") == nil)
        #expect(AgentRun.refusal("claude")?.contains("cannot ask its own kind") == true)
        let r2 = try #require(await router.startAgentRun(request("POST", "/v1/messages"), kind: "codex", dir: nil))
        defer { r2.stop() }
        #expect(r2.chain == ["claude", "codex"])
        AgentRun.$current.withValue(r2) {
            #expect(AgentRun.refusal("grok")?.contains("cannot ask further models") == true)
        }
    }
}

@Test func agentChainHeaderFromOutside() async {
    let w = FakeWriter()
    await makeRouter().handle(request("GET", "/v1/models", headers: ["Authorization": "Bearer tok", "X-Minigw-Agent-Chain": "claude"]), writer: w)
    #expect(w.status == 400)
}

/// Lažni claude v sinhronizirani mapi vpraša lažni codex prek MINIGW_AGENT_URL: codex piše v isti klon (brez drugega
/// prenosa), sprememba pride k odjemalcu; codex, ki vpraša claude, je zavrnjen (zanka).
@Test func agentAsksAgentInSameFolder() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("agentcalls-" + UUID().uuidString)
    let client = base.appendingPathComponent("client"), bin = base.appendingPathComponent("bin")
    defer { try? FileManager.default.removeItem(at: base) }
    for d in [client, bin] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
    func ask(_ model: String) -> String {
        #"curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $(cat "$MINIGW_AGENT_AUTH_FILE")" -H 'content-type: application/json' "#
            + #"-d '{"model":"\#(model)","max_tokens":10,"tools":[{"name":"x"}],"messages":[{"role":"user","content":"hi"}]}' "$MINIGW_AGENT_URL/v1/messages""#
    }
    func script(_ name: String, _ body: String) throws -> String {
        let u = bin.appendingPathComponent(name)
        try Data(("#!/bin/sh\n" + body).utf8).write(to: u)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: u.path)
        return u.path
    }
    var c = RouterConfig()
    c.scratchFolder = base.appendingPathComponent("scratch").path
    c.claudeCLI.enabled = true; c.claudeCLI.askOthers = true; c.claudeCLI.allowLocalTools = true
    c.claudeCLI.executable = try script("claude", """
        [ "$1" = --version ] && { echo '2.1.0 (Claude Code)'; exit 0; }
        cat > /dev/null
        prev=""; for a in "$@"; do [ "$prev" = --append-system-prompt ] && printf '%s' "$a" > note.txt; [ "$prev" = --settings ] && printf '%s' "$a" > settings.txt; prev="$a"; done
        echo "$(\(ask("claude-codex")))" > asked.txt
        echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
        echo '{"type":"result","subtype":"success","usage":{"output_tokens":3}}'

        """)
    c.codex.enabled = true; c.codex.askOthers = true
    c.codex.executable = try script("codex", """
        out=""; prev=""
        for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
        cat > prompt.txt
        echo codex > codex.txt
        echo "$(\(ask("claude-cli-haiku")))" > loop.txt
        echo "codex answer" > "$out"

        """)
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    let session = UUID().uuidString
    let h = ["Authorization": "Bearer tok", "x-claude-mini-session": session, "x-claude-mini-cwd": client.path]
    let reg = FakeWriter()
    await router.handle(request("POST", "/minigw/share", headers: h.merging(["x-claude-mini-work": "sync"]) { $1 }), writer: reg)
    let key = try #require(reg.text.split(separator: "\"").dropFirst(3).first.map(String.init))
    let minigw = FakeMinigw(Router.shares, key: key, root: client)
    let server = minigw.serve()
    defer { server.cancel() }

    let w = FakeWriter()
    await router.handle(request("POST", "/v1/messages", headers: h,
                                body: #"{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}"#), writer: w)
    #expect(w.status == 200, "\(w.text)")
    func read(_ n: String) -> String? {
        (try? String(contentsOf: client.appendingPathComponent(n), encoding: .utf8))?.trimmingCharacters(in: .whitespacesAndNewlines)
    }
    #expect(read("asked.txt") == "200")
    #expect(read("codex.txt") == "codex")  // codex je pisal v klon klicatelja, prevzem ob koncu claude
    #expect(read("loop.txt") == "403")
    let exe = Router.minigwExe
    #expect(read("note.txt")?.contains("You may ask another model of this gateway: `\"\(exe)\" -p --model") == true)
    #expect(read("settings.txt")?.contains(exe.replacingOccurrences(of: "/", with: "\\/")) == true || read("settings.txt")?.contains(exe) == true)
    #expect(read("prompt.txt")?.contains("Another agent on this gateway (claude) asks you this") == true)
    #expect(read("prompt.txt")?.contains("You may ask another model") == true)
    let unreg = FakeWriter()
    await router.handle(request("DELETE", "/minigw/share", headers: ["Authorization": "Bearer tok", "x-share-key": key]), writer: unreg)
}

/// Claude CLI: stikalo velja le z »Allow local tools« (brez Bash ne more klicati minigw).
@Test func agentAskOthersNeedsLocalTools() async {
    var c = RouterConfig()
    c.claudeCLI.askOthers = true
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    #expect(!router.askOthers("claude") && router.agentNote("claude") == nil)
    #expect(await router.startAgentRun(request("POST", "/v1/messages"), kind: "claude", dir: nil) == nil)
}

@Test func codexUsageLimitIs429() {
    let line = Data(#"{"type":"turn.failed","error":{"message":"You’ve hit your usage limit. Try again at 7:52 PM. /Users/x/secret"}}"#.utf8)
    let (status, type, msg) = AgentError.classify(Codex.failure(line) ?? "")
    #expect(status == 429 && type == "rate_limit_error" && msg.contains("usage limit") && !msg.contains("/Users"))
    #expect(Codex.failure(Data(#"{"type":"error","message":"boom"}"#.utf8)) == "boom")
    #expect(AgentError.classify("codex exited with 1").0 == 502)
}

@Test func agentLimitKeywordsAndCleaning() {
    for m in ["You've hit your 5-hour limit · resets 9pm", "You're out of usage credits", "Your org is out of usage", "monthly spend limit", "You've reached your cap"] {
        #expect(AgentError.classify(m).0 == 429)
    }
    #expect(AgentError.clean("failed C:/x/y ./rel file:///etc/passwd https://a.b/c request req_abc123") == "failed https://a.b/c request req_abc123")
}

/// Napaka CLI za odjemalca: le besedilo (brez »Internal error:«, ovoja JSON in »API error (status 402 …):«), kot v Go.
@Test func agentMessageExtraction() {
    let line = Data(#"{"type":"error","message":"Internal error: {\n  \"message\": \"API error (status 402 Payment Required): personal-team-blocked:spending-limit: You have run out of credits or need a Grok subscription. Add credits at https://grok.com/?_s=usage or upgrade at https://grok.com/supergrok.\",\n  \"http_status\": 402\n}","cwd":"/Users/x/w"}"#.utf8)
    guard case .error(let raw)? = Grok.event(line) else { Issue.record("no error event"); return }
    let (status, _, msg) = AgentError.classify(raw)
    #expect(status == 429)
    #expect(msg == "You have run out of credits or need a Grok subscription. Add credits at https://grok.com/?_s=usage or upgrade at https://grok.com/supergrok.")
    let codex = Codex.failure(Data(#"{"type":"error","message":"{\"error\":{\"type\":\"usage_limit_reached\",\"message\":\"You've hit your usage limit.\"}}"}"#.utf8)) ?? ""
    #expect(AgentError.classify(codex) == (429, "rate_limit_error", "You've hit your usage limit."))
    #expect(AgentError.message(#"{"error":"quota exceeded"}"#) == "quota exceeded")
    #expect(AgentError.message("Internal error: {not json") == "Internal error: {not json")
}

/// Napaka pred glavo (tudi pri pretoku) je pravi 429 z x-should-retry: false (Claude Code ne ponovi); po glavi dogodek SSE,
/// ponovitev brez pretoka dobi isti 429 brez novega zagona CLI.
@Test func replyWhenDoneRetryHeaders() async throws {
    let router = Router(config: RouterConfig(), token: "tok", secrets: FakeSecrets(values: [:]))
    let req = request("POST", "/v1/messages", headers: ["x-claude-mini-session": "retry-\(UUID().uuidString)"])
    let limit = Grok.Failure(message: #"Internal error: {"message":"API error (status 402 Payment Required): You have run out of credits."}"#)
    for stream in [true, false] {
        let w = FakeWriter()
        let st = await router.replyWhenDone(body: ["model": "claude-grok", "stream": stream], model: "claude-grok", rid: "req_1", writer: w, req: req) { throw limit }
        #expect(st == 429 && w.status == 429 && w.text.contains("x-should-retry: false") && w.text.contains(#""You have run out of credits.""#))
    }
    #expect(AgentError.recent(AgentError.key(req, model: "claude-grok")) == nil)  // HTTP 429 je končen
    let w = FakeWriter()
    _ = await router.replyWhenDone(body: ["model": "claude-grok", "stream": true], model: "claude-grok", rid: "req_2", writer: w, req: req,
                                   headDelay: .milliseconds(50)) {
        try await Task.sleep(for: .milliseconds(300))
        throw limit
    }
    #expect(w.status == 200 && w.text.contains("x-should-retry: false") && w.text.contains("event: error") && w.text.contains("rate_limit_error"))
    #expect(AgentError.recent(AgentError.key(req, model: "claude-grok")) == "You have run out of credits.")
    #expect(AgentError.recent(AgentError.key(req, model: "claude-grok")) == nil)  // enkrat
    let fallback = FakeWriter()
    AgentError.note(AgentError.key(req, model: "claude-grok"), "You have run out of credits.")
    await router.handle(request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok", "x-claude-mini-session": req.header("x-claude-mini-session")!],
                                    body: #"{"model":"claude-grok","messages":[{"role":"user","content":"hi"}]}"#), writer: fallback)
    #expect(fallback.status == 429 && fallback.text.contains("x-should-retry: false") && fallback.text.contains("run out of credits"))
}

/// --minigw-ro brez deljenja (prehod na istem Macu): glava x-claude-mini-ro da Claude CLI brez orodij za pisanje in
/// sandbox brez pisanja; codex, ki ga vpraša, dela le za branje, tudi ko vprašani agent pošlje »x-claude-mini-ro: 0«.
@Test func readOnlyHeaderWithoutShare() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("ro-" + UUID().uuidString)
    let scratch = FileManager.default.temporaryDirectory.appendingPathComponent("ro-scratch-" + UUID().uuidString)
    let client = base.appendingPathComponent("client"), bin = base.appendingPathComponent("bin")
    defer { for d in [base, scratch] { try? FileManager.default.removeItem(at: d) } }
    for d in [client, bin] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
    func script(_ name: String, _ body: String) throws -> String {
        let u = bin.appendingPathComponent(name)
        try Data(("#!/bin/sh\n" + body).utf8).write(to: u)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: u.path)
        return u.path
    }
    var c = RouterConfig()
    c.scratchFolder = scratch.path
    c.claudeCLI.allowedRoots = base.path
    c.claudeCLI.enabled = true; c.claudeCLI.askOthers = true; c.claudeCLI.allowLocalTools = true
    c.claudeCLI.executable = try script("claude", """
        [ "$1" = --version ] && { echo '2.1.0 (Claude Code)'; exit 0; }
        cat > /dev/null
        printf '%s\\n' "$@" > claude-args.txt
        curl -s -o /dev/null -H "Authorization: Bearer $(cat "$MINIGW_AGENT_AUTH_FILE")" -H 'content-type: application/json' -H 'x-claude-mini-ro: 0' \\
          -d '{"model":"claude-codex","max_tokens":10,"tools":[{"name":"x"}],"messages":[{"role":"user","content":"hi"}]}' "$MINIGW_AGENT_URL/v1/messages"
        echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
        echo '{"type":"result","subtype":"success","usage":{"output_tokens":3}}'

        """)
    c.codex.enabled = true
    c.codex.executable = try script("codex", """
        out=""; prev=""
        for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
        cat > /dev/null
        printf '%s\\n' "$@" > codex-args.txt
        echo "codex answer" > "$out"

        """)
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    func run(readOnly: Bool) async throws -> (claude: String, codex: String) {
        var h = ["Authorization": "Bearer tok", "x-claude-mini-session": UUID().uuidString, "x-claude-mini-cwd": client.path]
        if readOnly { h["x-claude-mini-ro"] = "1" }
        let w = FakeWriter()
        await router.handle(request("POST", "/v1/messages", headers: h,
                                    body: #"{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}"#), writer: w)
        #expect(w.status == 200, "\(w.text)")
        func read(_ n: String) throws -> String { try String(contentsOf: client.appendingPathComponent(n), encoding: .utf8) }
        defer { for n in ["claude-args.txt", "codex-args.txt"] { try? FileManager.default.removeItem(at: client.appendingPathComponent(n)) } }
        return (try read("claude-args.txt"), try read("codex-args.txt"))
    }
    let rw = try await run(readOnly: false)
    #expect(rw.codex.contains("workspace-write") && !rw.claude.contains("EnterWorktree"))  // brez glave: pisanje (kontrola)
    let ro = try await run(readOnly: true)
    #expect(ro.codex.contains("--sandbox\nread-only") && !ro.codex.contains("workspace-write") && !ro.codex.contains("--add-dir"))
    #expect(ro.claude.contains("--disallowedTools\nEdit,Write,MultiEdit,NotebookEdit,EnterWorktree,ExitWorktree,Monitor") && ro.claude.contains(#""allowWrite":[]"#))
}

/// Agent, ki v klonu le bere (worktree, pisanje izklopljeno): izvajanje nosi --minigw-ro, vprašani agenti dobijo glavo
/// x-claude-mini-ro (kot Go startRun); brez tega velja zahteva.
@Test func readOnlyAgentRunIsReadOnlyForAskedAgents() async throws {
    var c = RouterConfig()
    c.claudeCLI.askOthers = true; c.claudeCLI.allowLocalTools = true; c.codex.askOthers = true
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    let caller = request("POST", "/v1/messages", headers: ["x-claude-mini-session": UUID().uuidString, "x-claude-mini-cwd": "/client"])
    let ro = try #require(await router.startAgentRun(caller, kind: "codex", dir: nil, readOnly: true))
    defer { ro.stop() }
    let rw = try #require(await router.startAgentRun(caller, kind: "codex", dir: nil))
    defer { rw.stop() }
    #expect(ro.readOnly && !rw.readOnly)
}
