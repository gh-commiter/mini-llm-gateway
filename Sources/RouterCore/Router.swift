import Foundation
import FoundationModels
import os

public struct RequestLog: Sendable, Identifiable, Codable {
    public var id = UUID()
    public let time: Date
    public let method: String
    public let path: String
    public let model: String?
    public let provider: String?
    public let status: Int
    public let ms: Int
}

/// Tekoča zahteva za »Activity«: začetek, zadnji korak agenta (ukaz, datoteka), konec. Le metapodatki.
public enum Activity: Sendable {
    case started(id: String, time: Date, model: String)
    case step(id: String, text: String)
    case finished(id: String)
}

struct Route: Sendable {
    let provider: ProviderConfig
    let name: String      // ime v nastavitvah ali ID pri upstreamu
    let alias: String     // objavljeni ID (s predpono claude-)
    let upstream: String
    let window: Int?
}

/// Overitev, usmerjanje po imenu modela in passthrough do zunanjih ponudnikov.
/// Brez vsebine zahtev v dnevnikih.
public final class Router: Sendable {
    public let config: RouterConfig
    let token: String
    let secrets: SecretStore
    let session: URLSession
    let onLog: @Sendable (RequestLog) -> Void
    let onDevice: @Sendable (Noise.Device) -> Void  // aplikacija doda novo napravo v nastavitve, beleži »zadnjič«
    let onActivity: @Sendable (Activity) -> Void
    let admin: AdminHooks?  // skrbniški API (aplikacija); nil = ni ga
    let catalog: Catalog
    let probes = Probes()
    static let log = Logger(subsystem: "dev.trojar.minigateway", category: "router")

    public init(config: RouterConfig, token: String, secrets: SecretStore,
                session: URLSession = Router.defaultSession,
                onLog: @escaping @Sendable (RequestLog) -> Void = { _ in },
                onDevice: @escaping @Sendable (Noise.Device) -> Void = { _ in },
                onActivity: @escaping @Sendable (Activity) -> Void = { _ in },
                admin: AdminHooks? = nil) {
        self.config = config
        self.admin = admin
        self.onDevice = onDevice
        self.onActivity = onActivity
        self.token = token
        self.secrets = secrets
        self.session = session
        self.onLog = onLog
        Detected.start()  // programi agentov (zsh -lic) se iščejo na svoji niti, preden jih zahteva potrebuje
        // Brez ključa ponudnik ne deluje → kot izklopljen (ne objavi modelov, ne usmerja, ne poskuša odkrivanja).
        catalog = Catalog(providers: config.providers.filter { $0.enabled && !(secrets.get($0.id) ?? "").isEmpty },
                          secrets: secrets, session: session)
        let (catalog, cli, probes) = (self.catalog, config.claudeCLI, self.probes)
        Task {
            await catalog.refreshAll()  // odkrivanje modelov takoj ob vklopu/shranjevanju
            if cli.enabled {
                await probes.start("claude-cli") {
                    guard let exe = await ClaudeCLI.executable(cli.executable) else { return false }
                    return await offPool { if case .success = ClaudeCLI.validate(exe) { true } else { false } }
                }
            }
        }
    }

    public static let defaultSession: URLSession = {
        let c = URLSessionConfiguration.ephemeral
        c.timeoutIntervalForRequest = 1800
        c.timeoutIntervalForResource = 3600
        return URLSession(configuration: c)
    }()

    public func handle(_ req: HTTPRequest, writer: ResponseWriter) async {
        let start = Date()
        let rid = "req_" + UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased()
        if let d = req.device { onDevice(d) }
        let tracked = req.method == "POST" && req.path.hasPrefix("/v1/messages") && !req.path.hasSuffix("/count_tokens")
        if tracked, let model = (try? JSONSerialization.jsonObject(with: req.body) as? [String: Any])?["model"] as? String {
            onActivity(.started(id: rid, time: start, model: model))
        }
        defer { if tracked { onActivity(.finished(id: rid)) } }
        let (status, model, provider) = await route(req, rid: rid, writer: writer)
        writer.close()
        let ms = Int(Date().timeIntervalSince(start) * 1000)
        Self.log.info("\(rid, privacy: .public) \(req.method, privacy: .public) \(req.path.hasPrefix("/dav/") ? "/dav/…" : req.path, privacy: .public) model=\(model ?? "-", privacy: .public) provider=\(provider ?? "-", privacy: .public) status=\(status) \(ms)ms bytes=\(req.body.count)")
        if provider != "share" { onLog(RequestLog(time: start, method: req.method, path: req.path, model: model, provider: provider, status: status, ms: ms)) }  // brez prometa map v »Activity«
    }

    /// Za napake, ki nastanejo že pri branju zahteve.
    public func reject(status: Int, message: String, writer: ResponseWriter) async {
        _ = await error(status, Self.errorType(status), message, rid: "req_parse", writer: writer)
        writer.close()
    }

