import CryptoKit
import Foundation
import Synchronization

/// Tekoči procesi agentov (claude, codex, grok): `status.agents` (posodobitev jih ne prekine brez opozorila).
public enum AgentCount { public static let running = Atomic<Int>(0) }

/// Ponudnik `claude -p`: vgrajeni modeli Claude Code (opus/sonnet/haiku/fable), `claude-cli-<m>` (agent) in `claude-chat-<m>` (brez orodij).
public struct ClaudeCLIConfig: Codable, Sendable, Equatable {
    public var enabled = false
    public var executable = ""                       // prazno = samodejno iskanje
    public var allowedRoots = "~/Development"        // ločeno z vejico
    public var allowedRecords: [String: FolderRecord] = [:]  // M2: prava pot in istovetnost vsake dovoljene mape ob dodajanju (ključ: AllowedFolders.paths)
    public var tools = "Read,Grep,Glob"              // na voljo in samodejno dovoljena; prazno = brez orodij
    public var permissionMode = "dontAsk"            // brez vprašanj: kar ni dovoljeno, je zavrnjeno
    public var allowLocalTools = false               // vsa orodja Claude Code na tem Macu brez vprašanj; preglasi tools/permissionMode
    public var disabledTools: [String] = []          // izjeme pri allowLocalTools (tabela v nastavitvah)
    public var settingSources = ""                   // M2: brez nastavitev map (hooki iz dovoljene mape bi tekli zunaj sandboxa), kot Go
    public var askOthers = false          // »May ask other models«: agent vpraša drug model prehoda (AgentRun)
    public var work = ""                  // #45: najvišji način za pisanje (sync, worktree); "" = direct, vsi načini

    public init() {}

    /// Orodja z dostopom do datotek: dovoljena le v mapi seje in dovoljenih mapah (preverja Claude Code oz. sandbox).
    static let pathTools: Set<String> = ["Read", "Edit", "Write", "MultiEdit", "NotebookEdit", "Glob", "Grep", "LS", "Bash"]

    /// --minigw-ro: le ta orodja (seznam dovoljenih, da so nova orodja, ki pišejo, izključena); Bash le v sandboxu brez pisanja.
    static let readOnlyAllowed = ["Read", "Grep", "Glob", "LS", "WebSearch", "WebFetch"]
    /// Orodja, ki pišejo ali poganjajo ukaze mimo sandboxa Bash (EnterWorktree: git worktree add s hooki): pri --minigw-ro še izrecno prepovedana.
    static let writeTools = ["Edit", "Write", "MultiEdit", "NotebookEdit", "EnterWorktree", "ExitWorktree", "Monitor"]

