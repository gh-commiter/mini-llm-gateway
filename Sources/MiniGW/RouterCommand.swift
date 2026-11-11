// minigw router: nastavitve aplikacije Mini Gateway na tem Macu prek skrbniškega API (docs/admin-api.md), z istimi
// ukazi in izpisom kot minigw router v Go. Nastavitve so v aplikaciji (UserDefaults, Keychain): brez tekoče
// aplikacije ni sprememb. Vrata, ključ routerja in skrivnost so v Keychainu aplikacije; po žici gre le dokaz MGW-Admin
// (HMAC nad rokovanjem Noise), zato lažen strežnik ali program na vratih ne dobi ničesar uporabnega (S1, M1).
import CryptoKit
import Foundation
import OpenDirectory
import RouterCore
import Security

let routerHelp = """
minigw gw: show or change the Mini Gateway app on this Mac. Changes apply at once; the app must be running.

  minigw gw status
  minigw gw devices [allow <device id> [--name N] | rename <device id> --name N | deny <device id>]
  minigw gw devices history <device id>                 the device's last connections
  minigw gw sessions [--device ID] [--folder DIR] | <session id> | clear
  minigw gw usage [--days N] [--device ID] [--folder DIR]   counters per folder, day (UTC) and agent (clear: sessions clear)
  minigw gw provider add <name> <baseURL> [--insecure]   the API key is read from stdin or asked for
  minigw gw provider remove <name> | list
  minigw gw provider models <name> [text] [--all] [--enable m1,m2|all] [--disable m1,m2|all]
  minigw gw claude [--path FILE] [--tools T1,T2] [--folders DIR1,DIR2] [--allow-local-tools=true|false]
                  [--ask-others=true|false]   (may ask other models of this gateway; needs --allow-local-tools)
  minigw gw claude|codex|grok|copilot [--work=direct|sync|worktree]
                                                        the highest work mode in which the agent may change the
                                                        client's folder (in a lower mode it only reads)
  minigw gw copilot login                               Copilot's sign-in state (copilot login on this Mac), never a value
  minigw gw token [rotate]                              print the connect line; rotate makes a new token
  minigw gw autostart [on|off]                          Open at Login (Settings → General); no argument: show it
  minigw gw settings                                    the settings in this terminal (minigw-tui; also over SSH)
  minigw gw api GET|POST|PATCH|DELETE <path>            one admin API request (docs/admin-api.md): the JSON body
                                                        on stdin, the JSON answer on stdout (used by minigw-tui)
"""

private struct AdminConnection {
    let port: UInt16
    let key: Data     // javni ključ routerja (Keychain aplikacije)
    let secret: String

    /// Vnos »admin« v Keychainu aplikacije (skrivnost, vrata, javni ključ): brez datotek in spremenljivk okolja, ki bi
    /// jih podtaknil drug proces. Skrivnost nikoli ne gre po žici, le dokaz MGW-Admin, vezan na rokovanje.
    static func open() -> AdminConnection {
        if ProcessInfo.processInfo.environment["CFFIXED_USER_HOME"] != nil {
            fail("CFFIXED_USER_HOME is set: minigw gw does not run with a redirected home folder")
        }
        let kc = Keychain(loginOnly: true)  // le prijavni keychain: drug ga ne zasenči
        var (v, st) = kc.lookup("admin")
        let ssh = !(ProcessInfo.processInfo.environment["SSH_CONNECTION"] ?? "").isEmpty
        var unlocked = false
        if appOwner().map({ $0 == getuid() }) ?? true,
           Keychain.offerUnlock(status: st, terminal: isatty(0) == 1 && isatty(2) == 1, ssh: ssh, locked: loginLocked()) {
            unlockLoginKeychain(ssh: ssh)
            (v, st) = kc.lookup("admin")  // en sam ponovni poskus
            unlocked = true
        }
        guard st == errSecSuccess, let o = v.flatMap({ try? JSONSerialization.jsonObject(with: Data($0.utf8)) as? [String: Any] }),
              let secret = o["secret"] as? String, let port = (o["port"] as? Int).flatMap(UInt16.init(exactly:)),
              let key = (o["key"] as? String).flatMap(Noise.fromBase64url), key.count == 32 else {
            fail(keychainHint(st, ssh: ssh && !unlocked))
        }
        let a = AdminConnection(port: port, key: key, secret: secret)
        guard a.raw("GET", "/health/live", nil).0 == 200 else {  // rokovanje s ključem aplikacije, brez dokaza
            fail("Mini Gateway does not answer on port \(port) with its gateway key: start the app (if it runs, another program uses that port; nothing was sent to it)")
        }
        return a
    }

