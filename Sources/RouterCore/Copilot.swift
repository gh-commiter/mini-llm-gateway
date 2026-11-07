import Foundation

/// Ponudnik `copilot -p` (GitHub Copilot CLI): agent v mapi seje, kot Codex. Copilot nima lastnega peskovnika za vgrajena
/// orodja (urejanje datotek preveri le pot), zato privzeto le branje; pisanje datotek le v mapi seje; ukazi lupine le z
/// izrecnim dovoljenjem in tedaj ves copilot v sandbox-exec (pisanje le v mapo, branje domače mape ne).
/// Copilot se prijavi kot uporabnik tega Maca (copilot login ali žeton GitHub CLI `gh`).
public struct CopilotConfig: Codable, Sendable, Equatable {
    public var enabled = false
    public var executable = ""     // prazno = samodejno iskanje
    public var models = ""         // ločeno z vejico; claude-copilot-<model> za vsakega (Copilot nima seznama modelov)
    public var write = false       // urejanje datotek v mapi seje (sicer le branje)
    public var commands = false    // ukazi lupine (sandbox-exec; z dovoljenjem: agent lahko uporabi prijavo GitHub)
    public var network = false     // spletna orodja (url); omrežja ukazov lupine ne omeji (copilot ga potrebuje sam)
    public init() {}

    public var modelList: [String] {
        models.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }.filter { Router.validModel($0) }
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let d = CopilotConfig()
        enabled = try c.decodeIfPresent(Bool.self, forKey: .enabled) ?? d.enabled
        executable = try c.decodeIfPresent(String.self, forKey: .executable) ?? d.executable
        models = try c.decodeIfPresent(String.self, forKey: .models) ?? d.models
        write = try c.decodeIfPresent(Bool.self, forKey: .write) ?? d.write
        commands = try c.decodeIfPresent(Bool.self, forKey: .commands) ?? d.commands
        network = try c.decodeIfPresent(Bool.self, forKey: .network) ?? d.network
    }
}

public enum Copilot {
    static let ids: Set<String> = ["claude-copilot"]
    static let prefix = "claude-copilot-"

    /// Model za `--model` (nil = privzeti Copilot, »auto«); nil, če ime ni Copilot.
    static func match(_ model: String) -> String?? {
        if ids.contains(model) { return .some(nil) }
        return model.hasPrefix(prefix) && model.count > prefix.count ? .some(String(model.dropFirst(prefix.count))) : .none
    }

    public static let detected: String? = ClaudeCLI.locate("copilot")

    public static func executable(_ configured: String) -> String? {
        configured.isEmpty ? detected : [(configured as NSString).expandingTildeInPath].first { FileManager.default.isExecutableFile(atPath: $0) }
    }

    /// Argumenti. Nikoli --allow-all-tools, --allow-all-paths, --allow-all-urls, --allow-all ali --yolo; v načinu -p
    /// Copilot zavrne vsako orodje brez pravila --allow-tool, zato so zavrnitve tu le za jasnost (deny velja pred allow).
    /// Poti: le mapa seje (in `extraDirs`), brez sistemske začasne mape (tam je predpomnilnik xcrun). Brez vgrajenih
    /// strežnikov MCP (GitHub z uporabnikovo prijavo), brez vprašanj in samodejnih posodobitev; seja po `session`.
    /// Navodilo gre na stdin (brez -p; preverjeno s copilot 1.0.107), ne v ukazno vrstico: to vidijo `ps` in drugi uporabniki.
    static func args(extraDirs: [String], agent: Bool, model: String?, write: Bool, commands: Bool,
                     network: Bool, session: String) -> [String] {
        var a = ["--output-format", "json", "--no-ask-user", "--disable-builtin-mcps", "--disallow-temp-dir", "--no-auto-update", "--no-color",
                 "--disable-client-telemetry", "--secret-env-vars=" + tokenVars.joined(separator: ","), "--session-id", session]
        if let model { a += ["--model", model] }
        for d in extraDirs { a += ["--add-dir", d] }
        var allow: [String] = [], deny = ["memory"]
        if agent { allow.append("read") } else { deny.append("read") }
        if agent && write { allow.append("write") } else { deny.append("write") }
        if agent && write && commands {  // ukazi pišejo mimo pravila write: le s pisanjem
            allow.append("shell")
            deny += ["shell(gh:*)", "shell(security:*)"]  // prijava GitHub in Keychain (najboljši trud: peskovnik je meja)
        } else {
            deny.append("shell")
        }
        if agent && network { allow.append("url") } else { deny.append("url") }
        return a + allow.flatMap { ["--allow-tool", $0] } + deny.flatMap { ["--deny-tool", $0] }
    }