    /// Seznam orodij le iz readOnlyAllowed (brez »Allow local tools« ni sandboxa, zato brez Bash); tudi oblike »WebFetch(domain:…)«.
    static func readOnlyTools(_ tools: String) -> String {
        tools.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }
            .filter { t in readOnlyAllowed.contains(String(t.prefix { $0 != "(" })) }
            .joined(separator: ",")
    }

    /// Orodja in dovoljenja agenta (claude -p ne more vprašati: kar ni dovoljeno, je zavrnjeno).
    /// Z »Allow local tools« ima agent vsa orodja, a datoteke le v `cwd` in dovoljenih mapah:
    /// Read/Edit/Write zunaj teh map niso dovoljeni, Bash teče v sandboxu Claude Code (brez branja domače mape,
    /// pisanje le v teh mapah, omrežje dovoljeno), izhoda iz sandboxa ni.
    /// `readOnly` (--minigw-ro): brez orodij za pisanje; Bash le v sandboxu brez pisanja v te mape (brez sandboxa ga ni).
    func agentArgs(cwd: URL, allTools: [String], scratch: URL? = nil, readOnly: Bool = false, folders: AllowedFolders? = nil, checkedPaths: [String]? = nil) -> [String] {
        guard allowLocalTools else {
            let t = readOnly ? Self.readOnlyTools(tools) : tools
            return ["--tools", t, "--allowedTools", t, "--permission-mode", permissionMode]
        }
        let checked = folders ?? AllowedFolders()
        if folders == nil { checked.record(allowedRoots) }
        let dirs = [cwd.path] + (checkedPaths ?? checked.folders(allowedRoots, excluding: cwd.path, scratch: scratch))

        let sandbox: [String: Any] = ["sandbox": [
            "enabled": true, "failIfUnavailable": true, "autoAllowBashIfSandboxed": true, "allowUnsandboxedCommands": false,
            "network": ["allowedDomains": ["*"]],
            // #26 (N2): paket Copilot nikoli za pisanje (naloži ga priprava paketa), tudi če je v dovoljeni mapi.
            // --minigw-ro: pisanje v te mape prepovedano (denyWrite velja tudi za delovno mapo, ki jo Claude Code dovoli sam).
            "filesystem": ["denyRead": ["~"] + (scratch.map(RouterConfig.hiddenScratch) ?? []), "allowRead": dirs, "allowWrite": readOnly ? [] : dirs,
                           "denyWrite": Self.scratchPaths(RouterConfig.copilotCache)
                               + (readOnly ? dirs : dirs.flatMap { d in Self.gitDenies.flatMap { [d + "/.git/" + $0, d + "/**/.git/" + $0] } + [d + "/**/.git"] })],  // native v mapi sami (H1); N3: tudi ugnezdeni (macOS glob)
        ]]
        let settings = String(decoding: try! JSONSerialization.data(withJSONObject: sandbox, options: [.sortedKeys]), as: UTF8.self)
        // --minigw-ro: --tools le iz seznama dovoljenih (in Bash v sandboxu brez pisanja), drugih orodij agent nima.
        let roTools = (Self.readOnlyAllowed + ["Bash"]).filter { !disabledTools.contains($0) }
        let others = allTools.filter { !Self.pathTools.contains($0) && !disabledTools.contains($0) && (!readOnly || roTools.contains($0)) }
        // N3: nastavitve, hooki in preusmeritve ugnezdenih .git tudi ne z orodji Edit/Write (kot regex v Native.sandboxProfile)
        let nestedGit = readOnly ? [] : dirs.flatMap { d in Self.gitDenies.flatMap { g in ["Edit", "Write"].map { "\($0)(/\(d)/**/.git/\(g))" } + ["Edit", "Write"].map { "\($0)(/\(d)/**/.git/\(g)/**)" } }
            + ["Edit", "Write"].map { "\($0)(/\(d)/**/.git)" } }  // tudi datoteka gitdir: (podmodul) ali .git sam
        let denied = disabledTools + (readOnly ? Self.writeTools.filter { !disabledTools.contains($0) } : []) + nestedGit
        return ["--tools", readOnly ? roTools.joined(separator: ",") : "default", "--permission-mode", "acceptEdits", "--settings", settings]
            + dirs.dropFirst().flatMap { ["--add-dir", $0] }
            + (others.isEmpty ? [] : ["--allowedTools", others.joined(separator: ",")])
            + (denied.isEmpty ? [] : ["--disallowedTools", denied.joined(separator: ",")])
    }

    /// M4: .git, ki ga bere git lastnika: nastavitve, hooki, atributi in preusmeritve na drug gitdir (commondir,
    /// povezani worktree-ji z gitdir, commondir in config.worktree).
    static let gitDenies = ["config", "config.worktree", "hooks", "info", "commondir", "worktrees"]

    /// --minigw-work=worktree: vsa orodja (tudi Bash) vedno v peskovniku Claude Code (brez stikala za izklop):
    /// pisanje le v klon in predpomnilnike gradnje (Worktree.writable), branje domače mape le tam, omrežje po nastavitvi.
    /// denyWrite: .git klona (config, hooks, info, commondir) in nastavitve gradnje lastnika (Native.buildCacheDenies).
    /// Iz ~/.m2 le branje settings.xml in toolchains.xml (Maven); pisanje le v svoj lokalni repozitorij (MAVEN_OPTS).
    /// `read`: objekti, ki jih git v klonu bere prek alternates (zrcalo v Scratch, pri native repozitorij uporabnika).
    /// `gits`: .git vseh klonov (vrhnji in ugnezdeni); prazno = le vrhnji.
    func worktreeArgs(root: URL, maven: URL, cwd: URL, allTools: [String], network: Bool, scratch: URL? = nil, read: [URL] = [],
                      gits: [String] = []) -> [String] {
        let dirs = Worktree.writable(root: root, maven: maven)
        let sandbox: [String: Any] = ["sandbox": [
            "enabled": true, "failIfUnavailable": true, "autoAllowBashIfSandboxed": true, "allowUnsandboxedCommands": false,
            "network": ["allowedDomains": network ? ["*"] : []],
            // Nič iz DARWIN_USER_TEMP_DIR (peskovnik Claude Code ne zna »le znotraj mape«): swift build in xcodebuild
            // agenta lahko ne uspeta (gradnja z minigw run ali v načinu sync); Maven deluje.
            "filesystem": ["denyRead": ["~"] + (scratch.map(RouterConfig.hiddenScratch) ?? []), "allowRead": dirs + Self.mavenConfig() + read.map(\.path), "allowWrite": dirs,
                           "denyWrite": (gits.isEmpty ? [root.path + "/.git"] : gits).flatMap { g in ["config", "hooks", "info", "commondir"].map { g + "/" + $0 } }
                               + [(Worktree.tmpLink(root) as NSString).deletingLastPathComponent]  // #67 M2: koren povezav TMPDIR
                               + Native.buildCacheDenies().paths + Self.scratchPaths(RouterConfig.copilotCache)],
        ]]
        let settings = String(decoding: try! JSONSerialization.data(withJSONObject: sandbox, options: [.sortedKeys]), as: UTF8.self)
        let others = allTools.filter { !Self.pathTools.contains($0) && !disabledTools.contains($0) }
        // Urejanje .git (tudi ugnezdenega) z orodji Edit/Write ne: nastavitve in hooke bere git, ki ga poganja sam claude.
        let gitEdits = ["/.git", "/.git/**", "/**/.git", "/**/.git/**"].map { "Edit(/" + root.path + $0 + ")" }
        return ["--tools", "default", "--permission-mode", "acceptEdits", "--settings", settings]
            + (cwd.path == root.path ? [] : ["--add-dir", root.path])
            + (others.isEmpty ? [] : ["--allowedTools", others.joined(separator: ",")])
            + ["--disallowedTools", (disabledTools + gitEdits).joined(separator: ",")]
    }

    /// M1: Scratch (kopije drugih naprav, zrcala) dobesedno in kanonično v denyRead: tudi zunaj domače mape (zunanji SSD).
    /// Mapa seje in klon v njem ostaneta berljiva (allowRead je natančnejši). denyWrite ne: zapisovati je dovoljeno le v
    /// allowWrite (mapa seje, klon, dovoljene mape), Scratch pa ne sme biti v dovoljeni mapi (scratchProblem).
    static func scratchPaths(_ scratch: URL?) -> [String] {
        guard let scratch else { return [] }
        return Array(Set([scratch.path, Sync.real(scratch)?.path].compactMap { $0 })).sorted()
    }

    /// ~/.m2/settings.xml in toolchains.xml (dobesedno in kanonično, če sta povezavi).
    static func mavenConfig(home: String = NSHomeDirectory()) -> [String] {
        ["settings.xml", "toolchains.xml"].flatMap { f -> [String] in
            let p = home + "/.m2/" + f
            return Sync.real(URL(fileURLWithPath: p)).map { $0.path == p ? [p] : [p, $0.path] } ?? [p]
        }
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let d = ClaudeCLIConfig()
        enabled = try c.decodeIfPresent(Bool.self, forKey: .enabled) ?? d.enabled
        executable = try c.decodeIfPresent(String.self, forKey: .executable) ?? d.executable
        allowedRoots = try c.decodeIfPresent(String.self, forKey: .allowedRoots) ?? d.allowedRoots
        allowedRecords = try c.decodeIfPresent([String: FolderRecord].self, forKey: .allowedRecords) ?? d.allowedRecords
        tools = try c.decodeIfPresent(String.self, forKey: .tools) ?? d.tools
        permissionMode = try c.decodeIfPresent(String.self, forKey: .permissionMode) ?? d.permissionMode
        allowLocalTools = try c.decodeIfPresent(Bool.self, forKey: .allowLocalTools) ?? d.allowLocalTools
        disabledTools = try c.decodeIfPresent([String].self, forKey: .disabledTools) ?? d.disabledTools
        settingSources = try c.decodeIfPresent(String.self, forKey: .settingSources) ?? d.settingSources
        askOthers = try c.decodeIfPresent(Bool.self, forKey: .askOthers) ?? d.askOthers
        work = try c.decodeIfPresent(String.self, forKey: .work) ?? d.work
        if settingSources == "project,local" { settingSources = "" }  // M2: stara privzeta vrednost → nova; izrecna druga ostane
    }
}

