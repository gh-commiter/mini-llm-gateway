// minigw — zažene Claude Code prek Mini Gateway.
// Podpisan pomožni program v Mini Gateway.app (Contents/Helpers); Install v PATH naredi povezavo nanj.
import CryptoKit
import Foundation
import RouterCore
import Security

let service = "dev.trojar.minigateway.cli"
let out = FileHandle.standardOutput, err = FileHandle.standardError
func say(_ s: String) { out.write(Data((s + "\n").utf8)) }
func fail(_ s: String, code: Int32 = 1) -> Never { err.write(Data(("minigw: " + s + "\n").utf8)); exit(code) }

// MARK: shranjena povezava (Keychain ukaza; ne vsebuje je nobena datoteka)

struct Connection: Codable {
    var url: String
    var token: String
    var key: String?  // javni ključ routerja (Noise); niz »Copy Token« = <žeton>#<ključ>
    var device: String?  // zasebni ključ te naprave (Ed25519, base64url); router po njem dovoli Siri

    var isLocal: Bool { ["127.0.0.1", "localhost", "::1"].contains(URLComponents(string: url)?.host ?? "") }
}

/// Oddaljen router: vse (Claude Code, minigw, mapa) gre šifrirano prek lokalnega proxyja; lokalno navaden HTTP.
var proxy: NoiseProxy?
func route(_ c: Connection) -> Connection {
    guard !c.isLocal else { return c }
    var c = c
    if c.device == nil {  // povezava iz minigw 0.4.3 ali starejšega: ključ naprave ustvari zdaj
        c.device = Noise.base64url(Curve25519.Signing.PrivateKey().rawRepresentation)
        saveConnection(c)
    }
    let device = c.device.flatMap(Noise.fromBase64url).flatMap { try? Curve25519.Signing.PrivateKey(rawRepresentation: $0) }
        .map { (key: $0, name: Host.current().localizedName ?? ProcessInfo.processInfo.hostName) }
    guard let k = c.key.flatMap(Noise.fromBase64url), let p = NoiseProxy(routerURL: c.url, routerKey: k, device: device) else {
        fail("no router key: copy the token again (Mini Gateway 0.4+ → Copy Token) and run minigw connect")
    }
    do { try p.start() } catch { fail("cannot start the local encrypted proxy: \(error)") }
    proxy = p
    var r = c
    r.url = p.localURL
    return r
}

func keychainQuery() -> [String: Any] {
    [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: "router"]
}

func loadConnection() -> Connection? {
    var q = keychainQuery()
    q[kSecReturnData as String] = true
    q[kSecMatchLimit as String] = kSecMatchLimitOne
    var item: CFTypeRef?
    guard SecItemCopyMatching(q as CFDictionary, &item) == errSecSuccess, let d = item as? Data else { return nil }
    return try? JSONDecoder().decode(Connection.self, from: d)
}

func saveConnection(_ c: Connection) {
    let data = try! JSONEncoder().encode(c)
    if SecItemUpdate(keychainQuery() as CFDictionary, [kSecValueData as String: data] as CFDictionary) == errSecItemNotFound {
        var add = keychainQuery()
        add[kSecValueData as String] = data
        guard SecItemAdd(add as CFDictionary, nil) == errSecSuccess else { fail("cannot save to Keychain") }
    }
}

/// »192.168.1.20«, »192.168.1.20:8765«, »mini.local«, »http://…« → osnovni URL.
func normalize(_ s: String) -> String {
    var u = s.contains("://") ? s : "http://" + s
    if u.hasSuffix("/") { u.removeLast() }
    if let comps = URLComponents(string: u), comps.port == nil { u += ":8765" }
    return u
}

// MARK: HTTP do routerja

func get(_ c: Connection, _ path: String) -> (Int, [String: Any]?) {
    guard let url = URL(string: c.url + path) else { fail("invalid router URL \(c.url)") }
    var r = URLRequest(url: url, timeoutInterval: 5)
    r.setValue("Bearer \(c.token)", forHTTPHeaderField: "authorization")
    let done = DispatchSemaphore(value: 0)
    var result: (Int, [String: Any]?) = (0, nil)
    URLSession.shared.dataTask(with: r) { data, resp, _ in
        result = ((resp as? HTTPURLResponse)?.statusCode ?? 0,
                  data.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] })
        done.signal()
    }.resume()
    done.wait()
    return result
}

/// Poljubna zahteva; telo odgovora kot Data (0 = ni povezave).
func call(_ c: Connection, _ method: String, _ path: String, _ headers: [String: String] = [:], _ body: Data? = nil,
          timeout: TimeInterval) -> (Int, Data?) {
    var r = URLRequest(url: URL(string: c.url + path)!, timeoutInterval: timeout)
    r.httpMethod = method
    r.httpBody = body
    r.setValue("Bearer \(c.token)", forHTTPHeaderField: "authorization")
    for (k, v) in headers { r.setValue(v, forHTTPHeaderField: k) }
    let done = DispatchSemaphore(value: 0)
    var result: (Int, Data?) = (0, nil)
    URLSession.shared.dataTask(with: r) { data, resp, _ in
        result = ((resp as? HTTPURLResponse)?.statusCode ?? 0, data)
        done.signal()
    }.resume()
    done.wait()
    return result
}