    private func route(_ req: HTTPRequest, rid: String, writer: ResponseWriter) async -> (Int, String?, String?) {
        switch (req.method, req.path) {
        case ("GET", "/health/live"):
            return (await json(200, ["status": "ok"], rid: rid, writer: writer), nil, nil)
        case ("GET", "/health/ready"):
            var providers: [String: Bool] = [:]
            for (id, ok) in await catalog.status() { providers[id] = ok }
            if config.siri.enabled { providers["siri-ai"] = SiriAX.trusted(prompt: false) && SiriAX.isRunning() }
            return (await json(200, ["status": "ok", "providers": providers], rid: rid, writer: writer), nil, nil)
        default:
            break
        }

        // Priklop mape odjemalca (mount_webdav na tem Macu): skrivnost v poti namesto žetona.
        if req.path.hasPrefix("/dav/") {
            guard RouterServer.isLoopback(req.peer) else {  // le lokalni mount_webdav
                try? await writer.write(HTTPResponse.head(status: 404, headers: [], contentLength: 0))
                return (404, nil, nil)
            }
            let secret = String(req.path.dropFirst(5).prefix { $0 != "/" })
            let peerPort = req.peerPort, port = config.port
            let peer = ShareHub.DAVPeer.process(find: { PeerProcess.pid(localPort: peerPort, remotePort: port) },
                                                owns: { PeerProcess.owns($0, localPort: peerPort, remotePort: port) })
            guard let r = await Self.shares.dav(secret: secret, method: req.method, path: req.path,
                                                headers: req.headers, body: req.body, peer: peer) else {
                try? await writer.write(HTTPResponse.head(status: 404, headers: [], contentLength: 0))
                return (404, nil, nil)
            }
            var headers = r.headers.map { ($0.key, $0.value) }
            let length = r.headers["Content-Length"] == nil ? r.body.count : nil
            if length == nil { headers = headers.filter { $0.0 != "Content-Length" } + [("Content-Length", r.headers["Content-Length"]!)] }
            try? await writer.write(HTTPResponse.head(status: r.status, headers: headers, contentLength: length) + r.body)
            return (r.status, nil, "share")
        }

        if req.path == Self.adminPrefix || req.path.hasPrefix(Self.adminPrefix + "/") {  // lastna skrivnost, ne žeton
            return (await adminHTTP(req, rid: rid, writer: writer), nil, "admin")
        }
        guard authorized(req) else {
            return (await error(401, "authentication_error", "Invalid or missing token", rid: rid, writer: writer), nil, nil)
        }

        switch (req.method, req.path) {
        case ("GET", "/v1/models"):
            return (await json(200, await modelList(siri: config.siriAllowed(peer: req.peer, device: req.device)), rid: rid, writer: writer), nil, nil)

        case ("POST", "/v1/messages"), ("POST", "/v1/messages/count_tokens"):
            guard let body = try? JSONSerialization.jsonObject(with: req.body) as? [String: Any],
                  let model = body["model"] as? String else {
                return (await error(400, "invalid_request_error", "Body must be JSON with a model field", rid: rid, writer: writer), nil, nil)
            }
            guard let r = await catalog.route(model) else {
                if config.siri.enabled, Siri.ids.contains(model) {
                    guard config.siriAllowed(peer: req.peer, device: req.device) else {
                        let who = req.device.map { "device “\($0.name)”" } ?? "this client (update minigw to 0.4.4 or newer)"
                        return (await error(400, "invalid_request_error",  // 403 Claude Code prikaže kot napako prijave
                                            "Siri AI is not allowed for \(who): allow it in Mini Gateway → Settings → Siri AI → Devices",
                                            rid: rid, writer: writer), model, "siri-ai")
                    }
                    if req.path.hasSuffix("/count_tokens") {
                        return (await json(200, ["input_tokens": req.body.count / 4], rid: rid, writer: writer), model, "siri-ai")
                    }
                    return (await runSiri(req, body: body, rid: rid, writer: writer), model, "siri-ai")
                }
                if config.grok.enabled, let grokModel = Grok.match(model), grokModel.map(config.grok.isModelEnabled) ?? true {
                    if req.path.hasSuffix("/count_tokens") {
                        return (await json(200, ["input_tokens": req.body.count / 4], rid: rid, writer: writer), model, "grok")
                    }
                    return (await syncRun(agent: "grok") { await runGrok(req, body: body, model: grokModel, rid: rid, writer: await early(req, body, rid, writer)) }, model, "grok")
                }
                if config.codex.enabled, let codexModel = Codex.match(model), codexModel.map(config.codex.isModelEnabled) ?? true {
                    if req.path.hasSuffix("/count_tokens") {
                        return (await json(200, ["input_tokens": req.body.count / 4], rid: rid, writer: writer), model, "codex")
                    }
                    return (await syncRun(agent: "codex") { await runCodex(req, body: body, model: codexModel, rid: rid, writer: await early(req, body, rid, writer)) }, model, "codex")
                }
                if config.foundry.enabled, model.hasPrefix(Foundry.prefix), Foundry.validModel(String(model.dropFirst(Foundry.prefix.count))) {
                    if req.path.hasSuffix("/count_tokens") {
                        return (await json(200, ["input_tokens": req.body.count / 4], rid: rid, writer: writer), model, "foundry")
                    }
                    return (await runFoundry(req, body: body, model: String(model.dropFirst(Foundry.prefix.count)), rid: rid, writer: writer), model, "foundry")
                }
                if config.copilot.enabled, let copilotModel = Copilot.match(model) {
                    if req.path.hasSuffix("/count_tokens") {
                        return (await json(200, ["input_tokens": req.body.count / 4], rid: rid, writer: writer), model, "copilot")
                    }
                    return (await syncRun(agent: "copilot") { await runCopilot(req, body: body, model: copilotModel, rid: rid, writer: writer) }, model, "copilot")
                }
                if config.appleFM.enabled, AppleFM.ids.contains(model) {
                    if req.path.hasSuffix("/count_tokens") {
                        return (await json(200, ["input_tokens": req.body.count / 4], rid: rid, writer: writer), model, "apple-fm")
                    }
                    return (await runFM(req, body: body, rid: rid, writer: writer), model, "apple-fm")
                }
                if config.claudeCLI.enabled, let m = ClaudeCLI.match(model) {
                    if req.path.hasSuffix("/count_tokens") {
                        return (await json(200, ["input_tokens": req.body.count / 4], rid: rid, writer: writer), model, "claude-cli")
                    }
                    return (await syncRun(agent: "claude") { await runCLI(req, body: body, cliModel: m.cliModel, chatOnly: m.chatOnly, rid: rid, writer: await early(req, body, rid, writer)) },
                            model, "claude-cli")
                }
                return (await error(404, "not_found_error", "Unknown model: \(model)", rid: rid, writer: writer), model, nil)
            }
            // Claude Code za neznan ID »claude-…« privzame 200K; pravo okno uveljavi router.
            // Ta napaka v Claude Code sproži reaktivno kompakcijo.
            if let window = r.window, req.path == "/v1/messages", req.body.count / 4 > window {
                // ponytail: ocena bajti/4; upstream count_tokens, če bo ocena premalo natančna
                return (await error(400, "invalid_request_error", "prompt is too long: \(req.body.count / 4) tokens > \(window) maximum",
                                    rid: rid, writer: writer), model, r.provider.id)
            }
            guard let key = secrets.get(r.provider.id), !key.isEmpty else {
                return (await error(503, "api_error", "No API key configured for \(r.provider.name)", rid: rid, writer: writer), model, r.provider.id)
            }
            let status = await passthrough(req, body: body, route: r, key: key, rid: rid, writer: writer)
            return (status, model, r.provider.id)

        // minigw: mapa odjemalca za claude -p (glej ShareHub).
        case ("POST", "/minigw/share"):
            guard let session = req.header("x-claude-mini-session"), !session.isEmpty,
                  let cwd = req.header("x-claude-mini-cwd"), !cwd.isEmpty else {
                return (await error(400, "invalid_request_error", "Missing session or cwd", rid: rid, writer: writer), nil, nil)
            }
            // --minigw-work=sync: kopija v scratch/sync/<naprava>/<mapa>, ohranjena med sejami; druga naprava ali mapa je ne doseže.
            // Oddaljen odjemalec brez dokaza naprave bi si delil kopijo z drugimi → sync zavrnjen.
            let sync = req.header("x-claude-mini-work") == "sync"
            let owner = Self.nativeOwner(req)
            // --minigw-work=native: mapa iz izziva (POST /minigw/native), ki ga je odjemalec rešil skozi svoj priklop.
            var native: URL?
            if req.header("x-claude-mini-work") == "native" {
                guard req.header("x-share-mode") != "ro" else {  // brez priklopa rdonly ni mogoče zagotoviti
                    return (await error(400, "invalid_request_error", "Native mode cannot be read-only", rid: rid, writer: writer), nil, "share")
                }
                guard let owner, let dir = await Self.natives.verify(id: req.header("x-native-id") ?? "", proof: req.header("x-native-proof") ?? "", owner: owner) else {
                    return (await error(403, "permission_error", "Native mode: the folder check failed", rid: rid, writer: writer), nil, "share")
                }
                native = dir
            }
            // --minigw-work=worktree: golo zrcalo v scratch/repos/<naprava>/<repozitorij>.git, kloni v scratch/worktrees (Worktree).
            let worktree = req.header("x-claude-mini-work") == "worktree"
            if sync || worktree, owner == nil {
                return (await error(400, "invalid_request_error", "Sync needs an encrypted connection with a device key (run minigw connect again)",
                                    rid: rid, writer: writer), nil, "share")
            }
            let readOnly = req.header("x-share-mode") == "ro"
            var tree: ShareHub.Tree?
            if worktree {
                let top = req.header("x-worktree-top") ?? ""
                guard Worktree.scratchIsSafe(config.scratchURL) else {
                    return (await error(400, "invalid_request_error", "Worktree mode: the Scratch folder must not be in a temporary or build cache folder",
                                        rid: rid, writer: writer), nil, "share")
                }
                guard !readOnly, !top.isEmpty, cwd == top || cwd.hasPrefix(top.hasSuffix("/") ? top : top + "/") else {
                    return (await error(400, "invalid_request_error", "Worktree mode needs a writable git repository that contains the folder",
                                        rid: rid, writer: writer), nil, "share")
                }
                let sub = String(cwd.dropFirst(top.count)).trimmingCharacters(in: CharacterSet(charactersIn: "/"))
                guard sub.isEmpty || !sub.split(separator: "/", omittingEmptySubsequences: false)
                    .contains(where: { $0.isEmpty || $0 == "." || $0 == ".." || $0.contains("\0") || Sync.isGitPath(String($0)) }) else {
                    return (await error(400, "invalid_request_error", "Bad folder path", rid: rid, writer: writer), nil, "share")
                }
                let id = Sync.folderID(top)
                tree = ShareHub.Tree(repo: config.scratch("repos").appendingPathComponent(owner!).appendingPathComponent(id + ".git"),
                                     worktrees: config.scratch("worktrees").appendingPathComponent(owner!).appendingPathComponent(id), sub: sub)
            }
            let copy = sync ? config.scratch("sync").appendingPathComponent(owner!)  // samo za branje: ločena kopija
                .appendingPathComponent(Sync.folderID(cwd) + (readOnly ? "-ro" : "")) : nil
            if let copy, Sync.tooManyCopies(copy) {
                return (await error(429, "rate_limit_error", "This device already has \(Sync.maxCopies) synced folders on the gateway; unused ones expire after 14 days",
                                    rid: rid, writer: writer), nil, "share")
            }
            guard let key = await Self.shares.register(session: session, cwd: cwd, key: req.header("x-share-key"),
                                                       readOnly: readOnly, copy: copy, device: req.device?.id, native: native, tree: tree) else {
                return (await error(409, "invalid_request_error", "Session already has a folder", rid: rid, writer: writer), nil, "share")
            }
            return (await json(200, ["key": key], rid: rid, writer: writer), nil, "share")
        case ("GET", "/minigw/share/next"):
            switch await Self.shares.poll(key: req.header("x-share-key") ?? "") {
            case .none:
                return (await error(404, "not_found_error", "No such share", rid: rid, writer: writer), nil, "share")
            case .some(.none):
                try? await writer.write(HTTPResponse.head(status: 204, headers: [], contentLength: 0))
                return (204, nil, "share")
            case .some(.some(let job)):
                let body = (try? JSONEncoder().encode(job)) ?? Data()
                try? await writer.write(HTTPResponse.head(status: 200, headers: [("Content-Type", "application/json")], contentLength: body.count) + body)
                return (200, nil, "share")
            }
        case ("POST", "/minigw/share/reply"):
            guard let r = try? JSONDecoder().decode(DAVReply.self, from: req.body) else {
                return (await error(400, "invalid_request_error", "Bad reply", rid: rid, writer: writer), nil, "share")
            }
            let ok = await Self.shares.reply(key: req.header("x-share-key") ?? "", r)
            try? await writer.write(HTTPResponse.head(status: ok ? 204 : 404, headers: [], contentLength: 0))
            return (ok ? 204 : 404, nil, "share")
        // minigw --minigw-work=sync (glej Sync): manifest, datoteke k routerju, spremembe agenta nazaj.
        case ("POST", "/minigw/sync/manifest"):
            guard let files = try? JSONDecoder().decode([Sync.Entry].self, from: req.body) else {
                return (await error(400, "invalid_request_error", "Bad manifest", rid: rid, writer: writer), nil, "share")
            }
            if let why = Sync.overQuota(files) {  // #1, #9: odjemalec ne napolni diska routerja
                return (await error(413, "invalid_request_error", why, rid: rid, writer: writer), nil, "share")
            }
            await Self.shares.expireCopies(config.scratch("sync"))  // #1, #9: največ enkrat na dan
            guard let r = await Self.shares.syncManifest(key: req.header("x-share-key") ?? "", files,
                                                         execKnown: req.header("x-sync-exec") != "none", device: req.device?.id) else {  // Windows
                return (await error(404, "not_found_error", "No such share", rid: rid, writer: writer), nil, "share")
            }
            return (await json(200, ["need": r.need, "kept": r.kept], rid: rid, writer: writer), nil, "share")
        case ("POST", "/minigw/sync/ack"):
            guard let paths = try? JSONDecoder().decode([String].self, from: req.body),
                  await Self.shares.syncAck(key: req.header("x-share-key") ?? "", paths, device: req.device?.id) else {
                return (await error(404, "not_found_error", "No such share", rid: rid, writer: writer), nil, "share")
            }
            try? await writer.write(HTTPResponse.head(status: 204, headers: [], contentLength: 0))
            return (204, nil, "share")
        case ("PUT", "/minigw/sync/file"):
            let st = await Self.shares.syncPut(key: req.header("x-share-key") ?? "",
                                               path: req.header("x-sync-path")?.removingPercentEncoding ?? "", data: req.body, device: req.device?.id)
            try? await writer.write(HTTPResponse.head(status: st, headers: [], contentLength: 0))
            return (st, nil, "share")
        case ("GET", "/minigw/sync/changes"):
            guard let ch = await Self.shares.syncChanges(key: req.header("x-share-key") ?? "", device: req.device?.id), let body = try? JSONEncoder().encode(ch) else {
                return (await error(404, "not_found_error", "No such share", rid: rid, writer: writer), nil, "share")
            }
            try? await writer.write(HTTPResponse.head(status: 200, headers: [("Content-Type", "application/json")], contentLength: body.count) + body)
            return (200, nil, "share")
        case ("GET", "/minigw/sync/file"):
            guard let body = await Self.shares.syncGet(key: req.header("x-share-key") ?? "",
                                                       path: req.header("x-sync-path")?.removingPercentEncoding ?? "", device: req.device?.id) else {
                return (await error(404, "not_found_error", "No such file", rid: rid, writer: writer), nil, "share")
            }
            try? await writer.write(HTTPResponse.head(status: 200, headers: [("Content-Type", "application/octet-stream")], contentLength: body.count) + body)
            return (200, nil, "share")
        // minigw --minigw-work=worktree: bundle stanja k routerju (po delih), bundle veje agenta nazaj.
        case ("PUT", "/minigw/worktree/bundle"):
            let st = await Self.shares.worktreePut(key: req.header("x-share-key") ?? "", part: Int(req.header("x-part") ?? "") ?? -1,
                                                   data: req.body, device: req.device?.id)
            try? await writer.write(HTTPResponse.head(status: st, headers: [], contentLength: 0))
            return (st, nil, "share")
        case ("GET", "/minigw/worktree/result"):
            guard let body = await Self.shares.worktreeResult(key: req.header("x-share-key") ?? "", device: req.device?.id) else {
                return (await error(404, "not_found_error", "No result", rid: rid, writer: writer), nil, "share")
            }
            try? await writer.write(HTTPResponse.head(status: 200, headers: [("Content-Type", "application/octet-stream")], contentLength: body.count) + body)
            return (200, nil, "share")
        case ("POST", "/minigw/native"):
            return (await nativeChallenge(req, rid: rid, writer: writer), nil, "share")
        case ("DELETE", "/minigw/native"):
            let st = await nativeDiscard(req)
            try? await writer.write(HTTPResponse.head(status: st, headers: [], contentLength: 0))
            return (st, nil, "share")
        case ("POST", "/minigw/run"):
            return (await runCommand(req, rid: rid, writer: writer), nil, "run")
        case ("DELETE", "/minigw/share"):
            await Self.shares.unregister(key: req.header("x-share-key") ?? "")
            try? await writer.write(HTTPResponse.head(status: 204, headers: [], contentLength: 0))
            return (204, nil, "share")

        default:
            return (await error(404, "not_found_error", "Not found: \(req.method) \(req.path)", rid: rid, writer: writer), nil, nil)
        }
    }

