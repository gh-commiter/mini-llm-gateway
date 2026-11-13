import Foundation

/// Ponudnik `copilot -p` (GitHub Copilot CLI): agent v mapi seje, kot Codex. Copilot nima lastnega peskovnika za vgrajena
/// orodja (urejanje datotek preveri le pot), zato privzeto le branje; pisanje datotek le v mapi seje; ukazov lupine ni
/// (#81 H1); ves copilot vedno v sandbox-exec (pisanje le v mapo seje s pisanjem, domača mapa skrita, `runCopilot`).
/// #81 (0.5.25, lastnik D1): Copilot se prijavi le s `copilot login` uporabnika tega Maca; brez žetona, `gh` ali PAT.
/// Svež COPILOT_HOME vsake zahteve dobi nov config.json le s kazalcem na prijavo (`loggedInUsers`, `lastLoggedInUser` iz
/// ~/.copilot/config.json, preverjena oblika); žeton ostane v Keychainu (storitev com.github.copilot.cli, račun iz gostitelja
/// in prijave, ne iz poti) in ga copilot prebere sam: profil peskovnika mu dovoli branje ~/Library/Keychains (`keychainRule`;
/// postavke ščiti ACL Keychaina, le podpisani copilot jih bere brez vprašanja). Brez prijave ali z žetonom v navadnem
/// besedilu Copilot ni na voljo, z razlogom (`login`). H1: ukazov lupine ni (ukaz bi prebral login.keychain-db ali zagnal
/// podpisani copilot s podtaknjenim paketom in prebral žeton), zato Keychain bere le drevo procesov copilota samega.
public struct CopilotConfig: Codable, Sendable, Equatable {
    public var enabled = false
    public var executable = ""     // prazno = samodejno iskanje
    public var models = ""         // ločeno z vejico; claude-copilot-<model> za vsakega (Copilot nima seznama modelov)
    public var write = false       // urejanje datotek v mapi seje (sicer le branje)
    public var commands = false    // ukazi lupine: #81 H1 nikoli (shranjen true ne velja, Copilot.noCommands)
    public var network = false     // spletna orodja (url); omrežja ukazov lupine ne omeji (copilot ga potrebuje sam)
    public var askOthers = false          // »May ask other models«: agent vpraša drug model prehoda (AgentRun)
    public var work = ""                  // #45: najvišji način za pisanje (sync, worktree); "" = direct, vsi načini
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
        work = try c.decodeIfPresent(String.self, forKey: .work) ?? d.work
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
    static func args(agent: Bool, model: String?, write: Bool, network: Bool, session: String) -> [String] {
        var a = ["--output-format", "json", "--no-ask-user", "--disable-builtin-mcps", "--disallow-temp-dir", "--no-auto-update", "--no-color",
                 "--disable-client-telemetry", "--secret-env-vars=" + tokenVars.joined(separator: ","), "--session-id", session]
        if let model { a += ["--model", model] }
        var allow: [String] = [], deny = ["memory"]
        if agent { allow.append("read") } else { deny.append("read") }
        if agent && write { allow.append("write") } else { deny.append("write") }
        deny += ["shell", "shell(gh:*)", "shell(security:*)"]  // #81 H1: brez ukazov (gh, security še izrecno: obramba v globino)
        if agent && network { allow.append("url") } else { deny.append("url") }
        return a + allow.flatMap { ["--allow-tool", $0] } + deny.flatMap { ["--deny-tool", $0] }
    }

    /// Žetoni, ki jih copilot sprejme iz okolja (preglasijo prijavo); okolje jih nima (`environment`), ukazom lupine jih
    /// copilot skrije (--secret-env-vars).
    static let tokenVars = ["COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"]

    static let signIn = "sign in with `copilot login` on the gateway as the gateway user"
    /// H1: zakaj Copilot nima ukazov (enako besedilo kot copilotNoCommands v Go).
    /// #81 N3: git copilota brez pagerja, urejevalnika, vprašanj in omrežnih protokolov (vsi načini; kot copilotGitPins v Go).
    static let gitPins = ["GIT_PAGER": "cat", "GIT_EDITOR": "true", "GIT_ALLOW_PROTOCOL": "file", "GIT_TERMINAL_PROMPT": "0"]
    public static let noCommands = "Copilot commands are not available: a command could read Copilot's sign-in from the system keyring"
    /// Vrsta razloga (nastavitve ga pokažejo prevedenega; API in zahteva angleško besedilo).
    public enum LoginProblem: Sendable { case notSignedIn, unreadable, plainText, badForm }
    /// Prava nastavitev copilot uporabnika (le kazalec na prijavo se prebere).
    public static var userConfig: URL { URL(fileURLWithPath: NSHomeDirectory() + "/.copilot/config.json") }

