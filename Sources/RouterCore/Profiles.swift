import CryptoKit
import Foundation
import Network

/// Profili v minigw (#17): en odjemalec, več prehodov. Pripona modela @<profil> izbere prehod; /v1/models združi
/// sezname vseh dosegljivih (privzeti profil še brez pripone). Enako kot odjemalec Go (client/gateways.go).
public enum Profiles {
    public static let defaultName = "default"

    /// L2: zahteve z žetonom prehoda nikoli ne sledijo preusmeritvi (npr. na proxy drugega profila na 127.0.0.1).
    final class NoRedirect: NSObject, URLSessionTaskDelegate, @unchecked Sendable {
        func urlSession(_ s: URLSession, task: URLSessionTask, willPerformHTTPRedirection r: HTTPURLResponse, newRequest: URLRequest) async -> URLRequest? { nil }
    }
    public static let session = URLSession(configuration: .ephemeral, delegate: NoRedirect(), delegateQueue: nil)

    /// Modeli agentov, ki delajo v mapi odjemalca: le zanje se mapa deli z drugim prehodom.
    public static func isAgentModel(_ m: String) -> Bool {
        ["claude-cli-", "claude-codex", "claude-grok", "claude-copilot"].contains { m.hasPrefix($0) }
    }

    /// Male črke, števke in »-« (do 32): ime gre v pripono modela, ime poverilnice in datoteke.
    public static func valid(_ n: String) -> Bool {
        n.range(of: "^[a-z0-9][a-z0-9-]{0,31}$", options: .regularExpression) != nil
    }

    /// »claude-cli-opus@win« → (claude-cli-opus, win), če je win profil; sicer model ostane cel (claude-opus-4@20250514).
    public static func split(_ model: String, names: [String]) -> (model: String, profile: String?) {
        guard let at = model.lastIndex(of: "@"), at > model.startIndex else { return (model, nil) }
        let p = String(model[model.index(after: at)...])
        return names.contains(p) ? (String(model[..<at]), p) : (model, nil)
    }

    /// --model <model>@<profil>: profil izbere prehod za ves zagon (mapa, sync, worktree, native), kot --minigw-profile;
    /// model gre naprej brez pripone (#8). profile nil = noben --model ne konča s profilom.
    public static func modelProfile(_ args: [String], names: [String]) -> (args: [String], profile: String?, model: String) {
        var out = args, profile: String?, model = ""
        for (i, a) in args.enumerated() {
            let eq = a.hasPrefix("--model=")
            guard eq || (a == "--model" && i + 1 < args.count) else { continue }
            let v = eq ? String(a.dropFirst(8)) : args[i + 1]
            let (bare, p) = split(v, names: names)
            guard let p else { continue }
            profile = p; model = v
            if eq { out[i] = "--model=" + bare } else { out[i + 1] = bare }
        }
        return (out, profile, model)
    }

    /// Podedovano okolje, ki ga claude pod minigw ne dobi: ANTHROPIC_*, CLAUDECODE in CLAUDE_CODE_* (seja in prijava
    /// starševskega Claude Code ali Claude Desktop, npr. CLAUDE_CODE_OAUTH_TOKEN: sicer 401 na prehodu). minigw nato
    /// nastavi svoje; CLAUDE_CODE_GIT_BASH_PATH (nastavitev uporabnika) ostane (#7).
    public static func parentClaudeVar(_ key: String) -> Bool {
        let k = key.uppercased()
        return (k.hasPrefix("ANTHROPIC_") || k == "CLAUDECODE" || k.hasPrefix("CLAUDE_CODE_")) && k != "CLAUDE_CODE_GIT_BASH_PATH"
    }

    /// Možnosti claude, ki jih minigw pod Claude Code preda naprej (H3); true = ima vrednost. Druge (npr.
    /// --dangerously-skip-permissions, --permission-mode, --allowedTools, --settings, --mcp-config, --add-dir, --plugin-dir)
    /// bi gnezdenemu claude, ki ga vodi prehod, dale dovoljenja brez vprašanja uporabnika (kot Go claudeSafe). Brez
    /// --input-format (stream-json: nadzorne zahteve na stdin, N1) in -c/-r/--session-id (prepis druge seje gre prehodu).
    static let claudeSafe: [String: Bool] = ["-p": false, "--print": false, "--model": true, "--output-format": true,
        "--verbose": false, "--max-turns": true, "--append-system-prompt": true, "--fallback-model": true, "-h": false, "--help": false]

