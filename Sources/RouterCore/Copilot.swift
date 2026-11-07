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

    /// Argumenti. Nikoli --allow-all-tools, --allow-all-paths, --allow-all-urls, --allow-all, --yolo, --trust-folder ali
    /// --add-dir (ta naloži .github/skills in .github/agents mape kot zaupanja vredne; delovna mapa je koren klona). V načinu
    /// brez vprašanj Copilot zavrne vsako orodje brez pravila --allow-tool; zavrnitve so tu še izrecno (deny velja pred allow).
    /// Brez sistemske začasne mape, vgrajenih strežnikov MCP (GitHub s prijavo), vprašanj in samodejnih posodobitev (zažene
    /// vgrajeno različico, ne paketa iz ~/Library/Caches/copilot); seja po `session`.
    /// Navodilo gre na stdin (brez -p; preverjeno s copilot 1.0.107), ne v ukazno vrstico: to vidijo `ps` in drugi uporabniki.
    static func args(agent: Bool, model: String?, write: Bool, commands: Bool, network: Bool, session: String) -> [String] {
        var a = ["--output-format", "json", "--no-ask-user", "--disable-builtin-mcps", "--disallow-temp-dir", "--no-auto-update", "--no-color",
                 "--disable-client-telemetry", "--secret-env-vars=" + tokenVars.joined(separator: ","), "--session-id", session]
        if let model { a += ["--model", model] }
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
    /// Keychain: žeton za ukaze (fine-grained PAT le z dovoljenjem »Copilot Requests«), nastavi `minigw gw copilot token`.
    public static let tokenAccount = "copilot-token"
    static var gh: String? = ClaudeCLI.locate("gh")  // var: testi podtaknejo lažni gh

    /// Žeton za ukaze: fine-grained PAT (github_pat_…), natisljiv ASCII, brez presledkov.
    public static func validToken(_ t: String) -> Bool {
        t.hasPrefix("github_pat_") && t.count <= 255 && t.unicodeScalars.allSatisfy { (0x21...0x7e).contains($0.value) }
    }

    /// Žeton GitHub CLI (`gh auth token`, zunaj peskovnika; največ 10 s) za copilot v sandbox-exec: tam ne vidi
    /// ~/.config/gh in Keychaina (copilot 1.0.107: »No authentication information found«). nil brez gh ali prijave.
    static func ghToken() -> String? {
        guard let gh else { return nil }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: gh)
        p.arguments = ["auth", "token", "--hostname", "github.com"]
        p.environment = ProcessInfo.processInfo.environment.filter { !tokenVars.contains($0.key) }
            .merging(["GH_PROMPT_DISABLED": "1", "GH_NO_UPDATE_NOTIFIER": "1", "xcrun_nocache": "1"]) { $1 }
        let out = Pipe()
        p.standardOutput = out
        p.standardError = FileHandle.nullDevice
        p.standardInput = FileHandle.nullDevice
        guard (try? p.run()) != nil else { return nil }
        let deadline = Date().addingTimeInterval(10)
        while p.isRunning && Date() < deadline { usleep(20_000) }
        if p.isRunning { p.terminate(); return nil }
        let data = out.fileHandleForReading.readDataToEndOfFile()  // žeton je kratek: cev se ne napolni
        let t = String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        let ok = p.terminationStatus == 0 && (1...255).contains(t.count)
            && t.unicodeScalars.allSatisfy { $0.isASCII && (CharacterSet.alphanumerics.contains($0) || $0 == "_") }
        return ok ? t : nil
    }

    /// Okolje: brez spremenljivk, ki bi notranji CLI usmerile v ta router, brez SSH agenta in brez vseh COPILOT_* uporabnika
    /// (COPILOT_ALLOW_ALL odobri vsa orodja, COPILOT_PROVIDER_API_KEY_COMMAND izvede ukaz, COPILOT_CLI_DIST_DIR izbere paket …)
    /// ali žetonov iz okolja; svoj COPILOT_HOME in žeton (`token`).
    static func environment(_ base: [String: String], home: URL, token: String, extra: [String: String] = [:]) -> [String: String] {
        var env = base.filter { k, _ in
            !(k.hasPrefix("ANTHROPIC_") || k.hasPrefix("CLAUDE_CODE_") || k == "CLAUDECODE" || k == "SSH_AUTH_SOCK"
              || k.hasPrefix("COPILOT_") || tokenVars.contains(k))
        }
        env["COPILOT_HOME"] = home.path
        env["COPILOT_AUTO_UPDATE"] = "false"
        env["COPILOT_GITHUB_TOKEN"] = token
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

    /// Največja vrstica JSONL; daljša se zavrže (L4).
    static let maxLine = 1 << 20

    /// Kopija mape le z navadnimi datotekami in mapami: povezav ne kopira in jim nikoli ne sledi (stanje seje iz peskovnika).
    static func copyTree(_ src: URL, to dst: URL) -> Bool {
        let fm = FileManager.default
        guard (try? fm.createDirectory(at: dst, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])) != nil,
              let e = fm.enumerator(atPath: src.path) else { return false }  // enumerator(atPath:) povezavam ne sledi
        for case let rel as String in e {
            var st = stat()
            guard lstat(src.path + "/" + rel, &st) == 0 else { continue }
            let to = dst.appendingPathComponent(rel)
            switch st.st_mode & S_IFMT {
            case S_IFDIR: try? fm.createDirectory(at: to, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
            case S_IFREG: _ = ShareHub.copyRegular(URL(fileURLWithPath: src.path + "/" + rel), to: to)
            default: continue
            }
        }
        return true
    }

    struct Failure: Error { let message: String }

    final class Handle: @unchecked Sendable { let p = Process() }

    /// Zažene copilot v peskovniku (`sandbox`: profil sandbox-exec) in vrne zadnje sporočilo agenta.
    static func run(_ exe: String, args: [String], prompt: String, cwd: URL, env: [String: String], sandbox: String,
                    onStep: @escaping @Sendable (String) -> Void) async throws -> String {
        let h = Handle()
        return try await withTaskCancellationHandler {
            try await Task.detached { () throws -> String in
                let p = h.p
                p.executableURL = URL(fileURLWithPath: "/usr/bin/sandbox-exec")
                p.arguments = ["-p", sandbox, exe] + args
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
                var buf = Data(), answer = "", skipping = false
                while case let chunk = events.fileHandleForReading.availableData, !chunk.isEmpty {
                    buf.append(chunk)
                    while let nl = buf.firstIndex(of: 10) {
                        if !skipping {
                            let e = event(buf[buf.startIndex..<nl])
                            if let t = e.text { answer = t }  // odgovor je zadnje sporočilo agenta
                            if let s = e.step { onStep(s) }
                        }
                        skipping = false
                        buf = Data(buf[(nl + 1)...])
                    }
                    if buf.count > maxLine { buf.removeAll(); skipping = true }  // predolga vrstica: do naslednje nove vrstice
                }
                if !skipping, let t = event(buf).text { answer = t }
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

    /// Copilot vedno v sandbox-exec (C2, M4): pisanje le v mapo seje (s pisanjem) in v svojo začasno mapo zahteve s svežim
    /// COPILOT_HOME (hooki, mcp-config, nastavitve ali zaupane mape iz prejšnjega zagona ne obstajajo); domača mapa
    /// skrivnosti in Scratch skrita, ~/Library/Caches (paket copilot) le za branje. Med zahtevami ostane le stanje seje
    /// (session-state/<id>, brez povezav) v Scratch, ki ga peskovnik ne vidi.
    func runCopilot(_ req: HTTPRequest, body: [String: Any], model: String?, rid: String, writer: ResponseWriter) async -> Int {
        let cfg = config.copilot
        let agent = (body["tools"] as? [Any])?.isEmpty == false
        if !cfg.write, agent, await Self.shares.isWorktree(session: req.header("x-claude-mini-session") ?? "") {
            return await error(400, "invalid_request_error",
                               "Copilot may not write (Mini Gateway → Settings → Copilot CLI), so worktree mode is not possible; use sync",
                               rid: rid, writer: writer)
        }
        if let model, !cfg.modelList.contains(model) {  // L1: le našteti modeli
            return await error(404, "not_found_error", "Unknown model: claude-copilot-\(model) (list it in Mini Gateway → Settings → Copilot CLI)",
                               rid: rid, writer: writer)
        }
        guard let exe = Copilot.executable(cfg.executable) else {
            return await error(503, "api_error", "Copilot CLI not found (Mini Gateway → Settings → Copilot CLI)", rid: rid, writer: writer)
        }
        let all = ClaudeCLI.turns(body)
        guard let last = all.last, last.role == "user" else {
            return await error(400, "invalid_request_error", "Last message must be from the user", rid: rid, writer: writer)
        }
        // Začasna mapa zahteve (0700, nepredvidljivo ime): COPILOT_HOME, TMPDIR in pogovor brez orodij.
        var template = Array("/private/tmp/minigw-copilot-XXXXXXXX".utf8CString)
        guard let made = mkdtemp(&template) else {
            return await error(500, "api_error", "Copilot: cannot create a temporary folder", rid: rid, writer: writer)
        }
        let reqDir = URL(fileURLWithPath: String(cString: made), isDirectory: true)
        defer { try? FileManager.default.removeItem(at: reqDir) }
        let home = reqDir.appendingPathComponent("home"), tmp = reqDir.appendingPathComponent("tmp")
        for d in [home.appendingPathComponent("session-state"), tmp] {
            try? FileManager.default.createDirectory(at: d, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        }
        var cwd = tmp
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
            tree = await Self.shares.worktree(session: req.header("x-claude-mini-session") ?? "")
            cwd = tree?.root ?? dir  // M3: koren klona namesto --add-dir; sporočilo spodaj pove mapo seje
            if let shared, let clientCwd {
                readOnly = await Self.shares.isReadOnly(session: req.header("x-claude-mini-session") ?? "")
                prompt = Self.mountNote(clientCwd, shared, readOnly: readOnly, synced: synced, branch: tree?.branch) + "\n\n" + prompt
            }
        }
        let write = cfg.write && !readOnly, commands = agent && cfg.commands && write  // peskovnik piše v mapo: ukazi le s pisanjem (tudi ne pri --ro)
        // Prijava (H1): ukazi le z namenskim žetonom (fine-grained PAT, le »Copilot Requests«): ukaz bi lahko prebral okolje
        // copilota. Brez ukazov ni lupine: tudi žeton iz okolja ali GitHub CLI.
        let pat = secrets.get(Copilot.tokenAccount).flatMap { $0.isEmpty ? nil : $0 }
        if commands, pat == nil {
            return await error(503, "api_error",
                               "Copilot commands need their own GitHub token (fine-grained, only the “Copilot Requests” permission): minigw gw copilot token, or Mini Gateway → Settings → Copilot CLI",
                               rid: rid, writer: writer)
        }
        let base = ProcessInfo.processInfo.environment
        var token = pat ?? Copilot.tokenVars.compactMap { base[$0] }.first { !$0.isEmpty }
        if token == nil { token = await Task.detached { Copilot.ghToken() }.value }
        guard let token else {
            return await error(503, "api_error", "Copilot: no GitHub sign-in on the gateway; sign in with the GitHub CLI (gh auth login) or set a token (minigw gw copilot token)",
                               rid: rid, writer: writer)
        }
        let root = tree?.root ?? cwd
        guard let sandbox = Native.sandboxProfile(dir: root.path, network: true,
                                                  hidden: [config.scratchURL.path, RouterConfig.defaultScratch.path], routerPort: Int(config.port),
                                                  runTmp: reqDir.path, writable: agent && write ? [root.path] : []) else {
            return await error(403, "permission_error", "Copilot: the session's folder is a link", rid: rid, writer: writer)
        }
        let extraEnv = tree.map { Worktree.harnessEnv(root: $0.root, maven: $0.maven) }
            ?? ["TMPDIR": tmp.path + "/", "CLANG_MODULE_CACHE_PATH": tmp.appendingPathComponent("ModuleCache").path]
        let env = Copilot.environment(base, home: home, token: token, extra: extraEnv)
        // Nadaljevanje seje Copilot (--session-id): ključ je dosedanji pogovor v tej mapi; stanje seje iz Scratch.
        let store = config.scratch("copilot").appendingPathComponent("sessions")
        let scope = Self.agentScope("copilot", req, cwd: cwd)
        let existing = agent ? await Self.copilotSessions.get(ClaudeCLI.key(all.dropLast(), scope: scope)) : nil
        let session = existing ?? UUID().uuidString.lowercased()
        if let existing, Copilot.copyTree(store.appendingPathComponent(existing), to: home.appendingPathComponent("session-state/" + existing)) {
            prompt = last.text
        }
        let args = Copilot.args(agent: agent, model: model, write: write, commands: commands,
                                network: cfg.network && (tree == nil || config.worktreeCodexNetwork), session: session)
        let sessions = Self.copilotSessions, client = req.header("x-claude-mini-session") ?? ""
        return await replyWhenDone(body: body, model: "claude-copilot", rid: rid, writer: writer) {
            let text = try await Copilot.run(exe, args: args, prompt: prompt, cwd: cwd, env: env, sandbox: sandbox) { [onActivity] s in
                onActivity(.step(id: rid, text: s))
            }
            if agent { await Self.shares.syncBack(session: client) }  // sync: spremembe agenta k odjemalcu pred odgovorom
            if agent {
                let fm = FileManager.default, next = store.appendingPathComponent(session + ".new-" + rid)
                if Copilot.copyTree(home.appendingPathComponent("session-state/" + session), to: next) {
                    try? fm.removeItem(at: store.appendingPathComponent(session))
                    if (try? fm.moveItem(at: next, to: store.appendingPathComponent(session))) != nil {
                        await sessions.set(ClaudeCLI.key(all + [ClaudeCLI.Turn(role: "assistant", text: text)], scope: scope), session)
                    }
                }
                try? fm.removeItem(at: next)
            }
            return (text, [])
        }
    }
}
