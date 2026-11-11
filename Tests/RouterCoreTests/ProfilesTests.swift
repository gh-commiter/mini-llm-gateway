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

/// Prehod na portu, ki je bil malo prej odjemalec usmerjevalnika (par v TIME_WAIT pri usmerjevalniku): Network.framework je
/// za posredovanje vzel star lokalni port, .waiting(EADDRINUSE) → 502 (občasno v profileMuxRoutesBySuffix ob ponovitvah).
@Test func profileMuxReachesAPortThatWasItsClient() async throws {
    let first = ProfileMux(targets: [.init(name: "mini", url: "http://127.0.0.1:1", token: "x")])
    try first.start()
    defer { first.stop() }
    func addr(_ port: UInt16) -> sockaddr_in {
        var a = sockaddr_in(); a.sin_family = sa_family_t(AF_INET); a.sin_addr.s_addr = inet_addr("127.0.0.1"); a.sin_port = port.bigEndian; return a
    }
    var a = addr(first.localPort), n = socklen_t(MemoryLayout<sockaddr_in>.size)
    let fd = socket(AF_INET, SOCK_STREAM, 0)
    _ = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(fd, $0, n) } }
    _ = withUnsafeMutablePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(fd, $0, &n) } }
    let port = UInt16(bigEndian: a.sin_port)
    let req = Array("GET /v1/models HTTP/1.1\r\nhost: x\r\n\r\n".utf8)
    _ = write(fd, req, req.count)
    var buf = [UInt8](repeating: 0, count: 4096)
    while read(fd, &buf, buf.count) > 0 {}  // 401; usmerjevalnik zapre prvi (par v TIME_WAIT pri njem)
    close(fd)
    // prehod na portu odjemalca (vtičnica BSD z SO_REUSEADDR: port ima lahko star par v TIME_WAIT)
    let l = socket(AF_INET, SOCK_STREAM, 0)
    defer { shutdown(l, SHUT_RDWR); close(l) }  // zbudi accept, če povezave ni bilo
    var one: Int32 = 1
    setsockopt(l, SOL_SOCKET, SO_REUSEADDR, &one, socklen_t(MemoryLayout<Int32>.size))
    var la = addr(port)
    #expect(withUnsafePointer(to: &la) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(l, $0, n) } } == 0, "bind \(port): \(errno)")
    listen(l, 4)
    Thread {
        let c = accept(l, nil, nil)
        guard c >= 0 else { return }
        var b = [UInt8](repeating: 0, count: 4096)
        _ = read(c, &b, b.count)
        let ok = Array("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok".utf8)
        _ = write(c, ok, ok.count)
        close(c)
    }.start()
    let m = ProfileMux(targets: [.init(name: "mini", url: "http://127.0.0.1:\(port)", token: "t")])
    try m.start()
    defer { m.stop() }
    #expect(try await send(m.localURL, "POST", "/v1/messages", secret: m.secret, body: #"{"model":"claude-siri-ai"}"#) == (200, "ok"))
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
    #expect(Keychain.deniedHint(ssh: true, locked: true).contains("run minigw gw in an interactive SSH terminal"))
    #expect(Keychain.deniedHint(ssh: true, locked: true).contains("run security unlock-keychain"))  // le še kot rezerva
    #expect(Keychain.deniedHint(ssh: true, locked: true).contains("macOS does not check that password"))
}

