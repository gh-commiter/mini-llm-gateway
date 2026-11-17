import CryptoKit
import Foundation
import Network
import os

// This gateway as a CLIENT of another gateway (the KB upstream), on the Mac. The core (Go, in the helper minigw-core) decides what is
// sent; it has no network and no credential store, so it asks this app for them through callouts (client/coreapi/callout.go,
// CoreHelper.exchange). Everything secret stays here: the upstream's token line and this gateway's own device key for it are one
// Keychain item per upstream, "upstream:<name>" in the service of the provider keys. They are never given to the helper (except the
// token, once, when the owner adds the upstream, and the helper does not keep it), never logged, never in an answer or a file.
// The connection is the client's own encrypted protocol (Noise, the upstream's key pinned) through a proxy that lives for ONE request on
// a port nobody can guess. Same duties as client/upstream_host.go.

/// The upstream's answer as the core gets it. `status` 0 = no usable answer.
public struct UpstreamAnswer: Sendable, Equatable {
    public var status: Int
    public var body: Data
    public var retryAfter: Int
    public init(status: Int, body: Data = Data(), retryAfter: Int = 0) { self.status = status; self.body = body; self.retryAfter = retryAfter }
}

/// One request to an upstream: a request of a CLOSED set (UpstreamRequest.check), nothing else. Blocks the calling thread until it ends: never
/// call it from the cooperative pool (CoreHelper calls it on its own queue). Tests give a fake.
public protocol UpstreamTransport: Sendable {
    func send(address: String, routerKey: Data, device: (key: Curve25519.Signing.PrivateKey, name: String), token: String,
              method: String, path: String, idempotencyKey: String?, body: Data?) -> UpstreamAnswer
}

/// What the helper may ask the app to send. The helper is the untrusted party here (it exists to keep a core that a hostile note or bundle
/// has taken over away from the network and the credentials), so the app accepts only the requests the push makes: GET or POST of the
/// member API of the upstream, a plain path, one header (the idempotency key) and a body of bounded size. The URL is never built from
/// the helper's text: the request line is written by hand, to the address configured at `add`.
public enum UpstreamRequest {
    public static let maxBody = 1 << 20
    public static let maxPath = 512

    /// The checked idempotency key ("" = none), or nil when the request is refused.
    public static func check(method: String, path: String, headers: [String: String], body: Data?) -> String? {
        guard method == "GET" || method == "POST", (body?.count ?? 0) <= maxBody, path.utf8.count <= maxPath, path.hasPrefix("/minigw/v1/") else { return nil }
        let parts = path.split(separator: "?", maxSplits: 1, omittingEmptySubsequences: false).map(String.init)
        guard parts.count <= 2 else { return nil }
        let pathChars = CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/_.-")
        guard parts[0].unicodeScalars.allSatisfy({ pathChars.contains($0) }) else { return nil }  // no @ # % \ space or control character
        for seg in parts[0].dropFirst().split(separator: "/", omittingEmptySubsequences: false) where seg.isEmpty || seg == "." || seg == ".." { return nil }
        if parts.count == 2 {
            let queryChars = CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.=&-")
            guard parts[1].utf8.count <= 128, parts[1].unicodeScalars.allSatisfy({ queryChars.contains($0) }) else { return nil }
        }
        let key = headers.first { $0.key.lowercased() == "idempotency-key" }?.value ?? ""
        let keyChars = CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-")
        guard key.utf8.count <= 64, key.unicodeScalars.allSatisfy({ keyChars.contains($0) }) else { return nil }
        return key
    }
}

/// Where the secrets are kept: the Keychain in the app, memory in tests.
public protocol UpstreamSecretStore: Sendable {
    func get(_ account: String) -> String?
    func set(_ account: String, _ value: String)
    func delete(_ account: String)
}

extension Keychain: UpstreamSecretStore {}

public final class UpstreamHost: CoreCalloutHandler, @unchecked Sendable {
    /// The most of an answer that is passed to the core (coreapi.MaxCalloutBody).
    public static let maxAnswer = (1 << 20) + 1
    private let secrets: UpstreamSecretStore, transport: UpstreamTransport
    private let ownKey: @Sendable () -> Data?, ownPort: @Sendable () -> UInt16
    private let name: String

