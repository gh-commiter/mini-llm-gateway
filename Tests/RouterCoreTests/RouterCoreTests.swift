import CryptoKit
import Foundation
import Testing
@testable import RouterCore

final class FakeWriter: ResponseWriter, @unchecked Sendable {
    var data = Data()
    var closed = false
    func write(_ d: Data) async throws { data.append(d) }
    func close() { closed = true }
    var text: String { String(decoding: data, as: UTF8.self) }
    var status: Int { Int(text.split(separator: " ")[1]) ?? 0 }
}

struct FakeSecrets: SecretStore {
    var values: [String: String]
    func get(_ account: String) -> String? { values[account] }
}

/// Upstream stub: zabeleži zahtevo in vrne SSE.
final class StubUpstream: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var lastRequest: URLRequest?
    nonisolated(unsafe) static var lastBody = Data()

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func stopLoading() {}
    override func startLoading() {
        if request.httpMethod == "GET" {  // odkrivanje modelov
            let body = request.url!.path.hasSuffix("/model/info")
                ? #"{"data":[{"model_name":"lite/model","model_info":{"max_input_tokens":250000}}]}"#
                : #"{"data":[{"id":"qwen/qwen3-coder","context_length":32000},{"id":"lite/model"},{"id":"*"}]}"#
            let resp = HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "application/json"])!
            client?.urlProtocol(self, didReceive: resp, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: Data(body.utf8))
            client?.urlProtocolDidFinishLoading(self)
            return
        }
        Self.lastRequest = request
        if let s = request.httpBodyStream {
            s.open()
            var buf = [UInt8](repeating: 0, count: 4096)
            var d = Data()
            while s.hasBytesAvailable { let n = s.read(&buf, maxLength: buf.count); if n <= 0 { break }; d.append(buf, count: n) }
            s.close()
            Self.lastBody = d
        }
        let resp = HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1",
                                   headerFields: ["Content-Type": "text/event-stream"])!
        client?.urlProtocol(self, didReceive: resp, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data("event: message_start\ndata: {}\n\nevent: message_stop\ndata: {}\n\n".utf8))
        client?.urlProtocolDidFinishLoading(self)
    }
}

func request(_ method: String, _ target: String, headers: [String: String] = [:], body: String = "") -> HTTPRequest {
    var raw = "\(method) \(target) HTTP/1.1\r\nHost: x\r\nContent-Length: \(body.utf8.count)\r\n"
    for (k, v) in headers { raw += "\(k): \(v)\r\n" }
    raw += "\r\n" + body
    guard case .request(var r) = HTTPParser.parse(Data(raw.utf8)) else { fatalError("parse failed") }
    r.peer = "127.0.0.1"  // ta Mac (prava zahteva ima vedno naslov); oddaljene teste nastavijo peer sami
    return r
}

func makeRouter(session: URLSession = .shared) -> Router {
    var c = RouterConfig()
    c.providers = [
        ProviderConfig(id: "openrouter", name: "OpenRouter", enabled: true, baseURL: "https://up.example/api/",
                       aliases: "or-qwen = qwen/qwen3-coder, 1k\n# komentar\nanthropic/claude-sonnet"),
        ProviderConfig(id: "litellm", name: "LiteLLM", enabled: false, baseURL: "http://x.local", aliases: "qwen-local = q"),
    ]
    return Router(config: c, token: "tok", secrets: FakeSecrets(values: ["openrouter": "or-key"]), session: session)
}

@Test func parsesIncrementallyWithQuery() {
    let raw = "POST /v1/messages?beta=true HTTP/1.1\r\nContent-Length: 4\r\nX-Api-Key: k\r\n\r\nabcd"
    let full = Data(raw.utf8)
    guard case .needMore = HTTPParser.parse(full.prefix(full.count - 2)) else { Issue.record("expected needMore"); return }
    guard case .request(let r) = HTTPParser.parse(full) else { Issue.record("expected request"); return }
    #expect(r.method == "POST" && r.path == "/v1/messages" && r.query == "beta=true")
    #expect(r.header("x-api-key") == "k" && r.body == Data("abcd".utf8))
}

@Test func rejectsChunkedAndHugeBodies() {
    guard case .error(411, _) = HTTPParser.parse(Data("POST / HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n".utf8)) else { Issue.record("411"); return }
    guard case .error(413, _) = HTTPParser.parse(Data("POST / HTTP/1.1\r\nContent-Length: 999999999999\r\n\r\n".utf8)) else { Issue.record("413"); return }
}

@Test func aliasParsing() {
    let p = ProviderConfig(id: "x", name: "X", enabled: true, baseURL: "", aliases: " a = b \n\n# c\nd, 128k\n=bad\ne = f, 1M")
    #expect(p.aliasPairs.map { "\($0.alias)>\($0.upstream)>\($0.window ?? 0)" } == ["a>b>0", "d>d>128000", "=bad>=bad>0", "e>f>1000000"])
    #expect(ProviderConfig.publishedID("qwen") == "claude-qwen")
    #expect(ProviderConfig.publishedID("anthropic/claude-x") == "claude-anthropic/claude-x")
    #expect(ProviderConfig.publishedID("Claude-x") == "Claude-x")
}

@Test func healthWithoutTokenOtherwise401() async {
    let r = makeRouter()
    let w1 = FakeWriter(); await r.handle(request("GET", "/health/live"), writer: w1)
    #expect(w1.status == 200 && w1.closed)
    let w2 = FakeWriter(); await r.handle(request("GET", "/v1/models"), writer: w2)
    #expect(w2.status == 401 && w2.text.contains("authentication_error"))
    let w3 = FakeWriter(); await r.handle(request("GET", "/v1/models", headers: ["Authorization": "Bearer wrong"]), writer: w3)
    #expect(w3.status == 401)
}


@Test func unknownModel404() async {
    let w = FakeWriter()
    await makeRouter().handle(request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok"],
                                      body: #"{"model":"nope","messages":[]}"#), writer: w)
    #expect(w.status == 404 && w.text.contains("not_found_error"))
}




@Test func cliModelMatching() throws {
    #expect(ClaudeCLI.match("claude-opus-5-5")! == ("claude-opus-5-5", false))
    #expect(ClaudeCLI.match("haiku")! == ("haiku", false))
    #expect(ClaudeCLI.match("claude-chat-sonnet")! == ("sonnet", true))
    #expect(ClaudeCLI.match("claude-cli-sonnet")! == ("sonnet", false))
    var cli = ClaudeCLIConfig()
    let cwd = URL(fileURLWithPath: "/tmp/session")
    #expect(cli.agentArgs(cwd: cwd, allTools: []) == ["--tools", "Read,Grep,Glob", "--allowedTools", "Read,Grep,Glob", "--permission-mode", "dontAsk"])
    cli.tools = "Read,Edit,Bash(git:*),Write,Grep"  // --minigw-ro: brez orodij za pisanje; Bash tu nima sandboxa
    #expect(cli.agentArgs(cwd: cwd, allTools: [], readOnly: true) == ["--tools", "Read,Grep", "--allowedTools", "Read,Grep", "--permission-mode", "dontAsk"])
    cli.tools = "Read,Monitor,EnterWorktree,WebFetch"  // seznam dovoljenih: orodja, ki niso na njem, odpadejo
    #expect(cli.agentArgs(cwd: cwd, allTools: [], readOnly: true).prefix(2) == ["--tools", "Read,WebFetch"])
    cli.tools = "Read,Grep,Glob"
    var local = cli
    local.allowLocalTools = true
    let ro = local.agentArgs(cwd: cwd, allTools: ["Bash", "Edit", "EnterWorktree", "ExitWorktree", "Monitor", "Read", "Task", "WebFetch"], readOnly: true)
    func value(_ flag: String) -> String { ro.firstIndex(of: flag).map { ro[$0 + 1] } ?? "" }
    #expect(value("--tools") == "Read,Grep,Glob,LS,WebSearch,WebFetch,Bash" && value("--allowedTools") == "WebFetch")
    #expect(["EnterWorktree", "ExitWorktree", "Monitor", "Edit"].allSatisfy { value("--disallowedTools").split(separator: ",").contains(Substring($0)) })
    cli.allowLocalTools = true
    cli.allowedRoots = "/tmp"
    cli.disabledTools = ["WebSearch"]
    let a = cli.agentArgs(cwd: cwd, allTools: ["Bash", "Edit", "Read", "Task", "WebFetch", "WebSearch"])
    #expect(!a.contains("--dangerously-skip-permissions") && a.contains("acceptEdits"))
    #expect(a.contains("--add-dir") && a.contains("/tmp"))
    // datotečna orodja niso splošno dovoljena (veljajo le v mapah), ostala da; izjeme ostanejo zavrnjene
    #expect(a[a.firstIndex(of: "--allowedTools")! + 1] == "Task,WebFetch")
    #expect(a[a.count - 2] == "--disallowedTools" && a.last!.hasPrefix("WebSearch,") && a.last!.contains("Edit(//tmp/session/**/.git/config)"))  // N3
    let settings = try JSONSerialization.jsonObject(with: Data(a[a.firstIndex(of: "--settings")! + 1].utf8)) as! [String: Any]
    let sb = settings["sandbox"] as! [String: Any], fs = sb["filesystem"] as! [String: [String]]
    #expect(sb["allowUnsandboxedCommands"] as? Bool == false && fs["denyRead"] == ["~"] && fs["allowWrite"] == ["/tmp/session", "/tmp"])
    #expect(ClaudeCLI.match("claude-siri-ai") == nil)
    #expect(ClaudeCLI.match("claude-chat-gpt") == nil)
}

@Test func transcriptKeyAndPrompt() {
    let body: [String: Any] = ["messages": [
        ["role": "user", "content": "hi"],
        ["role": "assistant", "content": [["type": "text", "text": "hello "], ["type": "tool_use", "name": "Read"]]],
        ["role": "user", "content": [["type": "tool_result", "content": "data"], ["type": "text", "text": "next"]]],
        ["role": "system", "content": [["type": "text", "text": "# Environment …"]]],  // kot ga pošlje Claude Code
    ]]
    let t = ClaudeCLI.turns(body)
    #expect(t.map(\.text) == ["hi", "hello \n[tool_use Read]", "[tool_result] data\nnext"])
    #expect(ClaudeCLI.key(t.prefix(2), scope: "s") == ClaudeCLI.key(Array(t.prefix(2)), scope: "s"))
    #expect(ClaudeCLI.key(t.prefix(2), scope: "s") != ClaudeCLI.key(t.prefix(2), scope: "other"))
    #expect(ClaudeCLI.prompt(history: [ClaudeCLI.Turn](), last: t[0]) == "hi")
    #expect(ClaudeCLI.prompt(history: t.prefix(2), last: t[2]).hasSuffix("---\n\n[tool_result] data\nnext"))
}

@Test func cwdMustBeUnderAllowedRoots() throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent("cwdtest-\(UUID())")
    let inside = root.appendingPathComponent("proj")
    try FileManager.default.createDirectory(at: inside, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: root) }
    #expect(ClaudeCLI.resolveCwd(inside.path, roots: root.path) != nil)
    #expect(ClaudeCLI.resolveCwd(inside.path + "/../proj", roots: root.path) != nil)
    #expect(ClaudeCLI.resolveCwd(root.path + "x/proj", roots: root.path) == nil)  // predpona, ki ni mapa
    #expect(ClaudeCLI.resolveCwd(inside.path, roots: "/tmp/nope") == nil)
    #expect(ClaudeCLI.resolveCwd(inside.path + "/missing", roots: root.path) == nil)
    #expect(ClaudeCLI.resolveCwd(inside.path, roots: "/") == nil)  // koren ni dovoljen
}

