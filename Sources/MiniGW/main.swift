// minigw — zažene Claude Code prek Mini Gateway.
// Podpisan pomožni program v Mini Gateway.app (Contents/Helpers); Install v PATH naredi povezavo nanj.
import Foundation
import Security

let service = "dev.trojar.minigateway.cli"
let out = FileHandle.standardOutput, err = FileHandle.standardError
func say(_ s: String) { out.write(Data((s + "\n").utf8)) }
func fail(_ s: String, code: Int32 = 1) -> Never { err.write(Data(("minigw: " + s + "\n").utf8)); exit(code) }

// MARK: shranjena povezava (Keychain ukaza; ne vsebuje je nobena datoteka)

struct Connection: Codable {
    var url: String
    var token: String
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

func requireConnection() -> Connection {
    guard let c = loadConnection() else { fail("not connected; run: minigw connect <host[:port]> --token <token>") }
    return c
}

// MARK: ukazi

var args = Array(CommandLine.arguments.dropFirst())

switch args.first {
case "connect":
    guard args.count >= 2 else { fail("usage: minigw connect <host[:port]> [--token <token>]") }
    var token = loadConnection()?.token ?? ""
    if let i = args.firstIndex(of: "--token"), i + 1 < args.count { token = args[i + 1] }
    guard !token.isEmpty else { fail("missing --token (copy it from Mini Gateway → Settings → General → Copy Token)") }
    let c = Connection(url: normalize(args[1]), token: token)
    let (status, _) = get(c, "/v1/models")
    switch status {
    case 200: saveConnection(c); say("Connected to \(c.url)")
    case 401: fail("router at \(c.url) rejected the token")
    case 0: fail("cannot reach \(c.url)")
    default: fail("router at \(c.url) answered HTTP \(status)")
    }
    exit(0)

case "disconnect":
    SecItemDelete(keychainQuery() as CFDictionary)
    say("Disconnected")
    exit(0)

case "status":
    let c = requireConnection()
    let (s, ready) = get(c, "/health/ready")
    say("Router:    \(c.url) (\(s == 200 ? "reachable" : s == 0 ? "unreachable" : "HTTP \(s)"))")
    if let p = ready?["providers"] as? [String: Bool] {
        say("Providers: " + p.keys.sorted().map { "\($0) \(p[$0]! ? "✓" : "✗")" }.joined(separator: ", "))
    }
    let (ms, models) = get(c, "/v1/models")
    say("Models:    " + (ms == 200 ? "\((models?["data"] as? [Any])?.count ?? 0)" : ms == 401 ? "token rejected" : "–"))
    exit(s == 200 ? 0 : 1)

case "models":
    let c = requireConnection()
    let (s, models) = get(c, "/v1/models")
    guard s == 200, let data = models?["data"] as? [[String: Any]] else { fail(s == 401 ? "token rejected" : "cannot list models (HTTP \(s))") }
    let width = data.compactMap { ($0["id"] as? String)?.count }.max() ?? 0
    for m in data {
        let id = m["id"] as? String ?? "?"
        say(id.padding(toLength: width, withPad: " ", startingAt: 0) + "  " + (m["description"] as? String ?? ""))
    }
    exit(0)

case "--minigw-help":
    say("""
    minigw connect <host[:port]> [--token T]   save router address and token (Keychain)
    minigw status | models | disconnect
    minigw [claude options …]                    run Claude Code through the router
    """)
    exit(0)

default:
    break
}

// MARK: Claude Code prek routerja

let c = requireConnection()
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
execvp("claude", argv)
fail("cannot run claude: \(String(cString: strerror(errno))) (is Claude Code installed and in PATH?)", code: 127)