    func raw(_ method: String, _ path: String, _ body: Data?, proof: Bool = false) -> (Int, Data?) {
        let c = AdminClient(port: port, routerKey: key)
        guard proof else { return c.request(method, path, body) }
        let s = secret
        return c.request(method, path, body, authorization: { h in AdminHooks.scheme + AdminHooks.proof(secret: s, hash: h) })
    }

    /// Napaka API konča ukaz z njenim sporočilom.
    func call(_ method: String, _ path: String, _ body: [String: Any]? = nil) -> [String: Any] {
        let d = send(method, path, body.flatMap { try? JSONSerialization.data(withJSONObject: $0) })
        return (try? JSONSerialization.jsonObject(with: d) as? [String: Any]) ?? [:]
    }

    /// Odgovor API kot bajti (gw api); napaka konča ukaz.
    func send(_ method: String, _ path: String, _ data: Data?) -> Data {
        let (status, d) = raw(method, Router.adminPrefix + path, data, proof: true)
        let o = d.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] } ?? [:]
        guard status != 0 else { fail("lost the connection to Mini Gateway") }
        guard status < 300 else { fail(clean(((o["error"] as? [String: Any])?["message"] as? String) ?? "HTTP \(status)")) }
        return d ?? Data()
    }
}

/// Prijavni keychain je zaklenjen (SecKeychainGetStatus).
private func loginLocked() -> Bool {
    var s: SecKeychainStatus = 0
    guard let kc = Keychain.loginKeychain(), SecKeychainGetStatus(kc, &s) == errSecSuccess else { return false }
    return s & SecKeychainStatus(kSecUnlockStateStatus) == 0
}

/// Odklene prijavni keychain z geslom s terminala (brez odmeva; nikoli iz argv, nikoli ukaz security), medpomnilnik
/// se pred izhodom pobriše. Napačno geslo konča ukaz. Keychain ostane odklenjen po privzetih nastavitvah macOS.
/// SecKeychainUnlock na odklenjenem keychainu vrne uspeh za vsako geslo (#62), v seji SSH pa tudi, ko ima Macova
/// lastna seja keychain odklenjen: zato geslo najprej preveri Open Directory (macVerify), pred klicem znova preveri
/// zaklep, po uspehu pa, da keychain res ni več zaklenjen.
private func unlockLoginKeychain(ssh: Bool) {
    guard let kc = Keychain.loginKeychain() else { return }
    try? err.write(contentsOf: Data("The login keychain is locked\(ssh ? " in this SSH session" : ""). Mac password for \(userName(getuid())) (to unlock it): ".utf8))
    var buf = [CChar](repeating: 0, count: 1024)
    let got = readpassphrase("", &buf, buf.count, RPP_REQUIRE_TTY) != nil
    let locked = loginLocked()  // med vnosom gesla ga je morda odklenil drug proces
    let failure = got ? Keychain.unlock(password: buf, verify: macVerify,
                                        unlock: { locked ? SecKeychainUnlock(kc, UInt32(strlen($0)), $0, true) : errSecSuccess })
                      : nil
    memset_s(&buf, buf.count, 0, buf.count)
    guard got else { fail("no password: the keychain stays locked") }
    if let failure { fail(failure) }
    guard locked else {
        try? err.write(contentsOf: Data("The login keychain was unlocked meanwhile; the password was checked but not needed.\n".utf8))
        return
    }
    if loginLocked() { fail("the keychain stays locked") }
}

