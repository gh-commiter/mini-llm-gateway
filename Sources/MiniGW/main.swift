// minigw — zažene Claude Code prek Mini Gateway.
// Podpisan pomožni program v Mini Gateway.app (Contents/Helpers); Install v PATH naredi povezavo nanj.
import CryptoKit
import Foundation
import RouterCore
import Security

let service = "dev.trojar.minigateway.cli"
// Keychain brez dialogov: vnos, ki minigw ne zaupa (drug podpis), vrne errSecInteractionNotAllowed in jasno napako.
SecKeychainSetUserInteractionAllowed(false)
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

/// Ime naprave za odobritev na prehodu: <uporabnik>@<ime računalnika> (Sharing → Computer Name), da se dva uporabnika
/// istega Maca razlikujeta. Prehod ga očisti (Noise.cleanName); je le oznaka.
func deviceName() -> String {
    "\(NSUserName())@\(Host.current().localizedName ?? ProcessInfo.processInfo.hostName)"
}

/// OS in različica za vrstico čakajoče naprave (glava x-minigw-client; starejši prehodi jo prezrejo). Izračunana:
/// globalne spremenljivke v main.swift se nastavijo po vrsti, minigwVersion pa je niže (sicer sesutje ob zagonu).
/// Le ASCII: URLSession pošlje druge znake v glavi kot Latin-1, ki ga prehod (UTF-8) zavrne (»Bad request header«).
var clientInfo: String { "macOS, minigw \(minigwVersion)" }

/// Oddaljen prehod: vse (Claude Code, minigw, mapa) gre šifrirano prek lokalnega proxyja; lokalno navaden HTTP.
var proxies: [NoiseProxy] = []
func route(_ p: Profile) -> Connection {
    switch routeResult(p) {
    case .success(let c): return c
    case .failure(let e): fail(e.message)
    }
}

struct RouteError: Error { let message: String }

func routeResult(_ p: Profile) -> Result<Connection, RouteError> {
    var c = p.conn
    guard !(c.isLocal && c.key == nil) else { return .success(c) }  // L3: s ključem prehoda tudi lokalno šifrirano
    if c.device == nil {  // povezava iz minigw 0.4.3 ali starejšega: en ključ naprave za ves odjemalec
        c.device = anyDevice() ?? Noise.base64url(Curve25519.Signing.PrivateKey().rawRepresentation)
        saveProfile(p.name, c)
    }
    let device = c.device.flatMap(Noise.fromBase64url).flatMap { try? Curve25519.Signing.PrivateKey(rawRepresentation: $0) }
        .map { (key: $0, name: deviceName()) }
    guard let k = c.key.flatMap(Noise.fromBase64url), let np = NoiseProxy(routerURL: c.url, routerKey: k, device: device, suite: .aesGCM) else {  // MGW2: tudi minigw serve
        return .failure(RouteError(message: "no gateway key for profile \(p.name): copy the token again (Mini Gateway → Copy Token) and run minigw connect"))
    }
    do { try np.start() } catch { return .failure(RouteError(message: "cannot start the local encrypted proxy: \(error)")) }
    proxies.append(np)
    c.url = np.localURL
    return .success(c)
}

// MARK: profili (#17): »router« je prejšnja edina povezava = profil default, drugi »router:<ime>«; ključ naprave en

struct Profile { let name: String; let conn: Connection }

func keychainQuery(_ name: String = Profiles.defaultName) -> [String: Any] {
    [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service,
     kSecAttrAccount as String: name == Profiles.defaultName ? "router" : "router:" + name]
}

func loadProfile(_ name: String) -> Connection? {
    var q = keychainQuery(name)
    q[kSecReturnData as String] = true
    q[kSecMatchLimit as String] = kSecMatchLimitOne
    var item: CFTypeRef?
    let st = SecItemCopyMatching(q as CFDictionary, &item)
    if st == errSecInteractionNotAllowed || st == errSecAuthFailed {
        fail("macOS would ask before minigw reads profile \(name) (the Keychain is locked, or the profile was saved by a minigw with another signature): unlock the Keychain, or delete the item \(service) in Keychain Access and run minigw connect again")
    }
    guard st == errSecSuccess, let d = item as? Data else { return nil }
    return try? JSONDecoder().decode(Connection.self, from: d)
}

func saveProfile(_ name: String, _ c: Connection) {
    let data = try! JSONEncoder().encode(c)
    if SecItemUpdate(keychainQuery(name) as CFDictionary, [kSecValueData as String: data] as CFDictionary) == errSecItemNotFound {
        var add = keychainQuery(name)
        add[kSecValueData as String] = data
        guard SecItemAdd(add as CFDictionary, nil) == errSecSuccess else { fail("cannot save to Keychain") }
    }
}

/// Imena profilov iz Keychain (le atributi, brez gesel).
func profileNames() -> [String] {
    var q: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service]
    q[kSecReturnAttributes as String] = true
    q[kSecMatchLimit as String] = kSecMatchLimitAll
    var items: CFTypeRef?
    guard SecItemCopyMatching(q as CFDictionary, &items) == errSecSuccess, let list = items as? [[String: Any]] else { return [] }
    let names = list.compactMap { a -> String? in
        guard let acc = a[kSecAttrAccount as String] as? String else { return nil }
        if acc == "router" { return Profiles.defaultName }
        guard acc.hasPrefix("router:") else { return nil }
        let n = String(acc.dropFirst(7))
        return Profiles.valid(n) && n != Profiles.defaultName ? n : nil
    }
    return Array(Set(names)).sorted()
}

var defaultProfileFile: URL { sharedFoldersFile.deletingLastPathComponent().appendingPathComponent("default-profile") }

