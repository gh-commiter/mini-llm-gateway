import CryptoKit
import Foundation
import os

/// Kar skrbniški API (docs/admin-api.md) potrebuje od aplikacije. Nastavitve so aplikacijine: `update` jih spremeni
/// prek istega `apply` kot okno Settings (shrani, znova zažene router), zato jih okno takoj prikaže. Ključi so v
/// Keychainu aplikacije.
public struct AdminHooks: Sendable {
    public var secret: String                      // skrivnost API (Keychain aplikacije, »admin-secret«)
    public var connect: String                     // »Copy Token«: žeton#ključ routerja
    public var version: String
    public var installed: @Sendable () -> Bool     // zagon ob prijavi
    public var setInstalled: (@Sendable (Bool) throws -> Void)?  // PATCH general {startAtSignIn}: Open at Login
    /// Sprememba nastavitev; ključi (račun → ključ, nil = izbriši) gredo v Keychain šele, ko f uspe, in pred ponovnim
    /// zagonom routerja (ta ob zagonu prebere ključe prehodov).
    public var commit: @Sendable (@escaping @Sendable (inout RouterConfig) throws -> Void, [String: String?]) async throws -> RouterConfig
    public var rotateToken: @Sendable () async -> String  // nova vrstica za odjemalce
    /// Proces odjemalca (vrata odjemalca, vrata routerja) → razlog za zavrnitev ali nil (PeerProcess.refusal).
    public var peerCheck: (@Sendable (UInt16, UInt16) -> String?)?
    /// L3: Scratch v rabi (agenti, registrirane mape); testi ga zamenjajo.
    public var scratchInUse: @Sendable () async -> Bool = { await Router.scratchInUse() }
    /// GET /activity: zgodovina okna Activity (najnovejša prva) in tekoče zahteve (id, začetek, model, zadnji korak).
    public var activity: @Sendable () async -> (requests: [RequestLog], inflight: [(id: String, time: Date, model: String, step: String?)]) = { ([], []) }

    public init(secret: String, connect: String, version: String, installed: @escaping @Sendable () -> Bool,
                setInstalled: (@Sendable (Bool) throws -> Void)? = nil,
                commit: @escaping @Sendable (@escaping @Sendable (inout RouterConfig) throws -> Void, [String: String?]) async throws -> RouterConfig,
                rotateToken: @escaping @Sendable () async -> String, peerCheck: (@Sendable (UInt16, UInt16) -> String?)? = nil) {
        self.secret = secret; self.connect = connect; self.version = version; self.installed = installed
        self.commit = commit; self.rotateToken = rotateToken; self.peerCheck = peerCheck; self.setInstalled = setInstalled
    }

    func update(keys: [String: String?] = [:], _ f: @escaping @Sendable (inout RouterConfig) throws -> Void) async throws -> RouterConfig {
        try await commit(f, keys)
    }

    /// HMAC-SHA256(skrivnost, "minigw-admin-v1" || h): odjemalec ne pošlje skrivnosti, le dokaz, vezan na rokovanje
    /// te povezave (lažen strežnik z lastnim ključem dobi dokaz, ki ne velja nikjer drugje).
    public static func proof(secret: String, hash: Data) -> String {
        Noise.base64url(Data(HMAC<SHA256>.authenticationCode(for: Data("minigw-admin-v1".utf8) + hash, using: SymmetricKey(data: Data(secret.utf8)))))
    }
    public static let scheme = "MGW-Admin "
}

struct AdminError: Error {
    let status: Int
    let message: String
    init(_ status: Int, _ message: String) { self.status = status; self.message = message }
}

/// Strogo telo JSON: objekt, le znana polja (podatkov za objektom JSONSerialization ne sprejme), pravilni tipi.
struct AdminBody {
    let o: [String: Any]

    init(_ data: Data, _ allowed: Set<String>) throws {
        guard let o = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] else {
            throw AdminError(400, "bad JSON body: expected one JSON object")
        }
        if let k = Self.duplicateKey(data) { throw AdminError(400, "bad JSON body: duplicate field \"\(k)\"") }
        if let k = o.keys.sorted().first(where: { !allowed.contains($0) }) { throw AdminError(400, "bad JSON body: unknown field \"\(k)\"") }
        self.o = o
    }

    /// Ključ, ki se v objektu na prvi ravni ponovi (JSONSerialization obdrži enega; Go jih zavrne).
    static func duplicateKey(_ data: Data) -> String? {
        let b = [UInt8](data)
        var depth = 0, i = 0, seen = Set<String>()
        while i < b.count {
            switch b[i] {
            case UInt8(ascii: "{"), UInt8(ascii: "["): depth += 1
            case UInt8(ascii: "}"), UInt8(ascii: "]"): depth -= 1
            case UInt8(ascii: "\""):
                var j = i + 1
                while j < b.count, b[j] != UInt8(ascii: "\"") { j += b[j] == UInt8(ascii: "\\") ? 2 : 1 }
                var k = j + 1
                while k < b.count, [9, 10, 13, 32].contains(b[k]) { k += 1 }
                if depth == 1, k < b.count, b[k] == UInt8(ascii: ":"), let key = (try? JSONSerialization.jsonObject(
                    with: Data(b[i...min(j, b.count - 1)]), options: .fragmentsAllowed)) as? String, !seen.insert(key).inserted {
                    return key
                }
                i = j
            default: break
            }
            i += 1
        }
        return nil
    }

    func has(_ k: String) -> Bool { o[k] != nil }
    func string(_ k: String) throws -> String? {
        guard let v = o[k] else { return nil }
        guard let s = v as? String else { throw AdminError(400, "\(k) must be a string") }
        return s
    }
    func bool(_ k: String) throws -> Bool? {
        guard let v = o[k] else { return nil }
        guard let n = v as? NSNumber, CFGetTypeID(n) == CFBooleanGetTypeID() else { throw AdminError(400, "\(k) must be true or false") }
        return n.boolValue
    }
    func int(_ k: String) throws -> Int? {
        guard let v = o[k] else { return nil }
        guard let n = v as? NSNumber, CFGetTypeID(n) != CFBooleanGetTypeID(), n.doubleValue == Double(n.intValue) else {
            throw AdminError(400, "\(k) must be a whole number")
        }
        return n.intValue
    }
    func double(_ k: String) throws -> Double? {
        guard let v = o[k] else { return nil }
        guard let n = v as? NSNumber, CFGetTypeID(n) != CFBooleanGetTypeID() else { throw AdminError(400, "\(k) must be a number") }
        return n.doubleValue
    }
    func strings(_ k: String) throws -> [String]? {
        guard let v = o[k] else { return nil }
        guard let a = v as? [String] else { throw AdminError(400, "\(k) must be a list of strings") }
        return a
    }
    /// null = vsi modeli; seznam = le ti; brez polja = brez spremembe (Optional<Optional>).
    func models(_ k: String) throws -> [String]?? {
        guard let v = o[k] else { return .none }
        if v is NSNull { return .some(nil) }
        guard let a = v as? [String] else { throw AdminError(400, "\(k) must be null or a list of strings") }
        return .some(a)
    }
}