/// Preveri geslo trenutnega uporabnika v Open Directory (neodvisno od stanja keychaina, #62). NSString kaže na
/// medpomnilnik klicatelja (bytesNoCopy), da v procesu ni še ene kopije; kopije, ki jih naredita Foundation in
/// opendirectoryd med klicem, ostanejo zunaj našega dosega in se ne pobrišejo.
private func macVerify(_ pw: UnsafePointer<CChar>) -> Keychain.PasswordCheck {
    guard let me = getpwuid(getuid()).map({ String(cString: $0.pointee.pw_name) }) else { return .unavailable("no user name") }
    guard let s = NSString(bytesNoCopy: UnsafeMutableRawPointer(mutating: pw), length: strlen(pw),
                           encoding: String.Encoding.utf8.rawValue, freeWhenDone: false) else {
        return .unavailable("the password is not valid UTF-8")
    }
    do {
        let node = try ODNode(session: ODSession.default(), type: ODNodeType(kODNodeTypeAuthentication))
        let rec = try node.record(withRecordType: kODRecordTypeUsers, name: me, attributes: nil)
        try rec.verifyPassword(s as String)
        return .ok
    } catch let e as NSError {
        // napačno geslo le pri zavrnitvi poverilnic; zaklenjen račun, strežnik ipd. so »ni mogoče preveriti«
        let wrong = [kODErrorCredentialsInvalid, kODErrorCredentialsNotAuthorized].map { Int($0.rawValue) }
        return e.domain == ODFrameworkErrorDomain && wrong.contains(e.code) ? .wrong : .unavailable(e.localizedDescription)
    }
}

/// Jasno sporočilo za napako Keychaina pri vnosu »admin«. ssh: namig za zaklenjen keychain v seji SSH.
private func keychainHint(_ st: OSStatus, ssh: Bool) -> String {
    let me = userName(getuid())
    if let owner = appOwner(), owner != getuid() || st == errSecNoSuchKeychain {
        return "run minigw gw as the user who runs Mini Gateway (\(userName(owner))), not as \(me)"
    }
    switch st {
    case errSecNoSuchKeychain: return "run minigw gw as the user who runs Mini Gateway, not as \(me) (\(me) has no login keychain)"
    case errSecItemNotFound: return "start Mini Gateway 0.5.15 or newer once, so it can create the admin item"
    case errSecAuthFailed, errSecUserCanceled, errSecInteractionNotAllowed:  // brez dialoga (main.swift)
        // minigw sam nikoli ne kliče ukaza security; nasvet zanj le pri zaklenjenem keychainu (#62).
        return Keychain.deniedHint(ssh: ssh, locked: loginLocked())
    default: return "cannot read Mini Gateway's admin item from the Keychain (OSStatus \(st))"
    }
}

private func userName(_ uid: uid_t) -> String { getpwuid(uid).flatMap { String(cString: $0.pointee.pw_name) } ?? "uid \(uid)" }

/// Lastnik tekočega procesa MiniGateway (sysctl KERN_PROC_PID: bere tudi procese drugih uporabnikov).
private func appOwner() -> uid_t? {
    var pids = [pid_t](repeating: 0, count: 8192)
    let n = Int(proc_listallpids(&pids, Int32(pids.count * MemoryLayout<pid_t>.size)))
    for pid in pids.prefix(max(0, n)) where pid > 0 {
        var info = kinfo_proc(), size = MemoryLayout<kinfo_proc>.stride
        var mib: [Int32] = [CTL_KERN, KERN_PROC, KERN_PROC_PID, pid]
        guard sysctl(&mib, 4, &info, &size, nil, 0) == 0, size > 0 else { continue }
        let name = withUnsafeBytes(of: info.kp_proc.p_comm) { String(decoding: $0.prefix { $0 != 0 }, as: UTF8.self) }
        if name == "MiniGateway" { return info.kp_eproc.e_ucred.cr_uid }
    }
    return nil
}

/// Ključ s stdin (cev) ali s terminala brez odmeva; nikoli iz argv. Le natisljivi ASCII (gre v glavo HTTP).
/// what: kaj se bere (»API key for openrouter«, »router token«).
func readKey(_ what: String) -> String {
    var line = ""
    if isatty(0) == 1 {
        var buf = [CChar](repeating: 0, count: 8192)
        guard readpassphrase("\(what): ", &buf, buf.count, RPP_REQUIRE_TTY) != nil else { fail("no \(what)") }
        line = String(cString: buf)
    } else {
        line = String(decoding: FileHandle.standardInput.readData(ofLength: 8192), as: UTF8.self)
    }
    let key = line.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !key.isEmpty else { fail("no \(what) (pipe it on stdin or type it at the prompt)") }
    guard key.unicodeScalars.allSatisfy({ (0x21...0x7e).contains($0.value) }) else { fail("the \(what) must be printable ASCII without spaces") }
    return key
}

