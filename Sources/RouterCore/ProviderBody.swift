import Foundation

/// api.x.ai rejects a role "system" inside messages and a tool input_schema without "required" (Claude Code sends both).
/// Applied only to that host, so OpenRouter and LiteLLM bodies stay as they are.
/// Shared test cases with the Go gateway: client/testdata/provider-normalize.json (client/provider_normalize.go).
enum ProviderBody {
    static func normalize(baseURL: String, _ body: inout [String: Any]) {
        guard URL(string: baseURL)?.host?.lowercased() == "api.x.ai" else { return }
        if let tools = body["tools"] as? [[String: Any]] {
            body["tools"] = tools.map { t -> [String: Any] in
                guard var s = t["input_schema"] as? [String: Any], s["type"] as? String == "object",
                      s["required"] == nil || s["required"] is NSNull else { return t }
                var t = t
                s["required"] = [Any]()
                t["input_schema"] = s
                return t
            }
        }
        guard let msgs = body["messages"] as? [[String: Any]], msgs.contains(where: { $0["role"] as? String == "system" }) else { return }
        var out: [[String: Any]] = []
        var fresh = false  // the last message in out holds a converted system note
        for var m in msgs {
            var blocks = Self.blocks(m["content"])
            let note = m["role"] as? String == "system"
            if note {
                let text = blocks.map { $0["text"] as? String ?? "" }.joined(separator: "\n")
                blocks = [["type": "text", "text": "<system-reminder>\n" + text + "\n</system-reminder>"]]
                m = ["role": "user"]
            }
            if var last = out.last, m["role"] as? String == "user", last["role"] as? String == "user", note || fresh {
                // tool_result blocks stay first: the API wants them right after the tool_use
                let all = Self.blocks(last["content"]) + blocks
                last["content"] = all.filter { $0["type"] as? String == "tool_result" } + all.filter { $0["type"] as? String != "tool_result" }
                out[out.count - 1] = last
            } else {
                if note { m["content"] = blocks }
                out.append(m)
            }
            fresh = note
        }
        body["messages"] = out
    }

    /// Message content as a block list (a string becomes one text block).
    private static func blocks(_ c: Any?) -> [[String: Any]] {
        if let s = c as? String { return [["type": "text", "text": s]] }
        return c as? [[String: Any]] ?? []
    }
}