extension Router {
    public static let adminPrefix = "/minigw/admin/v1"

    /// Le s tega računalnika (naslov povezave, ne glava), Host 127.0.0.1/localhost pri navadnem HTTP (DNS rebinding;
    /// brskalnik ne govori Noise), skrivnost v konstantnem času, brez CORS, telo do 1 MB.
    func adminHTTP(_ req: HTTPRequest, rid: String, writer: ResponseWriter) async -> Int {
        guard let admin else { return await adminError(404, "Not found: \(req.method) \(req.path)", rid: rid, writer: writer) }
        // Vrstni red: loopback → Host (le navaden HTTP) → dokaz MGW-Admin → proces odjemalca.
        guard RouterServer.isLoopback(req.peer) else {
            return await adminError(403, "The admin API is only available on the gateway computer", rid: rid, writer: writer)
        }
        let p = String(config.port)
        if !req.encrypted, !["127.0.0.1:" + p, "localhost:" + p, "[::1]:" + p].contains((req.header("host") ?? "").lowercased()) {
            return await adminError(403, "Host must be 127.0.0.1:\(p) or localhost:\(p)", rid: rid, writer: writer)
        }
        let auth = req.header("authorization") ?? ""
        guard auth.hasPrefix(AdminHooks.scheme), let h = req.noiseHash, !admin.secret.isEmpty else {
            return await adminError(401, "The admin API takes MGW-Admin over an encrypted connection (minigw gw), never the secret itself",
                                    rid: rid, writer: writer)
        }
        let a = Array(auth.dropFirst(AdminHooks.scheme.count).utf8), b = Array(AdminHooks.proof(secret: admin.secret, hash: h).utf8)
        guard a.count == b.count, zip(a, b).reduce(0, { $0 | ($1.0 ^ $1.1) }) == 0 else {
            return await adminError(401, "Invalid admin proof: use minigw gw", rid: rid, writer: writer)
        }
        // Obramba v globino: agent (otrok aplikacije ali proces v sandboxu) ne sme do API niti z dokazom.
        if let check = admin.peerCheck, let why = check(req.peerPort, config.port) {
            Self.log.error("admin API refused: \(why, privacy: .public)")
            return await adminError(403, why, rid: rid, writer: writer)
        }
        guard req.body.count <= 1 << 20 else { return await adminError(413, "Request body too large", rid: rid, writer: writer) }
        let (status, obj) = await adminDo(req.method, path: String(req.path.dropFirst(Self.adminPrefix.count)), query: req.query,
                                          body: req.body, hooks: admin)
        return await adminReply(status, obj, rid: rid, writer: writer)
    }

    /// Odgovor skrbniškega API: nikoli v predpomnilnik (vsebuje žeton, ključe naprav); brez piškotkov in CORS.
    func adminReply(_ status: Int, _ obj: Any, rid: String, writer: ResponseWriter) async -> Int {
        let body = (try? JSONSerialization.data(withJSONObject: obj, options: [.sortedKeys, .withoutEscapingSlashes])) ?? Data("{}".utf8)
        try? await writer.write(HTTPResponse.head(status: status, headers: [
            ("Content-Type", "application/json"), ("Cache-Control", "no-store"), ("request-id", rid),
        ], contentLength: body.count) + body)
        return status
    }

    func adminError(_ status: Int, _ message: String, rid: String, writer: ResponseWriter) async -> Int {
        await adminReply(status, ["type": "error", "error": ["type": Self.errorType(status), "message": message]], rid: rid, writer: writer)
    }

    func adminDo(_ method: String, path: String, query: String?, body: Data, hooks: AdminHooks) async -> (Int, Any) {
        do {
            return try await adminRoute(method, path: path, query: query, body: body, hooks: hooks)
        } catch let e as AdminError {
            return (e.status, ["type": "error", "error": ["type": Self.errorType(e.status), "message": e.message]])
        } catch {
            return (500, ["type": "error", "error": ["type": "api_error", "message": "\(error)"]])
        }
    }

