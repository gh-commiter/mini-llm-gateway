import CryptoKit
import Foundation
import Testing
@testable import RouterCore

private func newID() -> String { Noise.base64url(Curve25519.Signing.PrivateKey().publicKey.rawRepresentation) }

/// Kot minigw serve: neznana naprava čaka na odobritev; starejše nastavitve ostanejo brez dela za uporabnika.
@Test func deviceApprovalMigration() throws {
    let listed = try JSONDecoder().decode(RouterConfig.self, from: Data(#"{"devices":[{"id":"A","name":"Air"}]}"#.utf8))
    #expect(!listed.allowAnyDevice && listed.deviceAllowed("A") && !listed.deviceAllowed("B"))  // naštete ostanejo dovoljene
    let empty = try JSONDecoder().decode(RouterConfig.self, from: Data("{}".utf8))
    #expect(empty.allowAnyDevice && empty.deviceAllowed("B"))  // prej je bila dovoljena vsaka
    let fresh = try JSONDecoder().decode(RouterConfig.self, from: JSONEncoder().encode(RouterConfig()))
    #expect(!RouterConfig().allowAnyDevice && !fresh.allowAnyDevice && !fresh.deviceAllowed("B"))  // nova namestitev: odobritev
}

@Test func deviceApprovalPendingAllowDeny() async throws {
    let known = newID(), newcomer = newID(), other = newID()
    var c = RouterConfig()
    c.devices = [DeviceConfig(id: known, name: "Air")]
    let app = FakeApp(c)
    func ask(_ id: String, _ name: String, peer: String = "192.0.2.9") async -> FakeWriter {
        var q = request("GET", "/v1/models", headers: ["Authorization": "Bearer tok"])
        q.peer = peer
        q.device = Noise.Device(id: id, name: name)
        let w = FakeWriter()
        await app.router().handle(q, writer: w)
        return w
    }
    #expect(await ask(known, "Air").status == 200)
    let refused = await ask(newcomer, "PC")
    #expect(refused.status == 403 && refused.text.contains("Settings → Devices") && refused.text.contains(newcomer))
    #expect(Router.deviceLog.pending.contains { $0.id == newcomer && $0.name == "PC" && $0.address == "192.0.2.9" })
    #expect(await ask(newcomer, "PC", peer: "127.0.0.1").status == 200)  // ta Mac: vedno

    let list = await admin(app.router(), "GET", "/devices").text
    #expect(list.contains(newcomer) && list.contains(#""allowAnyDevice":false"#) && list.contains(#""lastSeen""#))
    #expect(await admin(app.router(), "GET", "/status").text.range(of: #""pending":[1-9]"#, options: .regularExpression) != nil)

    // Allow brez imena: ime iz čakajoče naprave; nato dovoljena.
    let allowed = await admin(app.router(), "POST", "/devices", body: #"{"id":"\#(newcomer)"}"#)
    #expect(allowed.status == 200 && allowed.text.contains(#""added":true"#))
    #expect(app.config.devices.contains { $0.id == newcomer && $0.name == "PC" } && !Router.deviceLog.pending.contains { $0.id == newcomer })
    #expect(await ask(newcomer, "PC").status == 200)

    // Reject: le s seznama čakajočih, nastavitve ostanejo.
    #expect(await ask(other, "Spy").status == 403)
    let rejected = await admin(app.router(), "DELETE", "/devices/\(other)")
    #expect(rejected.status == 200 && rejected.text.contains(#""pending":true"#))
    #expect(!Router.deviceLog.pending.contains { $0.id == other } && !app.config.devices.contains { $0.id == other })

    // allowAnyDevice velja le pri praznem seznamu.
    #expect(await admin(app.router(), "PATCH", "/general", body: #"{"allowAnyDevice":true}"#).status == 200 && app.config.allowAnyDevice)
    #expect(await ask(other, "Spy").status == 403)
    Router.deviceLog.removePending(other)
    app.config.devices = []
    #expect(await ask(other, "Spy").status == 200)
}