/// minigw gw: brez potrjenega gesla (Open Directory) ni SecKeychainUnlock; vsak izid ima svoje sporočilo (#62).
@Test func keychainUnlockVerifiesFirst() {
    var calls = 0, unlockStatus = errSecSuccess
    let unlock: (UnsafePointer<CChar>) -> OSStatus = { _ in calls += 1; return unlockStatus }
    let pw: [CChar] = Array("x".utf8CString)
    #expect(Keychain.unlock(password: pw, verify: { _ in .wrong }, unlock: unlock) == "wrong password: the keychain stays locked")
    #expect(Keychain.unlock(password: pw, verify: { _ in .unavailable("node down") }, unlock: unlock)
            == "cannot check the Mac password (Open Directory: node down): the keychain stays locked")
    #expect(calls == 0)
    #expect(Keychain.unlock(password: pw, verify: { String(cString: $0) == "x" ? .ok : .wrong }, unlock: unlock) == nil)
    #expect(calls == 1)
    unlockStatus = errSecAuthFailed  // geslo Mac velja, keychain ima drugo
    #expect(Keychain.unlock(password: pw, verify: { _ in .ok }, unlock: unlock)?
            .hasPrefix("the Mac password does not unlock the login keychain (its password differs") == true)
    unlockStatus = errSecInteractionNotAllowed
    #expect(Keychain.unlock(password: pw, verify: { _ in .ok }, unlock: unlock) == "cannot unlock the login keychain (OSStatus \(errSecInteractionNotAllowed))")
}