    private func adminRoute(_ method: String, path: String, query: String?, body: Data, hooks: AdminHooks) async throws -> (Int, Any) {
        var seg = path.split(separator: "/", omittingEmptySubsequences: true).map { $0.removingPercentEncoding ?? String($0) }
        if seg.first == "providers" { seg[0] = "gateways" }  // /providers je kanonično ime, /gateways starejše (ostane)
        guard !seg.isEmpty, seg.count <= 3 else { throw AdminError(404, "Not found: \(method) \(path)") }
        let arg = seg.count > 1 ? seg[1] : ""
        let route = method + " " + seg[0] + (seg.count > 1 ? "/{}" : "") + (seg.count > 2 ? "/" + seg[2] : "")
        let c = config
        switch route {
        case "GET status":
            let list = await gateways(c)
            return (200, ["running": true, "version": hooks.version, "port": Int(c.port), "address": c.host, "installed": hooks.installed(),
                          "autostart": hooks.installed() ? "sign-in" : "", "agents": AgentCount.running.load(ordering: .relaxed),
                          "devices": c.devices.count, "pending": Router.deviceLog.pending.count, "claude": await claudeInfo(c.claudeCLI), "gateways": list, "providers": list]
                          .merging(c.copilot.enabled ? Copilot.login(copilotConfig).problem.map { ["copilot": $0] } ?? [:] : [:]) { $1 })  // #81

        // Naprave kot v minigw serve: dovoljene (z zadnjim stikom), čakajoče na odobritev, allowAnyDevice.
        case "GET devices":
            let iso = ISO8601DateFormatter()
            let list: [[String: Any]] = c.devices.map { d in
                var o: [String: Any] = ["id": d.id, "name": d.name, "siri": d.siri]
                if let t = history.lastSeen(d.id) { o["lastSeen"] = iso.string(from: t) }
                if let cl = history.client(d.id) { o["client"] = cl }
                return o
            }
            let pending: [[String: Any]] = Router.deviceLog.pending.map {
                ["id": $0.id, "name": $0.name, "address": $0.address, "client": $0.client, "time": iso.string(from: $0.time)]
            }
            return (200, ["devices": list, "pending": pending, "allowAnyDevice": c.allowAnyDevice])
        case "GET devices/{}/history":  // priklopi dovoljene naprave, najnovejši prvi
            guard let d = c.devices.first(where: { $0.id == arg }) else {
                throw AdminError(404, "no allowed device with the ID \(arg) (minigw gw devices lists the IDs)")
            }
            let iso = ISO8601DateFormatter()
            let h: [[String: Any]] = history.history(arg).map {
                var o: [String: Any] = ["time": iso.string(from: $0.time), "address": $0.address]
                if !$0.client.isEmpty { o["client"] = $0.client }
                return o
            }
            return (200, ["id": arg, "name": d.name, "history": h])
        case "GET activity":  // le zahteve modelom, največ 200 (kot minigw serve); nikoli vsebina
            let iso = ISO8601DateFormatter()
            let (logs, running) = await hooks.activity()
            let requests: [[String: Any]] = logs.filter { $0.model != nil }.prefix(200).map { e in
                var o: [String: Any] = ["time": iso.string(from: e.time), "method": e.method, "path": e.path, "model": e.model ?? "",
                                        "status": e.status, "ms": e.ms, "bytes": e.bytes ?? 0]
                if let d = e.device { o["device"] = d }
                if let n = e.deviceName { o["deviceName"] = n }
                return o
            }
            let inflight: [[String: Any]] = running.map { r in
                var o: [String: Any] = ["id": r.id, "time": iso.string(from: r.time), "model": r.model]
                if let s = r.step { o["step"] = s }
                return o
            }
            return (200, ["requests": requests, "inflight": inflight])
        case "GET sessions":
            let q = URLComponents(string: "?" + (query ?? ""))?.queryItems ?? []
            let v = { (n: String) in q.first { $0.name == n }?.value ?? "" }
            return (200, ["sessions": history.sessions(device: v("device"), folder: v("folder")).map(GatewayHistory.json)])
        case "GET sessions/{}":
            guard let s = history.sessions().first(where: { $0.id == arg }) else {
                throw AdminError(404, "no session \(arg) (minigw gw sessions lists them)")
            }
            return (200, GatewayHistory.json(s))
        case "GET usage":  // #46b: števci po projektu, najnovejši dan prvi
            let q = URLComponents(string: "?" + (query ?? ""))?.queryItems ?? []
            let v = { (n: String) in q.first { $0.name == n }?.value ?? "" }
            var days = 7
            if !v("days").isEmpty {
                guard let n = Int(v("days")), (1...GatewayHistory.projectDays).contains(n) else { throw AdminError(400, "days: 1-31") }
                days = n
            }
            return (200, ["usage": history.usage(device: v("device"), folder: v("folder"), days: days).map(GatewayHistory.json)])
        case "DELETE sessions":
            history.clearSessions()
            return (200, ["cleared": true])
        case "POST devices":
            let b = try AdminBody(body, ["id", "name"])
            let id = try b.string("id") ?? "", given = try b.string("name") ?? ""
            guard Self.validDeviceID(id) else {
                throw AdminError(400, "\"\(id)\" is not a device ID (43 characters, base64url): copy it from the client's minigw status")
            }
            let waiting = Router.deviceLog.pending.first { $0.id == id }
            let name = Noise.cleanName(given.isEmpty ? waiting?.name ?? "" : given)
            let added = !c.devices.contains { $0.id == id }
            _ = try await hooks.update { c in
                if !c.devices.contains(where: { $0.id == id }) { c.devices.append(DeviceConfig(id: id, name: name)) }
            }
            Router.deviceLog.removePending(id)
            return (200, ["device": ["id": id, "name": name], "added": added])
        case "DELETE devices/{}":
            if !c.devices.contains(where: { $0.id == arg }), let p = Router.deviceLog.removePending(arg) {  // zavrni čakajočo
                return (200, ["device": ["id": p.id, "name": p.name], "pending": true])
            }
            // Le po ID-ju: ime je oznaka, nikoli identiteta.
            guard let gone = c.devices.first(where: { $0.id == arg }) else {
                throw AdminError(404, "no allowed or waiting device with the ID \(arg) (minigw gw devices lists the IDs)")
            }
            _ = try await hooks.update { c in c.devices.removeAll { $0.id == gone.id } }
            return (200, ["device": ["id": gone.id, "name": gone.name]])
        case "PATCH devices/{}":  // preimenovanje dovoljene naprave (ime je le oznaka)
            let b = try AdminBody(body, ["name"])
            let name = Noise.cleanName(try b.string("name") ?? "")
            guard !name.isEmpty else { throw AdminError(400, "name: 1-64 printable characters") }
            guard c.devices.contains(where: { $0.id == arg }) else { throw AdminError(404, "no allowed device with the ID \(arg)") }
            _ = try await hooks.update { c in
                if let i = c.devices.firstIndex(where: { $0.id == arg }) { c.devices[i].name = name }
            }
            return (200, ["device": ["id": arg, "name": name]])

        case "GET siri":
            return (200, siriInfo(c))
        case "PATCH siri":
            let b = try AdminBody(body, ["enabled", "timeout", "allow", "deny"])
            let on = try b.bool("enabled"), timeout = try b.double("timeout"), allow = try b.strings("allow") ?? [], deny = try b.strings("deny") ?? []
            if let t = timeout, !(5...600).contains(t) { throw AdminError(400, "timeout must be 5-600 seconds") }
            let n = try await hooks.update { c in
                if let on { c.siri.enabled = on }
                if let timeout { c.siri.timeout = timeout }
                for (ids, v) in [(allow, true), (deny, false)] {
                    for id in ids {
                        guard let i = c.devices.firstIndex(where: { $0.id == id }) else { throw AdminError(400, "no device \(id)") }
                        c.devices[i].siri = v
                    }
                }
            }
            return (200, siriInfo(n))

        // minigw run: ukazi na tem Macu (izvajanje kode) le za vklopljene naprave in s seznama ukazov.
        case "GET run":
            return (200, runInfo(c))
        case "PATCH run":
            let b = try AdminBody(body, ["commands", "allow", "deny", "allowNetwork", "denyNetwork"])
            let commands = try b.string("commands"), allow = try b.strings("allow") ?? [], deny = try b.strings("deny") ?? []
            let allowNet = try b.strings("allowNetwork") ?? [], denyNet = try b.strings("denyNetwork") ?? []
            if let commands, commands.unicodeScalars.contains(where: { $0.value < 0x20 }) { throw AdminError(400, "commands: no control characters") }
            let n = try await hooks.update { c in
                if let commands { c.runCommands = commands }
                for (ids, v, net) in [(allow, true, false), (deny, false, false), (allowNet, true, true), (denyNet, false, true)] {
                    for id in ids {
                        guard let i = c.devices.firstIndex(where: { $0.id == id }) else { throw AdminError(400, "no device \(id)") }
                        if net { c.devices[i].runNetwork = v } else { c.devices[i].run = v }
                    }
                }
            }
            return (200, runInfo(n))

        case "GET gateways":
            let list = await gateways(c)
            return (200, ["providers": list, "gateways": list])
        case "POST gateways":
            let b = try AdminBody(body, ["name", "baseURL", "key", "insecure"])
            let name = try b.string("name") ?? "", key = try b.string("key") ?? ""
            guard Self.validProviderName(name) else { throw AdminError(400, "provider name: letters, digits, . _ - (up to 32)") }
            let base = try Self.checkGatewayURL(try b.string("baseURL") ?? "", insecure: try b.bool("insecure") ?? false)
            guard !key.isEmpty, key.unicodeScalars.allSatisfy({ (0x21...0x7e).contains($0.value) }) else {
                throw AdminError(400, "the API key must be printable ASCII without spaces")
            }
            let old = c.providers.first { $0.name == name }
            var p = old ?? ProviderConfig(id: "gw-" + UUID().uuidString.prefix(8).lowercased(), name: name, enabled: true, baseURL: base, aliases: "")
            p.baseURL = base
            p.insecure = try b.bool("insecure") ?? false
            p.enabled = true
            let models: [Discovery.Model]
            switch await Discovery.load(p, key: key, session: session) {
            case .success(let m): models = m
            case .failure(let f): throw AdminError(400, "provider not added: \(f.message)")
            }
            if old == nil, models.count > 20 { p.enabledModels = [] }  // OpenRouter: več sto modelov, izbereš sam
            let gw = p
            _ = try await hooks.update(keys: [p.id: key]) { c in  // ključ šele po uspešni spremembi, pred ponovnim zagonom
                if let i = c.providers.firstIndex(where: { $0.id == gw.id }) { c.providers[i] = gw } else { c.providers.append(gw) }
            }
            return (200, ["provider": gateway(p, ready: false), "gateway": gateway(p, ready: false), "keyStore": "the Keychain", "models": apiModels(p, models, filter: "")])
        case "PATCH gateways/{}":  // ime, vklop, URL (ključ ostane pod id), aliasi; kot minigw serve
            guard var p = c.providers.first(where: { $0.name == arg }) else { throw AdminError(404, "no provider \(arg)") }
            let b = try AdminBody(body, ["name", "enabled", "baseURL", "insecure", "aliases"])
            let newURL = try b.string("baseURL"), insecure = try b.bool("insecure")
            if newURL != nil || insecure != nil {  // kot Go: tudi sam »insecure« znova preveri URL
                p.baseURL = try Self.checkGatewayURL(newURL ?? p.baseURL, insecure: insecure ?? false)
            }
            if let e = try b.bool("enabled") { p.enabled = e }
            if let a = try b.string("aliases") {
                guard a.utf8.count <= 64 << 10 else { throw AdminError(400, "aliases: too long") }
                p.aliases = a
            }
            if let n = try b.string("name"), n != arg {
                guard !RouterConfig.builtInProviders.contains(p.id), !RouterConfig.builtInProviders.contains(n.lowercased()) else {
                    throw AdminError(400, "LiteLLM and OpenRouter are built in: they keep their names")
                }
                guard Self.validProviderName(n) else { throw AdminError(400, "provider name: letters, digits, . _ - (up to 32)") }
                p.name = n
            }
            let gw = p
            _ = try await hooks.update { c in  // L4: ime zasedeno? Preveri se na trenutnih nastavitvah, ne na posnetku.
                guard let i = c.providers.firstIndex(where: { $0.id == gw.id }) else { throw AdminError(404, "no provider \(arg)") }
                if c.providers.contains(where: { $0.id != gw.id && $0.name == gw.name }) { throw AdminError(400, "a provider named \(gw.name) already exists") }
                c.providers[i] = gw
            }
            return (200, ["provider": gateway(p, ready: false), "gateway": gateway(p, ready: false)])
        case "DELETE gateways/{}":
            guard let p = c.providers.first(where: { $0.name == arg }) else { throw AdminError(404, "no provider \(arg)") }
            guard !RouterConfig.builtInProviders.contains(p.id) else { throw AdminError(400, "\(p.name) is built in: turn it off instead") }
            _ = try await hooks.update(keys: [p.id: nil]) { c in c.providers.removeAll { $0.id == p.id } }
            return (200, ["name": arg])
        case "GET gateways/{}/models", "POST gateways/{}/models":
            guard var p = c.providers.first(where: { $0.name == arg }) else { throw AdminError(404, "no provider \(arg)") }
            let models: [Discovery.Model]
            switch await Discovery.load(p, key: secrets.get(p.id) ?? "", session: session) {
            case .success(let m): models = m
            case .failure(let f): throw AdminError(502, f.message)
            }
            if method == "GET" {
                let filter = URLComponents(string: "?" + (query ?? ""))?.queryItems?.first { $0.name == "filter" }?.value ?? ""
                return (200, ["models": apiModels(p, models, filter: filter)])
            }
            let b = try AdminBody(body, ["all", "enable", "disable"])
            let all = try b.bool("all"), enable = try b.strings("enable") ?? [], disable = try b.strings("disable") ?? []
            func upstream(_ v: String) throws -> String {
                guard let m = models.first(where: { $0.id == v || ProviderConfig.publishedID($0.id) == v }) else {
                    throw AdminError(400, "provider \(p.name) has no model \(v)")
                }
                return m.id
            }
            let before = p
            if let all { p.enabledModels = all ? nil : [] }
            for v in enable { let id = try upstream(v); if !p.isModelEnabled(id) { p.enabledModels = (p.enabledModels ?? []) + [id] } }
            if !disable.isEmpty {
                let off = try disable.map(upstream)
                p.enabledModels = models.map(\.id).filter { p.isModelEnabled($0) && !off.contains($0) }
            }
            let enabled = p.enabledModels, gw = p
            _ = try await hooks.update { c in
                guard let i = c.providers.firstIndex(where: { $0.id == gw.id }) else { throw AdminError(404, "no provider \(gw.name)") }
                c.providers[i].enabledModels = enabled
            }
            let now = apiModels(p, models, filter: "")
            let changed = now.filter { m in before.isModelEnabled(m["id"] as? String ?? "") != (m["enabled"] as? Bool ?? false) }
            return (200, ["models": now, "changed": changed])

        case "GET agents/{}":
            switch arg {
            case "claude": return (200, await claudeInfo(c.claudeCLI))
            case "codex": return (200, await codexInfo(c.codex))
            case "copilot": return (200, await copilotInfo(c.copilot))
            case "grok": return (200, await grokInfo(c.grok))
            default: throw AdminError(404, "no agent \(arg)")
            }
        case "GET agents/{}/tools":
            guard arg == "claude" else { throw AdminError(404, "no agent \(arg)") }
            try Self.scratchGuard(c)
            return (200, ["tools": await Self.toolList.get(c.claudeCLI.executable)])
        case "GET agents/{}/models":  // Codex, Grok: modeli računa (kot minigw serve)
            try Self.scratchGuard(c)
            switch arg {
            case "codex":
                guard let exe = await Codex.executable(c.codex.executable) else { return (200, ["models": [Any]()]) }
                let ms = await offPool { Codex.catalog(exe) }
                return (200, ["models": ms.map { m -> [String: Any] in
                    var o: [String: Any] = ["slug": m.slug, "name": m.name, "enabled": c.codex.isModelEnabled(m.slug)]
                    if !m.description.isEmpty { o["description"] = m.description }
                    if let w = m.window { o["window"] = w }
                    return o
                }])
            case "grok":
                guard let exe = await Grok.executable(c.grok.executable) else { return (200, ["models": [Any]()]) }
                let ms = await offPool { Grok.catalog(exe) }
                return (200, ["models": ms.map { ["slug": $0, "name": $0, "enabled": c.grok.enabledModels?.contains($0) ?? true] }])
            default: throw AdminError(404, "no agent \(arg)")
            }
        // Apple Foundation Models (pano v Settings): vklop in navodila.
        case "GET appleFM":
            return (200, ["enabled": c.appleFM.enabled, "instructions": c.appleFM.instructions])
        case "PATCH appleFM":
            let b = try AdminBody(body, ["enabled", "instructions"])
            let enabled = try b.bool("enabled"), instructions = try b.string("instructions")
            if let instructions, instructions.count > 8000 { throw AdminError(400, "instructions: up to 8000 characters") }
            let n = try await hooks.update { c in
                if let enabled { c.appleFM.enabled = enabled }
                if let instructions { c.appleFM.instructions = instructions }
            }
            return (200, ["enabled": n.appleFM.enabled, "instructions": n.appleFM.instructions])
        case "GET foundry":
            return (200, await foundryInfo(c.foundry))
        case "GET foundry/{}":
            guard arg == "models" else { throw AdminError(404, "Not found: \(method) \(path)") }
            do {
                let ms = try await Router.foundry.list(await Foundry.executable(c.foundry.executable))
                return (200, ["models": ms.map { ["name": $0.name, "vision": $0.vision, "enabled": c.foundry.isModelEnabled($0.name)] }])
            } catch { throw AdminError(503, (error as? Foundry.RequestError)?.message ?? "\(error)") }
        case "PATCH foundry":
            let b = try AdminBody(body, ["enabled", "path", "instructions", "models"])
            let enabled = try b.bool("enabled"), path = try b.string("path"), instructions = try b.string("instructions"), models = try b.models("models")
            if let path, !path.isEmpty, !FileManager.default.isExecutableFile(atPath: (path as NSString).expandingTildeInPath) {
                throw AdminError(400, "not an executable: \(path)")
            }
            if let instructions, instructions.count > 8000 { throw AdminError(400, "instructions: up to 8000 characters") }
            if let models, let list = models, let bad = list.first(where: { !Foundry.validModel($0) }) { throw AdminError(400, "not a model name: \(bad)") }
            let n = try await hooks.update { c in
                if let enabled { c.foundry.enabled = enabled }
                if let path { c.foundry.executable = path }
                if let instructions { c.foundry.instructions = instructions }
                if let models { c.foundry.enabledModels = models }
            }
            await Router.foundry.forget()
            return (200, await foundryInfo(n.foundry))
        case "PATCH agents/{}":
            switch arg {
            case "claude":
                let b = try AdminBody(body, ["enabled", "path", "tools", "folders", "allowLocalTools", "disabledTools", "permissionMode", "settingSources", "askOthers", "work"])
                let mode = try b.string("permissionMode"), sources = try b.string("settingSources").map { $0.replacingOccurrences(of: " ", with: "") }
                if let mode, !["dontAsk", "acceptEdits", "auto", "plan"].contains(mode) {
                    throw AdminError(400, "permission mode: dontAsk, acceptEdits, auto or plan")
                }
                if let sources, sources.range(of: "^((user|project|local)(,(user|project|local))*)?$", options: .regularExpression) == nil {
                    throw AdminError(400, "setting sources: comma-separated user, project, local (or empty)")
                }
                let enabled = try b.bool("enabled"), path = try b.string("path"), folders = try b.string("folders")
                let local = try b.bool("allowLocalTools"), disabled = try b.strings("disabledTools"), ask = try b.bool("askOthers")
                let tools = try b.string("tools").map { $0.replacingOccurrences(of: " ", with: "") }
                let work = try Self.work(b)
                if let t = tools, !t.isEmpty, t.range(of: #"^[A-Za-z0-9_]+(,[A-Za-z0-9_]+)*$"#, options: .regularExpression) == nil {
                    throw AdminError(400, "tools: comma-separated tool names, e.g. Read,Grep,Glob")
                }
                if let path, !path.isEmpty, case .failure(let e) = ClaudeCLI.validate(path) { throw AdminError(400, "claude: \(e)") }
                if let folders, let bad = folders.split(separator: ",").map(String.init).first(where: { RouterConfig.overlapsScratch($0, scratch: c.scratchURL) }) {
                    throw AdminError(400, "allowed folder \(bad.trimmingCharacters(in: .whitespaces)) contains or is inside the scratch folder \(c.scratchURL.path), or cannot be checked (agents could change the gateway's mirrors and other devices' copies): choose another folder")
                }
                if let folders, let bad = folders.split(separator: ",").map(String.init).first(where: { RouterConfig.overlapsCopilotCache($0) }) {
                    throw AdminError(400, "allowed folder \(bad.trimmingCharacters(in: .whitespaces)) contains or is inside Copilot's package cache \(RouterConfig.copilotCache.path) (agents could plant code the gateway runs): choose another folder")
                }
                let found = await ClaudeCLI.executable(path ?? config.claudeCLI.executable) != nil
                let n = try await hooks.update { c in
                    if let enabled { c.claudeCLI.enabled = enabled }
                    if let path { c.claudeCLI.executable = path }
                    if let tools { c.claudeCLI.tools = tools }
                    if let folders { c.claudeCLI.allowedRoots = folders }
                    if let local { c.claudeCLI.allowLocalTools = local }
                    if let disabled { c.claudeCLI.disabledTools = disabled }
                    if let mode { c.claudeCLI.permissionMode = mode }
                    if let sources { c.claudeCLI.settingSources = sources }
                    if let ask { c.claudeCLI.askOthers = ask }
                    if let work { c.claudeCLI.work = work }
                    if c.claudeCLI.enabled, !found {
                        throw AdminError(400, "claude not found: set path")
                    }
                }
                return (200, await claudeInfo(n.claudeCLI))
            case "copilot":
                // Kot codex; models = seznam imen (Copilot nima seznama modelov), commands = ukazi lupine (privolitev).
                // #81 (D1): brez žetona (le copilot login).
                let b = try AdminBody(body, ["enabled", "path", "write", "network", "commands", "models", "askOthers", "work"])
                let enabled = try b.bool("enabled"), path = try b.string("path"), write = try b.bool("write")
                let network = try b.bool("network"), commands = try b.bool("commands"), models = try b.models("models")
                let ask = try b.bool("askOthers"), work = try Self.work(b)
                if commands == true { throw AdminError(400, Copilot.noCommands) }  // #81 H1
                if let path, !path.isEmpty, !FileManager.default.isExecutableFile(atPath: path) { throw AdminError(400, "not an executable: \(path)") }
                if case let list?? = models, let bad = list.first(where: { !Self.validModel($0) }) { throw AdminError(400, "invalid model name: \(bad)") }
                let n = try await hooks.update { c in
                    if let enabled { c.copilot.enabled = enabled }
                    if let path { c.copilot.executable = path }
                    if let write { c.copilot.write = write }
                    if let network { c.copilot.network = network }
                    if let commands { c.copilot.commands = commands }
                    if let ask { c.copilot.askOthers = ask }
                    if let work { c.copilot.work = work }
                    if let models { c.copilot.models = (models ?? []).joined(separator: ", ") }  // null = le privzeti model
                }
                return (200, await copilotInfo(n.copilot))
            case "codex", "grok":
                let b = try AdminBody(body, arg == "codex" ? ["enabled", "path", "write", "network", "models", "askOthers", "work"] : ["enabled", "path", "write", "models", "askOthers", "work"])
                let enabled = try b.bool("enabled"), path = try b.string("path"), write = try b.bool("write")
                let network = try b.bool("network"), models = try b.models("models"), ask = try b.bool("askOthers"), work = try Self.work(b)
                if let path, !path.isEmpty, !FileManager.default.isExecutableFile(atPath: path) { throw AdminError(400, "not an executable: \(path)") }
                let n = try await hooks.update { c in
                    if arg == "codex" {
                        if let enabled { c.codex.enabled = enabled }
                        if let path { c.codex.executable = path }
                        if let write { c.codex.write = write }
                        if let network { c.codex.network = network }
                        if let models { c.codex.enabledModels = models }
                        if let ask { c.codex.askOthers = ask }
                        if let work { c.codex.work = work }
                    } else {
                        if let enabled { c.grok.enabled = enabled }
                        if let path { c.grok.executable = path }
                        if let write { c.grok.write = write }
                        if let models { c.grok.enabledModels = models }
                        if let ask { c.grok.askOthers = ask }
                        if let work { c.grok.work = work }
                    }
                }
                return (200, arg == "codex" ? await codexInfo(n.codex) : await grokInfo(n.grok))
            default:
                throw AdminError(404, "no agent \(arg)")
            }

        case "GET general":
            return (200, general(c, connect: hooks.connect))
        case "PATCH general":
            let b = try AdminBody(body, ["port", "address", "allowedClients", "allowPlainRemote", "allowAnyDevice", "allowLocalApps", "startAtSignIn", "scratch",
                                           "worktreeNetwork", "worktreeCodexNetwork"])
            let port = try b.int("port"), address = try b.string("address"), clients = try b.string("allowedClients")
            let plain = try b.bool("allowPlainRemote"), any = try b.bool("allowAnyDevice"), scratch = try b.string("scratch")
            let localApps = try b.bool("allowLocalApps"), wtNet = try b.bool("worktreeNetwork"), wtCodexNet = try b.bool("worktreeCodexNetwork")
            // L2: vse preverjeno pred kakršno koli spremembo (premik Scratch ali Open at Login)
            if let port, !(1...65535).contains(port) { throw AdminError(400, "port must be 1-65535") }
            if let address, !["0.0.0.0", "::", "127.0.0.1"].contains(address) {
                throw AdminError(400, "address: use 0.0.0.0, :: or 127.0.0.1, so that the admin API stays reachable on 127.0.0.1; limit clients with allowedClients")
            }
            if let clients {
                for ip in clients.split(whereSeparator: { $0 == "," || $0.isWhitespace }) where !Self.isIP(String(ip)) {
                    throw AdminError(400, "allowedClients: \(ip) is not an IP address")
                }
            }
            if any == true, !c.devices.isEmpty { throw AdminError(400, "allowAnyDevice: only while no device is allowed (remove the allowed devices first)") }
            // Scratch (kot Settings: vsebina gre v »Mini Gateway Scratch« v izbrani mapi; "" = privzeta). M1, L1: pravila
            // poti, shrani se kanonična pot; L3: ne med sejami.
            var newScratch: String?
            if let scratch {
                if !scratch.isEmpty {
                    let expanded = (scratch as NSString).expandingTildeInPath
                    guard expanded.hasPrefix("/") else { throw AdminError(400, "scratch folder: an absolute path") }
                    let url = URL(fileURLWithPath: expanded, isDirectory: true)
                    if let why = RouterConfig.scratchProblem(url, allowedRoots: c.claudeCLI.allowedRoots, name: c.instanceID) { throw AdminError(400, "scratch folder: \(why)") }
                    newScratch = Sync.real(url)?.path ?? url.path
                } else { newScratch = "" }
                if await hooks.scratchInUse() {
                    throw AdminError(409, "scratch folder: minigw sessions or agents are active; move it when no minigw session runs")
                }
            }
            if let on = try b.bool("startAtSignIn") {
                guard let set = hooks.setInstalled else { throw AdminError(400, "startAtSignIn: not available here") }
                do { try set(on) } catch { throw AdminError(500, "Open at Login: \(error.localizedDescription)") }
                if b.o.count == 1 { return (200, general(c, connect: hooks.connect)) }  // brez ponovnega zagona routerja
            }
            let moveTo = newScratch
            let n = try await hooks.update { c in
                // L2: premik v isti spremembi nastavitev (trenutna, ne posnetek); odločitev 3: <osnova>/<instanca>
                if let newScratch = moveTo, c.scratchTarget(newScratch.isEmpty ? nil : URL(fileURLWithPath: newScratch, isDirectory: true)).path != c.scratchURL.path {
                    let to = c.scratchTarget(newScratch.isEmpty ? nil : URL(fileURLWithPath: newScratch, isDirectory: true))
                    if !ScratchOwner.blocked.withLock({ $0 }) {  // N4: mapa drugega prehoda ostane; ta začne svojo
                        do { try RouterConfig.moveScratch(from: c.scratchURL, to: to) } catch { throw AdminError(500, "scratch folder: \(error.localizedDescription)") }
                    }
                    c.scratchFolder = newScratch
                    if !c.instanceID.isEmpty { c.scratchName = c.instanceID }
                }
                if let port { c.port = UInt16(port) }
                if let address { c.host = address }
                if let clients { c.allowedClients = clients }
                if let plain { c.allowPlainRemote = plain }
                if let any { c.allowAnyDevice = any }
                if let localApps { c.allowLocalApps = localApps }
                if let wtNet { c.worktreeNetwork = wtNet }  // Settings → General → Worktree mode
                if let wtCodexNet { c.worktreeCodexNetwork = wtCodexNet }
            }
            return (200, general(n, connect: hooks.connect))  // aplikacija znova zažene router: brez restartRequired
        case "POST general/{}/take":  // N4: Take over (Settings → General → Scratch folder; ScratchOwner.take)
            guard arg == "scratch" else { throw AdminError(404, "Not found: \(method) \(path)") }
            let name = body.isEmpty ? nil : try AdminBody(body, ["name"]).string("name")  // sosednja mapa v osnovi
            if await hooks.scratchInUse() {
                throw AdminError(409, "scratch folder: minigw sessions or agents are active; take it over when no minigw session runs")
            }
            let aside = OSAllocatedUnfairLock<String?>(initialState: nil)
            let n = try await hooks.update { c in
                do { let a = try ScratchOwner.take(&c, name: name); aside.withLock { $0 = a?.path } } catch let e as ScratchOwner.TakeError {
                    throw AdminError(e.status, e.message)
                }
            }
            var g = general(n, connect: hooks.connect)
            g["connect"] = nil
            if let a = aside.withLock({ $0 }) { g["warning"] = "this gateway's previous folder is kept as \(a)" }
            return (200, g)
        case "POST general/{}":
            guard arg == "token" else { throw AdminError(404, "Not found: \(method) \(path)") }
            return (200, general(c, connect: await hooks.rotateToken()))  // cela vrstica minigw connect, kot v Go
        default:
            throw AdminError(404, "Not found: \(method) \(path)")
        }
    }