    /// sync: izvajanje agenta v svojem klonu kopije (ShareHub.run); zaklep kopije velja le za prenos, prevzem in združitev,
    /// zato agenti v isti mapi delajo vzporedno. Klon se odstrani tudi ob napaki ali prekinitvi.
    /// #4: agent v sinhronizirani mapi s pretokom: glava 200 in komentarji SSE takoj, prenos mape lahko traja minute
    /// (Claude Code bi po 10 minutah brez bajtov poskusil znova). Drugače isti writer.
    func early(_ req: HTTPRequest, _ body: [String: Any], _ rid: String, _ writer: ResponseWriter) async -> ResponseWriter {
        guard body["stream"] as? Bool == true, (body["tools"] as? [Any])?.isEmpty == false,
              let sid = req.header("x-claude-mini-session"), await Self.shares.isSync(session: sid) else { return writer }
        let w = EarlyStreamWriter(writer)
        await w.begin(rid: rid)
        return w
    }

    func syncRun(agent: String, _ run: () async -> Int) async -> Int {
        let id = UUID().uuidString
        let status = await ShareHub.$run.withValue(id) { await ShareHub.$agent.withValue(agent) { await run() } }
        await Self.shares.finish(run: id)
        return status
    }

    func authorized(_ req: HTTPRequest) -> Bool {
        let bearer = req.header("authorization").flatMap { $0.hasPrefix("Bearer ") ? String($0.dropFirst(7)) : nil }
        guard let given = bearer ?? req.header("x-api-key"), !token.isEmpty else { return false }
        // Primerjava v konstantnem času.
        let a = Array(given.utf8), b = Array(token.utf8)
        return a.count == b.count && zip(a, b).reduce(0) { $0 | ($1.0 ^ $1.1) } == 0
    }