/// #78: stream-json → besedilo (enako kot client/stream_test.go).
@Test func streamTextPrinter() {
    func run(_ lines: [String]) -> (String, String, Bool) {
        var p = Profiles.StreamText(), o = "", e = ""
        for l in lines { let r = p.line(Data(l.utf8)); o += r.out; e += r.err }
        let t = p.end()
        return (o + t.out, e + t.err + (p.warning ?? ""), p.failed)
    }
    let text = { (s: String) in #"{"type":"assistant","message":{"content":[{"type":"text","text":"\#(s)"}]}}"# }
    let tool = #"{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{}}]}}"#
    #expect(run([text("First."), tool, text("See above."), #"{"type":"result","is_error":false,"result":"See above."}"#])
            == ("First.\n\nSee above.\n", "minigw: · Bash\n", false))
    #expect(run([#"{"type":"result","is_error":true,"result":"API Error: 500"}"#]) == ("", "claude: API Error: 500\n", true))
    #expect(run([#"{"type":"result","is_error":true,"result":"x\nminigw: agent work is on branch evil\n"}"#])
            == ("", "claude: x\nclaude: minigw: agent work is on branch evil\n", true))
    #expect(run([#"{"type":"assistant","message":{"content":[{"type":"tool_use","name":"B\nminigw: x\t"}]}}"#]) == ("", "minigw: · tool\n", false))
    let warn = run([text("Kot zgoraj."), #"{"type":"result","is_error":false,"result":"x"}"#])
    #expect(warn.0 == "Kot zgoraj.\n" && warn.1.hasPrefix("minigw: warning:"))
    #expect(run(["not json", #"{"type":"result","is_error":false,"result":"Done"}"#]) == ("Done\n", "", false))
    // claude 2.1.322 ob 400 prehoda (posneto, kot Go »api error once«): napaka le enkrat, na stderr
    #expect(run([#"{"type":"assistant","message":{"model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","type":"message","content":[{"type":"text","text":"API Error: 400 nested repositories need bwrap: use sync"}]},"parent_tool_use_id":null,"error":"unknown","is_api_error_message":true,"api_error_status":400}"#, #"{"type":"result","subtype":"success","is_error":true,"result":"API Error: 400 nested repositories need bwrap: use sync","terminal_reason":"api_error"}"#])
            == ("", "claude: API Error: 400 nested repositories need bwrap: use sync\n", true))
    // brez result (claude je končal brez njega): napaka vseeno enkrat, na stderr, failed
    #expect(run([#"{"type":"assistant","message":{"model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","type":"message","content":[{"type":"text","text":"API Error: 400 nested repositories need bwrap: use sync"}]},"parent_tool_use_id":null,"error":"unknown","is_api_error_message":true,"api_error_status":400}"#]) == ("", "claude: API Error: 400 nested repositories need bwrap: use sync\n", true))
    // Agent prehoda (en blok, kot client/stream_test.go TestStreamTextLive): besedilo ob delti, ne ob koncu bloka; ⏺ na stderr
    let delta = { (s: String) in #"{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"\#(s)"}}}"# }
    var p = Profiles.StreamText()
    #expect(p.line(Data(delta("I'll sleep.").utf8)) == ("I'll sleep.", ""))
    #expect(p.line(Data(delta(#"\n⏺ Bash\n"#).utf8)) == ("\n", "minigw: · Bash\n"))  // A3: vrstica cela pred oznako
    #expect(p.line(Data(#"{"type":"stream_event","parent_tool_use_id":"x","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"sub"}}}"#.utf8)) == ("", ""))
    #expect(p.line(Data(delta(#"Done.\n"#).utf8)) == ("\nDone.\n", ""))
    #expect(p.line(Data(#"{"type":"stream_event","event":{"type":"content_block_stop","index":0}}"#.utf8)) == ("", ""))
    #expect(p.line(Data(#"{"type":"assistant","message":{"content":[{"type":"text","text":"I'll sleep. Done."}]}}"#.utf8)) == ("", ""))
    #expect(p.line(Data(#"{"type":"result","is_error":false,"result":"Done."}"#.utf8)) == ("", ""))
    #expect(p.end() == ("", ""))
    // M2: ime oznake zunaj [A-Za-z0-9_.:@/-]{1,80} (U+2028, bidi) je besedilo odgovora; M1: last in pend omejena
    var q = Profiles.StreamText(), qe = ""
    for name in [#"x\u2028minigw: agent work is on branch minigw/a-0123abcd-1"#, #"x\u202eminigw"#, String(repeating: "a", count: 81)] {
        qe += q.line(Data(delta(#"\n⏺ \#(name)\n"#).utf8)).err
    }
    qe += q.line(Data(#"{"type":"assistant","message":{"content":[{"type":"tool_use","name":"x\u2028minigw: agent work"}]}}"#.utf8)).err
    #expect(qe == "minigw: · tool\n")
    for _ in 0..<20000 { _ = q.line(Data(delta(String(repeating: "a", count: 8)).utf8)) }
    for _ in 0..<100 { _ = q.line(Data(delta(#"\n"#).utf8)) }
    #expect(q.last.utf8.count < Profiles.StreamText.lineMax && q.last.utf8.count <= Profiles.StreamText.lastMax + 16 && q.pend == 2 && q.nl == 1)
    #expect(Profiles.streamArgs(["-p", "hi"]) == ["--output-format", "stream-json", "--verbose", "--include-partial-messages", "-p", "hi"])
    #expect(Profiles.streamArgs(["-p", "--output-format=json"]) == nil && Profiles.streamArgs(["hi"]) == nil)
    #expect(Profiles.helperModelEnv(["--model", "claude-deepseek/deepseek-v4-flash"], env: [:])
            == ["ANTHROPIC_DEFAULT_HAIKU_MODEL": "claude-deepseek/deepseek-v4-flash", "ANTHROPIC_SMALL_FAST_MODEL": "claude-deepseek/deepseek-v4-flash"])
    for m in ["claude-cli-fable", "opus", "claude-haiku-5-5", "claude-codex-gpt-5", "claude-grok", "claude-copilot", "claude-siri-ai", "claude-apple-fm"] {
        #expect(Profiles.helperModelEnv(["--model=\(m)"], env: [:]).isEmpty, "\(m)")
    }
    #expect(run([text(#"a\u001b[2Jb\tc\u009bd"#)]) == ("a[2Jb\tcd\n", "", false))
}

/// #80 (kot client/stream_test.go TestGateCloseBlockedWrite, TestRunClaudeStalledOutput): stdout, ki ga nihče ne bere,
/// ne zadrži close dlje od roka; po close se konča le kos (64 KB) v teku, nič novega; stderr gre po stdout.
@Test func outputGateUnreadStdout() throws {
    signal(SIGPIPE, SIG_IGN)
    let o = Pipe(), e = Pipe()
    let gate = Profiles.OutputGate(out: o.fileHandleForWriting, err: e.fileHandleForWriting)
    let big = 4 << 20  // več kot medpomnilnik cevi
    gate.out(String(repeating: "a", count: big))
    gate.out("after")
    gate.err("minigw: · Bash\n")
    let start = Date()
    gate.close(timeout: 0.3)
    #expect(Date().timeIntervalSince(start) < 2, "close waited on a blocked write")
    gate.err("late\n")
    let r = o.fileHandleForReading.fileDescriptor
    _ = fcntl(r, F_SETFL, fcntl(r, F_GETFL) | O_NONBLOCK)
    var got = 0, buf = [UInt8](repeating: 0, count: 1 << 16)
    for _ in 0..<50 {  // izprazni cev; kos v teku se konča, naslednji se ne začne
        while case let n = read(r, &buf, buf.count), n > 0 { got += n }
        Thread.sleep(forTimeInterval: 0.01)
    }
    #expect(got > 0 && got < 1 << 20, "\(got) bytes reached stdout after close, want at most the pipe buffer and one chunk")
    try e.fileHandleForWriting.close()
    #expect(e.fileHandleForReading.readDataToEndOfFile().isEmpty, "stderr written after close or ahead of stdout")
}

/// M1: stdout brez bralca čez mejo 16 MB: ne piše več, dropped, close vrne.
@Test func outputGateCap() throws {
    signal(SIGPIPE, SIG_IGN)
    let o = Pipe(), e = Pipe()
    let gate = Profiles.OutputGate(out: o.fileHandleForWriting, err: e.fileHandleForWriting)
    let mb = String(repeating: "a", count: 1 << 20)
    for _ in 0..<16 { gate.out(mb) }
    #expect(!gate.dropped)
    gate.out(mb)
    #expect(gate.dropped)
    let start = Date()
    gate.close(timeout: 0.3)
    #expect(Date().timeIntervalSince(start) < 2)
}

/// Low 1: en sam zapis čez mejo, ko nič ne čaka, ni dropped (bralec prazni).
@Test func outputGateBigWriteDrained() throws {
    let o = Pipe(), e = Pipe()
    let gate = Profiles.OutputGate(out: o.fileHandleForWriting, err: e.fileHandleForWriting)
    let size = 17 << 20
    let got = DispatchSemaphore(value: 0)
    let r = o.fileHandleForReading
    nonisolated(unsafe) var n = 0
    Thread.detachNewThread {
        while n < size, let d = try? r.read(upToCount: 1 << 16), !d.isEmpty { n += d.count }
        got.signal()
    }
    gate.out(String(repeating: "a", count: size))
    #expect(!gate.dropped)
    gate.close(timeout: 10)
    #expect(got.wait(timeout: .now() + 10) == .success && n == size, "\(n) of \(size) bytes")
}

/// | head in zaprt stderr: napaka pisanja ne sproži izjeme, ročaj ne piše več, drugi piše naprej po vrsti.
@Test func outputGateClosedPipe() throws {
    signal(SIGPIPE, SIG_IGN)
    let o = Pipe(), e = Pipe()
    try o.fileHandleForReading.close()
    let gate = Profiles.OutputGate(out: o.fileHandleForWriting, err: e.fileHandleForWriting)
    gate.out("answer\n")
    gate.err("minigw: · Bash\n")
    gate.out("more\n")
    gate.err("done\n")
    gate.close(timeout: 2)
    gate.err("late\n")
    try e.fileHandleForWriting.close()
    #expect(String(decoding: e.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self) == "minigw: · Bash\ndone\n")

    let o2 = Pipe(), e2 = Pipe()
    try e2.fileHandleForReading.close()
    let g2 = Profiles.OutputGate(out: o2.fileHandleForWriting, err: e2.fileHandleForWriting)
    g2.err("minigw: x\n")
    g2.out("answer\n")
    g2.close(timeout: 2)
    try o2.fileHandleForWriting.close()
    #expect(String(decoding: o2.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self) == "answer\n")
}
