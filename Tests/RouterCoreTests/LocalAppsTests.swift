import Foundation
import Testing
@testable import RouterCore

/// Modeli brez žetona za aplikacije tega uporabnika: glava, Host, Origin, uporabnik (vrata 4242 = ta uporabnik), stikalo.
@Test func localAppsWithoutToken() async {
    Router.localPeer = { peerPort, _ in peerPort == 4242 }  // le zahteve tega testa
    func status(_ method: String = "GET", _ path: String = "/v1/models", headers: [String: String] = ["anthropic-version": "2023-06-01"],
                host: String = "127.0.0.1:8765", port: UInt16 = 4242, peer: String = "127.0.0.1", localApps: Bool = true) async -> Int {
        var c = RouterConfig()
        c.allowLocalApps = localApps
        let r = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
        var q = request(method, path, headers: headers)
        q.headers["host"] = host
        q.peerPort = port
        q.peer = peer
        let w = FakeWriter()
        await r.handle(q, writer: w)
        return w.status
    }
    #expect(await status() == 200)                                                   // isti uporabnik
    #expect(await status(headers: ["x-api-key": "anything"]) == 200)
    #expect(await status(host: "localhost:8765") == 200)
    #expect(await status(host: "[::1]:8765") == 200)
    #expect(await status(headers: [:]) == 401)                                       // brez glave Anthropic (brskalnik)
    #expect(await status(headers: ["anthropic-version": "1", "origin": "http://127.0.0.1"]) == 401)
    #expect(await status(host: "evil.example:8765") == 401)                          // DNS rebinding
    #expect(await status(port: 5000) == 401)                                         // drug uporabnik ali agent
    #expect(await status(peer: "10.0.0.9") == 401)                                   // drug računalnik
    #expect(await status("POST", "/minigw/share") == 401)                            // le modeli
    #expect(await status(localApps: false) == 401)                                   // stikalo izklopljeno
    #expect(RouterConfig().allowLocalApps)
    #expect(((try? JSONDecoder().decode(RouterConfig.self, from: Data("{}".utf8)))?.allowLocalApps) == true)
}
