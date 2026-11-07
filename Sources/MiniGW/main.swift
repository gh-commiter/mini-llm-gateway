// minigw — zažene Claude Code prek Mini Gateway.
// Podpisan pomožni program v Mini Gateway.app (Contents/Helpers); Install v PATH naredi povezavo nanj.
import CryptoKit
import Foundation
import RouterCore
import Security

let service = "dev.trojar.minigateway.cli"
let out = FileHandle.standardOutput, err = FileHandle.standardError
func say(_ s: String) { out.write(Data((s + "\n").utf8)) }
/// Besedilo routerja brez kontrolnih znakov (razen \n), da ne krmili terminala.
func clean(_ s: String) -> String {
    String(String.UnicodeScalarView(s.unicodeScalars.filter { $0 == "\n" || !($0.value < 0x20 || (0x7f...0x9f).contains($0.value)) }))
}
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
    guard let k = c.key.flatMap(Noise.fromBase64url), let p = NoiseProxy(routerURL: c.url, routerKey: k, device: device, suite: .aesGCM) else {  // MGW2: tudi router minigw serve
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
    guard URL(string: c.url + path) != nil else { fail("invalid router URL \(c.url)") }
    let (s, d) = call(c, "GET", path, timeout: 5)
    return (s, d.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] })
}

/// Največji odgovor routerja v pomnilniku (datoteka do 64 MB + glave); router ni zaupanja vreden.
let maxReply = 80 << 20

/// Poljubna zahteva; telo odgovora kot Data (0 = ni povezave). Telo gre najprej v začasno datoteko, ne v pomnilnik.
func call(_ c: Connection, _ method: String, _ path: String, _ headers: [String: String] = [:], _ body: Data? = nil,
          timeout: TimeInterval) -> (Int, Data?) {
    var r = URLRequest(url: URL(string: c.url + path)!, timeoutInterval: timeout)
    r.httpMethod = method
    r.httpBody = body
    r.setValue("Bearer \(c.token)", forHTTPHeaderField: "authorization")
    for (k, v) in headers { r.setValue(v, forHTTPHeaderField: k) }
    let done = DispatchSemaphore(value: 0)
    var result: (Int, Data?) = (0, nil)
    URLSession.shared.downloadTask(with: r) { file, resp, _ in
        let size = file.flatMap { try? $0.resourceValues(forKeys: [.fileSizeKey]).fileSize } ?? 0
        result = ((resp as? HTTPURLResponse)?.statusCode ?? 0, size <= maxReply ? file.flatMap { try? Data(contentsOf: $0) } : nil)
        done.signal()
    }.resume()
    done.wait()
    return result
}

/// Trenutna mapa za claude -p na Mac miniju: router jo priklopi (mount_webdav), minigw streže datoteke.
/// Mac mini odjemalca ne doseže, zato vse teče po povezavah, ki jih odpre minigw (long poll).
/// --minigw-work=sync: namesto priklopa router dobi kopijo (Sync); pred agentom jo minigw posodobi, po njem prevzame spremembe.
final class ShareClient: @unchecked Sendable {
    let c: Connection, session: String, root: URL, readOnly: Bool, sync: Bool
    static let writeMethods: Set<String> = ["PUT", "DELETE", "MKCOL", "MOVE", "COPY", "PROPPATCH", "LOCK"]
    private let lock = NSLock()
    private var current = ""
    var key: String { lock.withLock { current } }
    private let syncLock = NSLock()
    private var synced: [String: Sync.Entry] = [:]  // manifest ob zadnjem prenosu (zgoščevanje, spori)
    private var reported: Set<String> = []          // že javljene prevelike datoteke
    private var unpulled = false                    // spremembe agenta, ki niso bile prevzete in potrjene

    init(_ c: Connection, session: String, root: URL, readOnly: Bool, sync: Bool) {
        self.c = c; self.session = session; self.root = root; self.readOnly = readOnly; self.sync = sync
    }

