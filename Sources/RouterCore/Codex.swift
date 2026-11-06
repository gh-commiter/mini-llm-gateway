import Foundation

/// Ponudnik `codex exec` (OpenAI Codex CLI): agent v mapi seje, kot `claude -p`.
public struct CodexConfig: Codable, Sendable, Equatable {
    public var enabled = false
    public var executable = ""            // prazno = samodejno iskanje
    public var enabledModels: [String]?   // nil = vsi modeli računa (tudi prihodnji); seznam = le ti
    public var write = true               // sandbox workspace-write (sicer read-only)
    public var network = true             // omrežje za ukaze v sandboxu (npr. gradnja)
    public init() {}

    public func isModelEnabled(_ slug: String) -> Bool { enabledModels?.contains(slug) ?? true }

    public mutating func setModel(_ slug: String, _ on: Bool, all: [String]) {
        var list = enabledModels ?? all
        list.removeAll { $0 == slug }
        if on { list.append(slug) }
        enabledModels = Set(list) == Set(all) ? nil : list
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let d = CodexConfig()
        enabled = try c.decodeIfPresent(Bool.self, forKey: .enabled) ?? d.enabled
        executable = try c.decodeIfPresent(String.self, forKey: .executable) ?? d.executable
        enabledModels = try c.decodeIfPresent([String].self, forKey: .enabledModels)
        write = try c.decodeIfPresent(Bool.self, forKey: .write) ?? d.write
        network = try c.decodeIfPresent(Bool.self, forKey: .network) ?? d.network
    }
}

public enum Codex {
    /// Claude Code pokaže le modele z »claude« v imenu: »claude-codex« (privzeti model Codex) in »claude-codex-<slug>«.
    static let ids: Set<String> = ["claude-codex"]
    static let prefix = "claude-codex-"

    /// Model za `codex -m` (nil = privzeti iz nastavitev Codex); nil, če ime ni Codex.
    static func match(_ model: String) -> String?? {
        if ids.contains(model) { return .some(nil) }
        return model.hasPrefix(prefix) && model.count > prefix.count ? .some(String(model.dropFirst(prefix.count))) : .none
    }

    public struct Model: Sendable { public let slug: String, name: String, description: String, window: Int? }

    /// Modeli računa (`codex debug models`, vidni v izbirniku Codex); ob napaki prazno.
    public static func catalog(_ exe: String) -> [Model] {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = ["debug", "models"]
        let out = Pipe()
        p.standardOutput = out
        p.standardError = FileHandle.nullDevice
        p.standardInput = FileHandle.nullDevice
        guard (try? p.run()) != nil else { return [] }
        let data = out.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        let json = try? JSONSerialization.jsonObject(with: data)
        let items = (json as? [[String: Any]]) ?? ((json as? [String: Any])?["models"] as? [[String: Any]]) ?? []
        return items.compactMap { m in
            guard let slug = m["slug"] as? String, m["visibility"] as? String != "hide" else { return nil }
            return Model(slug: slug, name: m["display_name"] as? String ?? slug, description: m["description"] as? String ?? "",
                         window: m["context_window"] as? Int)
        }
    }
    public static let detected: String? = ClaudeCLI.locate("codex")

    public static func executable(_ configured: String) -> String? {
        configured.isEmpty ? detected : [(configured as NSString).expandingTildeInPath].first { FileManager.default.isExecutableFile(atPath: $0) }
    }

    /// Argumenti: agent piše le v mapi seje in dovoljenih mapah (sandbox Codex), omrežje za gradnjo dovoljeno.
    /// Brez orodij (naslovi, povzetki) le branje v začasni mapi.
    /// `resume`: ID niti Codex za nadaljevanje (možnosti exec, tudi sandbox, veljajo tudi zanj).
    static func args(cwd: URL, extraDirs: [String], agent: Bool, output: URL, model: String? = nil,
                     write: Bool = true, network: Bool = true, resume: String? = nil) -> [String] {
        var a = ["exec", "--json", "--skip-git-repo-check", "--color", "never", "-C", cwd.path, "-o", output.path]
        if let model { a += ["-m", model] }
        if agent && write {
            a += ["--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=\(network)"]
            for d in extraDirs { a += ["--add-dir", d] }
        } else {
            a += ["--sandbox", "read-only"]
        }
        if !agent { a.append("--ephemeral") }  // naslovi, povzetki: brez zapisa seje
        if let resume { a += ["resume", resume] }
        return a + ["-"]  // navodila prek stdin (brez omejitve dolžine argumentov)
    }