    /// Prva možnost claude, ki ni na seznamu claudeSafe (nil = vse dovoljene), ko minigw teče v Claude Code (CLAUDECODE),
    /// razen ob MINIGW_ALLOW_UNSAFE_FLAGS=1 uporabnika. Po »--« so le argumenti (kot Go unsafeClaudeFlag).
    public static func unsafeClaudeFlag(_ args: [String], env: [String: String]) -> String? {
        guard checkClaudeArgs(env) else { return nil }
        var i = 0
        while i < args.count {
            let a = args[i]
            if a == "--" { return nil }
            if a.hasPrefix("-") {
                let name = String(a.split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false)[0]), eq = a.contains("=")
                guard let value = claudeSafe[name], !(eq && !name.hasPrefix("--")) else { return name }
                if value && !eq { i += 1 }
            }
            i += 1
        }
        return nil
    }

    /// minigw teče v Claude Code (CLAUDECODE) in uporabnik ni nastavil MINIGW_ALLOW_UNSAFE_FLAGS=1 (kot Go checkClaudeArgs).
    public static func checkClaudeArgs(_ env: [String: String]) -> Bool {
        !(env["CLAUDECODE"] ?? "").isEmpty && env["MINIGW_ALLOW_UNSAFE_FLAGS"] != "1"
    }

    /// N8: pod Claude Code --minigw-work= z načinom, ki piše v mapo brez veje (direct, sync, native-direct), le samo za
    /// branje ali kadar ga mapa že ima shranjenega (share add --work=); MINIGW_WORK v okolju Claude Code vpraša (kot Go unsafeWork).
    public static func unsafeWork(_ flag: String?, saved: String?, readOnly: Bool, env: [String: String]) -> Bool {
        guard let flag, checkClaudeArgs(env), !readOnly, flag != saved else { return false }
        return ["direct", "sync", "native-direct"].contains(flag)
    }

    /// L-31 (kot Go noDirectFallback): pod Claude Code izrecen, neshranjen način (native, worktree) ne pade na direct
    /// (agent bi pisal v mapo brez veje), razen samo za branje.
    public static func noDirectFallback(_ tries: [String], flag: String?, saved: String?, readOnly: Bool, env: [String: String]) -> [String] {
        guard let flag, flag != "auto", flag != saved, !readOnly, checkClaudeArgs(env) else { return tries }
        return tries.filter { $0 != "direct" }
    }

    /// N6, N7: claude -p bere navodilo kot vnos uporabnika: »@pot« (na začetku besede, tudi za ( " ', U+FEFF in 。、？！ kot
    /// pri claude, N6b) priloži katero koli datoteko, »/ukaz« zažene ukaz s poševnico (npr. /config permissionMode=…).
    /// nil = dovoljeno (kot Go promptRefused). Le še varovalka: claude dobi CLAUDE_CODE_DISABLE_FILE_MENTIONS (promptGuard).
    static func promptRefused(_ p: String) -> String? {
        if p.drop(while: { $0.isWhitespace }).hasPrefix("/") { return "a prompt that starts with / (a slash command)" }
        if p.range(of: #"(^|[\s\x{FEFF}\x{3002}\x{3001}\x{FF1F}\x{FF01}("'])@\S"#, options: .regularExpression) != nil {
            return "@ before a file name (claude would attach that file)"
        }
        return nil
    }

    /// N6b, N7: claude brez terminala na stdin ali z -p bere navodilo kot vnos uporabnika: tedaj dobi --disable-slash-commands
    /// in CLAUDE_CODE_DISABLE_FILE_MENTIONS=1. Ostane le pogovor v terminalu, kjer tipka človek (kot Go promptGuard).
    public static func promptGuard(_ args: [String], env: [String: String], stdinTerminal: Bool) -> Bool {
        if !(env["CLAUDECODE"] ?? "").isEmpty || !stdinTerminal { return true }
        return args.prefix { $0 != "--" }.contains { $0 == "-p" || $0 == "--print" }
    }

    /// Razlog za zavrnitev navodila pod Claude Code (nil = dovoljeno): besede, ki niso možnost ali njena vrednost, vse po
    /// »--« in stdin (kot Go unsafePrompt).
    public static func unsafePrompt(_ args: [String], stdin: String, env: [String: String]) -> String? {
        guard checkClaudeArgs(env) else { return nil }
        var words = [stdin], i = 0
        while i < args.count {
            let a = args[i]
            if a == "--" { words += args[(i + 1)...]; break }
            if !a.hasPrefix("-") { words.append(a) } else if claudeSafe[a] == true { i += 1 }
            i += 1
        }
        return words.lazy.compactMap(promptRefused).first
    }

    /// Besedilo prehoda v eni vrstici brez kontrolnih znakov, največ `max` znakov: gre v kontekst Claude (M2, M3).
    public static func line(_ s: String, max: Int) -> String {
        let l = String(String.UnicodeScalarView(s.unicodeScalars.compactMap { u -> Unicode.Scalar? in
            u == "\n" ? " " : (u.value < 0x20 || (0x7f...0x9f).contains(u.value) || u.properties.generalCategory == .format) ? nil : u
        }))
        return l.count > max ? l.prefix(max) + "…" : l
    }

    /// Vrstice »minigw models« (M3): id in opis prehoda v eni vrstici in omejene dolžine (kot Go modelLines).
    public static func modelLines(_ data: [[String: Any]]) -> [String] {
        let ids = data.map { line($0["id"] as? String ?? "?", max: 100) }
        let width = ids.map(\.count).max() ?? 0
        return zip(ids, data).map { $0.padding(toLength: width, withPad: " ", startingAt: 0) + "  " + line($1["description"] as? String ?? "", max: 200) }
    }

    /// M2: besedilo prehoda kot »minigw: gateway: …« v eni vrstici; vrstico o veji izpiše le minigw sam iz preverjenih
    /// podatkov, zato besedilo prehoda nikoli ni videti kot ona (kot Go gatewayNote).
    public static func gatewayNote(_ msg: String) -> String {
        let m = line(msg, max: 500)
        return "minigw: gateway: " + (m.lowercased().contains("agent work is on branch") ? "reported a branch that minigw could not verify; ignored" : m)
    }

    /// Telo zahteve z modelom brez pripone, ime profila in model brez pripone; sicer telo, kot je, in nil.
    public static func routeBody(_ body: Data, names: [String]) -> (body: Data, profile: String?, model: String) {
        guard var o = try? JSONSerialization.jsonObject(with: body) as? [String: Any], let model = o["model"] as? String else { return (body, nil, "") }
        let (bare, p) = split(model, names: names)
        guard let p else { return (body, nil, model) }
        o["model"] = bare
        guard let nb = try? JSONSerialization.data(withJSONObject: o) else { return (body, nil, model) }
        return (nb, p, bare)
    }

    /// Vsak model s pripono @profil, privzeti (prvi) še brez nje; nil = noben prehod ni odgovoril.
    public static func merge(_ lists: [[[String: Any]]?], names: [String]) -> [[String: Any]]? {
        guard lists.contains(where: { $0 != nil }) else { return nil }
        var out: [[String: Any]] = [], seen: Set<String> = []
        func add(_ m: [String: Any], _ id: String, _ suffix: String?) {
            guard seen.insert(id).inserted else { return }
            var c = m
            c["id"] = id
            if let s = suffix, let dn = m["display_name"] as? String, !dn.isEmpty { c["display_name"] = dn + " @" + s }
            out.append(c)
        }
        for (i, l) in lists.enumerated() {
            guard let l else { continue }
            let ms = l.filter { !(($0["id"] as? String) ?? "").isEmpty }  // vrstni red prehoda ostane
            if i == 0 { for m in ms { add(m, m["id"] as! String, nil) } }
            for m in ms { add(m, (m["id"] as! String) + "@" + names[i], names[i]) }
        }
        return out
    }
}