    /// Žetoni, ki jih copilot sprejme iz okolja; ukazom lupine jih skrije (--secret-env-vars).
    static let tokenVars = ["COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"]
    static var gh: String? = ClaudeCLI.locate("gh")  // var: testi podtaknejo lažni gh

    /// Žeton GitHub CLI (`gh auth token`, zunaj peskovnika) za copilot v sandbox-exec: tam ne vidi ~/.config/gh in
    /// Keychaina (preverjeno s copilot 1.0.107: »No authentication information found«). nil, če ima router žeton že v
    /// okolju (ta gre copilotu naprej), ali brez gh oz. prijave.
    static func ghToken(_ env: [String: String]) -> String? {
        guard tokenVars.allSatisfy({ (env[$0] ?? "").isEmpty }), let gh else { return nil }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: gh)
        p.arguments = ["auth", "token", "--hostname", "github.com"]
        p.environment = env.merging(["GH_PROMPT_DISABLED": "1", "GH_NO_UPDATE_NOTIFIER": "1", "xcrun_nocache": "1"]) { $1 }
        let out = Pipe()
        p.standardOutput = out
        p.standardError = FileHandle.nullDevice
        p.standardInput = FileHandle.nullDevice
        guard (try? p.run()) != nil else { return nil }
        let data = out.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        let t = String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        let ok = p.terminationStatus == 0 && (1...255).contains(t.count)
            && t.unicodeScalars.allSatisfy { $0.isASCII && (CharacterSet.alphanumerics.contains($0) || $0 == "_") }
        return ok ? t : nil
    }

    /// Okolje: brez spremenljivk, ki bi notranji CLI usmerile v ta router, brez SSH agenta in nikoli COPILOT_ALLOW_ALL
    /// (»true« odobri vsa orodja in zaupa mapi s hooki) ali COPILOT_ASSISTED_APPROVAL; svoj COPILOT_HOME.
    static func environment(_ base: [String: String], home: URL, extra: [String: String] = [:]) -> [String: String] {
        var env = base.filter { k, _ in
            !(k.hasPrefix("ANTHROPIC_") || k.hasPrefix("CLAUDE_CODE_") || k == "CLAUDECODE" || k == "SSH_AUTH_SOCK"
              || k.hasPrefix("COPILOT_ALLOW") || k == "COPILOT_ASSISTED_APPROVAL" || k == "COPILOT_HOME")
        }
        env["COPILOT_HOME"] = home.path
        env["COPILOT_AUTO_UPDATE"] = "false"
        env["xcrun_nocache"] = "1"
        return env.merging(extra) { $1 }
    }

    /// Dogodek `--output-format json` (JSONL): odgovor (assistant.message), korak (tool.*) za Activity.
    static func event(_ line: Data) -> (text: String?, step: String?) {
        guard let o = try? JSONSerialization.jsonObject(with: line) as? [String: Any], let type = o["type"] as? String else { return (nil, nil) }
        let d = o["data"] as? [String: Any] ?? [:]
        if type == "assistant.message" { return ((d["content"] as? String).flatMap { $0.isEmpty ? nil : $0 }, nil) }
        if type.hasPrefix("tool.") && type.hasSuffix("start") {
            let name = d["toolName"] as? String ?? d["name"] as? String ?? "tool"
            return (nil, String(name.prefix(120)))
        }
        return (nil, nil)
    }

    struct Failure: Error { let message: String }

    final class Handle: @unchecked Sendable { let p = Process() }

    /// Zažene copilot (`sandbox`: profil sandbox-exec za ukaze lupine) in vrne zadnje sporočilo agenta.
    static func run(_ exe: String, args: [String], prompt: String, cwd: URL, env: [String: String], sandbox: String?,
                    onStep: @escaping @Sendable (String) -> Void) async throws -> String {
        let h = Handle()
        return try await withTaskCancellationHandler {
            try await Task.detached { () throws -> String in
                let p = h.p
                p.executableURL = URL(fileURLWithPath: sandbox == nil ? exe : "/usr/bin/sandbox-exec")
                p.arguments = sandbox.map { ["-p", $0, exe] + args } ?? args
                p.currentDirectoryURL = cwd
                p.environment = env
                let input = Pipe(), events = Pipe(), errors = Pipe()
                p.standardInput = input
                p.standardOutput = events
                p.standardError = errors
                var stderr = Data()
                let errReader = Thread { stderr = errors.fileHandleForReading.readDataToEndOfFile() }
                try p.run()
                AgentCount.running.add(1, ordering: .relaxed)
                defer { AgentCount.running.subtract(1, ordering: .relaxed) }
                errReader.start()
                _ = fcntl(input.fileHandleForWriting.fileDescriptor, F_SETNOSIGPIPE, 1)  // EPIPE namesto SIGPIPE
                Thread {  // ločeno od branja dogodkov: velik pogovor ne zaklene cevi; try?: copilot je lahko že končal
                    try? input.fileHandleForWriting.write(contentsOf: Data(prompt.utf8))
                    try? input.fileHandleForWriting.close()
                }.start()
                var buf = Data(), answer = ""
                while case let chunk = events.fileHandleForReading.availableData, !chunk.isEmpty {
                    buf.append(chunk)
                    while let nl = buf.firstIndex(of: 10) {
                        let e = event(buf[buf.startIndex..<nl])
                        if let t = e.text { answer = t }  // odgovor je zadnje sporočilo agenta
                        if let s = e.step { onStep(s) }
                        buf = Data(buf[(nl + 1)...])
                    }
                }
                if let t = event(buf).text { answer = t }
                while errReader.isExecuting { usleep(10_000) }
                p.waitUntilExit()
                let text = answer.trimmingCharacters(in: .whitespacesAndNewlines)
                guard p.terminationStatus == 0, !text.isEmpty else {
                    let tail = String(decoding: stderr.suffix(400), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
                    throw Failure(message: "copilot exited with \(p.terminationStatus)" + (tail.isEmpty ? "" : ": \(tail)"))
                }
                return text
            }.value
        } onCancel: { if h.p.isRunning { h.p.terminate() } }
    }
}