    struct Failure: Error { let message: String }

    final class Handle: @unchecked Sendable { let p = Process() }

    /// Zažene codex in vrne zadnje sporočilo agenta; prekinitev odjemalca ustavi codex.
    /// Korak iz dogodka `codex exec --json` (ukaz ali vmesno sporočilo agenta), za Activity.
    static func step(_ line: Data) -> String? {
        guard let obj = try? JSONSerialization.jsonObject(with: line) as? [String: Any],
              let item = obj["item"] as? [String: Any] else { return nil }
        switch (obj["type"] as? String, item["type"] as? String) {
        case ("item.started", "command_execution"?):
            var cmd = item["command"] as? String ?? ""
            if let r = cmd.range(of: "-lc '"), cmd.hasSuffix("'") { cmd = String(cmd[r.upperBound...].dropLast()) }  // /bin/zsh -lc '…'
            return "$ " + cmd.prefix(120)
        case ("item.completed", "agent_message"?):
            return (item["text"] as? String).map { String($0.split(separator: "\n").first ?? "").prefix(120) }.map(String.init)
        case ("item.started", let t?) where t != "reasoning":
            return t.replacingOccurrences(of: "_", with: " ")
        default:
            return nil
        }
    }

    /// ID niti iz `{"type":"thread.started","thread_id":…}`.
    static func threadId(_ line: Data) -> String? {
        guard let o = try? JSONSerialization.jsonObject(with: line) as? [String: Any], o["type"] as? String == "thread.started" else { return nil }
        return o["thread_id"] as? String
    }