@Test func streamJsonToAnthropicSSE() {
    let lines = [
        #"{"type":"system","subtype":"init","session_id":"s1"}"#,
        #"{"type":"stream_event","event":{"type":"message_start","message":{}},"parent_tool_use_id":null}"#,
        #"{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Looking"}},"parent_tool_use_id":null}"#,
        #"{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"Read"}},"parent_tool_use_id":null}"#,
        #"{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ignored"}},"parent_tool_use_id":"toolu_1"}"#,
        #"{"type":"stream_event","event":{"type":"message_start","message":{}},"parent_tool_use_id":null}"#,
        #"{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}},"parent_tool_use_id":null}"#,
        #"{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Done."}},"parent_tool_use_id":null}"#,
        #"{"type":"result","subtype":"success","is_error":false,"result":"Done.","usage":{"input_tokens":5,"output_tokens":7}}"#,
    ]
    var t = StreamTranslator(model: "claude-opus-5-5")
    var out = t.start()
    for l in lines { out += t.feed(Data(l.utf8)) }
    out += t.finish()
    #expect(t.text == "Looking\n⏺ Read\nDone.")
    #expect(t.resultError == nil)
    let sse = String(decoding: out.reduce(Data(), +), as: UTF8.self)
    #expect(sse.hasPrefix("event: message_start\ndata: "))
    #expect(sse.contains(#""model":"claude-opus-5-5""#))
    #expect(sse.contains(#""output_tokens":7"#))
    #expect(sse.hasSuffix("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
    #expect((t.message()["content"] as? [[String: Any]])?.first?["text"] as? String == "Looking\n⏺ Read\nDone.")

    var e = StreamTranslator(model: "m")
    _ = e.feed(Data(#"{"type":"result","subtype":"error_during_execution","is_error":true}"#.utf8))
    #expect(e.resultError == "error_during_execution")
}

@Test func cliModelsListedOnlyWhenEnabledAndUnknownWhenDisabled() async {
    var c = RouterConfig()
    c.providers = []
    let off = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    let w1 = FakeWriter()
    await off.handle(request("POST", "/v1/messages", headers: ["x-api-key": "tok"], body: #"{"model":"claude-opus-5-5","messages":[]}"#), writer: w1)
    #expect(w1.status == 404)
    c.claudeCLI.enabled = true
    let on = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    // Preizkus `claude --version` ima pri zahtevi 2 s (odkrivanje v Claude Code), sicer »še ne«; pod obremenitvijo
    // (vzporedni testi, druge gradnje) traja dlje, zato kot Claude Code: vprašaj znova.
    var w2 = FakeWriter()
    for _ in 0..<15 {
        w2 = FakeWriter()
        await on.handle(request("GET", "/v1/models", headers: ["x-api-key": "tok"]), writer: w2)
        if w2.text.contains("claude-cli-opus") { break }
    }
    #expect(w2.text.contains("claude-cli-opus") && w2.text.contains("claude-cli-haiku") && !w2.text.contains("claude-chat-"))
    let w3 = FakeWriter()  // agent brez veljavne mape → jasna napaka, brez zagona claude
    await on.handle(request("POST", "/v1/messages", headers: ["x-api-key": "tok", "x-claude-mini-cwd": "/nonexistent"],
                            body: #"{"model":"claude-opus-5-5","tools":[{"name":"Read"}],"messages":[{"role":"user","content":"hi"}]}"#), writer: w3)
    #expect(w3.status == 400 && w3.text.contains("cwd_unavailable"))
}

/// Testi si delijo StubUpstream (statično stanje), zato tečejo zaporedno.
@Suite(.serialized) struct UpstreamStubTests {
    @Test func modelsListsOnlyEnabledProviders() async {
        let w = FakeWriter()
        let cfg = URLSessionConfiguration.ephemeral
        cfg.protocolClasses = [StubUpstream.self]
        await makeRouter(session: URLSession(configuration: cfg)).handle(request("GET", "/v1/models", headers: ["x-api-key": "tok"]), writer: w)
        #expect(w.status == 200)
        #expect(w.text.contains("\"claude-or-qwen\"") && w.text.contains("\"claude-anthropic/claude-sonnet\""))
        #expect(!w.text.contains("\"or-qwen\"") && !w.text.contains("qwen-local"))
        #expect(w.text.contains("OpenRouter · qwen/qwen3-coder · 1K"))
    }

    @Test func discoversModelsWhenAliasesEmpty() async {
        let cfg = URLSessionConfiguration.ephemeral
        cfg.protocolClasses = [StubUpstream.self]
        var c = RouterConfig()
        c.providers = [ProviderConfig(id: "litellm", name: "LiteLLM", enabled: true, baseURL: "http://lite.local", aliases: "")]
        let r = Router(config: c, token: "tok", secrets: FakeSecrets(values: ["litellm": "k"]), session: URLSession(configuration: cfg))
        let w = FakeWriter()
        await r.handle(request("GET", "/v1/models", headers: ["x-api-key": "tok"]), writer: w)
        #expect(w.text.contains(#""id":"claude-qwen/qwen3-coder""#) && w.text.contains("qwen/qwen3-coder · 32K"))
        #expect(w.text.contains(#""id":"claude-lite/model""#) && w.text.contains("lite/model · 250K"))  // okno iz /model/info
        #expect(!w.text.contains(#""id":"claude-*""#))
        let w2 = FakeWriter()  // usmerjanje po objavljenem ID-ju in po imenu pri upstreamu
        await r.handle(request("POST", "/v1/messages", headers: ["x-api-key": "tok"], body: #"{"model":"claude-lite/model","messages":[]}"#), writer: w2)
        #expect(w2.status == 200)
        let sent = try? JSONSerialization.jsonObject(with: StubUpstream.lastBody) as? [String: Any]
        #expect(sent?["model"] as? String == "lite/model")
    }

    @Test func manualWindowFallsBackToDiscovered() async {
        let cfg = URLSessionConfiguration.ephemeral
        cfg.protocolClasses = [StubUpstream.self]
        var c = RouterConfig()
        c.providers = [ProviderConfig(id: "or", name: "OR", enabled: true, baseURL: "http://or.local", aliases: "coder = qwen/qwen3-coder")]
        let r = Router(config: c, token: "tok", secrets: FakeSecrets(values: ["or": "k"]), session: URLSession(configuration: cfg))
        let w = FakeWriter()
        await r.handle(request("GET", "/v1/models", headers: ["x-api-key": "tok"]), writer: w)
        #expect(w.text.contains(#""id":"claude-coder""#) && w.text.contains("qwen/qwen3-coder · 32K"))
        #expect(!w.text.contains("lite/model"))  // izpolnjen seznam = samo našteti
    }

    @Test func onlyCheckedModelsArePublished() async {
        let cfg = URLSessionConfiguration.ephemeral
        cfg.protocolClasses = [StubUpstream.self]
        for (enabled, expectLite, expectQwen) in [(["lite/model"], true, false), ([], false, false), (nil as [String]?, true, true)] {
            var c = RouterConfig()
            c.providers = [ProviderConfig(id: "or", name: "OR", enabled: true, baseURL: "http://or.local", aliases: "", enabledModels: enabled)]
            let r = Router(config: c, token: "tok", secrets: FakeSecrets(values: ["or": "k"]), session: URLSession(configuration: cfg))
            let w = FakeWriter()
            await r.handle(request("GET", "/v1/models", headers: ["x-api-key": "tok"]), writer: w)
            #expect(w.text.contains("claude-lite/model") == expectLite && w.text.contains("claude-qwen/qwen3-coder") == expectQwen)
            let w2 = FakeWriter()  // izklopljen model se ne usmerja
            await r.handle(request("POST", "/v1/messages", headers: ["x-api-key": "tok"], body: #"{"model":"claude-qwen/qwen3-coder","messages":[]}"#), writer: w2)
            #expect((w2.status == 200) == expectQwen)
        }
    }

    @Test func unreachableProviderIsNotListed() async {
        let w = FakeWriter()  // up.example ne obstaja → preverjanje pade → modela ni na seznamu
        await makeRouter().handle(request("GET", "/v1/models", headers: ["x-api-key": "tok"]), writer: w)
        #expect(w.status == 200 && !w.text.contains("claude-or-qwen"))
    }

    @Test func passthroughRewritesModelAndAuthAndStreams() async throws {
        let cfg = URLSessionConfiguration.ephemeral
        cfg.protocolClasses = [StubUpstream.self]
        let w = FakeWriter()
        await makeRouter(session: URLSession(configuration: cfg)).handle(
            request("POST", "/v1/messages?beta=true",
                    headers: ["Authorization": "Bearer tok", "anthropic-version": "2023-06-01", "anthropic-beta": "b1", "x-secret": "no"],
                    body: #"{"model":"or-qwen","stream":true,"tools":[{"name":"t"}],"messages":[]}"#),
            writer: w)

        let up = try #require(StubUpstream.lastRequest)
        #expect(up.url?.absoluteString == "https://up.example/api/v1/messages?beta=true")
        #expect(up.value(forHTTPHeaderField: "authorization") == "Bearer or-key")
        #expect(up.value(forHTTPHeaderField: "anthropic-version") == "2023-06-01")
        #expect(up.value(forHTTPHeaderField: "anthropic-beta") == "b1")
        #expect(up.value(forHTTPHeaderField: "x-secret") == nil)
        let sent = try #require(JSONSerialization.jsonObject(with: StubUpstream.lastBody) as? [String: Any])
        #expect(sent["model"] as? String == "qwen/qwen3-coder")
        #expect((sent["tools"] as? [Any])?.count == 1)

        #expect(w.status == 200 && w.closed)
        #expect(w.text.contains("Content-Type: text/event-stream"))
        #expect(w.text.hasSuffix("event: message_stop\ndata: {}\n\n"))
    }

    @Test func passthroughForwardsOnlyBetaQuery() {
        #expect(Router.betaQuery("beta=true") == "?beta=true")
        #expect(Router.betaQuery("evil=1&beta=true") == "?beta=true")
        #expect(Router.betaQuery("evil=1") == "")
        #expect(Router.betaQuery("beta=false") == "")
        #expect(Router.betaQuery(nil) == "")
    }

    @Test func bothAliasAndPublishedIDRouteAndWindowIsEnforced() async throws {
        let cfg = URLSessionConfiguration.ephemeral
        cfg.protocolClasses = [StubUpstream.self]
        let r = makeRouter(session: URLSession(configuration: cfg))
        for id in ["or-qwen", "claude-or-qwen"] {
            let w = FakeWriter()
            await r.handle(request("POST", "/v1/messages", headers: ["x-api-key": "tok"], body: #"{"model":"\#(id)","messages":[]}"#), writer: w)
            #expect(w.status == 200)
        }
        let big = String(repeating: "x", count: 5000)  // ~1250 tokenov > 1k okno
        let w = FakeWriter()
        await r.handle(request("POST", "/v1/messages", headers: ["x-api-key": "tok"],
                               body: #"{"model":"claude-or-qwen","messages":[{"role":"user","content":"\#(big)"}]}"#), writer: w)
        #expect(w.status == 400 && w.text.contains("prompt is too long: ") && w.text.contains("tokens > 1000 maximum"))
    }
}

@Test func fmStripsRemindersAndFitsBudget() async {
    #expect(AppleFM.stripReminders("<system-reminder>\nCLAUDE.md …\n</system-reminder>\nWhat is 2+2?") == "What is 2+2?")
    // @datoteka: Claude Code pošlje rezultat orodja Read v opomniku → vsebina ostane, brez številk vrstic
    let at = "<system-reminder>\nCalled the Read tool with the following input: {\"file_path\":\"/x/NOTES.md\"}\n</system-reminder>"
    #expect(AppleFM.stripReminders(at) == "Attached file /x/NOTES.md:")
    let result = "<system-reminder>\nResult of calling the Read tool:\n     1\t# Test\n     2\tPORT=8765\n</system-reminder>\n<system-reminder>\nOther\n</system-reminder>\nPovzemi @NOTES.md"
    #expect(AppleFM.stripReminders(result) == "```\n# Test\nPORT=8765\n```\n\nPovzemi @NOTES.md")
    let turns = (1...6).map { ClaudeCLI.Turn(role: $0 % 2 == 1 ? "user" : "assistant", text: String(repeating: "w", count: $0 * 10)) }
    // števec = znaki; navodila 5, zadnje 60 → ostane 35 za zgodovino: turn 5 (50) ne gre več
    var fit = await AppleFM.fit(turns, instructions: "12345", budget: 100) { $0.count }
    #expect(fit.last.count == 60 && fit.truncated)
    #expect(fit.history.isEmpty)  // samo turn 5 (assistant? ne, user 50) presega; prazno
    fit = await AppleFM.fit(turns, instructions: "", budget: 1000) { $0.count }
    #expect(fit.history.count == 5 && !fit.truncated)
    fit = await AppleFM.fit(turns, instructions: "", budget: 60 + 50 + 40) { $0.count }
    #expect(fit.history.map(\.text.count) == [50] && fit.truncated)  // 40 (assistant) na začetku odpade
    fit = await AppleFM.fit([ClaudeCLI.Turn(role: "user", text: String(repeating: "a", count: 300) + "QUESTION")],
                            instructions: "", budget: 100) { $0.count }
    #expect(fit.last.hasSuffix("QUESTION") && fit.last.count <= 100 && fit.truncated)
}

@Test func siriReplyCleaningAndPacking() {
    #expect(Siri.cleanReply("Ljubljana, Maribor in Kranj. end-1d31  Keep in mind, I'm an assistant…") == "Ljubljana, Maribor in Kranj.")
    #expect(Siri.cleanReply("Odgovor\nEND-AB12") == "Odgovor")
    #expect(Siri.cleanReply("Brez oznake ") == "Brez oznake")
    #expect(Siri.cleanReply("legend-abcd ni oznaka") == "legend-abcd ni oznaka")
    let h = [ClaudeCLI.Turn(role: "user", text: "a"), ClaudeCLI.Turn(role: "assistant", text: "b")]
    #expect(Siri.packed(history: [], last: "q") == "q")
    #expect(Siri.packed(history: h, last: "q").hasPrefix("Earlier conversation") && Siri.packed(history: h, last: "q").hasSuffix("---\n\nq"))
    let long = [ClaudeCLI.Turn(role: "user", text: String(repeating: "x", count: 20000))]
    #expect(Siri.packed(history: long, last: "q", limit: 1000).count <= 1100)
}

@Test func claudeExecutableResolution() async {
    #expect(await ClaudeCLI.executable("/nonexistent/claude") == nil)  // ročna pot nima rezerve
    #expect(await ClaudeCLI.executable("/bin/ls") == "/bin/ls")
}

@Test func claudeExecutableValidation() {
    if case .failure(let e) = ClaudeCLI.validate("/nonexistent") { #expect(e.message == "File does not exist") } else { Issue.record("expected failure") }
    if case .failure(let e) = ClaudeCLI.validate("/etc/hosts") { #expect(e.message == "File is not executable") } else { Issue.record("expected failure") }
    if case .failure(let e) = ClaudeCLI.validate("/bin/ls") { #expect(e.message == "Not a Claude Code executable") } else { Issue.record("expected failure") }
    if let real = ClaudeCLI.detected, case .success(let v) = ClaudeCLI.validate(real) { #expect(v.contains("Claude Code")) }
}

@Test func firstOfDoesNotWaitForSlowTask() async {
    // Počasna naloga 60 s, meja 30 s: pod obremenitvijo nadaljevanje testa zamudi, a še vedno ne čaka počasne.
    let slow = Task { try? await Task.sleep(for: .seconds(60)); return 1 }
    defer { slow.cancel() }
    let t0 = ContinuousClock.now
    #expect(await firstOf(slow, within: .milliseconds(100)) == nil)
    #expect(ContinuousClock.now - t0 < .seconds(30))
    #expect(await firstOf(Task { 7 }, within: .seconds(1)) == 7)
}

/// Agenti (grok, codex, copilot) tečejo vso sejo: njihovo branje ne sme zasesti bazena Swift concurrency (ena nit na jedro),
/// sicer router pri več hkratnih agentih kot jeder ne odgovori niti na /health.
@Test func manyAgentRunsDoNotStallHealth() async throws {
    let dir = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("pool-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: dir) }
    let fake = dir.appendingPathComponent("agent"), gate = dir.appendingPathComponent("go")
    try Data("#!/bin/sh\nwhile [ ! -e '\(gate.path)' ]; do sleep 0.1; done\n".utf8).write(to: fake)  // teče, dokler test ne reče
    chmod(fake.path, 0o755)
    let n = 2 * ProcessInfo.processInfo.activeProcessorCount + 2
    let runs = (0..<n).map { i in
        Task { _ = try? await (i.isMultiple(of: 2) ? Grok.run(fake.path, args: [], cwd: dir, prompt: "x", onStep: { _ in })
                                                   : Codex.run(fake.path, args: [], cwd: dir, prompt: "x", output: dir.appendingPathComponent("o\(i)"))) }
    }
    // Agente ustavi nit po 20 s, ne naloga: zasedenega bazena (pred popravkom) ne bi nikoli dočakala.
    Thread { Thread.sleep(forTimeInterval: 20); FileManager.default.createFile(atPath: gate.path, contents: nil) }.start()
    let t0 = ContinuousClock.now  // pred čakanjem: tudi to nadaljevanje potrebuje nit bazena
    try await Task.sleep(for: .milliseconds(500))  // agenti zagnani in v branju
    let r = Router(config: RouterConfig(), token: "tok", secrets: FakeSecrets(values: [:]))
    let w = FakeWriter()
    await r.handle(request("GET", "/health/live"), writer: w)
    #expect(w.status == 200 && ContinuousClock.now - t0 < .seconds(10))  // pred popravkom: šele po koncu agentov (20 s)
    FileManager.default.createFile(atPath: gate.path, contents: nil)
    for t in runs { await t.value }
}

@Test func configRoundTripsAsReadablePlistAndMigratesJSON() throws {
    let d = UserDefaults(suiteName: "test-\(UUID())")!
    var c = RouterConfig()
    c.port = 9999
    c.providers[1].enabledModels = ["a/b"]
    c.siri.enabled = true
    c.save(d)
    let fresh = RouterConfig.load(d)  // N4: shranjene nastavitve brez ID-ja ga dobijo, Scratch ostane prejšnji
    #expect(fresh.instanceID.hasPrefix("gw-") && fresh.scratchName.isEmpty && fresh.scratchURL == c.scratchURL)
    c.instanceID = "gw-test"
    c.save(d)
    #expect(d.object(forKey: "routerConfig") is [String: Any])  // struktura, ne blob
    #expect(RouterConfig.load(d) == c)
    d.set(try JSONEncoder().encode(c), forKey: "routerConfig")  // stara oblika
    #expect(RouterConfig.load(d) == c)
}

@Test func discoveryFailureReasons() async {
    let p = ProviderConfig(id: "x", name: "X", enabled: true, baseURL: "http://127.0.0.1:1", aliases: "")
    if case .failure(let f) = await Discovery.load(p, key: "") { #expect(f.message == "No API key") } else { Issue.record("no key") }
    if case .failure(let f) = await Discovery.load(p, key: "k") { #expect(f.message.hasPrefix("Cannot connect") && f.message.contains("Local Network")) } else { Issue.record("connect") }
    var bad = p; bad.baseURL = "not a url"
    if case .failure(let f) = await Discovery.load(bad, key: "k") { #expect(f.message.hasPrefix("invalid provider URL")) } else { Issue.record("url") }
    #expect(Discovery.isLocal("192.168.1.20") && Discovery.isLocal("mini.local") && !Discovery.isLocal("openrouter.ai"))
}

@Test func webDAVStaysInsideRootAndWrites() throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("davtest-" + UUID().uuidString)
    let root = base.appendingPathComponent("proj"), outside = base.appendingPathComponent("secret.txt")
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    try Data("no".utf8).write(to: outside)
    try FileManager.default.createSymbolicLink(at: root.appendingPathComponent("link"), withDestinationURL: outside)
    defer { try? FileManager.default.removeItem(at: base) }
    func dav(_ m: String, _ p: String, _ h: [String: String] = [:], _ b: String = "") -> DAVResponse {
        WebDAV.handle(method: m, path: "/dav/s" + p, headers: h, body: Data(b.utf8), root: root, prefix: "/dav/s")
    }
    // Ven iz mape: .., kodiran .., simbolna povezava, napačna predpona.
    #expect(dav("GET", "/../secret.txt").status == 403)
    #expect(dav("GET", "/%2E%2E/secret.txt").status == 403)
    #expect(dav("GET", "/link").status == 403)
    #expect(WebDAV.handle(method: "GET", path: "/x/a", headers: [:], body: Data(), root: root, prefix: "/dav/s").status == 403)
    #expect(mkfifo(root.appendingPathComponent("fifo").path, 0o600) == 0)
    #expect(dav("GET", "/fifo").status == 403)  // L3: FIFO ne zadrži delavca

    #expect(dav("PUT", "/a%20b.txt", [:], "hello").status == 201)
    #expect(dav("MOVE", "/a%20b.txt", ["destination": "http://127.0.0.1:8765/dav/s/../moved"]).status == 403)
    #expect(String(decoding: dav("GET", "/a%20b.txt").body, as: UTF8.self) == "hello")
    #expect(String(decoding: dav("GET", "/a%20b.txt", ["range": "bytes=1-2"]).body, as: UTF8.self) == "el")
    #expect(dav("MKCOL", "/dir").status == 201)
    #expect(dav("MOVE", "/a%20b.txt", ["destination": "http://127.0.0.1:8765/dav/s/dir/c.txt"]).status == 201)
    let list = String(decoding: dav("PROPFIND", "/dir", ["depth": "1"]).body, as: UTF8.self)
    #expect(list.contains("<D:href>/dav/s/dir/c.txt</D:href>") && list.contains("<D:getcontentlength>5<"))
    #expect(dav("PUT", "/._c.txt", [:], "xattr").status == 201)  // AppleDouble se ne zapiše
    #expect(!FileManager.default.fileExists(atPath: root.appendingPathComponent("._c.txt").path))
    #expect(dav("DELETE", "/dir").status == 204)
    #expect(dav("DELETE", "/").status == 403)
}

@Test func shareSessionCannotBeTakenOver() async {
    let hub = ShareHub()
    let key = await hub.register(session: "s1", cwd: "/a", key: nil)
    #expect(key != nil)
    #expect(await hub.register(session: "s1", cwd: "/b", key: nil) == nil)          // tuj odjemalec
    #expect(await hub.register(session: "s1", cwd: "/a", key: "wrong") == nil)
    #expect(await hub.register(session: "s1", cwd: "/a", key: key) == key)          // lastnik
    #expect(await hub.mount(session: "../../x", cwd: nil, base: "http://127.0.0.1:1") == nil)
}

@Test func noiseHandshakeAndFrames() throws {
    let key = Noise.newKey()
    var i = Noise.Initiator(routerKey: key.publicKey.rawRepresentation)
    let m1 = try i.first()
    #expect(m1.count == Noise.handshakeLength)
    let (m2, server) = try Noise.respond(m1, key: key)
    let client = try i.finish(m2)

    let big = Data((0..<200_000).map { UInt8($0 % 251) })  // več okvirjev
    var wire = Noise.frames(big, client.send)
    #expect(try Noise.unframe(&wire, server.receive) == big && wire.isEmpty)
    var back = Noise.frames(Data("ok".utf8), server.send)
    var partial = back.prefix(5)                               // nepopoln okvir počaka
    #expect(try Noise.unframe(&partial, client.receive).isEmpty && partial.count == 5)
    #expect(try Noise.unframe(&back, client.receive) == Data("ok".utf8))

    var tampered = Noise.frames(Data("x".utf8), client.send)
    tampered[tampered.count - 1] ^= 1
    #expect(throws: (any Error).self) { try Noise.unframe(&tampered, server.receive) }

    // Odjemalec z napačnim ključem routerja se ne poveže.
    var wrong = Noise.Initiator(routerKey: Noise.newKey().publicKey.rawRepresentation)
    #expect(throws: (any Error).self) { try Noise.respond(try wrong.first(), key: key) }
}

/// Prost port TCP na 127.0.0.1 (#18: stalen port 18765 je bil včasih zaseden).
func freeTCPPort() -> UInt16 {
    let fd = socket(AF_INET, SOCK_STREAM, 0)
    defer { close(fd) }
    var a = sockaddr_in(sin_len: UInt8(MemoryLayout<sockaddr_in>.size), sin_family: sa_family_t(AF_INET), sin_port: 0,
                        sin_addr: in_addr(s_addr: inet_addr("127.0.0.1")), sin_zero: (0, 0, 0, 0, 0, 0, 0, 0))
    var len = socklen_t(MemoryLayout<sockaddr_in>.size)
    _ = withUnsafeMutablePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, len) } }
    _ = withUnsafeMutablePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(fd, $0, &len) } }
    return UInt16(bigEndian: a.sin_port)
}

/// Ali na portu 127.0.0.1 nekdo posluša.
func portInUse(_ port: UInt16) -> Bool {
    let fd = socket(AF_INET, SOCK_STREAM, 0)
    defer { close(fd) }
    var a = sockaddr_in(sin_len: UInt8(MemoryLayout<sockaddr_in>.size), sin_family: sa_family_t(AF_INET), sin_port: port.bigEndian,
                        sin_addr: in_addr(s_addr: inet_addr("127.0.0.1")), sin_zero: (0, 0, 0, 0, 0, 0, 0, 0))
    return withUnsafeMutablePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } } == 0
}

@Test func encryptedProxyToRouter() async throws {
    var c = RouterConfig()
    let routerPort = freeTCPPort(), routerURL = "http://127.0.0.1:\(routerPort)"
    c.host = "127.0.0.1"; c.port = routerPort
    let key = Noise.newKey()
    let seen = SeenDevices()
    let server = RouterServer(router: Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]),
                                             onDevice: { d in seen.add(d) }), key: key)
    try server.start()
    defer { server.stop() }
    for _ in 0..<100 where !portInUse(routerPort) { try await Task.sleep(for: .milliseconds(50)) }  // namesto fiksnega čakanja

    func get(_ base: String, _ path: String) async -> (Int, String) {
        // 30 s, ne 5: pod obremenitvijo (vzporedni testi, druge gradnje) router odgovori pozneje; preverja se pot, ne hitrost.
        var r = URLRequest(url: URL(string: base + path)!, timeoutInterval: 30)
        r.setValue("Bearer tok", forHTTPHeaderField: "authorization")
        guard let (d, resp) = try? await URLSession.shared.data(for: r) else { return (0, "") }
        return ((resp as! HTTPURLResponse).statusCode, String(decoding: d, as: UTF8.self))
    }
    let proxy = try #require(NoiseProxy(routerURL: routerURL, routerKey: key.publicKey.rawRepresentation))
    let stableTaken = portInUse(proxy.stablePort)  // drug program na stalnem portu: proxy vzame naključnega
    try proxy.start()
    defer { proxy.stop() }
    #expect(proxy.localPort == proxy.stablePort || stableTaken)  // stalen URL → Claude Code obdrži modele routerja
    let (s, body) = await get(proxy.localURL, "/v1/models")
    #expect(s == 200 && body.contains("\"data\""))

    #expect(seen.all.isEmpty)  // proxy brez ključa naprave (starejši minigw) deluje, le brez naprave

    let dev = Curve25519.Signing.PrivateKey()
    let withDevice = try #require(NoiseProxy(routerURL: routerURL, routerKey: key.publicKey.rawRepresentation,
                                             device: (key: dev, name: "Test Air"), suite: .aesGCM))  // AESGCM kot minigw.exe
    try withDevice.start()
    defer { withDevice.stop() }
    #expect(await get(withDevice.localURL, "/v1/models").0 == 200)
    #expect(seen.all == [Noise.Device(id: Noise.base64url(dev.publicKey.rawRepresentation), name: "Test Air")])

    let bad = try #require(NoiseProxy(routerURL: routerURL, routerKey: Noise.newKey().publicKey.rawRepresentation))
    try bad.start()
    defer { bad.stop() }
    #expect(bad.localPort != proxy.stablePort && bad.localPort != 0)  // port zaseden → naključen
    #expect(await get(bad.localURL, "/v1/models").0 == 0)  // napačen ključ: router prekine
}

/// Drug lokalni program ne more poslušati na vratih routerja (in prevzeti povezav Claude Code), tudi z SO_REUSEADDR in
/// SO_REUSEPORT, na 127.0.0.1 ali 0.0.0.0: allowLocalEndpointReuse (hiter ponovni zagon) ne odpre deljenja vrat.
@Test func routerPortCannotBeSharedByAnotherListener() async throws {
    var c = RouterConfig()
    let port = freeTCPPort()
    c.host = "127.0.0.1"; c.port = port
    let server = RouterServer(router: Router(config: c, token: "tok", secrets: FakeSecrets(values: [:])))
    try server.start()
    defer { server.stop() }
    for _ in 0..<100 where !portInUse(port) { try await Task.sleep(for: .milliseconds(50)) }
    #expect(portInUse(port))
    for host in ["127.0.0.1", "0.0.0.0"] {
        for opts in [[SO_REUSEADDR], [SO_REUSEPORT], [SO_REUSEADDR, SO_REUSEPORT]] {
            let fd = socket(AF_INET, SOCK_STREAM, 0)
            defer { close(fd) }
            var one: Int32 = 1
            for o in opts { setsockopt(fd, SOL_SOCKET, o, &one, socklen_t(MemoryLayout<Int32>.size)) }
            var a = sockaddr_in(sin_len: UInt8(MemoryLayout<sockaddr_in>.size), sin_family: sa_family_t(AF_INET), sin_port: port.bigEndian,
                                sin_addr: in_addr(s_addr: inet_addr(host)), sin_zero: (0, 0, 0, 0, 0, 0, 0, 0))
            let bound = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
            #expect(bound != 0 && errno == EADDRINUSE, "\(host) \(opts)")
        }
    }
}

@Test func availableToolsFromInstalledClaude() async throws {
    try #require(ClaudeCLI.detected != nil)  // brez nameščenega Claude Code ni česa preveriti
    let tools = await ClaudeCLI.availableTools("")
    #expect(tools.contains("Bash") && tools.contains("Edit") && tools.contains("Read"))
}

final class SeenDevices: @unchecked Sendable {
    private let lock = NSLock()
    private var list: [Noise.Device] = []
    func add(_ d: Noise.Device) { lock.withLock { list.append(d) } }
    var all: [Noise.Device] { lock.withLock { list.reduce(into: []) { r, d in if !r.contains(d) { r.append(d) } } } }  // brez ponovitev
}

@Test func deviceProofBoundToConnection() throws {
    let key = Curve25519.Signing.PrivateKey(), h = Data(repeating: 7, count: 32)
    var good = Noise.deviceProof(key, name: "Air", hash: h) + Data("GET / HTTP/1.1\r\n".utf8)
    #expect(Noise.readDevice(&good, hash: h) == .device(Noise.Device(id: Noise.base64url(key.publicKey.rawRepresentation), name: "Air")))
    #expect(good == Data("GET / HTTP/1.1\r\n".utf8))  // ostanek je HTTP
    var replay = Noise.deviceProof(key, name: "Air", hash: h)
    #expect(Noise.readDevice(&replay, hash: Data(repeating: 8, count: 32)) == .invalid)  // druga povezava
    var partial = Data(Noise.deviceProof(key, name: "Air", hash: h).prefix(50))
    #expect(Noise.readDevice(&partial, hash: h) == .needMore)
    var http = Data("GET /v1/models HTTP/1.1\r\n".utf8)
    #expect(Noise.readDevice(&http, hash: h) == .absent)
    var empty = Data()
    #expect(Noise.readDevice(&empty, hash: h) == .needMore)

    var c = RouterConfig()
    let d = Noise.Device(id: "abc", name: "Air")
    #expect(c.siriAllowed(peer: "127.0.0.1", device: nil))       // lokalno vedno
    #expect(!c.siriAllowed(peer: "10.0.0.5", device: d))         // nova naprava: ne
    #expect(!c.siriAllowed(peer: "10.0.0.5", device: nil))       // brez dokaza: ne
    c.devices = [DeviceConfig(id: "abc", name: "Air", siri: true)]
    #expect(c.siriAllowed(peer: "10.0.0.5", device: d))
}

// #59: vsaka nadmapa cwd (tudi /private/… brez simbolne povezave) je za zagon »untrusted« (projektne nastavitve izklopljene).
@Test func codexProjectsUntrusted() throws {
    let root = URL(fileURLWithPath: "/tmp/minigw-trust-" + UUID().uuidString), d = root.appendingPathComponent("s")
    try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: root) }
    for a in [Codex.args(cwd: d, extraDirs: [], agent: true, output: d, resume: "t1"), Codex.args(cwd: d, extraDirs: [], agent: false, output: d)] {
        let i = try #require(a.firstIndex { $0.hasPrefix("projects={") })
        #expect(a[i - 1] == "-c" && i < (a.firstIndex(of: "resume") ?? a.count))
        for k in ["/", "/tmp", root.path, d.path, "/private" + d.path] { #expect(a[i].contains("\"\(k)\"={trust_level=\"untrusted\"}")) }
    }
}

@Test func codexArgsSandboxed() {
    let out = URL(fileURLWithPath: "/tmp/o.txt"), cwd = URL(fileURLWithPath: "/tmp/s")
    let a = Codex.args(cwd: cwd, extraDirs: ["/tmp/x"], agent: true, output: out)
    #expect(a.starts(with: ["exec"]) && a.last == "-")
    #expect(a.contains("workspace-write") && !a.contains("danger-full-access") && !a.contains("--dangerously-bypass-approvals-and-sandbox"))
    #expect(a.contains("--add-dir") && a.contains("/tmp/x") && a.contains("/tmp/s"))
    #expect(Codex.args(cwd: cwd, extraDirs: [], agent: false, output: out).contains("read-only"))
    #expect(Codex.args(cwd: cwd, extraDirs: [], agent: false, output: out).contains("--ephemeral"))
    #expect(Codex.args(cwd: cwd, extraDirs: [], agent: true, output: out, model: "gpt-6-luna").contains("gpt-6-luna"))
    #expect(Codex.match("claude-codex")! == nil && Codex.match("claude-codex-gpt-6-luna")! == "gpt-6-luna")
    let r = Codex.args(cwd: cwd, extraDirs: [], agent: true, output: out, resume: "t1")
    #expect(Array(r.suffix(3)) == ["resume", "t1", "-"] && r.contains("workspace-write") && !r.contains("--ephemeral"))
    #expect(Codex.threadId(Data(#"{"type":"thread.started","thread_id":"t1"}"#.utf8)) == "t1")
    #expect(Codex.threadId(Data(#"{"type":"turn.started"}"#.utf8)) == nil)
    #expect(Codex.match("claude-codex-") == nil && Codex.match("claude-opus-5-5") == nil)
    #expect(Codex.args(cwd: cwd, extraDirs: ["/tmp/x"], agent: true, output: out, write: false).contains("read-only"))
    #expect(Codex.args(cwd: cwd, extraDirs: [], agent: true, output: out, write: false, resume: "t1").contains(#"sandbox_mode="read-only""#))
    #expect(Codex.args(cwd: cwd, extraDirs: [], agent: true, output: out, network: false).contains("sandbox_workspace_write.network_access=false"))
    var cx = CodexConfig()
    #expect(cx.isModelEnabled("a"))
    cx.setModel("a", false, all: ["a", "b"])
    #expect(!cx.isModelEnabled("a") && cx.isModelEnabled("b"))
    cx.setModel("a", true, all: ["a", "b"])
    #expect(cx.enabledModels == nil)  // vsi vklopljeni → tudi prihodnji modeli
}

@Test func mentionedFilesFoundAndAttached() async throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent("mentions-\(UUID().uuidString)")
    let fm = FileManager.default
    try fm.createDirectory(at: root.appendingPathComponent("docs/kb"), withIntermediateDirectories: true)
    try fm.createDirectory(at: root.appendingPathComponent("node_modules/x"), withIntermediateDirectories: true)
    defer { try? fm.removeItem(at: root) }
    try "# Projekt\nPORT=1".write(to: root.appendingPathComponent("CLAUDE.md"), atomically: true, encoding: .utf8)
    try "kb note".write(to: root.appendingPathComponent("docs/kb/arch.md"), atomically: true, encoding: .utf8)
    try "skip".write(to: root.appendingPathComponent("node_modules/x/CLAUDE.md"), atomically: true, encoding: .utf8)
    try Data([0, 1, 2]).write(to: root.appendingPathComponent("docs/kb/blob.bin"))

    let names = await Mentions.names(in: "povzemi ./CLAUDE.md prosim")
    #expect(names.contains("./CLAUDE.md"))  // brez Apple FM: ime s končnico
    #expect(Mentions.find([], question: "kaj piše v zapiskih v kb?", in: root).map(\.lastPathComponent) == ["kb"])  // cela beseda
    let hits = Mentions.find(["CLAUDE.md", "kb"], in: root)
    #expect(hits.map(\.lastPathComponent).sorted() == ["CLAUDE.md", "kb"])  // node_modules preskočen
    let text = Mentions.attachments(hits, root: root)
    #expect(text.contains("Attached file CLAUDE.md:") && text.contains("PORT=1") && text.contains("Attached file docs/kb/arch.md")
            && text.contains("- docs/kb/blob.bin") && !text.contains("Attached file docs/kb/blob.bin"))  // binarno le v seznamu
    // mapa le s podmapo (docs → kb/…): vsebina iz podmap, navodilo zraven
    let docs = Mentions.attachments(Mentions.find([], question: "povzemi vsebino mape ./docs", in: root), root: root)
    #expect(docs.hasPrefix(Mentions.header) && docs.contains("Folder docs contains:") && docs.contains("kb note"))
}

@Test func agentStepsForActivity() {
    #expect(Codex.step(Data(#"{"type":"item.started","item":{"type":"command_execution","command":"/bin/zsh -lc 'cat a.txt'","status":"in_progress"}}"#.utf8)) == "$ cat a.txt")
    #expect(Codex.step(Data(#"{"type":"item.completed","item":{"type":"agent_message","text":"Reading docs\nmore"}}"#.utf8)) == nil)  // besedilo agenta ne v Activity
    #expect(Codex.step(Data(#"{"type":"turn.started"}"#.utf8)) == nil)
    var t = StreamTranslator(model: "m")
    _ = t.feed(Data(#"{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"docs/x.md"}}]}}"#.utf8))
    #expect(t.step == "Read docs/x.md")
}

@Test func noiseSuitesInteroperate() throws {
    let key = Noise.newKey()
    for suite in Noise.Suite.allCases {
        var i = Noise.Initiator(routerKey: key.publicKey.rawRepresentation, suite: suite)
        let m1 = try i.first()
        let (m2, server) = try Noise.respond(m1, key: key, suite: suite)
        let client = try i.finish(m2)
        #expect(client.hash == server.hash)
        var buf = Noise.frames(Data("hello \(suite)".utf8), client.send)
        #expect(try Noise.unframe(&buf, server.receive) == Data("hello \(suite)".utf8))
    }
    // napačna šifra na strežniku → rokovanje ne uspe (ime protokola je v hashu)
    var i = Noise.Initiator(routerKey: key.publicKey.rawRepresentation, suite: .aesGCM)
    #expect(throws: (any Error).self) { try Noise.respond(try i.first(), key: key, suite: .chachaPoly) }
}

@Test func grokArgsAndEvents() {
    let cwd = URL(fileURLWithPath: "/tmp/s")
    let w = Grok.args(cwd: cwd, extraDirs: ["/tmp/x"], agent: true, model: "grok-4.7", write: true)
    #expect(w.contains("workspace") && w.contains("--always-approve") && !w.contains("--add-dir") && w.contains("grok-4.7"))
    let r = Grok.args(cwd: cwd, extraDirs: ["/tmp/x"], agent: true, model: nil, write: false)
    #expect(r.contains("read-only") && !r.contains("--always-approve") && !r.contains("/tmp/x"))
    #expect(Grok.match("claude-grok")! == nil && Grok.match("claude-grok-grok-4.7")! == "grok-4.7" && Grok.match("claude-codex") == nil)
    #expect(Grok.event(Data(#"{"type":"tool_call","title":"read_file","rawInput":{"target_file":"a.txt"}}"#.utf8)) == .step("read_file a.txt"))
    #expect(Grok.event(Data(#"{"type":"text","data":"hello"}"#.utf8)) == .text("hello"))
    #expect(Grok.event(Data(#"{"type":"end","stopReason":"end_turn","sessionId":"s1"}"#.utf8)) == .end("s1"))
}

@Test func davGetReadsOnlyTheRangeAndLimitsSize() throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent("dav-\(UUID().uuidString)")
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: root) }
    try Data("0123456789".utf8).write(to: root.appendingPathComponent("a.txt"))
    try Data().write(to: root.appendingPathComponent("empty"))
    FileManager.default.createFile(atPath: root.appendingPathComponent("big").path, contents: nil)
    let big = try FileHandle(forWritingTo: root.appendingPathComponent("big"))
    try big.truncate(atOffset: UInt64(WebDAV.maxServe + 1)); try big.close()
    func get(_ m: String, _ p: String, _ h: [String: String] = [:]) -> DAVResponse {
        WebDAV.handle(method: m, path: "/dav/s/" + p, headers: h, body: Data(), root: root, prefix: "/dav/s")
    }
    let r = get("GET", "a.txt", ["range": "bytes=2-4"])
    #expect(r.status == 206 && String(decoding: r.body, as: UTF8.self) == "234")
    #expect(get("GET", "empty").status == 200 && get("GET", "empty").body.isEmpty)
    #expect(get("HEAD", "big").status == 200 && get("HEAD", "big").headers["Content-Length"] == "\(WebDAV.maxServe + 1)")
    #expect(get("GET", "big").status == 413)
    #expect(get("GET", "big", ["range": "bytes=0-99"]).body.count == 100)
}

@Test func scratchFolderMovesAndFallsBack() throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("scratch-\(UUID().uuidString)")
    defer { try? FileManager.default.removeItem(at: base) }
    var c = RouterConfig()
    c.scratchFolder = base.appendingPathComponent("a").path
    #expect(c.scratchURL.path == base.appendingPathComponent("a/Mini Gateway Scratch").path)  // lastna podmapa
    let chat = c.scratch("chat")
    #expect(chat.path.hasPrefix(c.scratchURL.path))
    try Data("x".utf8).write(to: chat.appendingPathComponent("f.txt"))
    // Izbrana mapa z uporabnikovimi datotekami: premik ne briše ničesar, le doda svojo podmapo.
    let chosen = base.appendingPathComponent("b")
    try FileManager.default.createDirectory(at: chosen.appendingPathComponent("chat"), withIntermediateDirectories: true)
    try Data("user".utf8).write(to: chosen.appendingPathComponent("chat/mine.txt"))
    let new = RouterConfig.dedicated(chosen)
    #expect(RouterConfig.dedicated(new) == new)                      // že pravo ime → brez nove podmape
    try RouterConfig.moveScratch(from: c.scratchURL, to: new)
    #expect(FileManager.default.fileExists(atPath: new.appendingPathComponent("chat/f.txt").path))
    #expect(FileManager.default.fileExists(atPath: chosen.appendingPathComponent("chat/mine.txt").path))
    #expect(!FileManager.default.fileExists(atPath: c.scratchURL.path))  // prazna stara mapa odstranjena
    // Cilj že ima podmapo: napaka, nič se ne premakne ali izbriše.
    let other = base.appendingPathComponent("c/Mini Gateway Scratch")
    try FileManager.default.createDirectory(at: other.appendingPathComponent("chat"), withIntermediateDirectories: true)
    try Data("keep".utf8).write(to: other.appendingPathComponent("chat/keep.txt"))
    #expect(throws: (any Error).self) { try RouterConfig.moveScratch(from: new, to: other) }
    #expect(FileManager.default.fileExists(atPath: other.appendingPathComponent("chat/keep.txt").path))
    #expect(FileManager.default.fileExists(atPath: new.appendingPathComponent("chat/f.txt").path))
    // Neznane datoteke v stari mapi ostanejo, z njimi tudi stara mapa.
    try Data("?".utf8).write(to: new.appendingPathComponent("notes.txt"))
    let d = base.appendingPathComponent("d/Mini Gateway Scratch")
    try RouterConfig.moveScratch(from: new, to: d)
    #expect(FileManager.default.fileExists(atPath: new.appendingPathComponent("notes.txt").path))
    #expect(FileManager.default.fileExists(atPath: d.appendingPathComponent("chat/f.txt").path))
    #expect(throws: (any Error).self) { try RouterConfig.moveScratch(from: d, to: d.appendingPathComponent("inside")) }
    c.scratchFolder = "/Volumes/NoSuchDisk-\(UUID().uuidString)/Scratch"  // disk ni priklopljen → privzeta mapa
    #expect(c.scratch("chat").path.hasPrefix(RouterConfig.defaultScratch.path))
}

@Test func syncIgnoreRules() {
    let ig = Sync.Ignore(gitignore: "# komentar\n*.log\n/out\nsrc/gen/\n!keep.log\n")
    #expect(ig.ignored(".git", isDir: true) && ig.ignored("a/node_modules", isDir: true) && ig.ignored("x/.DS_Store", isDir: false))
    #expect(!ig.ignored("build", isDir: false))                       // »build/« le za mape
    #expect(ig.ignored("a/b/debug.log", isDir: false) && ig.ignored("keep.log", isDir: false))  // »!« ni podprt
    #expect(ig.ignored("out", isDir: true) && !ig.ignored("a/out", isDir: true))                // »/« = le koren
    #expect(ig.ignored("src/gen", isDir: true) && !ig.ignored("lib/src/gen", isDir: true))
    #expect(ig.ignored("a.swift.minigw-conflict", isDir: false) && !ig.ignored("main.swift", isDir: false))
}

@Test func syncScanAndPaths() throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("synctest-" + UUID().uuidString)
    let root = base.appendingPathComponent("proj")
    defer { try? FileManager.default.removeItem(at: base) }
    for d in [".git", "node_modules/x", "src"] {
        try FileManager.default.createDirectory(at: root.appendingPathComponent(d), withIntermediateDirectories: true)
    }
    for f in [".git/HEAD", "node_modules/x/i.js", "src/a.swift", "app.log", ".gitignore"] {
        try Data((f == ".gitignore" ? "*.log\n" : f).utf8).write(to: root.appendingPathComponent(f))
    }
    try Data("no".utf8).write(to: base.appendingPathComponent("secret"))
    try FileManager.default.createSymbolicLink(at: root.appendingPathComponent("link"), withDestinationURL: base)
    let (files, skipped) = Sync.scan(root)
    #expect(Set(files.keys) == [".gitignore", "src/a.swift"] && skipped.isEmpty)
    #expect(files["src/a.swift"]?.sha256 == Sync.sha256(Data("src/a.swift".utf8)))
    // Brez ponovnega zgoščevanja, ko sta velikost in čas enaka.
    var fake = files["src/a.swift"]!
    fake.sha256 = "cached"
    #expect(Sync.scan(root, previous: ["src/a.swift": fake]).files["src/a.swift"]?.sha256 == "cached")

    #expect(Sync.resolve("src/a.swift", root: root) != nil && Sync.resolve("src/new/b.txt", root: root) != nil)
    for p in ["../secret", "/etc/passwd", "src/../../secret", "src//a", "a\\b", "", "link/secret", "link/new.txt"] {
        #expect(Sync.resolve(p, root: root) == nil, "\(p)")
    }
}

@Test func syncRoundTripAndConflicts() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("syncrt-" + UUID().uuidString)
    let client = base.appendingPathComponent("client"), copy = base.appendingPathComponent("copy/local/x")
    defer { try? FileManager.default.removeItem(at: base) }
    try FileManager.default.createDirectory(at: client.appendingPathComponent("src"), withIntermediateDirectories: true)
    func put(_ root: URL, _ p: String, _ s: String) throws {
        try FileManager.default.createDirectory(at: root.appendingPathComponent(p).deletingLastPathComponent(), withIntermediateDirectories: true)
        try Data(s.utf8).write(to: root.appendingPathComponent(p))
    }
    func read(_ root: URL, _ p: String) -> String? { (try? Data(contentsOf: root.appendingPathComponent(p))).map { String(decoding: $0, as: UTF8.self) } }
    for p in ["a.txt", "b.txt", "c.txt", "d.txt", "src/m.swift"] { try put(client, p, "v1 " + p) }

    let hub = ShareHub()
    let key = try #require(await hub.register(session: "s", cwd: client.path, key: nil, copy: copy))
    #expect(await hub.isSync(session: "s"))
    // mount → SYNC-PUSH za minigw (long poll); po odgovoru 200 je cwd agenta kopija.
    let mounted = Task { await hub.mount(session: "s", cwd: client.path, base: "http://127.0.0.1:1") }
    guard case .some(.some(let job)) = await hub.poll(key: key, wait: 5) else { Issue.record("no job"); return }
    #expect(job.method == "SYNC-PUSH")

    // minigw: manifest → router pove, kaj potrebuje → datoteke.
    var state = Sync.scan(client).files
    let need = try #require(await hub.syncManifest(key: key, Array(state.values))).need
    #expect(need == ["a.txt", "b.txt", "c.txt", "d.txt", "src/m.swift"])
    #expect(await hub.syncManifest(key: "other", []) == nil)                       // tuj ključ
    #expect(await hub.syncPut(key: key, path: "../evil", data: Data()) == 403)     // ni v manifestu / zunaj
    #expect(await hub.syncPut(key: key, path: "a.txt", data: Data("changed".utf8)) == 409)
    for p in need { #expect(await hub.syncPut(key: key, path: p, data: try Data(contentsOf: client.appendingPathComponent(p))) == 204) }
    #expect(await hub.reply(key: key, DAVReply(id: job.id, response: DAVResponse(200))))
    #expect(await mounted.value == copy)
    #expect(read(copy, "src/m.swift") == "v1 src/m.swift")
    #expect(try #require(await hub.syncManifest(key: key, Array(state.values))).need.isEmpty)  // nič novega

    // Agent v kopiji: spremeni, doda, izbriše, gradi; uporabnik medtem spremeni c.txt in d.txt.
    try put(copy, "a.txt", "agent a"); try put(copy, "c.txt", "agent c"); try put(copy, "new/n.txt", "agent n")
    try put(copy, "build/out.o", "binary")
    try FileManager.default.removeItem(at: copy.appendingPathComponent("b.txt"))
    try FileManager.default.removeItem(at: copy.appendingPathComponent("d.txt"))
    try put(client, "c.txt", "user c"); try put(client, "d.txt", "user d")

    let ch = try #require(await hub.syncChanges(key: key))
    #expect(ch.changed.map(\.path) == ["a.txt", "c.txt", "new/n.txt"] && ch.deleted == ["b.txt", "d.txt"])
    var blobs: [String: Data] = [:]
    for e in ch.changed { blobs[e.path] = await hub.syncGet(key: key, path: e.path) }
    #expect(await hub.syncGet(key: key, path: "build/out.o") == nil)               // prezrto ne gre nazaj
    let (notes, done) = Sync.apply(ch, root: client, state: &state) { blobs[$0] }
    #expect(done == ["b.txt", "d.txt", "a.txt", "c.txt", "new/n.txt"])  // izbrisi najprej
    #expect(await hub.syncAck(key: key, done))
    #expect(read(client, "a.txt") == "agent a" && read(client, "new/n.txt") == "agent n" && read(client, "b.txt") == nil)
    #expect(read(client, "c.txt") == "user c" && read(client, "c.txt.minigw-conflict") == "agent c")  // spor: obe različici
    #expect(read(client, "d.txt") == "user d")                                     // izbris spremenjene ne izbriše
    #expect(notes.count == 2 && notes.allSatisfy { $0.hasPrefix("conflict:") })
    #expect(read(client, "build/out.o") == nil)
    // Naslednji prenos: le uporabnikova c.txt in d.txt (različici agenta sta prepisani/obnovljeni v kopiji).
    let next = Sync.scan(client, previous: state).files
    #expect(try #require(await hub.syncManifest(key: key, Array(next.values))).need == ["c.txt", "d.txt"])
    #expect(Sync.scan(client).files["a.txt"]?.mtime == state["a.txt"]?.mtime)      // isti čas → brez zgoščevanja
}

@Test func syncReadOnlyDoesNotCopyBack() async throws {
    let hub = ShareHub()
    let copy = FileManager.default.temporaryDirectory.appendingPathComponent("syncro-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: copy) }
    let key = try #require(await hub.register(session: "r", cwd: "/a", key: nil, readOnly: true, copy: copy))
    #expect(await hub.syncChanges(key: key) == nil)
    await hub.syncBack(session: "r")  // brez posla za minigw; se takoj vrne
}

@Test func syncRoutes() async throws {
    let scratch = FileManager.default.temporaryDirectory.appendingPathComponent("syncroutes-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: scratch) }
    var c = RouterConfig()
    c.scratchFolder = scratch.path
    let r = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    func send(_ m: String, _ p: String, _ h: [String: String] = [:], _ body: String = "") async -> FakeWriter {
        let w = FakeWriter()
        var q = request(m, p, headers: ["Authorization": "Bearer tok"].merging(h) { $1 }, body: body)
        q.peer = "127.0.0.1"  // lokalni minigw brez naprave → kopija »local«
        await r.handle(q, writer: w)
        return w
    }
    let session = UUID().uuidString, cwd = "/Users/x/proj-" + session
    let reg = await send("POST", "/minigw/share", ["x-claude-mini-session": session, "x-claude-mini-cwd": cwd, "x-claude-mini-work": "sync"])
    let key = try #require(reg.text.split(separator: "\"").dropFirst(3).first.map(String.init))
    let k = ["x-share-key": key]
    let entry = Sync.Entry(path: "dir/a b.txt", size: 2, mtime: 1_700_000_000_000, sha256: Sync.sha256(Data("hi".utf8)))
    let manifest = String(decoding: try JSONEncoder().encode([entry]), as: UTF8.self)
    #expect(await send("POST", "/minigw/sync/manifest", ["x-share-key": "nope"], manifest).status == 404)
    #expect(await send("POST", "/minigw/sync/manifest", k, manifest).text.contains("a b.txt"))
    #expect(await send("PUT", "/minigw/sync/file", k.merging(["x-sync-path": "..%2Fx"]) { $1 }, "hi").status == 403)
    #expect(await send("PUT", "/minigw/sync/file", k.merging(["x-sync-path": Sync.encode(entry.path)]) { $1 }, "hi").status == 204)
    let file = c.scratchURL.appendingPathComponent("sync/local/\(Sync.folderID(cwd))/dir/a b.txt")
    #expect((try? String(contentsOf: file, encoding: .utf8)) == "hi")
    try Data("agent".utf8).write(to: file)
    let ch = await send("GET", "/minigw/sync/changes", k)
    #expect(ch.status == 200 && ch.text.contains("\"changed\":[{"))
    #expect(await send("GET", "/minigw/sync/file", k.merging(["x-sync-path": Sync.encode(entry.path)]) { $1 }).text.hasSuffix("agent"))
    _ = await send("DELETE", "/minigw/share", k)
}

/// Ponovno izvedeno: nepotrjene spremembe agenta, prezrte/prevelike datoteke, poti, prezrte pri odjemalcu, način datotek.
@Test func syncKeepsUnackedAgentWorkAndIgnoredFiles() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("synckeep-" + UUID().uuidString)
    let client = base.appendingPathComponent("client"), copy = base.appendingPathComponent("copy")
    defer { try? FileManager.default.removeItem(at: base) }
    func put(_ root: URL, _ p: String, _ s: String) throws {
        try FileManager.default.createDirectory(at: root.appendingPathComponent(p).deletingLastPathComponent(), withIntermediateDirectories: true)
        try Data(s.utf8).write(to: root.appendingPathComponent(p))
    }
    try FileManager.default.createDirectory(at: client, withIntermediateDirectories: true)
    try put(client, "a.txt", "v1"); try put(client, ".env", "secret"); try put(client, "run.sh", "#!/bin/sh")
    try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: client.appendingPathComponent("run.sh").path)

    let hub = ShareHub()
    let key = try #require(await hub.register(session: "k", cwd: client.path, key: nil, copy: copy))
    var state = Sync.scan(client).files
    #expect(state["run.sh"]?.exec == true && state["a.txt"]?.exec == false)
    for p in try #require(await hub.syncManifest(key: key, Array(state.values))).need {
        #expect(await hub.syncPut(key: key, path: p, data: try Data(contentsOf: client.appendingPathComponent(p))) == 204)
    }
    let mode = try FileManager.default.attributesOfItem(atPath: copy.appendingPathComponent("run.sh").path)[.posixPermissions] as? NSNumber
    #expect(mode?.intValue == 0o755)                                                 // bit x v kopiji

    // Agent: prezre .env (ostane v kopiji), spremeni a.txt, zapiše v .git odjemalca in v node_modules.
    try put(copy, ".gitignore", ".env\n"); try put(copy, "a.txt", "agent"); try put(copy, "x/.git/config", "evil")
    let ch = try #require(await hub.syncChanges(key: key))
    #expect(ch.deleted.isEmpty)                                                      // .env ni izbrisan pri odjemalcu
    #expect(ch.changed.map(\.path) == [".gitignore", "a.txt"])

    // Pot, prezrta pri odjemalcu (tudi nadrejena mapa), se ne zapiše.
    var fake = ch
    fake.changed.append(Sync.Entry(path: "node_modules/p/i.js", size: 1, mtime: 1, sha256: Sync.sha256(Data("x".utf8))))
    let (_, done) = Sync.apply(fake, root: client, state: &state) { p in p == "node_modules/p/i.js" ? Data("x".utf8) : nil }
    #expect(!FileManager.default.fileExists(atPath: client.appendingPathComponent("node_modules").path))
    #expect(done == ["node_modules/p/i.js"])                                         // prezrta je potrjena, neuspeli prenosi ne
    #expect((try? String(contentsOf: client.appendingPathComponent(".env"), encoding: .utf8)) == "secret")

    // Prevzem ni uspel (brez potrditve): naslednji prenos sprememb agenta ne prepiše.
    let r = try #require(await hub.syncManifest(key: key, Array(Sync.scan(client, previous: state).files.values)))
    #expect(r.kept == [".gitignore", "a.txt"] && r.need == [".env"])  // .env skrije nova .gitignore kopije
    #expect((try? String(contentsOf: copy.appendingPathComponent("a.txt"), encoding: .utf8)) == "agent")
    // Ponovni prevzem jih pošlje znova; po potrditvi prenos spet velja za odjemalca.
    let again = try #require(await hub.syncChanges(key: key))
    #expect(again.changed.map(\.path) == [".gitignore", "a.txt"])
    #expect(await hub.syncAck(key: key, again.changed.map(\.path)))
    #expect(try #require(await hub.syncManifest(key: key, Array(state.values))).kept.isEmpty)
}

/// minigw za teste: odgovarja na SYNC-PUSH in SYNC-PULL kot ShareClient (zaporedno, z istim stanjem).
final class FakeMinigw: @unchecked Sendable {
    let hub: ShareHub, key: String, root: URL
    var state: [String: Sync.Entry] = [:], notes: [String] = []
    var failGet: Set<String> = []  // prenos teh poti ne uspe (brez potrditve)
    var refusePull = false  // #82: SYNC-PULL odgovori 503 (mapo drži druga seja), nič ne prevzame
    init(_ hub: ShareHub, key: String, root: URL) { self.hub = hub; self.key = key; self.root = root }

    func serve() -> Task<Void, Never> {
        Task {
            while !Task.isCancelled {
                guard case .some(.some(let job)) = await hub.poll(key: key, wait: 1) else { continue }
                if job.method == "SYNC-PUSH" {
                    let files = Sync.scan(root, previous: state).files
                    for p in await hub.syncManifest(key: key, Array(files.values))?.need ?? [] {
                        _ = await hub.syncPut(key: key, path: p, data: (try? Data(contentsOf: root.appendingPathComponent(p))) ?? Data())
                    }
                    state = files
                } else if refusePull {
                    _ = await hub.reply(key: key, DAVReply(id: job.id, response: DAVResponse(503)))
                    continue
                } else if let ch = await hub.syncChanges(key: key) {
                    var blobs: [String: Data] = [:]
                    for e in ch.changed where !failGet.contains(e.path) { blobs[e.path] = await hub.syncGet(key: key, path: e.path) }
                    let (n, done) = Sync.apply(ch, root: root, state: &state) { blobs[$0] }
                    notes += n
                    _ = await hub.syncAck(key: key, done)
                }
                _ = await hub.reply(key: key, DAVReply(id: job.id, response: DAVResponse(200)))
            }
        }
    }
}

/// Dva agenta v isti mapi hkrati, vsak v svojem klonu: različni datoteki prideta obe, ista datoteka → spor pri drugem.
@Test func syncParallelRunsInOneFolder() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("syncpar-" + UUID().uuidString)
    let client = base.appendingPathComponent("client"), copy = base.appendingPathComponent("sync/local/x")
    let runsDir = base.appendingPathComponent("sync/runs")
    defer { try? FileManager.default.removeItem(at: base) }
    func put(_ root: URL, _ p: String, _ s: String) throws {
        try FileManager.default.createDirectory(at: root.appendingPathComponent(p).deletingLastPathComponent(), withIntermediateDirectories: true)
        try Data(s.utf8).write(to: root.appendingPathComponent(p))
    }
    func read(_ root: URL, _ p: String) -> String? { (try? Data(contentsOf: root.appendingPathComponent(p))).map { String(decoding: $0, as: UTF8.self) } }
    try FileManager.default.createDirectory(at: client, withIntermediateDirectories: true)
    for p in ["a.txt", "b.txt", "c.txt", "d.txt"] { try put(client, p, "v1") }

    let hub = ShareHub()
    let key = try #require(await hub.register(session: "p", cwd: client.path, key: nil, copy: copy))
    let minigw = FakeMinigw(hub, key: key, root: client)
    let server = minigw.serve()
    defer { server.cancel() }
    func mount(_ run: String) async -> URL? {
        await ShareHub.$run.withValue(run) { await hub.mount(session: "p", cwd: client.path, base: "") }
    }

    // Oba prenosa pred katerim koli agentom; vsak dobi svoj klon, prvi vedno ».0«.
    let d1 = try #require(await mount("r1")), d2 = try #require(await mount("r2"))
    #expect(d1 == runsDir.appendingPathComponent("local-x.0") && d2 == runsDir.appendingPathComponent("local-x.1"))
    #expect(read(d1, "a.txt") == "v1" && read(d2, "a.txt") == "v1")
    try put(d1, "a.txt", "agent1"); try put(d2, "b.txt", "agent2")               // različni datoteki
    try put(d1, "c.txt", "one"); try put(d2, "c.txt", "two")                     // ista datoteka
    try FileManager.default.removeItem(at: d1.appendingPathComponent("d.txt")); try put(d2, "d.txt", "two")
    #expect(read(copy, "a.txt") == "v1")                                         // kopija ostane nedotaknjena med delom

    await ShareHub.$run.withValue("r1") { await hub.syncBack(session: "p") }
    await hub.finish(run: "r1")
    await ShareHub.$run.withValue("r2") { await hub.syncBack(session: "p") }
    await hub.finish(run: "r2")

    #expect(read(client, "a.txt") == "agent1" && read(client, "b.txt") == "agent2")
    #expect(read(client, "c.txt") == "one" && read(client, "c.txt.minigw-conflict") == "two")  // drugi ne prepiše prvega
    #expect(read(client, "d.txt") == nil && read(client, "d.txt.minigw-conflict") == "two")
    #expect(minigw.notes.filter { $0.hasPrefix("conflict:") }.count == 2)
    #expect(((try? FileManager.default.contentsOfDirectory(atPath: runsDir.path)) ?? []).isEmpty)  // kloni odstranjeni
    #expect(read(copy, "a.txt") == "agent1" && read(copy, "b.txt") == "agent2")  // združeno v kopijo

    // Naslednji prenos je spet le razlika: uporabnikova c.txt in d.txt ostaneta zmagovalki; klon je spet ».0«.
    #expect(try #require(await mount("r3")) == runsDir.appendingPathComponent("local-x.0"))
    #expect(read(copy, "c.txt") == "one" && read(copy, "d.txt") == nil)
    await hub.finish(run: "r3")
}

/// Pregled: R1 in R2 spremenita f.txt; R2 je prevzet, R1 prekinjen. R1 ne sme prepisati R2 ne v kopiji ne pri odjemalcu.
/// Agent zamenja datoteko »a« z mapo »a/b«: nova datoteka pride do odjemalca in v kopijo.
@Test func syncCancelledRunDoesNotOverwriteOtherRun() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("syncrev-" + UUID().uuidString)
    let client = base.appendingPathComponent("client"), copy = base.appendingPathComponent("sync/local/x")
    defer { try? FileManager.default.removeItem(at: base) }
    func put(_ root: URL, _ p: String, _ s: String) throws {
        try FileManager.default.createDirectory(at: root.appendingPathComponent(p).deletingLastPathComponent(), withIntermediateDirectories: true)
        try Data(s.utf8).write(to: root.appendingPathComponent(p))
    }
    func read(_ root: URL, _ p: String) -> String? { (try? Data(contentsOf: root.appendingPathComponent(p))).map { String(decoding: $0, as: UTF8.self) } }
    try FileManager.default.createDirectory(at: client, withIntermediateDirectories: true)
    try put(client, "f.txt", "v1"); try put(client, "a", "file")
    let hub = ShareHub()
    let key = try #require(await hub.register(session: "p", cwd: client.path, key: nil, copy: copy))
    let minigw = FakeMinigw(hub, key: key, root: client)
    let server = minigw.serve()
    defer { server.cancel() }
    func mount(_ run: String) async -> URL? { await ShareHub.$run.withValue(run) { await hub.mount(session: "p", cwd: client.path, base: "") } }
    func pull(_ run: String) async { await ShareHub.$run.withValue(run) { await hub.syncBack(session: "p") }; await hub.finish(run: run) }

    let d1 = try #require(await mount("r1")), d2 = try #require(await mount("r2"))
    try put(d1, "f.txt", "one-agent1"); try put(d2, "f.txt", "two")
    await pull("r2")
    #expect(read(client, "f.txt") == "two")
    await hub.finish(run: "r1")                                                   // prekinjen: brez prevzema
    #expect(read(copy, "f.txt") == "two" && read(copy, "f.txt.minigw-agent") == "one-agent1")
    _ = try #require(await mount("r3"))
    await pull("r3")
    #expect(read(client, "f.txt") == "two" && read(client, "f.txt.minigw-agent") == "one-agent1")  // obe deli pri uporabniku

    let d4 = try #require(await mount("r4"))
    try FileManager.default.removeItem(at: d4.appendingPathComponent("a")); try put(d4, "a/b", "new")
    await pull("r4")
    #expect(read(client, "a/b") == "new" && read(copy, "a/b") == "new")
    _ = try #require(await mount("r5"))
    await pull("r5")
    #expect(read(client, "a/b") == "new" && read(copy, "a/b") == "new")
    // In nazaj: mapa »a/« → datoteka »a«.
    let d6 = try #require(await mount("r6"))
    try FileManager.default.removeItem(at: d6.appendingPathComponent("a")); try put(d6, "a", "file again")
    await pull("r6")
    #expect(read(client, "a") == "file again" && read(copy, "a") == "file again")
}

/// Obratni vrstni red: R1 prekinjen (združen) pred prevzemom R2. R3 ne sme tiho prepisati R2 z različico R1.
/// Nato: prevzem R4 za f.txt ne uspe, R5 medtem spremeni kopijo → različica R4 pride kot f.txt.minigw-agent.
@Test func syncCancelFinishesBeforeOtherPull() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("syncrev2-" + UUID().uuidString)
    let client = base.appendingPathComponent("client"), copy = base.appendingPathComponent("sync/local/x")
    defer { try? FileManager.default.removeItem(at: base) }
    func put(_ root: URL, _ p: String, _ s: String) throws { try Data(s.utf8).write(to: root.appendingPathComponent(p)) }
    func read(_ root: URL, _ p: String) -> String? { (try? Data(contentsOf: root.appendingPathComponent(p))).map { String(decoding: $0, as: UTF8.self) } }
    try FileManager.default.createDirectory(at: client, withIntermediateDirectories: true)
    try put(client, "f.txt", "v1")
    let hub = ShareHub()
    let key = try #require(await hub.register(session: "p", cwd: client.path, key: nil, copy: copy))
    let minigw = FakeMinigw(hub, key: key, root: client)
    let server = minigw.serve()
    defer { server.cancel() }
    func mount(_ run: String) async -> URL? { await ShareHub.$run.withValue(run) { await hub.mount(session: "p", cwd: client.path, base: "") } }
    func pull(_ run: String) async { await ShareHub.$run.withValue(run) { await hub.syncBack(session: "p") }; await hub.finish(run: run) }

    let d1 = try #require(await mount("r1")), d2 = try #require(await mount("r2"))
    try put(d1, "f.txt", "one-agent1"); try put(d2, "f.txt", "two")
    await hub.finish(run: "r1")                                                   // prekinjen pred prevzemom R2
    await pull("r2")
    #expect(read(client, "f.txt") == "two")
    _ = try #require(await mount("r3"))
    await pull("r3")
    #expect(read(client, "f.txt") == "two" && read(client, "f.txt.minigw-conflict") == "one-agent1")  // spor, ne prepis
    _ = try #require(await mount("r3b"))
    await pull("r3b")
    #expect(read(client, "f.txt") == "two")                                       // in tudi pozneje ne

    // Prevzem ene poti ne uspe; drug agent medtem spremeni kopijo → različica agenta pride kot .minigw-agent.
    try FileManager.default.removeItem(at: client.appendingPathComponent("f.txt.minigw-conflict"))
    let d4 = try #require(await mount("r4")), d5 = try #require(await mount("r5"))
    try put(d4, "f.txt", "four"); try put(d5, "f.txt", "five")
    await pull("r5")
    minigw.failGet = ["f.txt"]
    await pull("r4")
    minigw.failGet = []
    #expect(read(client, "f.txt") == "five" && read(copy, "f.txt.minigw-agent") == "four")
    _ = try #require(await mount("r6"))
    await pull("r6")
    #expect(read(client, "f.txt") == "five" && read(client, "f.txt.minigw-agent") == "four")
    let leftovers = FileManager.default.enumerator(atPath: copy.path)?.compactMap { $0 as? String }.filter { $0.hasSuffix(".minigw-conflict") }
    #expect(leftovers == [])                                                     // v kopiji se spori ne kopičijo
}

