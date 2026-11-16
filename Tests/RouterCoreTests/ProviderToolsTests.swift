import Foundation
import Testing
@testable import RouterCore

/// The owner's cap on tools per provider and model. Same cases as Go (client/testdata/provider-tools.json, TestProviderToolsCap,
/// TestGatewayToolsCap): the gateway lists the cap and refuses a request whose x-claude-mini-tools header is above it.
@Suite struct ProviderToolsTests {
    @Test func sharedTable() throws {
        let url = ClaudeCodeCases.root.appendingPathComponent("client/testdata/provider-tools.json")
        let f = try #require(JSONSerialization.jsonObject(with: Data(contentsOf: url)) as? [String: Any])
        for c in try #require(f["caps"] as? [[String: Any]]) {
            var p = ProviderConfig(id: "x", name: "xAI", enabled: true, baseURL: "https://api.x.ai", aliases: "")
            p.tools = c["tools"] as! String
            p.modelTools = c["modelTools"] as! [String: String]
            #expect(p.toolsCap(c["route"] as! String) == c["cap"] as! String, "\(c)")
        }
        for c in try #require(f["levels"] as? [[String: Any]]) {
            let got = Profiles.toolsLevelRefusal(level: c["level"] as! String, cap: c["cap"] as! String, provider: "xAI", model: "grok-4")
            #expect((got ?? "") == c["want"] as! String, "\(c)")
        }
        for c in try #require(f["lines"] as? [[String: Any]]) {
            let got = Profiles.providerToolsLines(name: c["name"] as! String, tools: c["tools"] as! String,
                                                  models: c["models"] as! [String: String], model: c["model"] as! String)
            #expect(got == c["want"] as! [String], "\(c)")
        }
        for c in try #require(f["lists"] as? [[String: Any]]) {
            let got = Profiles.editRefusal(list: c["list"] as? [String: Any], model: c["model"] as! String)
            #expect((got ?? "") == c["refusal"] as! String, "\(c)")
        }
    }

    private func router(_ edit: (inout ProviderConfig) -> Void) -> Router {
        let cfg = URLSessionConfiguration.ephemeral
        cfg.protocolClasses = [StubUpstream.self]
        var c = RouterConfig()
        var p = ProviderConfig(id: "openrouter", name: "OpenRouter", enabled: true, baseURL: "https://up.example/api/", aliases: "or-qwen = qwen/qwen3-coder")
        edit(&p)
        c.providers = [p]
        return Router(config: c, token: "tok", secrets: FakeSecrets(values: ["openrouter": "or-key"]), session: URLSession(configuration: cfg))
    }

    private func status(_ r: Router, level: String?) async -> (Int, String) {
        var h = ["Authorization": "Bearer tok", "anthropic-version": "2023-06-01"]
        if let level { h["x-claude-mini-tools"] = level }
        let w = FakeWriter()
        await r.handle(request("POST", "/v1/messages", headers: h, body: #"{"model":"or-qwen","max_tokens":5,"messages":[]}"#), writer: w)
        return (w.status, w.text)
    }

    @Test func gatewayRefusesALevelAboveTheCap() async {
        let r = router { _ in }  // default cap: read
        let w = FakeWriter()
        await discoveryReady(r)
        await r.handle(request("GET", "/v1/models", headers: ["x-api-key": "tok"]), writer: w)
        #expect(w.text.contains(#""minigw_tools":"read""#) && w.text.contains(#""minigw_provider":"OpenRouter""#))
        for level in [nil, "none", "read"] { #expect(await status(r, level: level).0 == 200, "\(level ?? "-")") }
        let refused = await status(r, level: "edit")
        #expect(refused.0 == 400 && refused.1.contains("--minigw-tools=edit is not allowed for or-qwen: the gateway allows read at most")
                && refused.1.contains("minigw gw provider tools OpenRouter edit"))
        #expect(await status(r, level: "bogus").0 == 400)
        #expect(await status(router { $0.tools = "edit" }, level: "edit").0 == 200)  // the owner raised the provider
        let lowered = router { $0.tools = "edit"; $0.modelTools = ["or-qwen": "none"] }  // one model lowered: its own value wins
        #expect(await status(lowered, level: "none").0 == 200)
        let (read, edit) = (await status(lowered, level: "read").0, await status(lowered, level: "edit").0)
        #expect(read == 400 && edit == 400)
    }

    @Test func adminSetsTheCap() async {
        let app = FakeApp()
        app.config.providers = [ProviderConfig(id: "lab", name: "Lab", enabled: true, baseURL: "http://127.0.0.1:4000", aliases: "")]
        func patch(_ body: String) async -> (status: Int, text: String) {
            let w = await admin(app.router(), "PATCH", "/gateways/Lab", body: body, host: "127.0.0.1:9999")
            return (w.status, w.text)
        }
        let set = await patch(#"{"tools":"edit"}"#)
        #expect(set.status == 200 && set.text.contains(#""tools":"edit""#) && app.config.providers[0].tools == "edit")
        let one = await patch(#"{"tools":"none","model":"claude-gpt-y"}"#)
        #expect(one.status == 200 && one.text.contains(#""modelTools":{"gpt-y":"none"}"#) && app.config.providers[0].toolsCap("gpt-y") == "none")
        #expect(await patch(#"{"tools":"","model":"gpt-y"}"#).status == 200 && app.config.providers[0].modelTools.isEmpty)
        for bad in [#"{"tools":"root"}"#, #"{"model":"gpt-y"}"#, #"{"tools":"edit","model":""}"#, #"{"tools":"edit","model":"a\nb"}"#] {
            #expect(await patch(bad).status == 400, "\(bad)")
        }
        #expect(await patch(#"{"tools":""}"#).status == 200 && app.config.providers[0].toolsCap("x") == "read")
    }
}
