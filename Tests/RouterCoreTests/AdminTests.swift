import CryptoKit
import Foundation
import Network
import Testing
@testable import RouterCore

/// Aplikacija za teste: nastavitve, ki jih opazuje Settings, in Keychain.
final class FakeApp: @unchecked Sendable {
    let lock = NSLock()
    var config: RouterConfig
    var keys: [String: String] = [:]
    init(_ c: RouterConfig = RouterConfig()) { config = c }

    /// Router, kot ga aplikacija zgradi ob vsakem zagonu (apply znova zažene router z novimi nastavitvami).
    func router(session: URLSession = .shared) -> Router {
        let hooks = AdminHooks(secret: adminSecret, connect: "tok#KEY", version: "9.9", installed: { false },
                               update: { f in
                                   try self.lock.withLock {
                                       var c = self.config
                                       try f(&c)
                                       self.config = c
                                       return c
                                   }
                               },
                               setKey: { a, k in self.lock.withLock { self.keys[a] = k } },
                               rotateToken: { "newtok#KEY" })
        return lock.withLock { Router(config: config, token: "tok", secrets: FakeSecrets(values: keys), session: session, admin: hooks) }
    }
}

let adminSecret = String(repeating: "S", count: 43)

func admin(_ r: Router, _ method: String, _ path: String, body: String = "", host: String = "127.0.0.1:8765",
           auth: String? = adminSecret, peer: String = "127.0.0.1", encrypted: Bool = false) async -> FakeWriter {
    var h = ["Host": host]
    if let auth { h["Authorization"] = "Bearer " + auth }
    var req = request(method, Router.adminPrefix + path, headers: h, body: body)
    req.peer = peer
    req.encrypted = encrypted
    let w = FakeWriter()
    await r.handle(req, writer: w)
    return w
}

@Suite struct AdminAPI {
    @Test func authLoopbackHostAndSecret() async {
        let r = FakeApp().router()
        #expect(await admin(r, "GET", "/status").status == 200)
        #expect(await admin(r, "GET", "/status", host: "localhost:8765").status == 200)
        #expect(await admin(r, "GET", "/status", peer: "192.168.1.9").status == 403)  // naslov povezave, ne glava
        #expect(await admin(r, "GET", "/status", peer: "192.168.1.9", encrypted: true).status == 403)
        #expect(await admin(r, "GET", "/status", host: "evil.example:8765").status == 403)  // DNS rebinding
        #expect(await admin(r, "GET", "/status", host: "127.0.0.1:1").status == 403)
        #expect(await admin(r, "GET", "/status", host: "x", encrypted: true).status == 200)  // Noise: ni brskalnik
        #expect(await admin(r, "GET", "/status", auth: nil).status == 401)
        #expect(await admin(r, "GET", "/status", auth: adminSecret + "x").status == 401)
        #expect(await admin(r, "GET", "/status", auth: "tok").status == 401)  // žeton routerja ni skrivnost API
        let w = await admin(r, "GET", "/status")
        #expect(w.text.contains(#""running":true"#) && !w.text.lowercased().contains("access-control"))
        #expect(w.text.contains("Cache-Control: no-store") && !w.text.lowercased().contains("set-cookie"))
        #expect(await admin(r, "GET", "/status", auth: nil).text.contains("Cache-Control: no-store"))  // tudi napake
        // Brez kavljev (npr. testni router) skrbniškega API ni.
        let bare = Router(config: RouterConfig(), token: "tok", secrets: FakeSecrets(values: [:]))
        #expect(await admin(bare, "GET", "/status").status == 404)
    }