private func modelLine(_ m: [String: Any]) -> String {
    let w = (m["window"] as? Int).map { "  \($0 / 1000)K" } ?? ""
    return clean("\((m["enabled"] as? Bool ?? false) ? "[x]" : "[ ]") \(m["id"] as? String ?? "")  (/model \(m["model"] as? String ?? ""))\(w)")
}

private func enabledCount(_ models: [[String: Any]]) -> Int { models.filter { $0["enabled"] as? Bool == true }.count }

private func esc(_ s: String) -> String { s.addingPercentEncoding(withAllowedCharacters: .alphanumerics.union(CharacterSet(charactersIn: "-._~"))) ?? s }

/// Open at Login za gw status in gw autostart (kot autostartLine v Go).
private func autostartLine(_ st: [String: Any]) -> String {
    st["installed"] as? Bool == true ? "starts at sign-in (Settings → General → Open at Login)" : "does not start at sign-in (Settings → General → Open at Login)"
}

/// #60: minigw gw codex|grok|copilot [--work=…]: omejitev načina agenta prek skrbniškega API (kot agentWork v Go).
private func agentWork(_ a: AdminConnection, _ kind: String, _ work: String?) {
    let c = work.map { a.call("PATCH", "/agents/" + kind, ["work": $0]) } ?? a.call("GET", "/agents/" + kind)
    say(clean("\(["codex": "Codex", "grok": "Grok", "copilot": "Copilot"][kind] ?? kind) work: \(c["work"] as? String ?? "")"))
}

