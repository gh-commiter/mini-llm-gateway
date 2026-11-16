import Foundation
import Testing
@testable import RouterCore

// Failed agent time (0.6.7), the same rules as TestEventDurationAndFailureKinds (client/serve_usage_fail_test.go): execution time runs from
// dispatch, an upstream stream that breaks after HTTP 200 is an error with a bounded kind, the upstream's own status is kept.

private final class FailTimeUpstream: URLProtocol, @unchecked Sendable {
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func stopLoading() {}
    override func startLoading() {
        let url = request.url!, c = client!
        DispatchQueue.global().asyncAfter(deadline: .now() + 0.1) { [self] in
            let status = url.path.hasPrefix("/403") ? 403 : 200
            c.urlProtocol(self, didReceive: HTTPURLResponse(url: url, statusCode: status, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": status == 200 ? "text/event-stream" : "application/json"])!, cacheStoragePolicy: .notAllowed)
            c.urlProtocol(self, didLoad: Data((status == 200 ? "event: message_start\ndata: {}\n\n" : #"{"error":{"message":"spending limit"}}"#).utf8))
            if url.path.hasPrefix("/broken") { c.urlProtocol(self, didFailWithError: URLError(.networkConnectionLost)) } else { c.urlProtocolDidFinishLoading(self) }
        }
    }
}

@Test func failedTimeOfStreamsErrorsAndAgentsReachTheCore() async throws {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("mgfail-\(UUID().uuidString)")
    let store = base.appendingPathComponent("core")
    try CoreHelper.prepareStoreFolder(store)
    defer { try? FileManager.default.removeItem(at: base) }
    let helper = CoreHelper(binary: try await HelperBuild.shared.binary(), store: store, marks: MemoryMarks())
    helper.start(); defer { helper.stop() }
    #expect(await helper.waitReady(timeout: 20))
    let config = URLSessionConfiguration.ephemeral
    config.protocolClasses = [FailTimeUpstream.self]
    let session = URLSession(configuration: config)
    defer { session.invalidateAndCancel() }
    var rc = RouterConfig(); rc.providers = []
    let router = Router(config: rc, token: "test", secrets: FakeSecrets(values: [:]), session: session, core: helper)
    func route(_ path: String) -> Route {
        Route(provider: ProviderConfig(id: "gw", name: "GW", enabled: true, baseURL: "https://up.example" + path, aliases: ""), name: "m", alias: "m", upstream: "m", window: nil)
    }
    for (model, path) in [("ok", "/ok"), ("403", "/403"), ("broken", "/broken")] {
        let state = RequestState()
        state.admit(UsageEvent(id: "ev_\(model)", start: Date().addingTimeInterval(-3600), model: model))  // admitted an hour ago: a queue is not execution time
        let writer = UsageWriter(FakeWriter())
        let status = await Router.$reqState.withValue(state) {
            await router.passthrough(request("POST", "/v1/messages"), body: ["model": model], route: route(path), key: "k", rid: "r", writer: writer)
        }
        router.endEvent(state, status: status, usage: writer)
    }
    for (model, kind) in [("agent-exit", "exit"), ("agent-limit", "limit")] {  // an agent that failed behind a 200
        let state = RequestState()
        state.admit(UsageEvent(id: "ev_\(model)", start: Date(), model: model))
        _ = state.dispatch(provider: "codex")
        state.fail(kind)
        router.endEvent(state, status: 200, usage: nil)
    }
    let gone = RequestState()  // the client went away while the stream was written
    gone.admit(UsageEvent(id: "ev_gone", start: Date(), model: "gone"))
    _ = gone.dispatch(provider: "gw")
    gone.markClientGone()
    router.endEvent(gone, status: 200, usage: nil)
    #expect(await router.usageSender.flush())
    var rows: [String: [String: Any]] = [:]
    for _ in 0..<100 {
        let r = await helper.call(context: .admin, request: CoreRequest(method: "GET", path: "/usage/events"))
        let days = ((try? JSONSerialization.jsonObject(with: r.body)) as? [String: Any])?["days"] as? [[String: Any]] ?? []
        rows = Dictionary(days.map { ($0["model"] as? String ?? "", $0) }, uniquingKeysWith: { a, _ in a })
        if rows.count == 6 { break }
        try await Task.sleep(for: .milliseconds(20))
    }
    #expect(rows.count == 6)
    func n(_ model: String, _ key: String) -> Int { rows[model]?[key] as? Int ?? -1 }
    #expect(n("ok", "errorEvents") == 0 && n("ok", "failedEvents") == 0)
    for model in ["403", "broken"] { #expect(n(model, "errorEvents") == 1 && n(model, "failedEvents") == 1, "\(model)") }
    for model in ["agent-exit", "agent-limit"] { #expect(n(model, "errorEvents") == 1, "\(model)") }
    #expect(n("gone", "errorEvents") == 0 && n("gone", "cancelledEvents") == 1)
    for model in ["ok", "403", "broken"] {  // the hour before dispatch is not in the duration; the stub answers after 100 ms
        let d = n(model, "durationMs")
        #expect(d >= 50 && d < 600_000 && n(model, "unknownDurationEvents") == 0, "\(model): \(d) ms")
    }
    #expect(n("broken", "failedDurationMs") == n("broken", "durationMs") && n("ok", "failedDurationMs") == 0)
}

@Test func usageEventWireCarriesDurationAndKind() {
    var e = UsageEvent(id: "ev_x", start: Date(), model: "m")
    e.outcome = "error"; e.durationMs = 3_480_000; e.upstreamStatus = 403; e.errorKind = "http"
    #expect(e.json["durationMs"] as? Int64 == 3_480_000 && e.json["upstreamStatus"] as? Int == 403 && e.json["errorKind"] as? String == "http")
    e.outcome = "ok"
    #expect(e.json["errorKind"] == nil)  // a kind belongs to errors only
    #expect(UsageEvent(id: "ev_y", start: Date(), model: "m").json["durationMs"] == nil)  // unknown stays absent, never 0
}

@Test func failedTimeMeasure() throws {
    let rows = (1...3).map { i in
        "{\"day\":\"2026-11-15\",\"provider\":\"p\",\"model\":\"m\(i)\",\"events\":4,\"failedEvents\":\(i),\"errorEvents\":\(i),\"tokensIn\":1,\"tokensOut\":1,\"reportedCostUsd\":null,\"estimatedCostUsd\":null,\"eventsWithoutCost\":4,\"failedDurationMs\":\(i * 600_000),\"durationMs\":3000000,\"unknownDurationEvents\":\(i == 3 ? 2 : 0)}"
    }
    let chart = UsageChart(body: Data("{\"days\":[\(rows.joined(separator: ","))],\"gaps\":[],\"timezone\":\"UTC\"}".utf8))!
    let s = chart.summary(.failedTime)
    #expect(s.total == 3_600_000 && s.totalText == "60.0" && s.durationUnknown == 2)
    #expect(s.top.map(\.model) == ["m3", "m2", "m1"] && s.top[0].text == "30.0")
    #expect(s.total == s.bars.reduce(0) { $0 + $1.value } && s.costUnknown == 0)
    #expect(UsageChart.axisText(.failedTime, 3_480_000) == "58.0")
    #expect(chart.summary(.tokens).durationUnknown == 0)
}
