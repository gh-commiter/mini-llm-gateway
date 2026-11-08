// minigw router: nastavitve aplikacije Mini Gateway na tem Macu prek skrbniškega API (docs/admin-api.md), z istimi
// ukazi in izpisom kot minigw router v Go. Nastavitve so v aplikaciji (UserDefaults, Keychain): brez tekoče
// aplikacije ni sprememb. Vrata, ključ routerja in skrivnost so v Keychainu aplikacije; po žici gre le dokaz MGW-Admin
// (HMAC nad rokovanjem Noise), zato lažen strežnik ali program na vratih ne dobi ničesar uporabnega (S1, M1).
import CryptoKit
import Foundation
import RouterCore
import Security

let routerHelp = """
minigw gw: show or change the Mini Gateway app on this Mac. Changes apply at once; the app must be running.

  minigw gw status
  minigw gw devices [allow <device id> [--name N] | rename <device id> --name N | deny <device id>]
  minigw gw devices history <device id>                 the device's last connections
  minigw gw sessions [--device ID] [--folder DIR] | <session id> | clear
  minigw gw provider add <name> <baseURL> [--insecure]   the API key is read from stdin or asked for
  minigw gw provider remove <name> | list
  minigw gw provider models <name> [text] [--all] [--enable m1,m2|all] [--disable m1,m2|all]
  minigw gw claude [--path FILE] [--tools T1,T2] [--folders DIR1,DIR2] [--allow-local-tools=true|false]
                  [--ask-others=true|false]   (may ask other models of this gateway; needs --allow-local-tools)
  minigw gw token [rotate]                              print the connect line; rotate makes a new token
  minigw gw copilot token [remove]                      Copilot's token for commands (fine-grained, only Copilot
                                                        Requests), read from stdin or asked for
  minigw gw autostart on|off                            Open at Login (Settings → General)
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
        let (v, st) = Keychain(loginOnly: true).lookup("admin")  // le prijavni keychain: drug ga ne zasenči
        guard st == errSecSuccess, let o = v.flatMap({ try? JSONSerialization.jsonObject(with: Data($0.utf8)) as? [String: Any] }),
              let secret = o["secret"] as? String, let port = (o["port"] as? Int).flatMap(UInt16.init(exactly:)),
              let key = (o["key"] as? String).flatMap(Noise.fromBase64url), key.count == 32 else {
            fail(keychainHint(st))
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
        let data = body.flatMap { try? JSONSerialization.data(withJSONObject: $0) }
        let (status, d) = raw(method, Router.adminPrefix + path, data, proof: true)
        let o = d.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] } ?? [:]
        guard status != 0 else { fail("lost the connection to Mini Gateway") }
        guard status < 300 else { fail(clean(((o["error"] as? [String: Any])?["message"] as? String) ?? "HTTP \(status)")) }
        return o
    }
}

/// Jasno sporočilo za napako Keychaina pri vnosu »admin«.
private func keychainHint(_ st: OSStatus) -> String {
    let me = userName(getuid())
    if let owner = appOwner(), owner != getuid() || st == errSecNoSuchKeychain {
        return "run minigw gw as the user who runs Mini Gateway (\(userName(owner))), not as \(me)"
    }
    switch st {
    case errSecNoSuchKeychain: return "run minigw gw as the user who runs Mini Gateway, not as \(me) (\(me) has no login keychain)"
    case errSecItemNotFound: return "start Mini Gateway 0.5.15 or newer once, so it can create the admin item"
    case errSecAuthFailed, errSecUserCanceled, errSecInteractionNotAllowed: return "allow minigw when macOS asks (Keychain)"
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

func routerCommand(_ argv: [String]) {
    // Ukazi in zastavice v poljubnem vrstnem redu (»--x v« ali »--x=v«).
    var pos: [String] = [], flags: [String: String] = [:]
    let valued: Set<String> = ["name", "enable", "disable", "path", "tools", "folders", "allow-local-tools", "ask-others", "folder", "allow-unsandboxed-bash", "config", "device"]
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
    func need(_ min: Int, _ max: Int) { if pos.count < min || pos.count > max { fail(routerHelp) } }
    let a = AdminConnection.open()

    switch pos[0] {
    case "status":
        need(1, 1)
        let st = a.call("GET", "/status")
        say("gateway: running, changes apply at once")
        say("version \(st["version"] as? String ?? "?"), port \(st["port"] as? Int ?? 0) on \(st["address"] as? String ?? "?")")
        say(st["installed"] as? Bool == true ? "starts at sign-in (Settings → General → Open at Login)" : "does not start at sign-in (Settings → General → Open at Login)")
        say("agent requests running: \(st["agents"] as? Int ?? 0)")
        say("devices: \(st["devices"] as? Int ?? 0) allowed, \(st["pending"] as? Int ?? 0) waiting (minigw gw devices)")
        let c = st["claude"] as? [String: Any] ?? [:]
        say(clean("claude: \(c["found"] as? Bool == true ? "found" : "not found"), tools \(c["tools"] as? String ?? "")"))
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
        let c = body.isEmpty ? a.call("GET", "/agents/claude") : a.call("PATCH", "/agents/claude", body)
        let path = (c["path"] as? String).flatMap { $0.isEmpty ? nil : $0 } ?? "(from PATH)"
        say(clean("path: \(path), found: \(c["found"] as? Bool ?? false)\ntools: \(c["tools"] as? String ?? "")\nallowLocalTools: \(c["allowLocalTools"] as? Bool ?? false)\naskOthers: \(c["askOthers"] as? Bool ?? false)\nfolders: \(c["folders"] as? String ?? "")"))

    case "autostart":
        need(2, 2)
        guard ["on", "off"].contains(pos[1]) else { fail(routerHelp) }
        _ = a.call("PATCH", "/general", ["startAtSignIn": pos[1] == "on"])
        say(pos[1] == "on" ? "Mini Gateway opens at login" : "Mini Gateway no longer opens at login")

    case "token":
        need(1, 2)
        let g: [String: Any]
        if pos.count == 2 {
            guard pos[1] == "rotate" else { fail(routerHelp) }
            g = a.call("POST", "/general/token")
            say("New token. Each client must run the new line; the old token no longer works:")
        } else {
            g = a.call("GET", "/general")
            say("On each client run:")
        }
        say("  " + (g["connect"] as? String ?? ""))

    case "copilot":
        need(2, 3)
        guard pos[1] == "token", pos.count == 2 || pos[2] == "remove" else { fail(routerHelp) }
        let token = pos.count == 3 ? "" : readKey("Copilot token (github_pat_…, only the Copilot Requests permission)")
        _ = a.call("PATCH", "/agents/copilot", ["token": token])
        say(token.isEmpty ? "removed Copilot's token" : "Copilot's token stored in the Keychain")

    case "settings":
        fail("on the Mac use the app's Settings window (menu bar icon → Settings…)")
    default:
        fail("unknown command minigw gw \(pos[0])\n\n" + routerHelp)
    }
}

/// Vrstica seje za minigw gw sessions (enako kot sessionLine v Go).
func sessionLine(_ x: [String: Any]) -> String {
    var name = x["device"] == nil ? "this computer" : x["deviceName"] as? String ?? ""
    if x["deviceRemoved"] as? Bool == true { name += " (device removed)" }
    let mode = (x["mode"] as? String ?? "") + (x["readOnly"] as? Bool == true ? ", read-only" : "")
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
    if m["readOnly"] as? Bool == true { label += " read-only" }
    if let t = m["tools"] as? String, !t.isEmpty { label += " tools " + t }
    let i = { (k: String) in m[k] as? Int ?? 0 }
    return "\(label): \(i("requests")) requests, \(i("errors")) errors, \(i("refused")) refused, \(String(format: "%.0f", m["seconds"] as? Double ?? 0))s, \(i("inputTokens")) input and \(i("outputTokens")) output tokens"
}