    func modelList(siri siriAllowed: Bool = true) async -> [String: Any] {
        let created = ISO8601DateFormatter().string(from: Date(timeIntervalSince1970: 1_767_225_600))
        var seen: Set<String> = []
        var data: [[String: Any]] = await catalog.working().compactMap { r in
            guard seen.insert(r.alias).inserted else { return nil }  // prvi ponudnik z imenom zmaga
            let window = r.window.map { " · \($0 / 1000)K" } ?? ""
            return ["type": "model", "id": r.alias, "display_name": "\(r.name) · \(r.provider.name)",
                    "description": "\(r.provider.name) · \(r.upstream)\(window)", "created_at": created]
        }
        if config.siri.enabled, siriAllowed, SiriAX.trusted(prompt: false), FileManager.default.fileExists(atPath: SiriAX.appPath) {
            data.append(["type": "model", "id": "claude-siri-ai", "display_name": "Siri AI",
                         "description": "Siri AI app · no tools · uses this Mac's personal data", "created_at": created])
        }
        if config.appleFM.enabled, case .available = SystemLanguageModel.default.availability {
            data.append(["type": "model", "id": "claude-apple-fm", "display_name": "Apple Foundation Models",
                         "description": "On-device · \(SystemLanguageModel.default.contextSize / 1000)K context · no tools", "created_at": created])
        }
        if config.claudeCLI.enabled, await probes.ok("claude-cli") {  // vgrajeni opus/sonnet/haiku so že v izbirniku Claude Code
            data += ["opus", "sonnet", "haiku"].map { m in
                ["type": "model", "id": ClaudeCLI.agentPrefix + m, "display_name": "\(m.capitalized) · Claude CLI",
                 "description": "claude -p · \(m) · tools in your folder", "created_at": created]
            }
        }
        if config.codex.enabled, let exe = await Codex.executable(config.codex.executable) {
            data.append(["type": "model", "id": "claude-codex", "display_name": "Codex · Codex CLI",
                         "description": "codex exec · default model · tools in your folder", "created_at": created])
            data += Codex.catalog(exe).filter { config.codex.isModelEnabled($0.slug) }.map { m in
                ["type": "model", "id": Codex.prefix + m.slug, "display_name": "\(m.name) · Codex CLI",
                 "description": "codex exec · \(m.slug)\(m.window.map { " · \($0 / 1000)K" } ?? "") · tools in your folder",
                 "created_at": created]
            }
        }
        if config.copilot.enabled, await Copilot.executable(config.copilot.executable) != nil {
            data.append(["type": "model", "id": "claude-copilot", "display_name": "Copilot · Copilot CLI",
                         "description": "copilot -p · default model (auto) · tools in your folder", "created_at": created])
            data += config.copilot.modelList.map { m in
                ["type": "model", "id": Copilot.prefix + m, "display_name": "\(m) · Copilot CLI",
                 "description": "copilot -p · \(m) · tools in your folder", "created_at": created]
            }
        }
        if config.grok.enabled, let exe = await Grok.executable(config.grok.executable) {
            data.append(["type": "model", "id": "claude-grok", "display_name": "Grok · Grok CLI",
                         "description": "grok -p · default model · tools in your folder", "created_at": created])
            data += Grok.catalog(exe).filter { config.grok.isModelEnabled($0) }.map { m in
                ["type": "model", "id": Grok.prefix + m, "display_name": "\(m) · Grok CLI",
                 "description": "grok -p · \(m) · tools in your folder", "created_at": created]
            }
        }
        data += await foundryModelList(created: created)
        return ["data": data, "has_more": false,
                "first_id": data.first?["id"] ?? NSNull(), "last_id": data.last?["id"] ?? NSNull()]
    }