/// Trenutna mapa za claude -p na Mac miniju: router jo priklopi (mount_webdav), minigw streže datoteke.
/// Mac mini odjemalca ne doseže, zato vse teče po povezavah, ki jih odpre minigw (long poll).
final class ShareClient: @unchecked Sendable {
    let c: Connection, session: String, root: URL
    private let lock = NSLock()
    private var current = ""
    var key: String { lock.withLock { current } }

    init(_ c: Connection, session: String, root: URL) { self.c = c; self.session = session; self.root = root }

    func register() -> Bool {
        let (s, d) = call(c, "POST", "/minigw/share",
                          ["x-claude-mini-session": session, "x-claude-mini-cwd": root.path, "x-share-key": key], timeout: 10)
        guard s == 200, let d, let k = (try? JSONSerialization.jsonObject(with: d) as? [String: Any])?["key"] as? String else { return false }
        lock.withLock { current = k }
        return true
    }

    func unregister() { guard !key.isEmpty else { return }; _ = call(c, "DELETE", "/minigw/share", ["x-share-key": key], timeout: 3) }

    func start(workers: Int = 4) {
        for _ in 0..<workers { Thread { self.work() }.start() }
    }

    private func work() {
        while true {
            let k = key
            let (s, d) = call(c, "GET", "/minigw/share/next", ["x-share-key": k], timeout: 40)
            switch s {
            case 200:
                guard let d, let job = try? JSONDecoder().decode(DAVJob.self, from: d) else { continue }
                let r = WebDAV.handle(method: job.method, path: job.path, headers: job.headers, body: job.body,
                                      root: root, prefix: job.prefix)
                guard let body = try? JSONEncoder().encode(DAVReply(id: job.id, response: r)) else { continue }
                _ = call(c, "POST", "/minigw/share/reply", ["x-share-key": k, "content-type": "application/json"], body, timeout: 120)
            case 204:
                continue
            case 404:
                _ = register()  // router se je znova zagnal
                sleep(1)
            default:
                sleep(2)        // router nedosegljiv: poskusi znova
            }
        }
    }
}

func requireConnection() -> Connection {
    guard let c = loadConnection() else { fail("not connected; run: minigw connect <host[:port]> --token <token>") }
    return c
}

// MARK: ukazi

let minigwVersion = "0.4.5"  // bump-version.sh
var args = Array(CommandLine.arguments.dropFirst())

switch args.first {
case "connect":
    guard args.count >= 2 else { fail("usage: minigw connect <host[:port]> [--token <token>]") }
    let saved = loadConnection()
    var token = saved.map { $0.token + ($0.key.map { "#" + $0 } ?? "") } ?? ""
    if let i = args.firstIndex(of: "--token"), i + 1 < args.count { token = args[i + 1] }
    guard !token.isEmpty else { fail("missing --token (copy it from Mini Gateway → Settings → General → Copy Token)") }
    let parts = token.split(separator: "#", maxSplits: 1).map(String.init)
    let c = Connection(url: normalize(args[1]), token: parts[0], key: parts.count > 1 ? parts[1] : nil, device: saved?.device ?? Noise.base64url(Curve25519.Signing.PrivateKey().rawRepresentation))
    let (status, _) = get(route(c), "/v1/models")
    switch status {
    case 200: saveConnection(c); say("Connected to \(c.url)" + (c.isLocal ? "" : " (encrypted)"))
    case 401: fail("router at \(c.url) rejected the token")
    case 426: fail("router at \(c.url) needs an encrypted connection: copy the token again (it includes the router key)")
    case 0: fail(c.isLocal ? "cannot reach \(c.url)" : "cannot reach \(c.url), or the router key does not match (copy the token again)")
    default: fail("router at \(c.url) answered HTTP \(status)")
    }
    exit(0)

case "disconnect":
    SecItemDelete(keychainQuery() as CFDictionary)
    say("Disconnected")
    exit(0)

case "status":
    let saved = requireConnection()
    let c = route(saved)
    let (s, ready) = get(c, "/health/ready")
    say("Router:    \(saved.url) (\(s == 200 ? "reachable" : s == 0 ? "unreachable" : "HTTP \(s)")\(saved.isLocal ? "" : ", encrypted"))")
    if let p = ready?["providers"] as? [String: Bool] {
        say("Providers: " + p.keys.sorted().map { "\($0) \(p[$0]! ? "✓" : "✗")" }.joined(separator: ", "))
    }
    let (ms, models) = get(c, "/v1/models")
    say("Models:    " + (ms == 200 ? "\((models?["data"] as? [Any])?.count ?? 0)" : ms == 401 ? "token rejected" : "–"))
    exit(s == 200 ? 0 : 1)

case "models":
    let c = route(requireConnection())
    let (s, models) = get(c, "/v1/models")
    guard s == 200, let data = models?["data"] as? [[String: Any]] else { fail(s == 401 ? "token rejected" : "cannot list models (HTTP \(s))") }
    let width = data.compactMap { ($0["id"] as? String)?.count }.max() ?? 0
    for m in data {
        let id = m["id"] as? String ?? "?"
        say(id.padding(toLength: width, withPad: " ", startingAt: 0) + "  " + (m["description"] as? String ?? ""))
    }
    exit(0)

case "--minigw-help", "-h", "--help", "-v", "--version":
    if args.first == "-v" || args.first == "--version" { say("minigw \(minigwVersion)") } else {
        say("""
        minigw connect <host[:port]> [--token T]   save router address and token (Keychain)
        minigw status | models | disconnect
        minigw [claude options …]                    run Claude Code through the router
        """)
    }
    guard args.first != "--minigw-help" else { exit(0) }
    // Le podatki o claude: brez routerja, seje in deljenja mape.
    let argv = (["claude"] + args).map { strdup($0) } + [nil]
    execvp("claude", argv)
    fail("cannot run claude: \(String(cString: strerror(errno)))", code: 127)

default:
    break
}

