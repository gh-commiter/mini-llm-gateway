import Foundation
import Testing
@testable import RouterCore

@Suite struct PricingTests {
    @Test func sharedRatesAndAliases() throws {
        let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let f = try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("client/testdata/pricing.json"))) as! [String: Any]
        for c in f["rates"] as! [[String: Any]] {
            let p = PriceRates.parse(c["price"] as! [String: Any], openRouter: c["openRouter"] as? Bool ?? false)
            if let input = c["input"] as? String {
                #expect(p != nil, "\(c["name"]!)")
                #expect(p.flatMap { Decimal(string: $0.input) } == Decimal(string: input))
                #expect(p.flatMap { Decimal(string: $0.output) } == Decimal(string: c["output"] as! String))
            } else { #expect(p == nil, "\(c["name"]!)") }
        }
        let prices = deploymentPrices(f["deployments"] as! [[String: Any]])
        #expect(prices.count == 2)
        #expect(prices["null-fallback"] == nil && prices["unmapped"] == nil && prices["ambiguous"] == nil)
        #expect(prices["custom"] == PriceRates(input: "0.000009", output: "0.000008"))
        #expect(prices["alias"] == PriceRates(input: "0.000003", output: "0.000006"))
        for c in f["usage"] as! [[String: Any]] {
            let data = Data((c["body"] as! String).utf8)
            for split in 0...data.count {
                var m = UsageMetrics()
                m.feed(data.prefix(split)); m.feed(data.suffix(data.count - split))
                #expect(m.cacheUsed == c["unpriced"] as! Bool)
            }
        }
    }

    @Test func dispatchFreezesPriceAndBilling() {
        let state = RequestState()
        let price = PriceSnapshot(provider: "litellm", model: "base", source: "/model/info", rates: PriceRates(input: "0", output: "0"))
        state.admit(UsageEvent(id: "event", start: Date(), model: "alias"))
        let e = state.dispatch(provider: "litellm", upstreamModel: "base", price: price)
        #expect(e?.priceSnapshot == price)
        #expect(e?.billingMode == "metered")
        #expect(e?.json["priceSnapshot"] != nil)
        #expect(state.dispatch(provider: "other") == nil)
        #expect(state.event?.priceSnapshot == price)
        let cli = RequestState()
        cli.admit(UsageEvent(id: "cli", start: Date(), model: "claude-opus"))
        #expect(cli.dispatch(provider: "claude")?.billingMode == "subscription")
    }

    @Test func decimalLexemesSurviveDiscovery() {
        let data = Data(#"{"data":[{"id":"m","context_length":100,"pricing":{"prompt":0.000000000249999999999999999,"completion":0.00000000025}}]}"#.utf8)
        let items = priceJSON(data)?["data"] as? [[String: Any]]
        #expect(items?.first?["context_length"] as? Int == 100)
        let p = items?.first?["pricing"] as? [String: Any]
        #expect(p?["prompt"] as? String == "0.000000000249999999999999999")
    }

    @Test func cacheCountersAndOversizedUsage() {
        var m = UsageMetrics()
        m.feed(Data(#"{"usage":{"cache_read_input_tokens":10,"cache_creation_input_tokens":20}}"#.utf8))
        m.feed(Data(#"{"usage":{"cache_read_input_tokens":5,"cache_creation_input_tokens":10}}"#.utf8))
        #expect(m.cacheRead == 10)
        #expect(m.cacheWrite == 20)
        var large = UsageMetrics()
        large.feed(Data(("{\"usage\":{\"padding\":\"" + String(repeating: "x", count: 65536) + "\"}}").utf8))
        #expect(large.cacheUsed)
    }

    @Test func cacheTokensDisableBaseRateEstimation() {
        var m = UsageMetrics()
        m.feed(Data(#"{"usage":{"input_tokens":1,"cache_read_input_tokens":10,"output_tokens":2}}"#.utf8))
        #expect(m.cacheUsed)
        #expect(m.input == 1)
    }

    @Test func discoveryKeepsPricesWithKnownWindows() async throws {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [PricingUpstream.self]
        let session = URLSession(configuration: config)  // not invalidated: route() below starts a background refresh that outlives the test
        let p = ProviderConfig(id: "litellm", name: "LiteLLM", enabled: true, baseURL: "https://prices.example", aliases: "alias = base, 10k")
        let result = try await Discovery.load(p, key: "fixture", session: session).get()
        #expect(result.count == 1)
        #expect(result.first?.window == 10000)
        #expect(result.first?.price?.rates == PriceRates(input: "0.000002", output: "0.000006"))
        let catalog = Catalog(providers: [p], secrets: FakeSecrets(values: [p.id: "fixture"]), session: session)
        // Cold inference can use a manual alias without waiting for discovery or a price.
        let route = await catalog.route("claude-alias")
        #expect(route?.upstream == "base")
        #expect(route?.price == nil)
    }

    @Test func malformedPriceListMarksRefreshFailure() async throws {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [PricingUpstream.self]
        let session = URLSession(configuration: config)
        defer { session.invalidateAndCancel() }
        let p = ProviderConfig(id: "litellm", name: "LiteLLM", enabled: true, baseURL: "https://malformed.example", aliases: "")
        let models = try await Discovery.load(p, key: "fixture", session: session).get()
        #expect(models.count == 1)
        #expect(models.first?.price == nil)
        #expect(models.first?.priceRefreshFailed == true)
    }
}

private final class PricingUpstream: URLProtocol, @unchecked Sendable {
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func stopLoading() {}
    override func startLoading() {
        let body = request.url!.path == "/model/info" && request.url!.host == "malformed.example"
            ? #"{"error":"temporarily unavailable"}"#
            : request.url!.path == "/model/info"
            ? #"{"data":[{"model_name":"base","model_info":{"input_cost_per_token":0.000002,"output_cost_per_token":0.000006}}]}"#
            : #"{"data":[{"id":"base","context_length":10000}]}"#
        let response = HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1", headerFields: nil)!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data(body.utf8))
        client?.urlProtocolDidFinishLoading(self)
    }
}