func routerCommand(_ argv: [String]) {
    // Ukazi in zastavice v poljubnem vrstnem redu (»--x v« ali »--x=v«).
    var pos: [String] = [], flags: [String: String] = [:]
    let valued: Set<String> = ["name", "enable", "disable", "path", "tools", "folders", "allow-local-tools", "ask-others", "work", "folder", "allow-unsandboxed-bash", "config", "device", "days"]
    var i = 0
    while i < argv.count {
        let a = argv[i]
        // ID naprave (43 znakov base64url) se lahko začne z »--«: ni zastavica.
        if a.hasPrefix("--"), !(a.count == 43 && a.allSatisfy { $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "-" || $0 == "_") }) {
            let kv = a.dropFirst(2).split(separator: "=", maxSplits: 1).map(String.init)
            if kv.count == 2 { flags[kv[0]] = kv[1] } else if valued.contains(kv[0]), i + 1 < argv.count { flags[kv[0]] = argv[i + 1]; i += 1 } else { flags[kv[0]] = "true" }
        } else {
            pos.append(a)
        }
        i += 1
    }
    if pos.isEmpty || pos[0] == "help" || flags["help"] != nil { say(routerHelp); exit(0) }
    if flags["config"] != nil { fail("the Mac router keeps its settings in the Mini Gateway app (no --config)") }
    if pos.first == "claude" && flags["folder"] != nil || flags["allow-unsandboxed-bash"] != nil {
        fail("on the Mac use --folders (allowed folders) and --allow-local-tools (Bash runs in Claude Code's sandbox)")
    }
    if let w = flags["work"], !["direct", "sync", "worktree"].contains(w) { fail("--work: direct, sync or worktree") }
    if flags["work"] != nil, !["claude", "codex", "grok"].contains(pos[0]), pos != ["copilot"] {
        fail("--work only applies to minigw gw claude|codex|grok|copilot")
    }
    func need(_ min: Int, _ max: Int) { if pos.count < min || pos.count > max { fail(routerHelp) } }
    let a = AdminConnection.open()

    switch pos[0] {
    case "status":
        need(1, 1)
        let st = a.call("GET", "/status")
        say("gateway: running, changes apply at once")
        say("version \(st["version"] as? String ?? "?"), port \(st["port"] as? Int ?? 0) on \(st["address"] as? String ?? "?")")
        say(autostartLine(st))
        say("agent requests running: \(st["agents"] as? Int ?? 0)")
        say("devices: \(st["devices"] as? Int ?? 0) allowed, \(st["pending"] as? Int ?? 0) waiting (minigw gw devices)")
        let c = st["claude"] as? [String: Any] ?? [:]
        say(clean("claude: \(c["found"] as? Bool == true ? "found" : "not found"), tools \(c["tools"] as? String ?? "")"))
        if let why = st["copilot"] as? String { say(clean("Copilot is off: " + why)) }  // #81
        for g in st["gateways"] as? [[String: Any]] ?? [] {
            let n = (g["models"] as? [String]).map { String($0.count) } ?? "all"
            let ready = g["enabled"] as? Bool == false ? ", off" : g["ready"] as? Bool == true ? ", ready" : ", models not loaded (see the gateway log)"
            say(clean("provider \(g["name"] as? String ?? ""): \(g["baseURL"] as? String ?? ""), models enabled: \(n)\(ready)"))
        }

    case "devices":
        let q = Noise.quoted  // ime naprave iz omrežja: v narekovajih z ubežnimi znaki (kot %q v Go)
        if pos.count == 1 {
            let d = a.call("GET", "/devices")
            let list = d["devices"] as? [[String: Any]] ?? []
            for x in list {
                let client = (x["client"] as? String).map { " (" + q($0) + ")" } ?? ""
                say("\(x["id"] as? String ?? "")  \(q(x["name"] as? String ?? ""))\(client)")
            }
            if list.isEmpty {
                say(d["allowAnyDevice"] as? Bool == true ? "no devices listed: any device with the token is allowed (allowAnyDevice)" : "no devices allowed")
            }
            for x in d["pending"] as? [[String: Any]] ?? [] {
                let id = x["id"] as? String ?? "", client = x["client"] as? String ?? ""
                say("waiting: \(id)  \(q(x["name"] as? String ?? "")) from \(q(x["address"] as? String ?? ""))\(client.isEmpty ? "" : " (" + q(client) + ")") at \(x["time"] as? String ?? ""); compare the ID with the client (minigw status), then: minigw gw devices allow \(id) [--name N]")
            }
        } else if pos[1] == "allow", pos.count >= 3 {  // starejša oblika: allow <id> <ime>
            let r = a.call("POST", "/devices", ["id": pos[2], "name": flags["name"] ?? pos[3...].joined(separator: " ")])
            let d = r["device"] as? [String: Any] ?? [:]
            say((r["added"] as? Bool == true ? "allowed device " : "already allowed: ") + "\(d["id"] as? String ?? pos[2]) \(q(d["name"] as? String ?? ""))")
        } else if pos[1] == "rename", pos.count == 3, let name = flags["name"] {
            let r = a.call("PATCH", "/devices/" + esc(pos[2]), ["name": name])
            let d = r["device"] as? [String: Any] ?? [:]
            say("renamed device \(d["id"] as? String ?? "") to \(q(d["name"] as? String ?? ""))")
        } else if pos[1] == "history", pos.count == 3 {
            let r = a.call("GET", "/devices/" + esc(pos[2]) + "/history")
            say("\(r["id"] as? String ?? "")  \(q(r["name"] as? String ?? ""))")
            let h = r["history"] as? [[String: Any]] ?? []
            for x in h { say("\(x["time"] as? String ?? "")  \(q(x["address"] as? String ?? ""))  \(q(x["client"] as? String ?? ""))") }
            if h.isEmpty { say("no connections recorded") }
        } else if pos[1] == "deny", pos.count == 3 {
            // --name ipd.: brisanje ni namen (preimenuj: rename)
            guard flags.isEmpty else { fail("minigw gw devices deny takes no options (to rename a device: minigw gw devices rename <id> --name N)") }
            let r = a.call("DELETE", "/devices/" + esc(pos[2]))
            let d = r["device"] as? [String: Any] ?? [:]
            say("\(r["pending"] as? Bool == true ? "rejected waiting" : "removed") device \(d["id"] as? String ?? "") \(q(d["name"] as? String ?? ""))")
        } else {
            fail(routerHelp)
        }

    case "sessions":
        if pos.count == 1 {
            let query = "?device=" + esc(flags["device"] ?? "") + "&folder=" + esc(flags["folder"] ?? "")
            let list = a.call("GET", "/sessions" + query)["sessions"] as? [[String: Any]] ?? []
            list.forEach { say(sessionLine($0)) }
            if list.isEmpty { say("no sessions recorded") }
        } else if pos.count == 2, pos[1] == "clear" {
            _ = a.call("DELETE", "/sessions")
            say("session history cleared")
        } else if pos.count == 2 {
            let x = a.call("GET", "/sessions/" + esc(pos[1]))
            say(sessionLine(x))
            say("start \(x["start"] as? String ?? ""), last \(x["last"] as? String ?? ""), duration \(String(format: "%.0f", x["duration"] as? Double ?? 0))s")
            if let b = x["branch"] as? String, !b.isEmpty { say("branch " + Noise.quoted(b)) }
            if let r = x["role"] as? String, !r.isEmpty { say("role " + r) }
            for m in x["models"] as? [[String: Any]] ?? [] { say(useLine(m)) }
        } else {
            fail(routerHelp)
        }

    case "usage":
        guard pos.count == 1 else { fail(routerHelp) }
        let query = "?days=" + esc(flags["days"] ?? "7") + "&device=" + esc(flags["device"] ?? "") + "&folder=" + esc(flags["folder"] ?? "")
        let list = a.call("GET", "/usage" + query)["usage"] as? [[String: Any]] ?? []
        list.forEach { say(usageLine($0)) }
        if list.isEmpty { say("no usage recorded") }

    case "provider", "gateway":  // gateway: starejše ime, skrit alias
        guard pos.count >= 2 else { fail(routerHelp) }
        switch pos[1] {
        case "add":
            need(4, 4)
            let key = readKey("API key for \(pos[2])")
            let r = a.call("POST", "/gateways", ["name": pos[2], "baseURL": pos[3], "key": key, "insecure": flags["insecure"] == "true"])
            let models = r["models"] as? [[String: Any]] ?? []
            say("provider \(pos[2]): \(models.count) models, \(enabledCount(models)) enabled; API key stored in \(r["keyStore"] as? String ?? "the Keychain")")
            if !models.isEmpty, enabledCount(models) == 0 {
                say("enable models with: minigw gw provider models \(pos[2]) --enable <id>,<id> (find them with: minigw gw provider models \(pos[2]) <text>)")
            }
        case "remove":
            need(3, 3)
            _ = a.call("DELETE", "/gateways/" + esc(pos[2]))
            say("removed provider \(pos[2]) and its API key")
        case "list":
            need(2, 2)
            let list = a.call("GET", "/gateways")["gateways"] as? [[String: Any]] ?? []
            for g in list {
                let n = (g["models"] as? [String]).map { String($0.count) } ?? "all"
                say(clean("\(g["name"] as? String ?? "")  \(g["baseURL"] as? String ?? "")  models enabled: \(n)"))
            }
            if list.isEmpty { say("no providers (add one with: minigw gw provider add <name> <baseURL>)") }
        case "models":
            need(3, 4)
            if flags["enable"] != nil || flags["disable"] != nil {
                var body: [String: Any] = [:]
                for (flag, key, all) in [("enable", "enable", true), ("disable", "disable", false)] {
                    guard let v = flags[flag] else { continue }
                    if v == "all" { body["all"] = all } else { body[key] = v.replacingOccurrences(of: " ", with: "").split(separator: ",").map(String.init) }
                }
                let r = a.call("POST", "/gateways/" + esc(pos[2]) + "/models", body)
                for m in r["changed"] as? [[String: Any]] ?? [] { say(modelLine(m)) }
                let models = r["models"] as? [[String: Any]] ?? []
                say("\(enabledCount(models)) of \(models.count) enabled")
                return
            }
            let filter = pos.count == 4 ? pos[3] : ""
            let r = a.call("GET", "/gateways/" + esc(pos[2]) + "/models?filter=" + esc(filter))
            var hidden = 0  // OpenRouter: več sto modelov; brez izbire le vklopljeni
            for m in r["models"] as? [[String: Any]] ?? [] {
                if m["enabled"] as? Bool == true || flags["all"] != nil || !filter.isEmpty { say(modelLine(m)) } else { hidden += 1 }
            }
            if hidden > 0 { say("\(hidden) disabled models not shown: add --all, or a text to search (minigw gw provider models \(pos[2]) haiku)") }
        default:
            fail(routerHelp)
        }

    case "claude":
        need(1, 1)
        var body: [String: Any] = [:]
        if let v = flags["path"] { body["path"] = v }
        if let v = flags["tools"] { body["tools"] = v }
        if let v = flags["folders"] { body["folders"] = v }
        if let v = flags["allow-local-tools"] {
            guard v == "true" || v == "false" else { fail("--allow-local-tools: true or false") }
            body["allowLocalTools"] = v == "true"
        }
        if let v = flags["ask-others"] {
            guard v == "true" || v == "false" else { fail("--ask-others: true or false") }
            body["askOthers"] = v == "true"
        }
        if let v = flags["work"] { body["work"] = v }
        let c = body.isEmpty ? a.call("GET", "/agents/claude") : a.call("PATCH", "/agents/claude", body)
        let path = (c["path"] as? String).flatMap { $0.isEmpty ? nil : $0 } ?? "(from PATH)"
        say(clean("path: \(path), found: \(c["found"] as? Bool ?? false)\ntools: \(c["tools"] as? String ?? "")\nallowLocalTools: \(c["allowLocalTools"] as? Bool ?? false)\naskOthers: \(c["askOthers"] as? Bool ?? false)\nfolders: \(c["folders"] as? String ?? "")\nwork: \(c["work"] as? String ?? "")"))

    case "autostart":
        need(1, 2)
        if pos.count == 1 { say(autostartLine(a.call("GET", "/status"))); return }  // brez on/off: trenutno stanje
        guard ["on", "off"].contains(pos[1]) else { fail(routerHelp) }
        _ = a.call("PATCH", "/general", ["startAtSignIn": pos[1] == "on"])
        say(pos[1] == "on" ? "Mini Gateway opens at login" : "Mini Gateway no longer opens at login")

    case "token":
        need(1, 2)
        let g: [String: Any]
        if pos.count == 2 {
            guard pos[1] == "rotate" else { fail(routerHelp) }
            g = a.call("POST", "/general/token")
            say("New token. Each client must run the new line, or run minigw reconnect [--name P] with the new token; the old token no longer works:")
        } else {
            g = a.call("GET", "/general")
            say("On each client run:")
        }
        say("  " + (g["connect"] as? String ?? ""))

    case "codex", "grok":  // #60: omejitev načina (work); brez --work le izpis
        need(1, 1)
        agentWork(a, pos[0], flags["work"])

    case "copilot":  // #81: brez žetona (le copilot login); login: stanje prijave kot v Go
        if pos.count == 2, pos[1] == "login" {
            let login = a.call("GET", "/agents/copilot")["login"] as? [String: Any] ?? [:]
            say(clean("Copilot login: \(login["state"] as? String ?? "unknown")"))
            say(clean(login["hint"] as? String ?? ""))
            return
        }
        need(1, 1)
        agentWork(a, "copilot", flags["work"])

    case "api":  // minigw-tui: ena zahteva API; telo JSON s stdin (ključi nikoli v argv), odgovor JSON na stdout
        need(3, 3)
        guard AdminClient.apiRequestOK(pos[1], pos[2]) else { fail(routerHelp) }
        var body: Data?
        if pos[1] == "POST" || pos[1] == "PATCH" {
            do { body = try AdminClient.apiBody(FileHandle.standardInput.readData(ofLength: 1 << 20)) } catch { fail("minigw gw api: the body on stdin is not JSON") }
        }
        try? out.write(contentsOf: a.send(pos[1], pos[2], body) + Data("\n".utf8))

    case "settings":  // terminal UI (Go, minigw-tui): kliče nazaj ta minigw (gw api), skrivnosti ne vidi
        need(1, 1)
        let me = Bundle.main.executableURL?.resolvingSymlinksInPath().path ?? CommandLine.arguments[0]
        let dirs = [(me as NSString).deletingLastPathComponent, "/Applications/Mini Gateway.app/Contents/Helpers",
                    NSHomeDirectory() + "/Applications/Mini Gateway.app/Contents/Helpers"]
        guard let tui = dirs.map({ $0 + "/minigw-tui" }).first(where: FileManager.default.isExecutableFile) else {
            fail("the settings terminal UI (minigw-tui) is not installed: it comes with Mini Gateway 0.5.20 or newer (in Mini Gateway.app/Contents/Helpers); or use the app's Settings window (menu bar icon → Settings…)")
        }
        // Le podpisan pomočnik (ista ekipa, lasten identifikator): drug program na tem mestu ne dobi terminala in minigw.
        if let why = helperSignatureProblem(tui) { fail("not starting \(tui): \(why)") }
        let argv = [tui, "--minigw", me].map { strdup($0) } + [nil]
        execv(tui, argv)
        fail("cannot start \(tui): \(String(cString: strerror(errno)))")
    default:
        fail("unknown command minigw gw \(pos[0])\n\n" + routerHelp)
    }
}