    /// Zamenja ime modela in overitev, telo in SSE tok prepošlje nespremenjena.
    func passthrough(_ req: HTTPRequest, body: [String: Any], route r: Route, key: String,
                     rid: String, writer: ResponseWriter) async -> Int {
        var body = body
        body["model"] = r.upstream
        let base = r.provider.baseURL.hasSuffix("/") ? String(r.provider.baseURL.dropLast()) : r.provider.baseURL
        guard let url = URL(string: base + req.path + (req.query.map { "?" + $0 } ?? "")),
              let data = try? JSONSerialization.data(withJSONObject: body) else {
            return await error(500, "api_error", "Bad upstream URL for \(r.provider.name)", rid: rid, writer: writer)
        }
        var up = URLRequest(url: url)
        up.httpMethod = "POST"
        up.httpBody = data
        up.setValue("application/json", forHTTPHeaderField: "content-type")
        up.setValue("Bearer \(key)", forHTTPHeaderField: "authorization")
        for h in ["anthropic-version", "anthropic-beta"] {
            if let v = req.header(h) { up.setValue(v, forHTTPHeaderField: h) }
        }

        var headSent = false
        var status = 502
        do {
            let (bytes, resp) = try await session.bytes(for: up)
            guard let http = resp as? HTTPURLResponse else { throw URLError(.badServerResponse) }
            status = http.statusCode
            try await writer.write(HTTPResponse.head(status: status, headers: [
                ("Content-Type", http.value(forHTTPHeaderField: "content-type") ?? "application/json"),
                ("Cache-Control", "no-cache"),
                ("request-id", rid),
            ]))
            headSent = true
            var chunk = Data()
            for try await byte in bytes {
                chunk.append(byte)
                if byte == 0x0A || chunk.count >= 16 * 1024 {  // SSE dogodki gredo naprej sproti
                    try await writer.write(chunk)
                    chunk.removeAll(keepingCapacity: true)
                }
            }
            if !chunk.isEmpty { try await writer.write(chunk) }
            return status
        } catch {
            if headSent { return status }  // odjemalec ali upstream je prekinil med tokom
            return await self.error(502, "api_error", "\(r.provider.name) unreachable: \(error.localizedDescription)", rid: rid, writer: writer)
        }
    }

