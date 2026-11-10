import Foundation

/// Zgodovina naprav in sej (kot state.json v minigw serve): zadnji stik in priklopi dovoljenih naprav ter seje
/// odjemalcev s števci na model. Datoteka 0600 v Application Support; nikoli besedila pozivov ali odgovorov, žetonov
/// ali ključev. Zapis največ enkrat na minuto in ob koncu aplikacije (save).
public final class GatewayHistory: @unchecked Sendable {
    public struct Connection: Codable, Sendable, Equatable {
        public let time: Date
        public let address: String
        public let client: String
    }

    struct DeviceState: Codable {
        var lastSeen: Date
        var client = ""
        var history: [Connection] = []  // najstarejši prvi
        init(lastSeen: Date) { self.lastSeen = lastSeen }
        init(from decoder: Decoder) throws {  // manjkajoča polja (Go jih izpusti) = privzeta
            let c = try decoder.container(keyedBy: CodingKeys.self)
            lastSeen = try c.decode(Date.self, forKey: .lastSeen)
            client = try c.v(.client, client); history = try c.v(.history, history)
        }
    }

    /// Števci enega modela ali agenta v seji za eno vlogo in klicatelja (osnova za kasnejše omejitve); readOnly in
    /// tools: zadnje veljavne omejitve agenta (mapa samo za branje; orodja ali stikala agenta).
    public struct ModelUse: Codable, Sendable, Equatable {
        public var model = ""
        public var role = ""       // vloga klicatelja (--minigw-role)
        public var via = ""        // agent ali veriga, ki je klicala (agent-calls); "" = odjemalec
        public var readOnly = false
        public var tools = ""      // Claude CLI: orodja ali »local«; Codex, Grok, Copilot: write, network, commands
        public var cap = ""        // #45: omejitev načina (sync, worktree), zaradi katere je agent le bral
        public var requests = 0, errors = 0, refused = 0  // errors: 5xx in prekinjene; refused: 4xx
        public var seconds = 0.0                           // skupni čas zahtev (pri agentih čas izvajanja)
        public var inputTokens = 0, outputTokens = 0
        public init() {}
        public init(from decoder: Decoder) throws {  // manjkajoča polja (starejša datoteka, Go jih izpusti) = privzeta
            let c = try decoder.container(keyedBy: CodingKeys.self)
            model = try c.v(.model, model); role = try c.v(.role, role); via = try c.v(.via, via); readOnly = try c.v(.readOnly, readOnly)
            tools = try c.v(.tools, tools); cap = try c.v(.cap, cap); requests = try c.v(.requests, requests); errors = try c.v(.errors, errors)
            refused = try c.v(.refused, refused); seconds = try c.v(.seconds, seconds)
            inputTokens = try c.v(.inputTokens, inputTokens); outputTokens = try c.v(.outputTokens, outputTokens)
        }
    }