public enum ClaudeCLI {
    static let families = ["opus", "sonnet", "haiku", "fable"]
    static let chatPrefix = "claude-chat-"  // brez orodij; ni več v seznamu, ostane zaradi shranjenih izbir
    static let agentPrefix = "claude-cli-"  // agent z orodji v mapi seje (tudi priklopljeni mapi z drugega Maca)

    /// Model za `claude --model` in ali je način vsiljeno »chat« (brez orodij).
    static func match(_ model: String) -> (cliModel: String, chatOnly: Bool)? {
        for (prefix, chat) in [(chatPrefix, true), (agentPrefix, false)] where model.hasPrefix(prefix) {
            let m = String(model.dropFirst(prefix.count))
            return families.contains(where: { m.hasPrefix($0) || m.hasPrefix("claude-\($0)") }) ? (m, chat) : nil
        }
        if families.contains(model) { return (model, false) }
        return families.contains(where: { model.hasPrefix("claude-\($0)") }) ? (model, false) : nil
    }

    struct Turn: Equatable, Sendable {
        let role: String
        let text: String
    }

    static func text(of content: Any?) -> String {
        if let s = content as? String { return s }
        guard let blocks = content as? [[String: Any]] else { return "" }
        return blocks.compactMap { b -> String? in
            switch b["type"] as? String {
            case "text": b["text"] as? String
            case "tool_use": "[tool_use \(b["name"] as? String ?? "")]"
            case "tool_result": "[tool_result] " + text(of: b["content"])
            default: nil  // slike, thinking …
            }
        }.joined(separator: "\n")
    }

    /// Samo user/assistant: Claude Code med sporočila vriva tudi `system` (okolje, datum …),
    /// ki se spreminjajo in niso del pogovora.
    static func turns(_ body: [String: Any]) -> [Turn] {
        ((body["messages"] as? [[String: Any]]) ?? []).filter { ["user", "assistant"].contains($0["role"] as? String) }.map {
            Turn(role: $0["role"] as? String ?? "", text: text(of: $0["content"]).trimmingCharacters(in: .whitespacesAndNewlines))
        }
    }

    /// Ključ transkripta: ista zgodovina v isti zunanji seji → ista notranja seja.
    static func key(_ turns: some Collection<Turn>, scope: String) -> String {
        var h = SHA256()
        h.update(data: Data(scope.utf8))
        for t in turns { h.update(data: Data("\u{1}\(t.role)\u{2}\(t.text)".utf8)) }
        return h.finalize().map { String(format: "%02x", $0) }.joined()
    }

    /// Nova notranja seja dobi dosedanji pogovor kot kontekst.
    static func prompt(history: some Collection<Turn>, last: Turn) -> String {
        guard !history.isEmpty else { return last.text }
        let past = history.map { "\($0.role == "assistant" ? "Assistant" : "User"): \($0.text)" }.joined(separator: "\n\n")
        return "Earlier conversation (continued from another session):\n\n\(past)\n\n---\n\n\(last.text)"
    }

    /// Nastavljena pot ali, če je prazna, samodejno najdena.
    static func executable(_ configured: String) async -> String? {
        configured.isEmpty ? await Detected.path("claude") : [(configured as NSString).expandingTildeInPath].first { FileManager.default.isExecutableFile(atPath: $0) }
    }

    /// Ali je datoteka res Claude Code: izvršljiva in `--version` izpiše »Claude Code«.
    /// Vrne različico ali razlog zavrnitve (v angleščini; vmesnik ga prikaže kot je).
    public static func validate(_ path: String) -> Result<String, ValidationError> {
        let path = (path as NSString).expandingTildeInPath
        guard FileManager.default.fileExists(atPath: path) else { return .failure(.init("File does not exist")) }
        guard FileManager.default.isExecutableFile(atPath: path) else { return .failure(.init("File is not executable")) }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: path)
        p.arguments = ["--version"]
        let out = Pipe()
        p.standardOutput = out
        p.standardError = out
        p.standardInput = FileHandle.nullDevice
        guard (try? p.run()) != nil else { return .failure(.init("Cannot run file")) }
        let killer = DispatchWorkItem { Native.killTree(p.processIdentifier, pipes: [out.fileHandleForReading]) }  // #73: tudi vnuk s cevjo
        DispatchQueue.global().asyncAfter(deadline: .now() + 5, execute: killer)
        let text = String(decoding: out.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
            .trimmingCharacters(in: .whitespacesAndNewlines)
        killer.cancel()
        guard text.contains("Claude Code") else { return .failure(.init("Not a Claude Code executable")) }
        return .success(text.split(whereSeparator: \.isNewline).last.map(String.init) ?? text)
    }

    /// Vgrajena orodja te različice Claude Code: prva vrstica `claude -p` (init) jih našteje, nato proces ustavimo,
    /// še preden pošlje zahtevo modelu.
    public static func availableTools(_ configured: String) async -> [String] {
        guard let exe = await executable(configured) else { return [] }
        return await offPool {
            let p = Process()
            p.executableURL = URL(fileURLWithPath: exe)
            p.arguments = ["-p", "x", "--model", "haiku", "--output-format", "stream-json", "--verbose", "--tools", "default",
                           "--setting-sources", "", "--strict-mcp-config", "--no-session-persistence"]
            p.currentDirectoryURL = FileManager.default.temporaryDirectory
            let out = Pipe()
            p.standardOutput = out
            p.standardError = FileHandle.nullDevice
            p.standardInput = FileHandle.nullDevice
            guard (try? p.run()) != nil else { return [] }
            // #73: drevo (tudi vnuk s cevjo), ne le claude: sicer branje spodaj obvisi
            let killer = DispatchWorkItem { Native.killTree(p.processIdentifier, pipes: [out.fileHandleForReading]) }
            DispatchQueue.global().asyncAfter(deadline: .now() + 15, execute: killer)
            defer { killer.cancel(); Native.killTree(p.processIdentifier, pipes: [out.fileHandleForReading]) }
            var line = Data()
            while let b = try? out.fileHandleForReading.read(upToCount: 1), let c = b.first, c != 10 { line.append(c) }
            let tools = (try? JSONSerialization.jsonObject(with: line) as? [String: Any])?["tools"] as? [String]
            return (tools ?? []).sorted()
        }
    }