    func json(_ status: Int, _ obj: Any, rid: String, writer: ResponseWriter) async -> Int {
        let body = (try? JSONSerialization.data(withJSONObject: obj, options: [.sortedKeys, .withoutEscapingSlashes])) ?? Data("{}".utf8)
        try? await writer.write(HTTPResponse.head(status: status, headers: [
            ("Content-Type", "application/json"), ("request-id", rid),
        ], contentLength: body.count) + body)
        return status
    }

    func error(_ status: Int, _ type: String, _ message: String, rid: String, writer: ResponseWriter) async -> Int {
        await json(status, ["type": "error", "error": ["type": type, "message": message]], rid: rid, writer: writer)
    }

    static func errorType(_ status: Int) -> String {
        switch status {
        case 401: "authentication_error"
        case 403: "permission_error"
        case 404: "not_found_error"
        case 413: "request_too_large"
        case 429: "rate_limit_error"
        case 400..<500: "invalid_request_error"
        default: "api_error"
        }
    }
}

/// Rezultati preverjanj ponudnikov. Uspeh se zapomni; neuspeh se ob naslednjem klicu preveri znova.
actor Probes {
    private var passed: Set<String> = []
    private var running: [String: Task<Bool, Never>] = [:]
    private var work: [String: @Sendable () async -> Bool] = [:]

    func start(_ id: String, _ check: @escaping @Sendable () async -> Bool) {
        work[id] = check
        running[id] = Task { await check() }
    }

    /// Počaka največ `wait` (discovery v Claude Code ima 3 s), sicer šteje kot »še ne«.
    func ok(_ id: String, wait: Duration = .seconds(2)) async -> Bool {
        if passed.contains(id) { return true }
        guard let task = running[id] else { return false }
        let result = await firstOf(task, within: wait)
        if result == true {
            passed.insert(id)
            return true
        }
        if result == false, let check = work[id] { running[id] = Task { await check() } }  // poskusi znova naslednjič
        return false
    }
}

