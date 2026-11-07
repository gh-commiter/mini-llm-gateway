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

/// Allow http kot minigw serve: obstoječi http ponudniki z vklopljenim stikalom; https vedno; http le do localhost ali
/// zasebnega IP in le s stikalom (sicer ponudnik ne dobi ključa).
@Test func providerAllowHTTPRule() async throws {
    let old = try JSONDecoder().decode([ProviderConfig].self, from: Data(#"""
    [{"id":"a","name":"A","enabled":true,"baseURL":"http://localhost:4000","aliases":""},
     {"id":"b","name":"B","enabled":true,"baseURL":"https://openrouter.ai/api","aliases":""}]
    """#.utf8))
    #expect(old[0].insecure && !old[1].insecure && old[0].urlError == nil && old[1].urlError == nil)
    #expect(RouterConfig().providers.first { $0.id == "litellm" }?.insecure == true)  // privzeti LiteLLM: http://localhost:4000
    var p = ProviderConfig(id: "x", name: "X", enabled: true, baseURL: "http://192.168.1.5:4000", aliases: "", insecure: false)
    #expect(p.urlError != nil)  // http brez stikala
    p.insecure = true
    #expect(p.urlError == nil)
    p.baseURL = "http://mini.local:4000"
    #expect(p.urlError == nil)  // ime v lokalnem omrežju (isto kot minigw serve)
    p.baseURL = "http://example.com"
    #expect(p.urlError != nil)  // javni naslov: nikoli http
    if case .failure(let f) = await Discovery.load(p, key: "k") { #expect(f.message.contains("https")) } else { Issue.record("loaded") }
    let fresh = try JSONDecoder().decode(ProviderConfig.self, from: JSONEncoder().encode(ProviderConfig(id: "y", name: "Y", enabled: false, baseURL: "http://localhost:1", aliases: "", insecure: false)))
    #expect(!fresh.insecure)  // shranjena izbira ostane
}
