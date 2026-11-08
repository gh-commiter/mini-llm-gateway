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
        let oldSession = Foundry.session, oldCommand = FoundryHub.command, oldOurs = FoundryHub.listenerIsOurs
        Foundry.session = URLSession(configuration: c)
        FoundryHub.command = { _, args, _ in args == ["server", "status"] ? "running on http://127.0.0.1:5273/openai/status" : "" }
        FoundryHub.listenerIsOurs = { _, _ in true }  // stub: na 5273 nihče ne posluša
        defer { Foundry.session = oldSession; FoundryHub.command = oldCommand; FoundryHub.listenerIsOurs = oldOurs }
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
        // Zgodovina seje: žetoni iz odgovora (Foundry vrne usage), brez besedila.
        let sid = "foundry-" + UUID().uuidString
        let w = FakeWriter()
        await router.handle(request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok", "x-claude-mini-session": sid],
                                    body: #"{"model":"claude-foundry-phi-4-mini-generic-cpu","max_tokens":5,"messages":[{"role":"user","content":"secret words"}]}"#), writer: w)
        let rec = Router.history.sessions().first { $0.id == sid }
        let fu = rec?.models.first { $0.model == "claude-foundry-phi-4-mini-generic-cpu" }
        #expect(fu?.inputTokens == 7 && fu?.outputTokens == 3 && fu?.tools == "" && rec?.mode == "chat")
        #expect(!String(decoding: (try? JSONEncoder().encode(rec)) ?? Data(), as: UTF8.self).contains("secret words"))
        let unknown = await call("POST", "/v1/messages", #"{"model":"claude-foundry-nope","messages":[{"role":"user","content":"hi"}]}"#)
        #expect(unknown.contains("404"))
        // N5: poslušalca preveri vsaka zahteva, tudi ko je naslov v predpomnilniku
        FoundryHub.listenerIsOurs = { _, _ in false }
        let taken = await call("POST", "/v1/messages", #"{"model":"claude-foundry-phi-4-mini-generic-cpu","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}"#)
        #expect(taken.contains("port belongs to another user") && FoundryStub.requests.count == 3, "\(taken)")
        await Router.foundry.forget()
    }

    /// F1, F2, F5, F7.
    @Test func reviewFixes() throws {
        // F2: lastni poslušalec je naš; port brez poslušalca ne.
        let fd = socket(AF_INET, SOCK_STREAM, 0)
        defer { close(fd) }
        var a = sockaddr_in(sin_len: UInt8(MemoryLayout<sockaddr_in>.size), sin_family: sa_family_t(AF_INET), sin_port: 0,
                            sin_addr: in_addr(s_addr: inet_addr("127.0.0.1")), sin_zero: (0, 0, 0, 0, 0, 0, 0, 0))
        var len = socklen_t(MemoryLayout<sockaddr_in>.size)
        _ = withUnsafeMutablePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, len) } }
        _ = listen(fd, 1)
        _ = withUnsafeMutablePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(fd, $0, &len) } }
        let port = UInt16(bigEndian: a.sin_port)
        #expect(PeerProcess.listenerIsOurs(port: port))
        // L3: tudi program poslušalca (ta testni proces) mora biti v mapi namestitve.
        var buf = [CChar](repeating: 0, count: 4096)
        _ = proc_pidpath(getpid(), &buf, UInt32(buf.count))
        let me = try #require(Sync.real(URL(fileURLWithPath: String(cString: buf))))
        #expect(PeerProcess.listenerIsOurs(port: port, installDir: me.deletingLastPathComponent().path))
        #expect(!PeerProcess.listenerIsOurs(port: port, installDir: "/opt/homebrew/Cellar/foundrylocal"))
        #expect(FoundryHub.installDir("/opt/x/Cellar/foundrylocal/1.0/bin/foundry").hasSuffix("/foundrylocal/1.0"))
        // N4: navaden bin ostane bin; povezava v paket z lastno mapo gre nad njegov bin
        let t = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("n4-" + UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: t) }
        for d in ["usr/bin", "pkg/2.0/bin"] {
            try FileManager.default.createDirectory(at: t.appendingPathComponent(d), withIntermediateDirectories: true)
            FileManager.default.createFile(atPath: t.appendingPathComponent(d + "/foundry").path, contents: nil)
        }
        try FileManager.default.createSymbolicLink(at: t.appendingPathComponent("foundry"), withDestinationURL: t.appendingPathComponent("pkg/2.0/bin/foundry"))
        #expect(FoundryHub.installDir(t.appendingPathComponent("usr/bin/foundry").path) == t.appendingPathComponent("usr/bin").path)
        #expect(FoundryHub.installDir(t.appendingPathComponent("foundry").path) == t.appendingPathComponent("pkg/2.0").path)
        #expect(!PeerProcess.listenerIsOurs(port: freeTCPPort()))
        // F5, F7
        #expect(Foundry.count(42) == 42 && Foundry.count(-1) == nil && Foundry.count(2_000_000_000) == nil && Foundry.count("7") == nil)
        #expect(!Foundry.validModel("a/b") && !Foundry.validModel("x..y") && !Foundry.validModel("-rf") && Foundry.validModel("phi-4-mini-generic-cpu"))
        // F1: izpis ukaza do roka, tudi ko otrok drži cev odprto
        let start = Date()
        let out = Foundry.run("/bin/sh", ["-c", "(sleep 30 &); echo out"], timeout: 1)
        #expect(out.contains("out") && Date().timeIntervalSince(start) < 5)
    }
}
