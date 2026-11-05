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
    public var onState: (@Sendable (ServerState) -> Void)?

    public init(router: Router) { self.router = router }

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
        conn.start(queue: queue)
        receive(conn, Data(), peer: peer)
    }

    static func ip(_ host: NWEndpoint.Host) -> String {
        switch host {
        case .ipv4(let a): "\(a)"
        case .ipv6(let a): "\(a)"
        case .name(let n, _): n
        @unknown default: "\(host)"
        }
    }

    static func isLoopback(_ ip: String) -> Bool {
        ip == "::1" || ip.hasPrefix("127.") || ip.hasPrefix("::ffff:127.")
    }

    private func receive(_ conn: NWConnection, _ buffer: Data, peer: String) {
        conn.receive(minimumIncompleteLength: 1, maximumLength: 64 * 1024) { [weak self] data, _, done, error in
            guard let self else { return }
            var buf = buffer
            if let data { buf.append(data) }
            switch HTTPParser.parse(buf) {
            case .needMore:
                if done || error != nil { conn.cancel() } else { self.receive(conn, buf, peer: peer) }
            case .error(let status, let message):
                let w = ConnectionWriter(conn: conn)
                Task { await self.router.reject(status: status, message: message, writer: w) }
            case .request(var req):
                req.peer = peer
                let allowed = self.router.config.allowedClientList
                if Self.isLoopback(peer), !allowed.isEmpty, !allowed.contains(peer), !req.path.hasPrefix("/dav/") {
                    conn.cancel()
                    return
                }
                let w = ConnectionWriter(conn: conn)
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
}

struct ConnectionWriter: ResponseWriter {
    let conn: NWConnection

    func write(_ data: Data) async throws {
        try await withCheckedThrowingContinuation { (k: CheckedContinuation<Void, Error>) in
            conn.send(content: data, completion: .contentProcessed { e in
                if let e { k.resume(throwing: e) } else { k.resume() }
            })
        }
    }

    func close() {
        conn.send(content: nil, contentContext: .finalMessage, isComplete: true,
                  completion: .contentProcessed { [conn] _ in conn.cancel() })
    }
}
