import Foundation
import Network
import os

/// Agent vpraša drug model istega prehoda (kot minigw serve, serve_agentcalls.go). Za vsako izvajanje agenta s stikalom
/// »May ask other models« prehod odpre svoja vrata na 127.0.0.1 in žeton, veljavna le med izvajanjem
/// (MINIGW_AGENT_URL, MINIGW_AGENT_TOKEN). Zahteve tam dobijo napravo, naslov, sejo in mapo klicatelja; veriga agentov je
/// le v prehodu (ne v glavi), globina največ 2 in brez ponovitve vrste agenta.
final class AgentRun: @unchecked Sendable {
    let kind: String, chain: [String]
    let peer: String, device: Noise.Device?, session: String, cwd: String
    let dir: URL?        // mapa izvajanja iz ShareHub (kopija, klon, mapa); nil = mapa na tem Macu (resolveCwd)
    let runID: String?   // ShareHub.run klicatelja: vprašani agent dela v istem klonu oz. mestu worktree
    let token: String
    private let listener: NWListener
    private let queue = DispatchQueue(label: "dev.trojar.minigateway.agentrun")
    private var conns: [ObjectIdentifier: NWConnection] = [:]  // le na `queue`
    private(set) var port: UInt16 = 0

    /// Izvajanje, iz katerega pride zahteva (vrata agenta), ali nil.
    @TaskLocal static var current: AgentRun?
    static let chainHeader = "x-minigw-agent-chain"
    static let allowed: Set<String> = ["GET /v1/models", "POST /v1/messages", "POST /v1/messages/count_tokens"]

    var env: [String: String] { ["MINIGW_AGENT_URL": "http://127.0.0.1:\(port)", "MINIGW_AGENT_TOKEN": token] }

    private init(kind: String, chain: [String], req: HTTPRequest, dir: URL?, listener: NWListener) {
        self.kind = kind; self.chain = chain; self.dir = dir; self.listener = listener
        peer = req.peer; device = req.device
        session = req.header("x-claude-mini-session") ?? ""; cwd = req.header("x-claude-mini-cwd") ?? ""
        runID = ShareHub.run
        var b = [UInt8](repeating: 0, count: 32)
        _ = SecRandomCopyBytes(kSecRandomDefault, b.count, &b)
        token = Data(b).base64EncodedString().replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
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
    static func start(router: Router, req: HTTPRequest, kind: String, dir: URL?) async -> AgentRun? {
        let params = NWParameters.tcp
        params.requiredLocalEndpoint = .hostPort(host: "127.0.0.1", port: .any)
        guard let l = try? NWListener(using: params) else { return nil }
        let run = AgentRun(kind: kind, chain: (current?.chain ?? []) + [kind], req: req, dir: dir, listener: l)
        l.newConnectionHandler = { [weak run] c in run?.accept(c, router: router) ?? c.cancel() }
        let ready = await withCheckedContinuation { (k: CheckedContinuation<Bool, Never>) in
            let once = OSAllocatedUnfairLock(initialState: false)
            l.stateUpdateHandler = { s in
                let r: Bool? = switch s { case .ready: true; case .failed, .cancelled: false; default: nil }
                if let r, once.withLock({ let f = $0; $0 = true; return !f }) { k.resume(returning: r) }
            }
            l.start(queue: run.queue)
        }
        guard ready, let port = l.port?.rawValue else { l.cancel(); return nil }
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
            case .request(let req):
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
        var r = req
        for k in ["x-api-key", Self.chainHeader, "x-claude-mini-share"] { r.headers[k] = nil }
        r.headers["authorization"] = "Bearer " + router.token
        r.headers["x-claude-mini-session"] = session
        r.headers["x-claude-mini-cwd"] = cwd
        r.peer = peer
        r.device = device
        await Self.$current.withValue(self) { await router.handle(r, writer: writer) }
    }
}

extension Router {
    /// Vrata izvajanja, če ima agent `kind` stikalo »May ask other models«; okolje gre agentu, `stop()` ob koncu.
    func startAgentRun(_ req: HTTPRequest, kind: String, dir: URL?) async -> AgentRun? {
        let on = switch kind {
        case "claude": config.claudeCLI.askOthers
        case "codex": config.codex.askOthers
        case "grok": config.grok.askOthers
        case "copilot": config.copilot.askOthers
        default: false
        }
        guard on else { return nil }
        let run = await AgentRun.start(router: self, req: req, kind: kind, dir: dir)
        if run == nil { Self.log.error("agent calls: cannot open a loopback port for \(kind, privacy: .public)") }
        return run
    }
}