/// Izbrani privzeti (minigw profiles default), sicer »default«, sicer prvi.
func defaultName(_ names: [String]) -> String {
    let n = ((try? String(contentsOf: defaultProfileFile, encoding: .utf8)) ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
    if names.contains(n) { return n }
    return names.contains(Profiles.defaultName) || names.isEmpty ? Profiles.defaultName : names[0]
}

/// --minigw-profile <ime>: cel ukaz le s tem prehodom.
var profileFlag: String?

/// minigw teče v agentu prehoda (MINIGW_AGENT_URL, žeton v datoteki MINIGW_AGENT_AUTH_FILE): le ta povezava, brez profilov,
/// proxyja, šifriranja in deljenja mape (prehod sam doda napravo, sejo in mapo klicatelja).
func agentConn() -> Connection? {
    let e = ProcessInfo.processInfo.environment
    // M3: žeton ni v okolju (drugi procesi istega uporabnika ga tam lahko preberejo)
    guard let u = e["MINIGW_AGENT_URL"], let f = e["MINIGW_AGENT_AUTH_FILE"], !f.isEmpty,
          let raw = FileManager.default.contents(atPath: f), raw.count <= 256,
          case let t = String(decoding: raw, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines), !t.isEmpty,
          let c = URLComponents(string: u),
          c.scheme == "http", c.host == "127.0.0.1", c.port != nil, c.user == nil, c.path.isEmpty || c.path == "/", c.query == nil
    else { return nil }  // I3: le vrata izvajanja na tem računalniku
    return Connection(url: u, token: t)
}

/// Shranjeni profili, privzeti prvi; z --minigw-profile le ta. V agentu prehoda le njegova povezava.
func loadProfiles() -> [Profile] {
    if let c = agentConn() { return [Profile(name: "agent", conn: c)] }
    var names = profileNames()
    var def = defaultName(names)
    if let f = profileFlag {
        guard names.contains(f) else { fail("no profile \(f) (minigw profiles lists them)") }
        def = f; names = [f]
    }
    return ([def] + names.filter { $0 != def }).compactMap { n in loadProfile(n).map { Profile(name: n, conn: $0) } }
}

func anyDevice() -> String? { profileNames().lazy.compactMap { loadProfile($0)?.device }.first }

/// »192.168.1.20«, »192.168.1.20:8765«, »mini.local«, »http://…« → osnovni URL.
func normalize(_ s: String) -> String {
    var u = s.contains("://") ? s : "http://" + s
    if u.hasSuffix("/") { u.removeLast() }
    if let comps = URLComponents(string: u), comps.port == nil { u += ":8765" }
    return u
}

// MARK: HTTP do routerja

func get(_ c: Connection, _ path: String) -> (Int, [String: Any]?) {
    guard URL(string: c.url + path) != nil else { fail("invalid gateway URL \(c.url)") }
    let (s, d) = call(c, "GET", path, timeout: 5)
    return (s, d.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] })
}

/// Največji odgovor routerja v pomnilniku (datoteka do 64 MB + glave); router ni zaupanja vreden.
let maxReply = 80 << 20

