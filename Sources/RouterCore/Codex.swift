import Foundation
import Synchronization

/// Ponudnik `codex exec` (OpenAI Codex CLI): agent v mapi seje, kot `claude -p`.
public struct CodexConfig: Codable, Sendable, Equatable {
    public var enabled = false
    public var executable = ""            // prazno = samodejno iskanje
    public var enabledModels: [String]?   // nil = vsi modeli računa (tudi prihodnji); seznam = le ti
    public var write = true               // sandbox workspace-write (sicer read-only)
    public var network = false            // omrežje za ukaze v sandboxu (npr. gradnja); privzeto izklopljeno kot v Go
    public var askOthers = false          // »May ask other models«: agent vpraša drug model prehoda (AgentRun)
    public var work = ""                  // #45: najvišji način za pisanje (sync, worktree); "" = direct, vsi načini
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
        work = try c.decodeIfPresent(String.self, forKey: .work) ?? d.work
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
        // #59: v znani mapi in brez projektnih nastavitev (.codex/config.toml) v njej ali nad njo
        let dir = FileManager.default.temporaryDirectory
        p.currentDirectoryURL = dir
        p.arguments = untrustedProjects(dir) + ["debug", "models"]
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
                     write: Bool = true, network: Bool = false, resume: String? = nil, gits: [String] = [], path: String = "", deny: [String] = []) -> [String] {
        var a = ["exec", "--json", "--skip-git-repo-check", "--color", "never", "-C", cwd.path, "-o", output.path]
        if let model { a += ["-m", model] }
        if agent && write && !gits.isEmpty {  // #40: klon, profil dovoljenj namesto workspace-write
            a += treeProfile(writable: [cwd.path] + extraDirs, gits: gits, deny: deny, network: network, path: path)
        } else if agent && write {
            // $TMPDIR (/var/folders/…/T) ni pisljiv: tam je predpomnilnik xcrun (shim /usr/bin/git ga izvede); TMPDIR je zasebna mapa.
            a += ["--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=\(network)",
                  "-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true"]
            for d in extraDirs { a += ["--add-dir", d] }
        } else {
            a += ["--sandbox", "read-only", "-c", #"sandbox_mode="read-only""#]  // tudi kot -c: velja tudi za »exec resume« (kot v Go, I5)
        }
        if !agent { a.append("--ephemeral") }  // naslovi, povzetki: brez zapisa seje
        a += untrustedProjects(cwd)
        if let resume { a += ["resume", resume] }
        return a + ["-"]  // navodila prek stdin (brez omejitve dolžine argumentov)
    }

    /// #59: projektne nastavitve (.codex/config.toml v mapi seje ali nad njo: notify, mcp_servers, hooki) Codex naloži le
    /// za zaupanja vredne mape v ~/.codex/config.toml in bi tekle zunaj peskovnika. Vsaka nadmapa cwd (tudi pot brez
    /// simbolnih povezav, ključ Codex) je za ta zagon »untrusted«: -c se združi nad uporabnikovo tabelo projects in
    /// natančen ključ mape prevlada (codex 0.167: najprej ključ mape, nato korena projekta in repozitorija).
    static func untrustedProjects(_ cwd: URL) -> [String] {
        var keys = Set<String>(), starts = [cwd.path]
        if let r = realpath(cwd.path, nil) { starts.append(String(cString: r)); free(r) }  // ne resolvingSymlinksInPath: ta odreže /private
        for start in starts {
            var d = start
            while true {
                keys.insert(d)
                let up = (d as NSString).deletingLastPathComponent
                if up == d || up.isEmpty { break }
                d = up
            }
        }
        func q(_ s: String) -> String { String(decoding: try! JSONSerialization.data(withJSONObject: s, options: [.fragmentsAllowed, .withoutEscapingSlashes]), as: UTF8.self) }
        return ["-c", "projects={" + keys.sorted().map { q($0) + #"={trust_level="untrusted"}"# }.joined(separator: ",") + "}"]
    }

    /// Pisljive mape Codex v klonu (worktree): koren, Maven in (#40, M1) vsak ugnezdeni klon, da sandbox Codex zaščiti
    /// tudi njegov .git (le za branje v vsaki pisljivi mapi): git harnessa zunaj sandboxa zdaj sledi mapi v ugnezden klon.
    /// #67: še $MINIGW_DEPS in skupni predpomnilniki projekta.
    static func treeDirs(root: URL, cwd: URL, maven: URL) -> [String] {
        (root.path == cwd.path ? [] : [root.path]) + [maven.path, Worktree.deps(root).path, Worktree.cache(root).path]
    }

    /// #40 (kot Go codexTreeProfile): v klonu profil dovoljenj namesto workspace-write, ki .git vsake pisljive mape zaklene
    /// v celoti (commit ni mogoč): .git vsakega klona pisljiv razen nastavitev, hookov, info, commondir in preusmeritev
    /// (git harnessa jih bere zunaj peskovnika). Ukazi agenta: PATH z ovojem, brez GIT_DIR, GIT_WORK_TREE, GIT_COMMON_DIR
    /// vrhnjega klona (harness Codex jih obdrži, Worktree.claudeEnv).
    /// L1: iskanje repozitorija ne nad klonom; `deny`: mape brez dostopa (L3: podatki aplikacije, kot Go mapa nastavitev).
    static func treeProfile(writable: [String], gits: [String], deny: [String] = [], network: Bool, path: String) -> [String] {
        var fs = ["/": "read"]  // #67: nič v /tmp (vse pod Scratch; TMPDIR ukazov je .build/tmp klona)
        for d in deny { fs[d] = "none" }
        for d in writable { fs[d] = "write" }
        for g in gits {
            fs[g] = "write"
            for d in ClaudeCLIConfig.gitDenies { fs[g + "/" + d] = "read" }
        }
        func q(_ s: String) -> String { String(decoding: try! JSONSerialization.data(withJSONObject: s, options: [.fragmentsAllowed, .withoutEscapingSlashes]), as: UTF8.self) }
        let table = fs.keys.sorted().map { q($0) + "=\"" + fs[$0]! + "\"" }.joined(separator: ",")
        return ["-c", #"default_permissions="minigw""#, "-c", "permissions.minigw.filesystem={" + table + "}",
                "-c", "permissions.minigw.network.enabled=\(network)",
                "-c", #"shell_environment_policy.exclude=["GIT_DIR","GIT_WORK_TREE","GIT_COMMON_DIR"]"#, "-c", "shell_environment_policy.set.PATH=" + q(path),
                "-c", "shell_environment_policy.set.GIT_CEILING_DIRECTORIES=" + q(URL(fileURLWithPath: gits[0]).deletingLastPathComponent().deletingLastPathComponent().path),
                "-c", #"shell_environment_policy.set.MINIGW_SANDBOXED="1""#]  // #67: le ukazi (v peskovniku), ne harness
    }

    struct Failure: Error { let message: String }

    final class Handle: @unchecked Sendable { let p = Process() }

    /// Zažene codex in vrne zadnje sporočilo agenta; prekinitev odjemalca ustavi codex.
    /// Korak iz dogodka `codex exec --json` (ukaz ali orodje; nikoli besedilo agenta), za Activity.
    static func step(_ line: Data) -> String? {
        guard let obj = try? JSONSerialization.jsonObject(with: line) as? [String: Any],
              let item = obj["item"] as? [String: Any] else { return nil }
        switch (obj["type"] as? String, item["type"] as? String) {
        case ("item.started", "command_execution"?):
            var cmd = item["command"] as? String ?? ""
            if let r = cmd.range(of: "-lc '"), cmd.hasSuffix("'") { cmd = String(cmd[r.upperBound...].dropLast()) }  // /bin/zsh -lc '…'
            return "$ " + cmd.prefix(120)
        case ("item.started", let t?) where t != "reasoning":
            return t.replacingOccurrences(of: "_", with: " ")
        default:
            return nil
        }
    }

    /// Sporočilo iz `{"type":"error","message":…}` ali `{"type":"turn.failed","error":{"message":…}}`.
    static func failure(_ line: Data) -> String? {
        guard let o = try? JSONSerialization.jsonObject(with: line) as? [String: Any] else { return nil }
        switch o["type"] as? String {
        case "error"?: return o["message"] as? String
        case "turn.failed"?: return (o["error"] as? [String: Any])?["message"] as? String
        default: return nil
        }
    }

    /// ID niti iz `{"type":"thread.started","thread_id":…}`.
    static func threadId(_ line: Data) -> String? {
        guard let o = try? JSONSerialization.jsonObject(with: line) as? [String: Any], o["type"] as? String == "thread.started" else { return nil }
        return o["thread_id"] as? String
    }

    static func run(_ exe: String, args: [String], cwd: URL, prompt: String, output: URL, env extra: [String: String] = [:], dirs: [String] = [],
                    stopNested: @escaping @Sendable () -> Void = {},
                    started: @escaping @Sendable (pid_t) -> Void = { _ in },
                    onStep: @escaping @Sendable (String) -> Void = { _ in }) async throws -> (String, String?) {
        let h = Handle()
        return try await withTaskCancellationHandler {
            try await offPool { () throws -> (String, String?) in
                let p = h.p
                p.executableURL = URL(fileURLWithPath: exe)
                p.arguments = args
                p.currentDirectoryURL = cwd
                p.environment = ProcessInfo.processInfo.environment  // brez agenta ključev SSH; #67: harness ni v peskovniku
                    .filter { $0.key != "SSH_AUTH_SOCK" && !Worktree.sandboxMarkers.contains($0.key) }.merging(extra) { $1 }
                let input = Pipe(), errors = Pipe()
                let events = Pipe()
                p.standardInput = input
                p.standardOutput = events
                p.standardError = errors
                var stderr = Data()
                let errReader = Thread { stderr = errors.fileHandleForReading.readDataToEndOfFile() }  // ne blokira pipe
                // #73: 2 s po koncu codex drevo (tudi vnuk s cevjo) ven, da se branje spodaj konča
                let pipes = [events.fileHandleForReading, errors.fileHandleForReading]
                p.terminationHandler = { p in
                    let g = p.processIdentifier
                    DispatchQueue.global().asyncAfter(deadline: .now() + 2) { Native.killTree(g, pipes: pipes) }
                }
                try p.run()
                started(p.processIdentifier)
                AgentCount.running.add(1, ordering: .relaxed)
                defer { AgentCount.running.subtract(1, ordering: .relaxed) }
                errReader.start()
                input.fileHandleForWriting.write(Data(prompt.utf8))
                try? input.fileHandleForWriting.close()
                var buf = Data(), thread: String?, failed: String?
                while case let chunk = events.fileHandleForReading.availableData, !chunk.isEmpty {
                    buf.append(chunk)
                    while let nl = buf.firstIndex(of: 10) {
                        let line = buf[buf.startIndex..<nl]
                        if thread == nil { thread = threadId(line) }
                        if let f = Self.failure(line) { failed = f }
                        if let s = step(line) { onStep(s) }
                        buf = Data(buf[(nl + 1)...])
                    }
                }
                while errReader.isExecuting { usleep(10_000) }
                p.waitUntilExit()
                // #73: finalize (syncBack) le, ko drevesa ni več
                stopNested()  // vprašani agenti se končajo urejeno, ne s SIGKILL (kot ClaudeCLI)
                guard Native.killTree(p.processIdentifier, pipes: pipes, dirs: dirs) else { throw Native.TreeAlive(agent: "codex") }
                let text = (try? String(contentsOf: output, encoding: .utf8))?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                guard p.terminationStatus == 0, !text.isEmpty else {
                    let tail = String(decoding: stderr.suffix(400), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
                    if let failed { throw Failure(message: failed) }
                    throw Failure(message: "codex exited with \(p.terminationStatus)" + (tail.isEmpty ? "" : ": \(tail)"))
                }
                return (text, thread)
            }
        } onCancel: { if h.p.isRunning { Native.terminateGroup(h.p.processIdentifier) } }
    }
}

extension Router {
    func runCodex(_ req: HTTPRequest, body: [String: Any], model: String?, rid: String, writer: ResponseWriter) async -> Int {
        // worktree brez pisanja: le branje v klonu (brez veje); native z git zavrne (tam native-direct).
        if !config.codex.write, (body["tools"] as? [Any])?.isEmpty == false,
           await Self.shares.isWorktree(session: req.header("x-claude-mini-session") ?? ""),
           await Self.shares.readOnlyInTree(session: req.header("x-claude-mini-session") ?? "", agent: "Codex") {
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
        var tree: (root: URL, branch: String, maven: URL, read: [URL], gits: [String])?
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
                    return await error(why.status, "invalid_request_error", why.message, rid: rid, writer: writer)
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
                guard Worktree.grantDirs(root: tree.root) != nil else {  // #67 H1: pred peskovnikom (tudi .maven)
                    return await error(503, "api_error", Worktree.unsafeFolders, rid: rid, writer: writer)
                }
                extra = Codex.treeDirs(root: tree.root, cwd: dir, maven: tree.maven)
            }
            let cap = await capped(req, kind: "codex")  // #45: način seje nad omejitvijo → le branje
            readOnly = await Self.isReadOnly(req) || cap != nil
            Self.runBranch?.readOnly = readOnly || (tree != nil && !config.codex.write)  // #46a
            if readOnly, tree != nil, AgentRun.current == nil { return await error(400, "invalid_request_error", Self.readOnlyWorktree, rid: rid, writer: writer) }
            if let shared, let clientCwd {
                prompt = Self.mountNote(clientCwd, shared, readOnly: readOnly || (tree != nil && !config.codex.write), synced: synced,
                                        branch: tree?.branch, gits: tree?.gits ?? []) + "\n\n" + prompt
            }
        } else {
            try? FileManager.default.createDirectory(at: cwd, withIntermediateDirectories: true)
        }
        let output = config.scratch("tmp").appendingPathComponent("codex-\(rid).txt")
        defer { try? FileManager.default.removeItem(at: output) }
        // Zasebni TMPDIR v /private/tmp (pisljiv v sandboxu Codex), ne $TMPDIR uporabnika (glej Codex.args); #67: le zunaj
        // worktree (tam je TMPDIR .build/tmp klona pod Scratch)
        let privateTmp = URL(fileURLWithPath: "/private/tmp/minigw-codex-\(rid)", isDirectory: true)
        if tree == nil { try? FileManager.default.createDirectory(at: privateTmp, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700]) }
        defer { if tree == nil { try? FileManager.default.removeItem(at: privateTmp) } }
        let run = agent ? await startAgentRun(req, kind: "codex", dir: sharedDir, readOnly: readOnly || (tree != nil && !config.codex.write)) : nil
        defer { run?.stop() }
        var treeEnv: [String: String]?  // L-N9a: harness Codex sam poganja git v klonu (ovoj kot pri Claude CLI)
        if let t = tree {
            guard let e = Worktree.claudeEnv(root: t.root, maven: t.maven, gitexec: config.scratch("gitexec"), gits: t.gits, agent: "codex") else {
                return await error(503, "api_error", "Cannot prepare git for the agent's clone (git --exec-path): use sync", rid: rid, writer: writer)
            }
            treeEnv = e
            if !config.codex.write || readOnly { treeEnv?["MINIGW_DEPS"] = nil }  // #67: le s pisanjem
            else { treeEnv?["MINIGW_NETWORK_OFF"] = Worktree.networkOff("codex", config) }
        }
        let codexEnv = (treeEnv
            ?? ["TMPDIR": privateTmp.path + "/", "CLANG_MODULE_CACHE_PATH": privateTmp.appendingPathComponent("ModuleCache").path, "xcrun_nocache": "1"])
            .merging(run?.env ?? [:]) { $1 }
        // Nadaljevanje niti Codex (prebrane datoteke, rezultati ukazov): ključ je dosedanji pogovor v tej mapi.
        let scope = Self.agentScope("codex", req, cwd: cwd)
        let existing = agent ? await Self.codexSessions.get(ClaudeCLI.key(all.dropLast(), scope: scope)) : nil
        if existing != nil { prompt = last.text }
        if agent { prompt = agentNote("codex") + "\n\n" + prompt }
        let args = Codex.args(cwd: cwd, extraDirs: extra, agent: agent, output: output, model: model,
                              write: config.codex.write && !readOnly, network: config.codex.network && (tree == nil || config.worktreeCodexNetwork),
                              resume: existing, gits: tree?.gits ?? [],
                              path: tree == nil ? "" : Worktree.agentPath(gitexec: config.scratch("gitexec"), path: ProcessInfo.processInfo.environment["PATH"] ?? ""),
                              deny: [GatewayHistory.defaultFile.deletingLastPathComponent().path])  // L3
        let sessions = Self.codexSessions, session = req.header("x-claude-mini-session") ?? ""
        return await replyWhenDone(body: body, model: "claude-codex", rid: rid, writer: writer, req: req) {
            let (text, thread) = try await Codex.run(exe, args: args, cwd: cwd, prompt: prompt, output: output,
                                                         env: codexEnv, dirs: await Self.shares.runDirs(), stopNested: { run?.stop() }, started: { run?.started($0) }) { [onActivity] s in
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

    /// Pri pretoku glava 200 šele po tem času (nato ping vsakih 10 s). Napaka prej (omejitev agenta) je pravi status HTTP
    /// z x-should-retry: false; po glavi je le dogodek SSE »error«, na katerega Claude Code ponovi zahtevo brez pretoka.
    static let agentHeadDelay: Duration = .seconds(30)

    /// Ponudnik brez pretoka (Siri, Codex, Grok, Copilot): pri pretoku glava, message_start in ping šele po agentHeadDelay,
    /// nato cel odgovor. `req`: omejitev, sporočena v toku, se zapomni za ponovitev brez pretoka (AgentError.note).
    /// `headDelay`: testi krajši, kot parameter (ne globalno), da vzporedni testi ne vplivajo drug na drugega (F10).
    func replyWhenDone(body: [String: Any], model: String, rid: String, writer: ResponseWriter, req: HTTPRequest? = nil,
                       headDelay: Duration = Router.agentHeadDelay, work: () async throws -> (String, [(String, String)])) async -> Int {
        let stream = body["stream"] as? Bool == true
        var t = StreamTranslator(model: body["model"] as? String ?? model)
        let head = HTTPResponse.head(status: 200, headers: [
            ("Content-Type", "text/event-stream"), ("Cache-Control", "no-cache"), ("request-id", rid), ("x-should-retry", "false"),
        ]) + t.start().reduce(Data(), +)
        final class Sent: @unchecked Sendable { var head = false }  // piše le pinger; prebere se po njegovem koncu
        let sent = Sent()
        let pinger: Task<Void, Never>? = stream ? Task {
            try? await Task.sleep(for: headDelay)
            guard !Task.isCancelled else { return }
            sent.head = true
            try? await writer.write(head)
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(10))
                if Task.isCancelled { break }
                try? await writer.write(StreamTranslator.sse("ping", ["type": "ping"]))
            }
        } : nil
        do {
            let (text, extra) = try await work()
            pinger?.cancel()
            await pinger?.value
            let delta = t.emit(text)
            t.usage = ["output_tokens": text.utf8.count / 4]
            if stream {
                try await writer.write((sent.head ? Data() : head) + delta + t.finish().reduce(Data(), +))
                return 200
            }
            let data = (try? JSONSerialization.data(withJSONObject: t.message(), options: [.withoutEscapingSlashes])) ?? Data()
            try? await writer.write(HTTPResponse.head(status: 200, headers: [("Content-Type", "application/json"), ("request-id", rid)] + extra,
                                                      contentLength: data.count) + data)
            return 200
        } catch {
            pinger?.cancel()
            await pinger?.value
            let (status, type, message) = (error as? SiriError)?.status
                ?? (error as? Codex.Failure).map { AgentError.classify($0.message) }
                ?? (error as? Grok.Failure).map { AgentError.classify($0.message) }
                ?? (error as? Copilot.Failure).map { AgentError.classify($0.message) }
                ?? (error as? Native.TreeAlive).map { (500, "api_error", "\($0.agent): " + Native.notStopped(rid)) } ?? (500, "api_error", "\(error)")
            if error is Native.TreeAlive { await Self.shares.abandon() }  // #73: klon sync ne gre v kopijo
            Self.log.error("\(rid, privacy: .public) \(model, privacy: .public) failed: \(message, privacy: .public)")
            let agentLimit = status == 429 && !(error is SiriError)
            if let req, agentLimit, stream, sent.head || writer is EarlyStreamWriter {  // napaka bo dogodek SSE
                AgentError.note(AgentError.key(req, model: t.model), message)
            }
            if sent.head {
                try? await writer.write(StreamTranslator.sse("error", ["type": "error", "error": ["type": type, "message": message]]))
                return status
            }
            return await self.error(status, type, message, rid: rid, extra: agentLimit ? [("x-should-retry", "false")] : [], writer: writer)
        }
    }
}

/// Napaka agenta za odjemalca: omejitev porabe, kvote ali kreditov je 429 rate_limit_error (Claude Code je sporoči, ne
/// ponavlja kot začasno 502), sicer 502 api_error; sporočilo brez poti in žetonov, krajše od 300 znakov.
enum AgentError {
    static let limits = ["usage limit", "rate limit", "quota", "too many requests", "payment required", "spending-limit", "out of credits",
                         "hit your", "reached your", "out of usage", "spend limit"]

    static func classify(_ raw: String) -> (Int, String, String) {
        let low = raw.lowercased(), status = limits.contains { low.contains($0) } ? 429 : 502  // tudi ovoj (»Payment Required«)
        return (status, Router.errorType(status), clean(message(raw)))
    }

    /// Besedilo napake CLI brez ovoja: JSON {"message":…}, {"error":{"message":…}} ali {"error":"…"} (tudi za predpono, npr.
    /// »Internal error: {…}«, in ovoj v ovoju) ter brez »API error (status 402 …):«; sicer vrstica, kot je.
    /// Brez kontrolnih in oblikovnih znakov (razen \n, kot clean v minigw): JSON \u001b bi sicer postal pravi ESC (L4).
    static func message(_ raw: String) -> String {
        var msg = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        for _ in 0..<3 {
            guard let i = msg.firstIndex(of: "{"), let j = msg.lastIndex(of: "}"), i < j,
                  let o = try? JSONSerialization.jsonObject(with: Data(msg[i...j].utf8)) as? [String: Any] else { break }
            let m = o["message"] as? String ?? (o["error"] as? [String: Any])?["message"] as? String ?? o["error"] as? String ?? ""
            let trimmed = m.trimmingCharacters(in: .whitespacesAndNewlines)
            if trimmed.isEmpty { break }
            msg = trimmed
        }
        msg.unicodeScalars.removeAll { $0 != "\n" && ($0.value < 0x20 || (0x7f...0x9f).contains($0.value) || $0.properties.generalCategory == .format) }
        return msg.replacingOccurrences(of: #"^API error \(status \d+[^)]*\):\s*"#, with: "", options: .regularExpression)
    }

    /// Omejitev, sporočena kot dogodek SSE (glava 200 je bila že poslana): Claude Code na tako napako takoj ponovi zahtevo
    /// brez pretoka; `recent` ji 60 s odgovori z istim 429 brez novega zagona CLI (enkrat).
    static let memo = Mutex<[String: (String, Date)]>([:])

    static func key(_ req: HTTPRequest, model: String) -> String {
        [model, req.device?.id ?? "local", req.header("x-claude-mini-session") ?? ""].joined(separator: "|")
    }

    static func note(_ key: String, _ msg: String) {
        memo.withLock { m in
            m = m.filter { $0.value.1.timeIntervalSinceNow > -60 }
            m[key] = (msg, Date())
        }
    }

    static func recent(_ key: String) -> String? {
        memo.withLock { m in m.removeValue(forKey: key).flatMap { $0.1.timeIntervalSinceNow > -60 ? $0.0 : nil } }
    }

    /// Brez besed s `/` ali `\` (poti), razen http(s) naslovov, in brez dolgih žetonov; ID zahteve (`req_…`) ostane.
    static func clean(_ raw: String) -> String {
        let words = raw.split(whereSeparator: \.isWhitespace).filter { w in
            w.hasPrefix("req_") || w.hasPrefix("http://") || w.hasPrefix("https://")
                || !(w.contains("/") || w.contains("\\") || w.hasPrefix("~") || w.count >= 32)
        }
        return String(words.joined(separator: " ").prefix(300))
    }
}