    // MARK: oblike JSON (docs/admin-api.md)

    func claudeInfo(_ c: ClaudeCLIConfig) async -> [String: Any] {
        ["enabled": c.enabled, "path": c.executable, "found": await ClaudeCLI.executable(c.executable) != nil, "tools": c.tools,
         "folders": c.allowedRoots, "allowLocalTools": c.allowLocalTools, "disabledTools": c.disabledTools,
         "permissionMode": c.permissionMode, "settingSources": c.settingSources, "askOthers": c.askOthers,
         "work": Self.apiWork(c.work)]
    }

    func codexInfo(_ c: CodexConfig) async -> [String: Any] {
        ["enabled": c.enabled, "path": c.executable, "found": await Codex.executable(c.executable) != nil, "write": c.write,
         "network": c.network, "models": c.enabledModels.map { $0 as Any } ?? NSNull(), "askOthers": c.askOthers,
         "work": Self.apiWork(c.work)]
    }

    func copilotInfo(_ c: CopilotConfig) async -> [String: Any] {
        ["enabled": c.enabled, "path": c.executable, "found": await Copilot.executable(c.executable) != nil, "write": c.write,
         "network": c.network, "commands": false, "commandsUnavailable": Copilot.noCommands, "models": c.modelList, "askOthers": c.askOthers,
         "work": Self.apiWork(c.work)].merging(Self.copilotLogin(Copilot.login(copilotConfig).problem)) { $1 }
    }

