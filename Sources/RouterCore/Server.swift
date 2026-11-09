import CryptoKit
import Foundation
import Network

public enum ServerError: Error { case badPort }

/// Stanje poslušanja; besedilo za uporabnika sestavi (in lokalizira) aplikacija.
public enum ServerState: Sendable {
    case listening(host: String, port: Int)
    case failed(String)
    case stopped
}

/// TCP strežnik na Network.framework; ena zahteva na povezavo.
public final class RouterServer: @unchecked Sendable {
    public let router: Router
    private let queue = DispatchQueue(label: "dev.trojar.minigateway.server")
    private var listener: NWListener?
    private var pending = Set<ObjectIdentifier>()  // povezave brez cele zahteve (le na `queue`)
    public var onState: (@Sendable (ServerState) -> Void)?

    let key: Curve25519.KeyAgreement.PrivateKey?  // stalni ključ routerja (Noise); nil = brez šifriranih povezav

    public init(router: Router, key: Curve25519.KeyAgreement.PrivateKey? = nil) { self.router = router; self.key = key }

    public func start() throws {
        let c = router.config
        guard let port = NWEndpoint.Port(rawValue: c.port) else { throw ServerError.badPort }
        let params = NWParameters.tcp
        params.allowLocalEndpointReuse = true
        params.requiredLocalEndpoint = .hostPort(host: NWEndpoint.Host(c.host), port: port)
        let l = try NWListener(using: params)
        l.stateUpdateHandler = { [weak self] state in
            Router.log.notice("listener \(c.host, privacy: .public):\(c.port) state=\(String(describing: state), privacy: .public)")
            switch state {
            case .ready: self?.onState?(.listening(host: c.host, port: Int(c.port)))
            case .failed(let e): self?.onState?(.failed("\(e)"))
            case .cancelled: self?.onState?(.stopped)
            default: break
            }
        }
        l.newConnectionHandler = { [weak self] conn in self?.accept(conn) }
        l.start(queue: queue)
        listener = l
    }

    public func stop() {
        listener?.cancel()
        listener = nil
    }

    private func accept(_ conn: NWConnection) {
        let allowed = router.config.allowedClientList
        guard case let .hostPort(host, _) = conn.endpoint else { conn.cancel(); return }
        let peer = Self.ip(host)
        // Seznam dovoljenih velja za vse razen lokalnega mount_webdav (/dav, preverjeno po branju zahteve).
        guard allowed.isEmpty || allowed.contains(peer) || Self.isLoopback(peer) else { conn.cancel(); return }
        guard pending.count < Self.maxPending else { conn.cancel(); return }  // L1: kot minigw serve (256 v rokovanju)
        conn.start(queue: queue)
        let id = ObjectIdentifier(conn)
        pending.insert(id)
        // Brez cele zahteve v 120 s (zastalo rokovanje, počasen napadalec) → zapri. Velik PUT prek VPN ima dovolj časa.
        queue.asyncAfter(deadline: .now() + 120) { [weak self] in
            if self?.pending.remove(id) != nil { conn.cancel() }
        }
        sniff(conn, Data(), peer: peer)
    }

    static func ip(_ host: NWEndpoint.Host) -> String {
        switch host {
        case .ipv4(let a): "\(a)"
        case .ipv6(let a): "\(a)"
        case .name(let n, _): n
        @unknown default: "\(host)"
        }
    }

    static let maxPending = 256

    /// L1: telo nad 1 MB z drugega računalnika le z veljavnim žetonom v glavi (sicer bi do 64 MB brali pred preverbo).
    static func bodyNeedsToken(_ plain: Data, peer: String, authorized: (HTTPRequest) -> Bool) -> Bool {
        guard !isLoopback(peer), case .request(let h) = HTTPParser.parse(plain, headOnly: true),
              (Int(h.header("content-length") ?? "") ?? 0) > 1 << 20 else { return false }
        return !authorized(h)
    }

    static func isLoopback(_ ip: String) -> Bool {
        ip == "::1" || ip.hasPrefix("127.") || ip.hasPrefix("::ffff:127.")
    }