    public struct ValidationError: Error, LocalizedError {
        public let message: String
        init(_ m: String) { message = m }
        public var errorDescription: String? { message }
    }

    /// `command -v claude` v uporabnikovi prijavni lupini (njegov PATH), nato običajna mesta namestitve.
    /// Za nastavitve (glavna nit); router: `await executable("")`.
    public static var detected: String? { Detected.pathNow("claude") }

    /// `command -v <name>` v prijavni lupini, nato običajna mesta namestitve.
    static func locate(_ name: String, also extra: [String] = []) -> String? {
        let shell = ProcessInfo.processInfo.environment["SHELL"] ?? "/bin/zsh"
        let p = Process()
        p.executableURL = URL(fileURLWithPath: shell)
        p.arguments = ["-lic", "command -v \(name)"]
        let out = Pipe()
        p.standardOutput = out
        p.standardError = FileHandle.nullDevice
        p.standardInput = FileHandle.nullDevice
        var found: [String] = []
        if (try? p.run()) != nil {
            let text = String(decoding: out.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
            found = text.split(whereSeparator: \.isNewline).map(String.init).filter { $0.hasPrefix("/") }.reversed()
        }
        let home = NSHomeDirectory()
        let candidates = found + ["\(home)/.local/bin/\(name)", "/opt/homebrew/bin/\(name)", "/usr/local/bin/\(name)"] + extra
        return candidates.first { FileManager.default.isExecutableFile(atPath: $0) }
    }

    static func expand(_ path: String) -> String {
        URL(fileURLWithPath: (path as NSString).expandingTildeInPath).standardizedFileURL.resolvingSymlinksInPath().path
    }

    /// Delovna mapa mora obstajati in biti pod dovoljenimi korenskimi mapami, nikoli v Scratch (privzeti ali `scratch`:
    /// tam so kopije drugih naprav in stanje agentov, ki ga peskovniki skrijejo; R3) in se ne prekriva s paketom Copilot.
    /// Kanonični poti (realpath, tudi »/private«), da povezava nikjer v poti ne pelje ven.
    static func resolveCwd(_ raw: String?, roots: String, scratch: URL? = nil) -> URL? {
        guard let raw, !raw.isEmpty, let path = Sync.real(URL(fileURLWithPath: expand(raw)))?.path else { return nil }
        var isDir: ObjCBool = false
        guard FileManager.default.fileExists(atPath: path, isDirectory: &isDir), isDir.boolValue else { return nil }
        let allowed = roots.split(separator: ",").compactMap { Sync.real(URL(fileURLWithPath: expand($0.trimmingCharacters(in: .whitespaces))))?.path }
            .filter { $0 != "/" }
        guard allowed.contains(where: { path == $0 || path.hasPrefix($0 + "/") }) else { return nil }
        let hidden = RouterConfig.hiddenScratch(scratch)
        guard !hidden.contains(where: { path == $0 || path.hasPrefix($0 + "/") }) else { return nil }
        guard !RouterConfig.overlapsCopilotCache(path) else { return nil }  // #26 (N2): ne paket Copilot ali mapa z njim
        return URL(fileURLWithPath: path, isDirectory: true)
    }
}

/// Orodja nameščenega Claude Code (za --allowedTools), enkrat na izvršljivo datoteko.
actor ToolList {
    private var cache: [String: [String]] = [:]
    func get(_ executable: String) async -> [String] {
        if let t = cache[executable] { return t }
        let t = await ClaudeCLI.availableTools(executable)
        if !t.isEmpty { cache[executable] = t }
        return t
    }
}

/// Zunanja seja (ključ transkripta) → notranja `claude` seja. Ob ponovnem zagonu se izgubi;
/// takrat nova seja dobi pogovor kot kontekst.
actor SessionRegistry {
    private var map: [String: String] = [:]
    func get(_ key: String) -> String? { map[key] }
    func set(_ key: String, _ id: String) {
        if map.count > 2000 { map.removeAll() }  // ponytail: grobo čiščenje, LRU ko bo treba
        map[key] = id
    }
}

/// Pretvori `claude -p --output-format stream-json` v en Anthropic odgovor z enim besedilnim blokom.
struct StreamTranslator {
    let model: String
    let messageID = "msg_" + UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased()
    private(set) var text = ""
    private(set) var resultError: String?
    private(set) var step: String?  // zadnje orodje agenta, npr. »Read docs/x.md« (za Activity)
    var usage: [String: Any] = [:]
    var stop = "end_turn"  // stop_reason (Foundry Local: max_tokens pri length)

    func start() -> [Data] {
        [Self.sse("message_start", ["type": "message_start", "message": [
            "id": messageID, "type": "message", "role": "assistant", "model": model, "content": [Any](),
            "stop_reason": NSNull(), "stop_sequence": NSNull(), "usage": ["input_tokens": 0, "output_tokens": 0],
         ] as [String: Any]]),
         Self.sse("content_block_start", ["type": "content_block_start", "index": 0, "content_block": ["type": "text", "text": ""]])]
    }

