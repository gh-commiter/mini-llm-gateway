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

    // allowAnyDevice velja le pri praznem seznamu; API ga pri dovoljenih napravah zavrne (starejše nastavitve ga imajo).
    #expect(await admin(app.router(), "PATCH", "/general", body: #"{"allowAnyDevice":true}"#).status == 400)
    app.config.allowAnyDevice = true
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
    p.baseURL = "http://mini:4000"
    #expect(p.urlError?.contains(".local name") == true)  // ime brez pike ne (iskalna domena DNS)
    p.baseURL = "http://example.com"
    #expect(p.urlError != nil)  // javni naslov: nikoli http
    if case .failure(let f) = await Discovery.load(p, key: "k") { #expect(f.message.contains("https")) } else { Issue.record("loaded") }
    let fresh = try JSONDecoder().decode(ProviderConfig.self, from: JSONEncoder().encode(ProviderConfig(id: "y", name: "Y", enabled: false, baseURL: "http://localhost:1", aliases: "", insecure: false)))
    #expect(!fresh.insecure)  // shranjena izbira ostane
}

/// H1: z drugega računalnika brez ključa naprave le pri praznem seznamu z allowAnyDevice (kot minigw serve).
@Test func keylessRemoteClientsNeedEmptyListAndAnyDevice() async throws {
    func ask(_ c: RouterConfig) async -> FakeWriter {
        var q = request("GET", "/v1/models", headers: ["Authorization": "Bearer tok"])
        q.peer = "192.0.2.10"  // brez q.device: minigw pred 0.4.4 ali nešifriran odjemalec
        let w = FakeWriter()
        await Router(config: c, token: "tok", secrets: FakeSecrets(values: [:])).handle(q, writer: w)
        return w
    }
    var c = RouterConfig()
    c.devices = [DeviceConfig(id: newID(), name: "Air")]
    c.allowAnyDevice = true
    let w = await ask(c)
    #expect(w.status == 403 && w.text.contains("needs a device key"))  // seznam ni prazen
    c.devices = []
    #expect(await ask(c).status == 200)
    c.allowAnyDevice = false
    #expect(await ask(c).status == 403)
}

/// M1: ime naprave brez kontrolnih in oblikovnih znakov, največ 64; v terminalu v narekovajih (kot %q).
@Test func deviceNamesAreCleanedAndQuoted() {
    #expect(Noise.cleanName("  PC\n192.168.1.20 · x\u{202E}\u{200B}\t ") == "PC192.168.1.20 · x")
    #expect(Noise.cleanName(String(repeating: "a", count: 100)).count == 64)
    #expect(Noise.cleanName("PC\u{2028}10.0.0.1\u{2029}x") == "PC10.0.0.1x")  // L2: ločilo vrstice in odstavka
    #expect(Noise.quoted("a\"b\\c\nd\u{202E}") == #""a\"b\\c\u{a}d\u{202e}""#)
    let p = DeviceLog.Pending(id: "x", name: "PC\n10.0.0.1", address: "", client: "", time: Date())
    #expect(Router.refusal(Noise.Device(id: "x", name: p.name)).contains(#""PC\u{a}10.0.0.1""#))
}

/// L1: poln seznam obdrži najstarejše; ista naprava se osveži na mestu (tudi podatek odjemalca ostane).
@Test func deviceLogKeepsOldestAndThrottles() async throws {
    let log = DeviceLog()
    let calls = Counter()
    log.onChange = { calls.add() }
    for i in 0..<25 { log.addPending(id: "id\(i)", name: "n\(i)", address: "a", client: i == 0 ? "macOS, minigw 0.5.18" : "") }
    #expect(log.pending.count == DeviceLog.limit && log.pending.first?.id == "id0" && !log.pending.contains { $0.id == "id24" })
    log.addPending(id: "id0", name: "n0", address: "b")
    #expect(log.pending.first?.address == "b" && log.pending.first?.client == "macOS, minigw 0.5.18")
    #expect(calls.value <= 2)  // največ enkrat na sekundo (+ zakasnjen klic)
    for _ in 0..<30 where calls.value < 2 { try await Task.sleep(for: .milliseconds(100)) }  // zakasnjen klic (tudi pod obremenitvijo)
    #expect(calls.value == 2, "\(calls.value)")
}

final class Counter: @unchecked Sendable {
    private let lock = NSLock()
    private var n = 0
    func add() { lock.withLock { n += 1 } }
    var value: Int { lock.withLock { n } }
}

/// Ime je le oznaka: preimenovanje po ID-ju, brisanje le po ID-ju; podatek odjemalca pri čakajoči napravi.
@Test func deviceRenameAndClientInfo() async throws {
    let id = newID(), waiting = newID()
    var c = RouterConfig()
    c.devices = [DeviceConfig(id: id, name: "Air")]
    let app = FakeApp(c)
    let renamed = await admin(app.router(), "PATCH", "/devices/\(id)", body: #"{"name":"Uroš\u202e Air"}"#)
    #expect(renamed.status == 200 && app.config.devices[0].name == "Uroš Air")
    #expect(await admin(app.router(), "PATCH", "/devices/\(id)", body: #"{"name":"\n"}"#).status == 400)
    #expect(await admin(app.router(), "DELETE", "/devices/Uroš%20Air").status == 404)
    var q = request("GET", "/v1/models", headers: ["Authorization": "Bearer tok", "x-minigw-client": "Windows, minigw 0.5.18\u{7}"])
    q.peer = "192.0.2.11"
    q.device = Noise.Device(id: waiting, name: "u@PC")
    await app.router().handle(q, writer: FakeWriter())
    #expect(await admin(app.router(), "GET", "/devices").text.contains(#""client":"Windows, minigw 0.5.18""#))
    let allowed = await admin(app.router(), "POST", "/devices", body: #"{"id":"\#(waiting)","name":"Office PC"}"#)
    #expect(allowed.status == 200 && app.config.devices.contains { $0.id == waiting && $0.name == "Office PC" })
}

/// OS in različica tudi pri dovoljeni napravi; vgrajenih ponudnikov ni mogoče odstraniti (kot minigw serve).
@Test func allowedClientInfoAndBuiltInProviders() async throws {
    let id = newID()
    var c = RouterConfig()
    c.devices = [DeviceConfig(id: id, name: "PC")]
    let app = FakeApp(c)
    var q = request("GET", "/v1/models", headers: ["Authorization": "Bearer tok", "x-minigw-client": "Windows, minigw 0.5.18"])
    q.peer = "192.0.2.12"
    q.device = Noise.Device(id: id, name: "PC")
    await app.router().handle(q, writer: FakeWriter())
    #expect(await admin(app.router(), "GET", "/devices").text.contains(#""client":"Windows, minigw 0.5.18""#))
    let del = await admin(app.router(), "DELETE", "/gateways/LiteLLM")
    #expect(del.status == 400 && del.text.contains("built in") && app.config.providers.contains { $0.id == "litellm" })
}
