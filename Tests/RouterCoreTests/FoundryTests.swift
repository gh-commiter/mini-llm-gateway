import Foundation
import Testing
@testable import RouterCore

/// Lažen Foundry Local (status, modeli, katalog, nalaganje, chat s pretokom in brez).
final class FoundryStub: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var requests: [[String: Any]] = []
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func stopLoading() {}
    override func startLoading() {
        var body = "{}", type = "application/json"
        switch request.url!.path {
        case "/openai/status": body = #"{"Endpoints":["http://localhost:5273"],"ModelDirPath":"/m"}"#
        case "/openai/models": body = #"["phi-4-mini-generic-cpu","qwen-vl-generic-gpu"]"#
        case "/foundry/list": body = #"[{"name":"qwen-vl-generic-gpu","inputModalities":"text,image"}]"#
        case "/v1/chat/completions":
            var d = Data()
            if let s = request.httpBodyStream {
                s.open()
                var buf = [UInt8](repeating: 0, count: 4096)
                while s.hasBytesAvailable { let n = s.read(&buf, maxLength: buf.count); if n <= 0 { break }; d.append(buf, count: n) }
                s.close()
            }
            let o = (try? JSONSerialization.jsonObject(with: d) as? [String: Any]) ?? [:]
            Self.requests.append(o)
            if o["stream"] as? Bool == true {
                type = "text/event-stream"
                body = ["{\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}", "{\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}",
                        "{\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":2}}", "[DONE]"].map { "data: \($0)\n\n" }.joined()
            } else {
                body = #"{"choices":[{"message":{"content":"Hi there"},"finish_reason":"length"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}"#
            }
        default: break
        }
        let resp = HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": type])!
        client?.urlProtocol(self, didReceive: resp, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data(body.utf8))
        client?.urlProtocolDidFinishLoading(self)
    }
}

@Suite(.serialized) struct FoundryLocal {
    @Test func translation() throws {
        #expect(Foundry.port("Model management service is running on http://127.0.0.1:5273/openai/status") == 5273)
        #expect(Foundry.port("running on http://evil.example:5273/") == nil)
        #expect(Foundry.names(Data(#"[{"name":"a"},"b"]"#.utf8)) == ["a", "b"] && Foundry.names(Data(#"{"data":[{"id":"c"}]}"#.utf8)) == ["c"])
        let img: [String: Any] = ["type": "image", "source": ["type": "base64", "media_type": "image/png", "data": "iVBORw0K"]]
        var body: [String: Any] = ["max_tokens": 100, "tools": [["name": "Read"]], "system": "long system prompt",
                                   "messages": [["role": "assistant", "content": "orphan"], ["role": "user", "content": [["type": "text", "text": "look"], img]]]]
        let r = try Foundry.request(body, model: "m", instructions: "be brief", vision: true, stream: true)
        let j = String(decoding: try JSONSerialization.data(withJSONObject: r, options: [.sortedKeys, .withoutEscapingSlashes]), as: UTF8.self)
        #expect(!j.contains("tools") && !j.contains("orphan") && !j.contains("long system") && j.contains(#""max_completion_tokens":100"#)
                && j.contains(#""content":"be brief""#) && j.contains("data:image/png;base64,iVBORw0K") && j.contains(#""include_usage":true"#))
        let textOnly = try Foundry.request(body, model: "m", instructions: "x", vision: false, stream: false)
        #expect(String(describing: textOnly["messages"]!).contains("image(s) left out"))
        body["tool_choice"] = ["type": "any"]
        #expect(throws: Foundry.RequestError.self) { try Foundry.request(body, model: "m", instructions: "x", vision: false, stream: false) }
        #expect(Foundry.stop("length") == "max_tokens" && Foundry.stop("stop") == "end_turn")
    }

    @Test func throughTheGateway() async throws {
        let c = URLSessionConfiguration.ephemeral
        c.protocolClasses = [FoundryStub.self]
        let oldSession = Foundry.session, oldCommand = FoundryHub.command
        Foundry.session = URLSession(configuration: c)
        FoundryHub.command = { _, args, _ in args == ["server", "status"] ? "running on http://127.0.0.1:5273/openai/status" : "" }
        defer { Foundry.session = oldSession; FoundryHub.command = oldCommand }
        await Router.foundry.forget()
        FoundryStub.requests = []
        var cfg = RouterConfig()
        cfg.foundry.enabled = true
        cfg.foundry.executable = "/bin/sh"
        cfg.foundry.instructions = "be brief"
        let router = Router(config: cfg, token: "tok", secrets: FakeSecrets(values: [:]))
        func call(_ method: String, _ path: String, _ body: String = "") async -> String {
            let w = FakeWriter()
            await router.handle(request(method, path, headers: ["Authorization": "Bearer tok"], body: body), writer: w)
            return w.text
        }
        let models = await call("GET", "/v1/models")
        #expect(models.contains("claude-foundry-phi-4-mini-generic-cpu") && models.contains("claude-foundry-qwen-vl-generic-gpu"))
        let a = await call("POST", "/v1/messages", #"{"model":"claude-foundry-phi-4-mini-generic-cpu","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}"#)
        #expect(a.contains(#""text":"Hi there""#) || a.contains(#""text" : "Hi there""#))
        #expect(a.contains("max_tokens") && a.contains("200"))
        let s = await call("POST", "/v1/messages", #"{"model":"claude-foundry-phi-4-mini-generic-cpu","stream":true,"messages":[{"role":"user","content":"hi"}]}"#)
        #expect(s.contains("Hel") && s.contains("lo") && s.contains("message_stop") && s.contains("end_turn"))
        #expect(FoundryStub.requests.count == 2 && FoundryStub.requests.first?["model"] as? String == "phi-4-mini-generic-cpu" && FoundryStub.requests.first?["tools"] == nil)
        let unknown = await call("POST", "/v1/messages", #"{"model":"claude-foundry-nope","messages":[{"role":"user","content":"hi"}]}"#)
        #expect(unknown.contains("404"))
    }
}