    mutating func feed(_ line: Data) -> [Data] {
        guard let obj = try? JSONSerialization.jsonObject(with: line) as? [String: Any] else { return [] }
        switch obj["type"] as? String {
        case "stream_event":
            guard obj["parent_tool_use_id"] == nil || obj["parent_tool_use_id"] is NSNull,  // dogodki notranjih podagentov
                  let ev = obj["event"] as? [String: Any] else { return [] }
            switch ev["type"] as? String {
            case "message_start" where !text.isEmpty && !text.hasSuffix("\n"):
                return [emit("\n\n")]
            case "content_block_start":
                if let b = ev["content_block"] as? [String: Any], b["type"] as? String == "tool_use" {
                    return [emit("\(text.isEmpty || text.hasSuffix("\n") ? "" : "\n")⏺ \(b["name"] as? String ?? "tool")\n")]
                }
            case "content_block_delta":
                if let d = ev["delta"] as? [String: Any], d["type"] as? String == "text_delta", let t = d["text"] as? String {
                    return [emit(t)]
                }
            default: break
            }
        case "assistant":  // celotno sporočilo z vhodi orodij (le glavni agent)
            guard obj["parent_tool_use_id"] == nil || obj["parent_tool_use_id"] is NSNull else { return [] }
            for b in ((obj["message"] as? [String: Any])?["content"] as? [[String: Any]]) ?? [] where b["type"] as? String == "tool_use" {
                let input = b["input"] as? [String: Any] ?? [:]
                let arg = ["command", "file_path", "pattern", "path", "url", "query", "description"].lazy.compactMap { input[$0] as? String }.first ?? ""
                step = "\(b["name"] as? String ?? "tool") \(arg.prefix(120))".trimmingCharacters(in: .whitespaces)
            }
        case "result":
            usage = obj["usage"] as? [String: Any] ?? [:]
            if obj["is_error"] as? Bool == true || obj["subtype"] as? String != "success" {
                resultError = (obj["result"] as? String) ?? (obj["subtype"] as? String) ?? "claude error"
            }
        default: break
        }
        return []
    }

    func finish() -> [Data] {
        // input_tokens šele iz rezultata CLI (message_start ima 0): tudi v message_delta, kot API, za zgodovino sej
        let out = (usage["output_tokens"] as? Int) ?? 0, input = (usage["input_tokens"] as? Int) ?? 0
        return [Self.sse("content_block_stop", ["type": "content_block_stop", "index": 0]),
                Self.sse("message_delta", ["type": "message_delta",
                                           "delta": ["stop_reason": stop, "stop_sequence": NSNull()] as [String: Any],
                                           "usage": ["input_tokens": input, "output_tokens": out]]),
                Self.sse("message_stop", ["type": "message_stop"])]
    }

    func message() -> [String: Any] {
        ["id": messageID, "type": "message", "role": "assistant", "model": model,
         "content": [["type": "text", "text": text]], "stop_reason": stop, "stop_sequence": NSNull(),
         "usage": ["input_tokens": (usage["input_tokens"] as? Int) ?? 0, "output_tokens": (usage["output_tokens"] as? Int) ?? 0]]
    }

    mutating func emit(_ s: String) -> Data {
        text += s
        return Self.sse("content_block_delta", ["type": "content_block_delta", "index": 0, "delta": ["type": "text_delta", "text": s]])
    }

    static func sse(_ event: String, _ obj: [String: Any]) -> Data {
        let json = (try? JSONSerialization.data(withJSONObject: obj, options: [.withoutEscapingSlashes])) ?? Data("{}".utf8)
        return Data("event: \(event)\ndata: ".utf8) + json + Data("\n\n".utf8)
    }
}

extension Router {
    static let sessions = SessionRegistry()
    static let shares = ShareHub()
    static let grokSessions = SessionRegistry()
    static let codexSessions = SessionRegistry()

    /// --minigw-ro: registrirana mapa le za branje ali glava x-claude-mini-ro (minigw brez deljenja, npr. prehod na istem Macu;
    /// pri vgnezdenem klicu jo nastavi AgentRun iz klicatelja).
    static let readOnlyWorktree = "Worktree mode cannot be read-only (--minigw-ro): use direct or sync"  // L2: kot v Go

    static func isReadOnly(_ req: HTTPRequest) async -> Bool {
        if req.header("x-claude-mini-ro") == "1" { return true }
        return await shares.isReadOnly(session: req.header("x-claude-mini-session") ?? "")
    }

    /// Navodilo agentu za priklopljeno mapo odjemalca (macOS, Windows ali Linux).
    /// `gits`: .git vseh klonov (vrhnji prvi; B: ugnezdeni poimenovani z ukazom za commit v njih, kot Go nestedClonesNote).
    static func nestedClonesNote(_ gits: [String]) -> String {
        guard gits.count > 1 else { return "" }
        return " These nested repositories and submodules are their own clones on the same branch, and their changes come back too: "
            + gits.dropFirst().map { String($0.dropLast(5)) }.joined(separator: ", ") + ". Plain git here works on the top repository; git run in one of these folders"
            + " (cd <path> && git … or git -C <path> …) works in that one. Changes you leave uncommitted there are committed for you."
    }

    static func mountNote(_ clientCwd: String, _ shared: URL, readOnly: Bool, synced: Bool = false, branch: String? = nil, gits: [String] = []) -> String {
        if let branch, readOnly {  // worktree, agent brez pisanja
            return "The user's folder \(clientCwd) (on their computer, a git repository) is a clone here at \(shared.path), your working"
                + " directory, read-only for you: do not try to create, change or delete files in it. Map paths under \(clientCwd) to it."
                + " Nothing goes back to the user, so give your answer in your reply."
        }
        if let branch {
            return "The user's folder \(clientCwd) (on their computer, a git repository) is a clone here at \(shared.path), your working"
                + " directory, on branch \(branch). Map paths under \(clientCwd) to it. Commit your work on this branch; changes you leave"
                + " uncommitted are committed for you at the end. The branch goes back to the user's repository; their files are not changed."
                + " Build output (.build, target, DerivedData) is never committed." + Self.nestedClonesNote(gits)
                + Self.depsNote(Worktree.deps(gits.first.map { URL(fileURLWithPath: $0).deletingLastPathComponent() } ?? shared).path)
                + " Maven builds work here; swift build and xcodebuild may fail in this sandbox: then tell the user to build with minigw run."
        }
        return (synced ? "The user's folder \(clientCwd) (on their computer) is synced here as \(shared.path), your working directory: a local copy"
            + (readOnly ? "" : " whose changed source files are copied back to the user when you finish") + ". Map paths under \(clientCwd) to it."
            : "The user's folder \(clientCwd) (on their computer) is mounted here as \(shared.path), your working directory. Map paths under \(clientCwd) to it.")
            + (readOnly ? " The folder is shared read-only: do not try to create, change or delete files in it."
                + " Open SQLite databases with the URI file:<path>?mode=ro&immutable=1 (no journal or lock files)." : "")
    }
    /// #67 (kot Go depsNote): kam gredo odvisnosti in repozitoriji agenta. `sandboxed`: ukazi agenta nosijo oznako peskovnika
    /// (na Macu vsi pišoči agenti: Claude, Codex, Copilot).
    /// Recepti z dobesedno potjo: Claude Code v -p zavrne ukaz s spremenljivko, ki je ne more preveriti vnaprej.
    static func depsNote(_ deps: String, sandboxed: Bool = true) -> String {
        " Dependencies and repositories you create go to $MINIGW_DEPS (\(deps)), kept for this conversation; "
            + (sandboxed ? "git init and git clone work only there." : "git init and git clone are not available on this gateway (no sandbox).")
            + " CMake: -DFETCHCONTENT_BASE_DIR=\"\(deps)/cmake\". SwiftPM: swift build --scratch-path \"\(deps)/swiftpm\"."
            + " Xcode: -clonedSourcePackagesDirPath \"\(deps)/spm\" -derivedDataPath \"\(deps)/DerivedData\"."
            + " Use these paths as written, not $MINIGW_DEPS in a command."
            + " Maven, Go, npm, pip, Cargo and Gradle caches are already set."
    }
    static let toolList = ToolList()