    func register() -> Bool {
        let (s, d) = call(c, "POST", "/minigw/share",
                          ["x-claude-mini-session": session, "x-claude-mini-cwd": root.path, "x-share-key": key,
                           "x-share-mode": readOnly ? "ro" : "rw", "x-claude-mini-work": sync ? "sync" : "direct"], timeout: 10)
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
                // Router ni zaupanja vreden: Sync.jobAllowed (SYNC-* le v sync, PULL ne pri --minigw-ro, WebDAV le v direct).
                let r = !Sync.jobAllowed(job.method, sync: sync, readOnly: readOnly, write: Self.writeMethods.contains(job.method))
                    ? DAVResponse(403)
                    : job.method == "SYNC-PUSH" ? DAVResponse(syncLock.withLock { push(k) } ? 200 : 500)
                    : job.method == "SYNC-PULL" ? DAVResponse(syncLock.withLock { pull(k) } ? 200 : 500)
                    : WebDAV.handle(method: job.method, path: job.path, headers: job.headers, body: job.body, root: root, prefix: job.prefix)
                guard let body = try? JSONEncoder().encode(DAVReply(id: job.id, response: r)) else { continue }
                _ = call(c, "POST", "/minigw/share/reply", ["x-share-key": k, "content-type": "application/json"], body, timeout: 120)
            case 204:
                continue
            case 404:
                if sync, syncLock.withLock({ unpulled }) {  // nova registracija: router ne pozna več nepotrjenih sprememb
                    note("the router restarted; the agent's changes that were not copied back may be overwritten by the next sync")
                }
                _ = register()  // router se je znova zagnal
                sleep(1)
            default:
                sleep(2)        // router nedosegljiv: poskusi znova
            }
        }
    }

    private func note(_ s: String) { err.write(Data(("minigw: " + clean(s) + "\n").utf8)) }

    /// Pred agentom: manifest k routerju, nato datoteke, ki jih potrebuje.
    private func push(_ k: String) -> Bool {
        let (files, skipped) = Sync.scan(root, previous: synced)
        for p in skipped where reported.insert(p).inserted { note("not synced (larger than 64 MB): \(p)") }
        guard let body = try? JSONEncoder().encode(Array(files.values)) else { return false }
        let (s, d) = call(c, "POST", "/minigw/sync/manifest", ["x-share-key": k, "content-type": "application/json"], body, timeout: 300)
        let reply = s == 200 ? d.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] } : nil
        guard let need = reply?["need"] as? [String] else { note("sync to the router failed (HTTP \(s))"); return false }
        // Neuspešen prenos in »kept« (router je obdržal agentovo različico): stanje poti ostane staro, da prevzem zazna spor.
        var next = files
        for p in reply?["kept"] as? [String] ?? [] {
            next[p] = synced[p]
            note("the agent's change to \(p) is not copied back yet; kept on the router")
        }
        for p in need {
            guard files[p] != nil, let url = Sync.resolve(p, root: root), let data = try? Data(contentsOf: url) else { next[p] = synced[p]; continue }
            let (st, _) = call(c, "PUT", "/minigw/sync/file", ["x-share-key": k, "x-sync-path": Sync.encode(p)], data, timeout: 300)
            if st != 204 {
                next[p] = synced[p]
                note(st == 409 ? "\(p) changed during sync; it goes with the next request" : "cannot sync \(p) (HTTP \(st))")
            }
        }
        synced = next
        return true
    }

    /// Po agentu: spremembe kopije v mapo (spori → <ime>.minigw-conflict).
    private func pull(_ k: String) -> Bool {
        let (s, d) = call(c, "GET", "/minigw/sync/changes", ["x-share-key": k], timeout: 300)
        guard s == 200, let d, let ch = try? JSONDecoder().decode(Sync.Changes.self, from: d) else {
            note("cannot get the agent's changes from the router (HTTP \(s))"); unpulled = true; return false
        }
        let (notes, done) = Sync.apply(ch, root: root, state: &synced) { p in
            let (st, data) = call(c, "GET", "/minigw/sync/file", ["x-share-key": k, "x-sync-path": Sync.encode(p)], timeout: 300)
            return st == 200 ? data : nil
        }
        notes.forEach(note)
        // Potrditev: router šele zdaj šteje te spremembe za prevzete; ostale pošlje znova.
        unpulled = done.count < ch.changed.count + ch.deleted.count
        if !done.isEmpty, let body = try? JSONEncoder().encode(done),
           call(c, "POST", "/minigw/sync/ack", ["x-share-key": k, "content-type": "application/json"], body, timeout: 60).0 != 204 {
            note("the router did not confirm the copied-back changes; they come again next time")
            unpulled = true
        }
        if !ch.changed.isEmpty || !ch.deleted.isEmpty { note("synced back from the router: \(ch.changed.count) changed, \(ch.deleted.count) deleted") }
        return true
    }
}

