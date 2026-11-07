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