/// Modeli zunanjih ponudnikov. Ob vklopu jih odkrije pri upstreamu (`/v1/models`, okno iz `context_length`
/// ali LiteLLM `/model/info`); polje Models v nastavitvah izbor le zoži in poimenuje.
/// Seznam se osveži, ko je starejši od 5 minut; neuspeh se poskusi znova ob naslednjem klicu.
actor Catalog {
    struct Entry: Sendable {
        let routes: [Route]
        let ok: Bool  // upstream je odgovoril → modeli so preverjeni
        let at: Date
    }

    private let providers: [ProviderConfig]
    private let secrets: SecretStore
    private let session: URLSession
    private var entries: [String: Entry] = [:]
    private var loading: [String: Task<Entry, Never>] = [:]
    static let maxAge: TimeInterval = 300

    init(providers: [ProviderConfig], secrets: SecretStore, session: URLSession) {
        self.providers = providers
        self.secrets = secrets
        self.session = session
    }

    func refreshAll() { for p in providers { _ = load(p) } }

    /// Najprej ponudnik v vrstnem redu nastavitev; ime ali objavljeni ID.
    func route(_ model: String) async -> Route? {
        for p in providers {
            if let r = await entry(p)?.routes.first(where: { $0.name == model || $0.alias == model }) { return r }
        }
        return nil
    }

    func working() async -> [Route] {
        var out: [Route] = []
        for p in providers { if let e = await entry(p), e.ok { out += e.routes.sorted { $0.alias < $1.alias } } }
        return out
    }

    func status() async -> [String: Bool] {
        var out: [String: Bool] = [:]
        for p in providers { out[p.id] = await entry(p)?.ok ?? false }
        return out
    }

    /// Čaka največ 2 s (discovery v Claude Code ima 3 s); medtem velja zadnji znani seznam.
    private func entry(_ p: ProviderConfig) async -> Entry? {
        if let e = entries[p.id], e.ok, Date().timeIntervalSince(e.at) < Self.maxAge { return e }
        let fresh = await firstOf(load(p), within: .seconds(2))
        if let fresh {
            loading[p.id] = nil
            if fresh.ok || entries[p.id] == nil { entries[p.id] = fresh }  // ob napaki obdrži zadnji delujoči seznam
            return entries[p.id]
        }
        return entries[p.id]
    }

    private func load(_ p: ProviderConfig) -> Task<Entry, Never> {
        if let t = loading[p.id] { return t }
        let (key, session) = (secrets.get(p.id) ?? "", self.session)
        let t = Task { await Self.discover(p, key: key, session: session) }
        loading[p.id] = t
        return t
    }

    static func discover(_ p: ProviderConfig, key: String, session: URLSession) async -> Entry {
        let result = await Discovery.load(p, key: key, session: session)
        if case .failure(let f) = result { Router.log.error("discovery \(p.id, privacy: .public): \(f.message, privacy: .public)") }
        let found = try? result.get()
        let windows = Dictionary((found ?? []).map { ($0.id, $0.window) }, uniquingKeysWith: { a, _ in a }).compactMapValues { $0 }
        let manual = p.aliasPairs
        let routes: [Route] = manual.isEmpty
            ? (found ?? []).filter { p.isModelEnabled($0.id) }
                .map { Route(provider: p, name: $0.id, alias: ProviderConfig.publishedID($0.id), upstream: $0.id, window: $0.window) }
            : manual.map { Route(provider: p, name: $0.alias, alias: ProviderConfig.publishedID($0.alias),
                                 upstream: $0.upstream, window: $0.window ?? windows[$0.upstream]) }
        return Entry(routes: routes, ok: found != nil, at: Date())
    }
}

/// Modeli, ki jih ponudnik ponuja (za usmerjanje in za tabelo v nastavitvah).
public enum Discovery {
    public struct Model: Identifiable, Sendable, Equatable {
        public let id: String
        public let window: Int?
    }

