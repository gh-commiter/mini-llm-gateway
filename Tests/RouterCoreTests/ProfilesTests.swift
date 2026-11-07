import Foundation
import Network
import Testing
@testable import RouterCore

/// #17: imena profilov, pripona modela, združeni seznam (enako kot client/profiles_test.go).
@Test func profileNamesAndSuffix() {
    for n in ["default", "win", "mac-mini", "a1"] { #expect(Profiles.valid(n), "\(n)") }
    for n in ["", "Win", "-x", "a b", "a/b", "..", String(repeating: "a", count: 33), "x@y"] { #expect(!Profiles.valid(n), "\(n)") }
    let names = ["mini", "win"]
    #expect(Profiles.split("claude-cli-opus@win", names: names) == ("claude-cli-opus", "win"))
    for m in ["claude-opus-4@20250514", "x@other", "plain", "@win"] { #expect(Profiles.split(m, names: names).profile == nil, "\(m)") }
    let (b, p) = Profiles.routeBody(Data(#"{"model":"claude-cli-opus@win","max_tokens":5}"#.utf8), names: names)
    let o = try? JSONSerialization.jsonObject(with: b) as? [String: Any]
    #expect(p == "win" && o?["model"] as? String == "claude-cli-opus" && o?["max_tokens"] as? Int == 5)
    let raw = Data(#"{"model":"x@other"}"#.utf8)
    #expect(Profiles.routeBody(raw, names: names) == (raw, nil))
}

@Test func profileModelsMerge() {
    let a: [[String: Any]] = [["id": "b", "display_name": "B"], ["id": "a"]], w: [[String: Any]] = [["id": "a", "display_name": "A"]]
    let ids = Profiles.merge([a, w], names: ["mini", "win"])?.map { "\($0["id"]!)|\($0["display_name"] as? String ?? "")" }
    #expect(ids == ["b|B", "a|", "b@mini|B @mini", "a@mini|", "a@win|A @win"])
    #expect(Profiles.merge([nil, w], names: ["mini", "win"])?.map { $0["id"] as! String } == ["a@win"])
    #expect(Profiles.merge([nil, nil], names: ["mini", "win"]) == nil)
}

/// Lažen prehod: ena zahteva na povezavo; zabeleži avtorizacijo, model in glavo mape.
private final class FakeGateway: @unchecked Sendable {
    let listener: NWListener
    private let lock = NSLock()
    private var seen: [String] = []
    var port: UInt16 { listener.port?.rawValue ?? 0 }
    var log: [String] { lock.withLock { seen } }

    init(models: String) throws {
        let p = NWParameters.tcp
        p.requiredLocalEndpoint = .hostPort(host: "127.0.0.1", port: .any)
        listener = try NWListener(using: p)
        let ready = DispatchSemaphore(value: 0)
        listener.stateUpdateHandler = { if case .ready = $0 { ready.signal() } }
        listener.newConnectionHandler = { [unowned self] c in
            c.start(queue: .global())
            self.read(c, Data(), models)
        }
        listener.start(queue: .global())
        ready.wait()
    }

    private func read(_ c: NWConnection, _ buf: Data, _ models: String) {
        c.receive(minimumIncompleteLength: 1, maximumLength: 1 << 20) { d, _, end, _ in
            let b = buf + (d ?? Data())
            guard case .request(let r) = HTTPParser.parse(b) else { return end ? c.cancel() : self.read(c, b, models) }
            var body = Data(#"{"ok":true}"#.utf8)
            if r.path == "/v1/models" { body = Data(#"{"data":[\#(models)]}"#.utf8) } else {
                let m = (try? JSONSerialization.jsonObject(with: r.body) as? [String: Any])?["model"] as? String ?? ""
                self.lock.withLock { self.seen.append("\(r.header("authorization") ?? "")|\(m)|\(r.header("x-claude-mini-share") ?? "")|\(r.header("connection") ?? "")") }
            }
            c.send(content: HTTPResponse.head(status: 200, headers: [("Content-Type", "application/json")], contentLength: body.count) + body,
                   completion: .contentProcessed { _ in c.cancel() })
        }
    }
}

private func send(_ url: String, _ method: String, _ path: String, secret: String, body: String? = nil) async throws -> (Int, String) {
    var r = URLRequest(url: URL(string: url + path)!)
    r.httpMethod = method
    r.setValue("Bearer \(secret)", forHTTPHeaderField: "authorization")
    r.setValue("1", forHTTPHeaderField: "x-claude-mini-share")
    r.httpBody = body.map { Data($0.utf8) }
    let (d, resp) = try await URLSession.shared.data(for: r)
    return ((resp as! HTTPURLResponse).statusCode, String(decoding: d, as: UTF8.self))
}

@Test func profileMuxRoutesBySuffix() async throws {
    let g1 = try FakeGateway(models: #"{"id":"claude-siri-ai"}"#), g2 = try FakeGateway(models: #"{"id":"claude-cli-opus"}"#)
    defer { g1.listener.cancel(); g2.listener.cancel() }
    final class Count: @unchecked Sendable { var n = 0 }
    let shares = Count()
    let m = ProfileMux(targets: [.init(name: "mini", url: "http://127.0.0.1:\(g1.port)", token: "tok-mini"),
                                 .init(name: "win", url: "http://127.0.0.1:\(g2.port)", token: "tok-win"),
                                 .init(name: "off", url: "http://127.0.0.1:1", token: "x")],
                       share: ["win": { shares.n += 1; return true }])
    try m.start()
    defer { m.stop() }
    #expect(try await send(m.localURL, "GET", "/v1/models", secret: "wrong").0 == 401)
    let (st, list) = try await send(m.localURL, "GET", "/v1/models", secret: m.secret)
    #expect(st == 200 && list.contains(#""claude-siri-ai""#) && list.contains(#""claude-siri-ai@mini""#)
            && list.contains(#""claude-cli-opus@win""#) && !list.contains("@off"))
    for _ in 0..<2 { #expect(try await send(m.localURL, "POST", "/v1/messages", secret: m.secret, body: #"{"model":"claude-cli-opus@win"}"#) == (200, #"{"ok":true}"#)) }
    #expect(try await send(m.localURL, "POST", "/v1/messages/count_tokens", secret: m.secret, body: #"{"model":"claude-siri-ai"}"#).0 == 200)
    #expect(g2.log == ["Bearer tok-win|claude-cli-opus|1|close", "Bearer tok-win|claude-cli-opus|1|close"])
    #expect(g1.log == ["Bearer tok-mini|claude-siri-ai|1|close"])
    #expect(shares.n == 1)
    #expect(try await send(m.localURL, "POST", "/v1/messages", secret: m.secret, body: #"{"model":"x@off"}"#).0 == 502)
}