    nonisolated(unsafe) static let hostRe = try! Regex(#"(https://)?[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*"#)
    nonisolated(unsafe) static let loginRe = try! Regex(#"[A-Za-z0-9][A-Za-z0-9_-]{0,99}"#)  // GitHub; _ za Enterprise Managed Users
    nonisolated(unsafe) static let kindRe = try! Regex(#"[A-Za-z][A-Za-z0-9-]{0,31}"#)  // kind, derivedFrom (L4)

    /// En LoggedInUser: host, login obvezna; kind, derivedFrom neobvezna (^[A-Za-z][A-Za-z0-9-]{0,31}$); drugi ključi = nil.
    static func user(_ v: Any?) -> [String: String]? {
        guard let o = v as? [String: Any] else { return nil }
        var u: [String: String] = [:]
        for (k, v) in o {
            guard let s = v as? String else { return nil }
            switch k {
            case "host": guard s.utf8.count <= 253, (try? hostRe.wholeMatch(in: s)) != nil else { return nil }
            case "login": guard (try? loginRe.wholeMatch(in: s)) != nil else { return nil }
            case "kind", "derivedFrom": guard (try? kindRe.wholeMatch(in: s)) != nil else { return nil }
            default: return nil
            }
            u[k] = s
        }
        return u["host"] != nil && u["login"] != nil ? u : nil
    }

    /// #81: nov config.json za svež COPILOT_HOME (le loggedInUsers in lastLoggedInUser iz `url`) ali razlog, zakaj prijave
    /// ni mogoče uporabiti (kot copilotLoginPointer v Go). Vrednosti žetona ne kopira in ne izpiše.
    public static func login(_ url: URL) -> (config: Data?, problem: String?, kind: LoginProblem?) {
        let r = loginPointer(url)
        guard let why = r.problem else { return (r.config, nil, nil) }
        let p = url.path
        let text = switch why {
        case .notSignedIn: "Copilot is not signed in on the gateway: " + signIn
        case .unreadable: "Copilot's sign-in (\(p)) cannot be read: " + signIn
        case .badForm: "Copilot's sign-in (\(p)) is not in the expected form: " + signIn
        case .plainText: "Copilot's sign-in on the gateway is stored as plain text (no system keyring, e.g. a headless Linux), "
            + "which Mini Gateway does not use: Copilot needs the sign-in in the system keyring (Secret Service, Keychain)"
        }
        return (nil, text, why)
    }

    static func loginPointer(_ url: URL) -> (config: Data?, problem: LoginProblem?) {
        let p = url.path, bad = LoginProblem.badForm, notIn = LoginProblem.notSignedIn
        guard (try? FileManager.default.attributesOfItem(atPath: p))?[.type] as? FileAttributeType == .typeRegular else { return (nil, notIn) }
        let fd = open(p, O_RDONLY | O_NOFOLLOW | O_NONBLOCK)  // brez sledenja povezavi; FIFO ne blokira (L2)
        guard fd >= 0 else { return (nil, .unreadable) }
        let fh = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
        var st = stat()
        guard fstat(fd, &st) == 0, st.st_mode & S_IFMT == S_IFREG else { return (nil, .unreadable) }  // ista datoteka je navadna
        let data = (try? fh.read(upToCount: 1 << 20)) ?? Data()
        // copilot začne datoteko s celimi vrsticami »// …« (niz JSON ne more čez vrstico)
        let text = String(decoding: data, as: UTF8.self).split(separator: "\n", omittingEmptySubsequences: false)
            .filter { !$0.trimmingCharacters(in: .whitespaces).hasPrefix("//") }.joined(separator: "\n")
        guard let top = try? JSONSerialization.jsonObject(with: Data(text.utf8)) as? [String: Any] else { return (nil, bad) }
        for (k, v) in top where k.lowercased().contains("token") {  // žeton v navadnem besedilu (ni shrambe ključev): ne kopira se
            if let n = v as? NSNumber, CFGetTypeID(n) == CFBooleanGetTypeID() { continue }
            return (nil, .plainText)
        }
        guard let list = top["loggedInUsers"] as? [Any], !list.isEmpty else { return (nil, notIn) }
        guard list.count <= 32 else { return (nil, bad) }
        let users = list.map(user)
        guard !users.contains(where: { $0 == nil }) else { return (nil, bad) }
        var out: [String: Any] = ["loggedInUsers": users.compactMap { $0 }]
        if let last = top["lastLoggedInUser"], !(last is NSNull) {
            guard let u = user(last) else { return (nil, bad) }
            out["lastLoggedInUser"] = u
        }
        return (try? JSONSerialization.data(withJSONObject: out, options: [.sortedKeys]), nil)
    }

    /// #81: copilot v peskovniku prebere svojo prijavo iz Keychaina (prijavni keychain v ~/Library/Keychains, ki ga profil
    /// sicer skrije; storitve Mach so dovoljene). Le branje: postavke ščiti ACL Keychaina (podpis copilota), pisati ne sme.
    /// Zadnje pravilo profila velja, zato na konec.
    static func keychainRule(home: String = NSHomeDirectory()) -> String {
        let p = home + "/Library/Keychains"
        return "(allow file-read* file-read-data" + Set([p, Sync.real(URL(fileURLWithPath: p))?.path ?? p]).sorted().map {
            " (subpath \"" + $0.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"") + "\")"
        }.joined() + ")\n"
    }

    /// Paket copilot (~/Library/Caches/copilot/pkg/<platforma>/<različica>) razširi copilot sam ob prvem zagonu nove
    /// različice; v peskovniku (predpomnilnik le za branje) ne more (EPERM). Ob novi izvršljivi datoteki (pot, velikost, čas)
    /// ali brez mape pkg enkrat `copilot --version` brez peskovnika: uporabnikov program, brez navodila agenta in brez klica
    /// modela (R7); svež COPILOT_HOME, brez prijave, brez samodejne posodobitve.
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
        let env = environment(base, home: home)
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
    /// ali žetonov iz okolja (#81: prijava le copilot login); svoj COPILOT_HOME.
    static func environment(_ base: [String: String], home: URL, extra: [String: String] = [:]) -> [String: String] {
        var env = base.filter { k, _ in
            !(k.hasPrefix("ANTHROPIC_") || k.hasPrefix("CLAUDE_CODE_") || k == "CLAUDECODE" || k == "SSH_AUTH_SOCK"
              || k.hasPrefix("COPILOT_") || k.hasPrefix("GITHUB_COPILOT_") || tokenVars.contains(k))  // M1: …_PROMPT_MODE_REPO_HOOKS
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
    static func run(_ exe: String, args: [String], prompt: String, cwd: URL, env: [String: String], sandbox: String, dirs: [String] = [],
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
            DispatchQueue.global().asyncAfter(deadline: .now() + 2) { Native.killTree(pid, pipes: [out, err]) }
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
            // #73: ostanki skupine; finalize (syncBack) le, ko drevesa ni več
            guard await offPool({ Native.killTree(pid, pipes: [out, err], dirs: dirs) }) else { throw Native.TreeAlive(agent: "copilot") }
            let text = answer.trimmingCharacters(in: .whitespacesAndNewlines)
            guard code == 0, !text.isEmpty else {
                let tail = String(decoding: stderr.suffix(400), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
                throw Failure(message: "copilot exited with \(code)" + (tail.isEmpty ? "" : ": \(tail)"))
            }
            return text
        } onCancel: { Native.terminateGroup(pid) }
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
        if !cfg.write, agent, await Self.shares.isWorktree(session: req.header("x-claude-mini-session") ?? ""),
           await Self.shares.readOnlyInTree(session: req.header("x-claude-mini-session") ?? "", agent: "Copilot") {  // worktree: le branje
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
        // #81 (D1): le copilot login uporabnika (kazalec v svež COPILOT_HOME, žeton v Keychainu), nikoli žeton v okolju
        let login = Copilot.login(copilotConfig)
        guard let loginConfig = login.config else {
            return await error(503, "api_error", login.problem ?? Copilot.signIn, rid: rid, extra: [("x-should-retry", "false")], writer: writer)
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
        guard FileManager.default.createFile(atPath: home.appendingPathComponent("config.json").path, contents: loginConfig,
                                             attributes: [.posixPermissions: 0o600]) else {
            return await error(500, "api_error", "Copilot: cannot prepare its home folder", rid: rid, writer: writer)
        }
        var cwd = tmp, visible: URL?
        var readOnly = false
        var tree: (root: URL, branch: String, maven: URL, read: [URL], gits: [String])?
        var prompt = ClaudeCLI.prompt(history: all.dropLast(), last: last)
        var sharedDir: URL?
        if agent {
            let clientCwd = req.header("x-claude-mini-cwd")
            let shared = await Self.shares.mount(session: req.header("x-claude-mini-session") ?? "", cwd: clientCwd, base: localBase,
                                                 wait: req.header("x-claude-mini-share") == "1", device: req.device?.id,
                                                 roots: checkedRoots, scratch: config.scratchURL, writes: cfg.write)
            let synced = await Self.shares.isSync(session: req.header("x-claude-mini-session") ?? "")
            guard let dir = shared ?? (synced ? nil : ClaudeCLI.resolveCwd(clientCwd, roots: checkedRoots, scratch: config.scratchURL)) else {
                if let why = await Self.shares.takeRefusal() {  // zaklep pisanja
                    return await error(why.status, "invalid_request_error", why.message, rid: rid, writer: writer)
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
            let cap = await capped(req, kind: "copilot")  // #45: način seje nad omejitvijo → le branje
            readOnly = await Self.isReadOnly(req) || cap != nil
            Self.runBranch?.readOnly = readOnly || (tree != nil && !cfg.write)  // #46a
            if readOnly, tree != nil, AgentRun.current == nil { return await error(400, "invalid_request_error", Self.readOnlyWorktree, rid: rid, writer: writer) }
            if let shared, let clientCwd {
                prompt = Self.mountNote(clientCwd, shared, readOnly: readOnly || (tree != nil && !cfg.write), synced: synced,
                                        branch: tree?.branch, gits: tree?.gits ?? []) + "\n\n" + prompt
            }
        }
        let write = cfg.write && !readOnly  // #81 H1: brez ukazov (ukaz bi prebral prijavo iz Keychaina); shranjen true ne velja
        if agent && write && cfg.commands, let s = req.header("x-claude-mini-session") { await Self.shares.note(session: s, Copilot.noCommands) }
        let base = ProcessInfo.processInfo.environment
        let root = tree?.root ?? cwd
        // R9: mapa seje ne sme vsebovati paketa copilot (orodje write bi ga spremenilo za naslednji zagon brez peskovnika).
        let homes = Set([NSHomeDirectory(), Sync.real(URL(fileURLWithPath: NSHomeDirectory()))?.path ?? NSHomeDirectory()])
        if agent, homes.map({ $0 + "/Library/Caches/copilot" }).contains(where: { $0 == root.path || $0.hasPrefix(root.path + "/") }) {
            return await error(403, "permission_error", "Copilot: the session's folder contains Copilot's package cache (~/Library/Caches/copilot)",
                               rid: rid, writer: writer)
        }
        guard let profile = Native.sandboxProfile(dir: root.path, network: true,
                                                  hidden: RouterConfig.hiddenScratch(config.scratchURL), routerPort: Int(config.port),
                                                  runTmp: reqDir.path, writable: agent && write ? [root.path] : [], visible: visible?.path) else {
            return await error(403, "permission_error", "Copilot: the session's folder is a link", rid: rid, writer: writer)
        }
        // L-N9a: ovoj git (Scratch, sicer skrit) berljiv v peskovniku, kot ro pri Go agentWrap
        let gitexec = config.scratch("gitexec").path
        func subpaths(_ ps: [String]) -> String {
            Set(ps.flatMap { [$0, Sync.real(URL(fileURLWithPath: $0))?.path ?? $0] }).sorted().map {
                " (subpath \"" + $0.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"") + "\")"
            }.joined()
        }
        // #67: s pisanjem $MINIGW_DEPS (tudi .git repozitorijev, ki jih agent naredi tam), Maven in predpomnilniki pod Scratch.
        // H1: preverjene (grantDirs) in le z razrešeno nadrejeno mapo (Scratch je prehodov), zadnji del nikoli razrešen;
        // vnosov samih agent ne more odstraniti ali zamenjati (unlink, create na dobesedni poti).
        var deps: [String]?
        if let t = tree, agent && write {
            guard let g = Worktree.grantDirs(root: t.root) else { return await error(503, "api_error", Worktree.unsafeFolders, rid: rid, writer: writer) }
            // #73: mapa klonov razrešena enkrat (ne starš $MINIGW_DEPS/tmp, ki ga agent piše), nato dobesedni deli
            let clones = t.root.deletingLastPathComponent().path, real = Sync.real(URL(fileURLWithPath: clones))?.path ?? clones
            deps = g.map { real + $0.dropFirst(clones.count) }
        }
        func q(_ p: String) -> String { "\"" + p.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"") + "\"" }
        let sandbox = profile + (tree == nil ? "" : "(allow file-read* file-read-data" + subpaths([gitexec]) + ")\n")
            + (deps.map { d in "(allow file-read* file-read-data file-write*" + d.map { " (subpath \(q($0)))" }.joined() + ")\n"
                + "(deny file-write-unlink file-write-create" + d.map { " (literal \(q($0)))" }.joined() + ")\n" } ?? "")
            + Copilot.keychainRule()
        var treeEnv: [String: String]?  // L-N9a: harness Copilot sam poganja git v klonu (ovoj kot pri Claude CLI)
        if let t = tree {
            guard let e = Worktree.claudeEnv(root: t.root, maven: t.maven, gitexec: config.scratch("gitexec"), gits: t.gits, agent: "copilot") else {
                return await error(503, "api_error", "Cannot prepare git for the agent's clone (git --exec-path): use sync", rid: rid, writer: writer)
            }
            treeEnv = e
            if deps == nil { treeEnv?["MINIGW_DEPS"] = nil } else { treeEnv?["MINIGW_SANDBOXED"] = "1" }  // #67: ves Copilot je v sandbox-exec
            // #67: omrežje za git clone po istem pravilu kot orodje url (Copilot.args spodaj)
            if deps != nil { treeEnv?["MINIGW_NETWORK_OFF"] = Worktree.networkOff("copilot", config) }
        }
        // #81 N1, N3: zunaj worktree git copilota nima repozitorija (GIT_DIR=/dev/null: .git/info/attributes, filtri, diff
        // iz .git/config odjemalca ne tečejo), kot harness Codex; povsod še poceni pripone (pager, urejevalnik, protokoli).
        let extraEnv = (treeEnv
            ?? Worktree.gitGuardEnv(agent: "copilot").merging(["TMPDIR": tmp.path + "/", "CLANG_MODULE_CACHE_PATH": tmp.appendingPathComponent("ModuleCache").path,
                                                               "GIT_DIR": "/dev/null", "GIT_CEILING_DIRECTORIES": cwd.deletingLastPathComponent().path]) { $1 })
            .merging(Copilot.gitPins) { $1 }
        let run = agent ? await startAgentRun(req, kind: "copilot", dir: sharedDir, secretDir: reqDir, readOnly: readOnly || (tree != nil && !cfg.write)) : nil  // peskovnik: zaprta le vrata prehoda; žeton izvajanja v mapi zahteve
        defer { run?.stop() }
        let env = Copilot.environment(base, home: home, extra: extraEnv.merging(run?.env ?? [:]) { $1 })
        // Nadaljevanje seje Copilot (--session-id): ključ je dosedanji pogovor v tej mapi; stanje seje iz Scratch.
        let store = config.scratch("copilot").appendingPathComponent("sessions")
        let scope = Self.agentScope("copilot", req, cwd: cwd)
        let existing = agent ? await Self.copilotSessions.get(ClaudeCLI.key(all.dropLast(), scope: scope)) : nil
        let session = existing ?? UUID().uuidString.lowercased()
        if let existing, Copilot.copySession(store.appendingPathComponent(existing), to: home.appendingPathComponent("session-state/" + existing)) {
            prompt = last.text
        }
        if agent { prompt = agentNote("copilot") + "\n\n" + prompt }
        let prepHome = reqDir.appendingPathComponent("p")  // svoj prazen COPILOT_HOME, ne domača mapa seje (R9)
        let cache = RouterConfig.copilotCache.path.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"")
        if let box = Native.sandboxProfile(dir: tmp.path, network: false, hidden: RouterConfig.hiddenScratch(config.scratchURL),
                                           routerPort: Int(config.port), runTmp: reqDir.path, writable: []) {
            // Paket sme razširiti (zadnje pravilo velja: tudi čez prepoved v buildCacheDenies), drugje nič.
            let prep = box + "(allow file-read* file-read-data file-write* (subpath \"\(cache)\"))\n"
            await offPool { Copilot.prepare(exe, base: base, home: prepHome, cwd: tmp, sandbox: prep) }
        }
        let args = Copilot.args(agent: agent, model: model, write: write,
                                network: cfg.network && (tree == nil || config.worktreeCodexNetwork), session: session)
        let sessions = Self.copilotSessions, client = req.header("x-claude-mini-session") ?? ""
        return await replyWhenDone(body: body, model: "claude-copilot", rid: rid, writer: writer, req: req) {
            let text = try await Copilot.run(exe, args: args, prompt: prompt, cwd: cwd, env: env, sandbox: sandbox, dirs: await Self.shares.runDirs(), started: { run?.started($0) }) { [onActivity] s in
                onActivity(.step(id: rid, text: s))
            }
            run?.stop()  // I2: vprašanja drugim modelom se končajo pred prevzemom sprememb
            // sync: spremembe agenta k odjemalcu pred odgovorom; #82: neprevzete niso uspeh
            if agent, let why = await Self.shares.syncBack(session: client) { throw ShareHub.PullFailed(message: why) }
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
