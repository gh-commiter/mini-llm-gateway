import Foundation
import Testing
@testable import RouterCore

/// Same cases as Go (client/testdata/provider-normalize.json, client/provider_normalize_test.go).
struct ProviderBodyTests {
    @Test func sharedCases() throws {
        let url = ClaudeCodeCases.root.appendingPathComponent("client/testdata/provider-normalize.json")
        let f = try #require(JSONSerialization.jsonObject(with: Data(contentsOf: url)) as? [String: Any])
        let cases = try #require(f["cases"] as? [[String: Any]])
        #expect(!cases.isEmpty)
        for c in cases {
            let name = c["name"] as? String ?? ""
            let input = c["input"] as! [String: Any]
            var got = input
            ProviderBody.normalize(baseURL: "https://api.x.ai", &got)
            #expect(NSDictionary(dictionary: got).isEqual(to: c["expected"] as! [String: Any]), "\(name)")
            var again = got
            ProviderBody.normalize(baseURL: "https://api.x.ai", &again)
            #expect(NSDictionary(dictionary: again).isEqual(to: got), "\(name): not idempotent")
            var other = input
            ProviderBody.normalize(baseURL: "https://openrouter.ai/api", &other)
            #expect(NSDictionary(dictionary: other).isEqual(to: input), "\(name): changed for another provider")
        }
        for h in try #require(f["hosts"] as? [[String: Any]]) {
            let tools: [[String: Any]] = [["input_schema": ["type": "object"]]]
            var b: [String: Any] = ["tools": tools]
            ProviderBody.normalize(baseURL: h["url"] as! String, &b)
            let changed = !NSDictionary(dictionary: b).isEqual(to: ["tools": tools])
            #expect(changed == (h["applies"] as! Bool), "\(h["url"] ?? "")")
        }
    }
}