/// Poljubna zahteva; telo odgovora kot Data (0 = ni povezave). Telo gre najprej v začasno datoteko, ne v pomnilnik.
func call(_ c: Connection, _ method: String, _ path: String, _ headers: [String: String] = [:], _ body: Data? = nil,
          timeout: TimeInterval) -> (Int, Data?) {
    if agentConn() != nil {  // v agentu prehoda: vrata izvajanja, po potrebi prek posrednika sandboxa
        var status = 0, data = Data()
        _ = agentHTTP(c, method, path, body: body, onHead: { status = $0 }) { if data.count < maxReply { data.append($0) } }
        return (status, status == 0 ? nil : data)
    }
    var r = URLRequest(url: URL(string: c.url + path)!, timeoutInterval: timeout)
    r.httpMethod = method
    r.httpBody = body
    r.setValue("Bearer \(c.token)", forHTTPHeaderField: "authorization")
    r.setValue(clientInfo, forHTTPHeaderField: "x-minigw-client")
    for (k, v) in headers { r.setValue(v, forHTTPHeaderField: k) }
    let done = DispatchSemaphore(value: 0)
    var result: (Int, Data?) = (0, nil)
    Profiles.session.downloadTask(with: r) { file, resp, _ in  // L2: brez preusmeritev
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
    let c: Connection, session: String, root: URL, readOnly: Bool, kind: String  // direct, sync, native, native-direct, worktree
    let top: String?  // worktree: koren repozitorija git
    var sync: Bool { kind == "sync" }
    var native: Bool { kind == "native" || kind == "native-direct" }
    var branches = false  // native v repozitoriju git: agenti na svojih vejah (odgovor prehoda)
    var locked = false    // native v mapi sami: en pisoči agent naenkrat (starejši prehod tega ne pošlje)
    var worktree: Bool { kind == "worktree" }
    var refusal = ""  // zakaj router mape ni sprejel
    static let writeMethods: Set<String> = ["PUT", "DELETE", "MKCOL", "MOVE", "COPY", "PROPPATCH", "LOCK"]
    private let lock = NSLock()
    private var current = ""
    var key: String { lock.withLock { current } }
    private let syncLock = NSLock()
    private var synced: [String: Sync.Entry] = [:]  // manifest ob zadnjem prenosu (zgoščevanje, spori)
    private var sent: [String: Set<String>] = [:]   // pot → različice, ki jih je router dobil od tega odjemalca (#20)
    private var reported: Set<String> = []          // že javljene prevelike datoteke
    private var unpulled = false                    // spremembe agenta, ki niso bile prevzete in potrjene

    init(_ c: Connection, session: String, root: URL, readOnly: Bool, kind: String, top: String? = nil) {
        self.c = c; self.session = session; self.root = root; self.readOnly = readOnly; self.kind = kind; self.top = top
    }

    func register() -> Bool {
        var h = ["x-claude-mini-session": session, "x-claude-mini-cwd": root.path, "x-share-key": key,
                 "x-share-mode": readOnly ? "ro" : "rw", "x-claude-mini-work": native ? "native" : kind]
        if kind == "native-direct" { h["x-native-inplace"] = "1" }  // agenti v mapi sami (starejši prehod to že dela)
        if worktree { h["x-worktree-top"] = top ?? "" }
        if native {  // vsaka registracija (tudi po ponovnem zagonu routerja) z novim dokazom
            let (proof, why) = nativeProof(c, root.path)
            guard let proof else { refusal = why; return false }
            h.merge(proof) { $1 }
        }
        let (s, d) = call(c, "POST", "/minigw/share", h, timeout: 10)
        let o = d.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] }
        guard s == 200, let k = o?["key"] as? String else {
            if refusal.isEmpty { refusal = ((o?["error"] as? [String: Any])?["message"] as? String).map(clean) ?? "HTTP \(s)" }
            return false
        }
        lock.withLock { current = k }
        branches = o?["branches"] as? Bool == true
        locked = o?["lock"] as? Bool == true
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
                // worktree: le WORKTREE-PUSH (posnetek k routerju) in WORKTREE-PULL (veja agenta kot ref).
                let r = worktree ? (job.method == "WORKTREE-PUSH" ? syncLock.withLock { wtPush(k, job) }
                                    : job.method == "WORKTREE-PULL" ? syncLock.withLock { wtPull(k, job) } : DAVResponse(403))
                    : native && job.method == "WORKTREE-NOTE" ? noteJob(job)  // native z git: veja ni bila dostavljena
                    : native || job.method.hasPrefix("WORKTREE-")
                        || !Sync.jobAllowed(job.method, sync: sync, readOnly: readOnly, write: Self.writeMethods.contains(job.method))
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
                    note("the gateway restarted; the agent's changes that were not copied back may be overwritten by the next sync")
                }
                _ = register()  // router se je znova zagnal
                sleep(1)
            default:
                sleep(2)        // router nedosegljiv: poskusi znova
            }
        }
    }

    private func note(_ s: String) { err.write(Data(("minigw: " + clean(s) + "\n").utf8)) }

    private func noteJob(_ job: DAVJob) -> DAVResponse {
        note(((try? JSONSerialization.jsonObject(with: job.body)) as? [String: Any])?["message"] as? String ?? "")
        return DAVResponse(200)
    }

    /// worktree: posnetek stanja (brez spremembe indeksa ali vej) in bundle manjkajočih objektov po delih do 32 MB.
    private func wtPush(_ k: String, _ job: DAVJob) -> DAVResponse {
        let have = (try? JSONSerialization.jsonObject(with: job.body) as? [String: Any])?["have"] as? [String] ?? []
        guard let top, let snap = Worktree.snapshot(top: top) else { note("worktree: cannot take a snapshot (no commit yet?)"); return DAVResponse(500) }
        let file = FileManager.default.temporaryDirectory.appendingPathComponent("minigw-\(UUID().uuidString).bundle")
        defer { try? FileManager.default.removeItem(at: file) }
        guard let made = Worktree.bundle(top: top, snapshot: snap, have: have, to: file) else { note("worktree: git bundle failed"); return DAVResponse(500) }
        if made, let h = FileHandle(forReadingAtPath: file.path) {
            defer { try? h.close() }
            var part = 0
            while let d = try? h.read(upToCount: 32 << 20), !d.isEmpty {
                guard call(c, "PUT", "/minigw/worktree/bundle", ["x-share-key": k, "x-part": String(part)], d, timeout: 600).0 == 204 else {
                    note("worktree: upload to the gateway failed"); return DAVResponse(500)
                }
                part += 1
            }
        }
        return DAVResponse(200, [:], (try? JSONSerialization.data(withJSONObject: ["snapshot": snap])) ?? Data())
    }

    /// worktree: veja agenta iz routerja le kot ref (Worktree.fetch: fsck, brez hookov, podmodulov, checkouta).
    private func wtPull(_ k: String, _ job: DAVJob) -> DAVResponse {
        guard let top, let branch = (try? JSONSerialization.jsonObject(with: job.body) as? [String: Any])?["branch"] as? String,
              Worktree.validBranch(branch) else { return DAVResponse(403) }
        let (s, d) = call(c, "GET", "/minigw/worktree/result", ["x-share-key": k], timeout: 600)
        guard s == 200, let d else {  // L5: razlog prehoda (prevelik klon ali veja), sicer status
            let why = d.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] }.flatMap { ($0["error"] as? [String: Any])?["message"] as? String }
            note("worktree: cannot get \(branch) from the gateway: \(why.map(clean) ?? "HTTP \(s)")"); return DAVResponse(500)
        }
        let file = FileManager.default.temporaryDirectory.appendingPathComponent("minigw-\(UUID().uuidString).bundle")
        defer { try? FileManager.default.removeItem(at: file) }
        guard (try? d.write(to: file)) != nil else { return DAVResponse(500) }
        if let why = Worktree.fetch(top: top, bundle: file, branch: branch) { note("worktree: " + why); return DAVResponse(500) }
        note("the agent's work is on branch \(branch) (git log \(branch); merge it when you are ready)")
        return DAVResponse(200)
    }

    /// Pred agentom: manifest k routerju, nato datoteke, ki jih potrebuje.
    private func push(_ k: String) -> Bool {
        var (files, skipped) = Sync.scan(root, previous: synced, trusted: true)
        for p in skipped where reported.insert(p).inserted { note("not synced (larger than 64 MB): \(p)") }
        for p in Sync.caseCollisions(Set(files.keys)) {  // #2
            files[p] = nil
            if reported.insert("case:" + p).inserted { note("not synced: \(p) differs from another file only in upper and lower case") }
        }
        guard let body = try? JSONEncoder().encode(Array(files.values)) else { return false }
        let (s, d) = call(c, "POST", "/minigw/sync/manifest", ["x-share-key": k, "content-type": "application/json"], body, timeout: 300)
        let reply = s == 200 ? d.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] } : nil
        guard let need = reply?["need"] as? [String] else {
            let msg = d.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] }.flatMap { ($0["error"] as? [String: Any])?["message"] as? String }
            note(msg.map { "sync to the gateway failed: \($0)" } ?? "sync to the gateway failed (HTTP \(s))"); return false
        }
        // Neuspešen prenos in »kept« (router je obdržal agentovo različico): stanje poti ostane staro, da prevzem zazna spor.
        var next = files
        for p in reply?["kept"] as? [String] ?? [] {
            next[p] = synced[p]
            note("the agent's change to \(p) is not copied back yet; kept on the gateway")
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
        for (p, e) in next { if (sent[p]?.count ?? 0) > 64 { sent[p] = [] }; sent[p, default: []].insert(e.sha256) }
        return true
    }

    /// Po agentu: spremembe kopije v mapo (spori → <ime>.minigw-conflict).
    private func pull(_ k: String) -> Bool {
        let (s, d) = call(c, "GET", "/minigw/sync/changes", ["x-share-key": k], timeout: 300)
        guard s == 200, let d, let ch = try? JSONDecoder().decode(Sync.Changes.self, from: d) else {
            note("cannot get the agent's changes from the gateway (HTTP \(s))"); unpulled = true; return false
        }
        let known = sent
        let (notes, done) = Sync.apply(ch, root: root, state: &synced, known: { p, sha in known[p]?.contains(sha) == true }) { p in
            let (st, data) = call(c, "GET", "/minigw/sync/file", ["x-share-key": k, "x-sync-path": Sync.encode(p)], timeout: 300)
            return st == 200 ? data : nil
        }
        notes.forEach(note)
        // Potrditev: router šele zdaj šteje te spremembe za prevzete; ostale pošlje znova.
        unpulled = done.count < ch.changed.count + ch.deleted.count
        if !done.isEmpty, let body = try? JSONEncoder().encode(done),
           call(c, "POST", "/minigw/sync/ack", ["x-share-key": k, "content-type": "application/json"], body, timeout: 60).0 != 204 {
            note("the gateway did not confirm the copied-back changes; they come again next time")
            unpulled = true
        }
        if !ch.changed.isEmpty || !ch.deleted.isEmpty { note("synced back from the gateway: \(ch.changed.count) changed, \(ch.deleted.count) deleted") }
        return true
    }
}