/// Prekinjeno izvajanje: brez klona; delo agenta pred prekinitvijo ostane v kopiji (»kept«) za naslednji prevzem.
@Test func syncCancelledRunLeavesNoClone() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("synccancel-" + UUID().uuidString)
    let client = base.appendingPathComponent("client"), copy = base.appendingPathComponent("sync/local/y")
    let runsDir = base.appendingPathComponent("sync/runs")
    defer { try? FileManager.default.removeItem(at: base) }
    try FileManager.default.createDirectory(at: client, withIntermediateDirectories: true)
    try Data("v1".utf8).write(to: client.appendingPathComponent("a.txt"))
    let hub = ShareHub()
    let key = try #require(await hub.register(session: "c", cwd: client.path, key: nil, copy: copy))
    let minigw = FakeMinigw(hub, key: key, root: client)
    let server = minigw.serve()
    defer { server.cancel() }

    // Prekinjeno pred prenosom: nič.
    let early = Task { await ShareHub.$run.withValue("c0") { await hub.mount(session: "c", cwd: client.path, base: "") } }
    early.cancel()
    #expect(await early.value == nil)
    await hub.finish(run: "c0")
    // Prekinjeno med agentom (brez prevzema): finish združi in odstrani klon.
    let dir = try #require(await ShareHub.$run.withValue("c1") { await hub.mount(session: "c", cwd: client.path, base: "") })
    try Data("agent".utf8).write(to: dir.appendingPathComponent("a.txt"))
    await hub.finish(run: "c1")
    #expect(!FileManager.default.fileExists(atPath: dir.path))
    #expect(((try? FileManager.default.contentsOfDirectory(atPath: runsDir.path)) ?? []).isEmpty)
    #expect(try #require(await hub.syncManifest(key: key, Array(minigw.state.values))).kept == ["a.txt"])
}