    /// ownKey: the public key of THIS gateway's Noise server, ownPort: its port (an upstream must be another gateway).
    public init(secrets: UpstreamSecretStore, ownKey: @escaping @Sendable () -> Data?, ownPort: @escaping @Sendable () -> UInt16,
                transport: UpstreamTransport = NoiseHTTPTransport(), computerName: String = ProcessInfo.processInfo.hostName) {
        self.secrets = secrets; self.ownKey = ownKey; self.ownPort = ownPort; self.transport = transport
        name = "minigw gateway " + String(String.UnicodeScalarView(computerName.unicodeScalars.filter { $0.value > 0x20 && $0.value < 0x7f }))
    }

    struct Secret: Codable {
        var token: String, key: String, device: String, address: String
    }

    static func account(_ upstream: String) -> String { "upstream:" + upstream }

    private func load(_ upstream: String) -> Secret? {
        guard let v = secrets.get(Self.account(upstream)), let s = try? JSONDecoder().decode(Secret.self, from: Data(v.utf8)),
              !s.token.isEmpty, !s.key.isEmpty, !s.device.isEmpty else { return nil }
        return s
    }

    private func deviceKey(_ s: Secret) -> Curve25519.Signing.PrivateKey? {
        Noise.fromBase64url(s.device).flatMap { try? Curve25519.Signing.PrivateKey(rawRepresentation: $0) }
    }

    /// The token line is the upstream's "Copy Token" (<token>#<gateway key>). A token or an address of this same gateway is refused.
    func connect(_ upstream: String, address: String, line: String) -> [String: Any] {
        let parts = line.trimmingCharacters(in: .whitespacesAndNewlines).split(separator: "#", maxSplits: 1, omittingEmptySubsequences: false).map(String.init)
        guard let address = UpstreamAddress.normalize(address) else { return ["error": "upstream address: give host or host:port of a gateway (no scheme, user or path), for example mini.local:8765"] }
        guard parts.count == 2, !parts[0].isEmpty, !parts[1].isEmpty else {
            return ["error": "the token line must be the upstream's whole Copy Token (<token>#<key>)"]
        }
        guard let kb = Noise.fromBase64url(parts[1]), kb.count == 32 else {
            return ["error": "the key part of the token is not a gateway key: copy the token again on the upstream"]
        }
        if let mine = ownKey(), mine == kb { return ["error": "this is the token of this same gateway: an upstream must be another gateway"] }
        if let i = address.lastIndex(of: ":"), UInt16(address[address.index(after: i)...]) == ownPort(), ownPort() != 0 {
            let host = String(address[..<i]).trimmingCharacters(in: CharacterSet(charactersIn: "[]")).lowercased()
            if host == "localhost" || host == "::1" || host.hasPrefix("127.") {
                return ["error": "this address is this same gateway: an upstream must be another gateway"]
            }
        }
        let dev = Curve25519.Signing.PrivateKey()
        let s = Secret(token: parts[0], key: parts[1], device: Noise.base64url(dev.rawRepresentation), address: address)
        guard let data = try? JSONEncoder().encode(s) else { return ["error": "cannot store the credentials"] }
        secrets.set(Self.account(upstream), String(decoding: data, as: UTF8.self))
        guard load(upstream) != nil else { return ["error": "cannot store the credentials in the Keychain: unlock the login keychain and try again"] }
        return ["device": Noise.base64url(dev.publicKey.rawRepresentation), "where": "the Keychain"]
    }

    func info(_ upstream: String) -> [String: Any] {
        guard let s = load(upstream), let k = deviceKey(s) else { return ["error": "no credentials"] }
        return ["device": Noise.base64url(k.publicKey.rawRepresentation)]
    }

