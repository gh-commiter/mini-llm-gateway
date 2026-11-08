import Foundation

/// Ponudnik `grok -p` (xAI Grok Build CLI): agent v mapi seje, kot Codex. Omejitve postavi jedro (sandbox Grok):
/// »workspace« bere povsod, piše le v mapo seje in začasne mape; »read-only« ne piše v projekt.
public struct GrokConfig: Codable, Sendable, Equatable {
    public var enabled = false
    public var executable = ""            // prazno = samodejno iskanje
    public var enabledModels: [String]?   // nil = vsi modeli računa; seznam = le ti
    public var write = true               // sandbox workspace (sicer read-only)
    public var askOthers = false          // »May ask other models«: agent vpraša drug model prehoda (AgentRun)
    public init() {}

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let d = GrokConfig()
        enabled = try c.decodeIfPresent(Bool.self, forKey: .enabled) ?? d.enabled
        executable = try c.decodeIfPresent(String.self, forKey: .executable) ?? d.executable
        enabledModels = try c.decodeIfPresent([String].self, forKey: .enabledModels)
        write = try c.decodeIfPresent(Bool.self, forKey: .write) ?? d.write
        askOthers = try c.decodeIfPresent(Bool.self, forKey: .askOthers) ?? d.askOthers
    }

    public func isModelEnabled(_ m: String) -> Bool { enabledModels?.contains(m) ?? true }

    public mutating func setModel(_ m: String, _ on: Bool, all: [String]) {
        var list = enabledModels ?? all
        list.removeAll { $0 == m }
        if on { list.append(m) }
        enabledModels = Set(list) == Set(all) ? nil : list
    }
}

public enum Grok {
    static let ids: Set<String> = ["claude-grok"]
    static let prefix = "claude-grok-"
    /// Za nastavitve (glavna nit); router: `await executable("")`.
    public static var detected: String? { Detected.pathNow("grok") }

    public static func executable(_ configured: String) async -> String? {
        configured.isEmpty ? await Detected.path("grok") : [(configured as NSString).expandingTildeInPath].first { FileManager.default.isExecutableFile(atPath: $0) }
    }

    /// Model za `grok -m` (nil = privzeti); nil, če ime ni Grok.
    static func match(_ model: String) -> String?? {
        if ids.contains(model) { return .some(nil) }
        return model.hasPrefix(prefix) && model.count > prefix.count ? .some(String(model.dropFirst(prefix.count))) : .none
    }

