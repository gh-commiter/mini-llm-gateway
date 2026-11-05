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
    guard case .request(let r) = HTTPParser.parse(Data(raw.utf8)) else { fatalError("parse failed") }
    return r
}

func makeRouter(session: URLSession = .shared) -> Router {
    var c = RouterConfig()
    c.providers = [
        ProviderConfig(id: "openrouter", name: "OpenRouter", enabled: true, baseURL: "https://up.example/api/",
                       aliases: "or-qwen = qwen/qwen3-coder, 1k\n# komentar\nanthropic/claude-sonnet"),
        ProviderConfig(id: "litellm", name: "LiteLLM", enabled: false, baseURL: "http://x", aliases: "qwen-local = q"),
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
    #expect(ProviderConfig.publishedID("anthropic/claude-x") == "anthropic/claude-x")
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




@Test func cliModelMatching() {
    #expect(ClaudeCLI.match("claude-opus-5-5")! == ("claude-opus-5-5", false))
    #expect(ClaudeCLI.match("haiku")! == ("haiku", false))
    #expect(ClaudeCLI.match("claude-chat-sonnet")! == ("sonnet", true))
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
    let w2 = FakeWriter()
    await on.handle(request("GET", "/v1/models", headers: ["x-api-key": "tok"]), writer: w2)
    #expect(w2.text.contains("claude-chat-opus") && w2.text.contains("claude-chat-haiku"))
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
        #expect(w.text.contains("\"claude-or-qwen\"") && w.text.contains("\"anthropic/claude-sonnet\""))
        #expect(!w.text.contains("\"or-qwen\"") && !w.text.contains("qwen-local"))
        #expect(w.text.contains("OpenRouter · qwen/qwen3-coder · 1K"))
    }

    @Test func discoversModelsWhenAliasesEmpty() async {
        let cfg = URLSessionConfiguration.ephemeral
        cfg.protocolClasses = [StubUpstream.self]
        var c = RouterConfig()
        c.providers = [ProviderConfig(id: "litellm", name: "LiteLLM", enabled: true, baseURL: "http://lite", aliases: "")]
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
        c.providers = [ProviderConfig(id: "or", name: "OR", enabled: true, baseURL: "http://or", aliases: "coder = qwen/qwen3-coder")]
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
            c.providers = [ProviderConfig(id: "or", name: "OR", enabled: true, baseURL: "http://or", aliases: "", enabledModels: enabled)]
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

@Test func claudeExecutableResolution() {
    #expect(ClaudeCLI.executable("/nonexistent/claude") == nil)  // ročna pot nima rezerve
    #expect(ClaudeCLI.executable("/bin/ls") == "/bin/ls")
}

@Test func claudeExecutableValidation() {
    if case .failure(let e) = ClaudeCLI.validate("/nonexistent") { #expect(e.message == "File does not exist") } else { Issue.record("expected failure") }
    if case .failure(let e) = ClaudeCLI.validate("/etc/hosts") { #expect(e.message == "File is not executable") } else { Issue.record("expected failure") }
    if case .failure(let e) = ClaudeCLI.validate("/bin/ls") { #expect(e.message == "Not a Claude Code executable") } else { Issue.record("expected failure") }
    if let real = ClaudeCLI.detected, case .success(let v) = ClaudeCLI.validate(real) { #expect(v.contains("Claude Code")) }
}

@Test func firstOfDoesNotWaitForSlowTask() async {
    let slow = Task { try? await Task.sleep(for: .seconds(3)); return 1 }
    let t0 = ContinuousClock.now
    #expect(await firstOf(slow, within: .milliseconds(100)) == nil)
    #expect(ContinuousClock.now - t0 < .seconds(1))
    #expect(await firstOf(Task { 7 }, within: .seconds(1)) == 7)
}

@Test func configRoundTripsAsReadablePlistAndMigratesJSON() throws {
    let d = UserDefaults(suiteName: "test-\(UUID())")!
    var c = RouterConfig()
    c.port = 9999
    c.providers[1].enabledModels = ["a/b"]
    c.siri.enabled = true
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
    if case .failure(let f) = await Discovery.load(bad, key: "k") { #expect(f.message == "Invalid base URL") } else { Issue.record("url") }
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

@Test func encryptedProxyToRouter() async throws {
    var c = RouterConfig()
    c.host = "127.0.0.1"; c.port = 18_765
    let key = Noise.newKey()
    let server = RouterServer(router: Router(config: c, token: "tok", secrets: FakeSecrets(values: [:])), key: key)
    try server.start()
    defer { server.stop() }
    try await Task.sleep(for: .milliseconds(200))

    func get(_ base: String, _ path: String) async -> (Int, String) {
        var r = URLRequest(url: URL(string: base + path)!, timeoutInterval: 5)
        r.setValue("Bearer tok", forHTTPHeaderField: "authorization")
        guard let (d, resp) = try? await URLSession.shared.data(for: r) else { return (0, "") }
        return ((resp as! HTTPURLResponse).statusCode, String(decoding: d, as: UTF8.self))
    }
    let proxy = try #require(NoiseProxy(routerURL: "http://127.0.0.1:18765", routerKey: key.publicKey.rawRepresentation))
    try proxy.start()
    defer { proxy.stop() }
    let (s, body) = await get(proxy.localURL, "/v1/models")
    #expect(s == 200 && body.contains("\"data\""))

    let bad = try #require(NoiseProxy(routerURL: "http://127.0.0.1:18765", routerKey: Noise.newKey().publicKey.rawRepresentation))
    try bad.start()
    defer { bad.stop() }
    #expect(await get(bad.localURL, "/v1/models").0 == 0)  // napačen ključ: router prekine
}