extension Router {
    static let copilotSessions = SessionRegistry()

    func runCopilot(_ req: HTTPRequest, body: [String: Any], model: String?, rid: String, writer: ResponseWriter) async -> Int {
        let cfg = config.copilot
        let agent = (body["tools"] as? [Any])?.isEmpty == false
        if !cfg.write, agent, await Self.shares.isWorktree(session: req.header("x-claude-mini-session") ?? "") {
            return await error(400, "invalid_request_error",
                               "Copilot may not write (Mini Gateway → Settings → Copilot CLI), so worktree mode is not possible; use sync",
                               rid: rid, writer: writer)
        }
        if let model, !Self.validModel(model) {
            return await error(400, "invalid_request_error", "Invalid model name", rid: rid, writer: writer)
        }
        guard let exe = Copilot.executable(cfg.executable) else {
            return await error(503, "api_error", "Copilot CLI not found (Mini Gateway → Settings → Copilot CLI)", rid: rid, writer: writer)
        }
        let all = ClaudeCLI.turns(body)
        guard let last = all.last, last.role == "user" else {
            return await error(400, "invalid_request_error", "Last message must be from the user", rid: rid, writer: writer)
        }
        var cwd = config.scratch("chat")
        var extra: [String] = []
        var readOnly = false
        var tree: (root: URL, branch: String, maven: URL)?
        var prompt = ClaudeCLI.prompt(history: all.dropLast(), last: last)
        if agent {
            let clientCwd = req.header("x-claude-mini-cwd")
            let shared = await Self.shares.mount(session: req.header("x-claude-mini-session") ?? "", cwd: clientCwd, base: localBase,
                                                 wait: req.header("x-claude-mini-share") == "1", device: req.device?.id,
                                                 roots: config.claudeCLI.allowedRoots)
            let synced = await Self.shares.isSync(session: req.header("x-claude-mini-session") ?? "")
            guard let dir = shared ?? (synced ? nil : ClaudeCLI.resolveCwd(clientCwd, roots: config.claudeCLI.allowedRoots)) else {
                if synced, Task.isCancelled { return 499 }
                if synced { return await error(502, "api_error", "Syncing \(clientCwd ?? "the folder") to the gateway failed (see minigw output)", rid: rid, writer: writer) }
                return await error(400, "invalid_request_error",
                                   "cwd_unavailable: folder \(clientCwd ?? "(none)") does not exist on this Mac or is outside the allowed folders",
                                   rid: rid, writer: writer)
            }
            cwd = dir
            tree = await Self.shares.worktree(session: req.header("x-claude-mini-session") ?? "")
            if let tree, tree.root.path != dir.path { extra = [tree.root.path] }  // le klon, ne dovoljene mape
            if let shared, let clientCwd {
                readOnly = await Self.shares.isReadOnly(session: req.header("x-claude-mini-session") ?? "")
                prompt = Self.mountNote(clientCwd, shared, readOnly: readOnly, synced: synced, branch: tree?.branch) + "\n\n" + prompt
            }
        } else {
            try? FileManager.default.createDirectory(at: cwd, withIntermediateDirectories: true)
        }
        // Svoj COPILOT_HOME (seje, dnevniki) v Scratch; prijava ostane uporabnikova (Keychain ali gh).
        let home = config.scratch("copilot")
        let write = cfg.write && !readOnly, commands = agent && cfg.commands && write  // peskovnik piše v mapo: ukazi le s pisanjem (tudi ne pri --ro)
        // Ukazi lupine: ves copilot v peskovniku kot minigw run (pisanje le v mapo, domača mapa in Scratch skrita),
        // COPILOT_HOME in začasna mapa zahteve dovoljeni posebej.
        let privateTmp = URL(fileURLWithPath: "/private/tmp/minigw-copilot-\(rid)", isDirectory: true)
        try? FileManager.default.createDirectory(at: privateTmp, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        defer { try? FileManager.default.removeItem(at: privateTmp) }
        var sandbox: String?
        if commands {
            guard let p = Native.sandboxProfile(dir: (tree?.root ?? cwd).path, network: true,
                                                hidden: [config.scratchURL.path, RouterConfig.defaultScratch.path], routerPort: Int(config.port),
                                                runTmp: Sync.real(home)?.path) else {
                return await error(403, "permission_error", "Copilot: the folder or a build cache folder is a link", rid: rid, writer: writer)
            }
            sandbox = p
        }
        var extraEnv = tree.map { Worktree.harnessEnv(root: $0.root, maven: $0.maven) }
            ?? ["TMPDIR": privateTmp.path + "/", "CLANG_MODULE_CACHE_PATH": privateTmp.appendingPathComponent("ModuleCache").path]
        let base = ProcessInfo.processInfo.environment
        if commands, Copilot.tokenVars.allSatisfy({ (base[$0] ?? "").isEmpty }) {
            // V peskovniku copilot prijave ne najde sam: žeton pošlje router (ukazom ga copilot skrije).
            guard let t = Copilot.ghToken(base) else {
                return await error(503, "api_error",
                                   "Copilot with commands runs in a sandbox that hides your GitHub sign-in; sign in with the GitHub CLI (gh auth login)",
                                   rid: rid, writer: writer)
            }
            extraEnv["COPILOT_GITHUB_TOKEN"] = t
        }
        let env = Copilot.environment(base, home: home, extra: extraEnv)
        // Nadaljevanje seje Copilot (--session-id): ključ je dosedanji pogovor v tej mapi.
        let scope = Self.agentScope("copilot", req, cwd: cwd)
        let existing = agent ? await Self.copilotSessions.get(ClaudeCLI.key(all.dropLast(), scope: scope)) : nil
        if existing != nil { prompt = last.text }
        let session = existing ?? UUID().uuidString.lowercased()
        let args = Copilot.args(extraDirs: extra, agent: agent, model: model, write: write, commands: commands,
                                network: cfg.network && (tree == nil || config.worktreeCodexNetwork), session: session)
        let sessions = Self.copilotSessions, client = req.header("x-claude-mini-session") ?? ""
        return await replyWhenDone(body: body, model: "claude-copilot", rid: rid, writer: writer) {
            let text = try await Copilot.run(exe, args: args, prompt: prompt, cwd: cwd, env: env, sandbox: sandbox) { [onActivity] s in
                onActivity(.step(id: rid, text: s))
            }
            if agent { await Self.shares.syncBack(session: client) }  // sync: spremembe agenta k odjemalcu pred odgovorom
            if agent { await sessions.set(ClaudeCLI.key(all + [ClaudeCLI.Turn(role: "assistant", text: text)], scope: scope), session) }
            return (text, [])
        }
    }
}