    /// #81: vrstica Login (kot minigw serve: stanje in namig, nikoli vrednost) in loginProblem, ko Copilot ni na voljo.
    static func copilotLogin(_ problem: String?) -> [String: Any] {
        var o: [String: Any] = ["login": ["state": problem.map { "not available: " + $0 } ?? "signed in (copilot login)",
                                          "hint": "Copilot uses this user's own sign-in (`copilot login` on the gateway, as the gateway user); the token stays in the system keyring."]]
        if let problem { o["loginProblem"] = problem }
        return o
    }

    func foundryInfo(_ c: FoundryConfig) async -> [String: Any] {
        let exe = await Foundry.executable(c.executable)
        return ["enabled": c.enabled, "path": c.executable, "found": exe != nil, "exe": exe ?? "", "running": await Router.foundry.running,
                "error": await Router.foundry.lastError, "instructions": c.instructions, "models": c.enabledModels.map { $0 as Any } ?? NSNull()]
    }

    func grokInfo(_ c: GrokConfig) async -> [String: Any] {
        ["enabled": c.enabled, "path": c.executable, "found": await Grok.executable(c.executable) != nil, "write": c.write,
         "models": c.enabledModels.map { $0 as Any } ?? NSNull(), "askOthers": c.askOthers,
         "work": Self.apiWork(c.work)]
    }

