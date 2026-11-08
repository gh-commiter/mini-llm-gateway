import Foundation
import Security

/// Zunanji ponudnik z Anthropic `/v1/messages` (LiteLLM, OpenRouter): router ga le prepošilja.
public struct ProviderConfig: Codable, Identifiable, Sendable, Equatable {
    public var id: String          // tudi ime ključa v Keychainu
    public var name: String
    public var enabled: Bool
    public var baseURL: String     // brez /v1
    public var aliases: String     // napredno: vrstice »alias = upstream, okno«; prazno = odkriti modeli
    public var enabledModels: [String]?  // nil = vsi odkriti (tudi prihodnji); seznam = le ti
    /// Allow http (insecure), kot minigw serve: http le do localhost ali zasebnega IP in le s tem stikalom.
    public var insecure = false

    public init(id: String, name: String, enabled: Bool, baseURL: String, aliases: String, enabledModels: [String]? = nil,
                insecure: Bool? = nil) {
        self.id = id; self.name = name; self.enabled = enabled; self.baseURL = baseURL; self.aliases = aliases
        self.enabledModels = enabledModels
        self.insecure = insecure ?? baseURL.hasPrefix("http://")
    }

    public init(from decoder: Decoder) throws {  // starejše nastavitve brez »insecure«: http ponudniki z vklopljenim stikalom
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        name = try c.decode(String.self, forKey: .name)
        enabled = try c.decode(Bool.self, forKey: .enabled)
        baseURL = try c.decode(String.self, forKey: .baseURL)
        aliases = try c.decodeIfPresent(String.self, forKey: .aliases) ?? ""
        enabledModels = try c.decodeIfPresent([String].self, forKey: .enabledModels)
        insecure = try c.decodeIfPresent(Bool.self, forKey: .insecure) ?? baseURL.hasPrefix("http://")
    }

    /// Razlog, zakaj naslova ni mogoče uporabiti (isto pravilo kot skrbniški API in minigw serve); nil = v redu.
    public var urlError: String? {
        do { _ = try Router.checkGatewayURL(baseURL, insecure: insecure); return nil } catch let e as AdminError { return e.message } catch { return "\(error)" }
    }

    public func isModelEnabled(_ upstream: String) -> Bool { enabledModels?.contains(upstream) ?? true }

    /// Vrstica: »alias = upstream ID, okno« (okno neobvezno, npr. 128k); samo »upstream« pomeni alias = upstream.
    public var aliasPairs: [(alias: String, upstream: String, window: Int?)] {
        aliases.split(whereSeparator: \.isNewline).compactMap { raw in
            let line = raw.trimmingCharacters(in: .whitespaces)
            guard !line.isEmpty, !line.hasPrefix("#") else { return nil }
            let parts = line.split(separator: ",", maxSplits: 1).map { $0.trimmingCharacters(in: .whitespaces) }
            let window = parts.count > 1 ? Self.tokens(parts[1]) : nil
            let p = parts[0].split(separator: "=", maxSplits: 1).map { $0.trimmingCharacters(in: .whitespaces) }
            if p.count == 2, !p[0].isEmpty, !p[1].isEmpty { return (p[0], p[1], window) }
            return (parts[0], parts[0], window)
        }
    }

    /// »128k«, »1M«, »200000« → število tokenov.
    static func tokens(_ s: String) -> Int? {
        let t = s.lowercased()
        if t.hasSuffix("k"), let n = Double(t.dropLast()) { return Int(n * 1_000) }
        if t.hasSuffix("m"), let n = Double(t.dropLast()) { return Int(n * 1_000_000) }
        return Int(t)
    }

    /// ID, ki ga vidi Claude Code: model discovery obdrži le ID-je, ki vsebujejo »claude« ali »anthropic«.
    public static func publishedID(_ alias: String) -> String {
        let a = alias.lowercased()
        return a.contains("claude") || a.contains("anthropic") ? alias : "claude-" + alias
    }
}

