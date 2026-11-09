import Foundation
import Network
import os

/// Agent vpraša drug model istega prehoda (kot minigw serve, serve_agentcalls.go). Za vsako izvajanje agenta s stikalom
/// »May ask other models« prehod odpre svoja vrata na 127.0.0.1 in žeton, veljavna le med izvajanjem
/// (MINIGW_AGENT_URL, žeton v datoteki MINIGW_AGENT_AUTH_FILE). Zahteve tam dobijo napravo, naslov, sejo in mapo klicatelja; veriga agentov je
/// le v prehodu (ne v glavi), globina največ 2 in brez ponovitve vrste agenta.
final class AgentRun: @unchecked Sendable {
    let kind: String, chain: [String]
    let peer: String, device: Noise.Device?, session: String, cwd: String
    let role: String  // vloga klicatelja (--minigw-role); vprašani agent je ne more spremeniti
    let readOnly: Bool  // --minigw-ro klicatelja: velja tudi za vprašanega agenta, ta ga ne more odstraniti
    let dir: URL?        // mapa izvajanja iz ShareHub (kopija, klon, mapa); nil = mapa na tem Macu (resolveCwd)
    let runID: String?   // ShareHub.run klicatelja: vprašani agent dela v istem klonu oz. mestu worktree
    let token: String
    /// M3: žeton v datoteki 0600 v začasni mapi izvajanja, ne v okolju (KERN_PROCARGS2 ga drugim procesom istega uporabnika
    /// pokaže tudi v peskovniku); `ownDir`: mapa, ki jo izbriše stop.
    private(set) var authFile = URL(fileURLWithPath: "/dev/null")
    private var ownDir: URL?
    private let listener: NWListener
    private let queue = DispatchQueue(label: "dev.trojar.minigateway.agentrun")
    private var conns: [ObjectIdentifier: NWConnection] = [:]  // le na `queue`
    private(set) var port: UInt16 = 0
    private let rootPID = OSAllocatedUnfairLock<pid_t>(initialState: 0)  // M2: proces agenta (0 = še ne teče)
    let running = OSAllocatedUnfairLock(initialState: 0)                  // M1: hkratna vprašanja tega izvajanja
    /// M1: hkratna vprašanja na izvajanje in vsa vgnezdena izvajanja skupaj (kot maxNested in s.procs v minigw serve).
    static let maxNested = 2, maxAllNested = 6
    private static let allRunning = OSAllocatedUnfairLock(initialState: 0)

    /// Izvajanje, iz katerega pride zahteva (vrata agenta), ali nil.
    @TaskLocal static var current: AgentRun?
    static let chainHeader = "x-minigw-agent-chain"
    static let allowed: Set<String> = ["GET /v1/models", "POST /v1/messages", "POST /v1/messages/count_tokens"]

    var env: [String: String] { ["MINIGW_AGENT_URL": "http://127.0.0.1:\(port)", "MINIGW_AGENT_AUTH_FILE": authFile.path] }

    private init(kind: String, chain: [String], req: HTTPRequest, dir: URL?, readOnly: Bool, listener: NWListener, token: String) {
        self.kind = kind; self.chain = chain; self.dir = dir; self.readOnly = readOnly; self.listener = listener; self.token = token
        peer = req.peer; device = req.device
        session = req.header("x-claude-mini-session") ?? ""; cwd = req.header("x-claude-mini-cwd") ?? ""
        role = GatewayHistory.role(req)
        runID = ShareHub.run
    }

    /// Proces agenta teče (M2: žeton velja le za njegovo drevo procesov).
    func started(_ pid: pid_t) {
        rootPID.withLock { $0 = pid }
        if let runID { Worktree.recordGroup(run: runID, pid: pid) }  // N1: skupina procesov agenta za finalize
    }

    /// Veriga za dnevnik (L3), npr. »claude→codex«.
    var via: String { chain.joined(separator: "→") }

    /// M2: vsi procesi z odjemalčevo vtičnico so agent tega izvajanja ali njegovi potomci (tudi posrednik sandboxa
    /// Claude Code, ki teče v procesu claude).
    func ownsPeer(peerPort: UInt16) -> Bool {
        let root = rootPID.withLock { $0 }
        let pids = Set(PeerProcess.pids(localPort: peerPort, remotePort: port))
        guard root > 0, !pids.isEmpty else { return false }
        return pids.allSatisfy { pid in
            var p = pid
            for _ in 0..<4096 {
                if p == root { return true }
                guard p > 1, let pp = PeerProcess.parent(p), pp != p else { return false }
                p = pp
            }
            return false
        }
    }

