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
    var login = false  // Open at Login
    init(_ c: RouterConfig = RouterConfig()) { config = c }

    /// Router, kot ga aplikacija zgradi ob vsakem zagonu (apply znova zažene router z novimi nastavitvami).
    func router(session: URLSession = .shared, scratchInUse: Bool? = nil) -> Router {
        var hooks = AdminHooks(secret: adminSecret, connect: "tok#KEY", version: "9.9", installed: { self.lock.withLock { self.login } },
                               setInstalled: { on in self.lock.withLock { self.login = on } },
                               commit: { f, keys in
                                   try self.lock.withLock {
                                       var c = self.config
                                       try f(&c)
                                       for (a, k) in keys { self.keys[a] = k }  // ključi le po uspešni spremembi
                                       self.config = c
                                       return c
                                   }
                               },
                               rotateToken: { "newtok#KEY" })
        if let busy = scratchInUse { hooks.scratchInUse = { busy } }
        return lock.withLock { Router(config: config, token: "tok", secrets: FakeSecrets(values: keys), session: session, admin: hooks,
                                         copilotConfig: URL(fileURLWithPath: "/nonexistent/.copilot/config.json")) }  // #81: nikoli prava prijava
    }
}

let adminSecret = String(repeating: "S", count: 43)

let testHash = Data(repeating: 7, count: 32)  // h rokovanja Noise v testih

