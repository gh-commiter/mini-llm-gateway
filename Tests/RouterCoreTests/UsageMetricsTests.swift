import Foundation
import Testing
@testable import RouterCore

private func usageFixture(_ name: String) throws -> Data {
    let root = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
    return try Data(contentsOf: root.appendingPathComponent("client/testdata/" + name))
}

@Test func sharedUsageMetricsFixtures() throws {
    struct Case: Decodable { let name: String, body: String; let input: Int?, output: Int?, cost: Int64? }
    let cases = try JSONDecoder().decode([Case].self, from: usageFixture("usage-metrics.json"))
    for c in cases {
        let data = Data(c.body.utf8)
        for split in 0...data.count {
            var m = UsageMetrics()
            m.feed(data.prefix(split)); m.feed(data.suffix(data.count - split))
            #expect(m.input == c.input && m.output == c.output && m.cost == c.cost, "\(c.name), split \(split)")
        }
        var m = UsageMetrics()
        for b in data { m.feed(Data([b])) }
        #expect(m.input == c.input && m.output == c.output && m.cost == c.cost, "\(c.name), byte chunks")
    }
}

@Test func sharedUpstreamErrorsFixtures() throws {
    struct Case: Decodable { let name: String, provider: String, baseURL: String, body: String, message: String; let status: Int; let noRetry: Bool; let key: String? }
    let cases = try JSONDecoder().decode([Case].self, from: usageFixture("upstream-errors.json"))
    for c in cases {
        let body = Data(c.body.utf8)
        let (status, message) = Router.upstreamError(status: c.status, name: c.provider, baseURL: c.baseURL, body: body, key: c.key ?? "")
        #expect(status == 502 && message == c.message, "\(c.name)")
        #expect(spendingLimit(baseURL: c.baseURL, body: body) == c.noRetry, "\(c.name)")
    }
}