/// native: router zapiše označbo v svojo mapo, minigw jo prebere skozi priklop SMB → glave za dokaz; sicer razlog.
func nativeProof(_ c: Connection, _ cwd: String) -> (proof: [String: String]?, why: String) {
    guard let m = Native.detect(cwd: cwd, override: ProcessInfo.processInfo.environment["MINIGW_NATIVE_MOUNT"]) else {
        return (nil, "\(cwd) is not on an SMB share (set MINIGW_NATIVE_MOUNT=<share>=<mount folder> if it is)")
    }
    let body = try? JSONSerialization.data(withJSONObject: ["share": m.share, "path": m.path])
    let (s, d) = call(c, "POST", "/minigw/native", ["content-type": "application/json"], body, timeout: 10)
    let o = d.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] }
    guard s == 200, let id = o?["id"] as? String, let file = o?["file"] as? String, file == ".minigw-native-" + id, Native.validMarker(file) else {
        let msg = ((o?["error"] as? [String: Any])?["message"] as? String).map(clean)
        return (nil, s == 404 ? "the gateway does not support native mode" : msg ?? "the gateway refused native mode (HTTP \(s))")
    }
    for _ in 0..<25 {  // SMB: nova datoteka je lahko vidna z zamikom (predpomnilnik imenika)
        if let v = Native.readMarker(URL(fileURLWithPath: cwd).appendingPathComponent(file)) {
            return (["x-native-id": id, "x-native-proof": v], "")
        }
        usleep(200_000)
    }
    _ = call(c, "DELETE", "/minigw/native", ["x-native-id": id], nil, timeout: 5)  // označba ne ostane v mapi do TTL
    return (nil, "\(cwd) is not the gateway's folder \(m.share)/\(m.path) (its check file is not visible here)")
}

/// minigw run: ukaz v mapi na routerju; izhod in izhodna koda kot pri lokalnem zagonu (255 = povezava prekinjena).
func runRemote(_ c: Connection, _ argv: [String], _ proof: [String: String]) -> Int32 {
    var r = URLRequest(url: URL(string: c.url + "/minigw/run")!, timeoutInterval: 120)  // router pošlje »p« vsakih 15 s
    r.httpMethod = "POST"
    r.httpBody = try? JSONSerialization.data(withJSONObject: ["argv": argv])
    r.setValue("Bearer \(c.token)", forHTTPHeaderField: "authorization")
    r.setValue("application/json", forHTTPHeaderField: "content-type")
    for (k, v) in proof { r.setValue(v, forHTTPHeaderField: k) }
    final class Box: @unchecked Sendable { var code: Int32 = 255 }
    let box = Box(), done = DispatchSemaphore(value: 0)
    Task {
        defer { done.signal() }
        do {
            let (bytes, resp) = try await Profiles.session.bytes(for: r)
            guard (resp as? HTTPURLResponse)?.statusCode == 200 else {
                var d = Data()
                for try await b in bytes { d.append(b); if d.count > 1 << 16 { break } }
                let msg = ((try? JSONSerialization.jsonObject(with: d) as? [String: Any])?["error"] as? [String: Any])?["message"] as? String
                err.write(Data("minigw: \(clean(msg ?? "the router answered HTTP \((resp as? HTTPURLResponse)?.statusCode ?? 0)"))\n".utf8))
                return
            }
            let (ttyOut, ttyErr) = (isatty(1) == 1, isatty(2) == 1)  // terminal: le barve, brez drugih zaporedij routerja
            var (fOut, fErr) = (Native.TerminalFilter(), Native.TerminalFilter())
            for try await line in bytes.lines {
                let v = String(line.dropFirst(2))
                if line.hasPrefix("o "), let d = Data(base64Encoded: v) { out.write(ttyOut ? fOut.feed(d) : d) }
                else if line.hasPrefix("e "), let d = Data(base64Encoded: v) { err.write(ttyErr ? fErr.feed(d) : d) }
                else if line.hasPrefix("x "), let n = Int32(v) { box.code = n }
            }
        } catch { err.write(Data("minigw: lost the connection to the gateway: \(error.localizedDescription)\n".utf8)) }
    }
    done.wait()
    return box.code
}

func requireProfile() -> Profile {
    guard let p = loadProfiles().first else { fail("not connected; run: minigw connect <host[:port]> --token <token>") }
    return p
}