/// Naprava z minigw (ključ naprave iz šifrirane povezave). Nove se dodajo same, Siri je privzeto izklopljena.
public struct DeviceConfig: Codable, Sendable, Equatable, Identifiable {
    public var id: String     // base64url javnega ključa naprave
    public var name: String
    public var siri = false
    public var run = false    // minigw run: ukazi na tem Macu v mapi native (izvajanje kode), privzeto izklopljeno
    public var runNetwork = true  // minigw run sme v omrežje (Maven prenaša odvisnosti)
    public init(id: String, name: String, siri: Bool = false, run: Bool = false, runNetwork: Bool = true) {
        self.id = id; self.name = name; self.siri = siri; self.run = run; self.runNetwork = runNetwork
    }

    public init(from decoder: Decoder) throws {  // starejše nastavitve brez »run«
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        name = try c.decode(String.self, forKey: .name)
        siri = try c.decodeIfPresent(Bool.self, forKey: .siri) ?? false
        run = try c.decodeIfPresent(Bool.self, forKey: .run) ?? false
        runNetwork = try c.decodeIfPresent(Bool.self, forKey: .runNetwork) ?? true
    }
}

public struct RouterConfig: Codable, Sendable, Equatable {
    public var host = "127.0.0.1"
    public var port: UInt16 = 8765
    public var allowedClients = ""  // IP naslovi, ločeni s presledkom ali vejico; prazno = vsi
    public var allowPlainRemote = false  // navaden HTTP z drugih Macov (brez minigw 0.4+); lokalno vedno
    public var providers: [ProviderConfig] = [
        ProviderConfig(id: "litellm", name: "LiteLLM", enabled: false,
                       baseURL: "http://localhost:4000", aliases: ""),  // prazno = modeli in okna, ki jih sporoči LiteLLM
        ProviderConfig(id: "openrouter", name: "OpenRouter", enabled: false,
                       baseURL: "https://openrouter.ai/api", aliases: "", enabledModels: []),  // več sto modelov: izbereš sam
    ]

    public var claudeCLI = ClaudeCLIConfig()
    public var appleFM = AppleFMConfig()
    public var siri = SiriConfig()
    public var devices: [DeviceConfig] = []
    /// Vgrajena ponudnika (kot builtinGateways v minigw serve): vedno na seznamu, izklop da, odstranitev in preimenovanje ne.
    public static let builtInProviders: Set<String> = ["litellm", "openrouter"]
    /// Kot minigw serve: neznana naprava z žetonom je dovoljena le, dokler je seznam prazen in je to vklopljeno; sicer čaka
    /// na odobritev (Settings → Devices, minigw gw devices allow). Starejše nastavitve: glej init(from:).
    public var allowAnyDevice = false
    public var codex = CodexConfig()
    public var copilot = CopilotConfig()
    public var grok = GrokConfig()
    public var foundry = FoundryConfig()  // Foundry Local na tem Macu (Foundry.swift)
    public var scratchFolder = ""  // prazno = defaultScratch (Caches); npr. hiter zunanji SSD
    /// minigw run: dovoljeni ukazi (prvi argument, ločeno z vejico). Ni peskovnik: gradnja izvaja kodo projekta.
    public var runCommands = "mvn, swift, xcodebuild"
    /// --minigw-work=worktree: ukazi agentov (Bash, Codex, Grok) smejo v omrežje (Maven, SwiftPM prenašajo odvisnosti).
    public var worktreeNetwork = true
    /// Codex v načinu worktree bere vse datoteke: omrežje privzeto izklopljeno (in le, če ga Codex sicer sme).
    public var worktreeCodexNetwork = false

    public init() {}

