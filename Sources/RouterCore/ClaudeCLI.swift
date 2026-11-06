import CryptoKit
import Foundation

/// Ponudnik `claude -p`: vgrajeni modeli Claude Code (opus/sonnet/haiku/fable), `claude-cli-<m>` (agent) in `claude-chat-<m>` (brez orodij).
public struct ClaudeCLIConfig: Codable, Sendable, Equatable {
    public var enabled = false
    public var executable = ""                       // prazno = samodejno iskanje
    public var allowedRoots = "~/Development"        // ločeno z vejico
    public var tools = "Read,Grep,Glob"              // na voljo in samodejno dovoljena; prazno = brez orodij
    public var permissionMode = "dontAsk"            // brez vprašanj: kar ni dovoljeno, je zavrnjeno
    public var allowLocalTools = false               // vsa orodja Claude Code na tem Macu brez vprašanj; preglasi tools/permissionMode
    public var disabledTools: [String] = []          // izjeme pri allowLocalTools (tabela v nastavitvah)
    public var settingSources = "project,local"      // uporabnikove nastavitve (hooki, vtičniki) se ne naložijo

    public init() {}

    /// Orodja z dostopom do datotek: dovoljena le v mapi seje in dovoljenih mapah (preverja Claude Code oz. sandbox).
    static let pathTools: Set<String> = ["Read", "Edit", "Write", "MultiEdit", "NotebookEdit", "Glob", "Grep", "LS", "Bash"]