    /// Naslov tega routerja za lokalni mount_webdav.
    var localBase: String {
        let h = config.host == "0.0.0.0" || config.host == "::" || config.host.isEmpty ? "127.0.0.1" : config.host
        return "http://\(h.contains(":") ? "[\(h)]" : h):\(config.port)"
    }

    func runCLI(_ req: HTTPRequest, body: [String: Any], cliModel: String, chatOnly: Bool,
                rid: String, writer: ResponseWriter) async -> Int {
        let cfg = config.claudeCLI
        let tools = (body["tools"] as? [Any])?.isEmpty == false
        // Zahteve brez orodij (naslov seje, povzetki, claude-chat-*) so pogovor; z orodji so agent v mapi seje.
        let agent = tools && !chatOnly

        let all = ClaudeCLI.turns(body)
        guard let last = all.last, last.role == "user" else {
            return await error(400, "invalid_request_error", "Last message must be from the user", rid: rid, writer: writer)
        }
        let history = all.dropLast()

        // N9: besedilo naprave je vnos uporabnika claude -p: brez ukazov s poševnico (/config piše nastavitve lastnika
        // prehoda) in brez »@pot« (CLAUDE_CODE_DISABLE_FILE_MENTIONS spodaj: priložil bi datoteko prehoda mimo peskovnika)
        var checked = AllowedFolders.Checked()
        var args = ["-p", "--disable-slash-commands", "--output-format", "stream-json", "--include-partial-messages", "--verbose",
                    "--model", cliModel, "--strict-mcp-config"]
        let cwd: URL
        var prompt = last.text
        var sessionID: String?
        var registryKey: String?
        var sharedDir: URL?
        var readOnly = false

        if agent {
            // Mapa odjemalca (minigw na drugem Macu) ima prednost pred enako potjo na tem Macu.
            let clientCwd = req.header("x-claude-mini-cwd")
            let shared = await Self.shares.mount(session: req.header("x-claude-mini-session") ?? "", cwd: clientCwd, base: localBase,
                                                 wait: req.header("x-claude-mini-share") == "1", device: req.device?.id,
                                                 roots: checkedRoots, scratch: config.scratchURL, writes: true)
            let synced = await Self.shares.isSync(session: req.header("x-claude-mini-session") ?? "")
            let tree = await Self.shares.worktree(session: req.header("x-claude-mini-session") ?? "")
            let cap = await capped(req, kind: "claude")  // #45: način seje nad omejitvijo → le branje
            readOnly = await Self.isReadOnly(req) || cap != nil
            Self.runBranch?.readOnly = readOnly  // #46a
            if readOnly, tree != nil { return await error(400, "invalid_request_error", Self.readOnlyWorktree, rid: rid, writer: writer) }  // R4: tudi vprašani (kot Go)
            if let t = tree, Worktree.grantDirs(root: t.root) == nil {  // #67 H1: pred peskovnikom
                return await error(503, "api_error", Worktree.unsafeFolders, rid: rid, writer: writer)
            }
            if let shared, let clientCwd {
                args += ["--append-system-prompt", Self.mountNote(clientCwd, shared, readOnly: readOnly, synced: synced, branch: tree?.branch, gits: tree?.gits ?? [])]
            }
            guard let dir = shared ?? (synced ? nil : ClaudeCLI.resolveCwd(clientCwd, roots: checkedRoots, scratch: config.scratchURL)) else {
                if let why = await Self.shares.takeRefusal() {  // zaklep pisanja
                    return await error(why.status, "invalid_request_error", why.message, rid: rid, writer: writer)
                }
                if synced, Task.isCancelled { return 499 }  // Claude Code je prekinil med čakanjem na prenos
                if synced { return await error(502, "api_error", "Syncing \(clientCwd ?? "the folder") to the gateway failed (see minigw output)", rid: rid, writer: writer) }
                return await error(400, "invalid_request_error",
                                   "cwd_unavailable: folder \(req.header("x-claude-mini-cwd") ?? "(none)") does not exist on this Mac or is outside the allowed folders",
                                   rid: rid, writer: writer)
            }
            cwd = dir
            sharedDir = shared
            let scope = (req.header("x-claude-mini-session") ?? "") + "|" + dir.path
            registryKey = scope
            if let existing = await Self.sessions.get(ClaudeCLI.key(history, scope: scope)) {
                args += ["--resume", existing]
                sessionID = existing
            } else {
                let id = UUID().uuidString.lowercased()
                args += ["--session-id", id]
                sessionID = id
                prompt = ClaudeCLI.prompt(history: history, last: last)
            }
            let allTools = cfg.allowLocalTools || tree != nil ? await Self.toolList.get(cfg.executable) : []
            if tree == nil { checked = allowedFolders.checked(cfg.allowedRoots, excluding: dir.path, scratch: config.scratchURL) }
            args += (tree.map { cfg.worktreeArgs(root: $0.root, maven: $0.maven, cwd: dir, allTools: allTools, network: config.worktreeNetwork,
                                                 scratch: config.scratchURL, read: $0.read, gits: $0.gits) }
                     ?? cfg.agentArgs(cwd: dir, allTools: allTools, scratch: config.scratchURL, readOnly: readOnly, folders: allowedFolders, checkedPaths: checked.paths))
                + ["--setting-sources", shared == nil ? cfg.settingSources : ""]  // hooki iz mape odjemalca ne tečejo tukaj
        } else {
            cwd = config.scratch("chat")
            try? FileManager.default.createDirectory(at: cwd, withIntermediateDirectories: true)
            prompt = ClaudeCLI.prompt(history: history, last: last)
            args += ["--tools", "", "--no-session-persistence", "--setting-sources", ""]
            if let system = Self.systemText(body["system"]), !system.isEmpty { args += ["--system-prompt", system] }
        }

        let stream = body["stream"] as? Bool == true
        var t = StreamTranslator(model: body["model"] as? String ?? cliModel)
        var headSent = false
        func send(_ chunks: [Data]) async throws {
            guard stream, !chunks.isEmpty else { return }
            try await writer.write(chunks.reduce(Data(), +))
        }

        let run = agent ? await startAgentRun(req, kind: "claude", dir: sharedDir, readOnly: readOnly) : nil
        defer { run?.stop() }
        let gitexec = config.scratch("gitexec")  // N9: ovoj git v klonu (Worktree.claudeEnv), berljiv v peskovniku
        if agent {  // #69: navodilo vedno; branje minigw in ovoja git kot doslej le s stikalom ali za vprašanega agenta
            // #67 (Air): ovoj git (gitexec/bin/git prvi na PATH, exec-path) v klonu vedno, sicer peskovnik ne izvede ovoja
            // (git z drugega PATH, exec-path v gitexec neberljiv: remote-https, CMake GIT_EXECUTABLE)
            let read = (askOthers("claude") || AgentRun.current != nil ? [Self.minigwExe] + (run.map { [$0.authFile.path] } ?? []) : [])
                + (await Self.shares.worktree(session: req.header("x-claude-mini-session") ?? "") != nil ? [gitexec.path] : [])
            args = Self.withNote(args, agentNote("claude"), read: read)
        }
        guard let exe = await ClaudeCLI.executable(cfg.executable) else {
            return await error(503, "api_error", cfg.executable.isEmpty ? "claude not found; set its path in Mini Gateway settings"
                                                                         : "claude not found at \(cfg.executable)", rid: rid, writer: writer)
        }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = args
        p.currentDirectoryURL = cwd
        var env = ProcessInfo.processInfo.environment
        for k in env.keys where k.hasPrefix("ANTHROPIC_") || k.hasPrefix("CLAUDE_CODE_") || k == "CLAUDECODE" || Worktree.sandboxMarkers.contains(k) {
            env[k] = nil  // sicer bi notranji claude klical nazaj ta router; #67: harness ni v peskovniku
        }
        env["PATH"] = (env["PATH"].map { $0 + ":" } ?? "") + "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
        if agent, let t = await Self.shares.worktree(session: req.header("x-claude-mini-session") ?? "") {
            guard let tenv = Worktree.claudeEnv(root: t.root, maven: t.maven, gitexec: gitexec, gits: t.gits, agent: "claude", path: env["PATH"] ?? "") else {
                return await error(503, "api_error", "Cannot prepare git for the agent's clone (git --exec-path): use sync", rid: rid, writer: writer)
            }
            env.merge(tenv) { $1 }
            guard let link = Worktree.makeTmpLink(t.root) else {  // #67: kratka pot, podatki pod Scratch; brez nje ne v /tmp
                return await error(503, "api_error", "Cannot prepare Claude Code's temporary folder link in /private/tmp/mgw-\(getuid()) (it must be a private folder of the gateway user): start a new conversation",
                                   rid: rid, writer: writer)
            }
            env["CLAUDE_CODE_TMPDIR"] = link
            env["MINIGW_NETWORK_OFF"] = Worktree.networkOff("claude", config)  // #67: ovoj git
        }
        for k in ["MINIGW_AGENT_URL", "MINIGW_AGENT_AUTH_FILE"] { env[k] = run?.env[k] }
        env["CLAUDE_CODE_DISABLE_FILE_MENTIONS"] = "1"
        p.environment = env
        let stdin = Pipe(), stdout = Pipe(), stderr = Pipe()
        p.standardInput = stdin
        p.standardOutput = stdout
        p.standardError = stderr
        let exit = ExitWaiter(), pipes = [stdout.fileHandleForReading, stderr.fileHandleForReading], dirs = await Self.shares.runDirs()
        p.terminationHandler = { p in  // waitUntilExit brez run loopa lahko obvisi
            exit.done(p.terminationStatus)
            let g = p.processIdentifier  // #73: 2 s po koncu drevo (tudi vnuk s cevjo) ven, da se branje konča
            DispatchQueue.global().asyncAfter(deadline: .now() + 2) { Native.killTree(g, pipes: pipes) }
        }

        do {
            let held = try checked.open()
            defer { for handle in held { try? handle.close() } }
            try p.run()
            run?.started(p.processIdentifier)
        } catch {
            return await self.error(500, "api_error", "Cannot start claude: \(error.localizedDescription)", rid: rid, writer: writer)
        }
        AgentCount.running.add(1, ordering: .relaxed)
        defer { AgentCount.running.subtract(1, ordering: .relaxed) }
        try? stdin.fileHandleForWriting.write(contentsOf: Data(prompt.utf8))
        try? stdin.fileHandleForWriting.close()
        let errTask = Task { (try? stderr.fileHandleForReading.readToEnd()).flatMap { String(data: $0.suffix(4000), encoding: .utf8) } ?? "" }

        var pinger: Task<Void, Never>?
        do {
            try await withTaskCancellationHandler {
                if stream {
                    try await writer.write(HTTPResponse.head(status: 200, headers: [
                        ("Content-Type", "text/event-stream"), ("Cache-Control", "no-cache"), ("request-id", rid), ("x-should-retry", "false"),
                    ]))
                    headSent = true
                    try await send(t.start())
                    pinger = Task {  // notranji agent lahko dolgo dela brez dogodkov
                        while !Task.isCancelled {
                            try? await Task.sleep(for: .seconds(15))
                            if Task.isCancelled { break }
                            try? await writer.write(StreamTranslator.sse("ping", ["type": "ping"]))
                        }
                    }
                }
                for try await line in stdout.fileHandleForReading.bytes.lines {
                    let before = t.step
                    try await send(t.feed(Data(line.utf8)))
                    if let s = t.step, s != before { onActivity(.step(id: rid, text: s)) }
                }
            } onCancel: {
                Native.terminateGroup(p.processIdentifier)
            }
        } catch {
            pinger?.cancel()
            Native.terminateGroup(p.processIdentifier)
            _ = await firstOf(Task { await exit.wait() }, within: .seconds(5))  // sync: klon se odstrani šele po koncu agenta
            let pid = p.processIdentifier
            if !(await offPool { Native.killTree(pid, pipes: pipes, dirs: dirs) }) {
                Self.log.error("\(rid, privacy: .public) claude: its processes could not be stopped")
                await Self.shares.abandon()  // #73: klon sync ne gre v kopijo
            }
            return headSent ? 200 : 499
        }
        let status = await exit.wait()
        run?.stop()  // I2: vprašanja drugim modelom se končajo pred prevzemom sprememb
        let pid = p.processIdentifier
        let stopped = await offPool { Native.killTree(pid, pipes: pipes, dirs: dirs) }  // #73: finalize (syncBack) le, ko drevesa ni več
        if !stopped { await Self.shares.abandon() }  // klon sync ne gre v kopijo
        // sync: spremembe agenta k odjemalcu pred koncem odgovora (Claude Code jih nato že vidi); med tem še ping
        let pullErr = agent && stopped ? await Self.shares.syncBack(session: req.header("x-claude-mini-session") ?? "") : nil  // #82
        pinger?.cancel()
        let errText = await errTask.value

        if let msg = (stopped ? nil : Native.notStopped(rid)) ?? t.resultError
            ?? (status != 0 && t.text.isEmpty ? (errText.isEmpty ? "claude exited with \(status)" : errText) : nil) ?? pullErr {
            Self.log.error("\(rid, privacy: .public) claude failed: \(msg, privacy: .public)")
            let (code, type, text) = AgentError.classify(msg)
            // #82: neprevzeto delo agenta je končno (ponovitev bi agenta zagnala znova in spremembe uveljavila dvakrat)
            let final = code == 429 || pullErr != nil
            if !stream { return await error(code, type, "claude: \(text)", rid: rid, extra: final ? [("x-should-retry", "false")] : [], writer: writer) }
            if t.text.isEmpty || !stopped || pullErr != nil {
                if final { AgentError.note(AgentError.key(req, model: t.model), "claude: \(text)", status: code) }  // ponovitev brez pretoka
                try? await writer.write(StreamTranslator.sse("error", ["type": "error", "error": ["type": type, "message": "claude: \(text)"]]))
                return code
            }
        }

        if agent, let scope = registryKey, let id = sessionID {
            await Self.sessions.set(ClaudeCLI.key(all + [ClaudeCLI.Turn(role: "assistant", text: t.text.trimmingCharacters(in: .whitespacesAndNewlines))], scope: scope), id)
        }
        if stream {
            try? await send(t.finish())
            return 200
        }
        return await json(200, t.message(), rid: rid, writer: writer)
    }