    /// Zahteva iz izvajanja za agenta vrste `kind`: nil = dovoljeno, sicer razlog (403). Besedilo kot v Go.
    static func refusal(_ kind: String) -> String? {
        guard let p = current else { return nil }
        let c = p.chain.joined(separator: " → ")
        if p.chain.count >= 2 { return "an agent asked by another agent cannot ask further models (\(c))" }
        if p.chain.contains(kind) { return "\(kind) is already working on this request (\(c)): an agent cannot ask its own kind" }
        return nil
    }

    /// Vrata in žeton za izvajanje agenta `kind` (pred zagonom procesa); nil, ko jih ni mogoče odpreti.
    /// `secretDir`: začasna mapa izvajanja, ki jo vidi le ta agent (Copilot: mapa zahteve); nil = nova v `scratchTmp`.
    /// `readOnly`: agent sam teče le za branje (worktree, pisanje izklopljeno): tudi vprašani agenti (kot Go startRun).
    static func start(router: Router, req: HTTPRequest, kind: String, dir: URL?, secretDir: URL?, scratchTmp: URL, readOnly own: Bool = false) async -> AgentRun? {
        let params = NWParameters.tcp
        params.requiredLocalEndpoint = .hostPort(host: "127.0.0.1", port: .any)
        var b = [UInt8](repeating: 0, count: 32)
        guard SecRandomCopyBytes(kSecRandomDefault, b.count, &b) == errSecSuccess else { return nil }  // L2
        let token = Data(b).base64EncodedString().replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
        let readOnly = own ? true : await Router.isReadOnly(req)
        guard let l = try? NWListener(using: params) else { return nil }
        let run = AgentRun(kind: kind, chain: (current?.chain ?? []) + [kind], req: req, dir: dir, readOnly: readOnly, listener: l, token: token)
        var folder = secretDir
        if folder == nil {
            let d = scratchTmp.appendingPathComponent("run-" + UUID().uuidString, isDirectory: true)
            guard (try? FileManager.default.createDirectory(at: d, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])) != nil
            else { l.cancel(); return nil }
            folder = d; run.ownDir = d
        }
        run.authFile = folder!.appendingPathComponent("minigw-agent-auth")
        guard FileManager.default.createFile(atPath: run.authFile.path, contents: Data(token.utf8), attributes: [.posixPermissions: 0o600])
        else { run.stop(); return nil }
        l.newConnectionHandler = { [weak run] c in run?.accept(c, router: router) ?? c.cancel() }
        let ready = await withCheckedContinuation { (k: CheckedContinuation<Bool, Never>) in
            let once = OSAllocatedUnfairLock(initialState: false)
            l.stateUpdateHandler = { s in
                let r: Bool? = switch s { case .ready: true; case .failed, .cancelled: false; default: nil }
                if let r, once.withLock({ let f = $0; $0 = true; return !f }) { k.resume(returning: r) }
            }
            l.start(queue: run.queue)
        }
        guard ready, let port = l.port?.rawValue else { run.stop(); return nil }
        run.port = port
        return run
    }

    /// Konec izvajanja: vrata zaprta, tekoče vgnezdene zahteve prekinjene.
    func stop() {
        queue.sync {
            listener.cancel()
            for c in conns.values { c.cancel() }
            conns = [:]
        }
        try? FileManager.default.removeItem(at: authFile)
        if let ownDir { try? FileManager.default.removeItem(at: ownDir) }
    }

    private func accept(_ c: NWConnection, router: Router) {
        conns[ObjectIdentifier(c)] = c
        c.stateUpdateHandler = { [weak self] s in
            switch s {
            case .failed, .cancelled: self?.queue.async { self?.conns[ObjectIdentifier(c)] = nil }
            default: break
            }
        }
        c.start(queue: queue)
        read(c, Data(), router: router)
    }

    private func read(_ c: NWConnection, _ buf: Data, router: Router) {
        c.receive(minimumIncompleteLength: 1, maximumLength: 64 * 1024) { [weak self] data, _, done, error in
            guard let self else { c.cancel(); return }
            var buf = buf
            if let data { buf.append(data) }
            let w = ConnectionWriter(conn: c, cipher: nil)
            switch HTTPParser.parse(buf) {
            case .needMore:
                if done || error != nil { c.cancel() } else { self.read(c, buf, router: router) }
            case .error(let status, let message):
                Task { await router.reject(status: status, message: message, writer: w) }
            case .request(var req):
                if case let .hostPort(_, p) = c.endpoint { req.peerPort = p.rawValue }
                let task = Task { await self.serve(req, router: router, writer: w) }
                c.stateUpdateHandler = { [weak self] s in
                    switch s {
                    case .failed, .cancelled:
                        task.cancel()
                        self?.queue.async { self?.conns[ObjectIdentifier(c)] = nil }
                    default: break
                    }
                }
            }
        }
    }

    /// Žeton izvajanja in tri poti; nato zahteva z identiteto klicatelja skozi Router.handle.
    private func serve(_ req: HTTPRequest, router: Router, writer: ResponseWriter) async {
        let bearer = req.header("authorization").flatMap { $0.hasPrefix("Bearer ") ? String($0.dropFirst(7)) : nil }
        let given = Array((bearer ?? req.header("x-api-key") ?? "").utf8), want = Array(token.utf8)
        guard given.count == want.count, zip(given, want).reduce(0, { $0 | ($1.0 ^ $1.1) }) == 0 else {
            return await router.reject(status: 401, message: "Invalid token for this agent run", writer: writer)
        }
        guard Self.allowed.contains(req.method + " " + req.path) else {
            return await router.reject(status: 404, message: "Not available to agents: \(req.method) \(req.path)", writer: writer)
        }
        guard await offPool({ self.ownsPeer(peerPort: req.peerPort) }) else {  // M2
            return await router.reject(status: 403, message: "Not a process of this agent run", writer: writer)
        }
        // M1: največ maxNested na izvajanje in maxAllNested vseh vgnezdenih hkrati; sicer 429 (čakanje bi lahko zaklenilo).
        let ok = running.withLock { n in
            Self.allRunning.withLock { all in
                guard n < Self.maxNested, all < Self.maxAllNested else { return false }
                n += 1; all += 1
                return true
            }
        }
        guard ok else {
            return await router.reject(status: 429, message: "This agent already has \(Self.maxNested) requests to other models running, or the gateway is busy", writer: writer)
        }
        defer { running.withLock { $0 -= 1 }; Self.allRunning.withLock { $0 -= 1 } }
        var r = req
        for k in ["x-api-key", Self.chainHeader, "x-claude-mini-share"] { r.headers[k] = nil }
        r.headers["authorization"] = "Bearer " + router.token
        r.headers["x-claude-mini-session"] = session
        r.headers["x-claude-mini-cwd"] = cwd
        r.headers["x-claude-mini-role"] = role.isEmpty ? nil : role  // vloga iz izvajanja, ne od vprašanega agenta
        r.headers["x-claude-mini-ro"] = readOnly ? "1" : nil  // --minigw-ro iz izvajanja, ne od vprašanega agenta
        r.peer = peer
        r.device = device
        await Self.$current.withValue(self) { await router.handle(r, writer: writer) }
    }
}