    /// #45: omejitev načina agenta v API (direct, sync, worktree; v nastavitvah direct = ""), kot v minigw serve.
    static func work(_ b: AdminBody) throws -> String? {
        guard let w = try b.string("work") else { return nil }
        guard ["direct", "sync", "worktree"].contains(w) else { throw AdminError(400, "work: direct, sync or worktree") }
        return w == "direct" ? "" : w
    }

    static func apiWork(_ w: String) -> String { ["sync", "worktree"].contains(w) ? w : "direct" }

    func runInfo(_ c: RouterConfig) -> [String: Any] {
        ["commands": c.runCommands, "devices": c.devices.map { ["id": $0.id, "name": $0.name, "run": $0.run, "network": $0.runNetwork] }]
    }

    func siriInfo(_ c: RouterConfig) -> [String: Any] {
        ["enabled": c.siri.enabled, "timeout": c.siri.timeout,
         "devices": c.devices.map { ["id": $0.id, "name": $0.name, "siri": $0.siri] }]
    }

    /// N4: v mapi drugega prehoda nič (kot minigw serve: tudi poizvedbe orodij in modelov).
    static func scratchGuard(_ c: RouterConfig) throws {
        guard ScratchOwner.blocked.withLock({ $0 }) else { return }
        let st = ScratchOwner.state(c.scratchURL, id: c.instanceID)
        throw AdminError(503, ScratchOwner.message(c.scratchURL, st.isEmpty ? ScratchOwner.stale : st))
    }