/// Naslov, dosegljivost, ponudniki in število modelov enega profila; true = dosegljiv.
func profileStatus(_ p: Profile) -> Bool {
    let c: Connection
    switch routeResult(p) {
    case .success(let r): c = r
    case .failure(let e): say("Gateway:   \(p.conn.url) (\(e.message))"); return false
    }
    let (s, ready) = get(c, "/health/ready")
    say("Gateway:   \(p.conn.url) (\(s == 200 ? "reachable" : s == 0 ? "unreachable" : "HTTP \(s)")\(p.conn.isLocal && p.conn.key == nil ? "" : ", encrypted"))")
    if let pr = ready?["providers"] as? [String: Bool] {
        say("Providers: " + pr.keys.sorted().map { "\($0) \(pr[$0]! ? "✓" : "✗")" }.joined(separator: ", "))
    }
    let (ms, models) = get(c, "/v1/models")
    say("Models:    " + (ms == 200 ? "\((models?["data"] as? [Any])?.count ?? 0)" : ms == 401 ? "token rejected" : "–"))
    return s == 200
}

func fetchModels(_ c: Connection) -> [[String: Any]]? {
    let (s, m) = get(c, "/v1/models")
    return s == 200 ? (m?["data"] as? [[String: Any]] ?? []) : nil
}

// MARK: deljene mape

/// Mape zunaj domače, ki jih uporabnik deli z »minigw share add« (enako kot odjemalec Go na macOS).
let sharedFoldersFile = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
    .appendingPathComponent("minigw/shared-folders")

func sharedFolders() -> [SharedFolders.Entry] {
    SharedFolders.parse((try? String(contentsOf: sharedFoldersFile, encoding: .utf8)) ?? "")
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
    // add: --work=direct|sync|native|native-direct|worktree|auto in --ro = privzeti način te mape in podmap (zastavica ob zagonu ima prednost)
    var work: String?, ro = false, rest: [String] = []
    for x in a.dropFirst() {
        if x.hasPrefix("--work=") { work = String(x.dropFirst(7)) } else if x == "--ro" { ro = true } else { rest.append(x) }
    }
    if let work, !SharedFolders.modes.contains(work) { fail("unknown --work=\(work) (use direct, sync, native, native-direct, worktree or auto)") }
    if let work, work.hasPrefix("native"), ro { fail("--work=\(work) cannot be read-only (the agent works in the gateway's own folder)") }
    let dir = URL(fileURLWithPath: rest.first ?? FileManager.default.currentDirectoryPath).standardizedFileURL.path
    guard SharedFolders.validPath(dir) else { fail("folder names with tabs or line breaks cannot be shared") }
    var list = sharedFolders()
    func save() {
        try? FileManager.default.createDirectory(at: sharedFoldersFile.deletingLastPathComponent(), withIntermediateDirectories: true)
        guard (try? SharedFolders.format(list).write(to: sharedFoldersFile, atomically: true, encoding: .utf8)) != nil
        else { fail("cannot save \(sharedFoldersFile.path)") }
    }
    func describe(_ e: SharedFolders.Entry) -> String {
        e.path + (e.work.map { "  (\($0)" + (e.readOnly ? ", read-only)" : ")") } ?? (e.readOnly ? "  (read-only)" : ""))
    }
    switch sub {
    case "add":
        var isDir: ObjCBool = false
        guard FileManager.default.fileExists(atPath: dir, isDirectory: &isDir), isDir.boolValue else { fail("not a folder: \(dir)") }
        guard dir != "/", dir != NSHomeDirectory() else {
            fail("refusing to share \(dir) (the disk root or your home folder would expose everything); share a project folder")
        }
        let e = SharedFolders.Entry(path: dir, work: work, readOnly: ro)
        if list.contains(e) { print("already shared: \(describe(e))"); exit(0) }
        list.removeAll { $0.path == dir }; list.append(e); save()
        print("shared with the gateway from now on: \(describe(e)) (and its subfolders)")
    case "remove":
        list.removeAll { $0.path == dir }; save()
        print("no longer shared: \(dir)")
    case "list":
        if list.isEmpty { print("no extra shared folders (folders under your home folder are shared by default)") }
        list.forEach { print(describe($0)) }
    default:
        fail("usage: minigw share add [--work=direct|sync|native|native-direct|worktree|auto] [--ro] [folder] | minigw share remove [folder] | minigw share list")
    }
    exit(0)
}

// MARK: ukazi

let minigwVersion = "0.5.18"  // bump-version.sh
var args = Array(CommandLine.arguments.dropFirst())
// --minigw-ro (ali MINIGW_SHARE=ro): mapa samo za branje; zastavica ne gre naprej v claude.
var readOnly = args.contains("--minigw-ro") || ProcessInfo.processInfo.environment["MINIGW_SHARE"] == "ro"
args.removeAll { $0 == "--minigw-ro" }
// --minigw-role <ime> (ali MINIGW_ROLE): vloga klicatelja za zgodovino sej (kasneje omejitve); varovalo, ne meja.
let roleArg = GatewayHistory.takeRole(args, env: ProcessInfo.processInfo.environment["MINIGW_ROLE"])
let role = roleArg.role
args = roleArg.rest
if !roleArg.ok { fail("--minigw-role: 1-32 letters, digits, . _ or -") }
// --minigw-work=sync (ali MINIGW_WORK=sync): agenti delajo v kopiji na routerju namesto v priklopu; privzeto direct.
// native: mapa je prek SMB mapa routerja → agenti delajo neposredno v njej (glej Native).
// Prednost: --minigw-work= / MINIGW_WORK > način mape iz »minigw share add --work=…« > direct.
var workFlag = ProcessInfo.processInfo.environment["MINIGW_WORK"].flatMap { $0.isEmpty ? nil : $0 }
for a in args where a.hasPrefix("--minigw-work=") { workFlag = String(a.dropFirst("--minigw-work=".count)) }
args.removeAll { $0.hasPrefix("--minigw-work=") }
if let w = workFlag, !SharedFolders.modes.contains(w) { fail("unknown --minigw-work=\(w) (use direct, sync, native, native-direct, worktree or auto)") }
// --minigw-profile <ime> (ali =ime): cel ukaz le s tem prehodom (#17).
while let i = args.firstIndex(where: { $0 == "--minigw-profile" || $0.hasPrefix("--minigw-profile=") }) {
    if args[i] == "--minigw-profile" {
        guard i + 1 < args.count else { fail("--minigw-profile needs a profile name") }
        profileFlag = args[i + 1]; args.removeSubrange(i...i + 1)
    } else { profileFlag = String(args[i].dropFirst("--minigw-profile=".count)); args.remove(at: i) }
}

