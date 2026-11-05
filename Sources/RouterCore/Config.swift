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

    public init(id: String, name: String, enabled: Bool, baseURL: String, aliases: String, enabledModels: [String]? = nil) {
        self.id = id; self.name = name; self.enabled = enabled; self.baseURL = baseURL; self.aliases = aliases
        self.enabledModels = enabledModels
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

public struct RouterConfig: Codable, Sendable, Equatable {
    public var host = "127.0.0.1"
    public var port: UInt16 = 8765
    public var allowedClients = ""  // IP naslovi, ločeni s presledkom ali vejico; prazno = vsi
    public var providers: [ProviderConfig] = [
        ProviderConfig(id: "litellm", name: "LiteLLM", enabled: false,
                       baseURL: "http://localhost:4000", aliases: ""),  // prazno = modeli in okna, ki jih sporoči LiteLLM
        ProviderConfig(id: "openrouter", name: "OpenRouter", enabled: false,
                       baseURL: "https://openrouter.ai/api", aliases: "", enabledModels: []),  // več sto modelov: izbereš sam
    ]

    public var claudeCLI = ClaudeCLIConfig()
    public var appleFM = AppleFMConfig()
    public var siri = SiriConfig()

    public init() {}

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let d = RouterConfig()
        host = try c.decodeIfPresent(String.self, forKey: .host) ?? d.host
        port = try c.decodeIfPresent(UInt16.self, forKey: .port) ?? d.port
        allowedClients = try c.decodeIfPresent(String.self, forKey: .allowedClients) ?? d.allowedClients
        providers = try c.decodeIfPresent([ProviderConfig].self, forKey: .providers) ?? d.providers
        claudeCLI = try c.decodeIfPresent(ClaudeCLIConfig.self, forKey: .claudeCLI) ?? d.claudeCLI
        appleFM = try c.decodeIfPresent(AppleFMConfig.self, forKey: .appleFM) ?? d.appleFM
        siri = try c.decodeIfPresent(SiriConfig.self, forKey: .siri) ?? d.siri
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
    public init(service: String = "dev.trojar.minigateway") { self.service = service }

    private func query(_ account: String) -> [String: Any] {
        [kSecClass as String: kSecClassGenericPassword,
         kSecAttrService as String: service,
         kSecAttrAccount as String: account]
    }

    public func get(_ account: String) -> String? {
        var q = query(account)
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        guard SecItemCopyMatching(q as CFDictionary, &out) == errSecSuccess, let d = out as? Data,
              let v = String(data: d, encoding: .utf8), !v.isEmpty else { return Self.defaultKeys[account] }
        return v
    }

    /// Privzeti ključi, dokler uporabnik ne vpiše svojega (niso skrivnost; LiteLLM LITELLM_MASTER_KEY iz claude-local).
    public static let defaultKeys = ["litellm": "sk-local-1234"]

    public func set(_ account: String, _ value: String) {
        let data = Data(value.utf8)
        if SecItemUpdate(query(account) as CFDictionary, [kSecValueData as String: data] as CFDictionary) == errSecItemNotFound {
            var add = query(account)
            add[kSecValueData as String] = data
            SecItemAdd(add as CFDictionary, nil)
        }
    }
}