// MARK: Claude Code prek routerja

let saved = requireConnection()
let c = route(saved)
setenv("ANTHROPIC_BASE_URL", c.url, 1)
setenv("ANTHROPIC_AUTH_TOKEN", c.token, 1)
setenv("ANTHROPIC_API_KEY", "", 1)
setenv("CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "1", 1)
setenv("API_TIMEOUT_MS", "1800000", 1)
setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1", 1)

// Seja: router po njej loči seje claude -p in niti Siri. Obstoječa ob --resume <id>, sicer nova.
var sid = ""
var own = false
for (i, a) in args.enumerated() {
    switch a {
    case "-r", "--resume": if i + 1 < args.count { sid = args[i + 1] }
    case "--session-id": if i + 1 < args.count { sid = args[i + 1] }; own = true
    case "-c", "--continue": own = true
    default: break
    }
}
if sid.isEmpty && !own {
    sid = UUID().uuidString.lowercased()
    args = ["--session-id", sid] + args
}
setenv("ANTHROPIC_CUSTOM_HEADERS", "x-claude-mini-session: \(sid)\nx-claude-mini-cwd: \(FileManager.default.currentDirectoryPath)", 1)

let argv = (["claude"] + args).map { strdup($0) } + [nil]
let cwd = FileManager.default.currentDirectoryPath
let routerHost = URLComponents(string: saved.url)?.host ?? ""
// Mapo deli le z oddaljenim routerjem in le pod domačo mapo (nikoli cele domače mape); MINIGW_SHARE=0/1 preglasi.
let shareEnv = ProcessInfo.processInfo.environment["MINIGW_SHARE"]
// Brez ~/Library in skritih map v domači (.ssh, .gnupg …): tam le z MINIGW_SHARE=1.
let underHome = cwd.hasPrefix(NSHomeDirectory() + "/") ? String(cwd.dropFirst(NSHomeDirectory().count + 1)) : nil
let wantShare = shareEnv == "1"
    || (shareEnv != "0" && !["127.0.0.1", "localhost", "::1"].contains(routerHost)
        && underHome.map { !$0.hasPrefix(".") && $0 != "Library" && !$0.hasPrefix("Library/") } == true)

// Brez proxyja in brez mape: minigw ni več potreben → exec.
guard proxy != nil || (wantShare && !sid.isEmpty) else {
    execvp("claude", argv)
    fail("cannot run claude: \(String(cString: strerror(errno))) (is Claude Code installed and in PATH?)", code: 127)
}

let share = ShareClient(c, session: sid, root: URL(fileURLWithPath: cwd))
if !wantShare || sid.isEmpty {
    // le šifrirani proxy
} else if share.register() {
    share.start()
    err.write(Data("minigw: sharing \(cwd) with \(routerHost) for Claude CLI models with tools (MINIGW_SHARE=0 to turn off)\n".utf8))
} else {
    err.write(Data("minigw: router did not accept the folder; Claude CLI models with tools will not see it\n".utf8))
}

// claude kot podproces (minigw mora ostati za share); Ctrl+C dobi claude.
signal(SIGINT, SIG_IGN)
signal(SIGQUIT, SIG_IGN)
var attr: posix_spawnattr_t?
posix_spawnattr_init(&attr)
var defaults = sigset_t()
sigemptyset(&defaults); sigaddset(&defaults, SIGINT); sigaddset(&defaults, SIGQUIT)
posix_spawnattr_setsigdefault(&attr, &defaults)
posix_spawnattr_setflags(&attr, Int16(POSIX_SPAWN_SETSIGDEF))
var pid: pid_t = 0
let rc = posix_spawnp(&pid, "claude", nil, &attr, argv, environ)
guard rc == 0 else {
    share.unregister()
    fail("cannot run claude: \(String(cString: strerror(rc))) (is Claude Code installed and in PATH?)", code: 127)
}
var status: Int32 = 0
while waitpid(pid, &status, 0) == -1 && errno == EINTR {}
share.unregister()
exit(status & 0x7f == 0 ? (status >> 8) & 0xff : 128 + (status & 0x7f))
