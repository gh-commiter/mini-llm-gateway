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
    let (b, p, bare) = Profiles.routeBody(Data(#"{"model":"claude-cli-opus@win","max_tokens":5}"#.utf8), names: names)
    let o = try? JSONSerialization.jsonObject(with: b) as? [String: Any]
    #expect(p == "win" && bare == "claude-cli-opus" && o?["model"] as? String == "claude-cli-opus" && o?["max_tokens"] as? Int == 5)
    let raw = Data(#"{"model":"x@other"}"#.utf8)
    let r = Profiles.routeBody(raw, names: names)
    #expect(r.body == raw && r.profile == nil && r.model == "x@other")
    #expect(Profiles.isAgentModel("claude-cli-opus") && Profiles.isAgentModel("claude-codex") && Profiles.isAgentModel("claude-copilot-gpt")
            && !Profiles.isAgentModel("claude-siri-ai") && !Profiles.isAgentModel("claude-chat-haiku"))
}

/// #8: --model …@profil izbere profil za ves zagon (enako kot TestModelProfileSelectsTheGateway).
@Test func modelSuffixSelectsTheProfile() {
    let names = ["default", "local"]
    let a = Profiles.modelProfile(["-p", "--model", "claude-cli-opus@local", "hi"], names: names)
    #expect(a.profile == "local" && a.args == ["-p", "--model", "claude-cli-opus", "hi"] && a.model == "claude-cli-opus@local")
    let b = Profiles.modelProfile(["--model=claude-opus-4@20250514"], names: names)
    #expect(b.profile == nil && b.args == ["--model=claude-opus-4@20250514"])
    #expect(Profiles.modelProfile(["--model=gpt@default"], names: names).args == ["--model=gpt"])
}

/// #7: claude pod minigw ne podeduje seje in prijave starševskega Claude Code (enako kot TestClaudeChildEnvDropsParentSession).
@Test func parentClaudeSessionIsNotInherited() {
    for k in ["CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_SSE_PORT", "CLAUDE_CODE_SESSION_ID",
              "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_MODEL", "anthropic_base_url"] {
        #expect(Profiles.parentClaudeVar(k), "\(k)")
    }
    for k in ["CLAUDE_CODE_GIT_BASH_PATH", "CLAUDE_CONFIG_DIR", "MINIGW_SHARE", "PATH", "HOME"] { #expect(!Profiles.parentClaudeVar(k), "\(k)") }
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

    init(models: String, redirect: String? = nil) throws {
        let p = NWParameters.tcp
        p.requiredLocalEndpoint = .hostPort(host: "127.0.0.1", port: .any)
        listener = try NWListener(using: p)
        let ready = DispatchSemaphore(value: 0)
        listener.stateUpdateHandler = { if case .ready = $0 { ready.signal() } }
        listener.newConnectionHandler = { [unowned self] c in
            c.start(queue: .global())
            self.read(c, Data(), models, redirect)
        }
        listener.start(queue: .global())
        ready.wait()
    }

    private func read(_ c: NWConnection, _ buf: Data, _ models: String, _ redirect: String?) {
        c.receive(minimumIncompleteLength: 1, maximumLength: 1 << 20) { d, _, end, _ in
            let b = buf + (d ?? Data())
            guard case .request(let r) = HTTPParser.parse(b) else { return end ? c.cancel() : self.read(c, b, models, redirect) }
            var body = Data(#"{"ok":true}"#.utf8)
            if let redirect, r.path == "/v1/models" {
                return c.send(content: HTTPResponse.head(status: 302, headers: [("Location", redirect)], contentLength: 0), completion: .contentProcessed { _ in c.cancel() })
            }
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
    // model brez orodij (npr. naslov seje) mape ne deli
    #expect(try await send(m.localURL, "POST", "/v1/messages", secret: m.secret, body: #"{"model":"claude-chat-haiku@win"}"#).0 == 200)
    #expect(shares.n == 0 && g2.log == ["Bearer tok-win|claude-chat-haiku||close"])
    for _ in 0..<2 { #expect(try await send(m.localURL, "POST", "/v1/messages", secret: m.secret, body: #"{"model":"claude-cli-opus@win"}"#) == (200, #"{"ok":true}"#)) }
    #expect(try await send(m.localURL, "POST", "/v1/messages/count_tokens", secret: m.secret, body: #"{"model":"claude-siri-ai"}"#).0 == 200)
    #expect(g2.log.suffix(2) == ["Bearer tok-win|claude-cli-opus|1|close", "Bearer tok-win|claude-cli-opus|1|close"])
    #expect(g1.log == ["Bearer tok-mini|claude-siri-ai|1|close"])
    #expect(shares.n == 1)
    let (st5, e5) = try await send(m.localURL, "POST", "/v1/messages", secret: m.secret, body: #"{"model":"x@off"}"#)
    #expect(st5 == 502 && e5.contains("gateway off is not reachable (minigw status)"), "\(e5)")
    // I4: kontrolni znaki v vrednosti glave → 400
    var bad = URLRequest(url: URL(string: m.localURL + "/v1/messages")!)
    bad.httpMethod = "POST"; bad.setValue("Bearer \(m.secret)", forHTTPHeaderField: "authorization")
    bad.setValue("a\u{7f}b", forHTTPHeaderField: "x-claude-mini-cwd"); bad.httpBody = Data(#"{"model":"claude-siri-ai"}"#.utf8)
    #expect((try await URLSession.shared.data(for: bad).1 as! HTTPURLResponse).statusCode == 400)
}

/// L2: seznam modelov ne sledi preusmeritvi drugega prehoda (žeton ne gre na drug proxy).
@Test func profileMuxModelsDoNotFollowRedirects() async throws {
    let other = try FakeGateway(models: #"{"id":"stolen"}"#)
    let g = try FakeGateway(models: "", redirect: "http://127.0.0.1:\(other.port)/v1/models")
    let ok = try FakeGateway(models: #"{"id":"claude-siri-ai"}"#)
    defer { other.listener.cancel(); g.listener.cancel(); ok.listener.cancel() }
    let m = ProfileMux(targets: [.init(name: "mini", url: "http://127.0.0.1:\(ok.port)", token: "t1"),
                                 .init(name: "evil", url: "http://127.0.0.1:\(g.port)", token: "t2")])
    try m.start()
    defer { m.stop() }
    let (st, list) = try await send(m.localURL, "GET", "/v1/models", secret: m.secret)
    #expect(st == 200 && !list.contains("stolen") && list.contains("claude-siri-ai"))
}

/// minigw reconnect: cela vrstica ali le žeton (shranjeni ključ prehoda ostane); nepopoln žeton zavrnjen (kot Go TestNewToken).
@Test func reconnectNewToken() {
    #expect(Profiles.newToken("t", key: "old")! == ("t", "old"))
    #expect(Profiles.newToken("t#n", key: "old")! == ("t", "n"))
    #expect(Profiles.newToken("t", key: nil)! == ("t", nil))
    for l in ["", "#", "#n", "t#"] { #expect(Profiles.newToken(l, key: "old") == nil, "\(l)") }
}

/// Nova oblika žetona (mgw- + base64url, kot minigw serve): router ne preverja predpone, stari sk-siri-… žeton ostane veljaven.
@Test func gatewayTokenFormatsAccepted() {
    let old = "sk-siri-" + String(repeating: "ab", count: 32), new = Router.newToken()
    #expect(new.hasPrefix("mgw-") && new.count == 4 + 43 && new != Router.newToken())
    #expect(new.dropFirst(4).allSatisfy { $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "-" || $0 == "_") })
    for t in [old, new] {
        let r = Router(config: RouterConfig(), token: t, secrets: FakeSecrets(values: [:]))
        #expect(r.authorized(request("GET", "/v1/models", headers: ["authorization": "Bearer " + t])))
        #expect(r.authorized(request("GET", "/v1/models", headers: ["x-api-key": t])))
        #expect(!r.authorized(request("GET", "/v1/models", headers: ["authorization": "Bearer " + t + "x"])))
    }
}

/// minigw gw: odklep prijavnega keychaina le v terminalu in le, ko je zaklenjen (tudi v SSH, #62); brez pravega keychaina.
@Test func keychainUnlockOffer() {
    for st in [errSecInteractionNotAllowed, errSecAuthFailed] {
        #expect(!Keychain.offerUnlock(status: st, terminal: true, ssh: true, locked: false))  // odklenjen: le namig
        #expect(Keychain.offerUnlock(status: st, terminal: true, ssh: true, locked: true))
        #expect(Keychain.offerUnlock(status: st, terminal: true, ssh: false, locked: true))
        #expect(!Keychain.offerUnlock(status: st, terminal: false, ssh: true, locked: true))  // skripta: le sporočilo
        #expect(!Keychain.offerUnlock(status: st, terminal: true, ssh: false, locked: false))
    }
    for st in [errSecSuccess, errSecItemNotFound, errSecNoSuchKeychain, errSecUserCanceled] {
        #expect(!Keychain.offerUnlock(status: st, terminal: true, ssh: true, locked: true))
    }
    // namig: v SSH ukaz security le za zaklenjen keychain (brez terminala), odklenjen dobi svoj namig (#62)
    #expect(Keychain.deniedHint(ssh: true, locked: false).hasPrefix("macOS reports the login keychain as unlocked"))
    #expect(!Keychain.deniedHint(ssh: true, locked: false).contains("security unlock-keychain"))
    #expect(Keychain.deniedHint(ssh: true, locked: true).contains("run security unlock-keychain"))
}

/// minigw gw: napačno geslo (Open Directory) ne pokliče SecKeychainUnlock, pravo ga (#62); brez pravega gesla.
@Test func keychainUnlockVerifiesFirst() {
    var calls = 0
    let unlock: (UnsafePointer<CChar>) -> OSStatus = { _ in calls += 1; return errSecSuccess }
    let pw: [CChar] = Array("x".utf8CString)
    #expect(Keychain.unlock(password: pw, verify: { _ in false }, unlock: unlock) == errSecAuthFailed)
    #expect(calls == 0)
    #expect(Keychain.unlock(password: pw, verify: { String(cString: $0) == "x" }, unlock: unlock) == errSecSuccess)
    #expect(calls == 1)
}