/// Podpis minigw-tui: ekipa Y2M489RDV8 in identifikator dev.trojar.minigateway.tui (nil = v redu). Ta minigw brez
/// podpisa ekipe (razvojna gradnja iz swift build) preverjanje preskoči, z opozorilom.
func helperSignatureProblem(_ path: String) -> String? {
    let team = "anchor apple generic and certificate leaf[subject.OU] = \"Y2M489RDV8\""
    func valid(_ code: SecStaticCode, _ req: String) -> OSStatus {
        var r: SecRequirement?
        guard SecRequirementCreateWithString(req as CFString, [], &r) == errSecSuccess else { return errSecParam }
        return SecStaticCodeCheckValidity(code, [], r)
    }
    var me: SecCode?, meStatic: SecStaticCode?
    if SecCodeCopySelf([], &me) != errSecSuccess || me.map({ SecCodeCopyStaticCode($0, [], &meStatic) }) != errSecSuccess
        || meStatic.map({ valid($0, team) }) != errSecSuccess {
        try? err.write(contentsOf: Data("minigw: warning: this minigw is a development build (not signed by Mini Gateway): \(path) is started without a signature check\n".utf8))
        return nil
    }
    var code: SecStaticCode?
    guard SecStaticCodeCreateWithPath(URL(fileURLWithPath: path) as CFURL, [], &code) == errSecSuccess, let code else { return "cannot read its signature" }
    let st = valid(code, team + " and identifier \"dev.trojar.minigateway.tui\"")
    return st == errSecSuccess ? nil : "it is not signed by Mini Gateway (team Y2M489RDV8, identifier dev.trojar.minigateway.tui; OSStatus \(st))"
}