    func `do`(_ upstream: String, _ q: [String: Any]) -> [String: Any] {
        guard let s = load(upstream), let k = deviceKey(s), let key = Noise.fromBase64url(s.key), key.count == 32 else {
            return ["error": "no credentials for upstream \(upstream): remove it and add it again"]
        }
        let body = (q["body"] as? String).flatMap { Data(base64Encoded: $0) }
        let method = q["method"] as? String ?? "", path = q["path"] as? String ?? ""
        guard let idem = UpstreamRequest.check(method: method, path: path, headers: q["header"] as? [String: String] ?? [:], body: body) else {
            return ["error": "this request is not one the upstream push makes"]
        }
        let a = transport.send(address: s.address, routerKey: key, device: (k, name), token: s.token, method: method, path: path,
                               idempotencyKey: idem.isEmpty ? nil : idem, body: body)
        if a.body.count > Self.maxAnswer { return ["error": "an answer of the upstream is too large"] }
        var r: [String: Any] = ["status": a.status]
        if !a.body.isEmpty { r["body"] = a.body.base64EncodedString() }
        if a.retryAfter > 0 { r["retryAfter"] = a.retryAfter }
        return r
    }

    /// A callout of the helper (coreapi.CalloutRequest) → its result (coreapi.CalloutResult). Never throws, never logs a value.
    public func callout(_ q: [String: Any]) -> [String: Any] {
        guard let op = q["op"] as? String, let up = q["upstream"] as? String, !up.isEmpty else { return ["error": "bad callout"] }
        switch op {
        case "connect": return connect(up, address: q["address"] as? String ?? "", line: q["token"] as? String ?? "")
        case "info": return info(up)
        case "forget": secrets.delete(Self.account(up)); return [:]
        case "do": return `do`(up, q)
        default: return ["error": "unknown callout"]
        }
    }
}

/// The real transport: the client's own Noise protocol (the upstream's key pinned) over a connection this app opens itself, in process:
/// there is NO listener, so nothing else on this computer can borrow the gateway's device key. One request per connection, written by hand
/// (HTTP/1.1, Connection: close); the answer is read to at most `cap` bytes of body, and a redirect is just an answer (nothing follows it).
public struct NoiseHTTPTransport: UpstreamTransport {
    public var timeout: TimeInterval
    public init(timeout: TimeInterval = 10) { self.timeout = timeout }  // a call of the helper has 30 seconds

    public func send(address: String, routerKey: Data, device: (key: Curve25519.Signing.PrivateKey, name: String), token: String,
                     method: String, path: String, idempotencyKey: String?, body: Data?) -> UpstreamAnswer {
        guard let i = address.lastIndex(of: ":"), let port = NWEndpoint.Port(String(address[address.index(after: i)...])), routerKey.count == 32 else { return UpstreamAnswer(status: 0) }
        let host = String(address[..<i]).trimmingCharacters(in: CharacterSet(charactersIn: "[]"))
        var head = "\(method) \(path) HTTP/1.1\r\nHost: upstream\r\nAuthorization: Bearer \(token)\r\nX-Minigw-Client: macOS, minigw gateway\r\nConnection: close\r\n"
        if let idempotencyKey { head += "Idempotency-Key: \(idempotencyKey)\r\n" }
        if let body { head += "Content-Type: application/json\r\nContent-Length: \(body.count)\r\n" }
        let request = Data((head + "\r\n").utf8) + (body ?? Data())
        return NoiseExchange(host: NWEndpoint.Host(host), port: port, routerKey: routerKey, device: device, request: request,
                             cap: UpstreamHost.maxAnswer, timeout: timeout).run()
    }
}

/// The answer of an HTTP/1.1 server as far as the bytes go: nil while it is incomplete. A body above `cap` is refused (status 0).
enum UpstreamHTTPAnswer {
    static let maxHead = 64 << 10