    func general(_ c: RouterConfig, connect: String) -> [String: Any] {
        var g: [String: Any] = ["port": Int(c.port), "address": c.host, "allowedClients": c.allowedClients, "allowPlainRemote": c.allowPlainRemote,
         "scratch": c.scratchURL.path, "allowAnyDevice": c.allowAnyDevice, "allowLocalApps": c.allowLocalApps,
         "worktreeNetwork": c.worktreeNetwork, "worktreeCodexNetwork": c.worktreeCodexNetwork,
         "connect": "minigw connect \(c.host == "0.0.0.0" || c.host == "::" ? Self.localIP() : c.host):\(c.port) --token '\(connect)'"]
        if !c.instanceID.isEmpty, case let st = ScratchOwner.state(c.scratchURL, id: c.instanceID), !st.isEmpty { g["scratchState"] = st }  // N4: in-use, stale
        if !c.instanceID.isEmpty, case let o = ScratchOwner.siblings(c.scratchURL, id: c.instanceID), !o.isEmpty {
            g["scratchOthers"] = o.map { ["name": $0.name, "state": $0.state] }  // N4: mape drugih prehodov v osnovi
        }
        return g
    }

    func gateway(_ p: ProviderConfig, ready: Bool) -> [String: Any] {
        ["name": p.name, "baseURL": p.baseURL, "insecure": p.insecure, "enabled": p.enabled,
         "models": p.enabledModels.map { $0 as Any } ?? NSNull(), "hasKey": !(secrets.get(p.id) ?? "").isEmpty, "ready": ready]
    }