/// Lokalni usmerjevalnik za Claude Code pri več profilih: na 127.0.0.1 z lastnim žetonom; zahteva gre prehodu iz
/// pripone modela (prek njegovega šifriranega proxyja), z njegovim žetonom. Ena zahteva na povezavo (Connection: close).
public final class ProfileMux: @unchecked Sendable {
    public struct Target: Sendable {
        public let name: String, url: String, token: String  // url: lokalni proxy prehoda ali prehod na tem Macu
        public init(name: String, url: String, token: String) { self.name = name; self.url = url; self.token = token }
    }

    public let targets: [Target]
    public let secret = Noise.base64url(Data((0..<32).map { _ in UInt8.random(in: 0...255) }))
    private let shareHooks: [String: @Sendable () -> Bool]  // mapa pri drugem prehodu ob prvi zahtevi zanj
    private let lock = NSLock()
    private var shared: [String: Bool] = [:]
    private let queue = DispatchQueue(label: "dev.trojar.minigw.mux", attributes: .concurrent)
    private var listener: NWListener?
    public private(set) var localPort: UInt16 = 0
    public var localURL: String { "http://127.0.0.1:\(localPort)" }

    public init(targets: [Target], share: [String: @Sendable () -> Bool] = [:]) {
        self.targets = targets; shareHooks = share
    }

