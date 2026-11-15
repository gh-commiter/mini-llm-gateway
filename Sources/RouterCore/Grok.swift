import Foundation

/// Ponudnik `grok -p` (xAI Grok Build CLI): agent v mapi seje, kot Codex. Omejitve postavi jedro (sandbox Grok):
/// »workspace« bere povsod, piše le v mapo seje in začasne mape; »read-only« ne piše v projekt.
public struct GrokConfig: Codable, Sendable, Equatable {
    public var enabled = false
    public var executable = ""            // prazno = samodejno iskanje
    public var enabledModels: [String]?   // nil = vsi modeli računa; seznam = le ti
    public var write = true               // sandbox workspace (sicer read-only)
    public var askOthers = false          // »May ask other models«: agent vpraša drug model prehoda (AgentRun)
    public var work = ""                  // #45: najvišji način za pisanje (sync, worktree); "" = direct, vsi načini
    public init() {}

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let d = GrokConfig()
        enabled = try c.decodeIfPresent(Bool.self, forKey: .enabled) ?? d.enabled
        executable = try c.decodeIfPresent(String.self, forKey: .executable) ?? d.executable
        enabledModels = try c.decodeIfPresent([String].self, forKey: .enabledModels)
        write = try c.decodeIfPresent(Bool.self, forKey: .write) ?? d.write
        askOthers = try c.decodeIfPresent(Bool.self, forKey: .askOthers) ?? d.askOthers
        work = try c.decodeIfPresent(String.self, forKey: .work) ?? d.work
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

    /// 0.6.0 (#106): Grok's own --deny is only a rule of its tools (verified, grok 1.0.77: a python one-liner read a denied
    /// file). A custom sandbox profile with `deny` is kernel-enforced (Seatbelt, read and write). The profiles live in
    /// <GROK_HOME>/sandbox.toml, which Grok's sandbox write-protects; the gateway gives Grok its own home (GROK_HOME,
    /// under Scratch) and keeps the login where it is (GROK_AUTH_PATH), so the user's ~/.grok/sandbox.toml is never touched.
    static let coreProfiles = [("minigw-core", "workspace"), ("minigw-core-ro", "read-only")]
    static let coreUnavailable = "Grok cannot run on this gateway while projects are enabled: its sandbox profile that keeps it out of the core data folder could not be set up"