    /// Začasne datoteke agentov (pogovori, izhodi, priponke; pozneje delovne kopije in build izhodi).
    public static let defaultScratch = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0]
        .appendingPathComponent("dev.trojar.minigateway/Scratch", isDirectory: true)

    public var scratchURL: URL {
        scratchFolder.isEmpty ? Self.defaultScratch
            : Self.dedicated(URL(fileURLWithPath: (scratchFolder as NSString).expandingTildeInPath, isDirectory: true))
    }

    /// Izbrana mapa dobi lastno podmapo (nikoli ne pišemo med uporabnikove datoteke), razen če se že tako imenuje.
    public static func dedicated(_ folder: URL) -> URL {
        folder.lastPathComponent == dedicatedName ? folder : folder.appendingPathComponent(dedicatedName, isDirectory: true)
    }
    public static let dedicatedName = "Mini Gateway Scratch"

    /// H1: dovoljena mapa vsebuje Scratch ali je v njem (agenti bi pisali v zrcala in kopije drugih naprav).
    public static func overlapsScratch(_ root: String, scratch: URL) -> Bool {
        let p = ClaudeCLI.expand(root.trimmingCharacters(in: .whitespaces))
        guard !p.isEmpty else { return false }
        let r = Sync.real(URL(fileURLWithPath: p))?.path ?? p, s = Sync.real(scratch)?.path ?? scratch.standardizedFileURL.path
        func inside(_ a: String, _ b: String) -> Bool { a == b || a.hasPrefix(b == "/" ? "/" : b + "/") }
        return inside(s, r) || inside(r, s)
    }

    /// Paket Copilot CLI (koda, ki jo copilot --version naloži in razširi).
    public static var copilotCache: URL {
        (Sync.real(URL(fileURLWithPath: NSHomeDirectory())) ?? URL(fileURLWithPath: NSHomeDirectory())).appendingPathComponent("Library/Caches/copilot")
    }

    /// #26 (L1): dovoljena mapa vsebuje paket Copilot ali je v njem: agent s pisanjem bi podtaknil kodo, ki jo naloži
    /// priprava paketa (copilot --version). Kot overlapsScratch.
    public static func overlapsCopilotCache(_ root: String) -> Bool { overlapsScratch(root, scratch: copilotCache) }

    /// M1, L1: izbrana mapa za Scratch (Settings in API); nil = v redu, sicer razlog. Scratch hrani kopije drugih naprav
    /// in zrcala, ki jih bere git prehoda: ne sme biti koren diska, domača mapa ali mapa, ki jo vsebuje, začasna mapa
    /// ali mapa v dovoljeni mapi agentov (ali obratno), in »Mini Gateway Scratch« v njej ne sme biti povezava.
    public static func scratchProblem(_ folder: URL, allowedRoots: String) -> String? {
        let fm = FileManager.default
        var isDir: ObjCBool = false
        guard folder.path.hasPrefix("/"), fm.fileExists(atPath: folder.path, isDirectory: &isDir), isDir.boolValue,
              let real = Sync.real(folder)?.path else { return "it is not an existing folder (absolute path)" }
        let d = dedicated(folder)
        if (try? fm.destinationOfSymbolicLink(atPath: d.path)) != nil { return "\(d.path) is a symbolic link" }
        func inside(_ a: String, _ b: String) -> Bool { a == b || a.hasPrefix(b == "/" ? "/" : b + "/") }
        let home = Sync.real(URL(fileURLWithPath: NSHomeDirectory()))?.path ?? NSHomeDirectory()
        if real == "/" { return "it is the disk root" }
        if inside(home, real) { return "it is your home folder or contains it" }
        let temps = ["/private/tmp", "/private/var/folders", Sync.real(FileManager.default.temporaryDirectory)?.path ?? ""].filter { !$0.isEmpty }
        if temps.contains(where: { inside(real, $0) }) { return "it is a temporary folder" }
        for r in allowedRoots.split(separator: ",") {
            let p = ClaudeCLI.expand(r.trimmingCharacters(in: .whitespaces))
            guard !p.isEmpty, let rr = Sync.real(URL(fileURLWithPath: p))?.path else { continue }
            if inside(real, rr) || inside(rr, real) { return "it is inside or contains the allowed folder \(p) (agents could read other devices' copies)" }
        }
        return nil
    }
    /// Podmape, ki jih uporablja scratch(_:); premik prenese le te.
    public static let scratchSubfolders = ["chat", "sync", "tmp", "attachments", "repos", "worktrees"]

    /// Podmapa scratch mape (ustvari jo); če izbrani disk ni dosegljiv (izklopljen SSD), privzeta mapa.
    public func scratch(_ sub: String) -> URL {
        let fm = FileManager.default
        for base in [scratchURL, Self.defaultScratch] {
            let dir = base.appendingPathComponent(sub, isDirectory: true)
            if (try? fm.createDirectory(at: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])) != nil {
                return dir
            }
        }
        return fm.temporaryDirectory.appendingPathComponent(sub, isDirectory: true)
    }

    /// Premakne znane podmape scratch mape v novo (tudi na drug disk). Obstoječega v cilju ne briše: napaka.
    /// Staro mapo odstrani le, če je prazna.
    public static func moveScratch(from old: URL, to new: URL) throws {
        let fm = FileManager.default
        let o = old.standardizedFileURL.path, n = new.standardizedFileURL.path
        guard o != n else { return }
        guard !n.hasPrefix(o + "/"), !o.hasPrefix(n + "/") else {
            throw CocoaError(.fileWriteInvalidFileName, userInfo: [NSLocalizedDescriptionKey: "The new folder is inside the old one or the other way round."])
        }
        let items = scratchSubfolders.filter { fm.fileExists(atPath: old.appendingPathComponent($0).path) }
        if let taken = items.first(where: { fm.fileExists(atPath: new.appendingPathComponent($0).path) }) {
            throw CocoaError(.fileWriteFileExists, userInfo: [NSLocalizedDescriptionKey: "The new folder already contains “\(taken)”."])
        }
        try fm.createDirectory(at: new, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        for name in items { try fm.moveItem(at: old.appendingPathComponent(name), to: new.appendingPathComponent(name)) }
        rmdir(old.path)  // le prazna
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let d = RouterConfig()
        host = try c.decodeIfPresent(String.self, forKey: .host) ?? d.host
        port = try c.decodeIfPresent(UInt16.self, forKey: .port) ?? d.port
        allowedClients = try c.decodeIfPresent(String.self, forKey: .allowedClients) ?? d.allowedClients
        allowPlainRemote = try c.decodeIfPresent(Bool.self, forKey: .allowPlainRemote) ?? d.allowPlainRemote
        providers = try c.decodeIfPresent([ProviderConfig].self, forKey: .providers) ?? d.providers
        claudeCLI = try c.decodeIfPresent(ClaudeCLIConfig.self, forKey: .claudeCLI) ?? d.claudeCLI
        appleFM = try c.decodeIfPresent(AppleFMConfig.self, forKey: .appleFM) ?? d.appleFM
        siri = try c.decodeIfPresent(SiriConfig.self, forKey: .siri) ?? d.siri
        devices = try c.decodeIfPresent([DeviceConfig].self, forKey: .devices) ?? d.devices
        // Nastavitve pred odobritvijo naprav (vsaka naprava je bila sprejeta in dodana): naštete ostanejo dovoljene, pri
        // praznem seznamu ostane dovoljena vsaka (brez dela za uporabnika).
        allowAnyDevice = try c.decodeIfPresent(Bool.self, forKey: .allowAnyDevice) ?? devices.isEmpty
        codex = try c.decodeIfPresent(CodexConfig.self, forKey: .codex) ?? d.codex
        copilot = try c.decodeIfPresent(CopilotConfig.self, forKey: .copilot) ?? d.copilot
        grok = try c.decodeIfPresent(GrokConfig.self, forKey: .grok) ?? d.grok
        foundry = try c.decodeIfPresent(FoundryConfig.self, forKey: .foundry) ?? d.foundry
        scratchFolder = try c.decodeIfPresent(String.self, forKey: .scratchFolder) ?? d.scratchFolder
        runCommands = try c.decodeIfPresent(String.self, forKey: .runCommands) ?? d.runCommands
        worktreeNetwork = try c.decodeIfPresent(Bool.self, forKey: .worktreeNetwork) ?? d.worktreeNetwork
        worktreeCodexNetwork = try c.decodeIfPresent(Bool.self, forKey: .worktreeCodexNetwork) ?? d.worktreeCodexNetwork
    }

    /// Siri ima osebni kontekst tega Maca: lokalno vedno, z drugih Macov le odobrene naprave.
    public func siriAllowed(peer: String, device: Noise.Device?) -> Bool {
        RouterServer.isLoopback(peer) || devices.contains { $0.id == device?.id && $0.siri }
    }

    /// Naprava s seznama; neznana le pri praznem seznamu z allowAnyDevice (kot deviceAllowed v minigw serve).
    public func deviceAllowed(_ id: String) -> Bool {
        devices.contains { $0.id == id } || (devices.isEmpty && allowAnyDevice)
    }

    /// Tristranska združitev za okno Settings ob spremembi od drugod (skrbniški API, nova naprava): kar uporabnik v
    /// osnutku ni spremenil (enako kot old), prevzame iz new; pri napravah in prehodih tudi dodane, odstranjene in
    /// drugje spremenjene elemente. Tako samodejna uveljavitev osnutka ne povozi sprememb skrbniškega API.
    public static func merge(draft: RouterConfig, old: RouterConfig, new: RouterConfig) -> RouterConfig {
        var d = draft
        func take<T: Equatable>(_ kp: WritableKeyPath<RouterConfig, T>) {
            if draft[keyPath: kp] == old[keyPath: kp] { d[keyPath: kp] = new[keyPath: kp] }
        }
        take(\.host); take(\.port); take(\.allowedClients); take(\.allowPlainRemote); take(\.allowAnyDevice); take(\.claudeCLI); take(\.appleFM)
        take(\.siri); take(\.codex); take(\.copilot); take(\.grok); take(\.foundry); take(\.scratchFolder); take(\.runCommands); take(\.worktreeNetwork); take(\.worktreeCodexNetwork)
        d.devices = mergeList(draft.devices, old.devices, new.devices)
        d.providers = mergeList(draft.providers, old.providers, new.providers)
        return d
    }

    static func mergeList<T: Equatable & Identifiable>(_ d: [T], _ o: [T], _ n: [T]) -> [T] {
        if d == o { return n }
        func find(_ a: [T], _ id: T.ID) -> T? { a.first { $0.id == id } }
        var out = d.filter { x in !(find(o, x.id) != nil && find(n, x.id) == nil) }  // drugje odstranjeni
        for i in out.indices {  // drugje spremenjeni, v osnutku ne
            if let ox = find(o, out[i].id), ox == out[i], let nx = find(n, out[i].id) { out[i] = nx }
        }
        out += n.filter { x in find(o, x.id) == nil && find(out, x.id) == nil }  // drugje dodani
        return out
    }

    public var allowedClientList: Set<String> {
        Set(allowedClients.split(whereSeparator: { $0 == "," || $0.isWhitespace }).map(String.init))
    }

    static let defaultsKey = "routerConfig"

    /// Preferences aplikacije (dev.trojar.minigateway) kot berljiva struktura plist.
    /// Starejša oblika (JSON blob) se prebere in ob naslednjem shranjevanju pretvori.
    public static func load(_ defaults: UserDefaults = .standard) -> RouterConfig {
        switch defaults.object(forKey: defaultsKey) {
        case let dict as [String: Any]:
            if let data = try? PropertyListSerialization.data(fromPropertyList: dict, format: .binary, options: 0),
               let c = try? PropertyListDecoder().decode(RouterConfig.self, from: data) { return c }
        case let data as Data:
            if let c = try? JSONDecoder().decode(RouterConfig.self, from: data) { return c }
        default:
            break
        }
        return RouterConfig()
    }

    public func save(_ defaults: UserDefaults = .standard) {
        guard let data = try? PropertyListEncoder().encode(self),
              let dict = try? PropertyListSerialization.propertyList(from: data, format: nil) else { return }
        defaults.set(dict, forKey: Self.defaultsKey)
    }
}