    /// Stalen port za ta nabor profilov (Claude Code hrani modele skupaj z ANTHROPIC_BASE_URL), sicer naključen.
    public var stablePort: UInt16 {
        let h = SHA256.hash(data: Data(targets.map { "\($0.name)=\($0.url);" }.joined().utf8)).prefix(2).reduce(0) { $0 << 8 | Int($1) }
        return UInt16(30000 + h % 10000)
    }

    public func start() throws {
        do { try listen(NWEndpoint.Port(rawValue: stablePort)!) } catch { try listen(.any) }
    }

    private func listen(_ lp: NWEndpoint.Port) throws {
        let params = NWParameters.tcp
        params.requiredLocalEndpoint = .hostPort(host: "127.0.0.1", port: lp)
        let l = try NWListener(using: params)
        let ready = DispatchSemaphore(value: 0)
        l.stateUpdateHandler = { s in  // I5: .waiting (npr. zaseden port) je napaka: naključen port, ne čakanje
            switch s {
            case .ready, .failed, .waiting, .cancelled: ready.signal()
            default: break
            }
        }
        l.newConnectionHandler = { [weak self] c in self?.accept(c) }
        l.start(queue: queue)
        ready.wait()
        guard l.state == .ready, let p = l.port?.rawValue, p != 0 else { l.cancel(); throw NWError.posix(.EADDRINUSE) }
        localPort = p; listener = l
    }

    public func stop() { listener?.cancel() }

    private func accept(_ c: NWConnection) {
        c.start(queue: queue)
        read(c, Data())
    }

    private func read(_ c: NWConnection, _ buf: Data) {
        c.receive(minimumIncompleteLength: 1, maximumLength: 1 << 20) { [weak self] data, _, end, error in
            guard let self else { return c.cancel() }
            let b = buf + (data ?? Data())
            switch HTTPParser.parse(b) {
            case .request(let r): self.queue.async { self.handle(r, c) }
            case .error(let st, let msg): self.respond(c, st, Self.error(msg))
            case .needMore: if end || error != nil { c.cancel() } else { self.read(c, b) }
            }
        }
    }

    static func error(_ msg: String) -> Data {
        (try? JSONSerialization.data(withJSONObject: ["type": "error", "error": ["type": "api_error", "message": msg]])) ?? Data()
    }

    private func respond(_ c: NWConnection, _ status: Int, _ body: Data) {
        c.send(content: HTTPResponse.head(status: status, headers: [("Content-Type", "application/json")], contentLength: body.count) + body,
               completion: .contentProcessed { _ in c.cancel() })
    }

    private func authorized(_ r: HTTPRequest) -> Bool {
        var got = r.header("authorization") ?? ""
        if got.hasPrefix("Bearer ") { got.removeFirst(7) }
        if got.isEmpty { got = r.header("x-api-key") ?? "" }
        let a = Array(got.utf8), b = Array(secret.utf8)
        return a.count == b.count && zip(a, b).reduce(0) { $0 | ($1.0 ^ $1.1) } == 0
    }

    private func handle(_ r: HTTPRequest, _ c: NWConnection) {
        guard authorized(r) else { return respond(c, 401, Self.error("invalid token for minigw's local proxy")) }
        // I4: brez kontrolnih znakov v cilju zahteve in vrednostih glav (se posredujejo prehodu)
        let bad = { (s: String) in s.unicodeScalars.contains { $0.value < 0x20 || $0.value == 0x7f } }
        guard !bad(r.path), !bad(r.query ?? ""), !r.headers.contains(where: { bad($0.key) || bad($0.value) }) else {
            return respond(c, 400, Self.error("control characters in the request"))
        }
        if r.method == "GET", r.path == "/v1/models" { return models(c) }
        var t = targets[0], body = r.body
        if r.method == "POST", r.path.hasPrefix("/v1/messages") {
            let (nb, p, model) = Profiles.routeBody(r.body, names: targets.map(\.name))
            if let p, let x = targets.first(where: { $0.name == p }) { t = x; body = nb }
            if t.name != targets[0].name, Profiles.isAgentModel(model), let hook = shareHooks[t.name] {  // le model agenta deli mapo
                lock.lock()  // eno registracijo; druge zahteve za ta prehod počakajo nanjo
                if shared[t.name] == nil { shared[t.name] = hook() }
                lock.unlock()
            }
        }
        forward(r, body, t, c)
    }