func requireConnection() -> Connection {
    guard let c = loadConnection() else { fail("not connected; run: minigw connect <host[:port]> --token <token>") }
    return c
}

// MARK: deljene mape

/// Mape zunaj domače, ki jih uporabnik deli z »minigw share add« (enako kot odjemalec Go na macOS).
let sharedFoldersFile = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
    .appendingPathComponent("minigw/shared-folders")

func sharedFolders() -> [String] {
    ((try? String(contentsOf: sharedFoldersFile, encoding: .utf8)) ?? "").split(separator: "\n")
        .map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
}

/// Projektna mapa kjer koli (tudi /Volumes/Disk/projekt), razen korena diska ali priklopa, domače mape same,
/// sistemskih map, domačih map drugih ter skritih map in ~/Library.
func autoShare(_ path: String) -> Bool {
    let home = NSHomeDirectory()
    let p = URL(fileURLWithPath: path).standardizedFileURL.path
    guard p != "/", p != home else { return false }
    if p.hasPrefix(home + "/") {
        let top = String(p.dropFirst(home.count + 1).split(separator: "/").first ?? "")
        return !top.hasPrefix(".") && top != "Library"
    }
    let parts = p.split(separator: "/").map { $0.lowercased() }
    let system: Set<String> = ["system", "library", "usr", "bin", "sbin", "etc", "private", "var", "dev", "opt", "applications",
                               "users", "home", "root", "proc", "sys", "boot", "tmp", "run", "snap", "nix", "cores"]
    guard let top = parts.first, !top.hasPrefix("."), !system.contains(top) else { return false }
    return !["volumes", "mnt", "media"].contains(top) || parts.count >= 3
}

func shareCommand(_ a: [String]) -> Never {
    let sub = a.first ?? "list"
    let dir = URL(fileURLWithPath: a.count > 1 ? a[1] : FileManager.default.currentDirectoryPath).standardizedFileURL.path
    var list = sharedFolders()
    func save() {
        try? FileManager.default.createDirectory(at: sharedFoldersFile.deletingLastPathComponent(), withIntermediateDirectories: true)
        guard (try? (list.joined(separator: "\n") + "\n").write(to: sharedFoldersFile, atomically: true, encoding: .utf8)) != nil
        else { fail("cannot save \(sharedFoldersFile.path)") }
    }
    switch sub {
    case "add":
        var isDir: ObjCBool = false
        guard FileManager.default.fileExists(atPath: dir, isDirectory: &isDir), isDir.boolValue else { fail("not a folder: \(dir)") }
        guard dir != "/", dir != NSHomeDirectory() else {
            fail("refusing to share \(dir) (the disk root or your home folder would expose everything); share a project folder")
        }
        if list.contains(dir) { print("already shared: \(dir)"); exit(0) }
        list.append(dir); save()
        print("shared with the router from now on: \(dir) (and its subfolders)")
    case "remove":
        list.removeAll { $0 == dir }; save()
        print("no longer shared: \(dir)")
    case "list":
        if list.isEmpty { print("no extra shared folders (folders under your home folder are shared by default)") }
        list.forEach { print($0) }
    default:
        fail("usage: minigw share [add|remove] [folder] | minigw share list")
    }
    exit(0)
}

// MARK: ukazi

let minigwVersion = "0.5.13"  // bump-version.sh
var args = Array(CommandLine.arguments.dropFirst())
// --minigw-ro (ali MINIGW_SHARE=ro): mapa samo za branje; zastavica ne gre naprej v claude.
let readOnly = args.contains("--minigw-ro") || ProcessInfo.processInfo.environment["MINIGW_SHARE"] == "ro"
args.removeAll { $0 == "--minigw-ro" }
// --minigw-work=sync (ali MINIGW_WORK=sync): agenti delajo v kopiji na routerju namesto v priklopu; privzeto direct.
var work = ProcessInfo.processInfo.environment["MINIGW_WORK"] ?? "direct"
for a in args where a.hasPrefix("--minigw-work=") { work = String(a.dropFirst("--minigw-work=".count)) }
args.removeAll { $0.hasPrefix("--minigw-work=") }
guard ["direct", "sync"].contains(work) else { fail("unknown --minigw-work=\(work) (use direct or sync)") }