/// Vrstica seje za minigw gw sessions (enako kot sessionLine v Go).
func sessionLine(_ x: [String: Any]) -> String {
    var name = x["device"] == nil ? "this computer" : x["deviceName"] as? String ?? ""
    if x["deviceRemoved"] as? Bool == true { name += " (device removed)" }
    var mode = (x["mode"] as? String ?? "") + (x["readOnly"] as? Bool == true ? ", read-only" : "")
    if x["readOnly"] as? Bool == true, let cap = (x["models"] as? [[String: Any]])?.compactMap({ $0["cap"] as? String }).first(where: { !$0.isEmpty }) {
        mode += " (cap \(cap))"
    }
    var models: [String] = []  // L4: imena v narekovajih, v vrstnem redu vnosov (kot v Go)
    for m in x["models"] as? [[String: Any]] ?? [] {
        let q = Noise.quoted(m["model"] as? String ?? "")
        if !models.contains(q) { models.append(q) }
    }
    return "\(x["last"] as? String ?? "")  \(Noise.quoted(x["id"] as? String ?? ""))  \(Noise.quoted(name))  \(mode)  \(Noise.quoted(x["folder"] as? String ?? ""))  \(models.sorted().joined(separator: ", "))"
}

/// Vrstica modela seje za minigw gw sessions <id> (enako kot useLine v Go).
func useLine(_ m: [String: Any]) -> String {
    var label = Noise.quoted(m["model"] as? String ?? "")
    if let r = m["role"] as? String, !r.isEmpty { label += " role " + r }
    if let v = m["via"] as? String, !v.isEmpty { label += " via " + Noise.quoted(v) }
    if m["readOnly"] as? Bool == true { label += " read-only" + ((m["cap"] as? String).map { $0.isEmpty ? "" : " (cap \($0))" } ?? "") }
    if let t = m["tools"] as? String, !t.isEmpty { label += " tools " + t }
    let i = { (k: String) in m[k] as? Int ?? 0 }
    return "\(label): \(i("requests")) requests, \(i("errors")) errors, \(i("refused")) refused, \(String(format: "%.0f", m["seconds"] as? Double ?? 0))s, \(i("inputTokens")) input and \(i("outputTokens")) output tokens"
}

/// Vrstica števcev projekta za minigw gw usage (enako kot usageLine v Go).
func usageLine(_ p: [String: Any]) -> String {
    let i = { (k: String) in p[k] as? Int ?? 0 }
    let device = p["device"] as? String ?? "this computer"
    return "\(p["day"] as? String ?? "")  \(Noise.quoted(p["folder"] as? String ?? ""))  \(device)  \(p["agent"] as? String ?? ""): \(i("requests")) requests, "
        + "\(i("errors")) errors, \(i("refused")) refused, \(String(format: "%.1f", (p["seconds"] as? Double ?? 0) / 60)) agent minutes, "
        + "\(i("inputTokens")) input and \(i("outputTokens")) output tokens"
}