    static func parse(_ raw: Data, closed: Bool, cap: Int) -> UpstreamAnswer? {
        let sep = Data("\r\n\r\n".utf8)
        guard let end = raw.range(of: sep) else { return raw.count > maxHead ? UpstreamAnswer(status: 0) : (closed ? UpstreamAnswer(status: 0) : nil) }
        guard end.lowerBound <= maxHead, let head = String(data: raw[..<end.lowerBound], encoding: .utf8) else { return UpstreamAnswer(status: 0) }
        let lines = head.components(separatedBy: "\r\n")
        let first = lines[0].split(separator: " ")
        guard first.count >= 2, first[0].hasPrefix("HTTP/1."), let status = Int(first[1]), (100...599).contains(status) else { return UpstreamAnswer(status: 0) }
        var h: [String: String] = [:]
        for l in lines.dropFirst() {
            guard let c = l.firstIndex(of: ":") else { return UpstreamAnswer(status: 0) }
            h[l[..<c].lowercased()] = l[l.index(after: c)...].trimmingCharacters(in: .whitespaces)
        }
        let retry = Int(h["retry-after"] ?? "") ?? 0
        let body = Data(raw[end.upperBound...])
        func done(_ b: Data) -> UpstreamAnswer { b.count > cap ? UpstreamAnswer(status: 0) : UpstreamAnswer(status: status, body: b, retryAfter: max(0, retry)) }
        if (h["transfer-encoding"] ?? "").lowercased().contains("chunked") {
            var out = Data(), rest = body
            while true {
                guard let eol = rest.range(of: Data("\r\n".utf8)) else { return closed ? UpstreamAnswer(status: 0) : nil }
                guard let size = Int(String(decoding: rest[..<eol.lowerBound], as: UTF8.self).split(separator: ";")[0], radix: 16), size >= 0 else { return UpstreamAnswer(status: 0) }
                if size == 0 { return done(out) }
                guard size <= cap + 1, out.count + size <= cap + 1 else { return UpstreamAnswer(status: 0) }
                let start = eol.upperBound
                guard rest.count >= start + size + 2 else { return closed ? UpstreamAnswer(status: 0) : nil }
                out += rest[start..<start + size]
                rest = Data(rest[(start + size + 2)...])
            }
        }
        if let n = Int(h["content-length"] ?? "") {
            guard n >= 0, n <= cap else { return UpstreamAnswer(status: 0) }
            if body.count >= n { return done(Data(body.prefix(n))) }
            return closed ? UpstreamAnswer(status: 0) : nil
        }
        if body.count > cap { return UpstreamAnswer(status: 0) }
        return closed ? done(body) : nil  // no length: the end of the connection ends the answer
    }
}

/// One Noise connection for one request, with Network.framework callbacks and a semaphore: the calling thread waits (never the cooperative
/// pool), everything else runs on a userInitiated queue. The same handshake and framing as NoiseProxy.accept, without a local side.
final class NoiseExchange: @unchecked Sendable {
    private let host: NWEndpoint.Host, port: NWEndpoint.Port, routerKey: Data, device: (key: Curve25519.Signing.PrivateKey, name: String)
    private let request: Data, cap: Int, timeout: TimeInterval
    private let queue = DispatchQueue(label: "dev.trojar.minigateway.upstream", qos: .userInitiated)
    private let done = DispatchSemaphore(value: 0)
    private let lock = NSLock()
    private var result: UpstreamAnswer?
    private var conn: NWConnection?

    init(host: NWEndpoint.Host, port: NWEndpoint.Port, routerKey: Data, device: (key: Curve25519.Signing.PrivateKey, name: String), request: Data, cap: Int, timeout: TimeInterval) {
        self.host = host; self.port = port; self.routerKey = routerKey; self.device = device; self.request = request; self.cap = cap; self.timeout = timeout
    }

    private func finish(_ a: UpstreamAnswer) {
        lock.lock()
        let first = result == nil
        if first { result = a }
        lock.unlock()
        if first { conn?.cancel(); done.signal() }
    }

    func run() -> UpstreamAnswer {
        let c = NWConnection(host: host, port: port, using: .tcp)
        conn = c
        var hs = Noise.Initiator(routerKey: routerKey, suite: .aesGCM)
        c.stateUpdateHandler = { [self] state in
            switch state {
            case .ready:
                guard let first = try? hs.first() else { return finish(UpstreamAnswer(status: 0)) }
                c.send(content: hs.suite.magic + first, completion: .contentProcessed { _ in })
                readHandshake(c, Data()) { [self] reply in
                    guard let reply, let session = try? hs.finish(reply) else { return finish(UpstreamAnswer(status: 0)) }
                    // the device proof is its own frame, then the request in frames
                    c.send(content: Noise.frames(Noise.deviceProof(device.key, name: device.name, hash: session.hash), session.send), completion: .contentProcessed { _ in })
                    var off = 0
                    while off < request.count {
                        let n = min(Noise.maxPlain, request.count - off)
                        c.send(content: Noise.frames(request.subdata(in: off..<off + n), session.send), completion: .contentProcessed { _ in })
                        off += n
                    }
                    readAnswer(c, session.receive, Data(), Data())
                }
            case .failed, .cancelled: finish(UpstreamAnswer(status: 0))
            default: break
            }
        }
        c.start(queue: queue)
        if done.wait(timeout: .now() + timeout) == .timedOut { finish(UpstreamAnswer(status: 0)) }
        lock.lock(); defer { lock.unlock() }
        return result ?? UpstreamAnswer(status: 0)
    }

