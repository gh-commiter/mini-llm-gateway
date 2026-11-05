import Foundation
import Network

/// Lokalni proxy v minigw: Claude Code in minigw govorita navaden HTTP na 127.0.0.1, proxy vsako povezavo
/// prenese šifrirano (Noise) do routerja na drugem Macu. Ena lokalna povezava = ena šifrirana povezava.
public final class NoiseProxy: @unchecked Sendable {
    private let host: NWEndpoint.Host, port: NWEndpoint.Port, routerKey: Data
    private let queue = DispatchQueue(label: "dev.trojar.minigateway.proxy")
    private var listener: NWListener?
    public private(set) var localPort: UInt16 = 0
    public var localURL: String { "http://127.0.0.1:\(localPort)" }

    public init?(routerURL: String, routerKey: Data) {
        guard let u = URLComponents(string: routerURL), let h = u.host, let p = NWEndpoint.Port(rawValue: UInt16(u.port ?? 8765)),
              routerKey.count == 32 else { return nil }
        host = NWEndpoint.Host(h); port = p; self.routerKey = routerKey
    }

    /// Začne poslušati na naključnem lokalnem portu; vrne, ko je pripravljen.
    public func start() throws {
        let params = NWParameters.tcp
        params.requiredLocalEndpoint = .hostPort(host: "127.0.0.1", port: .any)
        let l = try NWListener(using: params)
        let ready = DispatchSemaphore(value: 0)
        l.stateUpdateHandler = { state in
            switch state {
            case .ready, .failed, .cancelled: ready.signal()
            default: break
            }
        }
        l.newConnectionHandler = { [weak self] c in self?.accept(c) }
        l.start(queue: queue)
        ready.wait()
        guard let p = l.port?.rawValue, p != 0 else { throw NWError.posix(.EADDRNOTAVAIL) }
        localPort = p
        listener = l
    }

    public func stop() { listener?.cancel() }

    private func accept(_ local: NWConnection) {
        let remote = NWConnection(host: host, port: port, using: .tcp)
        func fail() { local.cancel(); remote.cancel() }
        var hs = Noise.Initiator(routerKey: routerKey)
        remote.stateUpdateHandler = { [weak self] state in
            switch state {
            case .ready:
                guard let first = try? hs.first() else { return fail() }
                remote.send(content: Noise.magic + first, completion: .contentProcessed { _ in })
                self?.readHandshake(local, remote, Data()) { reply in
                    guard let reply, let session = try? hs.finish(reply) else { return fail() }
                    local.stateUpdateHandler = { s in
                        if case .cancelled = s { remote.cancel() }  // Claude Code je prekinil → prekini tudi pri routerju
                        if case .failed = s { remote.cancel() }
                    }
                    local.start(queue: self!.queue)
                    self?.pumpUp(local, remote, session.send)
                    self?.pumpDown(remote, local, session.receive, Data())
                }
            case .failed, .cancelled:
                local.cancel()
            default: break
            }
        }
        remote.start(queue: queue)
    }

    private func readHandshake(_ local: NWConnection, _ remote: NWConnection, _ buf: Data, _ done: @escaping (Data?) -> Void) {
        remote.receive(minimumIncompleteLength: 1, maximumLength: Noise.handshakeLength - buf.count) { data, _, end, error in
            let b = buf + (data ?? Data())
            if b.count == Noise.handshakeLength { return done(b) }
            if end || error != nil { return done(nil) }  // router ni sprejel (napačen ključ?)
            self.readHandshake(local, remote, b, done)
        }
    }

    /// Lokalno (navaden HTTP) → okvirji → router.
    private func pumpUp(_ local: NWConnection, _ remote: NWConnection, _ c: Noise.Cipher) {
        local.receive(minimumIncompleteLength: 1, maximumLength: Noise.maxPlain) { data, _, end, error in
            if let data, !data.isEmpty { remote.send(content: Noise.frames(data, c), completion: .contentProcessed { _ in }) }
            if end || error != nil { return }  // odgovor še teče; povezavo zapre router
            self.pumpUp(local, remote, c)
        }
    }

    /// Router → dešifrirano → lokalno; ob koncu zapre lokalno povezavo.
    private func pumpDown(_ remote: NWConnection, _ local: NWConnection, _ c: Noise.Cipher, _ buf: Data) {
        remote.receive(minimumIncompleteLength: 1, maximumLength: 256 * 1024) { data, _, end, error in
            var buf = buf + (data ?? Data())
            guard let plain = try? Noise.unframe(&buf, c) else { local.cancel(); remote.cancel(); return }
            if !plain.isEmpty { local.send(content: plain, completion: .contentProcessed { _ in }) }
            if end || error != nil {
                local.send(content: nil, contentContext: .finalMessage, isComplete: true,
                           completion: .contentProcessed { _ in local.cancel() })
                remote.cancel()
                return
            }
            self.pumpDown(remote, local, c, buf)
        }
    }
}