    /// Prvi bajti: »MGW1« = šifrirano (Noise), sicer navaden HTTP (z drugih Macov le, če je dovoljen).
    private func sniff(_ conn: NWConnection, _ buf: Data, peer: String) {
        conn.receive(minimumIncompleteLength: 1, maximumLength: 64 * 1024) { [weak self] data, _, done, error in
            guard let self else { return }
            var buf = buf
            if let data { buf.append(data) }
            let end = done || error != nil
            if buf.count < 4, Data("MGW".utf8).starts(with: buf.prefix(3)), !end {
                return self.sniff(conn, buf, peer: peer)
            }
            if buf.count >= 4, let suite = Noise.Suite.from(magic: Data(buf.prefix(4))) {
                return self.handshake(conn, Data(buf.dropFirst(4)), peer: peer, suite: suite)
            }
            guard Self.isLoopback(peer) || self.router.config.allowPlainRemote else {
                let body = Data(#"{"type":"error","error":{"type":"permission_error","message":"Unencrypted connections from other Macs are off: update minigw to 0.4 or newer"}}"#.utf8)
                let w = ConnectionWriter(conn: conn, cipher: nil)
                Task {
                    try? await w.write(HTTPResponse.head(status: 426, headers: [("Content-Type", "application/json")], contentLength: body.count) + body)
                    w.close()
                }
                return
            }
            self.feed(conn, raw: buf, plain: Data(), peer: peer, session: nil, device: .done(nil), end: end)
        }
    }

    /// Naprava šifrirane povezave: najprej neprebrana, nato znana (ali nil pri starejšem minigw / navadnem HTTP).
    enum DeviceState { case unchecked, done(Noise.Device?) }

    private func handshake(_ conn: NWConnection, _ buf: Data, peer: String, suite: Noise.Suite) {
        guard buf.count >= Noise.handshakeLength else {
            conn.receive(minimumIncompleteLength: 1, maximumLength: 64 * 1024) { [weak self] data, _, done, error in
                guard let self, let data, !data.isEmpty else { conn.cancel(); return }
                if (done || error != nil) && buf.count + data.count < Noise.handshakeLength { conn.cancel(); return }
                self.handshake(conn, buf + data, peer: peer, suite: suite)
            }
            return
        }
        guard let key, let (reply, session) = try? Noise.respond(Data(buf.prefix(Noise.handshakeLength)), key: key, suite: suite) else {
            conn.cancel()  // napačen ključ routerja ali poškodovano sporočilo
            return
        }
        conn.send(content: reply, completion: .contentProcessed { _ in })
        feed(conn, raw: Data(buf.dropFirst(Noise.handshakeLength)), plain: Data(), peer: peer, session: session, device: .unchecked, end: false)
    }

    private func receive(_ conn: NWConnection, raw: Data, plain: Data, peer: String, session: Noise.Session?, device: DeviceState) {
        conn.receive(minimumIncompleteLength: 1, maximumLength: 64 * 1024) { [weak self] data, _, done, error in
            guard let self else { return }
            var raw = raw
            if let data { raw.append(data) }
            self.feed(conn, raw: raw, plain: plain, peer: peer, session: session, device: device, end: done || error != nil)
        }
    }

    private func feed(_ conn: NWConnection, raw: Data, plain: Data, peer: String, session: Noise.Session?, device: DeviceState, end: Bool) {
        var raw = raw, plain = plain, device = device
        if let session {
            guard let p = try? Noise.unframe(&raw, session.receive) else { conn.cancel(); return }
            plain.append(p)
            if case .unchecked = device {
                switch Noise.readDevice(&plain, hash: session.hash) {
                case .needMore:
                    if end { conn.cancel() } else { receive(conn, raw: raw, plain: plain, peer: peer, session: session, device: device) }
                    return
                case .invalid: conn.cancel(); return  // ponarejen ali tuj podpis
                case .absent: device = .done(nil)
                case .device(let d): device = .done(d)
                }
            }
        } else {
            plain.append(raw); raw = Data()
        }
        let w = ConnectionWriter(conn: conn, cipher: session?.send)
        var parsed = HTTPParser.parse(plain)
        if case .needMore = parsed, Self.bodyNeedsToken(plain, peer: peer, authorized: router.authorized) {
            parsed = .error(401, "Invalid or missing token")
        }
        if case .needMore = parsed {} else { pending.remove(ObjectIdentifier(conn)) }
        switch parsed {
        case .needMore:
            if end { conn.cancel() } else { receive(conn, raw: raw, plain: plain, peer: peer, session: session, device: device) }
        case .error(let status, let message):
            Task { await self.router.reject(status: status, message: message, writer: w) }
        case .request(var req):
            req.peer = peer
            req.noiseHash = session?.hash
            if case let .hostPort(_, p) = conn.endpoint { req.peerPort = p.rawValue }
            if case .done(let d) = device { req.device = d }
            let allowed = router.config.allowedClientList
            if Self.isLoopback(peer), !allowed.isEmpty, !allowed.contains(peer), !req.path.hasPrefix("/dav/") {
                conn.cancel()
                return
            }
            let task = Task { await self.router.handle(req, writer: w) }
            conn.stateUpdateHandler = { state in
                switch state {
                case .failed, .cancelled: task.cancel()  // odjemalec je odšel → prekini upstream
                default: break
                }
            }
        }
    }
}

struct ConnectionWriter: ResponseWriter {
    let conn: NWConnection
    let cipher: Noise.Cipher?  // šifrirana povezava: vsak zapis v okvirjih

    func write(_ data: Data) async throws {
        guard !data.isEmpty else { return }
        try await withCheckedThrowingContinuation { (k: CheckedContinuation<Void, Error>) in
            let done: NWConnection.SendCompletion = .contentProcessed { e in
                if let e { k.resume(throwing: e) } else { k.resume() }
            }
            if let c = cipher {
                c.sendLock.withLock { conn.send(content: Noise.frames(data, c), completion: done) }  // send ohrani vrstni red klicev
            } else {
                conn.send(content: data, completion: done)
            }
        }
    }

    func close() {
        conn.send(content: nil, contentContext: .finalMessage, isComplete: true,
                  completion: .contentProcessed { [conn] _ in conn.cancel() })
    }
}