    static func coreSandboxToml(deny: [String]) -> String? {
        func q(_ s: String) -> String? {
            s.unicodeScalars.contains { $0.value < 0x20 || $0.value == 0x7f } ? nil
                : "\"" + s.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"") + "\""
        }
        let list = deny.compactMap(q)
        guard !deny.isEmpty, list.count == deny.count else { return nil }
        return coreProfiles.map { "[profiles.\($0.0)]\nextends = \"\($0.1)\"\ndeny = [\(list.joined(separator: ", "))]\n" }.joined(separator: "\n")
    }

    /// What Grok's kernel profile must not reach: what `agentDenied` lists plus the folders the sandboxes of the other agents hide (`hiddenScratch`: Scratch with the sync
    /// copies, worktrees and mirrors of other clients, its `.old-*` siblings), except Grok's own login home (the CLI itself reads its auth there).
    static func deniedRoots(scratch: URL?) -> [String] {
        let own = Set(RouterConfig.credentialHomes().filter { !RouterConfig.credentialHomes(except: "grok").contains($0) })
        var seen = Set<String>()
        return (RouterConfig.agentDenied(kind: "grok", scratch: scratch) + RouterConfig.hiddenScratch(scratch).filter { !own.contains($0) }).filter { seen.insert($0).inserted }
    }

    /// #114 for Grok: its profile has no allow side (it reads everywhere), so a hidden root that holds the session folder (`…/Mini Gateway/mounts/<dir>`,
    /// `Scratch/sync/<device>/<proj>`) cannot simply give way: the root's other content (state.json, the other clients' mounts, the core) must stay denied.
    /// The root is replaced by globs for every entry on every level down to the session folder except the one that leads to it (the profile's `deny` takes
    /// gitignore-style globs, kernel-enforced on macOS also for entries created later). Names are matched by character classes: for the name `s1`: `[!s]*`,
    /// `s[!1]*`, `s1?*` and the exact prefix `s`. nil = a name or path that cannot be written that way (a glob character, a space): the run is refused.
    /// What a real grok (1.0.77, measured) starts with: a literal path in `deny` is cheap (400 started), a glob is not: macOS compiles the globs into regexes and fails ("too many
    /// levels of backtracking") at about 41 of them, whatever their length or depth (40 start, 43 do not). So the globs are counted, with a margin, the literals only capped.
    static let maxGlobEntries = 34, maxLiteralEntries = 300
    static func isGlob(_ e: String) -> Bool { e.contains(where: { "*?[".contains($0) }) }

    /// The longest class prefix whose list fits (for a deeper level the prefix is all that tells names apart); none fits = nil (the run is refused, never given a list grok
    /// cannot start with).
    /// Not shorter than 6 characters: a sibling made after launch that shares our first `prefix` characters is not denied (16^6 for hex names), and `prefix` is told to the caller.
    static let minPrefix = 6
    static func scopedDenies(hidden: [String], scope: String) -> [String]? { scopedDeniesWithPrefix(hidden: hidden, scope: scope)?.list }

    static func scopedDeniesWithPrefix(hidden: [String], scope: String) -> (list: [String], prefix: Int)? {
        for prefix in [12, 10, 8, minPrefix] {
            guard let l = scopedDenies(hidden: hidden, scope: scope, prefix: prefix) else { return nil }
            if l.filter(isGlob).count <= maxGlobEntries && l.count - l.filter(isGlob).count <= maxLiteralEntries { return (l, prefix) }
        }
        return nil
    }

    /// Whether the profile denies `path` and everything below it: a literal entry names it or a parent, a glob `G/**` denies what is below a path that `G` matches.
    /// The matching Seatbelt does for the globs (`*` and `[...]` do not cross a `/`); a glob without `/**` denies only its own entry, so it does not count.
    static func denies(_ entries: [String], _ path: String) -> Bool {
        let parts = path.split(separator: "/").map(String.init)
        return entries.contains { e in
            if !isGlob(e) { return path == e || path.hasPrefix(e + "/") }
            guard e.hasSuffix("/**") else { return false }
            let base = String(e.dropLast(3))
            return parts.indices.contains { fnmatch(base, "/" + parts[0...$0].joined(separator: "/"), FNM_PATHNAME) == 0 }
        }
    }

    static func scopedDenies(hidden: [String], scope: String, prefix: Int) -> [String]? {
        func real(_ p: String) -> String { Sync.realResolving(URL(fileURLWithPath: p))?.path ?? p }
        let scopeReal = real(scope)
        var out: [String] = [], seen = Set<String>()
        func add(_ p: String) { if seen.insert(p).inserted { out.append(p) } }
        func addGlob(_ p: String) { add(p); add(p + "/**") }  // a glob names the entry only (real run: `cat s2/x` still worked): its contents too
        func simple(_ n: String) -> Bool { !n.isEmpty && n.unicodeScalars.allSatisfy { ($0.value < 128) && (CharacterSet.alphanumerics.contains($0) || "._-".unicodeScalars.contains($0)) } }
        var plain: [String] = []  // hidden roots that do not hold the session: entries as they are
        for h in hidden {
            let hr = real(h)
            guard scopeReal == hr || scopeReal.hasPrefix(hr + "/") else {
                // an owner-configured root with a glob or control character is refused, as configured and as it resolves (a glob would not match the literal path)
                for name in [h, hr] { guard !name.contains(where: { "*?[".contains($0) }), name.unicodeScalars.allSatisfy({ $0.value >= 0x20 }) else { return nil } }
                plain.append(h); continue
            }
            guard !hr.contains(where: { "*?[".contains($0) }), hr.unicodeScalars.allSatisfy({ $0.value >= 0x20 }) else { return nil }
            let comps = scopeReal.dropFirst(hr.count).split(separator: "/").map(String.init)
            var level = hr
            for (depth, c) in comps.enumerated() {
                guard simple(c) else { return nil }
                // On every level down to the session every child but the one that leads to it is denied, also one made later: the first `prefix` characters of our name are
                // told apart by classes (`s1`: `[!s]*`, `s[!1]*`; the children of the root also as the entry itself, they can be files), names that are a proper prefix of
                // them or that share them and exist now are listed as literals (cheap). Not covered: a sibling made AFTER launch whose name starts with the same `prefix`
                // characters as ours, which the gateway's random names (ids of mounts, devices, projects, clones) do not do.
                let chars = Array(c), top = depth == 0, k = min(chars.count, prefix)
                for i in 0..<k {
                    let g = level + "/" + String(chars[0..<i]) + "[!" + String(chars[i]) + "]*"
                    if top { addGlob(g) } else { add(g + "/**") }
                    if i > 0 { add(level + "/" + String(chars[0..<i])) }  // a name that is a proper prefix of c
                }
                if k == chars.count { if top { addGlob(level + "/" + c + "?*") } else { add(level + "/" + c + "?*/**") } }
                else { add(level + "/" + String(chars[0..<k])) }
                if let sib = try? FileManager.default.contentsOfDirectory(atPath: level) {
                    for n in sib.sorted() where n != c && n.hasPrefix(String(chars[0..<k])) {
                        guard !n.contains(where: { "*?[".contains($0) }), n.unicodeScalars.allSatisfy({ $0.value >= 0x20 }) else { return nil }
                        add(level + "/" + n)
                    }
                }
                level += "/" + c
            }
        }
        // A root that does not hold the session stays as an entry unless the entries above already deny it (proved by the same matching, not assumed).
        // Seatbelt matches the resolved path, so a root reached through a link is also listed as what it resolves to.
        for h in plain where !denies(out, real(h)) { add(h); add(real(h)) }
        return out
    }

    /// Gateway-owned Grok home with the profiles, checked here before each run: env for the process, or nil (refuse).
    /// `login`: the folder of the real login (the user's ~/.grok or $GROK_HOME).
    static func prepareCoreHome(_ home: URL, deny: [String], login: URL = Grok.loginHome()) -> [String: String]? {
        let fm = FileManager.default
        var st = stat()
        if lstat(home.path, &st) == 0, st.st_mode & S_IFMT != S_IFDIR { return nil }  // a link: Grok refuses it too
        guard (try? fm.createDirectory(at: home, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])) != nil,
              let real = Sync.real(home)?.path, let toml = coreSandboxToml(deny: deny) else { return nil }
        let file = URL(fileURLWithPath: real).appendingPathComponent("sandbox.toml")
        if (try? String(contentsOf: file, encoding: .utf8)) != toml {
            let tmp = file.appendingPathExtension("tmp-" + UUID().uuidString)
            guard fm.createFile(atPath: tmp.path, contents: Data(toml.utf8), attributes: [.posixPermissions: 0o600]),
                  rename(tmp.path, file.path) == 0 else { try? fm.removeItem(at: tmp); return nil }
        }
        // verify: the file on disk lists every denied path in both profiles (else the run is refused)
        guard let back = try? String(contentsOf: file, encoding: .utf8), back == toml else { return nil }
        return ["GROK_HOME": real, "GROK_AUTH_PATH": login.appendingPathComponent("auth.json").path]
    }

    static func loginHome() -> URL {
        if let h = ProcessInfo.processInfo.environment["GROK_HOME"], !h.isEmpty { return URL(fileURLWithPath: h) }
        return URL(fileURLWithPath: NSHomeDirectory()).appendingPathComponent(".grok")
    }

    /// `core`: the kernel-enforced profile with the core data folder denied instead of the built-in workspace/read-only.
    static func args(cwd: URL, extraDirs: [String], agent: Bool, model: String?, write: Bool, core: Bool = false) -> [String] {
        var a = ["--cwd", cwd.path, "--output-format", "streaming-json", "--prompt-file", "/dev/stdin"]
        if let model { a += ["-m", model] }
        if agent && write {
            // Jedrni sandbox omeji pisanje na mapo seje (dodatnih map »workspace« ne dovoli, zato brez --add-dir);
            // znotraj njega orodja brez vprašanj (headless).
            a += ["--sandbox", core ? coreProfiles[0].0 : "workspace", "--always-approve"]
        } else {
            a += ["--sandbox", core ? coreProfiles[1].0 : "read-only"]
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
    static func run(_ exe: String, args: [String], cwd: URL, prompt: String, env extra: [String: String] = [:], dirs: [String] = [], checked: AllowedFolders.Checked = .init(),
                    stopNested: @escaping @Sendable () -> Void = {},
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
                // #73: 2 s po koncu grok drevo (tudi vnuk s cevjo) ven, da se branje spodaj konča
                let pipes = [events.fileHandleForReading, errors.fileHandleForReading]
                p.terminationHandler = { p in
                    let g = p.processIdentifier
                    Thread { Thread.sleep(forTimeInterval: 2); Native.killTree(g, pipes: pipes) }.start()
                }
                let held = try checked.open()
                defer { for handle in held { try? handle.close() } }
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
                        case .error(let m)?: failure = m; p.terminate()  // napaka konča tek: ne čakaj, da grok konča sam
                        case .end(let id)?: session = id
                        default: break
                        }
                        buf = Data(buf[(nl + 1)...])
                    }
                }
                p.waitUntilExit()
                while errReader.isExecuting { usleep(10_000) }
                // #73: finalize (syncBack) le, ko drevesa ni več
                stopNested()  // vprašani agenti se končajo urejeno, ne s SIGKILL (kot ClaudeCLI)
                guard Native.killTree(p.processIdentifier, pipes: pipes, dirs: dirs) else { throw Native.TreeAlive(agent: "grok") }
                let text = answer.trimmingCharacters(in: .whitespacesAndNewlines)
                guard failure == nil, !text.isEmpty else {
                    let tail = String(decoding: stderr.data.suffix(400), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
                    throw Failure(message: failure ?? "grok exited with \(p.terminationStatus)" + (tail.isEmpty ? "" : ": \(tail)"))
                }
                return (text, session)
            }
        } onCancel: { if h.p.isRunning { Native.terminateGroup(h.p.processIdentifier) } }
    }
}