    func gateways(_ c: RouterConfig) async -> [[String: Any]] {
        let st = await catalog.status()
        return c.providers.map { gateway($0, ready: st[$0.id] ?? false) }
    }

    func apiModels(_ p: ProviderConfig, _ models: [Discovery.Model], filter: String) -> [[String: Any]] {
        models.filter { filter.isEmpty || ProviderConfig.publishedID($0.id).lowercased().contains(filter.lowercased()) }.map { m in
            var o: [String: Any] = ["id": m.id, "model": ProviderConfig.publishedID(m.id), "enabled": p.isModelEnabled(m.id)]
            if let w = m.window { o["window"] = w }
            return o
        }
    }

    // MARK: preverjanja (enaka kot v Go minigw serve)

    /// L3: Scratch uporabljajo agenti ali registrirane mape minigw (premik bi jim vzel kopije in klone).
    public static func scratchInUse() async -> Bool {
        if AgentCount.running.load(ordering: .relaxed) > 0 { return true }
        return await shares.hasSessions
    }

    /// L4: ime ponudnika kot na Go (validGatewayName): gre v imena modelov in poti API.
    static func validProviderName(_ n: String) -> Bool { n.range(of: "^[A-Za-z0-9._-]{1,32}$", options: .regularExpression) != nil }

    static func validDeviceID(_ id: String) -> Bool {
        id.count == 43 && Noise.fromBase64url(id).map { $0.count == 32 && Noise.base64url($0) == id } == true
    }

    /// https; http le do localhost ali zasebnega IP in le z insecure. Brez uporabnika, poizvedbe in /v1.
    static func checkGatewayURL(_ raw: String, insecure: Bool) throws -> String {
        let s = raw.trimmingCharacters(in: .whitespaces)
        guard let u = URLComponents(string: s), let host = u.host, !host.isEmpty, u.user == nil, u.password == nil,
              u.query == nil, u.fragment == nil else { throw AdminError(400, "invalid provider URL: \(raw)") }
        switch u.scheme {
        case "https": break
        case "http":
            // Lokalno: localhost, zasebni ali loopback IP, ime .local (mDNS); isto pravilo kot minigw serve. Ime brez pike ne:
            // iskalna domena DNS ga lahko naredi javnega.
            let h = host.lowercased()
            guard h == "localhost" || h.hasSuffix(".local") || isPrivateIP(host) else {
                throw AdminError(400, "provider URL must be https (http only to localhost, a private IP or a .local name, with insecure)")
            }
            guard insecure else { throw AdminError(400, "http sends the API key unencrypted: set insecure for a provider on localhost, a private IP or a .local name") }
        default:
            throw AdminError(400, "provider URL must be https: \(raw)")
        }
        var out = s
        while out.hasSuffix("/") { out.removeLast() }
        if out.hasSuffix("/v1") { out.removeLast(3) }
        return out
    }

    static func isPrivateIP(_ h: String) -> Bool {
        let host = h.trimmingCharacters(in: CharacterSet(charactersIn: "[]")).lowercased()
        if host == "::1" || host.hasPrefix("fc") || host.hasPrefix("fd") || host.hasPrefix("fe80:") { return isIP(host) }
        let o = host.split(separator: ".").compactMap { Int($0) }
        guard o.count == 4, o.allSatisfy({ (0...255).contains($0) }), isIP(host) else { return false }
        return o[0] == 10 || o[0] == 127 || (o[0] == 192 && o[1] == 168) || (o[0] == 172 && (16...31).contains(o[1])) || (o[0] == 169 && o[1] == 254)
    }

    static func isIP(_ s: String) -> Bool {
        var a = in_addr(), b = in6_addr()
        return inet_pton(AF_INET, s, &a) == 1 || inet_pton(AF_INET6, s, &b) == 1
    }

    /// Prvi naslov IPv4, ki ni loopback (za vrstico minigw connect).
    static func localIP() -> String {
        var ifaddr: UnsafeMutablePointer<ifaddrs>?
        guard getifaddrs(&ifaddr) == 0, let first = ifaddr else { return "<this-mac>" }
        defer { freeifaddrs(ifaddr) }
        for p in sequence(first: first, next: { $0.pointee.ifa_next }) {
            guard let sa = p.pointee.ifa_addr, sa.pointee.sa_family == UInt8(AF_INET) else { continue }
            var host = [CChar](repeating: 0, count: Int(NI_MAXHOST))
            guard getnameinfo(sa, socklen_t(sa.pointee.sa_len), &host, socklen_t(host.count), nil, 0, NI_NUMERICHOST) == 0 else { continue }
            let ip = String(cString: host)
            if !ip.hasPrefix("127.") { return ip }
        }
        return "<this-mac>"
    }
}