@Test func syncRefusesUnpairedRemoteClient() async {
    let r = Router(config: RouterConfig(), token: "tok", secrets: FakeSecrets(values: [:]))
    let w = FakeWriter()
    var q = request("POST", "/minigw/share", headers: ["Authorization": "Bearer tok", "x-claude-mini-session": UUID().uuidString,
                                                      "x-claude-mini-cwd": "/p", "x-claude-mini-work": "sync"])
    q.peer = "192.0.2.4"
    await r.handle(q, writer: w)
    #expect(w.status == 403 && w.text.contains("device key"))  // oddaljen brez ključa naprave: že prehod (H1)
}

@Test func syncExecOnlyChangeAndInvalidPathsAreDone() throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent("syncexec-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    try Data("x".utf8).write(to: root.appendingPathComponent("run.sh"))
    var state = Sync.scan(root).files
    var e = try #require(state["run.sh"])
    e.exec = true
    #expect(Sync.diff(old: state, new: ["run.sh": e], root: root).changed.map(\.path) == ["run.sh"])  // le bit x
    let ch = Sync.Changes(changed: [e, Sync.Entry(path: "../bad", size: 1, mtime: 1, sha256: "z")], deleted: ["../gone"])
    let (_, done) = Sync.apply(ch, root: root, state: &state) { _ in nil }
    let mode = try FileManager.default.attributesOfItem(atPath: root.appendingPathComponent("run.sh").path)[.posixPermissions] as? NSNumber
    #expect((mode?.intValue ?? 0) & 0o100 != 0)
    #expect(done == ["../gone", "run.sh", "../bad"])  // neveljavne potrjene, sicer bi ostale v čakanju
}