public protocol SecretStore: Sendable {
    func get(_ account: String) -> String?
}

public struct Keychain: SecretStore {
    public let service: String
    /// Le prijavni keychain (vnos »admin«): keychain, ki je na seznamu iskanja pred njim, vnosa ne more zasenčiti.
    public let loginOnly: Bool
    public init(service: String = "dev.trojar.minigateway", loginOnly: Bool = false) { self.service = service; self.loginOnly = loginOnly }

    /// Prijavni keychain po poti iz baze uporabnikov (getpwuid, ne HOME/CFFIXED_USER_HOME). Za aplikacijo Developer ID
    /// brez profila je to preprostejše kot keychain za zaščito podatkov (ta zahteva pravice dostopne skupine).
    static func loginKeychain() -> SecKeychain? {
        guard let pw = getpwuid(getuid()), let dir = pw.pointee.pw_dir else { return nil }
        let path = String(cString: dir) + "/Library/Keychains/login.keychain-db"
        guard FileManager.default.fileExists(atPath: path) else { return nil }
        var kc: SecKeychain?
        return SecKeychainOpen(path, &kc) == errSecSuccess ? kc : nil
    }

    /// nil = vnos je le v prijavnem keychainu, a tega ni (brez zamenjave z drugim keychainom).
    private func query(_ account: String, adding: Bool = false) -> [String: Any]? {
        var q: [String: Any] = [kSecClass as String: kSecClassGenericPassword,
                                kSecAttrService as String: service,
                                kSecAttrAccount as String: account]
        if loginOnly {
            guard let kc = Self.loginKeychain() else { return nil }
            if adding { q[kSecUseKeychain as String] = kc } else { q[kSecMatchSearchList as String] = [kc] }
        }
        return q
    }

