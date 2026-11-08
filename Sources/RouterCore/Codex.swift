import Foundation

/// Ponudnik `codex exec` (OpenAI Codex CLI): agent v mapi seje, kot `claude -p`.
public struct CodexConfig: Codable, Sendable, Equatable {
    public var enabled = false
    public var executable = ""            // prazno = samodejno iskanje
    public var enabledModels: [String]?   // nil = vsi modeli računa (tudi prihodnji); seznam = le ti
    public var write = true               // sandbox workspace-write (sicer read-only)
    public var network = false            // omrežje za ukaze v sandboxu (npr. gradnja); privzeto izklopljeno kot v Go
    public var askOthers = false          // »May ask other models«: agent vpraša drug model prehoda (AgentRun)
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
        askOthers = try c.decodeIfPresent(Bool.self, forKey: .askOthers) ?? d.askOthers
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
    /// Za nastavitve (glavna nit); router: `await executable("")`.
    public static var detected: String? { Detected.pathNow("codex") }

    public static func executable(_ configured: String) async -> String? {
        configured.isEmpty ? await Detected.path("codex") : [(configured as NSString).expandingTildeInPath].first { FileManager.default.isExecutableFile(atPath: $0) }
    }

    /// Argumenti: agent piše le v mapi seje in dovoljenih mapah (sandbox Codex), omrežje za gradnjo dovoljeno.
    /// Brez orodij (naslovi, povzetki) le branje v začasni mapi.
    /// `resume`: ID niti Codex za nadaljevanje (možnosti exec, tudi sandbox, veljajo tudi zanj).
    static func args(cwd: URL, extraDirs: [String], agent: Bool, output: URL, model: String? = nil,
                     write: Bool = true, network: Bool = false, resume: String? = nil) -> [String] {
        var a = ["exec", "--json", "--skip-git-repo-check", "--color", "never", "-C", cwd.path, "-o", output.path]
        if let model { a += ["-m", model] }
        if agent && write {
            // $TMPDIR (/var/folders/…/T) ni pisljiv: tam je predpomnilnik xcrun (shim /usr/bin/git ga izvede); TMPDIR je zasebna mapa.
            a += ["--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=\(network)",
                  "-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true"]
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

    static func run(_ exe: String, args: [String], cwd: URL, prompt: String, output: URL, env extra: [String: String] = [:],
                    started: @escaping @Sendable (pid_t) -> Void = { _ in },
                    onStep: @escaping @Sendable (String) -> Void = { _ in }) async throws -> (String, String?) {
        let h = Handle()
        return try await withTaskCancellationHandler {
            try await offPool { () throws -> (String, String?) in
                let p = h.p
                p.executableURL = URL(fileURLWithPath: exe)
                p.arguments = args
                p.currentDirectoryURL = cwd
                p.environment = ProcessInfo.processInfo.environment.filter { $0.key != "SSH_AUTH_SOCK" }  // brez agenta ključev SSH
                    .merging(extra) { $1 }
                let input = Pipe(), errors = Pipe()
                let events = Pipe()
                p.standardInput = input
                p.standardOutput = events
                p.standardError = errors
                var stderr = Data()
                let errReader = Thread { stderr = errors.fileHandleForReading.readDataToEndOfFile() }  // ne blokira pipe
                try p.run()
                started(p.processIdentifier)
                AgentCount.running.add(1, ordering: .relaxed)
                defer { AgentCount.running.subtract(1, ordering: .relaxed) }
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
            }
        } onCancel: { if h.p.isRunning { h.p.terminate() } }
    }
}

extension Router {
    func runCodex(_ req: HTTPRequest, body: [String: Any], model: String?, rid: String, writer: ResponseWriter) async -> Int {
        // worktree: agent mora pisati (klon); lastnikova nastavitev »brez pisanja« velja, zato zavrni.
        if !config.codex.write, (body["tools"] as? [Any])?.isEmpty == false,
           await Self.shares.isWorktree(session: req.header("x-claude-mini-session") ?? "") {
            let why = await Self.shares.worktreeRefusal(session: req.header("x-claude-mini-session") ?? "",
                                                         worktree: "Codex may not write (Mini Gateway → Settings → Codex), so worktree mode is not possible; use sync",
                                                         native: "Codex may not write (Mini Gateway → Settings → Codex), so it cannot work")
            return await error(400, "invalid_request_error", why, rid: rid, writer: writer)
        }
        if let model, !Self.validModel(model) {
            return await error(400, "invalid_request_error", "Invalid model name", rid: rid, writer: writer)
        }
        guard let exe = await Codex.executable(config.codex.executable) else {
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
        var tree: (root: URL, branch: String, maven: URL, read: [URL])?
        var prompt = ClaudeCLI.prompt(history: all.dropLast(), last: last)
        var sharedDir: URL?
        if agent {
            let clientCwd = req.header("x-claude-mini-cwd")
            let shared = await Self.shares.mount(session: req.header("x-claude-mini-session") ?? "", cwd: clientCwd, base: localBase,
                                                 wait: req.header("x-claude-mini-share") == "1", device: req.device?.id,
                                                 roots: config.claudeCLI.allowedRoots, scratch: config.scratchURL, writes: config.codex.write)
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
                .filter { !RouterConfig.overlapsScratch($0, scratch: config.scratchURL) && !RouterConfig.overlapsCopilotCache($0) }  // H1, #26
            // worktree: pisljiv le klon (koren, ne dovoljene mape)
            tree = await Self.shares.worktree(session: req.header("x-claude-mini-session") ?? "")
            if let tree {
                try? FileManager.default.createDirectory(at: tree.maven, withIntermediateDirectories: true)
                extra = (tree.root.path == dir.path ? [] : [tree.root.path]) + [tree.maven.path]
            }
            if let shared, let clientCwd {
                readOnly = await Self.shares.isReadOnly(session: req.header("x-claude-mini-session") ?? "")
                prompt = Self.mountNote(clientCwd, shared, readOnly: readOnly, synced: synced, branch: tree?.branch) + "\n\n" + prompt
            }
        } else {
            try? FileManager.default.createDirectory(at: cwd, withIntermediateDirectories: true)
        }
        let output = config.scratch("tmp").appendingPathComponent("codex-\(rid).txt")
        defer { try? FileManager.default.removeItem(at: output) }
        // Zasebni TMPDIR v /private/tmp (pisljiv v sandboxu Codex), ne $TMPDIR uporabnika (glej Codex.args).
        let privateTmp = URL(fileURLWithPath: "/private/tmp/minigw-codex-\(rid)", isDirectory: true)
        try? FileManager.default.createDirectory(at: privateTmp, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        defer { try? FileManager.default.removeItem(at: privateTmp) }
        let run = agent ? await startAgentRun(req, kind: "codex", dir: sharedDir) : nil
        defer { run?.stop() }
        let codexEnv = (tree.map { Worktree.harnessEnv(root: $0.root, maven: $0.maven) }
            ?? ["TMPDIR": privateTmp.path + "/", "CLANG_MODULE_CACHE_PATH": privateTmp.appendingPathComponent("ModuleCache").path, "xcrun_nocache": "1"])
            .merging(run?.env ?? [:]) { $1 }
        // Nadaljevanje niti Codex (prebrane datoteke, rezultati ukazov): ključ je dosedanji pogovor v tej mapi.
        let scope = Self.agentScope("codex", req, cwd: cwd)
        let existing = agent ? await Self.codexSessions.get(ClaudeCLI.key(all.dropLast(), scope: scope)) : nil
        if existing != nil { prompt = last.text }
        if agent, let note = agentNote("codex") { prompt = note + "\n\n" + prompt }
        let args = Codex.args(cwd: cwd, extraDirs: extra, agent: agent, output: output, model: model,
                              write: config.codex.write && !readOnly, network: config.codex.network && (tree == nil || config.worktreeCodexNetwork),
                              resume: existing)
        let sessions = Self.codexSessions, session = req.header("x-claude-mini-session") ?? ""
        return await replyWhenDone(body: body, model: "claude-codex", rid: rid, writer: writer) {
            let (text, thread) = try await Codex.run(exe, args: args, cwd: cwd, prompt: prompt, output: output,
                                                         env: codexEnv, started: { run?.started($0) }) { [onActivity] s in
                onActivity(.step(id: rid, text: s))
            }
            run?.stop()  // I2: vprašanja drugim modelom se končajo pred prevzemom sprememb
            if agent { await Self.shares.syncBack(session: session) }  // sync: spremembe agenta k odjemalcu pred odgovorom
            if agent, let thread {
                await sessions.set(ClaudeCLI.key(all + [ClaudeCLI.Turn(role: "assistant", text: text)], scope: scope), thread)
            }
            return (text, [])
        }
    }

    /// Ime modela za -m (Codex, Grok): le [A-Za-z0-9._:/-], ne na začetku »-« (sicer bi bila zastavica CLI).
    static func validModel(_ m: String) -> Bool {
        !m.isEmpty && !m.hasPrefix("-") && m.allSatisfy { $0.isASCII && ($0.isLetter || $0.isNumber || "._:/-".contains($0)) }
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
                ?? (error as? Grok.Failure).map { (502, "api_error", $0.message) }
                ?? (error as? Copilot.Failure).map { (502, "api_error", $0.message) } ?? (500, "api_error", "\(error)")
            Self.log.error("\(rid, privacy: .public) \(model, privacy: .public) failed: \(message, privacy: .public)")
            if stream {
                try? await writer.write(StreamTranslator.sse("error", ["type": "error", "error": ["type": type, "message": message]]))
                return status
            }
            return await self.error(status, type, message, rid: rid, writer: writer)
        }
    }
}