    static func run(_ exe: String, args: [String], cwd: URL, prompt: String, output: URL,
                    onStep: @escaping @Sendable (String) -> Void = { _ in }) async throws -> (String, String?) {
        let h = Handle()
        return try await withTaskCancellationHandler {
            try await Task.detached { () throws -> (String, String?) in
                let p = h.p
                p.executableURL = URL(fileURLWithPath: exe)
                p.arguments = args
                p.currentDirectoryURL = cwd
                let input = Pipe(), errors = Pipe()
                let events = Pipe()
                p.standardInput = input
                p.standardOutput = events
                p.standardError = errors
                var stderr = Data()
                let errReader = Thread { stderr = errors.fileHandleForReading.readDataToEndOfFile() }  // ne blokira pipe
                try p.run()
                errReader.start()
                input.fileHandleForWriting.write(Data(prompt.utf8))
                try? input.fileHandleForWriting.close()
                var buf = Data(), thread: String?
                while case let chunk = events.fileHandleForReading.availableData, !chunk.isEmpty {
                    buf.append(chunk)
                    while let nl = buf.firstIndex(of: 10) {
                        let line = buf[buf.startIndex..<nl]
                        if thread == nil { thread = threadId(line) }
                        if let s = step(line) { onStep(s) }
                        buf = Data(buf[(nl + 1)...])
                    }
                }
                while errReader.isExecuting { usleep(10_000) }
                p.waitUntilExit()
                let text = (try? String(contentsOf: output, encoding: .utf8))?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                guard p.terminationStatus == 0, !text.isEmpty else {
                    let tail = String(decoding: stderr.suffix(400), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
                    throw Failure(message: "codex exited with \(p.terminationStatus)" + (tail.isEmpty ? "" : ": \(tail)"))
                }
                return (text, thread)
            }.value
        } onCancel: { if h.p.isRunning { h.p.terminate() } }
    }
}

extension Router {
    func runCodex(_ req: HTTPRequest, body: [String: Any], model: String?, rid: String, writer: ResponseWriter) async -> Int {
        guard let exe = Codex.executable(config.codex.executable) else {
            return await error(503, "api_error", "Codex CLI not found (Mini Gateway → Settings → Claude CLI → Codex)", rid: rid, writer: writer)
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
        if agent {
            let clientCwd = req.header("x-claude-mini-cwd")
            let shared = await Self.shares.mount(session: req.header("x-claude-mini-session") ?? "", cwd: clientCwd, base: localBase,
                                                 wait: req.header("x-claude-mini-share") == "1")
            guard let dir = shared ?? ClaudeCLI.resolveCwd(clientCwd, roots: config.claudeCLI.allowedRoots) else {
                return await error(400, "invalid_request_error",
                                   "cwd_unavailable: folder \(clientCwd ?? "(none)") does not exist on this Mac or is outside the allowed folders",
                                   rid: rid, writer: writer)
            }
            cwd = dir
            extra = config.claudeCLI.allowedRoots.split(separator: ",").map { ClaudeCLI.expand($0.trimmingCharacters(in: .whitespaces)) }
                .filter { $0 != "/" && !$0.isEmpty && $0 != dir.path && FileManager.default.fileExists(atPath: $0) }
            if let shared, let clientCwd {
                readOnly = await Self.shares.isReadOnly(session: req.header("x-claude-mini-session") ?? "")
                prompt = Self.mountNote(clientCwd, shared, readOnly: readOnly) + "\n\n" + prompt
            }
        } else {
            try? FileManager.default.createDirectory(at: cwd, withIntermediateDirectories: true)
        }
        let output = config.scratch("tmp").appendingPathComponent("codex-\(rid).txt")
        defer { try? FileManager.default.removeItem(at: output) }
        // Nadaljevanje niti Codex (prebrane datoteke, rezultati ukazov): ključ je dosedanji pogovor v tej mapi.
        let scope = Self.agentScope("codex", req, cwd: cwd)
        let existing = agent ? await Self.codexSessions.get(ClaudeCLI.key(all.dropLast(), scope: scope)) : nil
        if existing != nil { prompt = last.text }
        let args = Codex.args(cwd: cwd, extraDirs: extra, agent: agent, output: output, model: model,
                              write: config.codex.write && !readOnly, network: config.codex.network, resume: existing)
        let sessions = Self.codexSessions
        return await replyWhenDone(body: body, model: "claude-codex", rid: rid, writer: writer) {
            let (text, thread) = try await Codex.run(exe, args: args, cwd: cwd, prompt: prompt, output: output) { [onActivity] s in
                onActivity(.step(id: rid, text: s))
            }
            if agent, let thread {
                await sessions.set(ClaudeCLI.key(all + [ClaudeCLI.Turn(role: "assistant", text: text)], scope: scope), thread)
            }
            return (text, [])
        }
    }

    /// Ponudnik brez pretoka (Siri, Codex): pri pretoku glave in message_start takoj, med čakanjem ping, nato cel odgovor.
    func replyWhenDone(body: [String: Any], model: String, rid: String, writer: ResponseWriter,
                       work: () async throws -> (String, [(String, String)])) async -> Int {
        let stream = body["stream"] as? Bool == true
        var t = StreamTranslator(model: body["model"] as? String ?? model)
        var pinger: Task<Void, Never>?
        do {
            if stream {
                try await writer.write(HTTPResponse.head(status: 200, headers: [
                    ("Content-Type", "text/event-stream"), ("Cache-Control", "no-cache"), ("request-id", rid),
                ]) + t.start().reduce(Data(), +))
                pinger = Task {
                    while !Task.isCancelled {
                        try? await Task.sleep(for: .seconds(10))
                        if Task.isCancelled { break }
                        try? await writer.write(StreamTranslator.sse("ping", ["type": "ping"]))
                    }
                }
            }
            let (text, extra) = try await work()
            pinger?.cancel()
            let delta = t.emit(text)
            t.usage = ["output_tokens": text.utf8.count / 4]
            if stream {
                try await writer.write(delta + t.finish().reduce(Data(), +))
                return 200
            }
            let data = (try? JSONSerialization.data(withJSONObject: t.message(), options: [.withoutEscapingSlashes])) ?? Data()
            try? await writer.write(HTTPResponse.head(status: 200, headers: [("Content-Type", "application/json"), ("request-id", rid)] + extra,
                                                      contentLength: data.count) + data)
            return 200
        } catch {
            pinger?.cancel()
            let (status, type, message) = (error as? SiriError)?.status
                ?? (error as? Codex.Failure).map { (502, "api_error", $0.message) }
                ?? (error as? Grok.Failure).map { (502, "api_error", $0.message) } ?? (500, "api_error", "\(error)")
            Self.log.error("\(rid, privacy: .public) \(model, privacy: .public) failed: \(message, privacy: .public)")
            if stream {
                try? await writer.write(StreamTranslator.sse("error", ["type": "error", "error": ["type": type, "message": message]]))
                return status
            }
            return await self.error(status, type, message, rid: rid, writer: writer)
        }
    }
}