    /// Navodilo agentu k --append-system-prompt (ali novo) in pot, ki jo sandbox Claude Code sme brati (minigw).
    static func withNote(_ args: [String], _ note: String, read: [String]) -> [String] {
        var a = args
        if let i = a.firstIndex(of: "--append-system-prompt"), i + 1 < a.count { a[i + 1] += "\n\n" + note } else { a += ["--append-system-prompt", note] }
        if let i = a.firstIndex(of: "--settings"), i + 1 < a.count,
           var st = (try? JSONSerialization.jsonObject(with: Data(a[i + 1].utf8))) as? [String: Any],
           var sb = st["sandbox"] as? [String: Any], var fs = sb["filesystem"] as? [String: Any] {
            fs["allowRead"] = (fs["allowRead"] as? [String] ?? []) + read
            sb["filesystem"] = fs; st["sandbox"] = sb
            a[i + 1] = String(decoding: try! JSONSerialization.data(withJSONObject: st, options: [.sortedKeys]), as: UTF8.self)
        }
        return a
    }

    static func systemText(_ system: Any?) -> String? {
        if let s = system as? String { return s }
        if let blocks = system as? [[String: Any]] { return blocks.compactMap { $0["text"] as? String }.joined(separator: "\n\n") }
        return nil
    }
}

/// Počaka na konec procesa prek terminationHandler (deluje tudi, če se je proces že končal).
final class ExitWaiter: @unchecked Sendable {
    private let lock = NSLock()
    private var status: Int32?
    private var waiter: CheckedContinuation<Int32, Never>?