    private func readHandshake(_ c: NWConnection, _ buf: Data, _ then: @escaping (Data?) -> Void) {
        c.receive(minimumIncompleteLength: 1, maximumLength: Noise.handshakeLength - buf.count) { data, _, end, error in
            let b = buf + (data ?? Data())
            if b.count == Noise.handshakeLength { return then(b) }
            if end || error != nil { return then(nil) }  // the upstream did not accept (wrong key?)
            self.readHandshake(c, b, then)
        }
    }

    private func readAnswer(_ c: NWConnection, _ cipher: Noise.Cipher, _ pending: Data, _ plain: Data) {
        c.receive(minimumIncompleteLength: 1, maximumLength: 256 * 1024) { data, _, end, error in
            var pending = pending + (data ?? Data()), plain = plain
            guard let more = try? Noise.unframe(&pending, cipher) else { return self.finish(UpstreamAnswer(status: 0)) }
            plain += more
            if plain.count > self.cap + UpstreamHTTPAnswer.maxHead { return self.finish(UpstreamAnswer(status: 0)) }  // never more than this is held
            let closed = end || error != nil
            if let a = UpstreamHTTPAnswer.parse(plain, closed: closed, cap: self.cap) { return self.finish(a) }
            if closed { return self.finish(UpstreamAnswer(status: 0)) }
            self.readAnswer(c, cipher, pending, plain)
        }
    }
}

/// The same rules as the core's NormalizeUpstreamAddress (client/core/upstream.go): host or host:port of a gateway, no scheme, user or path,
/// not an unspecified, link-local or multicast address. Shared fixture client/testdata/upstream-addresses.json, asserted by Go and Swift.
public enum UpstreamAddress {
    public static func normalize(_ s: String) -> String? {
        guard !s.isEmpty, s.utf8.count <= 253, !s.contains("://"), s.unicodeScalars.allSatisfy({ !"/@?#\\ \t\r\n".unicodeScalars.contains($0) }) else { return nil }
        var host = s, port = "8765"
        if s.hasPrefix("[") {
            guard let close = s.firstIndex(of: "]") else { return nil }
            host = String(s[s.index(after: s.startIndex)..<close])
            let rest = s[s.index(after: close)...]
            if !rest.isEmpty { guard rest.hasPrefix(":") else { return nil }; port = String(rest.dropFirst()) }
        } else if let c = s.firstIndex(of: ":") {
            guard s.firstIndex(of: ":") == s.lastIndex(of: ":") else { return nil }  // an IPv6 address needs [ ]
            host = String(s[..<c]); port = String(s[s.index(after: c)...])
        }
        guard !host.isEmpty, port.allSatisfy({ $0.isASCII && $0.isNumber }), let p = Int(port), (1...65535).contains(p) else { return nil }
        var v4 = in_addr(), v6 = in6_addr()
        if inet_pton(AF_INET, host, &v4) == 1 {
            let b = withUnsafeBytes(of: &v4) { Array($0) }
            if b == [0, 0, 0, 0] || (b[0] == 169 && b[1] == 254) || (224...239).contains(b[0]) { return nil }
        } else if inet_pton(AF_INET6, host, &v6) == 1 {
            let b = withUnsafeBytes(of: &v6) { Array($0) }
            if b.allSatisfy({ $0 == 0 }) || (b[0] == 0xfe && b[1] & 0xc0 == 0x80) || b[0] == 0xff { return nil }
        } else if host.range(of: "^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$", options: .regularExpression) == nil { return nil }
        let h = host.lowercased()
        return h.contains(":") ? "[\(h)]:\(p)" : "\(h):\(p)"
    }
}