    /// #46b: števci enega agenta v mapi naprave za en dan UTC; ostanejo, ko seja odpade (osnova za omejitve). Le števila,
    /// nikoli besedila; žetone javi ponudnik (niso preverjeni). Kot projectUse v minigw serve.
    public struct ProjectUse: Codable, Sendable, Equatable {
        public var device = ""   // "" = ta računalnik brez ključa naprave
        public var folder = ""   // mapa seje: vgnezdeni klici štejejo pod projekt klicatelja
        public var os = ""
        public var day = ""      // UTC yyyy-MM-dd
        public var agent = ""    // claude, codex, grok, copilot ali other (ponudniki)
        public var requests = 0, errors = 0, refused = 0
        public var seconds = 0.0
        public var inputTokens = 0, outputTokens = 0
        public init() {}
        public init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            device = try c.v(.device, device); folder = try c.v(.folder, folder); os = try c.v(.os, os); day = try c.v(.day, day)
            agent = try c.v(.agent, agent); requests = try c.v(.requests, requests); errors = try c.v(.errors, errors)
            refused = try c.v(.refused, refused); seconds = try c.v(.seconds, seconds)
            inputTokens = try c.v(.inputTokens, inputTokens); outputTokens = try c.v(.outputTokens, outputTokens)
        }
    }

    public struct Session: Codable, Sendable, Identifiable {
        public var id: String           // x-claude-mini-session ali »t-…« (naprava in časovno okno)
        public var device = ""          // "" = ta računalnik brez ključa naprave
        public var deviceName = ""
        public var deviceRemoved = false
        public var start: Date
        public var last: Date
        public var folder = ""          // mapa odjemalca, normalizirana (folder(_:)): ključ za nastavitve po projektu
        public var os = ""              // OS odjemalca (macOS, Windows, Linux): mapa brez velikih črk pri macOS in Windows
        public var role = ""            // zadnja vloga klicatelja
        public var mode = "chat"        // chat, direct, sync, native, worktree (Go tudi refused)
        public var readOnly = false
        public var branch = ""
        public var models: [ModelUse] = []
        init(id: String, device: String, start: Date, last: Date) { self.id = id; self.device = device; self.start = start; self.last = last }
        public init(from decoder: Decoder) throws {  // manjkajoča polja (Go jih izpusti) = privzeta
            let c = try decoder.container(keyedBy: CodingKeys.self)
            id = try c.decode(String.self, forKey: .id); start = try c.decode(Date.self, forKey: .start); last = try c.decode(Date.self, forKey: .last)
            device = try c.v(.device, device); deviceName = try c.v(.deviceName, deviceName); deviceRemoved = try c.v(.deviceRemoved, deviceRemoved)
            folder = try c.v(.folder, folder); os = try c.v(.os, os); role = try c.v(.role, role); mode = try c.v(.mode, mode)
            readOnly = try c.v(.readOnly, readOnly); branch = try c.v(.branch, branch); models = try c.v(.models, models)
        }
    }

    /// Ena zahteva za model (brez besedila).
    public struct Use: Sendable {
        public var session = "", device = "", deviceName = "", folder = "", os = "", mode = "chat", branch = "", model = "", role = ""
        public var via = "", tools = "", cap = ""  // cap: #45 omejitev, ki jo je izvajanje uporabilo
        public var readOnly = false
        public var status = 200
        public var took = 0.0
        public var input = 0, output = 0
        public init() {}
    }

    struct State: Codable {
        var devices: [String: DeviceState] = [:]
        var sessions: [Session] = []  // po začetku, najstarejša prva
        var projects: [ProjectUse] = []  // #46b: po nastanku (dnevi naraščajo), najstarejša prva
        init() {}
        init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            devices = try c.v(.devices, devices); sessions = try c.v(.sessions, sessions); projects = try c.v(.projects, projects)
        }
    }

    static let historyPerDevice = 20, maxSessions = 200, maxSessionModels = 32
    static let maxDeviceSessions = 50  // M2: seje ene naprave (najprej odpadejo njene najstarejše)
    static let connectionGap: TimeInterval = 600  // priklop = prva zahteva po 10 minutah tišine; tudi meja seje brez ID-ja
    static let maxFile = 4 << 20
    static let projectDays = 31, maxProjects = 5000, maxDeviceProjects = 1250  // #46b: kot v minigw serve
    static let maxProjectFolder = 256  // L2: ključ mape v števcih (bajti UTF-8)

    private let lock = NSLock()
    private var state = State()
    private var file: URL?
    private var saveQueued = false, dirty = false
    private let saveLock = NSLock()  // L2: posnetek in zapis sta en zaporedni korak (časovnik, Clear History, izhod)

    public init() {}

    public static let defaultFile = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        .appendingPathComponent("Mini Gateway/state.json")

    private static let encoder: JSONEncoder = { let e = JSONEncoder(); e.dateEncodingStrategy = .iso8601; return e }()
    private static let decoder: JSONDecoder = { let d = JSONDecoder(); d.dateDecodingStrategy = .iso8601; return d }()

    /// Prebere datoteko (večje od 4 MB ne) in odslej piše vanjo.
    public func load(_ url: URL) {
        // L3: le navadna datoteka (attributesOfItem ne sledi povezavi), branje največ do meje
        var data: Data?
        if (try? FileManager.default.attributesOfItem(atPath: url.path)[.type] as? FileAttributeType) == .typeRegular,
           let h = try? FileHandle(forReadingFrom: url) {
            data = try? h.read(upToCount: Self.maxFile + 1)
            try? h.close()
        }
        let s = data.flatMap { $0.count <= Self.maxFile ? try? Self.decoder.decode(State.self, from: $0) : nil }
        lock.withLock { file = url; state = s ?? State() }
    }

    public func save() {
        saveLock.lock()
        defer { saveLock.unlock() }
        let (data, url): (Data?, URL?) = lock.withLock {
            saveQueued = false
            guard dirty, let file else { return (nil, nil) }
            dirty = false
            var data = try? Self.encoder.encode(state)
            while let d = data, d.count > Self.maxFile, !state.sessions.isEmpty || !state.projects.isEmpty {  // M1: datoteka nikoli čez mejo
                if !state.sessions.isEmpty {  // najprej seje; števci projektov naj ostanejo
                    state.sessions.removeFirst(max(1, state.sessions.count / 10))
                } else {  // L2: najprej najstarejše vrstice naprave z največ vrsticami
                    state.projects = Self.trimBusiest(state.projects)
                }
                data = try? Self.encoder.encode(state)
            }
            return (data, file)
        }
        guard let data, let url else { return }
        let tmp = url.deletingLastPathComponent().appendingPathComponent(".state-" + UUID().uuidString)
        try? FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        guard FileManager.default.createFile(atPath: tmp.path, contents: data, attributes: [.posixPermissions: 0o600]),
              rename(tmp.path, url.path) == 0 else { try? FileManager.default.removeItem(at: tmp); return }
    }

    /// Pod lock: sprememba; zapis čez največ minuto.
    private func changed() {
        dirty = true
        guard !saveQueued, file != nil else { return }
        saveQueued = true
        DispatchQueue.global().asyncAfter(deadline: .now() + 60) { [weak self] in self?.save() }
    }

    private static func second(_ d: Date) -> Date { Date(timeIntervalSince1970: d.timeIntervalSince1970.rounded(.down)) }
    /// M1, L4: brez kontrolnih znakov C0, C1, DEL, ločil vrstic in smernih kontrol; največ n bajtov UTF-8 (cel znak),
    /// enako kot clip v minigw serve.
    static func clip(_ s: String, _ n: Int) -> String {
        var out = String.UnicodeScalarView(), bytes = 0
        for u in s.unicodeScalars where !(u.value < 0x20 || (0x7f...0x9f).contains(u.value) || u.value == 0x2028 || u.value == 0x2029
                                           || (0x202a...0x202e).contains(u.value) || (0x2066...0x2069).contains(u.value)) {
            let len = String(u).utf8.count
            if bytes + len > n { break }
            out.append(u); bytes += len
        }
        return String(out)
    }

    /// ID seje, ki ga zgodovina sprejme (M2): do 128 bajtov, brez kontrolnih znakov; sicer zahteva ostane brez zapisa.
    static func validSession(_ s: String) -> Bool { s.utf8.count <= 128 && clip(s, 128) == s }

    /// Zahteva dovoljene naprave; priklop, če je od prejšnje minilo vsaj 10 minut.
    public func seen(_ id: String, address: String, client: String, now: Date = Date()) {
        let now = Self.second(now)
        lock.withLock {
            var d = state.devices[id] ?? DeviceState(lastSeen: .distantPast)
            if !client.isEmpty { d.client = client }
            if now.timeIntervalSince(d.lastSeen) >= Self.connectionGap {
                d.history.append(Connection(time: now, address: Self.clip(address, 64), client: Self.clip(d.client, 128)))
                d.history = Array(d.history.suffix(Self.historyPerDevice))
            }
            d.lastSeen = now
            state.devices[id] = d
            changed()
        }
    }

    public func lastSeen(_ id: String) -> Date? { lock.withLock { state.devices[id]?.lastSeen } }
    public func client(_ id: String) -> String? { lock.withLock { state.devices[id].flatMap { $0.client.isEmpty ? nil : $0.client } } }
    /// Priklopi, najnovejši prvi.
    public func history(_ id: String) -> [Connection] { lock.withLock { (state.devices[id]?.history ?? []).reversed() } }

    /// Zgodovina le dovoljenih naprav; seje odstranjenih ostanejo z oznako »device removed«.
    public func prune(allowed ids: Set<String>) {
        lock.withLock {
            let gone = state.devices.keys.filter { !ids.contains($0) }
            gone.forEach { state.devices[$0] = nil }
            var marked = false
            for i in state.sessions.indices where !state.sessions[i].device.isEmpty && !state.sessions[i].deviceRemoved
                && !ids.contains(state.sessions[i].device) {
                state.sessions[i].deviceRemoved = true
                marked = true
            }
            if !gone.isEmpty || marked { changed() }
        }
    }

    public func record(_ u: Use, now: Date = Date()) {
        // M2: neveljaven ID se ne bi ujemal s svojim zapisom; L5: zahteva vseeno šteje, v seji »t-…«
        let now = Self.second(now), sid = Self.validSession(u.session) ? u.session : ""
        lock.withLock {
            let i = state.sessions.lastIndex {
                $0.device == u.device && (sid.isEmpty
                    ? $0.id.hasPrefix("t-") && now.timeIntervalSince($0.last) < Self.connectionGap : $0.id == sid)
            }
            var s = i.map { state.sessions[$0] }
                ?? Session(id: sid.isEmpty ? "t-" + String(UInt64(now.timeIntervalSince1970 * 1e9), radix: 36) : sid,
                           device: u.device, start: now, last: now)
            s.last = now
            if !u.deviceName.isEmpty { s.deviceName = u.deviceName }
            if s.folder.isEmpty { s.folder = Self.clip(Self.folder(Self.clip(u.folder, 1 << 16)), 1024) }
            if !u.os.isEmpty { s.os = Self.clip(u.os, 16) }
            s.role = u.role  // zadnja vloga = vloga zadnje zahteve, tudi prazna
            if u.mode != "chat", !u.mode.isEmpty { s.mode = u.mode }
            s.readOnly = s.readOnly || u.readOnly
            if !u.branch.isEmpty { s.branch = Self.clip(u.branch, 255) }
            var key = ModelUse()
            key.model = Self.clip(u.model, 64); key.role = Self.clip(u.role, 32); key.via = Self.clip(u.via, 128)
            var j = s.models.firstIndex { $0.model == key.model && $0.role == key.role && $0.via == key.via }
            if j == nil, s.models.count >= Self.maxSessionModels {
                key = ModelUse(); key.model = "other"
                j = s.models.firstIndex { $0.model == "other" && $0.role.isEmpty && $0.via.isEmpty }
            }
            if j == nil { s.models.append(key); j = s.models.count - 1 }
            var m = s.models[j!]
            m.readOnly = u.readOnly; m.tools = Self.clip(u.tools, 256); m.cap = Self.clip(u.cap, 16)
            m.requests += 1
            if u.status >= 500 || u.status == 499 || u.status == 0 { m.errors += 1 } else if u.status >= 400 { m.refused += 1 }
            m.seconds += u.took
            m.inputTokens += u.input
            m.outputTokens += u.output
            s.models[j!] = m
            if let i { state.sessions[i] = s } else {
                // M2: naprava ne izrine drugih: najprej odpade njena najstarejša seja, nato najstarejša sploh
                if state.sessions.count(where: { $0.device == u.device }) >= Self.maxDeviceSessions,
                   let k = state.sessions.firstIndex(where: { $0.device == u.device }) {
                    state.sessions.remove(at: k)
                }
                state.sessions.append(s)
                state.sessions = Array(state.sessions.suffix(Self.maxSessions))
            }
            if !s.folder.isEmpty { Self.count(&state.projects, s, u, now) }
            changed()
        }
    }

    static let dayFormat: DateFormatter = {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX"); f.timeZone = TimeZone(identifier: "UTC"); f.dateFormat = "yyyy-MM-dd"
        return f
    }()
    static func day(_ d: Date) -> String { dayFormat.string(from: d) }

    /// #46b (pod lock): zahteva v števce projekta (naprava, mapa seje, dan UTC, agent). Nova vrstica: brez dni, starejših
    /// od 31; naprava največ maxDeviceProjects vrstic (najprej njena najstarejša), skupaj maxProjects. Kot count v Go.
    static func count(_ rows: inout [ProjectUse], _ s: Session, _ u: Use, _ now: Date) {
        let day = Self.day(now), agent = Self.agent(u.model) ?? "other", folder = Self.clip(s.folder, Self.maxProjectFolder)
        var i = rows.lastIndex { $0.day == day && $0.agent == agent && $0.device == s.device && $0.folder == folder }
        if i == nil {
            let oldest = Self.day(now.addingTimeInterval(-Double(Self.projectDays - 1) * 86400))
            rows.removeAll { $0.day < oldest }
            if rows.count(where: { $0.device == s.device }) >= Self.maxDeviceProjects, let k = rows.firstIndex(where: { $0.device == s.device }) {
                rows.remove(at: k)
            }
            var p = ProjectUse()
            p.device = s.device; p.folder = folder; p.os = s.os; p.day = day; p.agent = agent
            rows.append(p)
            if rows.count > Self.maxProjects { rows.removeFirst(rows.count - Self.maxProjects) }
            i = rows.count - 1
        }
        rows[i!].requests += 1
        if u.status >= 500 || u.status == 499 || u.status == 0 { rows[i!].errors += 1 } else if u.status >= 400 { rows[i!].refused += 1 }
        rows[i!].seconds += u.took
        rows[i!].inputTokens += u.input
        rows[i!].outputTokens += u.output
    }

    /// L2: brez desetine (vsaj ene) najstarejših vrstic naprave z največ vrsticami (kot trimBusiest v Go).
    static func trimBusiest(_ rows: [ProjectUse]) -> [ProjectUse] {
        let n = Dictionary(rows.map { ($0.device, 1) }, uniquingKeysWith: +)
        guard let busiest = n.max(by: { $0.value != $1.value ? $0.value < $1.value : $0.key > $1.key })?.key else { return rows }
        var drop = max(1, n[busiest]! / 10)
        return rows.filter { r in
            guard r.device == busiest, drop > 0 else { return true }
            drop -= 1
            return false
        }
    }

    /// Agent modela (kot agentOfModel v Go): claude, codex, grok, copilot; nil za druge modele.
    static func agent(_ m: String) -> String? {
        [("claude-cli-", "claude"), ("claude-chat-", "claude"), ("claude-codex", "codex"), ("claude-grok", "grok"), ("claude-copilot", "copilot")]
            .first { m.hasPrefix($0.0) }?.1
    }

    /// #46b: števci zadnjih `days` dni (danes je prvi), najnovejši dan prvi, nato mapa, naprava, agent (kot usage v Go).
    public func usage(device: String = "", folder: String = "", days: Int = 7, now: Date = Date()) -> [ProjectUse] {
        let from = Self.day(now.addingTimeInterval(-Double(days - 1) * 86400)), f = Self.folder(folder)
        return lock.withLock {
            state.projects.filter { $0.day >= from && (device.isEmpty || $0.device == device) && (folder.isEmpty || Self.inFolder($0.folder, f, os: $0.os)) }
        }.sorted { ($1.day, $0.folder, $0.device, $0.agent) < ($0.day, $1.folder, $1.device, $1.agent) }
    }

    /// Oblika za skrbniški API, enaka kot v minigw serve (prazna device in os izpuščena).
    static func json(_ p: ProjectUse) -> [String: Any] {
        var o: [String: Any] = ["folder": p.folder, "day": p.day, "agent": p.agent, "requests": p.requests, "errors": p.errors, "refused": p.refused,
                                "seconds": p.seconds, "inputTokens": p.inputTokens, "outputTokens": p.outputTokens]
        if !p.device.isEmpty { o["device"] = p.device }
        if !p.os.isEmpty { o["os"] = p.os }
        return o
    }

    /// Seje, najnovejša prva, modeli po modelu, vlogi in klicatelju; folder: ta mapa in njene podmape (inFolder).
    public func sessions(device: String = "", folder: String = "") -> [Session] {
        let f = Self.folder(folder)
        return lock.withLock {
            state.sessions.reversed().filter {
                (device.isEmpty || $0.device == device) && (folder.isEmpty || Self.inFolder($0.folder, f, os: $0.os))
            }.map {
                var s = $0
                s.models.sort { ($0.model, $0.role, $0.via) < ($1.model, $1.role, $1.via) }
                return s
            }
        }
    }

    /// Mapa odjemalca (kot normFolder v minigw serve): brez ».«, »..« in končnega ločila; poti Windows (C:\…,
    /// \\strežnik\…) z »/«; velike črke ostanejo.
    public static func folder(_ f: String) -> String {
        guard !f.isEmpty else { return "" }
        let win = f.range(of: #"^([A-Za-z]:([\\/]|$)|\\\\)"#, options: .regularExpression) != nil
        let p = win ? f.replacingOccurrences(of: "\\", with: "/") : f
        let abs = p.hasPrefix("/"), unc = p.hasPrefix("//")
        var out: [Substring] = []
        for c in p.split(separator: "/") where c != "." {
            if c == ".." {
                if let last = out.last, last != ".." { out.removeLast() } else if !abs { out.append(c) }
            } else {
                out.append(c)
            }
        }
        let joined = out.joined(separator: "/")
        return abs ? (unc ? "//" : "/") + joined : joined.isEmpty ? "." : joined
    }

    /// Mapa seje je f ali v njej; brez razlike v velikih črkah, če je odjemalec macOS ali Windows.
    static func inFolder(_ p: String, _ f: String, os: String) -> Bool {
        let fold = os == "macOS" || os == "Windows"
        let (p, f) = fold ? (p.lowercased(), f.lowercased()) : (p, f)
        return p == f || p.hasPrefix(f.hasSuffix("/") ? f : f + "/")
    }

    /// Veljavne omejitve agenta za zgodovino (kot agentRestrictions v minigw serve): Claude CLI orodja ali »local«;
    /// Codex, Grok, Copilot stikala write, network, commands ("" = le branje); drugi modeli "".
    /// `readOnly` (--minigw-ro): brez pisanja (Claude CLI brez orodij za pisanje oz. »local,read-only«; ukazi Copilot le s pisanjem).
    /// Codex: omrežje le s pisanjem (sandbox read-only ga nima), v worktree (`tree`, tudi native) le z worktreeCodexNetwork.
    public static func restrictions(_ model: String, _ c: RouterConfig, readOnly: Bool = false, tree: Bool = false) -> String {
        func agent(_ prefix: String) -> Bool { model == prefix || model.hasPrefix(prefix + "-") }
        func on(_ pairs: [(Bool, String)]) -> String { pairs.filter(\.0).map(\.1).joined(separator: ",") }
        let w = !readOnly
        if agent("claude-cli") {
            if c.claudeCLI.allowLocalTools { return readOnly ? "local,read-only" : "local" }
            return readOnly ? ClaudeCLIConfig.readOnlyTools(c.claudeCLI.tools) : c.claudeCLI.tools
        }
        if agent("claude-codex") {
            let write = c.codex.write && w
            return on([(write, "write"), (write && c.codex.network && (!tree || c.worktreeCodexNetwork), "network")])
        }
        if agent("claude-grok") { return on([(c.grok.write && w, "write")]) }
        if agent("claude-copilot") { return on([(c.copilot.write && w, "write"), (c.copilot.network, "network"), (c.copilot.commands && w, "commands")]) }
        return ""
    }

    /// Ime vloge (--minigw-role, glava x-claude-mini-role): 1-32 znakov iz črk, števk, . _ -.
    public static func validRole(_ s: String) -> Bool {
        (1...32).contains(s.count) && s.unicodeScalars.allSatisfy { $0.isASCII && (CharacterSet.alphanumerics.contains($0) || "._-".unicodeScalars.contains($0)) }
    }

    public func clearSessions() {
        lock.withLock { state.sessions = []; state.projects = []; changed() }  // tudi števci: eno dejanje lastnika
        save()
    }

    /// Oblika za skrbniški API, enaka kot v minigw serve (prazna polja izpuščena, trajanje v sekundah).
    static func json(_ s: Session) -> [String: Any] {
        let iso = ISO8601DateFormatter()
        var o: [String: Any] = ["id": s.id, "start": iso.string(from: s.start), "last": iso.string(from: s.last), "mode": s.mode,
                                "duration": s.last.timeIntervalSince(s.start),
                                "models": s.models.map { m -> [String: Any] in
                                    var o: [String: Any] = ["model": m.model, "requests": m.requests, "errors": m.errors, "refused": m.refused,
                                                            "seconds": m.seconds, "inputTokens": m.inputTokens, "outputTokens": m.outputTokens]
                                    if !m.role.isEmpty { o["role"] = m.role }
                                    if !m.via.isEmpty { o["via"] = m.via }
                                    if m.readOnly { o["readOnly"] = true }
                                    if !m.tools.isEmpty { o["tools"] = m.tools }
                                    if !m.cap.isEmpty { o["cap"] = m.cap }
                                    return o
                                }]
        if !s.device.isEmpty { o["device"] = s.device }
        if !s.deviceName.isEmpty { o["deviceName"] = s.deviceName }
        if s.deviceRemoved { o["deviceRemoved"] = true }
        if !s.folder.isEmpty { o["folder"] = s.folder }
        if !s.os.isEmpty { o["os"] = s.os }
        if !s.role.isEmpty { o["role"] = s.role }
        if s.readOnly { o["readOnly"] = true }
        if !s.branch.isEmpty { o["branch"] = s.branch }
        return o
    }
}