    func done(_ s: Int32) {
        lock.lock()
        if let w = waiter { waiter = nil; lock.unlock(); w.resume(returning: s) } else { status = s; lock.unlock() }
    }

    func wait() async -> Int32 {
        await withCheckedContinuation { c in
            lock.lock()
            if let s = status { lock.unlock(); c.resume(returning: s) } else { waiter = c; lock.unlock() }
        }
    }
}

/// Programi agentov v prijavni lupini uporabnika (`zsh -lic`, sekunde): poiščejo se enkrat, ob zagonu routerja, na svoji niti.
/// Router počaka z `await` (nit bazena Swift concurrency ostane prosta); nastavitve s `pathNow` (glavna nit).
public enum Detected {
    private static let group = DispatchGroup()
    nonisolated(unsafe) private static var paths: [String: String] = [:]  // zapisano enkrat pred group.leave()
    private static let started: Void = {
        group.enter()
        Thread {
            let names = ["claude", "codex", "copilot", "grok"], lock = NSLock()
            var found: [String: String] = [:]
            DispatchQueue.concurrentPerform(iterations: names.count) { i in
                let extra = names[i] == "claude" ? ["\(NSHomeDirectory())/.claude/local/claude"] : []
                if let p = ClaudeCLI.locate(names[i], also: extra) { lock.withLock { found[names[i]] = p } }
            }
            paths = found
            group.leave()
        }.start()
    }()

    /// Začne iskanje (ob zagonu routerja).
    public static func start() { started }

    static func path(_ name: String) async -> String? {
        start()
        await withCheckedContinuation { (c: CheckedContinuation<Void, Never>) in group.notify(queue: .global()) { c.resume() } }
        return paths[name]
    }

    /// Blokira klicočo nit, dokler iskanje ne konča: le nastavitve (glavna nit), nikoli v bazenu.
    public static func pathNow(_ name: String) -> String? {
        start()
        group.wait()
        return paths[name]
    }
}