/// Zahteva kot minigw router: povezava Noise (h) in dokaz MGW-Admin; encrypted: false = navaden HTTP z Bearer.
func admin(_ r: Router, _ method: String, _ path: String, body: String = "", host: String = "127.0.0.1:8765",
           auth: String? = adminSecret, peer: String = "127.0.0.1", encrypted: Bool = true, proofHash: Data? = nil) async -> FakeWriter {
    var h = ["Host": host]
    if let auth {
        h["Authorization"] = encrypted ? AdminHooks.scheme + AdminHooks.proof(secret: auth, hash: proofHash ?? testHash) : "Bearer " + auth
    }
    var req = request(method, Router.adminPrefix + path, headers: h, body: body)
    req.peer = peer
    req.noiseHash = encrypted ? testHash : nil
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
        #expect(await admin(r, "GET", "/status", peer: "192.168.1.9", encrypted: false).status == 403)
        #expect(await admin(r, "GET", "/status", host: "evil.example:8765", encrypted: false).status == 403)  // DNS rebinding, pred overitvijo
        #expect(await admin(r, "GET", "/status", host: "127.0.0.1:1", encrypted: false).status == 403)
        #expect(await admin(r, "GET", "/status", host: "x").status == 200)  // Noise: ni brskalnik
        #expect(await admin(r, "GET", "/status", encrypted: false).status == 401)  // Bearer s pravo skrivnostjo: nikoli
        #expect(await admin(r, "GET", "/status", proofHash: Data(repeating: 8, count: 32)).status == 401)  // dokaz druge povezave
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

    /// minigw gw api (minigw-tui): metoda, pot pod skrbniškim API in telo JSON s stdin.
    @Test func gwAPIRequestValidation() throws {
        #expect(AdminClient.apiRequestOK("GET", "/status"))
        #expect(AdminClient.apiRequestOK("PATCH", "/providers/lab/models?filter=qwen"))
        for (m, p) in [("PUT", "/general"), ("GET", "status"), ("GET", "/x/../general"), ("GET", "/st atus"), ("GET", "/status#x"),
                       ("GET", "/status\n"), ("GET", "/status\u{7f}"), ("get", "/status")] {
            #expect(!AdminClient.apiRequestOK(m, p), "\(m) \(p)")
        }
        #expect(try AdminClient.apiBody(Data(" \n".utf8)) == nil)
        #expect(try AdminClient.apiBody(Data(#"{"a":1}"#.utf8)) == Data(#"{"a":1}"#.utf8))
        #expect(throws: (any Error).self) { try AdminClient.apiBody(Data("not json".utf8)) }
    }

    @Test func strictJSON() async {
        let r = FakeApp().router()
        let unknown = await admin(r, "PATCH", "/agents/claude", body: #"{"tools":"Read","bogus":1}"#)
        #expect(unknown.status == 400 && unknown.text.contains("unknown field"))
        #expect(await admin(r, "PATCH", "/agents/claude", body: #"{"tools":"Read"}x"#).status == 400)
        #expect(await admin(r, "PATCH", "/agents/claude", body: #"{"tools":"Read"}{"tools":"Bash"}"#).status == 400)
        #expect(await admin(r, "PATCH", "/agents/claude", body: #"{"allowLocalTools":1}"#).status == 400)  // 1 ni true
        let dup = await admin(r, "PATCH", "/agents/claude", body: #"{"tools":"Read","tools":"Bash"}"#)  // kot Go
        #expect(dup.status == 400 && dup.text.contains("duplicate field"))
        #expect(AdminBody.duplicateKey(Data(#"{"a":{"b":1,"b":2},"c":["\"a\":"],"d":1}"#.utf8)) == nil)  // le prva raven
        #expect(await admin(r, "PATCH", "/general", body: #"{"port":true}"#).status == 400)
        #expect(await admin(r, "PATCH", "/agents/claude", body: #"{"tools":"Read;rm"}"#).status == 400)
        #expect(await admin(r, "PATCH", "/agents/claude", body: "[1]").status == 400)
        let big = String(repeating: " ", count: 1 << 20) + "{}"
        #expect(await admin(r, "PATCH", "/general", body: big).status == 413)
    }

    @Test func liveApplyThroughAppConfig() async throws {
        let app = FakeApp()
        let id = Noise.base64url(Curve25519.Signing.PrivateKey().publicKey.rawRepresentation)
        #expect(await admin(app.router(), "POST", "/devices", body: #"{"id":"203.0.113.5"}"#).status == 400)
        let w = await admin(app.router(), "POST", "/devices", body: #"{"id":"\#(id)","name":"PC"}"#)
        #expect(w.status == 200 && w.text.contains(#""added":true"#))
        #expect(app.config.devices.map(\.id) == [id])  // Settings opazuje te nastavitve
        // allowAnyDevice le pri praznem seznamu (poročilo Windows #4)
        #expect(await admin(app.router(), "PATCH", "/general", body: #"{"allowAnyDevice":true}"#).status == 400)
        #expect(await admin(app.router(), "GET", "/devices").text.contains(id))  // nov router (po apply) jo vidi
        #expect(await admin(app.router(), "PATCH", "/siri", body: #"{"enabled":true,"allow":["\#(id)"]}"#).status == 200)
        #expect(app.config.siri.enabled && app.config.devices[0].siri)
        #expect(await admin(app.router(), "PATCH", "/appleFM", body: #"{"enabled":false,"instructions":"Be brief."}"#).status == 200)
        #expect(!app.config.appleFM.enabled && app.config.appleFM.instructions == "Be brief.")
        #expect(await admin(app.router(), "GET", "/appleFM").text.contains(#""instructions":"Be brief.""#))
        #expect(await admin(app.router(), "PATCH", "/appleFM", body: #"{"model":"x"}"#).status == 400)
        let wt = await admin(app.router(), "PATCH", "/general", body: #"{"worktreeNetwork":false,"worktreeCodexNetwork":true}"#)
        #expect(wt.status == 200 && wt.text.contains(#""worktreeCodexNetwork":true"#))
        #expect(!app.config.worktreeNetwork && app.config.worktreeCodexNetwork)
        #expect(await admin(app.router(), "DELETE", "/devices/PC").status == 404)  // le po ID-ju: ime je oznaka
        #expect(await admin(app.router(), "DELETE", "/devices/\(id)").status == 200 && app.config.devices.isEmpty)

        #expect(await admin(app.router(), "PATCH", "/agents/claude", body: #"{"tools":"Read, Grep","folders":"~/src"}"#).status == 200)
        #expect(app.config.claudeCLI.tools == "Read,Grep" && app.config.claudeCLI.allowedRoots == "~/src")
        // #111: restricted (no "Allow local tools") configurations: names trimmed and checked, no tool outside the path rules, only modes that enforce them, rule-safe folders
        func claude(_ body: String) async -> FakeWriter { await admin(app.router(), "PATCH", "/agents/claude", body: body) }
        #expect(await claude(#"{"tools":" Read , Grep,Glob "}"#).status == 200 && app.config.claudeCLI.tools == "Read,Grep,Glob")
        for bad in ["Read,Bash", "Task", "mcp__x__run", "Read;rm", "Bash(git:*)"] {
            let r = await claude(#"{"tools":"\#(bad)"}"#)
            #expect(r.status == 400 && (r.text.contains("Allow local tools") || r.text.contains("not a tool name")), "\(bad) \(r.text)")
        }
        for mode in ["auto", "plan"] { #expect(await claude(#"{"permissionMode":"\#(mode)"}"#).status == 400, "\(mode)") }
        #expect(await claude(#"{"permissionMode":"acceptEdits"}"#).status == 200 && app.config.claudeCLI.permissionMode == "acceptEdits")
        #expect(await claude(#"{"folders":"~/src,/x/Bad (copy)"}"#).status == 400 && app.config.claudeCLI.allowedRoots == "~/src")
        #expect(await claude(#"{"allowLocalTools":true,"tools":"Read,Bash","permissionMode":"dontAsk"}"#).status == 200)  // the sandbox confines Bash there
        #expect(await claude(#"{"allowLocalTools":false}"#).status == 400 && app.config.claudeCLI.allowLocalTools)  // only the switch goes off: the stored Bash is refused
        #expect(await claude(#"{"allowLocalTools":false,"tools":"Read,Grep"}"#).status == 200 && !app.config.claudeCLI.allowLocalTools)
        #expect(await admin(app.router(), "PATCH", "/agents/codex", body: #"{"write":false,"models":["gpt-5"]}"#).status == 200)
        #expect(!app.config.codex.write && app.config.codex.enabledModels == ["gpt-5"])
        #expect(await admin(app.router(), "PATCH", "/agents/grok", body: #"{"models":null}"#).status == 200 && app.config.grok.enabledModels == nil)
        #expect(await admin(app.router(), "PATCH", "/agents/grok", body: #"{"network":true}"#).status == 400)  // le Codex ima omrežje

        #expect(await admin(app.router(), "PATCH", "/general", body: #"{"port":9999,"allowedClients":"192.168.1.7"}"#).status == 200)
        #expect(app.config.port == 9999 && app.config.allowedClients == "192.168.1.7")
        #expect(await admin(app.router(), "PATCH", "/general", body: #"{"address":"192.168.1.5"}"#, host: "127.0.0.1:9999").status == 400)
        #expect(await admin(app.router(), "PATCH", "/general", body: #"{"allowedClients":"not-an-ip"}"#, host: "127.0.0.1:9999").status == 400)
        // Open at Login prek API (skill /minigw:gw install) in enak povzetek kot Go: autostart, agents.
        #expect(await admin(app.router(), "PATCH", "/general", body: #"{"startAtSignIn":true}"#, host: "127.0.0.1:9999").status == 200 && app.login)
        let st = await admin(app.router(), "GET", "/status", host: "127.0.0.1:9999").text
        #expect(st.contains(#""providers":"#) && st.contains(#""gateways":"#))  // providers kanonično, gateways starejše ime
        let pv = await admin(app.router(), "GET", "/providers", host: "127.0.0.1:9999")
        #expect(pv.status == 200 && pv.text.contains(#""providers":"#) && pv.text.contains(#""gateways":"#))
        // agents: števec je globalen (vzporedni testi agentov), zato le število (I1).
        #expect(st.contains(#""autostart":"sign-in""#) && st.range(of: #""agents":\d+"#, options: .regularExpression) != nil && st.contains(#""installed":true"#))
        let t = await admin(app.router(), "POST", "/general/token", host: "127.0.0.1:9999")
        #expect(t.status == 200 && t.text.contains("--token 'newtok#KEY'"))
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

    /// S1: lažen strežnik z lastnim ključem (podtaknjen ključ routerja) konča rokovanje, a dobi le dokaz, vezan na to
    /// rokovanje: ne skrivnosti in nič, kar bi pravi router sprejel.
    @Test func fakeResponderGetsNothingReusable() async throws {
        // Pravi router (strežnik Noise) in lažen odgovornik z lastnim ključem.
        let app = FakeApp()
        app.config.host = "127.0.0.1"
        let realKey = Noise.newKey()
        let (server, port) = try await startOnFreePort(key: realKey) { app.config.port = $0; return app.router() }
        defer { server.stop() }
        let fakeKey = Noise.newKey()
        let (l, fakePort) = listener()
        defer { close(l) }
        let got = Received()
        // Lastna nit, ne DispatchQueue.global(): pod obremenitvijo (vzporedni testi) blok tam začne prepozno.
        Thread {
            let c = accept(l, nil, nil)
            defer { close(c) }
            func read(_ n: Int) -> Data {
                var d = Data(), buf = [UInt8](repeating: 0, count: 65536)
                while d.count < n { let k = Darwin.read(c, &buf, min(buf.count, n - d.count)); if k <= 0 { break }; d += buf[0..<k] }
                return d
            }
            let msg = read(4 + Noise.handshakeLength)
            guard let (reply, session) = try? Noise.respond(Data(msg.dropFirst(4)), key: fakeKey, suite: .aesGCM) else { return }
            _ = reply.withUnsafeBytes { write(c, $0.baseAddress, $0.count) }
            var raw = Data(), plain = Data(), buf = [UInt8](repeating: 0, count: 65536)
            while !String(decoding: plain, as: UTF8.self).contains("\r\n\r\n") {
                let k = Darwin.read(c, &buf, buf.count)
                if k <= 0 { break }
                raw += buf[0..<k]
                if let p = try? Noise.unframe(&raw, session.receive) { plain += p }
            }
            got.add(plain)
        }.start()
        let secret = adminSecret
        _ = AdminClient(port: fakePort, routerKey: fakeKey.publicKey.rawRepresentation)
            .request("GET", Router.adminPrefix + "/status", nil, authorization: { h in AdminHooks.scheme + AdminHooks.proof(secret: secret, hash: h) }, timeout: 30)
        for _ in 0..<300 where got.data.isEmpty { try await Task.sleep(for: .milliseconds(50)) }  // namesto fiksnih 100 ms
        let text = String(decoding: got.data, as: UTF8.self)
        let captured = try #require(text.split(separator: "\r\n").first { $0.lowercased().hasPrefix("authorization: ") }).dropFirst("authorization: ".count)
        #expect(captured.hasPrefix(AdminHooks.scheme) && !text.contains(adminSecret))
        // Ponovitev pri pravem routerju: druga povezava, drug h → 401; pravi dokaz → 200.
        let real = AdminClient(port: port, routerKey: realKey.publicKey.rawRepresentation)
        let replay = String(captured)
        #expect(real.request("GET", Router.adminPrefix + "/status", nil, authorization: { _ in replay }, timeout: 30).0 == 401)
        #expect(real.request("GET", Router.adminPrefix + "/status", nil, authorization: { h in AdminHooks.scheme + AdminHooks.proof(secret: secret, hash: h) }, timeout: 30).0 == 200)
        // Brez ključa routerja (program na vratih ne konča rokovanja): 0, nič poslano.
        #expect(AdminClient(port: port, routerKey: Noise.newKey().publicKey.rawRepresentation).request("GET", "/health/live", nil, timeout: 30).0 == 0)
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

/// Zaporedno: otrok iz refusesAChildProcess (Process podeduje odprte datoteke) bi sicer podedoval vtičnico drugega testa.
@Suite(.serialized) struct AdminPeerProcess {
    /// Odjemalec je ta proces: »aplikacija« sama (oz. njen otrok) je zavrnjena, tuja aplikacija ne; neznan proces zavrnjen.
    @Test func identifiesTheClientProcess() throws {
        let (l, routerPort) = listener()
        defer { close(l) }
        let c = socket(AF_INET, SOCK_STREAM, 0)
        defer { close(c) }
        _ = fcntl(c, F_SETFD, FD_CLOEXEC)  // otroci vzporednih testov je ne podedujejo (sicer dva procesa z vtičnico)
        var a = sockaddr_in()
        a.sin_family = sa_family_t(AF_INET); a.sin_addr.s_addr = inet_addr("127.0.0.1"); a.sin_port = routerPort.bigEndian
        _ = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(c, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
        let (s, peerPort) = acceptPeerPort(l)
        defer { close(s) }
        func holders() -> String {  // diagnostika: procesi z vtičnico (otrok vzporednega testa, ki jo je podedoval?)
            PeerProcess.pids(localPort: peerPort, remotePort: routerPort).map { p -> String in
                var b = [CChar](repeating: 0, count: 4096); proc_pidpath(p, &b, 4096); return "\(p) \(String(cString: b))"
            }.joined(separator: ", ")
        }
        // Med posix_spawn vzporednega testa ima otrok za trenutek kopijo vseh datotek (tudi FD_CLOEXEC, do exec): drug proces
        // z isto vtičnico je lahko bežen. Zato vsako preverjanje do 2 s, dokler ne velja.
        func eventually(_ ok: () -> Bool) -> Bool {
            for _ in 0..<40 { if ok() { return true }; usleep(50_000) }
            return false
        }
        #expect(eventually { PeerProcess.pid(localPort: peerPort, remotePort: routerPort) == getpid() }, "\(holders())")
        #expect(eventually { PeerProcess.refusal(peerPort: peerPort, routerPort: routerPort, appPID: getpid())
                    == "admin requests from agent processes are not allowed" }, "\(holders())")
        #expect(eventually { PeerProcess.refusal(peerPort: peerPort, routerPort: routerPort, appPID: 999_999) == nil },  // veriga do PID 1
                "\(PeerProcess.refusal(peerPort: peerPort, routerPort: routerPort, appPID: 999_999) ?? ""): \(holders())")
        #expect(PeerProcess.parent(1) == 0 && PeerProcess.parent(999_999) == nil)
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
        let hooks = AdminHooks(secret: adminSecret, connect: "", version: "", installed: { false }, commit: { f, _ in
            var c = app.config; try f(&c); return c
        }, rotateToken: { "" }, peerCheck: { peer, router in peer == 4242 && router == 8765 ? "an agent" : nil })
        let r = Router(config: RouterConfig(), token: "tok", secrets: FakeSecrets(values: [:]), admin: hooks)
        var req = request("GET", Router.adminPrefix + "/status", headers: ["Host": "127.0.0.1:8765",
                          "Authorization": AdminHooks.scheme + AdminHooks.proof(secret: adminSecret, hash: testHash)])
        req.peer = "127.0.0.1"
        req.noiseHash = testHash
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

/// S7: okno Settings z neshranjenimi spremembami ne povozi sprememb skrbniškega API (tudi odstranitev).
@Test func settingsDraftMergesOutsideChanges() {
    var old = RouterConfig()
    old.devices = [DeviceConfig(id: "a", name: "A"), DeviceConfig(id: "b", name: "B")]
    old.claudeCLI.tools = "Read"
    var draft = old
    draft.port = 9000  // uporabnik tipka
    draft.devices[0].name = "A (renamed)"
    var new = old  // skrbniški API: nova orodja, b odstranjena, c dodana, a nespremenjena
    new.claudeCLI.tools = "Read,Grep"
    new.devices = [DeviceConfig(id: "a", name: "A"), DeviceConfig(id: "c", name: "C")]
    let m = RouterConfig.merge(draft: draft, old: old, new: new)
    #expect(m.port == 9000 && m.claudeCLI.tools == "Read,Grep")
    #expect(m.devices.map(\.id) == ["a", "c"] && m.devices[0].name == "A (renamed)")
    #expect(RouterConfig.merge(draft: old, old: old, new: new) == new)  // brez neshranjenih sprememb: vse iz new
}

@Test func settingsMergeKeepsAgentWriteInSync() {
    // Okno, odprto s starim osnutkom, mora po spremembi drugje (skrbniški API) kazati in uveljaviti novo vrednost.
    let old = RouterConfig()
    var new = old
    new.codex.write = false; new.grok.write = false; new.copilot.write = true
    let m = RouterConfig.merge(draft: old, old: old, new: new)
    #expect(!m.codex.write && !m.grok.write && m.copilot.write)
}

/// Revizija pariteta, vrstica 6, in varnostni pregled parity-cli (M1, L1-L4): končne točke minigw serve tudi v aplikaciji.
@Test func adminParityWithServe() async throws {
    let fm = FileManager.default
    // Scratch v domači mapi (začasne mape so zavrnjene, M1)
    let tmp = fm.urls(for: .cachesDirectory, in: .userDomainMask)[0].appendingPathComponent("minigw-test-adm6-" + UUID().uuidString)
    defer { try? fm.removeItem(at: tmp) }
    let a = tmp.appendingPathComponent("a"), b = tmp.appendingPathComponent("b"), allowed = tmp.appendingPathComponent("proj")
    try fm.createDirectory(at: RouterConfig.dedicated(a).appendingPathComponent("sync"), withIntermediateDirectories: true)
    try fm.createDirectory(at: b, withIntermediateDirectories: true)
    try fm.createDirectory(at: allowed.appendingPathComponent("inner"), withIntermediateDirectories: true)
    fm.createFile(atPath: RouterConfig.dedicated(a).appendingPathComponent("sync/x").path, contents: Data("x".utf8))
    var c = RouterConfig()
    c.scratchFolder = a.path
    c.claudeCLI.allowedRoots = allowed.path
    c.providers = [ProviderConfig(id: "gw-1", name: "or", enabled: true, baseURL: "https://openrouter.ai/api", aliases: ""),
                   ProviderConfig(id: "gw-2", name: "lite", enabled: true, baseURL: "https://lite.example", aliases: "")]
    c.claudeCLI.executable = "/nonexistent/claude"
    c.codex.executable = "/nonexistent/codex"
    c.grok.executable = "/nonexistent/grok"
    let app = FakeApp(c)

    // PATCH /providers/{name}: ime, vklop, aliasi; ključ ostane pod id; L4: imena kot na Go, zasedeno v update
    #expect(await admin(app.router(), "PATCH", "/providers/or", body: #"{"name":"open","enabled":false,"aliases":"x = y"}"#).status == 200)
    let p = app.config.providers[0]
    #expect(p.id == "gw-1" && p.name == "open" && !p.enabled && p.aliases == "x = y")
    #expect(await admin(app.router(), "PATCH", "/providers/open", body: #"{"name":"lite"}"#).status == 400)
    #expect(await admin(app.router(), "PATCH", "/providers/open", body: #"{"name":"has space"}"#).status == 400)
    #expect(await admin(app.router(), "PATCH", "/providers/open", body: #"{"baseURL":"http://example.com"}"#).status == 400)
    #expect(await admin(app.router(), "PATCH", "/providers/nope", body: #"{"enabled":true}"#).status == 404)
    #expect(await admin(app.router(), "PATCH", "/providers/open", body: #"{"bogus":1}"#).status == 400)
    // insecure brez baseURL znova preveri URL (kot Go); privzeta ponudnika obdržita ime
    #expect(await admin(app.router(), "PATCH", "/providers/open", body: #"{"insecure":false}"#).status == 200)
    let b2 = FakeApp(RouterConfig())
    let lit = await admin(b2.router(), "PATCH", "/providers/LiteLLM", body: #"{"name":"mine"}"#)
    #expect(lit.status == 400 && lit.text.contains("built in"), "\(lit.text)")
    #expect(await admin(app.router(), "PATCH", "/providers/open", body: #"{"name":"OpenRouter"}"#).status == 400)

    // permissionMode, settingSources
    #expect(await admin(app.router(), "PATCH", "/agents/claude", body: #"{"permissionMode":"bogus"}"#).status == 400)
    #expect(await admin(app.router(), "PATCH", "/agents/claude", body: #"{"settingSources":"user,evil"}"#).status == 400)
    let w = await admin(app.router(), "PATCH", "/agents/claude", body: #"{"permissionMode":"acceptEdits","settingSources":"user, project"}"#)  // #111: plan and auto do not enforce the path rules: refused without "Allow local tools"
    #expect(w.status == 200 && w.text.contains(#""permissionMode":"acceptEdits""#))
    #expect(app.config.claudeCLI.permissionMode == "acceptEdits" && app.config.claudeCLI.settingSources == "user,project")

    // GET agents/claude/tools, agents/{codex,grok}/models (brez programa prazno), drugi agenti 404
    let t = await admin(app.router(), "GET", "/agents/claude/tools")
    #expect(t.status == 200 && t.text.contains(#""tools":[]"#))
    for k in ["codex", "grok"] {
        let m = await admin(app.router(), "GET", "/agents/\(k)/models")
        #expect(m.status == 200 && m.text.contains(#""models":[]"#), "\(k)")
    }
    #expect(await admin(app.router(), "GET", "/agents/copilot/models").status == 404)
    #expect(await admin(app.router(), "GET", "/agents/codex/tools").status == 404)

    // scratch: M1/L1 pravila poti (vse 400, nič premaknjeno), L2 neveljaven port ne premakne, L3 seje → 409
    for bad in ["relative/x", "/", NSHomeDirectory(), tmp.path, allowed.path, allowed.appendingPathComponent("inner").path,
                fm.temporaryDirectory.path, "/nonexistent-minigw"] {
        let r = await admin(app.router(), "PATCH", "/general", body: #"{"scratch":"\#(bad)"}"#)
        #expect(r.status == 400, "\(bad): \(r.text)")
    }
    #expect(await admin(app.router(), "PATCH", "/general", body: #"{"scratch":"\#(b.path)","port":0}"#).status == 400)
    let linked = tmp.appendingPathComponent("linked")
    try fm.createDirectory(at: linked, withIntermediateDirectories: true)
    try fm.createSymbolicLink(at: RouterConfig.dedicated(linked), withDestinationURL: b)
    #expect(await admin(app.router(), "PATCH", "/general", body: #"{"scratch":"\#(linked.path)"}"#).status == 400)
    #expect(app.config.scratchFolder == a.path && fm.fileExists(atPath: RouterConfig.dedicated(a).appendingPathComponent("sync/x").path))
    let busy = FakeApp(app.config)
    var hooks = busy.router().admin!
    hooks.scratchInUse = { true }
    #expect(await admin(Router(config: busy.config, token: "tok", secrets: FakeSecrets(values: [:]), admin: hooks),
                        "PATCH", "/general", body: #"{"scratch":"\#(b.path)"}"#).status == 409)
    let ok = await admin(app.router(scratchInUse: false), "PATCH", "/general", body: #"{"scratch":"\#(b.path)"}"#)
    #expect(ok.status == 200 && app.config.scratchFolder == (Sync.real(b)?.path ?? b.path), "\(ok.text)")
    #expect(fm.fileExists(atPath: RouterConfig.dedicated(b).appendingPathComponent("sync/x").path))
    #expect(!fm.fileExists(atPath: RouterConfig.dedicated(a).appendingPathComponent("sync/x").path))
}

/// GET /activity: le zahteve modelom (najnovejša prva, največ 200) in tekoče, ista polja kot minigw serve; starejši
/// activity.json (brez bytes in naprave) se še prebere.
@Test func activityEndpoint() async throws {
    let old = try JSONDecoder().decode([RequestLog].self, from: Data(#"[{"id":"\#(UUID().uuidString)","time":0,"method":"POST","path":"/v1/messages","model":"m","status":200,"ms":5}]"#.utf8))
    #expect(old.first?.bytes == nil && old.first?.device == nil)
    var logs = (0..<205).map { RequestLog(time: Date(timeIntervalSince1970: Double($0)), method: "POST", path: "/v1/messages", model: "m\($0)",
                                          provider: "p", status: 200, ms: $0) }.reversed() as [RequestLog]
    logs[0].bytes = 42; logs[0].device = "DEV"; logs[0].deviceName = "Bob's Mac"
    logs.insert(RequestLog(time: Date(), method: "GET", path: "/minigw/admin/v1/status", model: nil, provider: "admin", status: 200, ms: 1), at: 0)
    let app = FakeApp()
    var hooks = app.router().admin!
    hooks.activity = { [logs] in (logs, [(id: "req_1", time: Date(timeIntervalSince1970: 0), model: "claude-codex", step: "Read a.go")]) }
    let w = await admin(Router(config: app.config, token: "tok", secrets: FakeSecrets(values: [:]), admin: hooks), "GET", "/activity")
    #expect(w.status == 200)
    let body = try #require(w.text.range(of: "\r\n\r\n").map { Data(w.text[$0.upperBound...].utf8) })
    let o = try #require(try JSONSerialization.jsonObject(with: body) as? [String: Any])
    let reqs = try #require(o["requests"] as? [[String: Any]]), runs = try #require(o["inflight"] as? [[String: Any]])
    #expect(reqs.count == 200 && reqs[0]["model"] as? String == "m204" && reqs[1]["bytes"] as? Int == 0)
    #expect(Set(reqs[0].keys) == ["time", "method", "path", "model", "status", "ms", "bytes", "device", "deviceName"])
    #expect(reqs[0]["deviceName"] as? String == "Bob's Mac" && reqs[0]["bytes"] as? Int == 42 && reqs[0]["time"] as? String == "1970-01-01T00:03:24Z")
    #expect(runs.count == 1 && runs[0]["id"] as? String == "req_1" && runs[0]["step"] as? String == "Read a.go"
            && runs[0]["time"] as? String == "1970-01-01T00:00:00Z")
}

/// Activity: neznan model (404) in predolg niz odjemalca sta »unknown« (zgodovina in tekoča zahteva).
@Test func activityUnknownModel() async {
    final class Seen: @unchecked Sendable { let lock = NSLock(); var logs: [RequestLog] = []; var started: [String] = [] }
    let seen = Seen()
    let r = Router(config: RouterConfig(), token: "tok", secrets: FakeSecrets(values: [:]),
                   onLog: { e in seen.lock.withLock { seen.logs.append(e) } },
                   onActivity: { if case .started(_, _, let m) = $0 { seen.lock.withLock { seen.started.append(m) } } })
    for model in ["nope-SECRET", String(repeating: "m", count: 201)] {
        let w = FakeWriter()
        await r.handle(request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok"], body: #"{"model":"\#(model)","messages":[]}"#), writer: w)
        #expect(w.status == 404)
    }
    seen.lock.withLock {
        #expect(seen.logs.map(\.model) == ["unknown", "unknown"] && seen.started == ["nope-SECRET", "unknown"])
    }
    #expect(Router.activityModel("claude-x", status: 200) == "claude-x")
}

/// M2: stara privzeta settingSources (project,local) postane "", izrecna druga ostane.
@Test func settingSourcesMigration() throws {
    let old = try JSONDecoder().decode(ClaudeCLIConfig.self, from: Data(#"{"settingSources":"project,local"}"#.utf8))
    let custom = try JSONDecoder().decode(ClaudeCLIConfig.self, from: Data(#"{"settingSources":"user"}"#.utf8))
    #expect(old.settingSources == "" && custom.settingSources == "user" && ClaudeCLIConfig().settingSources == "")
}

/// M1: Scratch v denyRead peskovnika Claude (dobesedno in kanonično), mapa seje ostane berljiva.
@Test func scratchDeniedInClaudeSandbox() {
    var cfg = ClaudeCLIConfig()
    cfg.allowLocalTools = true
    let scratch = URL(fileURLWithPath: "/Volumes/SSD/Mini Gateway Scratch")
    let args = cfg.agentArgs(cwd: URL(fileURLWithPath: "/Volumes/SSD/Mini Gateway Scratch/sync/d/x"), allTools: [], scratch: scratch, folders: AllowedFolders())
    let s = args[args.firstIndex(of: "--settings")! + 1]
    #expect(s.contains(#""denyRead":["~","\/Volumes\/SSD\/Mini Gateway Scratch","#) || s.contains(#""denyRead":["~","/Volumes/SSD/Mini Gateway Scratch","#), "\(s)")
    let wt = cfg.worktreeArgs(root: URL(fileURLWithPath: "/w"), maven: URL(fileURLWithPath: "/m"), cwd: URL(fileURLWithPath: "/w"), allTools: [], network: false, scratch: scratch)
    #expect(wt[wt.firstIndex(of: "--settings")! + 1].contains("Mini Gateway Scratch"))
}

/// H1: dovoljena mapa okoli Scratch ali v njem: API jo zavrne, starejša nastavitev ne pride v peskovnik agenta.
@Test func allowedFolderAroundScratchRefused() async throws {
    let fm = FileManager.default
    let tmp = fm.urls(for: .cachesDirectory, in: .userDomainMask)[0].appendingPathComponent("minigw-test-h1-" + UUID().uuidString)
    defer { try? fm.removeItem(at: tmp) }
    let scratch = tmp.appendingPathComponent("s")
    try fm.createDirectory(at: RouterConfig.dedicated(scratch).appendingPathComponent("repos"), withIntermediateDirectories: true)
    var c = RouterConfig()
    c.scratchFolder = scratch.path
    let app = FakeApp(c)
    for bad in [tmp.path, RouterConfig.dedicated(scratch).appendingPathComponent("repos").path] {
        let r = await admin(app.router(), "PATCH", "/agents/claude", body: #"{"folders":"\#(bad)"}"#)
        #expect(r.status == 400 && r.text.contains("scratch folder"), "\(bad): \(r.text)")
    }
    var cfg = ClaudeCLIConfig()
    cfg.allowLocalTools = true
    cfg.allowedRoots = tmp.path
    let recorded = AllowedFolders()
    recorded.record(cfg.allowedRoots)
    let args = cfg.agentArgs(cwd: URL(fileURLWithPath: "/p"), allTools: [], scratch: c.scratchURL, folders: recorded)
    #expect(!args.contains(tmp.path) && !args[args.firstIndex(of: "--settings")! + 1].contains(#""allowWrite":["\/p","#))
}