if agentConn() != nil, let first = args.first, ["connect", "disconnect", "profiles"].contains(first) {
    fail("minigw runs inside an agent of the gateway (MINIGW_AGENT_URL): \(first) is not available here")
}
if let c = agentConn(), args.first?.hasPrefix("-") == true, args.contains("-p") || args.contains("--print") { agentAsk(c, args) }

switch args.first {
case "share":
    shareCommand(Array(args.dropFirst()))
case "gw", "router":  // router: starejše ime, skrit alias
    routerCommand(Array(args.dropFirst()))
    exit(0)
case "run":
    var argv = Array(args.dropFirst())
    if argv.first == "--" { argv.removeFirst() }
    guard !argv.isEmpty else { fail("usage: minigw run [--] <command> [args …]") }
    let c = route(requireProfile())
    let (proof, why) = nativeProof(c, FileManager.default.currentDirectoryPath)
    guard let proof else { fail("cannot run on the gateway: \(why)") }
    exit(runRemote(c, argv, proof))
case "connect":
    guard args.count >= 2 else { fail("usage: minigw connect <host[:port]> [--name <profile>] [--token <token> | --token -]   (- reads it from stdin)") }
    let names = profileNames()
    var name = defaultName(names)  // brez --name: zamenja privzeti profil (kot prej edino povezavo)
    if let i = args.firstIndex(of: "--name"), i + 1 < args.count { name = args[i + 1] }
    guard Profiles.valid(name) else { fail("invalid --name \(name) (lowercase letters, digits and -, up to 32)") }
    let saved = loadProfile(name)
    var token = saved.map { $0.token + ($0.key.map { "#" + $0 } ?? "") } ?? ""
    if let i = args.firstIndex(of: "--token"), i + 1 < args.count { token = args[i + 1] }
    if token == "-" { token = readKey("gateway token") }  // F3: s stdin ali brez odmeva, ne v argv
    guard !token.isEmpty else { fail("missing --token (copy it from Mini Gateway → Settings → General → Copy Token)") }
    let parts = token.split(separator: "#", maxSplits: 1).map(String.init)
    let c = Connection(url: normalize(args[1]), token: parts[0], key: parts.count > 1 ? parts[1] : nil,
                       device: anyDevice() ?? Noise.base64url(Curve25519.Signing.PrivateKey().rawRepresentation))  // en ključ za vse profile
    let asProfile = name != Profiles.defaultName || names.contains { $0 != Profiles.defaultName } ? " as profile \(name)" : ""
    let (status, body) = get(route(Profile(name: name, conn: c)), "/v1/models")
    switch status {
    case 200: saveProfile(name, c); say("Connected to \(c.url)" + (c.isLocal && c.key == nil ? "" : " (encrypted)") + asProfile)
    case 401: fail("gateway at \(c.url) rejected the token")
    case 403:  // naprava še ni dovoljena (minigw serve): ključ naprave ostane, da ga prehod lahko doda
        saveProfile(name, c)
        let msg = (body?["error"] as? [String: Any])?["message"] as? String ?? ""
        fail("gateway at \(c.url) refused this device: \(clean(msg))\nthen run minigw connect again")
    case 426: fail("gateway at \(c.url) needs an encrypted connection: copy the token again (it includes the gateway key)")
    case 0: fail(c.isLocal ? "cannot reach \(c.url)" : "cannot reach \(c.url), or the gateway key does not match (copy the token again)")
    default: fail("gateway at \(c.url) answered HTTP \(status)")
    }
    exit(0)

case "disconnect":
    var name = requireProfile().name
    if let i = args.firstIndex(of: "--name"), i + 1 < args.count { name = args[i + 1] }
    guard Profiles.valid(name), loadProfile(name) != nil else { fail("no profile \(name)") }
    SecItemDelete(keychainQuery(name) as CFDictionary)
    say(profileNames().isEmpty && name == Profiles.defaultName ? "Disconnected" : "Disconnected profile \(name)")
    exit(0)

case "profiles":
    let names = profileNames()
    switch Array(args.dropFirst()) {
    case [], ["list"]:
        guard !names.isEmpty else { fail("no profiles; run: minigw connect <host[:port]> --name <name> --token -") }
        let def = defaultName(names)
        for n in names { say((n == def ? "* " : "  ") + n + "  " + (loadProfile(n)?.url ?? "?")) }
    case let a where a.count == 2 && a[0] == "default":
        guard names.contains(a[1]) else { fail("no profile \(a[1])") }
        try? FileManager.default.createDirectory(at: defaultProfileFile.deletingLastPathComponent(), withIntermediateDirectories: true)
        guard (try? (a[1] + "\n").write(to: defaultProfileFile, atomically: true, encoding: .utf8)) != nil else { fail("cannot save \(defaultProfileFile.path)") }
        say("Default profile: \(a[1]) (its models also appear without @\(a[1]))")
    default: fail("usage: minigw profiles [list] | minigw profiles default <name>")
    }
    exit(0)

case "status":
    let ps = loadProfiles()
    if ps.isEmpty { _ = requireProfile() }
    var ok = true
    for (i, p) in ps.enumerated() {
        if ps.count > 1 {
            if i > 0 { say("") }
            say("Profile:   \(p.name)" + (i == 0 ? " (default: its models also appear without @\(p.name))" : ""))
        }
        if !profileStatus(p), i == 0 { ok = false }
    }
    exit(ok ? 0 : 1)

case "models":
    let ps = loadProfiles()
    if ps.isEmpty { _ = requireProfile() }
    var data: [[String: Any]]
    if ps.count == 1 {
        let (s, models) = get(route(ps[0]), "/v1/models")
        guard s == 200, let d = models?["data"] as? [[String: Any]] else { fail(s == 401 ? "token rejected" : "cannot list models (HTTP \(s))") }
        data = d
    } else {
        let lists = ps.map { p -> [[String: Any]]? in
            let l = (try? routeResult(p).get()).flatMap(fetchModels)
            if l == nil { err.write(Data("minigw: gateway \(p.name) (\(p.conn.url)) is not reachable; its models are left out\n".utf8)) }
            return l
        }
        guard let m = Profiles.merge(lists, names: ps.map(\.name)) else { fail("no gateway is reachable (minigw status)") }
        data = m
    }
    let width = data.compactMap { ($0["id"] as? String)?.count }.max() ?? 0
    for m in data {
        let id = m["id"] as? String ?? "?"
        say(id.padding(toLength: width, withPad: " ", startingAt: 0) + "  " + (m["description"] as? String ?? ""))
    }
    exit(0)

case "--minigw-help", "-h", "--help", "-v", "--version":
    if args.first == "-v" || args.first == "--version" { say("minigw \(minigwVersion)"); exit(0) } else {  // le minigw (gw status)
        say("""
        minigw connect <host[:port]> [--name P] [--token T|-]  save a gateway as profile P (Keychain; - = stdin)
        minigw status | models | disconnect [--name P]   all profiles; models of other profiles end in @P
        minigw profiles [default P]                list profiles, or pick the default (its models also without @P)
        minigw --minigw-profile P …                 use only profile P for this command
        minigw share add|remove [folder] | list    share a system or hidden folder itself (contents are always shared)
        minigw share add --work=native|native-direct|worktree|sync|direct|auto [--ro] [folder]   default mode for that folder (flag/env wins)
        minigw gw status|devices|provider|claude|token …    change the Mini Gateway app on this Mac (minigw gw help)
        minigw --minigw-ro [claude args]           share the current folder read-only (or MINIGW_SHARE=ro)
        minigw --minigw-role NAME [claude args]    the caller's role for the gateway's session history (or MINIGW_ROLE)
        minigw --minigw-work=sync [claude args]    agents work in a synced copy on the gateway (or MINIGW_WORK=sync)
        minigw --minigw-work=native [claude args]  folder is the gateway's SMB share: agents work there; git repository: each on its own branch minigw/… (or MINIGW_WORK=native)
        minigw --minigw-work=native-direct [claude args]  as native, but agents change the folder itself, one writing agent at a time
        minigw --minigw-work=worktree [claude args]  git repository: agents work in a clone on the gateway, results come back as branches minigw/…
        minigw --minigw-work=auto [claude args]    gateway share → native, git repository → worktree, otherwise sync (--minigw-ro → direct)
        minigw run [--] <command> [args …]          run a build in this folder on the gateway (SMB share, allowed in its Settings)
        minigw [claude options …]                    run Claude Code through the gateway
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

if agentConn() == nil {  // --model …@profil: ves zagon s tem prehodom, kot --minigw-profile (#8)
    let mp = Profiles.modelProfile(args, names: profileNames())
    if let p = mp.profile {
        if let f = profileFlag, f != p { fail("--model \(mp.model) and --minigw-profile \(f) name different gateways") }
        profileFlag = p; args = mp.args
    }
}
let profiles = loadProfiles()
if profiles.isEmpty { _ = requireProfile() }
let saved = profiles[0].conn  // privzeti profil: mapa, glave in modeli brez pripone kot prej
let c = route(profiles[0])
for k in ProcessInfo.processInfo.environment.keys where Profiles.parentClaudeVar(k) { unsetenv(k) }  // #7
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
        && (autoShare(cwd) || SharedFolders.lookup(sharedFolders(), cwd: cwd) != nil))
let mode = SharedFolders.mode(flag: workFlag, saved: SharedFolders.lookup(sharedFolders(), cwd: cwd), readOnly: readOnly)
if let asked = workFlag ?? SharedFolders.lookup(sharedFolders(), cwd: cwd)?.work, asked.hasPrefix("native") || asked == "worktree", mode.work != asked {
    err.write(Data("minigw: \(asked) mode cannot be read-only; sharing read-only over the network instead\n".utf8))
}
let work = mode.work
readOnly = mode.readOnly
let implicitWork = workFlag == nil && SharedFolders.lookup(sharedFolders(), cwd: cwd)?.work == nil

// x-claude-mini-share: router po ponovnem zagonu počaka, da se mapa znova registrira (namesto napake cwd_unavailable).
setenv("ANTHROPIC_CUSTOM_HEADERS", "x-claude-mini-session: \(sid)\nx-claude-mini-cwd: \(cwd)" + (wantShare ? "\nx-claude-mini-share: 1" : "")
       + (role.isEmpty ? "" : "\nx-claude-mini-role: \(role)"), 1)

// Brez proxyja in brez mape: minigw ni več potreben → exec.
guard !proxies.isEmpty || (wantShare && !sid.isEmpty) || profiles.count > 1 else {
    execvp("claude", argv)
    fail("cannot run claude: \(String(cString: strerror(errno))) (is Claude Code installed and in PATH?)", code: 127)
}

if !wantShare && remoteRouter && shareEnv != "0" {
    err.write(Data("minigw: \(cwd) is not shared with the gateway, so agent models (Codex, Claude CLI, Grok) cannot see it; to share it run: minigw share add\n".utf8))
}
// Načini po vrsti (auto: native → worktree → sync); naslednji, če router prejšnjega ne sprejme.
let top = ["auto", "worktree"].contains(work) ? Worktree.toplevel(cwd) : nil
let smb = work == "auto" && Native.detect(cwd: cwd, override: ProcessInfo.processInfo.environment["MINIGW_NATIVE_MOUNT"]) != nil
var tries = SharedFolders.candidates(work: work, readOnly: readOnly, smb: smb, git: top != nil)
// Brez commita ni posnetka: enkrat preveri, nato sync (#10).
let noCommit = top.map { tries.contains("worktree") && !Worktree.hasCommit(top: $0) } ?? false
if noCommit { tries.removeAll { $0 == "worktree" } }
var share = ShareClient(c, session: sid, root: URL(fileURLWithPath: cwd), readOnly: readOnly, kind: "direct")
var registered = false
if wantShare, !sid.isEmpty {
    if work == "worktree", top == nil { err.write(Data("minigw: \(cwd) is not in a git repository; worktree mode is not possible\n".utf8)) }
    if noCommit { err.write(Data("minigw: the git repository \(top ?? "") has no commit yet, so worktree mode is not possible; using \(tries[0])\n".utf8)) }
    (share, registered) = registerShare(c, tries: tries, quiet: false)
}
if !wantShare || sid.isEmpty {
    // le šifrirani proxy
} else if registered {
    share.start()
    if share.native && share.branches {
        err.write(Data("minigw: \(cwd) is a folder of \(routerHost) (SMB): agent models work there in clones of this git repository, each on its own branch minigw/… (native; --minigw-work=native-direct = in the folder itself)\n".utf8))
    } else if share.native {
        err.write(Data("minigw: \(cwd) is a folder of \(routerHost) (SMB): agent models work in it there directly\(share.locked ? ", one writing agent at a time" : "") (native)\n".utf8))
    } else if share.worktree {
        err.write(Data("minigw: agent models work in a clone of this git repository on \(routerHost); their work comes back as branches minigw/… (worktree)\n".utf8))
    } else {
        err.write(Data("minigw: \(share.sync ? "syncing" : "sharing") \(cwd) \(readOnly ? "read-only" : "read-write") with \(routerHost) for agent models (--minigw-ro = read-only, MINIGW_SHARE=0 = off)\n".utf8))
    }
} else {
    err.write(Data("minigw: gateway did not accept the folder; Claude CLI models with tools will not see it\n".utf8))
}

/// Načini po vrsti (auto: native → worktree → sync); naslednji, če prehod prejšnjega ne sprejme.
/// Način ni izbran (zastavica, MINIGW_WORK, share add): prehod brez priklopa mape (minigw serve na Windows, Linux) direct
/// zavrne → kot auto: worktree v repozitoriju git s commitom, sicer sync. Izrecni direct ostane napaka.
@Sendable func registerShare(_ c: Connection, tries: [String], quiet: Bool, top: String? = top, implicit: Bool = implicitWork) -> (ShareClient, Bool) {
    var sc = ShareClient(c, session: sid, root: URL(fileURLWithPath: cwd), readOnly: readOnly, kind: "direct")
    for (i, w) in tries.enumerated() {
        sc = ShareClient(c, session: sid, root: URL(fileURLWithPath: cwd), readOnly: readOnly, kind: w, top: top)
        if sc.register() { return (sc, true) }
        if i + 1 < tries.count, !quiet { err.write(Data("minigw: \(w) mode not available: \(sc.refusal); trying \(tries[i + 1])\n".utf8)) }
    }
    guard implicit, sc.refusal.hasPrefix("this gateway supports --minigw-work=") else { return (sc, false) }
    let t = Worktree.toplevel(cwd)
    // M1: worktree le, če je koren repozitorija ta mapa; nadrejeni (dotfiles v domači mapi, monorepo) bi poslal tudi
    // datoteke zunaj nje.
    let real = { URL(fileURLWithPath: $0).resolvingSymlinksInPath().standardizedFileURL.path }
    let auto = !readOnly && t.map { real($0) == real(cwd) && Worktree.hasCommit(top: $0) } == true ? ["worktree", "sync"] : ["sync"]
    if !quiet { err.write(Data("minigw: the gateway has no folder mount; using \(auto[0]) mode (--minigw-work= to choose)\n".utf8)) }
    return registerShare(c, tries: auto, quiet: quiet, top: t, implicit: false)
}

// Več profilov: lokalni usmerjevalnik pred Claude Code; mapa pri drugem prehodu ob prvi zahtevi zanj (#17).
final class Extras: @unchecked Sendable {
    private let lock = NSLock()
    private var list: [ShareClient] = []
    func add(_ s: ShareClient) { lock.withLock { list.append(s) } }
    func unregister() { lock.withLock { list }.forEach { $0.unregister() } }
}
let extras = Extras()
var mux: ProfileMux?
if profiles.count > 1 {
    var targets = [ProfileMux.Target(name: profiles[0].name, url: c.url, token: c.token)]
    var hooks: [String: @Sendable () -> Bool] = [:]
    let tries = tries
    for p in profiles.dropFirst() {
        let rc: Connection
        switch routeResult(p) {
        case .success(let r): rc = r
        case .failure(let e): err.write(Data("minigw: \(e.message); its models are left out\n".utf8)); continue
        }
        targets.append(ProfileMux.Target(name: p.name, url: rc.url, token: rc.token))
        let remote = !p.conn.isLocal
        let want = shareEnv == "1" || (shareEnv != "0" && remote && (autoShare(cwd) || SharedFolders.lookup(sharedFolders(), cwd: cwd) != nil))
        guard want, !sid.isEmpty else { continue }
        let name = p.name
        hooks[name] = {
            let (sc, ok) = registerShare(rc, tries: tries, quiet: true)
            guard ok else { err.write(Data("minigw: gateway \(name) did not accept the folder: \(sc.refusal)\n".utf8)); return false }
            sc.start(); extras.add(sc)
            return true
        }
    }
    if targets.count > 1 {
        let m = ProfileMux(targets: targets, share: hooks)
        do { try m.start() } catch { fail("cannot start minigw's local proxy: \(error)") }
        mux = m
        setenv("ANTHROPIC_BASE_URL", m.localURL, 1)
        setenv("ANTHROPIC_AUTH_TOKEN", m.secret, 1)
        err.write(Data("minigw: \(targets.count) gateways; models of \(profiles[0].name) also without a suffix, the others as <model>@<profile> (minigw models)\n".utf8))
    }
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
    share.unregister(); extras.unregister()
    fail("cannot run claude: \(String(cString: strerror(rc))) (is Claude Code installed and in PATH?)", code: 127)
}
var status: Int32 = 0
while waitpid(pid, &status, 0) == -1 && errno == EINTR {}
share.unregister()
extras.unregister()
exit(status & 0x7f == 0 ? (status >> 8) & 0xff : 128 + (status & 0x7f))