extension Router {
    /// Stikalo »May ask other models« agenta `kind` (Claude CLI le z »Allow local tools«: brez Bash ne more klicati minigw).
    func askOthers(_ kind: String) -> Bool {
        switch kind {
        case "claude": config.claudeCLI.askOthers && config.claudeCLI.allowLocalTools
        case "codex": config.codex.askOthers
        case "grok": config.grok.askOthers
        case "copilot": config.copilot.askOthers
        default: false
        }
    }

    /// minigw te aplikacije (Contents/Helpers): agent z njim vpraša druge modele.
    static var minigwExe: String { Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/minigw").path }

    /// Navodilo agentu `kind`: vprašal ga je drug agent (mapa klicatelja) in/ali sme vprašati druge modele. Kot v Go.
    func agentNote(_ kind: String) -> String? {
        var notes: [String] = []
        if let p = AgentRun.current {
            let dir = p.dir?.path ?? p.cwd
            notes.append("Another agent on this gateway (\(p.chain.joined(separator: " → "))) asks you this. You work in its folder \(dir), "
                + "which is the user's folder \(p.cwd) on this gateway: map paths under \(p.cwd) to it. Answer the question; change files only if asked.")
        }
        if askOthers(kind) {
            let exe = Self.minigwExe
            notes.append("You may ask another model of this gateway: `\"\(exe)\" -p --model <id> \"…\"`; `\"\(exe)\" models` lists them.")
        }
        return notes.isEmpty ? nil : notes.joined(separator: "\n\n")
    }

    /// Vrata izvajanja, če ima agent `kind` stikalo »May ask other models«; okolje gre agentu, `stop()` ob koncu.
    func startAgentRun(_ req: HTTPRequest, kind: String, dir: URL?, secretDir: URL? = nil, readOnly: Bool = false) async -> AgentRun? {
        guard askOthers(kind) else { return nil }
        let run = await AgentRun.start(router: self, req: req, kind: kind, dir: dir, secretDir: secretDir, scratchTmp: config.scratch("tmp"), readOnly: readOnly)
        if run == nil { Self.log.error("agent calls: cannot open a loopback port for \(kind, privacy: .public)") }
        return run
    }
}