    @Test func strictJSON() async {
        let r = FakeApp().router()
        let unknown = await admin(r, "PATCH", "/agents/claude", body: #"{"tools":"Read","bogus":1}"#)
        #expect(unknown.status == 400 && unknown.text.contains("unknown field"))
        #expect(await admin(r, "PATCH", "/agents/claude", body: #"{"tools":"Read"}x"#).status == 400)
        #expect(await admin(r, "PATCH", "/agents/claude", body: #"{"tools":"Read"}{"tools":"Bash"}"#).status == 400)
        #expect(await admin(r, "PATCH", "/agents/claude", body: #"{"allowLocalTools":1}"#).status == 400)  // 1 ni true
        #expect(await admin(r, "PATCH", "/general", body: #"{"port":true}"#).status == 400)
        #expect(await admin(r, "PATCH", "/agents/claude", body: #"{"tools":"Read;rm"}"#).status == 400)
        #expect(await admin(r, "PATCH", "/agents/claude", body: "[1]").status == 400)
        let big = String(repeating: " ", count: 1 << 20) + "{}"
        #expect(await admin(r, "PATCH", "/general", body: big).status == 413)
    }

    @Test func liveApplyThroughAppConfig() async throws {
        let app = FakeApp()
        let id = Noise.base64url(Curve25519.Signing.PrivateKey().publicKey.rawRepresentation)
        #expect(await admin(app.router(), "POST", "/devices", body: #"{"id":"10.192.31.5"}"#).status == 400)
        let w = await admin(app.router(), "POST", "/devices", body: #"{"id":"\#(id)","name":"PC"}"#)
        #expect(w.status == 200 && w.text.contains(#""added":true"#))
        #expect(app.config.devices.map(\.id) == [id])  // Settings opazuje te nastavitve
        #expect(await admin(app.router(), "GET", "/devices").text.contains(id))  // nov router (po apply) jo vidi
        #expect(await admin(app.router(), "PATCH", "/siri", body: #"{"enabled":true,"allow":["\#(id)"]}"#).status == 200)
        #expect(app.config.siri.enabled && app.config.devices[0].siri)
        #expect(await admin(app.router(), "DELETE", "/devices/PC").status == 200 && app.config.devices.isEmpty)

        #expect(await admin(app.router(), "PATCH", "/agents/claude", body: #"{"tools":"Read, Grep","folders":"~/src"}"#).status == 200)
        #expect(app.config.claudeCLI.tools == "Read,Grep" && app.config.claudeCLI.allowedRoots == "~/src")
        #expect(await admin(app.router(), "PATCH", "/agents/codex", body: #"{"write":false,"models":["gpt-5"]}"#).status == 200)
        #expect(!app.config.codex.write && app.config.codex.enabledModels == ["gpt-5"])
        #expect(await admin(app.router(), "PATCH", "/agents/grok", body: #"{"models":null}"#).status == 200 && app.config.grok.enabledModels == nil)
        #expect(await admin(app.router(), "PATCH", "/agents/grok", body: #"{"network":true}"#).status == 400)  // le Codex ima omrežje

        #expect(await admin(app.router(), "PATCH", "/general", body: #"{"port":9999,"allowedClients":"192.168.1.7"}"#).status == 200)
        #expect(app.config.port == 9999 && app.config.allowedClients == "192.168.1.7")
        #expect(await admin(app.router(), "PATCH", "/general", body: #"{"address":"192.168.1.5"}"#, host: "127.0.0.1:9999").status == 400)
        #expect(await admin(app.router(), "PATCH", "/general", body: #"{"allowedClients":"not-an-ip"}"#, host: "127.0.0.1:9999").status == 400)
        let t = await admin(app.router(), "POST", "/general/token", host: "127.0.0.1:9999")
        #expect(t.status == 200 && t.text.contains("newtok#KEY"))
    }

    @Test func gatewaysWithDiscoveryAndKeys() async throws {
        let cfg = URLSessionConfiguration.ephemeral
        cfg.protocolClasses = [StubUpstream.self]
        let session = URLSession(configuration: cfg)
        let app = FakeApp()
        app.config.providers = []
        #expect(await admin(app.router(session: session), "POST", "/gateways",
                            body: #"{"name":"Lab","baseURL":"http://lab.example:4000","key":"k1","insecure":true}"#).status == 400)  // http le zasebno
        #expect(await admin(app.router(session: session), "POST", "/gateways",
                            body: #"{"name":"Lab","baseURL":"http://127.0.0.1:4000","key":"k1"}"#).status == 400)  // brez insecure
        let add = await admin(app.router(session: session), "POST", "/gateways",
                              body: #"{"name":"Lab","baseURL":"http://127.0.0.1:4000/v1/","key":"k1","insecure":true}"#)
        #expect(add.status == 200 && add.text.contains(#""model":"claude-qwen/qwen3-coder""#) && !add.text.contains("k1"))
        let p = try #require(app.config.providers.first)
        #expect(p.name == "Lab" && p.baseURL == "http://127.0.0.1:4000" && p.enabled && app.keys[p.id] == "k1")
        let off = await admin(app.router(session: session), "POST", "/gateways/Lab/models", body: #"{"disable":["claude-lite/model"]}"#)
        #expect(off.status == 200 && off.text.contains(#""changed":[{"#) && app.config.providers[0].enabledModels == ["qwen/qwen3-coder"])
        let filtered = await admin(app.router(session: session), "GET", "/gateways/Lab/models?filter=QWEN")
        #expect(filtered.text.contains("qwen3-coder") && !filtered.text.contains("lite/model"))
        #expect(await admin(app.router(session: session), "DELETE", "/gateways/Lab").status == 200)
        #expect(app.config.providers.isEmpty && app.keys[p.id] == nil)
    }

    /// M1: program, ki zasede vrata routerja, nikoli ne dobi skrivnosti: proxy minigw pošlje HTTP šele po rokovanju.
    @Test func squatterNeverGetsTheSecret() async throws {
        let got = Received()
        let squatter = try NWListener(using: .tcp, on: .any)
        squatter.newConnectionHandler = { c in
            c.start(queue: .global())
            c.send(content: Data(repeating: 0x41, count: 200), completion: .contentProcessed { _ in })  // »odgovor« rokovanja
            func read() {
                c.receive(minimumIncompleteLength: 1, maximumLength: 65536) { d, _, end, e in
                    if let d { got.add(d) }
                    if end || e != nil { c.cancel() } else { read() }
                }
            }
            read()
        }
        let ready = DispatchSemaphore(value: 0)
        squatter.stateUpdateHandler = { if case .ready = $0 { ready.signal() } }
        squatter.start(queue: .global())
        ready.wait()
        defer { squatter.cancel() }
        let port = try #require(squatter.port?.rawValue)
        let proxy = try #require(NoiseProxy(routerURL: "http://127.0.0.1:\(port)", routerKey: Noise.newKey().publicKey.rawRepresentation, suite: .aesGCM))
        try proxy.start()
        defer { proxy.stop() }
        var r = URLRequest(url: URL(string: proxy.localURL + Router.adminPrefix + "/gateways")!, timeoutInterval: 3)
        r.httpMethod = "POST"
        r.setValue("Bearer " + adminSecret, forHTTPHeaderField: "authorization")
        r.httpBody = Data(#"{"key":"sk-gateway-secret"}"#.utf8)
        _ = try? await URLSession.shared.data(for: r)
        try await Task.sleep(for: .milliseconds(200))
        let bytes = String(decoding: got.data, as: UTF8.self)
        #expect(!got.data.isEmpty)  // proxy je res poskusil (rokovanje)
        #expect(!bytes.contains(adminSecret) && !bytes.contains("Bearer") && !bytes.contains("sk-gateway-secret"))
    }
}

final class Received: @unchecked Sendable {
    private let lock = NSLock()
    private var d = Data()
    func add(_ x: Data) { lock.withLock { d.append(x) } }
    var data: Data { lock.withLock { d } }
}

/// Vtičnica, ki posluša na 127.0.0.1 (vrata routerja v testu); accept vrne vrata odjemalca.
private func listener() -> (fd: Int32, port: UInt16) {
    let s = socket(AF_INET, SOCK_STREAM, 0)
    var a = sockaddr_in()
    a.sin_family = sa_family_t(AF_INET); a.sin_addr.s_addr = inet_addr("127.0.0.1")
    _ = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(s, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
    listen(s, 4)
    var l = socklen_t(MemoryLayout<sockaddr_in>.size)
    _ = withUnsafeMutablePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(s, $0, &l) } }
    return (s, UInt16(bigEndian: a.sin_port))
}

private func acceptPeerPort(_ fd: Int32) -> (Int32, UInt16) {
    var a = sockaddr_in(), l = socklen_t(MemoryLayout<sockaddr_in>.size)
    let c = withUnsafeMutablePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { accept(fd, $0, &l) } }
    return (c, UInt16(bigEndian: a.sin_port))
}

@Suite struct AdminPeerProcess {
    /// Odjemalec je ta proces: »aplikacija« sama (oz. njen otrok) je zavrnjena, tuja aplikacija ne; neznan proces zavrnjen.
    @Test func identifiesTheClientProcess() throws {
        let (l, routerPort) = listener()
        defer { close(l) }
        let c = socket(AF_INET, SOCK_STREAM, 0)
        defer { close(c) }
        var a = sockaddr_in()
        a.sin_family = sa_family_t(AF_INET); a.sin_addr.s_addr = inet_addr("127.0.0.1"); a.sin_port = routerPort.bigEndian
        _ = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(c, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
        let (s, peerPort) = acceptPeerPort(l)
        defer { close(s) }
        #expect(PeerProcess.pid(localPort: peerPort, remotePort: routerPort) == getpid())
        #expect(PeerProcess.refusal(peerPort: peerPort, routerPort: routerPort, appPID: getpid()) == "admin requests from agent processes are not allowed")
        #expect(PeerProcess.refusal(peerPort: peerPort, routerPort: routerPort, appPID: 999_999) == nil)
        #expect(PeerProcess.refusal(peerPort: 1, routerPort: routerPort, appPID: 999_999)?.contains("cannot identify") == true)
    }

    /// Otrok »aplikacije« (kot agent ali minigw, ki ga zažene agent) je zavrnjen.
    @Test func refusesAChildProcess() throws {
        let (l, routerPort) = listener()
        defer { close(l) }
        let nc = Process()
        nc.executableURL = URL(fileURLWithPath: "/usr/bin/nc")
        nc.arguments = ["127.0.0.1", String(routerPort)]
        nc.standardInput = Pipe()  // ostane odprt, dokler ga test ne konča
        try nc.run()
        defer { nc.terminate() }
        let (s, peerPort) = acceptPeerPort(l)
        defer { close(s) }
        #expect(PeerProcess.pid(localPort: peerPort, remotePort: routerPort) == nc.processIdentifier)
        #expect(PeerProcess.refusal(peerPort: peerPort, routerPort: routerPort, appPID: getpid()) == "admin requests from agent processes are not allowed")
    }

    /// Proces v sandboxu je zavrnjen, tudi če ni potomec aplikacije (dvojni fork se ne izogne sandboxu).
    @Test func refusesASandboxedProcess() throws {
        let (l, routerPort) = listener()
        defer { close(l) }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/sandbox-exec")
        p.arguments = ["-p", "(version 1)(allow default)", "/usr/bin/nc", "127.0.0.1", String(routerPort)]
        p.standardInput = Pipe()
        try p.run()
        defer { p.terminate() }
        let (s, peerPort) = acceptPeerPort(l)
        defer { close(s) }
        #expect(PeerProcess.refusal(peerPort: peerPort, routerPort: routerPort, appPID: 999_999)?.contains("sandboxed") == true)
    }

    @Test func adminAPIRefusesWhatPeerCheckRefuses() async {
        let app = FakeApp()
        let hooks = AdminHooks(secret: adminSecret, connect: "", version: "", installed: { false }, update: { f in
            var c = app.config; try f(&c); return c
        }, setKey: { _, _ in }, rotateToken: { "" }, peerCheck: { peer, router in peer == 4242 && router == 8765 ? "an agent" : nil })
        let r = Router(config: RouterConfig(), token: "tok", secrets: FakeSecrets(values: [:]), admin: hooks)
        var req = request("GET", Router.adminPrefix + "/status", headers: ["Host": "127.0.0.1:8765", "Authorization": "Bearer " + adminSecret])
        req.peer = "127.0.0.1"
        req.peerPort = 4242
        let w = FakeWriter()
        await r.handle(req, writer: w)
        #expect(w.status == 403 && w.text.contains("an agent"))
        req.peerPort = 4243
        let ok = FakeWriter()
        await r.handle(req, writer: ok)
        #expect(ok.status == 200)
    }
}