/// Šteje žetone iz odgovora (Anthropic JSON ali SSE: največji input_tokens in output_tokens); zadnjih 64 bajtov ostane
/// za število, ki ga razdeli meja zapisa.
final class UsageWriter: ResponseWriter, @unchecked Sendable {
    private let inner: ResponseWriter
    private let lock = NSLock()
    private var tail = Data()
    private var counts = (input: 0, output: 0)
    private static let re = try! NSRegularExpression(pattern: #""(input|output)_tokens"\s*:\s*(\d+)"#)

    init(_ inner: ResponseWriter) { self.inner = inner }

    var usage: (input: Int, output: Int) { lock.withLock { counts } }

    func write(_ data: Data) async throws {
        scan(data)
        try await inner.write(data)
    }

    func close() { inner.close() }

    func scan(_ data: Data) {
        lock.withLock {
            let buf = tail + data
            let s = String(decoding: buf, as: UTF8.self) as NSString
            for m in Self.re.matches(in: s as String, range: NSRange(location: 0, length: s.length)) {
                let n = Int(s.substring(with: m.range(at: 2))) ?? 0
                if s.substring(with: m.range(at: 1)) == "input" { counts.input = max(counts.input, n) } else { counts.output = max(counts.output, n) }
            }
            tail = Data(buf.suffix(64))
        }
    }
}

extension Router {
    public static let history = GatewayHistory()
}

extension GatewayHistory {
    /// Vloga iz glave x-claude-mini-role, preverjena (brez obrezovanja); neveljavna ali predolga = "". Pri vgnezdenem klicu jo glava nosi iz
    /// izvajanja (AgentRun.serve), zato je vprašani agent ne more spremeniti.
    static func role(_ req: HTTPRequest) -> String {
        let r = req.header("x-claude-mini-role") ?? ""
        return validRole(r) ? r : ""
    }

    /// --minigw-role <ime> ali --minigw-role=<ime> (zadnji velja) iz argumentov, sicer env; ok=false za neveljavno ime
    /// (enako kot takeRole v minigw za Windows in Linux).
    public static func takeRole(_ args: [String], env: String?) -> (role: String, rest: [String], ok: Bool) {
        var role = env ?? "", rest: [String] = [], i = 0, missing = false
        while i < args.count {
            if args[i] == "--minigw-role" {
                if i + 1 < args.count { role = args[i + 1]; i += 1 } else { missing = true }
            } else if args[i].hasPrefix("--minigw-role=") {
                role = String(args[i].dropFirst("--minigw-role=".count))
            } else {
                rest.append(args[i])
            }
            i += 1
        }
        return (role, rest, !missing && (role.isEmpty || validRole(role)))
    }
}

extension KeyedDecodingContainer {
    /// Vrednost ali privzeta, če ključa ni (datoteke drugih različic, Go izpusti prazna polja).
    func v<T: Decodable>(_ key: Key, _ d: T) throws -> T { try decodeIfPresent(T.self, forKey: key) ?? d }
}