extension Router {
    /// Ključ za nadaljevanje seje agenta: naprava + mapa odjemalca. ID seje routerja in pot priklopa
    /// se pri `minigw -c`/`--resume` zamenjata, zato nista v ključu.
    /// 0.6.0: the parts of the key; `conversationScope` joins them (no project: the 0.5.26 key byte for byte) or gives the project's namespace.
    static func agentScopeFields(_ provider: String, _ req: HTTPRequest, cwd: URL) -> [String] {
        [provider, req.device?.id ?? "local", req.header("x-claude-mini-cwd") ?? cwd.path]
    }

    /// The refusal of an agent request whose project the core cannot serve any more (the helper went down after admission): it does not run.
    func projectDown(rid: String, writer: ResponseWriter) async -> Int {
        guard case .refused(let s, let t, let m, _) = Self.projectUnavailable else { return 503 }
        return await refuseProject(s, t, m, retryAfter: 0, rid: rid, writer: writer)
    }

    func runGrok(_ req: HTTPRequest, body: [String: Any], model: String?, rid: String, writer: ResponseWriter) async -> Int {
        // 0.6.0 (#106): with the project core Grok runs only in its kernel-enforced profile that denies the core data folder
        // (Grok.prepareCoreHome); if that cannot be set up (also while the helper is merely not ready) it does not run.
        var coreEnv: [String: String] = [:]
        var held: URL?
        var coreHome: URL?  // the home with the profiles, prepared below once the session folder is known (its deny list depends on it)
        defer { if let held { Self.releaseProjectHome(held) } }  // F8: the project home is held for the whole run
        if core != nil {
            // A project run has its own home per user, project and epoch (projectHome) instead of the shared one: the same profile file in it,
            // the login stays where it is (GROK_AUTH_PATH). Its path is under <Scratch>/state, which every agent's deny list holds.
            var home = config.scratch("grok-home")
            if let p = Self.reqState?.project {
                guard let h = await projectHome(kind: "grok", owner: req.device?.id ?? "local", project: p) else { return await projectDown(rid: rid, writer: writer) }
                held = h
                home = h
            }
            coreHome = home
        }
        // Grok nima zaščite .git v peskovniku, njegov program pa poganja git zunaj njega (in ~/.grok je pisljiv): s pisanjem
        // zavrni; brez pisanja v worktree le bere v klonu (veje ne vrne).
        let sid = req.header("x-claude-mini-session") ?? ""
        var inTree = false
        if (body["tools"] as? [Any])?.isEmpty == false { inTree = await Self.shares.isWorktree(session: sid) }
        var refuse = inTree && config.grok.write
        if inTree, !refuse { refuse = await Self.shares.readOnlyInTree(session: sid, agent: "Grok") }
        if refuse {
            let why = await Self.shares.worktreeRefusal(session: req.header("x-claude-mini-session") ?? "",
                                                        worktree: "Grok is not supported in worktree mode yet; rerun with minigw --minigw-work=sync …",
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
        var checked = AllowedFolders.Checked()
        var readOnly = false
        var prompt = ClaudeCLI.prompt(history: all.dropLast(), last: last)
        var sharedDir: URL?
        if agent {
            let clientCwd = req.header("x-claude-mini-cwd")
            let shared = await Self.shares.mount(session: req.header("x-claude-mini-session") ?? "", cwd: clientCwd, base: localBase,
                                                 wait: req.header("x-claude-mini-share") == "1", device: req.device?.id,
                                                 roots: checkedRoots, scratch: config.scratchURL, writes: config.grok.write)
            let synced = await Self.shares.isSync(session: req.header("x-claude-mini-session") ?? "")
            guard let dir = shared ?? (synced ? nil : ClaudeCLI.resolveCwd(clientCwd, roots: checkedRoots, scratch: config.scratchURL)) else {
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
            checked = allowedFolders.checked(config.claudeCLI.allowedRoots, scratch: config.scratchURL)  // A2b: a cwd that is an allowed root is verified at launch too
            extra = checked.paths(without: dir)
            let cap = await capped(req, kind: "grok")  // #45: način seje nad omejitvijo → le branje
            readOnly = await Self.isReadOnly(req) || cap != nil
            Self.runBranch?.readOnly = readOnly || inTree  // #46a
            if readOnly, inTree, AgentRun.current == nil { return await error(400, "invalid_request_error", Self.readOnlyWorktree, rid: rid, writer: writer) }  // kot Codex
            if let shared, let clientCwd {
                prompt = Self.mountNote(clientCwd, shared, readOnly: readOnly || inTree || !config.grok.write, synced: synced) + "\n\n" + prompt
            }
        } else {
            try? FileManager.default.createDirectory(at: cwd, withIntermediateDirectories: true)
        }
        if let home = coreHome {
            // #116 A2: the gateway's folders are denied to Grok's kernel profile, except the way down to the session folder (a remote client's mount lives inside the app folder)
            guard let (deny, prefix) = Grok.scopedDeniesWithPrefix(hidden: Grok.deniedRoots(scratch: config.scratchURL), scope: cwd.path),
                  let e = Grok.prepareCoreHome(home, deny: deny) else {
                return await error(503, "api_error", Grok.coreUnavailable, rid: rid, writer: writer)
            }
            coreEnv = e
            if prefix < 12 { try? FileHandle.standardError.write(contentsOf: Data("minigw: grok: deny list with a class prefix of \(prefix) characters (\(deny.filter(Grok.isGlob).count) globs, \(deny.count - deny.filter(Grok.isGlob).count) paths)\n".utf8)) }
        }
        var args = Grok.args(cwd: cwd, extraDirs: extra, agent: agent, model: model, write: config.grok.write && !readOnly, core: core != nil)
        // Nadaljevanje Grokove seje (prebrane datoteke, rezultati ukazov): ključ je dosedanji pogovor v tej mapi.
        guard let scope = await conversationScope("agent", Self.agentScopeFields("grok", req, cwd: cwd), project: Self.reqState?.project) else {
            return await projectDown(rid: rid, writer: writer)
        }
        if agent, let existing = await Self.grokSessions.get(ClaudeCLI.key(all.dropLast(), scope: scope)) {
            args += ["-r", existing]
            prompt = last.text
        }
        if agent { prompt = agentNote("grok") + "\n\n" + prompt }
        let sessions = Self.grokSessions, client = req.header("x-claude-mini-session") ?? ""
        let run = agent ? await startAgentRun(req, kind: "grok", dir: sharedDir, readOnly: readOnly || inTree) : nil
        defer { run?.stop() }
        var env = (run?.env ?? [:]).merging(coreEnv) { $1 }
        if inTree, let t = await Self.shares.worktree(session: client) {  // L-N9a: Grok bere v klonu; njegov git z ovojem
            guard let e = Worktree.claudeEnv(root: t.root, maven: t.maven, gitexec: config.scratch("gitexec"), gits: t.gits, agent: "grok") else {
                return await error(503, "api_error", "Cannot prepare git for the agent's clone (git --exec-path): use sync", rid: rid, writer: writer)
            }
            env.merge(e.filter { $0.key != "MINIGW_DEPS" }) { $1 }  // #67: Grok v klonu le bere
        }
        return await replyWhenDone(body: body, model: "claude-grok", rid: rid, writer: writer, req: req) {
            self.dispatched("grok")  // the agent process starts (usage event)
            let (text, session) = try await Grok.run(exe, args: args, cwd: cwd, prompt: prompt, env: env, dirs: await Self.shares.runDirs(), checked: checked, stopNested: { run?.stop() }, started: { run?.started($0) }) { [onActivity] s in onActivity(.step(id: rid, text: s)) }
            run?.stop()  // I2: vprašanja drugim modelom se končajo pred prevzemom sprememb
            // sync: spremembe agenta k odjemalcu pred odgovorom; #82: neprevzete niso uspeh
            if agent, let why = await Self.shares.syncBack(session: client) { throw ShareHub.PullFailed(message: why) }
            if agent, let session {
                await sessions.set(ClaudeCLI.key(all + [ClaudeCLI.Turn(role: "assistant", text: text)], scope: scope), session)
            }
            return (text, [])
        }
    }
}