    public func get(_ account: String) -> String? {
        guard var q = query(account) else { return nil }
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        guard SecItemCopyMatching(q as CFDictionary, &out) == errSecSuccess, let d = out as? Data,
              let v = String(data: d, encoding: .utf8), !v.isEmpty else { return Self.defaultKeys[account] }
        return v
    }

    /// Vrednost in status (errSecItemNotFound = vnosa ni; druga napaka = keychain zaklenjen ali brez dovoljenja).
    public func lookup(_ account: String) -> (value: String?, status: OSStatus) {
        guard var q = query(account) else { return (nil, errSecNoSuchKeychain) }
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        let st = SecItemCopyMatching(q as CFDictionary, &out)
        return ((out as? Data).flatMap { String(data: $0, encoding: .utf8) }, st)
    }

    /// Privzeti ključi, dokler uporabnik ne vpiše svojega (niso skrivnost; LiteLLM LITELLM_MASTER_KEY iz claude-local).
    public static let defaultKeys = ["litellm": "sk-local-1234"]

    public func delete(_ account: String) {
        if let q = query(account) { SecItemDelete(q as CFDictionary) }
    }

    public func set(_ account: String, _ value: String) {
        let data = Data(value.utf8)
        guard let q = query(account) else { return }
        if SecItemUpdate(q as CFDictionary, [kSecValueData as String: data] as CFDictionary) == errSecItemNotFound,
           var add = query(account, adding: true) {
            add[kSecValueData as String] = data
            SecItemAdd(add as CFDictionary, nil)
        }
    }
}