@Test func syncMountWaitsForRegistration() async throws {
    let hub = ShareHub()
    let copy = FileManager.default.temporaryDirectory.appendingPathComponent("lockwait-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: copy) }
    let late = Task { await hub.mount(session: "late", cwd: nil, base: "", wait: true) }
    try? await Task.sleep(for: .milliseconds(200))
    let key = try #require(await hub.register(session: "late", cwd: "/a", key: nil, copy: copy))
    guard case .some(.some(let job)) = await hub.poll(key: key, wait: 5) else { Issue.record("no job"); return }
    #expect(job.method == "SYNC-PUSH")
    _ = await hub.reply(key: key, DAVReply(id: job.id, response: DAVResponse(200)))
    #expect(await late.value == copy)
}

/// Uporabnik chmod +x (in sprememba) → prenos → agent te datoteke ne spremeni → prevzem ohrani bit x.
@Test func syncKeepsUserExecBit() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("syncx-" + UUID().uuidString)
    let client = base.appendingPathComponent("client"), copy = base.appendingPathComponent("copy")
    defer { try? FileManager.default.removeItem(at: base) }
    try FileManager.default.createDirectory(at: client, withIntermediateDirectories: true)
    let a = client.appendingPathComponent("a.sh"), b = client.appendingPathComponent("b.sh")
    func mode(_ u: URL) -> Int { ((try? FileManager.default.attributesOfItem(atPath: u.path))?[.posixPermissions] as? NSNumber)?.intValue ?? 0 }
    func chmod(_ u: URL, _ m: Int) throws { try FileManager.default.setAttributes([.posixPermissions: m], ofItemAtPath: u.path) }
    try Data("v1".utf8).write(to: a); try Data("v1".utf8).write(to: b); try chmod(a, 0o644); try chmod(b, 0o644)

    let hub = ShareHub()
    let key = try #require(await hub.register(session: "x", cwd: client.path, key: nil, copy: copy))
    func push(_ state: [String: Sync.Entry]) async throws {
        for p in try #require(await hub.syncManifest(key: key, Array(state.values))).need {
            #expect(await hub.syncPut(key: key, path: p, data: try Data(contentsOf: client.appendingPathComponent(p))) == 204)
        }
    }
    var state = Sync.scan(client).files
    try await push(state)
    try Data("v2".utf8).write(to: a); try chmod(a, 0o755)          // uporabnik: vsebina + chmod +x
    state = Sync.scan(client, previous: state).files
    try await push(state)
    #expect(mode(copy.appendingPathComponent("a.sh")) == 0o755)    // kopija dobi bit x (ne ostane star način)
    let ch = try #require(await hub.syncChanges(key: key))
    #expect(ch.changed.isEmpty)                                    // agent ni naredil nič → nič nazaj
    _ = Sync.apply(ch, root: client, state: &state) { _ in nil }
    #expect(mode(a) == 0o755)

    // Uporabnik chmod +x po prenosu, agent pa samo spremeni vsebino ali bit: uporabnikov bit ostane.
    try chmod(b, 0o755)
    var e = try #require(state["b.sh"])
    e.exec = false
    let (notes, _) = Sync.apply(Sync.Changes(changed: [e]), root: client, state: &state) { _ in nil }
    #expect(mode(b) == 0o755 && notes.first?.hasPrefix("kept your file mode") == true)
    let agent = Data("agent".utf8)
    let edit = Sync.Entry(path: "b.sh", size: 5, mtime: 1, sha256: Sync.sha256(agent), exec: false)
    _ = Sync.apply(Sync.Changes(changed: [edit]), root: client, state: &state) { _ in agent }
    #expect(mode(b) == 0o755 && (try? String(contentsOf: b, encoding: .utf8)) == "agent")
    // Agent chmod +x in sprememba vsebine pride do odjemalca.
    let x = Sync.Entry(path: "a.sh", size: 5, mtime: 2, sha256: Sync.sha256(agent), exec: true)
    try chmod(a, 0o644); state["a.sh"] = Sync.scan(client).files["a.sh"]
    _ = Sync.apply(Sync.Changes(changed: [x]), root: client, state: &state) { _ in agent }
    #expect(mode(a) == 0o755)
}

@Test func syncJobAllowedOnlyInItsMode() {
    #expect(Sync.jobAllowed("SYNC-PUSH", sync: true, readOnly: true, write: false))
    // #45: minigw izpiše opombo prehoda v vsakem načinu (tudi sync in direct); drugi posli kot doslej.
    for kind in ["sync", "direct", "native", "native-direct", "worktree"] {
        #expect(Sync.clientJob("WORKTREE-NOTE", kind: kind, readOnly: true, write: false) == .note, "\(kind)")
    }
    #expect(Sync.clientJob("WORKTREE-PUSH", kind: "sync", readOnly: false, write: false) == nil
            && Sync.clientJob("SYNC-PULL", kind: "sync", readOnly: false, write: false) == .syncPull
            && Sync.clientJob("SYNC-PULL", kind: "sync", readOnly: true, write: false) == nil
            && Sync.clientJob("PUT", kind: "direct", readOnly: false, write: true) == .webDAV
            && Sync.clientJob("PUT", kind: "native", readOnly: false, write: true) == nil
            && Sync.clientJob("WORKTREE-PULL", kind: "worktree", readOnly: false, write: false) == .worktreePull
            && Sync.clientJob("SYNC-PUSH", kind: "worktree", readOnly: false, write: false) == nil)
    #expect(Profiles.gatewayNote("Mini Gateway lets Codex change files only in worktree mode, so it only read the folder (run minigw --minigw-work=worktree)")
            == "minigw: gateway: Mini Gateway lets Codex change files only in worktree mode, so it only read the folder (run minigw --minigw-work=worktree)")
    #expect(!Sync.jobAllowed("SYNC-PULL", sync: true, readOnly: true, write: false))   // --minigw-ro: router ne piše
    #expect(Sync.jobAllowed("SYNC-PULL", sync: true, readOnly: false, write: false))
    #expect(!Sync.jobAllowed("SYNC-PULL", sync: false, readOnly: false, write: false)) // direct: brez SYNC-*
    #expect(!Sync.jobAllowed("SYNC-PUSH", sync: false, readOnly: false, write: false))
    #expect(!Sync.jobAllowed("SYNC-OTHER", sync: true, readOnly: false, write: false))
    #expect(!Sync.jobAllowed("GET", sync: true, readOnly: false, write: false))        // sync: brez WebDAV
    #expect(Sync.jobAllowed("GET", sync: false, readOnly: true, write: false))
    #expect(!Sync.jobAllowed("PUT", sync: false, readOnly: true, write: true))
}