@Test func usageMetricsSkipsLargeContent() {
    var m = UsageMetrics()
    m.feed(Data((#"{"content":[{"text":""# + String(repeating: "x", count: 2 << 20) + #""}],"usage":{"input_tokens":1,"cost":0}}"#).utf8))
    #expect(m.input == 1 && m.cost == 0)
}

@Test func usageMetricsRecoversAfterUnsupportedSSE() {
    for frame in [
        #"{"usage":{"padding":""# + String(repeating: "x", count: 65536) + #"","input_tokens":999,"cost":100}}"#,
        String(repeating: "[", count: 129),
    ] {
        var m = UsageMetrics()
        m.feed(Data(("data: " + frame + "\n\n").utf8))
        #expect(m.input == nil && m.cost == nil)
        m.feed(Data("data: {\"usage\":{\"input_tokens\":2,\"cost\":0}}\n\n".utf8))
        #expect(m.input == 2 && m.cost == 0)
    }
}

@Test func usageEventCostWire() throws {
    let e = UsageEvent(id: "ev_test", start: Date(), model: "alias", upstreamModel: "upstream", costUsdNanos: 0, costSource: "reported")
    #expect(e.json["costUsdNanos"] as? Int64 == 0)
    #expect(e.json["upstreamModel"] as? String == "upstream")
    let state = RequestState(); state.admit(e)
    #expect(state.dispatch(provider: "openrouter", upstreamModel: "routed")?.upstreamModel == "routed")
    #expect(state.dispatch(provider: "other", upstreamModel: "changed") == nil)
    #expect(state.event?.model == "alias" && state.event?.upstreamModel == "routed")
}

private final class SpendingLimitUpstream: URLProtocol, @unchecked Sendable {
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func stopLoading() {}
    override func startLoading() {
        let response = HTTPURLResponse(url: request.url!, statusCode: 403, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "application/json"])!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data(#"{"error":{"code":"spending_limit_reached","message":"private detail"}}"#.utf8))
        client?.urlProtocolDidFinishLoading(self)
    }
}

@Test func spendingLimitPassthroughDoesNotRetry() async {
    let config = URLSessionConfiguration.ephemeral
    config.protocolClasses = [SpendingLimitUpstream.self]
    let session = URLSession(configuration: config)
    // not invalidated: the router's catalog may still start a discovery request after the test body (an invalidated session raises an uncatchable NSGenericException and ends the whole run)
    let route = Route(provider: ProviderConfig(id: "openrouter", name: "OpenRouter", enabled: true, baseURL: "https://openrouter.ai/api", aliases: ""),
                      name: "alias", alias: "claude-alias", upstream: "routed", window: nil)
    let state = RequestState()
    state.admit(UsageEvent(id: "ev_limit", start: Date(), model: "claude-alias"))
    let writer = FakeWriter()
    let status = await Router.$reqState.withValue(state) {
        await makeRouter(session: session).passthrough(request("POST", "/v1/messages"), body: ["model": "claude-alias"], route: route, key: "test", rid: "r", writer: writer)
    }
    #expect(status == 502 && writer.status == 502)
    #expect(writer.text.contains("x-should-retry: false") && writer.text.contains("OpenRouter: spending limit reached"))
    #expect(!writer.text.contains("private detail"))
    #expect(state.event?.model == "claude-alias" && state.event?.upstreamModel == "routed" && state.event?.provider == "openrouter")
}

@Test func completedUsageMetricsReachSharedCore() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("mgmetrics-\(UUID().uuidString)")
    let store = base.appendingPathComponent("core")
    try CoreHelper.prepareStoreFolder(store)
    defer { try? FileManager.default.removeItem(at: base) }
    let helper = CoreHelper(binary: try await HelperBuild.shared.binary(), store: store, marks: MemoryMarks())
    helper.start(); defer { helper.stop() }
    #expect(await helper.waitReady(timeout: 20))
    var config = RouterConfig(); config.providers = []
    let router = Router(config: config, token: "test", secrets: FakeSecrets(values: [:]), core: helper)
    for model in ["reported", "missing", "subscription"] {
        let state = RequestState()
        state.admit(UsageEvent(id: "ev_\(model)", start: Date(), model: model))
        state.tokenSource = model == "subscription" ? "estimated" : ""
        let event = state.dispatch(provider: "p", upstreamModel: model == "subscription" ? "" : "routed")!
        router.usageSender.begin(event)
        let writer = UsageWriter(FakeWriter())
        writer.scan(Data((model == "missing" ? #"{"usage":{"input_tokens":0}}"# : #"{"usage":{"input_tokens":0,"output_tokens":4,"cost":0}}"#).utf8))
        router.endEvent(state, status: 200, usage: writer)
    }
    #expect(await router.usageSender.flush())
    var rows: [[String: Any]] = []
    // A row exists from the first event of its model; its numbers arrive with the finished one: wait for the numbers, not for the rows (a loaded machine showed 0).
    for _ in 0..<500 {
        let response = await helper.call(context: .admin, request: CoreRequest(method: "GET", path: "/usage/events"))
        rows = ((try? JSONSerialization.jsonObject(with: response.body)) as? [String: Any])?["days"] as? [[String: Any]] ?? []
        if rows.count == 3, ["reported", "subscription"].allSatisfy({ m in rows.first { $0["model"] as? String == m }?["tokensOut"] as? Int == 4 }) { break }
        try await Task.sleep(for: .milliseconds(20))
    }
    #expect(rows.count == 3)
    let reported = rows.first { $0["model"] as? String == "reported" }
    #expect(reported?["reportedCostUsd"] as? String == "0.000000000")
    #expect(reported?["tokensIn"] as? Int == 0 && reported?["tokensOut"] as? Int == 4)
    for model in ["missing", "subscription"] {
        let row = rows.first { $0["model"] as? String == model }
        #expect(row?["reportedCostUsd"] is NSNull)
        #expect(row?["eventsWithoutCost"] as? Int == 1)
    }
    #expect(rows.first { $0["model"] as? String == "subscription" }?["tokensOut"] as? Int == 4)
}

/// Same rules as TestEventTokensFallbackAndAgentZero (client/serve_usage_test.go).
@Test func usageTokensFallbackAndAgentZero() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("mgmetrics-\(UUID().uuidString)")
    let store = base.appendingPathComponent("core")
    try CoreHelper.prepareStoreFolder(store)
    defer { try? FileManager.default.removeItem(at: base) }
    let helper = CoreHelper(binary: try await HelperBuild.shared.binary(), store: store, marks: MemoryMarks())
    helper.start(); defer { helper.stop() }
    #expect(await helper.waitReady(timeout: 20))
    var config = RouterConfig(); config.providers = []
    let router = Router(config: config, token: "test", secrets: FakeSecrets(values: [:]), core: helper)
    let cases: [(name: String, body: String, upstream: String, tokens: Int?)] = [
        ("fallback", #"{"x":{"usage":{"input_tokens":7,"output_tokens":3}}}"#, "routed", 7),
        ("nothing", #"{"content":[]}"#, "routed", nil),
        ("provider-zero", #"{"usage":{"input_tokens":0,"output_tokens":0}}"#, "routed", 0),
        ("agent-zero", #"{"message":{"usage":{"input_tokens":0,"output_tokens":0}}}"#, "", nil),
        ("agent-counts", #"{"usage":{"input_tokens":4,"output_tokens":2}}"#, "", 4),
    ]
    for c in cases {
        let state = RequestState()
        state.admit(UsageEvent(id: "ev_\(c.name)", start: Date(), model: c.name))
        router.usageSender.begin(state.dispatch(provider: "p", upstreamModel: c.upstream)!)
        let writer = UsageWriter(FakeWriter())
        writer.scan(Data(c.body.utf8))
        router.endEvent(state, status: 200, usage: writer)
    }
    #expect(await router.usageSender.flush())
    var rows: [[String: Any]] = []
    for _ in 0..<100 {
        let response = await helper.call(context: .admin, request: CoreRequest(method: "GET", path: "/usage/events"))
        rows = ((try? JSONSerialization.jsonObject(with: response.body)) as? [String: Any])?["days"] as? [[String: Any]] ?? []
        // A row appears with an event's start; its token counts come with the event's end. Wait for the last case's end,
        // not only for the row (the gate of 0.11.0 once read the row of "agent-counts" before its end was stored).
        if rows.count == cases.count, rows.first(where: { $0["model"] as? String == "agent-counts" })?["tokensIn"] as? Int == 4 { break }
        try await Task.sleep(for: .milliseconds(20))
    }
    #expect(rows.count == cases.count)
    for c in cases {
        let row = rows.first { $0["model"] as? String == c.name }
        #expect(row?["tokensIn"] as? Int == (c.tokens ?? 0), "\(c.name)")
        #expect(row?["eventsWithoutTokens"] as? Int == (c.tokens == nil ? 1 : 0), "\(c.name)")
    }
}

/// The Mac gateway forwards the label headers (cut at 128 characters) and the core applies the real limits: a value that is out of
/// limit or not a safe token is stored empty, and the request still counts. A change in either place is caught here.
@Test func labelHeadersAreCheckedByTheCoreNotJustTheRouter() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("mglabels-\(UUID().uuidString)")
    let store = base.appendingPathComponent("core")
    try CoreHelper.prepareStoreFolder(store)
    defer { try? FileManager.default.removeItem(at: base) }
    let helper = CoreHelper(binary: try await HelperBuild.shared.binary(), store: store, marks: MemoryMarks())
    helper.start(); defer { helper.stop() }
    #expect(await helper.waitReady(timeout: 20))
    var config = RouterConfig(); config.providers = []
    let router = Router(config: config, token: "test", secrets: FakeSecrets(values: [:]), core: helper)
    let cases: [(model: String, headers: [String: String])] = [
        ("good", ["x-claude-mini-phase": "Check", "x-claude-mini-activity": "bugfix", "x-claude-mini-task": "feature/x", "x-claude-mini-task-source": "branch"]),
        ("bad", ["x-claude-mini-phase": "not a token!", "x-claude-mini-activity": String(repeating: "a", count: 40),
                 "x-claude-mini-task": String(repeating: "t", count: 130), "x-claude-mini-task-source": "classified"]),
    ]
    for c in cases {
        let state = RequestState()
        await router.admitEvent(request("POST", "/v1/messages", headers: c.headers, body: "{}"), model: c.model, state: state)
        let event = try #require(state.dispatch(provider: "p", upstreamModel: "routed"))
        router.usageSender.begin(event)
        let writer = UsageWriter(FakeWriter())
        writer.scan(Data(#"{"usage":{"input_tokens":1,"output_tokens":1}}"#.utf8))
        router.endEvent(state, status: 200, usage: writer)
    }
    #expect(await router.usageSender.flush())
    var rows: [[String: Any]] = []
    for _ in 0..<500 {
        let response = await helper.call(context: .admin, request: CoreRequest(method: "GET", path: "/usage/events", query: "days=1&labels=1"))
        rows = ((try? JSONSerialization.jsonObject(with: response.body)) as? [String: Any])?["days"] as? [[String: Any]] ?? []
        if rows.count == 2, rows.allSatisfy({ $0["tokensOut"] as? Int == 1 }) { break }
        try await Task.sleep(for: .milliseconds(20))
    }
    let good = try #require(rows.first { $0["model"] as? String == "good" })
    #expect(good["phase"] as? String == "check" && good["activity"] as? String == "bugfix" && good["task"] as? String == "feature/x" && good["taskSource"] as? String == "branch")
    let bad = try #require(rows.first { $0["model"] as? String == "bad" })
    for key in ["phase", "activity", "task", "phaseSource", "activitySource", "taskSource"] { #expect(bad[key] == nil, "\(key) stored for an unsafe value") }
    #expect(bad["events"] as? Int == 1)
}