switch args.first {
case "share":
    shareCommand(Array(args.dropFirst()))
case "connect":
    guard args.count >= 2 else { fail("usage: minigw connect <host[:port]> [--token <token>]") }
    let saved = loadConnection()
    var token = saved.map { $0.token + ($0.key.map { "#" + $0 } ?? "") } ?? ""
    if let i = args.firstIndex(of: "--token"), i + 1 < args.count { token = args[i + 1] }
    guard !token.isEmpty else { fail("missing --token (copy it from Mini Gateway → Settings → General → Copy Token)") }
    let parts = token.split(separator: "#", maxSplits: 1).map(String.init)
    let c = Connection(url: normalize(args[1]), token: parts[0], key: parts.count > 1 ? parts[1] : nil, device: saved?.device ?? Noise.base64url(Curve25519.Signing.PrivateKey().rawRepresentation))
    let (status, body) = get(route(c), "/v1/models")
    switch status {
    case 200: saveConnection(c); say("Connected to \(c.url)" + (c.isLocal ? "" : " (encrypted)"))
    case 401: fail("router at \(c.url) rejected the token")
    case 403:  // naprava še ni dovoljena (minigw serve): ključ naprave ostane, da ga router lahko doda
        saveConnection(c)
        let msg = (body?["error"] as? [String: Any])?["message"] as? String ?? ""
        fail("router at \(c.url) refused this device: \(clean(msg))\nthen run minigw connect again")
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
        minigw share add|remove [folder] | list    also share a system or hidden folder with the router
        minigw --minigw-ro [claude args]           share the current folder read-only (or MINIGW_SHARE=ro)
        minigw --minigw-work=sync [claude args]    agents work in a synced copy on the router (or MINIGW_WORK=sync)
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
    case "-r", "--resume":  // brez ID-ja = izbirnik sej v claude; nikoli skupaj z --session-id
        own = true
        if i + 1 < args.count, !args[i + 1].hasPrefix("-") { sid = args[i + 1] }
    case let a where a.hasPrefix("--resume="): own = true; sid = String(a.dropFirst("--resume=".count))
    case "--session-id": if i + 1 < args.count { sid = args[i + 1] }; own = true
    case "-c", "--continue": own = true
    default: break
    }
}
if sid.isEmpty {
    sid = UUID().uuidString.lowercased()  // tudi pri -c/--continue: router po njem deli mapo
    if !own { args = ["--session-id", sid] + args }
}

let argv = (["claude"] + args).map { strdup($0) } + [nil]
let cwd = FileManager.default.currentDirectoryPath
let routerHost = URLComponents(string: saved.url)?.host ?? ""
// Mapo deli le z oddaljenim routerjem (pravilo v autoShare, enako kot odjemalec Go); MINIGW_SHARE=0/1 preglasi.
let shareEnv = ProcessInfo.processInfo.environment["MINIGW_SHARE"]
let remoteRouter = !["127.0.0.1", "localhost", "::1"].contains(routerHost)
let wantShare = shareEnv == "1"
    || (shareEnv != "0" && remoteRouter
        && (autoShare(cwd) || sharedFolders().contains { cwd == $0 || cwd.hasPrefix($0 + "/") }))

// x-claude-mini-share: router po ponovnem zagonu počaka, da se mapa znova registrira (namesto napake cwd_unavailable).
setenv("ANTHROPIC_CUSTOM_HEADERS", "x-claude-mini-session: \(sid)\nx-claude-mini-cwd: \(cwd)" + (wantShare ? "\nx-claude-mini-share: 1" : ""), 1)

// Brez proxyja in brez mape: minigw ni več potreben → exec.
guard proxy != nil || (wantShare && !sid.isEmpty) else {
    execvp("claude", argv)
    fail("cannot run claude: \(String(cString: strerror(errno))) (is Claude Code installed and in PATH?)", code: 127)
}

let share = ShareClient(c, session: sid, root: URL(fileURLWithPath: cwd), readOnly: readOnly, sync: work == "sync")
if !wantShare && remoteRouter && shareEnv != "0" {
    err.write(Data("minigw: \(cwd) is not shared with the router, so agent models (Codex, Claude CLI, Grok) cannot see it; to share it run: minigw share add\n".utf8))
}
if !wantShare || sid.isEmpty {
    // le šifrirani proxy
} else if share.register() {
    share.start()
    err.write(Data("minigw: \(work == "sync" ? "syncing" : "sharing") \(cwd) \(readOnly ? "read-only" : "read-write") with \(routerHost) for agent models (--minigw-ro = read-only, MINIGW_SHARE=0 = off)\n".utf8))
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