    /// Modeli računa iz `grok models` (vrstice »  * grok-4.7 (default)« ali »  grok-…«).
    public static func catalog(_ exe: String) -> [String] {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = ["models"]
        let out = Pipe()
        p.standardOutput = out
        p.standardError = FileHandle.nullDevice
        p.standardInput = FileHandle.nullDevice
        guard (try? p.run()) != nil else { return [] }
        let text = String(decoding: out.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
        p.waitUntilExit()
        guard let start = text.range(of: "Available models:") else { return [] }
        return text[start.upperBound...].split(whereSeparator: \.isNewline).compactMap { line in
            let t = line.trimmingCharacters(in: .whitespaces).trimmingCharacters(in: CharacterSet(charactersIn: "*- "))
            return t.split(separator: " ").first.map(String.init).flatMap { $0.isEmpty ? nil : $0 }
        }
    }

    static func args(cwd: URL, extraDirs: [String], agent: Bool, model: String?, write: Bool) -> [String] {
        var a = ["--cwd", cwd.path, "--output-format", "streaming-json", "--prompt-file", "/dev/stdin"]
        if let model { a += ["-m", model] }
        if agent && write {
            // Jedrni sandbox omeji pisanje na mapo seje (dodatnih map »workspace« ne dovoli, zato brez --add-dir);
            // znotraj njega orodja brez vprašanj (headless).
            a += ["--sandbox", "workspace", "--always-approve"]
        } else {
            a += ["--sandbox", "read-only"]
        }
        return a
    }

    struct Failure: Error { let message: String }

    /// Dogodek `streaming-json`: korak (za Activity), besedilo ali konec.
    enum Event: Equatable { case step(String), text(String), toolCall, end(String?), error(String) }

    static func event(_ line: Data) -> Event? {
        guard let o = try? JSONSerialization.jsonObject(with: line) as? [String: Any] else { return nil }
        switch o["type"] as? String {
        case "text": return (o["data"] as? String).map(Event.text)
        case "tool_call":
            let input = o["rawInput"] as? [String: Any] ?? [:]
            let arg = ["command", "target_file", "path", "target_directory", "query", "url"].lazy.compactMap { input[$0] as? String }.first ?? ""
            return .step("\(o["title"] as? String ?? "tool") \(arg.prefix(120))".trimmingCharacters(in: .whitespaces))
        case "end": return .end(o["sessionId"] as? String)
        case "error": return .error(o["message"] as? String ?? "grok error")
        default: return nil
        }
    }

    final class Handle: @unchecked Sendable { let p = Process() }

    /// Zažene grok in vrne končni odgovor (besedilo po zadnjem klicu orodja); prekinitev odjemalca ustavi grok.
    /// Vrne končni odgovor in ID Grokove seje (za nadaljevanje z -r).
    static func run(_ exe: String, args: [String], cwd: URL, prompt: String, env extra: [String: String] = [:],
                    started: @escaping @Sendable (pid_t) -> Void = { _ in }, onStep: @escaping @Sendable (String) -> Void) async throws -> (String, String?) {
        let h = Handle()
        return try await withTaskCancellationHandler {
            try await offPool { () throws -> (String, String?) in
                let p = h.p
                p.executableURL = URL(fileURLWithPath: exe)
                p.arguments = args
                p.currentDirectoryURL = cwd
                p.environment = ProcessInfo.processInfo.environment.filter { $0.key != "SSH_AUTH_SOCK" }  // brez agenta ključev SSH
                    .merging(extra) { $1 }
                let input = Pipe(), events = Pipe(), errors = Pipe()
                p.standardInput = input
                p.standardOutput = events
                p.standardError = errors
                final class Box: @unchecked Sendable { var data = Data() }
                let stderr = Box()
                let errReader = Thread { stderr.data = errors.fileHandleForReading.readDataToEndOfFile() }
                try p.run()
                started(p.processIdentifier)
                AgentCount.running.add(1, ordering: .relaxed)
                defer { AgentCount.running.subtract(1, ordering: .relaxed) }
                errReader.start()
                input.fileHandleForWriting.write(Data(prompt.utf8))
                try? input.fileHandleForWriting.close()
                var buf = Data(), answer = "", failure: String?, session: String?
                while case let chunk = events.fileHandleForReading.availableData, !chunk.isEmpty {
                    buf.append(chunk)
                    while let nl = buf.firstIndex(of: 10) {
                        switch event(buf[buf.startIndex..<nl]) {
                        case .step(let s)?: onStep(s); answer = ""  // odgovor je besedilo po zadnjem orodju
                        case .text(let t)?: answer += t
                        case .error(let m)?: failure = m
                        case .end(let id)?: session = id
                        default: break
                        }
                        buf = Data(buf[(nl + 1)...])
                    }
                }
                p.waitUntilExit()
                while errReader.isExecuting { usleep(10_000) }
                let text = answer.trimmingCharacters(in: .whitespacesAndNewlines)
                guard failure == nil, !text.isEmpty else {
                    let tail = String(decoding: stderr.data.suffix(400), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
                    throw Failure(message: failure ?? "grok exited with \(p.terminationStatus)" + (tail.isEmpty ? "" : ": \(tail)"))
                }
                return (text, session)
            }
        } onCancel: { if h.p.isRunning { h.p.terminate() } }
    }
}

extension Router {
    /// Ključ za nadaljevanje seje agenta: naprava + mapa odjemalca. ID seje routerja in pot priklopa
    /// se pri `minigw -c`/`--resume` zamenjata, zato nista v ključu.
    static func agentScope(_ provider: String, _ req: HTTPRequest, cwd: URL) -> String {
        [provider, req.device?.id ?? "local", req.header("x-claude-mini-cwd") ?? cwd.path].joined(separator: "|")
    }

    func runGrok(_ req: HTTPRequest, body: [String: Any], model: String?, rid: String, writer: ResponseWriter) async -> Int {
        // Grok nima zaščite .git v peskovniku, njegov program pa poganja git zunaj njega (in ~/.grok je pisljiv).
        if (body["tools"] as? [Any])?.isEmpty == false, await Self.shares.isWorktree(session: req.header("x-claude-mini-session") ?? "") {
            let why = await Self.shares.worktreeRefusal(session: req.header("x-claude-mini-session") ?? "",
                                                        worktree: "Grok is not supported in worktree mode yet; use sync (MINIGW_WORK=sync)",
                                                        native: "Grok is not supported yet")
            return await error(400, "invalid_request_error", why, rid: rid, writer: writer)
        }
        if let model, !Self.validModel(model) {
            return await error(400, "invalid_request_error", "Invalid model name", rid: rid, writer: writer)
        }
        guard let exe = await Grok.executable(config.grok.executable) else {
            return await error(503, "api_error", "Grok CLI not found (Mini Gateway → Settings → Grok CLI)", rid: rid, writer: writer)
        }
        let all = ClaudeCLI.turns(body)
        guard let last = all.last, last.role == "user" else {
            return await error(400, "invalid_request_error", "Last message must be from the user", rid: rid, writer: writer)
        }
        let agent = (body["tools"] as? [Any])?.isEmpty == false
        var cwd = config.scratch("chat")
        var extra: [String] = []
        var readOnly = false
        var prompt = ClaudeCLI.prompt(history: all.dropLast(), last: last)
        var sharedDir: URL?
        if agent {
            let clientCwd = req.header("x-claude-mini-cwd")
            let shared = await Self.shares.mount(session: req.header("x-claude-mini-session") ?? "", cwd: clientCwd, base: localBase,
                                                 wait: req.header("x-claude-mini-share") == "1", device: req.device?.id,
                                                 roots: config.claudeCLI.allowedRoots, scratch: config.scratchURL, writes: config.grok.write)
            let synced = await Self.shares.isSync(session: req.header("x-claude-mini-session") ?? "")
            guard let dir = shared ?? (synced ? nil : ClaudeCLI.resolveCwd(clientCwd, roots: config.claudeCLI.allowedRoots, scratch: config.scratchURL)) else {
                if let why = await Self.shares.takeRefusal() {  // zaklep pisanja
                    return await error(409, "invalid_request_error", why, rid: rid, writer: writer)
                }
                if synced, Task.isCancelled { return 499 }  // Claude Code je prekinil med čakanjem na prenos
                if synced { return await error(502, "api_error", "Syncing \(clientCwd ?? "the folder") to the gateway failed (see minigw output)", rid: rid, writer: writer) }
                return await error(400, "invalid_request_error",
                                   "cwd_unavailable: folder \(clientCwd ?? "(none)") does not exist on this Mac or is outside the allowed folders",
                                   rid: rid, writer: writer)
            }
            cwd = dir
            sharedDir = shared
            extra = config.claudeCLI.allowedRoots.split(separator: ",").map { ClaudeCLI.expand($0.trimmingCharacters(in: .whitespaces)) }
                .filter { $0 != "/" && !$0.isEmpty && $0 != dir.path && FileManager.default.fileExists(atPath: $0) }
            readOnly = await Self.isReadOnly(req)
            if let shared, let clientCwd {
                prompt = Self.mountNote(clientCwd, shared, readOnly: readOnly, synced: synced) + "\n\n" + prompt
            }
        } else {
            try? FileManager.default.createDirectory(at: cwd, withIntermediateDirectories: true)
        }
        var args = Grok.args(cwd: cwd, extraDirs: extra, agent: agent, model: model, write: config.grok.write && !readOnly)
        // Nadaljevanje Grokove seje (prebrane datoteke, rezultati ukazov): ključ je dosedanji pogovor v tej mapi.
        let scope = Self.agentScope("grok", req, cwd: cwd)
        if agent, let existing = await Self.grokSessions.get(ClaudeCLI.key(all.dropLast(), scope: scope)) {
            args += ["-r", existing]
            prompt = last.text
        }
        if agent, let note = agentNote("grok") { prompt = note + "\n\n" + prompt }
        let sessions = Self.grokSessions, client = req.header("x-claude-mini-session") ?? ""
        let run = agent ? await startAgentRun(req, kind: "grok", dir: sharedDir) : nil
        defer { run?.stop() }
        let env = run?.env ?? [:]
        return await replyWhenDone(body: body, model: "claude-grok", rid: rid, writer: writer) {
            let (text, session) = try await Grok.run(exe, args: args, cwd: cwd, prompt: prompt, env: env, started: { run?.started($0) }) { [onActivity] s in onActivity(.step(id: rid, text: s)) }
            run?.stop()  // I2: vprašanja drugim modelom se končajo pred prevzemom sprememb
            if agent { await Self.shares.syncBack(session: client) }  // sync: spremembe agenta k odjemalcu pred odgovorom
            if agent, let session {
                await sessions.set(ClaudeCLI.key(all + [ClaudeCLI.Turn(role: "assistant", text: text)], scope: scope), session)
            }
            return (text, [])
        }
    }
}