    private func isShared(_ name: String) -> Bool { lock.withLock { shared[name] == true } }

    private func forward(_ r: HTTPRequest, _ body: Data, _ t: Target, _ local: NWConnection) {
        guard let u = URLComponents(string: t.url), let h = u.host, let port = NWEndpoint.Port(rawValue: UInt16(u.port ?? 80)) else {
            return respond(local, 502, Self.error("gateway \(t.name): invalid address"))
        }
        let drop: Set<String> = ["host", "authorization", "x-api-key", "content-length", "connection", "keep-alive", "transfer-encoding"]
        var head = "\(r.method) \(r.path)\(r.query.map { "?" + $0 } ?? "") HTTP/1.1\r\nhost: \(h):\(port)\r\n"
        for (k, v) in r.headers where !drop.contains(k) && !(k == "x-claude-mini-share" && t.name != targets[0].name) {
            head += "\(k): \(v)\r\n"
        }
        if t.name != targets[0].name, isShared(t.name) { head += "x-claude-mini-share: 1\r\n" }
        head += "authorization: Bearer \(t.token)\r\ncontent-length: \(body.count)\r\nconnection: close\r\n\r\n"
        let remote = NWConnection(host: NWEndpoint.Host(h), port: port, using: .tcp)
        var started = false
        remote.stateUpdateHandler = { [weak self] s in
            switch s {
            case .ready where !started:
                started = true
                remote.send(content: Data(head.utf8) + body, completion: .contentProcessed { _ in })
                self?.pump(remote, local, first: true, t.name)
            case .failed, .waiting:
                if !started { started = true; remote.cancel(); self?.respond(local, 502, Self.error("gateway \(t.name) is not reachable (minigw status)")) }
            default: break
            }
        }
        local.stateUpdateHandler = { s in if case .cancelled = s { remote.cancel() }; if case .failed = s { remote.cancel() } }
        remote.start(queue: queue)
    }

    /// Prehod → Claude Code brez sprememb (tudi SSE); ob koncu zapre obe strani.
    private func pump(_ remote: NWConnection, _ local: NWConnection, first: Bool, _ name: String) {
        remote.receive(minimumIncompleteLength: 1, maximumLength: 256 * 1024) { [weak self] data, _, end, error in
            if let data, !data.isEmpty { local.send(content: data, completion: .contentProcessed { _ in }) }
            if end || error != nil {
                if first, data?.isEmpty ?? true { self?.respond(local, 502, Self.error("gateway \(name) is not reachable (minigw status)")); remote.cancel(); return }
                local.send(content: nil, contentContext: .finalMessage, isComplete: true, completion: .contentProcessed { _ in local.cancel() })
                remote.cancel()
                return
            }
            self?.pump(remote, local, first: false, name)
        }
    }

    private func models(_ c: NWConnection) {
        var lists = [[[String: Any]]?](repeating: nil, count: targets.count)
        let g = DispatchGroup(), l = NSLock()
        for (i, t) in targets.enumerated() {
            guard let url = URL(string: t.url + "/v1/models") else { continue }
            var req = URLRequest(url: url, timeoutInterval: 10)
            req.setValue("Bearer \(t.token)", forHTTPHeaderField: "authorization")
            g.enter()
            Profiles.session.dataTask(with: req) { d, resp, _ in
                defer { g.leave() }
                guard (resp as? HTTPURLResponse)?.statusCode == 200, let d,
                      let o = try? JSONSerialization.jsonObject(with: d) as? [String: Any] else { return }
                l.withLock { lists[i] = o["data"] as? [[String: Any]] ?? [] }
            }.resume()
        }
        g.wait()
        guard let data = Profiles.merge(lists, names: targets.map(\.name)) else {
            return respond(c, 502, Self.error("no gateway is reachable (minigw status)"))
        }
        var o: [String: Any] = ["data": data, "has_more": false]
        if let f = data.first?["id"], let last = data.last?["id"] { o["first_id"] = f; o["last_id"] = last }
        respond(c, 200, (try? JSONSerialization.data(withJSONObject: o)) ?? Data())
    }
}
