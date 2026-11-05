import CryptoKit
import Foundation

/// Ponudnik `claude -p`: vgrajeni modeli Claude Code (opus/sonnet/haiku/fable) in `claude-chat-<m>`.
public struct ClaudeCLIConfig: Codable, Sendable, Equatable {
    public var enabled = false
    public var executable = ""                       // prazno = samodejno iskanje
    public var allowedRoots = "~/Development"        // ločeno z vejico
    public var tools = "Read,Grep,Glob"              // na voljo in samodejno dovoljena; prazno = brez orodij
    public var permissionMode = "dontAsk"            // brez vprašanj: kar ni dovoljeno, je zavrnjeno
    public var settingSources = "project,local"      // uporabnikove nastavitve (hooki, vtičniki) se ne naložijo

    public init() {}

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let d = ClaudeCLIConfig()
        enabled = try c.decodeIfPresent(Bool.self, forKey: .enabled) ?? d.enabled
        executable = try c.decodeIfPresent(String.self, forKey: .executable) ?? d.executable
        allowedRoots = try c.decodeIfPresent(String.self, forKey: .allowedRoots) ?? d.allowedRoots
        tools = try c.decodeIfPresent(String.self, forKey: .tools) ?? d.tools
        permissionMode = try c.decodeIfPresent(String.self, forKey: .permissionMode) ?? d.permissionMode
        settingSources = try c.decodeIfPresent(String.self, forKey: .settingSources) ?? d.settingSources
    }
}

public enum ClaudeCLI {
    static let families = ["opus", "sonnet", "haiku", "fable"]
    static let chatPrefix = "claude-chat-"

    /// Model za `claude --model` in ali je način vsiljeno »chat« (brez orodij).
    static func match(_ model: String) -> (cliModel: String, chatOnly: Bool)? {
        if model.hasPrefix(chatPrefix) {
            let m = String(model.dropFirst(chatPrefix.count))
            return families.contains(where: { m.hasPrefix($0) || m.hasPrefix("claude-\($0)") }) ? (m, true) : nil
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

    public struct ValidationError: Error, LocalizedError {
        public let message: String
        init(_ m: String) { message = m }
        public var errorDescription: String? { message }
    }

    /// `command -v claude` v uporabnikovi prijavni lupini (njegov PATH), nato običajna mesta namestitve.
    /// Izračuna se enkrat ob prvi uporabi.
    public static let detected: String? = {
        let shell = ProcessInfo.processInfo.environment["SHELL"] ?? "/bin/zsh"
        let p = Process()
        p.executableURL = URL(fileURLWithPath: shell)
        p.arguments = ["-lic", "command -v claude"]
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
        let candidates = found + ["\(home)/.local/bin/claude", "/opt/homebrew/bin/claude", "/usr/local/bin/claude", "\(home)/.claude/local/claude"]
        return candidates.first { FileManager.default.isExecutableFile(atPath: $0) }
    }()

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
            guard let dir = ClaudeCLI.resolveCwd(req.header("x-claude-mini-cwd"), roots: cfg.allowedRoots) else {
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
            args += ["--tools", cfg.tools, "--allowedTools", cfg.tools, "--permission-mode", cfg.permissionMode,
                     "--setting-sources", cfg.settingSources]
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
                    try await send(t.feed(Data(line.utf8)))
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