@Test func syncNeverTouchesGitOrCommandSettings() async throws {
    for p in [".git/config", ".GIT/hooks/post-checkout", "a/.Git./hooks/x", ".git /x", ".claude/settings.json", ".Claude/Settings.Local.json",
              "sub/.vscode/tasks.json", ".envrc", "x/.ENVRC", ".claude/hooks/x.sh", ".claude/hooks", ".Claude/Commands/c.md",
              ".claude/agents/a.md", "sub/.claude/skills/s/SKILL.md", ".husky/pre-commit", ".husky", "a/.githooks/pre-push"] {
        #expect(Sync.isProtected(p), "\(p)")
    }
    for p in ["src/git.swift", ".gitignore", ".claude/notes.md", "src/hooks/use.ts", "skills/a.md", ".vscode/settings.json", "envrc"] { #expect(!Sync.isProtected(p), "\(p)") }
    #expect(Sync.Ignore(gitignore: "").ignored("Node_Modules", isDir: true))   // brez razlike v velikih črkah (APFS)

    let base = FileManager.default.temporaryDirectory.appendingPathComponent("syncgit-" + UUID().uuidString)
    let client = base.appendingPathComponent("client"), copy = base.appendingPathComponent("copy")
    defer { try? FileManager.default.removeItem(at: base) }
    try FileManager.default.createDirectory(at: client.appendingPathComponent(".GIT"), withIntermediateDirectories: true)
    try Data("x".utf8).write(to: client.appendingPathComponent(".GIT/config"))
    try Data("y".utf8).write(to: client.appendingPathComponent("a.txt"))
    #expect(Set(Sync.scan(client).files.keys) == ["a.txt"])                   // ».GIT« ne gre na router

    var state = Sync.scan(client).files
    let evil = Data("#!/bin/sh".utf8)
    let ch = Sync.Changes(changed: [".GIT/hooks/post-checkout", ".claude/settings.json", ".envrc"].map {
        Sync.Entry(path: $0, size: Int64(evil.count), mtime: 1, sha256: Sync.sha256(evil), exec: true)
    }, deleted: [".GIT/config"])
    let (notes, done) = Sync.apply(ch, root: client, state: &state) { _ in evil }
    #expect(done.count == 4 && notes.count == 4)
    #expect(!FileManager.default.fileExists(atPath: client.appendingPathComponent(".GIT/hooks").path))
    #expect(!FileManager.default.fileExists(atPath: client.appendingPathComponent(".claude").path))
    #expect(!FileManager.default.fileExists(atPath: client.appendingPathComponent(".envrc").path))
    #expect(FileManager.default.fileExists(atPath: client.appendingPathComponent(".GIT/config").path))

    // Router: odjemalec ne more poslati ».GIT/…« v kopijo.
    let hub = ShareHub()
    let key = try #require(await hub.register(session: "g", cwd: client.path, key: nil, copy: copy))
    let e = Sync.Entry(path: ".GIT/hooks/pre-commit", size: Int64(evil.count), mtime: 1, sha256: Sync.sha256(evil))
    #expect(try #require(await hub.syncManifest(key: key, [e])).need.isEmpty)
    #expect(await hub.syncPut(key: key, path: e.path, data: evil) == 403)
}

@Test func shareBoundToRegisteringDevice() async throws {
    let hub = ShareHub()
    let copy = FileManager.default.temporaryDirectory.appendingPathComponent("syncdev-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: copy) }
    let key = try #require(await hub.register(session: "d", cwd: "/a", key: nil, copy: copy, device: "dev1"))
    #expect(await hub.mount(session: "d", cwd: nil, base: "http://127.0.0.1:1") == nil)
    #expect(await hub.mount(session: "d", cwd: nil, base: "http://127.0.0.1:1", device: "dev2") == nil)
    #expect(await hub.syncManifest(key: key, [], device: "dev2") == nil)
    #expect(await hub.syncPut(key: key, path: "a", data: Data(), device: nil) == 404)
    #expect(await hub.syncChanges(key: key, device: "dev2") == nil)
    #expect(await hub.syncManifest(key: key, [], device: "dev1") != nil)
}

@Test func readOnlySyncHasItsOwnCopy() async throws {
    let scratch = FileManager.default.temporaryDirectory.appendingPathComponent("syncrocopy-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: scratch) }
    var c = RouterConfig()
    c.scratchFolder = scratch.path
    let r = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    let cwd = "/p-" + UUID().uuidString
    var keys: [String] = []
    for mode in ["ro", "rw"] {
        var q = request("POST", "/minigw/share", headers: ["Authorization": "Bearer tok", "x-claude-mini-session": UUID().uuidString,
                                                           "x-claude-mini-cwd": cwd, "x-claude-mini-work": "sync", "x-share-mode": mode])
        q.peer = "127.0.0.1"
        let w = FakeWriter()
        await r.handle(q, writer: w)
        keys.append(try #require(w.text.split(separator: "\"").dropFirst(3).first.map(String.init)))
    }
    for k in keys {
        var q = request("POST", "/minigw/sync/manifest", headers: ["Authorization": "Bearer tok", "x-share-key": k], body: "[]")
        q.peer = "127.0.0.1"
        await r.handle(q, writer: FakeWriter())
    }
    let dir = c.scratchURL.appendingPathComponent("sync/local")
    let names = Set(((try? FileManager.default.contentsOfDirectory(atPath: dir.path)) ?? []).filter { !$0.hasSuffix(".used") })  // oznake rabe (#1)
    #expect(names == [Sync.folderID(cwd), Sync.folderID(cwd) + "-ro"])
}

@Test func agentModelNamesAreSlugs() {
    #expect(Router.validModel("gpt-5.1-codex") && Router.validModel("openai/o3:high") && Router.validModel("grok_4"))
    for m in ["", "-c", "--config=x", "a b", "a;rm", "a\nb", "ž"] { #expect(!Router.validModel(m), "\(m)") }
}

@Test func syncProtectsShortAndFoldedNames() {
    for p in ["GIT~1/hooks/post-checkout", "git~12/config", ".claude/ſettings.json", ".vscode/taſks.json"] {
        #expect(Sync.isProtected(p), "\(p)")
    }
    #expect(!Sync.isProtected("src/git~x.txt"))
}

/// Povezava, ki jo agent podtakne v klon ali kopijo (datoteka ali starševska mapa), ne prenese vsebine cilja:
/// ne v kopijo (merge → clone) ne k odjemalcu (syncGet → Sync.read).
@Test func syncNeverFollowsSymlinksFromClone() throws {
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("synclink-" + UUID().uuidString)
    defer { try? fm.removeItem(at: base) }
    let secret = base.appendingPathComponent("outside/secret"), clone = base.appendingPathComponent("clone")
    try fm.createDirectory(at: secret.deletingLastPathComponent(), withIntermediateDirectories: true)
    try fm.createDirectory(at: clone, withIntermediateDirectories: true)
    try Data("secret".utf8).write(to: secret)
    try fm.createSymbolicLink(at: clone.appendingPathComponent("a.txt"), withDestinationURL: secret)
    try fm.createSymbolicLink(at: clone.appendingPathComponent("dir"), withDestinationURL: secret.deletingLastPathComponent())
    for src in [clone.appendingPathComponent("a.txt"), clone.appendingPathComponent("dir/secret")] {
        let dst = base.appendingPathComponent("copy-" + UUID().uuidString)
        #expect(!ShareHub.clone(src, to: dst) && !fm.fileExists(atPath: dst.path), "\(src.path)")
        #expect(Sync.read(src, max: 1 << 20, flags: O_NOFOLLOW_ANY) == nil, "\(src.path)")
        #expect(!ShareHub.copyRegular(src, to: dst) && !fm.fileExists(atPath: dst.path), "\(src.path)")  // brez APFS
    }
    try Data("ok".utf8).write(to: clone.appendingPathComponent("b.txt"))
    #expect(Sync.read(clone.appendingPathComponent("b.txt"), max: 1 << 20, flags: O_NOFOLLOW_ANY) == Data("ok".utf8))
    try fm.setAttributes([.posixPermissions: 0o755], ofItemAtPath: clone.appendingPathComponent("b.txt").path)
    let copied = base.appendingPathComponent("b-copy")
    #expect(ShareHub.copyRegular(clone.appendingPathComponent("b.txt"), to: copied))
    #expect(Sync.read(copied, max: 1 << 20) == Data("ok".utf8))
    #expect((try fm.attributesOfItem(atPath: copied.path))[.posixPermissions] as? Int == 0o755)
}

/// .gitignore kot povezava ven iz korena ali FIFO: prezrta, brez čakanja.
@Test func syncIgnoreSkipsLinkedOrSpecialGitignore() throws {
    let fm = FileManager.default
    let base = fm.temporaryDirectory.appendingPathComponent("syncgi-" + UUID().uuidString)
    defer { try? fm.removeItem(at: base) }
    let root = base.appendingPathComponent("root")
    try fm.createDirectory(at: root, withIntermediateDirectories: true)
    try Data("*.txt\n".utf8).write(to: base.appendingPathComponent("outside"))
    try fm.createSymbolicLink(at: root.appendingPathComponent(".gitignore"), withDestinationURL: base.appendingPathComponent("outside"))
    #expect(!Sync.Ignore(root: root, follow: false).ignored("a.txt", isDir: false))
    // Odjemalec: povezan .gitignore (dotfiles) velja; .env ne gre na router.
    try Data(".env\n".utf8).write(to: base.appendingPathComponent("outside"))
    try Data("x".utf8).write(to: root.appendingPathComponent(".env"))
    #expect(Sync.scan(root, trusted: true).files[".env"] == nil && Sync.scan(root).files[".env"] != nil)
    try fm.removeItem(at: root.appendingPathComponent(".gitignore"))
    #expect(mkfifo(root.appendingPathComponent(".gitignore").path, 0o600) == 0)
    #expect(!Sync.Ignore(root: root, follow: true).ignored("a.txt", isDir: false))
    #expect(!Sync.Ignore(root: root, follow: false).ignored("a.txt", isDir: false))
    try fm.removeItem(at: root.appendingPathComponent(".gitignore"))
    try Data("*.txt\n".utf8).write(to: root.appendingPathComponent(".gitignore"))
    #expect(Sync.Ignore(root: root, follow: false).ignored("a.txt", isDir: false))
}

/// Dva agenta v sporu s kopijo: oba ostaneta (f.txt.minigw-agent, f.txt.minigw-agent-2).
@Test func syncTwoAgentConflictsBothSurvive() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("syncagents-" + UUID().uuidString)
    let client = base.appendingPathComponent("client"), copy = base.appendingPathComponent("sync/local/x")
    defer { try? FileManager.default.removeItem(at: base) }
    func put(_ root: URL, _ p: String, _ s: String) throws { try Data(s.utf8).write(to: root.appendingPathComponent(p)) }
    func read(_ root: URL, _ p: String) -> String? { (try? Data(contentsOf: root.appendingPathComponent(p))).map { String(decoding: $0, as: UTF8.self) } }
    try FileManager.default.createDirectory(at: client, withIntermediateDirectories: true)
    try put(client, "f.txt", "v1")
    let hub = ShareHub()
    let key = try #require(await hub.register(session: "p", cwd: client.path, key: nil, copy: copy))
    let minigw = FakeMinigw(hub, key: key, root: client)
    let server = minigw.serve()
    defer { server.cancel() }
    func mount(_ run: String) async -> URL? { await ShareHub.$run.withValue(run) { await hub.mount(session: "p", cwd: client.path, base: "") } }

    let d1 = try #require(await mount("r1")), d2 = try #require(await mount("r2")), d3 = try #require(await mount("r3"))
    try put(d1, "f.txt", "one"); try put(d2, "f.txt", "two"); try put(d3, "f.txt", "three")
    await ShareHub.$run.withValue("r3") { await hub.syncBack(session: "p") }
    await hub.finish(run: "r3")
    await hub.finish(run: "r1")                                                   // prekinjena: spor s kopijo
    await hub.finish(run: "r2")
    #expect(read(copy, "f.txt") == "three")
    #expect(read(copy, "f.txt.minigw-agent") == "one" && read(copy, "f.txt.minigw-agent-2") == "two")
}

/// #7: /dav/<skrivnost> le za webdavfs_agent, ki ga je pognal priklop routerja (skrivnost je vidna v mount in ps).
@Test func davOnlyForTheMountingAgent() {
    let agent: (pid_t) -> Bool = { $0 == 100 }
    // med priklopom: prvi sistemski webdavfs_agent je izbran
    #expect(ShareHub.davClaim(nil, mounting: true, find: { 100 }, owns: { _ in false }, isAgent: agent) == 100)
    // brez priklopa routerja (agent je sam pognal mount_webdav s skrivnostjo): zavrnjeno
    #expect(ShareHub.davClaim(nil, mounting: false, find: { 100 }, owns: { _ in true }, isAgent: agent) == nil)
    // drug proces (curl agenta, drug uporabnik) tudi med priklopom: zavrnjeno
    #expect(ShareHub.davClaim(nil, mounting: true, find: { 200 }, owns: { _ in true }, isAgent: agent) == nil)
    // po izbiri: le isti proces na povezavi
    #expect(ShareHub.davClaim(100, mounting: false, find: { 100 }, owns: { $0 == 100 }, isAgent: agent) == 100)
    #expect(ShareHub.davClaim(100, mounting: false, find: { 200 }, owns: { _ in false }, isAgent: agent) == nil)
    // pravi proces: ta testni program ni webdavfs_agent
    #expect(!PeerProcess.isWebDAVAgent(getpid()))
}

/// #11: zaščita velja za kanonično pot: povezava hooks → .git/hooks ne pusti pisanja ali brisanja v .git.
@Test func syncProtectedThroughLink() throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("p11-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    try FileManager.default.createDirectory(at: root.appendingPathComponent(".git/hooks"), withIntermediateDirectories: true)
    try Data("x".utf8).write(to: root.appendingPathComponent(".git/hooks/old"))
    try FileManager.default.createSymbolicLink(atPath: root.appendingPathComponent("hooks").path, withDestinationPath: ".git/hooks")
    let data = Data("#!/bin/sh\necho owned\n".utf8), sum = Sync.sha256(data)
    let ch = Sync.Changes(changed: [Sync.Entry(path: "hooks/pre-commit", size: Int64(data.count), mtime: 0, sha256: sum),
                                    Sync.Entry(path: "src/ok.txt", size: Int64(data.count), mtime: 0, sha256: sum)],
                          deleted: ["hooks/old"], base: ["hooks/old": Sync.sha256(Data("x".utf8))])
    var state: [String: Sync.Entry] = [:]
    let (notes, _) = Sync.apply(ch, root: root, state: &state) { _ in data }
    #expect(!FileManager.default.fileExists(atPath: root.appendingPathComponent(".git/hooks/pre-commit").path))
    #expect(FileManager.default.fileExists(atPath: root.appendingPathComponent(".git/hooks/old").path))
    #expect((try? Data(contentsOf: root.appendingPathComponent("src/ok.txt"))) == data)
    #expect(notes.contains { $0.contains("protected path (through a link)") })
}

/// #20: neznana osnova (router jo je izvedel iz prenosa) ne prepiše uporabnikove spremembe med izvajanjem.
@Test func syncBaseMustBeAVersionWeSent() throws {
    for known in [false, true] {
        let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("p20-" + UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        try Data("user edit".utf8).write(to: root.appendingPathComponent("a.txt"))
        var state = ["a.txt": Sync.Entry(path: "a.txt", size: 4, mtime: 0, sha256: Sync.sha256(Data("orig".utf8)))]
        let agent = Data("agent".utf8)
        let ch = Sync.Changes(changed: [Sync.Entry(path: "a.txt", size: Int64(agent.count), mtime: 0, sha256: Sync.sha256(agent))],
                              base: ["a.txt": Sync.sha256(Data("user edit".utf8))])
        _ = Sync.apply(ch, root: root, state: &state, known: { _, _ in known }) { _ in agent }
        let got = try String(contentsOf: root.appendingPathComponent("a.txt"), encoding: .utf8)
        let conflict = try? String(contentsOf: root.appendingPathComponent("a.txt.minigw-conflict"), encoding: .utf8)
        #expect(known ? got == "agent" : (got == "user edit" && conflict == "agent"))
    }
}

/// #22: največ ShareHub.maxRunsPerCopy hkratnih izvajanj na kopijo; po koncu enega je mesto spet prosto.
@Test func syncRunsPerCopyAreCapped() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("sync22-" + UUID().uuidString)
    let client = base.appendingPathComponent("client"), copy = base.appendingPathComponent("sync/local/x")
    defer { try? FileManager.default.removeItem(at: base) }
    try FileManager.default.createDirectory(at: client, withIntermediateDirectories: true)
    try Data("v1".utf8).write(to: client.appendingPathComponent("f.txt"))
    let hub = ShareHub()
    let key = try #require(await hub.register(session: "p", cwd: client.path, key: nil, copy: copy))
    let server = FakeMinigw(hub, key: key, root: client).serve()
    defer { server.cancel() }
    func mount(_ run: String) async -> URL? { await ShareHub.$run.withValue(run) { await hub.mount(session: "p", cwd: client.path, base: "") } }
    for i in 0..<ShareHub.maxRunsPerCopy { #expect(await mount("r\(i)") != nil) }
    #expect(await mount("over") == nil)
    await hub.finish(run: "r0")
    #expect(await mount("again") != nil)
}

/// #23: mapa kopije, zamenjana s povezavo ven, se pri pregledu ne izpiše (odprta z O_NOFOLLOW_ANY).
@Test func syncListDoesNotFollowASwappedFolder() throws {
    let base = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("p23-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: base) }
    let root = base.appendingPathComponent("copy"), outside = base.appendingPathComponent("home")
    try FileManager.default.createDirectory(at: root.appendingPathComponent("src"), withIntermediateDirectories: true)
    try FileManager.default.createDirectory(at: outside, withIntermediateDirectories: true)
    try Data("x".utf8).write(to: root.appendingPathComponent("src/čšž.txt"))
    try Data("secret".utf8).write(to: outside.appendingPathComponent("private-name.txt"))
    try FileManager.default.createSymbolicLink(at: root.appendingPathComponent("swapped"), withDestinationURL: outside)
    #expect(Sync.list(root.appendingPathComponent("src")) == ["čšž.txt"])
    #expect(Sync.list(root.appendingPathComponent("swapped")) == nil)
    let (files, skipped) = Sync.scan(root)
    #expect(!files.keys.contains { $0.contains("private-name") } && !skipped.contains { $0.contains("private-name") })
}

/// #1, #9: kopije, neuporabljene 14 dni, se izbrišejo (ne dejavne, ne runs); manifest nad mejo je zavrnjen.
@Test func syncCopiesExpireAndQuota() throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("p1-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let fm = FileManager.default, old = Date().addingTimeInterval(-15 * 86400)
    for p in ["dev/old", "dev/fresh", "dev/active", "runs/clone"] { try fm.createDirectory(at: root.appendingPathComponent(p), withIntermediateDirectories: true) }
    for p in ["dev/old", "dev/active", "runs/clone"] { try fm.setAttributes([.modificationDate: old], ofItemAtPath: root.appendingPathComponent(p).path) }
    ShareHub.touchUsed(root.appendingPathComponent("dev/fresh"))
    fm.createFile(atPath: root.appendingPathComponent("dev/old.used").path, contents: nil)
    try fm.setAttributes([.modificationDate: old], ofItemAtPath: root.appendingPathComponent("dev/old.used").path)
    ShareHub.removeOld(root, active: [root.appendingPathComponent("dev/active").standardizedFileURL.path], now: Date())
    #expect(!fm.fileExists(atPath: root.appendingPathComponent("dev/old").path) && !fm.fileExists(atPath: root.appendingPathComponent("dev/old.used").path))
    for p in ["dev/fresh", "dev/active", "runs/clone"] { #expect(fm.fileExists(atPath: root.appendingPathComponent(p).path), "\(p)") }

    #expect(Sync.overQuota([Sync.Entry(path: "a", size: 10, mtime: 0, sha256: "x")]) == nil)
    #expect(Sync.overQuota([Sync.Entry(path: "big", size: Sync.maxBytes + 1, mtime: 0, sha256: "x")])?.contains("too large") == true)
}

/// M2: velikost iz manifesta je zavezujoča (kvota); največ maxCopies kopij na napravo.
@Test func syncPutSizeAndCopyLimit() async throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("m2-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let hub = ShareHub(), data = Data("hello".utf8)
    let key = try #require(await hub.register(session: "m2", cwd: "/c", key: nil, copy: root.appendingPathComponent("copy")))
    _ = try #require(await hub.syncManifest(key: key, [Sync.Entry(path: "a.txt", size: 0, mtime: 1, sha256: Sync.sha256(data))]))
    #expect(await hub.syncPut(key: key, path: "a.txt", data: data) == 409)

    let dev = root.appendingPathComponent("dev")
    for i in 0..<Sync.maxCopies {
        #expect(!Sync.tooManyCopies(dev.appendingPathComponent("\(i)")))
        try FileManager.default.createDirectory(at: dev.appendingPathComponent("\(i)"), withIntermediateDirectories: true)
        FileManager.default.createFile(atPath: dev.appendingPathComponent("\(i).used").path, contents: nil)
    }
    #expect(Sync.tooManyCopies(dev.appendingPathComponent("new")) && !Sync.tooManyCopies(dev.appendingPathComponent("0")))
}

/// #2: imena, ki se razlikujejo le v velikih črkah.
@Test func syncCaseCollisions() {
    #expect(Sync.caseCollisions(["A.txt", "a.txt", "src/B.go", "src/b.GO", "ok.txt"]) == ["A.txt", "a.txt", "src/B.go", "src/b.GO"])
}

/// #3: ime v NFD (Finder) gre v manifest kot NFC, kot ga pošljeta Windows in Linux; datoteka se najde pod obema.
@Test func syncNamesAreNFC() throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("p3-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    let nfd = "c\u{30C}a.txt", nfc = "\u{10D}a.txt"  // »ča.txt«
    try Data("x".utf8).write(to: root.appendingPathComponent(nfd))
    let keys = Array(Sync.scan(root, trusted: true).files.keys)
    #expect(keys == [nfc])
    #expect(Sync.resolve(nfc, root: root).map { FileManager.default.fileExists(atPath: $0.path) } == true)
}

/// #4: glava in komentarji SSE pred prenosom mape; glava ponudnika izpuščena, napaka kot dogodek SSE.
@Test func earlyStreamBeforeFolderSync() async throws {
    let ok = FakeWriter(), w = EarlyStreamWriter(ok)
    await w.begin(rid: "r1")
    try await w.write(HTTPResponse.head(status: 200, headers: [("Content-Type", "text/event-stream")]) + Data("event: message_start\ndata: {}\n\n".utf8))
    #expect(ok.text.components(separatedBy: "HTTP/1.1").count == 2 && ok.text.contains(": syncing the folder") && ok.text.contains("event: message_start"))
    let bad = FakeWriter(), e = EarlyStreamWriter(bad)
    await e.begin(rid: "r2")
    let body = Data(#"{"error":{"message":"Syncing /p to the gateway failed","type":"api_error"},"type":"error"}"#.utf8)
    try await e.write(HTTPResponse.head(status: 502, headers: [("Content-Type", "application/json")], contentLength: body.count) + body)
    #expect(bad.text.hasPrefix("HTTP/1.1 200") && !bad.text.contains("502") && bad.text.contains("event: error") && bad.text.contains("Syncing /p to the gateway failed"))
}

/// #10: Siri v načinu sync po neuspelem prenosu ne bere mape tega Maca na isti poti.
@Test func siriMentionsNoFallbackInSync() {
    let local = URL(fileURLWithPath: "/Users/x/proj"), copy = URL(fileURLWithPath: "/scratch/sync/dev/abc")
    #expect(Router.mentionRoot(shared: nil, synced: true) { local } == nil)
    #expect(Router.mentionRoot(shared: copy, synced: true) { local } == copy)
    #expect(Router.mentionRoot(shared: nil, synced: false) { local } == local)
}

/// Varnost L2: /health/ready našteje ponudnike le temu Macu ali klicatelju z žetonom (kot TestHealthReadyProvidersOnlyLocalOrAuthorized v Go).
@Test func healthReadyProvidersOnlyLocalOrAuthorized() async {
    let router = Router(config: RouterConfig(), token: "tok", secrets: FakeSecrets(values: [:]))
    // #42: z loopbacka le s Host loopback (DNS rebinding pošlje svoj Host)
    for (peer, token, host, want) in [("192.0.2.9", "", "127.0.0.1:8080", false), ("192.0.2.9", "tok", "gw.example", true),
                                      ("127.0.0.1", "", "127.0.0.1:8080", true), ("127.0.0.1", "", "localhost", true),
                                      ("::1", "", "[::1]:8080", true), ("127.0.0.1", "", "evil.example", false),
                                      ("127.0.0.1", "", "127.evil.example:8080", false), ("127.0.0.1", "tok", "evil.example", true),
                                      ("127.0.0.1", "", "LOCALHOST", false), ("127.0.0.1", "", "localhost.", false),
                                      ("::1", "", "::1", true), ("127.0.0.1", "", "", false)] {  // golo ::1 kot Go loopbackHost
        var r = request("GET", "/health/ready", headers: token.isEmpty ? [:] : ["authorization": "Bearer " + token])
        r.peer = peer; r.headers["host"] = host
        let w = FakeWriter()
        await router.handle(r, writer: w)
        #expect(w.status == 200 && w.text.contains(#""providers""#) == want, "\(peer) \(token) \(host): \(w.text)")
    }
}

// #10: »a.txt« → »A.txt« (APFS: ista datoteka) ne sme izbrisati vira; COPY nase in mapa v podmapo = 403.
@Test func davMoveSameFileKeepsContent() throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent("dav-\(UUID().uuidString)")
    try FileManager.default.createDirectory(at: root.appendingPathComponent("d/sub"), withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: root) }
    try Data("keep".utf8).write(to: root.appendingPathComponent("a.txt"))
    func dav(_ m: String, _ s: String, _ d: String) -> Int {
        WebDAV.handle(method: m, path: "/dav/s/" + s, headers: ["destination": "http://x/dav/s/" + d], body: Data(), root: root, prefix: "/dav/s").status
    }
    #expect(dav("COPY", "a.txt", "a.txt") == 403)
    #expect(dav("MOVE", "a.txt", "A.txt") == 201)
    #expect(String(decoding: try Data(contentsOf: root.appendingPathComponent("A.txt")), as: UTF8.self) == "keep")
    let names = try FileManager.default.contentsOfDirectory(atPath: root.path)
    #expect(names.contains("A.txt") && !names.contains("a.txt") && !names.contains { $0.hasPrefix(".minigw") })
    #expect(dav("MOVE", "A.txt", "A.txt") == 201)
    #expect(String(decoding: try Data(contentsOf: root.appendingPathComponent("A.txt")), as: UTF8.self) == "keep")
    #expect(dav("MOVE", "d", "d/sub/d") == 403 && dav("COPY", "d", "D/sub/d") == 403)
    // L2: COPY mape ne kopira simbolnih povezav (kot Go copyTree)
    try FileManager.default.createSymbolicLink(atPath: root.appendingPathComponent("d/link").path, withDestinationPath: "/etc")
    try Data("x".utf8).write(to: root.appendingPathComponent("d/sub/f"))
    #expect(dav("COPY", "d", "e") == 201)
    #expect(FileManager.default.fileExists(atPath: root.appendingPathComponent("e/sub/f").path))
    #expect((try? FileManager.default.destinationOfSymbolicLink(atPath: root.appendingPathComponent("e/link").path)) == nil)
}

// #2: direktni WebDAV ne piše v zaščitene poti (kot sync), tudi prek povezave; branje ostane.
@Test func davRefusesProtectedWrites() throws {
    let fm = FileManager.default
    let root = fm.temporaryDirectory.appendingPathComponent("dav-\(UUID().uuidString)")
    try fm.createDirectory(at: root.appendingPathComponent(".git/hooks"), withIntermediateDirectories: true)
    defer { try? fm.removeItem(at: root) }
    try Data("ref".utf8).write(to: root.appendingPathComponent(".git/HEAD"))
    try Data("x".utf8).write(to: root.appendingPathComponent("a.txt"))
    try fm.createSymbolicLink(at: root.appendingPathComponent("hooks"), withDestinationURL: root.appendingPathComponent(".git/hooks"))
    func dav(_ m: String, _ p: String, _ d: String? = nil) -> Int {
        WebDAV.handle(method: m, path: "/dav/s/" + p, headers: d.map { ["destination": "http://x/dav/s/" + $0] } ?? [:],
                      body: Data("y".utf8), root: root, prefix: "/dav/s").status
    }
    for (m, p, d) in [("PUT", ".git/config", nil), ("PUT", ".GIT/config", nil), ("PUT", "git~1/config", nil), ("PUT", ".envrc", nil),
                      ("PUT", ".claude/settings.local.json", nil), ("PUT", ".vscode/tasks.json", nil), ("MKCOL", ".git/x", nil),
                      ("DELETE", ".git/HEAD", nil), ("DELETE", ".git", nil), ("LOCK", ".git/new", nil), ("PROPPATCH", ".git/HEAD", nil),
                      ("PUT", "hooks/pre-commit", nil), ("MOVE", "a.txt", ".git/hooks/pre-commit"), ("COPY", "a.txt", ".envrc"),
                      ("MOVE", ".git", "g")] as [(String, String, String?)] {
        #expect(dav(m, p, d) == 403, "\(m) \(p)")
    }
    #expect(dav("GET", ".git/HEAD") == 200)
    #expect(dav("COPY", ".git/HEAD", "head.txt") == 201)
    #expect(dav("PUT", "b.txt") == 201)
}

// #2: vsebina premaknjene/kopirane/izbrisane mape ne sme ustvariti ali odstraniti zaščitene datoteke.
@Test func davRefusesProtectedTreeBypass() throws {
    let fm = FileManager.default
    let root = fm.temporaryDirectory.appendingPathComponent("dav-\(UUID().uuidString)")
    for d in ["stage", "x", ".claude", "old"] { try fm.createDirectory(at: root.appendingPathComponent(d), withIntermediateDirectories: true) }
    defer { try? fm.removeItem(at: root) }
    for f in ["stage/settings.json", "x/tasks.json", ".claude/settings.json"] { try Data("{}".utf8).write(to: root.appendingPathComponent(f)) }
    func dav(_ m: String, _ p: String, _ d: String? = nil) -> Int {
        WebDAV.handle(method: m, path: "/dav/s/" + p, headers: d.map { ["destination": "http://x/dav/s/" + $0] } ?? [:],
                      body: Data(), root: root, prefix: "/dav/s").status
    }
    for (m, p, d) in [("MOVE", "stage", ".claude"), ("MOVE", "stage", ".Claude"), ("COPY", "x", ".vscode"), ("DELETE", ".claude", nil),
                      ("MOVE", ".claude", "y"), ("MOVE", "old", ".claude"), ("COPY", "old", ".claude")] as [(String, String, String?)] {
        #expect(dav(m, p, d) == 403, "\(m) \(p)")
    }
    #expect(fm.fileExists(atPath: root.appendingPathComponent(".claude/settings.json").path))
    #expect(!fm.fileExists(atPath: root.appendingPathComponent(".vscode").path))
    #expect(dav("COPY", ".claude", "c2") == 201)
    #expect(dav("MOVE", "stage", "s2") == 201)
}

// M1/M2/L3: ugnezden .git ne izgine prek starša; vir v cilju = 403; trda povezava ne pusti začasnega imena.
@Test func davNestedGitAncestorAndHardLink() throws {
    let fm = FileManager.default
    let root = fm.temporaryDirectory.appendingPathComponent("dav-\(UUID().uuidString)")
    for d in ["repoA/sub/.git", "other", "a/b"] { try fm.createDirectory(at: root.appendingPathComponent(d), withIntermediateDirectories: true) }
    defer { try? fm.removeItem(at: root) }
    try Data("ref".utf8).write(to: root.appendingPathComponent("repoA/sub/.git/HEAD"))
    try Data("keep".utf8).write(to: root.appendingPathComponent("a/b/f"))
    func dav(_ m: String, _ p: String, _ d: String? = nil) -> Int {
        WebDAV.handle(method: m, path: "/dav/s/" + p, headers: d.map { ["destination": "http://x/dav/s/" + $0] } ?? [:],
                      body: Data(), root: root, prefix: "/dav/s").status
    }
    for (m, p, d) in [("DELETE", "repoA", nil), ("MOVE", "other", "repoA"), ("COPY", "other", "repoA"), ("MOVE", "repoA", "x"),
                      ("MOVE", "a/b", "a"), ("COPY", "a/b", "a"), ("MOVE", "a/b", "A")] as [(String, String, String?)] {
        #expect(dav(m, p, d) == 403, "\(m) \(p)")
    }
    #expect(fm.fileExists(atPath: root.appendingPathComponent("repoA/sub/.git/HEAD").path))
    #expect(String(decoding: try Data(contentsOf: root.appendingPathComponent("a/b/f")), as: UTF8.self) == "keep")
    try Data("hl".utf8).write(to: root.appendingPathComponent("h1"))
    try fm.linkItem(at: root.appendingPathComponent("h1"), to: root.appendingPathComponent("h2"))
    #expect(dav("MOVE", "h1", "h2") == 201)
    #expect(try !fm.contentsOfDirectory(atPath: root.path).contains { $0.hasPrefix(".minigw") })
    #expect(String(decoding: try Data(contentsOf: root.appendingPathComponent("h2")), as: UTF8.self) == "hl")
}

/// L1: telo nad 1 MB z drugega računalnika šele po žetonu v glavi; lokalno (mount_webdav) in majhna telesa brez tega.
@Test func bigBodyNeedsTokenFirst() {
    let head = { (len: Int, auth: String) in Data("POST /v1/messages HTTP/1.1\r\ncontent-length: \(len)\r\n\(auth)\r\n".utf8) + Data(count: 10) }
    let ok = { (r: HTTPRequest) in r.header("x-api-key") == "t" }
    #expect(RouterServer.bodyNeedsToken(head(2 << 20, ""), peer: "10.0.0.2", authorized: ok))
    #expect(!RouterServer.bodyNeedsToken(head(2 << 20, "x-api-key: t\r\n"), peer: "10.0.0.2", authorized: ok))
    #expect(!RouterServer.bodyNeedsToken(head(1000, ""), peer: "10.0.0.2", authorized: ok))
    #expect(!RouterServer.bodyNeedsToken(head(2 << 20, ""), peer: "127.0.0.1", authorized: ok))
    #expect(!RouterServer.bodyNeedsToken(Data("POST / HTTP/1.1\r\ncontent-length: 9999999\r\n".utf8), peer: "10.0.0.2", authorized: ok))
}

/// L1: omejitev povezav brez cele zahteve: 16 z enega naslova, 256 skupaj, lokalne brez omejitve; zaprta sprosti mesto.
@Test func pendingConnectionLimits() {
    var p = RouterServer.Pending()
    let ids = (0..<300).map { _ in NSObject() }
    func admit(_ i: Int, _ peer: String) -> Bool { p.admit(ObjectIdentifier(ids[i]), peer: peer) }
    func remove(_ i: Int) -> Bool { p.remove(ObjectIdentifier(ids[i])) }
    for i in 0..<16 { #expect(admit(i, "10.0.0.2")) }
    #expect(!admit(16, "10.0.0.2"))
    #expect(remove(3) && admit(16, "10.0.0.2"))  // zaprta povezava sprosti mesto
    for i in 17..<57 { #expect(admit(i, "127.0.0.1")) }  // lokalne: brez omejitve, ne štejejo
    var n = 57
    for k in 3..<17 { for _ in 0..<16 { #expect(admit(n, "10.0.\(k).1")); n += 1 } }  // 16 + 14·16 = 240 z drugih
    for k in 0..<16 { #expect(admit(n, "10.1.\(k).1")); n += 1 }  // 256
    #expect(!admit(n, "10.9.9.9"))
    #expect(admit(n, "::1"))
}

/// N8: pod Claude Code --minigw-work= brez veje le samo za branje ali kot shranjen način mape (kot Go TestUnsafeWorkInsideClaudeCode).
@Test func unsafeWorkInsideClaudeCode() {
    let env = ["CLAUDECODE": "1"]
    for w in ["direct", "sync", "native-direct"] {
        #expect(Profiles.unsafeWork(w, saved: nil, readOnly: false, env: env), "\(w)")
        #expect(Profiles.unsafeWork(w, saved: "worktree", readOnly: false, env: env), "\(w)")
        #expect(!Profiles.unsafeWork(w, saved: w, readOnly: false, env: env), "\(w)")
        #expect(!Profiles.unsafeWork(w, saved: nil, readOnly: true, env: env), "\(w)")
    }
    for w in [nil, "auto", "worktree", "native"] { #expect(!Profiles.unsafeWork(w, saved: nil, readOnly: false, env: env)) }
    #expect(!Profiles.unsafeWork("sync", saved: nil, readOnly: false, env: ["CLAUDECODE": "1", "MINIGW_ALLOW_UNSAFE_FLAGS": "1"]))
    #expect(!Profiles.unsafeWork("sync", saved: nil, readOnly: false, env: [:]))
    // L-31: izrecen native ali worktree ne pade na direct (shranjen ali samo za branje da)
    for w in ["native", "worktree"] {
        #expect(!Profiles.noDirectFallback(SharedFolders.candidates(work: w, readOnly: false, smb: false, git: false), flag: w, saved: nil,
                                           readOnly: false, env: env).contains("direct"), "\(w)")
        #expect(Profiles.noDirectFallback([w, "direct"], flag: w, saved: w, readOnly: false, env: env) == [w, "direct"])
        #expect(Profiles.noDirectFallback([w, "direct"], flag: w, saved: nil, readOnly: true, env: env) == [w, "direct"])
    }
    #expect(Profiles.noDirectFallback(["direct"], flag: nil, saved: nil, readOnly: false, env: env) == ["direct"])
    #expect(Profiles.noDirectFallback(["sync", "direct"], flag: "auto", saved: nil, readOnly: false, env: env) == ["sync", "direct"])
}

@Test func unsafeClaudeFlagsInsideClaudeCode() {
    let env = ["CLAUDECODE": "1"]
    for a in [["--dangerously-skip-permissions", "-p", "x"], ["-p", "--permission-mode", "bypassPermissions", "x"],
              ["-p", "--permission-mode=bypassPermissions", "x"], ["-p", "--allowedTools", "Bash", "x"], ["--settings", "s.json", "-p", "x"],
              ["--mcp-config", "m.json"], ["--plugin-dir", "d"], ["--add-dir", "/"], ["-p", "--model", "m", "--allow-dangerously-skip-permissions"],
              ["-pc", "x"], ["-p=1"], ["--Model", "m"], ["-p", "--model", "m", "--input-format", "stream-json"],
              ["-p", "--input-format=text"], ["-r", "id", "-p", "x"], ["--resume", "id"], ["-c", "-p", "x"], ["--session-id", "s", "-p"]] {
        #expect(Profiles.unsafeClaudeFlag(a, env: env) != nil, "\(a)")
    }
    for a in [["-p", "--model", "claude-codex-gpt-6.1-sol", "task"], ["-p", "--model", "--dangerously-skip-permissions", "x"],
              ["-p", "--model=m", "--output-format", "stream-json", "--verbose", "x"], ["-p", "--", "--add-dir"], ["--help"],
              ["-p", "--model", "m", "--", "- fix x"], []] {
        #expect(Profiles.unsafeClaudeFlag(a, env: env) == nil, "\(a)")
    }
    #expect(Profiles.unsafeClaudeFlag(["--dangerously-skip-permissions"], env: [:]) == nil)
    #expect(Profiles.unsafeClaudeFlag(["--dangerously-skip-permissions"], env: ["CLAUDECODE": "1", "MINIGW_ALLOW_UNSAFE_FLAGS": "1"]) == nil)
}

// Odprt stdin, ki se nikoli ne zapre (orodje Bash v Claude Code): minigw -p z navodilom ne čaka na EOF.
@Test func readStdinDoesNotBlockWithPrompt() {
    let open = Pipe(), start = Date()
    #expect(Profiles.readStdin(fd: open.fileHandleForReading.fileDescriptor, waitMs: 200) == nil)
    #expect(Date().timeIntervalSince(start) < 2)
    let piped = Pipe()
    piped.fileHandleForWriting.write(Data("piped text".utf8)); try? piped.fileHandleForWriting.close()
    #expect(Profiles.readStdin(fd: piped.fileHandleForReading.fileDescriptor, waitMs: 3000) == Data("piped text".utf8))
    #expect(Profiles.promptWords(["-p", "--model", "m", "reply pong"]) == ["reply pong"])
    #expect(Profiles.promptWords(["-p", "--model", "m"]).isEmpty)
    _ = open.fileHandleForWriting
}

@Test func unsafePromptInsideClaudeCode() {
    let env = ["CLAUDECODE": "1"], m = ["-p", "--model", "m"]
    for (a, stdin) in [(m + ["/config permissionMode=auto"], ""), (m + ["--", "  /model x"], ""), (m + ["--", "\n/mcp enable x"], ""),
                       (m + ["@~/.ssh/id"], ""), (m + ["--", #"see @"a b""#], ""), (m + ["--", "(@x"], ""), (m + ["--", "x\t@'y'"], ""),
                       (m + ["--", "ok"], "summarize @/etc/passwd"), (m, "/config permissionMode=acceptEdits"), (["-p", "fix", "@/etc/hosts"], "")] {
        #expect(Profiles.unsafePrompt(a, stdin: stdin, env: env) != nil, "\(a) \(stdin)")
    }
    for (a, stdin) in [(m + ["--", "email a@b.com"], ""), (m + ["--", "use 2 / 3"], ""), (m + ["--", "- fix x"], ""),
                       (m + ["task"], "mail to a@b, not to me"), (["-p", "--model", "/m", "--append-system-prompt", "@x", "ok"], ""), ([], "")] {
        #expect(Profiles.unsafePrompt(a, stdin: stdin, env: env) == nil, "\(a) \(stdin)")
    }
    // N6b: ločila pred @, ki jih claude 2.1.318 šteje za začetek besede (JS \s, U+FEFF, 。、？！), tudi na stdin
    for sep in ["\u{a0}", "\u{1680}", "\u{2000}", "\u{2003}", "\u{200a}", "\u{2028}", "\u{2029}", "\u{202f}", "\u{205f}", "\u{3000}",
                "\u{feff}", "\u{0b}", "。", "、", "？", "！"] {
        #expect(Profiles.unsafePrompt(m + ["--", "x\(sep)@/etc/hosts"], stdin: "", env: env) != nil, "\(sep.unicodeScalars.first!.value)")
        #expect(Profiles.unsafePrompt(m, stdin: "x\(sep)@/etc/hosts", env: env) != nil, "\(sep.unicodeScalars.first!.value)")
    }
    for p in ["a\u{200b}@b", "x＠/etc/hosts", "x=@y", "x:@y"] {  // claude jih ne priloži (izmerjeno)
        #expect(Profiles.unsafePrompt(m + ["--", p], stdin: "", env: env) == nil, "\(p)")
    }
    #expect(Profiles.unsafePrompt(["-p", "/config x"], stdin: "", env: [:]) == nil)
    #expect(Profiles.unsafePrompt(["-p", "@/etc/hosts"], stdin: "", env: ["CLAUDECODE": "1", "MINIGW_ALLOW_UNSAFE_FLAGS": "1"]) == nil)
}

/// #44: diff na stdin (»@@«) gre naprej le, ko claude »@« zanesljivo ne bere; »/« na stdin in »@« v argumentih ostaneta zavrnjena.
@Test func unsafePromptStdin() {
    let env = ["CLAUDECODE": "1"], m = ["-p", "--model", "m", "--", "review"]
    let diff = "diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+@~/.ssh/id_ed25519\n"
    #expect(Profiles.unsafePrompt(m, stdin: diff, env: env, stdinMentionsOff: true) == nil)
    #expect(Profiles.unsafePrompt(m, stdin: diff, env: env).map(Profiles.promptRefusal) == "@ before a file name on stdin (claude would attach that file) is not accepted when minigw runs inside Claude Code; remove the @ before file names on stdin or update Claude Code to 2.1.299 or newer")
    #expect(Profiles.unsafePrompt(["-p", "--", "@x"], stdin: "", env: env, stdinMentionsOff: true).map(Profiles.promptRefusal) == "@ before a file name (claude would attach that file) is not accepted when minigw runs inside Claude Code: paste the text itself into the prompt")
    #expect(Profiles.unsafePrompt(["-p", "--", "@x review"], stdin: "", env: env, stdinMentionsOff: true) != nil)
    #expect(Profiles.unsafePrompt(["-p", "--", "ok"], stdin: " /config x", env: env, stdinMentionsOff: true) != nil)
    for (v, want) in ["2.1.299 (Claude Code)": true, "2.1.319": true, "2.2.0 (Claude Code)": true, "10.0.0": true,
                      "2.1.296 (Claude Code)": false, "2.0.999": false, "garbage": false, "": false, "2.1.299-beta": false] {
        #expect(Profiles.mentionGuardVersion(v) == want, "\(v)")
    }
}

/// I1: na poti -v/-h claude nima --settings, zato »@« na stdin nikoli; različico preveri le ob »@«.
@Test func stdinMentionsOffGate() {
    let diff = "@@ -1 +1 @@\n"
    #expect(Profiles.stdinMentionsOff(diff, info: false, guard: true) { "2.1.299 (Claude Code)" })
    #expect(!Profiles.stdinMentionsOff(diff, info: true, guard: true) { "2.1.299" })
    #expect(!Profiles.stdinMentionsOff(diff, info: false, guard: false) { "2.1.299" })
    #expect(!Profiles.stdinMentionsOff(diff, info: false, guard: true) { "2.1.296" })
    #expect(!Profiles.stdinMentionsOff(diff, info: false, guard: true) { "" })
    var asked = 0
    #expect(!Profiles.stdinMentionsOff("no mention", info: false, guard: true) { asked += 1; return "2.1.299" })
    #expect(asked == 0)
}

/// L1: claude je prva navadna izvršljiva datoteka v absolutni mapi PATH (vrstni red kot execvp); mape, neizvršljive
/// datoteke in relativne mape preskoči; nič ustreznega = nil.
@Test func executableOnAbsolutePath() throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent("exe-\(UUID().uuidString)").path
    defer { try? FileManager.default.removeItem(atPath: root) }
    let fm = FileManager.default
    for d in ["dir", "plain", "real", "later"] { try fm.createDirectory(atPath: "\(root)/\(d)", withIntermediateDirectories: true) }
    try fm.createDirectory(atPath: "\(root)/dir/claude", withIntermediateDirectories: true)  // mapa z imenom claude
    fm.createFile(atPath: "\(root)/plain/claude", contents: Data("x".utf8), attributes: [.posixPermissions: 0o644])
    fm.createFile(atPath: "\(root)/real/claude", contents: Data("x".utf8), attributes: [.posixPermissions: 0o755])
    fm.createFile(atPath: "\(root)/later/claude", contents: Data("x".utf8), attributes: [.posixPermissions: 0o755])
    try fm.createSymbolicLink(atPath: "\(root)/link", withDestinationPath: "\(root)/real/claude")
    #expect(Profiles.executable("claude", path: "real:\(root)/dir:\(root)/plain::\(root)/real:\(root)/later") == "\(root)/real/claude")
    #expect(Profiles.executable("link", path: root) == "\(root)/link")  // simbolna povezava na datoteko velja
    #expect(Profiles.executable("claude", path: "\(root)/dir:\(root)/plain:.:real") == nil)
}

@Test func stdinNoteForADroppedPipe() {
    #expect(Profiles.stdinNote(read: nil, waitMs: 3000, mode: S_IFIFO) == "minigw: stdin had no data within 3 s and was not sent")
    #expect(Profiles.stdinNote(read: nil, waitMs: 3000, mode: S_IFREG) != nil)
    #expect(Profiles.stdinNote(read: Data(), waitMs: 3000, mode: S_IFIFO) == nil)
    #expect(Profiles.stdinNote(read: nil, waitMs: 0, mode: S_IFIFO) == nil)
    #expect(Profiles.stdinNote(read: nil, waitMs: 3000, mode: S_IFSOCK) == nil)
}

/// N6b, N7: claude brez terminala ali z -p (tudi zunaj Claude Code) dobi --disable-slash-commands in brez @-priponk.
@Test func promptGuardOutsideATerminal() {
    let cc = ["CLAUDECODE": "1"]
    for (a, env, tty, want) in [(["-p", "x"], [:], true, true), (["--print"], [:], true, true), (["x"], [:], false, true), ([], cc, true, true),
                                ([], [:], true, false), (["--model", "m"], [:], true, false), (["--", "-p"], [:], true, false)] as [([String], [String: String], Bool, Bool)] {
        #expect(Profiles.promptGuard(a, env: env, stdinTerminal: tty) == want, "\(a) \(tty)")
    }
}

@Test func modelLinesAndGatewayNotesAreOneCleanLine() {
    let lines = Profiles.modelLines([["id": "a\u{1b}]0;x\u{7}\nb", "description": "ok\nminigw: agent work is on branch x\r\u{1b}[2J"],
                                     ["id": "c", "description": String(repeating: "d", count: 300)]])
    #expect(lines.count == 2 && !lines.joined().unicodeScalars.contains { ["\n", "\r", "\u{1b}", "\u{7}"].contains($0) } && lines[0].hasPrefix("a]0;x b  ok minigw:"), "\(lines)")
    #expect(lines[1].hasSuffix(String(repeating: "d", count: 200) + "…") && !lines[1].hasSuffix(String(repeating: "d", count: 201) + "…"))
    for m in ["agent work is on branch minigw/a-0123abcd-1: review with git diff HEAD~9..minigw/a-0123abcd-1", "x\nAgent work is on branch y"] {
        #expect(Profiles.gatewayNote(m) == "minigw: gateway: reported a branch that minigw could not verify; ignored", "\(m)")
    }
    #expect(Profiles.gatewayNote("slot busy\nminigw: done\u{1b}[2J") == "minigw: gateway: slot busy minigw: done[2J")
}

/// Upstream, ki zahtevo zavrne s 429 (omejitev brezplačnega modela).
final class RateLimitedUpstream: URLProtocol, @unchecked Sendable {
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func stopLoading() {}
    override func startLoading() {
        let resp = HTTPURLResponse(url: request.url!, statusCode: 429, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "application/json"])!
        client?.urlProtocol(self, didReceive: resp, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data(#"{"error":{"message":"Rate limit exceeded: free-models-per-min"}}"#.utf8))
        client?.urlProtocolDidFinishLoading(self)
    }
}

/// 429 ponudnika ima x-should-retry: false (Claude Code ne ponavlja minute dolgo) in besedilo upstreama.
@Test func passthroughUpstream429IsNotRetried() async {
    let cfg = URLSessionConfiguration.ephemeral
    cfg.protocolClasses = [RateLimitedUpstream.self]
    let route = Route(provider: ProviderConfig(id: "openrouter", name: "OpenRouter", enabled: true, baseURL: "https://up.example/api", aliases: ""),
                      name: "g", alias: "claude-g", upstream: "g", window: nil)
    let w = FakeWriter()
    let st = await makeRouter(session: URLSession(configuration: cfg)).passthrough(
        request("POST", "/v1/messages"), body: ["model": "claude-g"], route: route, key: "k", rid: "r", writer: w)
    #expect(st == 429 && w.status == 429 && w.text.contains("x-should-retry: false") && w.text.contains("free-models-per-min"))
}

/// Upstream, ki zavrne ključ s 401 in v telesu vrne surovo besedilo (#43).
final class UnauthorizedUpstream: URLProtocol, @unchecked Sendable {
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func stopLoading() {}
    override func startLoading() {
        let resp = HTTPURLResponse(url: request.url!, statusCode: 401, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "text/html"])!
        client?.urlProtocol(self, didReceive: resp, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data("<html>secret upstream page</html>".utf8))
        client?.urlProtocolDidFinishLoading(self)
    }
}

/// #43: 401/403 upstreama → 502 brez besedila upstreama; drugače le očiščen error.message, največ 300 bajtov.
@Test func passthroughUpstreamErrorIsSanitized() async {
    let cfg = URLSessionConfiguration.ephemeral
    cfg.protocolClasses = [UnauthorizedUpstream.self]
    let route = Route(provider: ProviderConfig(id: "openrouter", name: "OpenRouter", enabled: true, baseURL: "https://up.example/api", aliases: ""),
                      name: "g", alias: "claude-g", upstream: "g", window: nil)
    let w = FakeWriter()
    let st = await makeRouter(session: URLSession(configuration: cfg)).passthrough(
        request("POST", "/v1/messages"), body: ["model": "claude-g"], route: route, key: "k", rid: "r", writer: w)
    #expect(st == 502 && w.status == 502 && !w.text.contains("secret") && w.text.contains("check the API key (minigw gw provider add OpenRouter)"))

    let long = String(repeating: "é", count: 400)
    let (s1, m1) = Router.upstreamError(status: 500, name: "P", body: Data(#"{"error":{"message":"a\u001b[31m‮b \#(long)"}}"#.utf8))
    #expect(s1 == 500 && m1.hasPrefix("P: a[31mb ") && m1.utf8.count <= 300 + 3)
    let (s2, m2) = Router.upstreamError(status: 302, name: "P", body: Data("moved".utf8))
    #expect(s2 == 502 && m2 == "P: HTTP 302: moved")
    #expect(Router.upstreamError(status: 403, name: "P", body: Data()).0 == 502)
}

/// #73: drevo agenta po teku ni ustavljeno (abandon): klon sync ne gre v kopijo; runDirs da klon za killTree.
@Test func syncAbandonedRunIsNotMerged() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("syncab-" + UUID().uuidString)
    let client = base.appendingPathComponent("client"), copy = base.appendingPathComponent("sync/local/x")
    defer { try? FileManager.default.removeItem(at: base) }
    try FileManager.default.createDirectory(at: client, withIntermediateDirectories: true)
    try Data("v1".utf8).write(to: client.appendingPathComponent("f.txt"))
    let hub = ShareHub()
    let key = try #require(await hub.register(session: "p", cwd: client.path, key: nil, copy: copy))
    let minigw = FakeMinigw(hub, key: key, root: client)
    let server = minigw.serve()
    defer { server.cancel() }
    let d = try #require(await ShareHub.$run.withValue("r1") { await hub.mount(session: "p", cwd: client.path, base: "") })
    try Data("agent".utf8).write(to: d.appendingPathComponent("f.txt"))
    #expect(await ShareHub.$run.withValue("r1") { await hub.runDirs() }.contains(d.path))
    await ShareHub.$run.withValue("r1") { await hub.abandon() }
    await hub.finish(run: "r1")
    #expect(try String(contentsOf: copy.appendingPathComponent("f.txt"), encoding: .utf8) == "v1")
    #expect(!FileManager.default.fileExists(atPath: copy.appendingPathComponent("f.txt.minigw-agent").path))
}

@Test func syncSummarySkipsUnapplied() {
    var ch = Sync.Changes()
    ch.changed = [Sync.Entry(path: "a", size: 1, mtime: 0, sha256: "x"), Sync.Entry(path: ".git/x", size: 1, mtime: 0, sha256: "y")]
    ch.deleted = ["b", ".git/y"]
    #expect(Sync.summary(ch, notes: ["skipped .git/x: protected path", "not deleted .git/y: protected path"])
        == "synced back from the gateway: 1 changed, 1 deleted")
    ch.changed.removeFirst(); ch.deleted.removeFirst()
    #expect(Sync.summary(ch, notes: ["skipped .git/x: protected path", "not deleted .git/y: protected path"]) == nil)
    #expect(Sync.summary(Sync.Changes(), notes: []) == nil)
}

@Test func shareAddRefusesHomeAndRoot() throws {
    let tmp = Sync.real(FileManager.default.temporaryDirectory)!.appendingPathComponent("share-\(UUID().uuidString)")
    let home = tmp.appendingPathComponent("home"), proj = home.appendingPathComponent("proj"), link = tmp.appendingPathComponent("link")
    try FileManager.default.createDirectory(at: proj, withIntermediateDirectories: true)
    try FileManager.default.createSymbolicLink(at: link, withDestinationURL: home)
    defer { try? FileManager.default.removeItem(at: tmp) }
    let refused = { (d: String, h: String) in SharedFolders.shareRefusal(d, home: h) != nil }
    #expect(refused("/", home.path))
    #expect(refused(home.path, home.path))
    #expect(refused(link.path, home.path))  // povezava na dom
    #expect(refused(home.path, link.path))
    #expect(refused(tmp.path, home.path))  // vsebuje dom (#84)
    #expect(refused(home.appendingPathComponent("missing").path, home.path))  // ni istovetnosti: zavrne
    #expect(!refused(proj.path, home.path))
    #expect(SharedFolders.shareCheck(proj.path, home: home.path).id == SharedFolders.fileID(proj.path))  // preverjena istovetnost za start
    #expect(!refused(link.appendingPathComponent("proj").path, home.path))
    // #84: ob uporabi mapa s seznama, ki jo zdaj zavrne (dodana pred 0.5.25), ni deljena; projekt je
    let list = [SharedFolders.Entry(path: tmp.path), SharedFolders.Entry(path: proj.path)]
    let other = SharedFolders.lookupAllowed(list, cwd: tmp.appendingPathComponent("other").path, home: home.path)
    #expect(other.entry == nil && other.refused?.contains("not sharing \(tmp.path)") == true)
    #expect(SharedFolders.lookupAllowed(list, cwd: proj.appendingPathComponent("src").path, home: home.path).entry?.path == proj.path)
}

/// #84: firmlink /System/Volumes/Data/Users/<ime> je dom (isti st_dev/st_ino, realpath ga pusti), /System/Volumes/Data
/// je koren nosilca, /Users vsebuje dom; projekt pod aliasom je dovoljen.
@Test func shareRefusesMacFirmlinks() throws {
    let home = NSHomeDirectory(), data = "/System/Volumes/Data"
    guard FileManager.default.fileExists(atPath: data + home) else { return }
    for d in [data + home, data, data + "/Users", "/Users", home, "/"] { #expect(SharedFolders.shareRefusal(d) != nil, "\(d)") }
    let proj = data + home + "/minigw-share-id-test-\(UUID().uuidString)"
    try FileManager.default.createDirectory(atPath: proj, withIntermediateDirectories: false)
    defer { try? FileManager.default.removeItem(atPath: proj) }
    #expect(SharedFolders.shareRefusal(proj) == nil)
    // drug disk (/Volumes/<Disk>, koren nosilca brez doma ali sistema) je dovoljen
    for v in (try? FileManager.default.contentsOfDirectory(atPath: "/Volumes")) ?? [] where !v.hasPrefix(".") && v != "Recovery" {
        // lstat: povezava (Macintosh HD → /) ne; brez attributesOfItem, ki bere xattr in na nosilcu Time Machine obvisi
        let p = "/Volumes/" + v; var st = stat()
        guard lstat(p, &st) == 0, st.st_mode & S_IFMT == S_IFDIR, st.st_uid == getuid() else { continue }
        #expect(SharedFolders.shareRefusal(p) == nil, "\(p)")
    }
}

/// #82: SYNC-PULL, zavrnjen s 503, nič ne prevzame; naslednji prenos in prevzem dostavita obe spremembi natanko enkrat
/// (združitev klona v kopijo, merge(r), ju ne uveljavi dvakrat: brez sporov, brez kopij »minigw-agent«).
@Test func syncRefusedPullDeliveredOnceByNextRequest() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("syncbusy-" + UUID().uuidString)
    let client = base.appendingPathComponent("client"), copy = base.appendingPathComponent("sync/local/x")
    defer { try? FileManager.default.removeItem(at: base) }
    func put(_ root: URL, _ p: String, _ s: String) throws {
        try FileManager.default.createDirectory(at: root.appendingPathComponent(p).deletingLastPathComponent(), withIntermediateDirectories: true)
        try Data(s.utf8).write(to: root.appendingPathComponent(p))
    }
    func read(_ root: URL, _ p: String) -> String? { (try? Data(contentsOf: root.appendingPathComponent(p))).map { String(decoding: $0, as: UTF8.self) } }
    try FileManager.default.createDirectory(at: client, withIntermediateDirectories: true)
    try put(client, "a.txt", "v1"); try put(client, "b.txt", "v1")
    let hub = ShareHub()
    let key = try #require(await hub.register(session: "p", cwd: client.path, key: nil, copy: copy))
    let minigw = FakeMinigw(hub, key: key, root: client)
    let server = minigw.serve()
    defer { server.cancel() }
    func mount(_ run: String) async -> URL? { await ShareHub.$run.withValue(run) { await hub.mount(session: "p", cwd: client.path, base: "") } }
    func pull(_ run: String) async -> String? {
        let why = await ShareHub.$run.withValue(run) { await hub.syncBack(session: "p") }
        await hub.finish(run: run)
        return why
    }

    let d1 = try #require(await mount("r1"))
    try put(d1, "a.txt", "agent1"); try put(d1, "n1.txt", "new1")
    minigw.refusePull = true
    let why = await pull("r1")
    #expect(why?.contains("the shared folder is busy") == true && why?.contains("only the next agent request") == true, "\(why ?? "nil")")
    #expect(read(client, "a.txt") == "v1" && read(client, "n1.txt") == nil)
    minigw.refusePull = false
    let d2 = try #require(await mount("r2"))
    #expect(read(d2, "a.txt") == "agent1" && read(d2, "n1.txt") == "new1")  // agent nadaljuje od svojih sprememb
    try put(d2, "b.txt", "agent2"); try put(d2, "n2.txt", "new2")
    #expect(await pull("r2") == nil)
    for (p, v) in [("a.txt", "agent1"), ("n1.txt", "new1"), ("b.txt", "agent2"), ("n2.txt", "new2")] { #expect(read(client, p) == v, "\(p)") }
    let left = (try? FileManager.default.contentsOfDirectory(atPath: client.path)) ?? []
    #expect(!left.contains { $0.contains("minigw-") }, "\(left)")
    #expect(!minigw.notes.contains { $0.hasPrefix("conflict:") }, "\(minigw.notes)")
    // Tretji prevzem brez dela agenta nič ne uveljavi znova.
    let before = minigw.notes.count
    _ = try #require(await mount("r3"))
    #expect(await pull("r3") == nil)
    #expect(minigw.notes.count == before && read(client, "a.txt") == "agent1")
}