    public struct Failure: Error, Sendable {
        public let message: String
    }

    /// `nil`, če upstream ne odgovori ali ključ ne velja.
    public static func models(_ p: ProviderConfig, key: String, session: URLSession = Router.defaultSession) async -> [Model]? {
        try? await load(p, key: key, session: session).get()
    }

    /// Modeli ali razlog, zakaj jih ni (za nastavitve in dnevnik).
    public static func load(_ p: ProviderConfig, key: String, session: URLSession = Router.defaultSession) async -> Result<[Model], Failure> {
        guard !key.isEmpty else { return .failure(Failure(message: String(localized: "No API key"))) }
        let list: [String: Any]
        do { list = try await fetch(p, "/v1/models", key: key, session: session) } catch { return .failure(error) }
        guard let items = list["data"] as? [[String: Any]] else {
            return .failure(Failure(message: String(localized: "Unexpected response from /v1/models")))
        }
        func window(_ item: [String: Any]) -> Int? { (item["context_length"] ?? item["max_input_tokens"] ?? item["context_window"]) as? Int }
        var windows: [String: Int] = [:]
        if items.contains(where: { window($0) == nil }),  // okna manjkajo → LiteLLM jih ima v /model/info
           let info = (try? await fetch(p, "/model/info", key: key, session: session))?["data"] as? [[String: Any]] {
            for m in info {
                if let name = m["model_name"] as? String,
                   let w = (m["model_info"] as? [String: Any])?["max_input_tokens"] as? Int { windows[name] = w }
            }
        }
        return .success(items.compactMap { item -> Model? in
            guard let id = item["id"] as? String, !id.contains("*") else { return nil }  // LiteLLM wildcard ni model
            return Model(id: id, window: window(item) ?? windows[id])
        })
    }

    static func fetch(_ p: ProviderConfig, _ path: String, key: String, session: URLSession) async throws(Failure) -> [String: Any] {
        let base = p.baseURL.hasSuffix("/") ? String(p.baseURL.dropLast()) : p.baseURL
        guard let url = URL(string: base + path), url.host != nil else { throw Failure(message: String(localized: "Invalid base URL")) }
        var r = URLRequest(url: url, timeoutInterval: 5)
        r.setValue("Bearer \(key)", forHTTPHeaderField: "authorization")
        let data: Data, resp: URLResponse
        do {
            (data, resp) = try await session.data(for: r)
        } catch {
            throw Failure(message: isLocal(url.host ?? "")
                ? String(localized: "Cannot connect (\(error.localizedDescription)). Check that this Mac can reach the server (VPN, firewall). For a device on the same subnet, also allow Mini Gateway in System Settings → Privacy & Security → Local Network.")
                : String(localized: "Cannot connect (\(error.localizedDescription))"))
        }
        let status = (resp as? HTTPURLResponse)?.statusCode ?? 0
        guard status == 200 else {
            throw Failure(message: [401, 403].contains(status)
                ? String(localized: "HTTP \(String(status)) – check the API key")
                : String(localized: "HTTP \(String(status)) from \(path)"))
        }
        guard let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            throw Failure(message: String(localized: "Unexpected response from \(path)"))
        }
        return obj
    }

    /// Zasebni IPv4 ali .local: za te naslove macOS zahteva dovoljenje Local Network.
    static func isLocal(_ host: String) -> Bool {
        if host.hasSuffix(".local") || host == "localhost" { return true }
        let o = host.split(separator: ".").compactMap { Int($0) }
        guard o.count == 4 else { return false }
        return o[0] == 10 || o[0] == 127 || (o[0] == 192 && o[1] == 168) || (o[0] == 172 && (16...31).contains(o[1])) || (o[0] == 169 && o[1] == 254)
    }
}

/// Blokirajoče delo (procesi, git, drevesa datotek) na svoji niti. Ne Task.detached: ta teče v skupnem bazenu Swift
/// concurrency (ena nit na jedro); blokirane niti ga izčrpajo in zahteve routerja ter časovniki (firstOf) čakajo.
func offPool<T: Sendable>(_ work: @escaping @Sendable () -> T) async -> T {
    await withCheckedContinuation { c in Thread { c.resume(returning: work()) }.start() }
}

/// offPool za delo, ki lahko vrže napako (agenti: proces teče vso sejo).
func offPool<T: Sendable>(_ work: @escaping @Sendable () throws -> T) async throws -> T {
    try await withCheckedThrowingContinuation { c in Thread { c.resume(with: Result { try work() }) }.start() }
}

/// Rezultat opravila ali `nil` po `within`; ne čaka na opravilo, ki teče dlje (withTaskGroup bi čakal).
func firstOf<T: Sendable>(_ task: Task<T, Never>, within: Duration) async -> T? {
    let once = Once()
    return await withCheckedContinuation { (c: CheckedContinuation<T?, Never>) in
        Task { let v = await task.value; if once.claim() { c.resume(returning: v) } }
        Task { try? await Task.sleep(for: within); if once.claim() { c.resume(returning: nil) } }
    }
}

final class Once: @unchecked Sendable {
    private let lock = NSLock()
    private var done = false
    func claim() -> Bool { lock.withLock { defer { done = true }; return !done } }
    var claimed: Bool { lock.withLock { done } }
}
