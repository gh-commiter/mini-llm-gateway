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
    public var askOthers = false          // »May ask other models«: agent vpraša drug model prehoda (AgentRun)
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
        askOthers = try c.decodeIfPresent(Bool.self, forKey: .askOthers) ?? d.askOthers
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

    /// Za nastavitve (glavna nit); router: `await executable("")`.
    public static var detected: String? { Detected.pathNow("copilot") }

    public static func executable(_ configured: String) async -> String? {
        configured.isEmpty ? await Detected.path("copilot") : [(configured as NSString).expandingTildeInPath].first { FileManager.default.isExecutableFile(atPath: $0) }
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
    /// Žeton za ukaze: fine-grained PAT (github_pat_…), natisljiv ASCII, brez presledkov.
    public static func validToken(_ t: String) -> Bool {
        t.hasPrefix("github_pat_") && t.count <= 255 && t.unicodeScalars.allSatisfy { (0x21...0x7e).contains($0.value) }
    }

    /// Paket copilot (~/Library/Caches/copilot/pkg/<platforma>/<različica>) razširi copilot sam ob prvem zagonu nove
    /// različice; v peskovniku (predpomnilnik le za branje) ne more (EPERM). Ob novi izvršljivi datoteki (pot, velikost, čas)
    /// ali brez mape pkg enkrat `copilot --version` brez peskovnika: uporabnikov program, brez navodila agenta in brez klica
    /// modela (R7); svež COPILOT_HOME, brez žetona, brez samodejne posodobitve.
    final class Prepared: @unchecked Sendable { let lock = NSLock(); var keys = Set<String>() }
    static let prepared = Prepared()

    /// #26: v peskovniku `sandbox` (brez omrežja, pisanje le v paket in začasno mapo): podtaknjena koda v paketu ne doseže
    /// nastavitev, ključev ali omrežja.
    static func prepare(_ exe: String, base: [String: String], home: URL, cwd: URL, sandbox: String) {
        let real = Sync.real(URL(fileURLWithPath: exe))?.path ?? exe
        let a = try? FileManager.default.attributesOfItem(atPath: real)
        let key = "\(real)|\(a?[.size] ?? 0)|\((a?[.modificationDate] as? Date)?.timeIntervalSince1970 ?? 0)"
        let pkg = (Sync.real(URL(fileURLWithPath: NSHomeDirectory())) ?? URL(fileURLWithPath: NSHomeDirectory())).path + "/Library/Caches/copilot/pkg"
        guard !(prepared.lock.withLock { prepared.keys.contains(key) } && FileManager.default.fileExists(atPath: pkg)) else { return }
        var env = environment(base, home: home, token: "")
        env["COPILOT_GITHUB_TOKEN"] = nil
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/sandbox-exec")
        p.arguments = ["-p", sandbox, exe, "--version"]
        p.currentDirectoryURL = cwd
        p.environment = env
        p.standardInput = FileHandle.nullDevice
        p.standardOutput = FileHandle.nullDevice
        p.standardError = FileHandle.nullDevice
        guard (try? p.run()) != nil else { return }
        let deadline = Date().addingTimeInterval(120)  // razširitev ~200 MB
        while p.isRunning && Date() < deadline { usleep(50_000) }
        if p.isRunning { p.terminate(); return }
        if p.terminationStatus == 0 { prepared.lock.withLock { _ = prepared.keys.insert(key) } }
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

    /// Datoteke stanja seje, ki jih copilot potrebuje za nadaljevanje (preverjeno s copilot 1.0.107); drugo (checkpoints,
    /// rewind-file-snapshots, files …) ne gre med zahtevami (R6).
    static let sessionFiles = ["events.jsonl", "workspace.yaml"]

    /// Le `sessionFiles`, le navadne datoteke, brez povezave kjerkoli v poti (O_NOFOLLOW_ANY). false, če ni nobene.
    static func copySession(_ src: URL, to dst: URL) -> Bool {
        guard (try? FileManager.default.createDirectory(at: dst, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])) != nil
        else { return false }
        return sessionFiles.filter { ShareHub.copyRegular(src.appendingPathComponent($0), to: dst.appendingPathComponent($0)) }.count > 0
    }

    struct Failure: Error { let message: String }

    /// Zažene copilot v peskovniku (`sandbox`: profil sandbox-exec) v lastni skupini procesov (R5) in vrne zadnje sporočilo
    /// agenta. Po koncu glavnega procesa (in 2 s) SIGKILL skupini in vsakemu procesu s pisalnim koncem cevi (kot minigw run);
    /// ob prekinitvi SIGTERM, po 5 s SIGKILL.
    static func run(_ exe: String, args: [String], prompt: String, cwd: URL, env: [String: String], sandbox: String,
                    started: @escaping @Sendable (pid_t) -> Void = { _ in }, onStep: @escaping @Sendable (String) -> Void) async throws -> String {
        guard let (pid, out, err, input) = Native.spawnGroup("/usr/bin/sandbox-exec", ["-p", sandbox, exe] + args, dir: cwd, env: env, input: true),
              let input else { throw Failure(message: "cannot start copilot") }
        started(pid)
        AgentCount.running.add(1, ordering: .relaxed)
        defer { AgentCount.running.subtract(1, ordering: .relaxed) }
        let exit = ExitWaiter()
        Thread {
            var st: Int32 = 0
            while waitpid(pid, &st, 0) == -1 && errno == EINTR {}
            exit.done(st & 0x7f == 0 ? (st >> 8) & 0xff : 128 + (st & 0x7f))
            DispatchQueue.global().asyncAfter(deadline: .now() + 2) {
                _ = Darwin.kill(-pid, SIGKILL)
                for h in [out, err] { for w in Native.pipeWriters(of: h.fileDescriptor) { _ = Darwin.kill(w, SIGKILL) } }
            }
        }.start()
        Thread {  // ločeno od branja dogodkov: velik pogovor ne zaklene cevi; try?: copilot je lahko že končal
            try? input.write(contentsOf: Data(prompt.utf8))
            try? input.close()
        }.start()
        return try await withTaskCancellationHandler {
            // branje dogodkov blokira vso sejo: na svoji niti, ne v bazenu Swift concurrency
            let (answer, stderr) = await offPool { () -> (String, Data) in
                final class Box: @unchecked Sendable { var data = Data() }
                let stderr = Box()
                let errReader = Thread { stderr.data = err.readDataToEndOfFile() }
                errReader.start()
                var buf = Data(), answer = "", skipping = false
                while case let chunk = out.availableData, !chunk.isEmpty {
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
                return (answer, stderr.data)
            }
            let code = await exit.wait()
            _ = Darwin.kill(-pid, SIGKILL)  // ostanki skupine
            let text = answer.trimmingCharacters(in: .whitespacesAndNewlines)
            guard code == 0, !text.isEmpty else {
                let tail = String(decoding: stderr.suffix(400), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
                throw Failure(message: "copilot exited with \(code)" + (tail.isEmpty ? "" : ": \(tail)"))
            }
            return text
        } onCancel: {
            _ = Darwin.kill(-pid, SIGTERM)
            DispatchQueue.global().asyncAfter(deadline: .now() + 5) { _ = Darwin.kill(-pid, SIGKILL) }
        }
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
            let why = await Self.shares.worktreeRefusal(session: req.header("x-claude-mini-session") ?? "",
                                                        worktree: "Copilot may not write (Mini Gateway → Settings → Copilot CLI), so worktree mode is not possible; use sync",
                                                        native: "Copilot may not write (Mini Gateway → Settings → Copilot CLI), so it cannot work")
            return await error(400, "invalid_request_error", why, rid: rid, writer: writer)
        }
        if let model, !cfg.modelList.contains(model) {  // L1: le našteti modeli
            return await error(404, "not_found_error", "Unknown model: claude-copilot-\(model) (list it in Mini Gateway → Settings → Copilot CLI)",
                               rid: rid, writer: writer)
        }
        guard let exe = await Copilot.executable(cfg.executable) else {
            return await error(503, "api_error", "Copilot CLI not found (Mini Gateway → Settings → Copilot CLI)", rid: rid, writer: writer)
        }
        let all = ClaudeCLI.turns(body)
        guard let last = all.last, last.role == "user" else {
            return await error(400, "invalid_request_error", "Last message must be from the user", rid: rid, writer: writer)
        }
        // R1: vedno namenski žeton (fine-grained PAT le z »Copilot Requests«): okolje copilota lahko drugi agenti v peskovniku
        // preberejo (KERN_PROCARGS2), zato nikoli žeton GitHub CLI ali okolja z vsemi pravicami uporabnika.
        guard let token = secrets.get(Copilot.tokenAccount), !token.isEmpty else {
            return await error(503, "api_error", "Set a Copilot token (fine-grained, Copilot Requests only): minigw gw copilot token, or Mini Gateway → Settings → Copilot CLI",
                               rid: rid, writer: writer)
        }
        // Začasna mapa zahteve (0700, nepredvidljivo ime) v Scratch, ki je skrit vsem profilom; ta profil jo izrecno dovoli
        // (R2): COPILOT_HOME (h), TMPDIR (t) in pogovor brez orodij. Kratka imena: poti vtičnic so omejene.
        var template = Array((Sync.real(config.scratch("copilot/req")) ?? config.scratch("copilot/req")).appendingPathComponent("XXXXXXXX").path.utf8CString)
        guard let made = mkdtemp(&template) else {
            return await error(500, "api_error", "Copilot: cannot create a temporary folder", rid: rid, writer: writer)
        }
        let reqDir = URL(fileURLWithPath: String(cString: made), isDirectory: true)
        defer { try? FileManager.default.removeItem(at: reqDir) }
        let home = reqDir.appendingPathComponent("h"), tmp = reqDir.appendingPathComponent("t")
        for d in [home.appendingPathComponent("session-state"), tmp] {
            try? FileManager.default.createDirectory(at: d, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        }
        var cwd = tmp, visible: URL?
        var readOnly = false
        var tree: (root: URL, branch: String, maven: URL, read: [URL])?
        var prompt = ClaudeCLI.prompt(history: all.dropLast(), last: last)
        var sharedDir: URL?
        if agent {
            let clientCwd = req.header("x-claude-mini-cwd")
            let shared = await Self.shares.mount(session: req.header("x-claude-mini-session") ?? "", cwd: clientCwd, base: localBase,
                                                 wait: req.header("x-claude-mini-share") == "1", device: req.device?.id,
                                                 roots: config.claudeCLI.allowedRoots, scratch: config.scratchURL, writes: cfg.write)
            let synced = await Self.shares.isSync(session: req.header("x-claude-mini-session") ?? "")
            guard let dir = shared ?? (synced ? nil : ClaudeCLI.resolveCwd(clientCwd, roots: config.claudeCLI.allowedRoots, scratch: config.scratchURL)) else {
                if let why = await Self.shares.takeRefusal() {  // zaklep pisanja
                    return await error(409, "invalid_request_error", why, rid: rid, writer: writer)
                }
                if synced, Task.isCancelled { return 499 }
                if synced { return await error(502, "api_error", "Syncing \(clientCwd ?? "the folder") to the gateway failed (see minigw output)", rid: rid, writer: writer) }
                return await error(400, "invalid_request_error",
                                   "cwd_unavailable: folder \(clientCwd ?? "(none)") does not exist on this Mac or is outside the allowed folders",
                                   rid: rid, writer: writer)
            }
            tree = await Self.shares.worktree(session: req.header("x-claude-mini-session") ?? "")
            sharedDir = shared
            cwd = tree?.root ?? dir  // M3: koren klona namesto --add-dir; sporočilo spodaj pove mapo seje
            if shared != nil || tree != nil { visible = cwd }  // R3: le pot iz ShareHub sme biti v (skritem) Scratch
            readOnly = await Self.isReadOnly(req)
            if let shared, let clientCwd {
                prompt = Self.mountNote(clientCwd, shared, readOnly: readOnly, synced: synced, branch: tree?.branch) + "\n\n" + prompt
            }
        }
        let write = cfg.write && !readOnly, commands = agent && cfg.commands && write  // peskovnik piše v mapo: ukazi le s pisanjem (tudi ne pri --ro)
        let base = ProcessInfo.processInfo.environment
        let root = tree?.root ?? cwd
        // R9: mapa seje ne sme vsebovati paketa copilot (orodje write bi ga spremenilo za naslednji zagon brez peskovnika).
        let homes = Set([NSHomeDirectory(), Sync.real(URL(fileURLWithPath: NSHomeDirectory()))?.path ?? NSHomeDirectory()])
        if agent, homes.map({ $0 + "/Library/Caches/copilot" }).contains(where: { $0 == root.path || $0.hasPrefix(root.path + "/") }) {
            return await error(403, "permission_error", "Copilot: the session's folder contains Copilot's package cache (~/Library/Caches/copilot)",
                               rid: rid, writer: writer)
        }
        guard let sandbox = Native.sandboxProfile(dir: root.path, network: true,
                                                  hidden: [config.scratchURL.path, RouterConfig.defaultScratch.path], routerPort: Int(config.port),
                                                  runTmp: reqDir.path, writable: agent && write ? [root.path] : [], visible: visible?.path) else {
            return await error(403, "permission_error", "Copilot: the session's folder is a link", rid: rid, writer: writer)
        }
        let extraEnv = tree.map { Worktree.harnessEnv(root: $0.root, maven: $0.maven) }
            ?? ["TMPDIR": tmp.path + "/", "CLANG_MODULE_CACHE_PATH": tmp.appendingPathComponent("ModuleCache").path]
        let run = agent ? await startAgentRun(req, kind: "copilot", dir: sharedDir, secretDir: reqDir) : nil  // peskovnik: zaprta le vrata prehoda; žeton v mapi zahteve
        defer { run?.stop() }
        let env = Copilot.environment(base, home: home, token: token, extra: extraEnv.merging(run?.env ?? [:]) { $1 })
        // Nadaljevanje seje Copilot (--session-id): ključ je dosedanji pogovor v tej mapi; stanje seje iz Scratch.
        let store = config.scratch("copilot").appendingPathComponent("sessions")
        let scope = Self.agentScope("copilot", req, cwd: cwd)
        let existing = agent ? await Self.copilotSessions.get(ClaudeCLI.key(all.dropLast(), scope: scope)) : nil
        let session = existing ?? UUID().uuidString.lowercased()
        if let existing, Copilot.copySession(store.appendingPathComponent(existing), to: home.appendingPathComponent("session-state/" + existing)) {
            prompt = last.text
        }
        if agent, let note = agentNote("copilot") { prompt = note + "\n\n" + prompt }
        let prepHome = reqDir.appendingPathComponent("p")  // svoj prazen COPILOT_HOME, ne domača mapa seje (R9)
        let cache = RouterConfig.copilotCache.path.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"")
        if let box = Native.sandboxProfile(dir: tmp.path, network: false, hidden: [config.scratchURL.path, RouterConfig.defaultScratch.path],
                                           routerPort: Int(config.port), runTmp: reqDir.path, writable: []) {
            // Paket sme razširiti (zadnje pravilo velja: tudi čez prepoved v buildCacheDenies), drugje nič.
            let prep = box + "(allow file-read* file-read-data file-write* (subpath \"\(cache)\"))\n"
            await offPool { Copilot.prepare(exe, base: base, home: prepHome, cwd: tmp, sandbox: prep) }
        }
        let args = Copilot.args(agent: agent, model: model, write: write, commands: commands,
                                network: cfg.network && (tree == nil || config.worktreeCodexNetwork), session: session)
        let sessions = Self.copilotSessions, client = req.header("x-claude-mini-session") ?? ""
        return await replyWhenDone(body: body, model: "claude-copilot", rid: rid, writer: writer) {
            let text = try await Copilot.run(exe, args: args, prompt: prompt, cwd: cwd, env: env, sandbox: sandbox, started: { run?.started($0) }) { [onActivity] s in
                onActivity(.step(id: rid, text: s))
            }
            run?.stop()  // I2: vprašanja drugim modelom se končajo pred prevzemom sprememb
            if agent { await Self.shares.syncBack(session: client) }  // sync: spremembe agenta k odjemalcu pred odgovorom
            if agent {
                let fm = FileManager.default, next = store.appendingPathComponent(session + ".new-" + rid)
                if Copilot.copySession(home.appendingPathComponent("session-state/" + session), to: next) {
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