    /// Orodja in dovoljenja agenta (claude -p ne more vprašati: kar ni dovoljeno, je zavrnjeno).
    /// Z »Allow local tools« ima agent vsa orodja, a datoteke le v `cwd` in dovoljenih mapah:
    /// Read/Edit/Write zunaj teh map niso dovoljeni, Bash teče v sandboxu Claude Code (brez branja domače mape,
    /// pisanje le v teh mapah, omrežje dovoljeno), izhoda iz sandboxa ni.
    func agentArgs(cwd: URL, allTools: [String]) -> [String] {
        guard allowLocalTools else { return ["--tools", tools, "--allowedTools", tools, "--permission-mode", permissionMode] }
        let dirs = [cwd.path] + allowedRoots.split(separator: ",").map { ClaudeCLI.expand($0.trimmingCharacters(in: .whitespaces)) }
            .filter { $0 != "/" && !$0.isEmpty && $0 != cwd.path && FileManager.default.fileExists(atPath: $0) }
        let sandbox: [String: Any] = ["sandbox": [
            "enabled": true, "failIfUnavailable": true, "autoAllowBashIfSandboxed": true, "allowUnsandboxedCommands": false,
            "network": ["allowedDomains": ["*"]],
            "filesystem": ["denyRead": ["~"], "allowRead": dirs, "allowWrite": dirs],
        ]]
        let settings = String(decoding: try! JSONSerialization.data(withJSONObject: sandbox, options: [.sortedKeys]), as: UTF8.self)
        let others = allTools.filter { !Self.pathTools.contains($0) && !disabledTools.contains($0) }
        return ["--tools", "default", "--permission-mode", "acceptEdits", "--settings", settings]
            + dirs.dropFirst().flatMap { ["--add-dir", $0] }
            + (others.isEmpty ? [] : ["--allowedTools", others.joined(separator: ",")])
            + (disabledTools.isEmpty ? [] : ["--disallowedTools", disabledTools.joined(separator: ",")])
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let d = ClaudeCLIConfig()
        enabled = try c.decodeIfPresent(Bool.self, forKey: .enabled) ?? d.enabled
        executable = try c.decodeIfPresent(String.self, forKey: .executable) ?? d.executable
        allowedRoots = try c.decodeIfPresent(String.self, forKey: .allowedRoots) ?? d.allowedRoots
        tools = try c.decodeIfPresent(String.self, forKey: .tools) ?? d.tools
        permissionMode = try c.decodeIfPresent(String.self, forKey: .permissionMode) ?? d.permissionMode
        allowLocalTools = try c.decodeIfPresent(Bool.self, forKey: .allowLocalTools) ?? d.allowLocalTools
        disabledTools = try c.decodeIfPresent([String].self, forKey: .disabledTools) ?? d.disabledTools
        settingSources = try c.decodeIfPresent(String.self, forKey: .settingSources) ?? d.settingSources
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
    static func executable(_ configured: String) -> String? {
        configured.isEmpty ? detected : [(configured as NSString).expandingTildeInPath].first { FileManager.default.isExecutableFile(atPath: $0) }
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
        let killer = DispatchWorkItem { if p.isRunning { p.terminate() } }
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
        guard let exe = executable(configured) else { return [] }
        return await Task.detached {
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
            let killer = DispatchWorkItem { if p.isRunning { p.terminate() } }
            DispatchQueue.global().asyncAfter(deadline: .now() + 15, execute: killer)
            defer { killer.cancel(); if p.isRunning { p.terminate() } }
            var line = Data()
            while let b = try? out.fileHandleForReading.read(upToCount: 1), let c = b.first, c != 10 { line.append(c) }
            let tools = (try? JSONSerialization.jsonObject(with: line) as? [String: Any])?["tools"] as? [String]
            return (tools ?? []).sorted()
        }.value
    }

    public struct ValidationError: Error, LocalizedError {
        public let message: String
        init(_ m: String) { message = m }
        public var errorDescription: String? { message }
    }

    /// `command -v claude` v uporabnikovi prijavni lupini (njegov PATH), nato običajna mesta namestitve.
    /// Izračuna se enkrat ob prvi uporabi.
    public static let detected: String? = locate("claude", also: ["\(NSHomeDirectory())/.claude/local/claude"])

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

    /// Delovna mapa mora obstajati in biti pod dovoljenimi korenskimi mapami.
    static func resolveCwd(_ raw: String?, roots: String) -> URL? {
        guard let raw, !raw.isEmpty else { return nil }
        let path = expand(raw)
        var isDir: ObjCBool = false
        guard FileManager.default.fileExists(atPath: path, isDirectory: &isDir), isDir.boolValue else { return nil }
        let allowed = roots.split(separator: ",").map { expand($0.trimmingCharacters(in: .whitespaces)) }.filter { $0 != "/" }
        guard allowed.contains(where: { path == $0 || path.hasPrefix($0 + "/") }) else { return nil }
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
        let out = (usage["output_tokens"] as? Int) ?? 0
        return [Self.sse("content_block_stop", ["type": "content_block_stop", "index": 0]),
                Self.sse("message_delta", ["type": "message_delta",
                                           "delta": ["stop_reason": "end_turn", "stop_sequence": NSNull()] as [String: Any],
                                           "usage": ["output_tokens": out]]),
                Self.sse("message_stop", ["type": "message_stop"])]
    }

    func message() -> [String: Any] {
        ["id": messageID, "type": "message", "role": "assistant", "model": model,
         "content": [["type": "text", "text": text]], "stop_reason": "end_turn", "stop_sequence": NSNull(),
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

        var args = ["-p", "--output-format", "stream-json", "--include-partial-messages", "--verbose",
                    "--model", cliModel, "--strict-mcp-config"]
        let cwd: URL
        var prompt = last.text
        var sessionID: String?
        var registryKey: String?

        if agent {
            // Mapa odjemalca (minigw na drugem Macu) ima prednost pred enako potjo na tem Macu.
            let clientCwd = req.header("x-claude-mini-cwd")
            let shared = await Self.shares.mount(session: req.header("x-claude-mini-session") ?? "", cwd: clientCwd, base: localBase,
                                                 wait: req.header("x-claude-mini-share") == "1")
            if let shared, let clientCwd {
                args += ["--append-system-prompt",
                         "The user's folder \(clientCwd) (on their Mac) is mounted here as \(shared.path), your working directory. Map paths under \(clientCwd) to it."]
            }
            guard let dir = shared ?? ClaudeCLI.resolveCwd(clientCwd, roots: cfg.allowedRoots) else {
                return await error(400, "invalid_request_error",
                                   "cwd_unavailable: folder \(req.header("x-claude-mini-cwd") ?? "(none)") does not exist on this Mac or is outside the allowed folders",
                                   rid: rid, writer: writer)
            }
            cwd = dir
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
            let allTools = cfg.allowLocalTools ? await Self.toolList.get(cfg.executable) : []
            args += cfg.agentArgs(cwd: dir, allTools: allTools) + ["--setting-sources", shared == nil ? cfg.settingSources : ""]  // hooki iz mape odjemalca ne tečejo tukaj
        } else {
            cwd = FileManager.default.temporaryDirectory.appendingPathComponent("minigateway-chat", isDirectory: true)
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

        guard let exe = ClaudeCLI.executable(cfg.executable) else {
            return await error(503, "api_error", cfg.executable.isEmpty ? "claude not found; set its path in Mini Gateway settings"
                                                                         : "claude not found at \(cfg.executable)", rid: rid, writer: writer)
        }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = args
        p.currentDirectoryURL = cwd
        var env = ProcessInfo.processInfo.environment
        for k in env.keys where k.hasPrefix("ANTHROPIC_") || k.hasPrefix("CLAUDE_CODE_") || k == "CLAUDECODE" {
            env[k] = nil  // sicer bi notranji claude klical nazaj ta router
        }
        env["PATH"] = (env["PATH"].map { $0 + ":" } ?? "") + "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
        p.environment = env
        let stdin = Pipe(), stdout = Pipe(), stderr = Pipe()
        p.standardInput = stdin
        p.standardOutput = stdout
        p.standardError = stderr
        let exit = ExitWaiter()
        p.terminationHandler = { exit.done($0.terminationStatus) }  // waitUntilExit brez run loopa lahko obvisi

        do {
            try p.run()
        } catch {
            return await self.error(500, "api_error", "Cannot start claude: \(error.localizedDescription)", rid: rid, writer: writer)
        }
        try? stdin.fileHandleForWriting.write(contentsOf: Data(prompt.utf8))
        try? stdin.fileHandleForWriting.close()
        let errTask = Task { (try? stderr.fileHandleForReading.readToEnd()).flatMap { String(data: $0.suffix(4000), encoding: .utf8) } ?? "" }

        var pinger: Task<Void, Never>?
        do {
            try await withTaskCancellationHandler {
                if stream {
                    try await writer.write(HTTPResponse.head(status: 200, headers: [
                        ("Content-Type", "text/event-stream"), ("Cache-Control", "no-cache"), ("request-id", rid),
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
                p.terminate()
            }
        } catch {
            pinger?.cancel()
            p.terminate()
            return headSent ? 200 : 499
        }
        pinger?.cancel()
        let status = await exit.wait()
        let errText = await errTask.value

        if let msg = t.resultError ?? (status != 0 && t.text.isEmpty ? (errText.isEmpty ? "claude exited with \(status)" : errText) : nil) {
            Self.log.error("\(rid, privacy: .public) claude failed: \(msg, privacy: .public)")
            if !stream { return await error(502, "api_error", "claude: \(msg)", rid: rid, writer: writer) }
            if t.text.isEmpty {
                try? await writer.write(StreamTranslator.sse("error", ["type": "error", "error": ["type": "api_error", "message": "claude: \(msg)"]]))
                return 502
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
