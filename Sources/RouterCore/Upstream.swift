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
    /// timeout: a hard TOTAL deadline for the whole request (not an idle timeout): a server that sends a byte now and then gets no more time.
    func send(address: String, routerKey: Data, device: (key: Curve25519.Signing.PrivateKey, name: String), token: String,
              method: String, path: String, idempotencyKey: String?, body: Data?, timeout: TimeInterval) -> UpstreamAnswer
}

/// What the helper may ask the app to send. The helper is the untrusted party here (it exists to keep a core that a hostile note or bundle
/// has taken over away from the network and the credentials), so the app admits only the requests the push really makes, by method and
/// path pattern (client/testdata/upstream-requests.json is asserted by Go, which sends them, and by Swift, which admits them), with one
/// header (the idempotency key) and a body of bounded size. The request line is written by hand, to the address configured at `add`.
public enum UpstreamRequest {
    public static let maxBody = 1 << 20
    public static let maxPath = 512

    /// method, anchored path pattern. The ids are numbers of at most 18 digits (a project, a team's notes segment, a submission, a note);
    /// the short name is as the gateways make them.
    public static let patterns: [(method: String, pattern: String)] = [
        ("GET", "^/minigw/v1/projects$"),
        ("GET", "^/minigw/v1/teams$"),
        ("GET", "^/minigw/v1/projects/[a-z0-9][a-z0-9-]{1,62}$"),
        ("GET", "^/minigw/v1/projects/[0-9]{1,18}/kb/submissions/[0-9]{1,18}$"),
        ("POST", "^/minigw/v1/projects/[0-9]{1,18}/kb/submissions$"),
        ("GET", "^/minigw/v1/projects/[0-9]{1,18}/notes/[0-9]{1,18}$"),
    ]

    static func admits(method: String, path: String) -> Bool {
        guard path.utf8.count <= maxPath else { return false }
        return patterns.contains { p in
            p.method == method && path.range(of: p.pattern, options: .regularExpression).map { $0 == path.startIndex..<path.endIndex } == true
        }
    }

    /// The checked idempotency key ("" = none), or nil when the request is refused.
    public static func check(method: String, path: String, headers: [String: String], body: Data?) -> String? {
        guard admits(method: method, path: path), (body?.count ?? 0) <= maxBody, method == "POST" || body == nil else { return nil }
        let key = headers.first { $0.key.lowercased() == "idempotency-key" }?.value ?? ""
        let keyChars = CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-")
        guard key.utf8.count <= 64, key.unicodeScalars.allSatisfy({ keyChars.contains($0) }) else { return nil }
        return key
    }

    /// One header line, the only way a header is written: the name a token, the value printable ASCII (no CR, LF, other control character or
    /// non-ASCII byte), so nothing a caller supplies can end a header or start another.
    static func headerLine(_ name: String, _ value: String) -> String? {
        let tokenChars = CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-")
        guard !name.isEmpty, name.unicodeScalars.allSatisfy({ tokenChars.contains($0) }),
              value.unicodeScalars.allSatisfy({ $0.value >= 0x20 && $0.value < 0x7f }) else { return nil }
        return name + ": " + value + "\r\n"
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
        var name: String?  // how this gateway is named on the upstream's device list
    }

    /// The same rule as the core's ValidDeviceName: printable ASCII, 1 to 64 characters, no space at either end.
    static func validDeviceName(_ s: String) -> Bool {
        (1...64).contains(s.utf8.count) && s.unicodeScalars.allSatisfy { $0.value >= 0x20 && $0.value <= 0x7e } && s == s.trimmingCharacters(in: .whitespaces)
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
    func connect(_ upstream: String, address: String, line: String, deviceName: String) -> [String: Any] {
        guard Self.validDeviceName(deviceName) else { return ["error": "device name: printable ASCII, 1 to 64 characters"] }
        let parts = line.trimmingCharacters(in: .whitespacesAndNewlines).split(separator: "#", maxSplits: 1, omittingEmptySubsequences: false).map(String.init)
        guard let address = UpstreamAddress.normalize(address) else { return ["error": "upstream address: give host or host:port of a gateway (no scheme, user or path), for example mini.local:8765"] }
        guard parts.count == 2, let tokenPart = parts.first, let keyPart = parts.last, !tokenPart.isEmpty, !keyPart.isEmpty,
              tokenPart.unicodeScalars.allSatisfy({ $0.value >= 0x21 && $0.value <= 0x7e }) else {  // it goes into a header: as AdminCommands.secretLine allows
            return ["error": "the token line must be the upstream's whole Copy Token (<token>#<key>)"]
        }
        guard let kb = Noise.fromBase64url(keyPart), kb.count == 32 else {
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
        let s = Secret(token: tokenPart, key: keyPart, device: Noise.base64url(dev.rawRepresentation), address: address, name: deviceName)
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
        // The helper tells how long this call may take; the app's own hard deadline is never longer (and never more than 15 s).
        let budget = min(15, max(0.5, Double(q["budgetMs"] as? Int ?? 10_000) / 1000))
        let a = transport.send(address: s.address, routerKey: key, device: (k, s.name ?? "minigw gateway"), token: s.token, method: method, path: path,
                               idempotencyKey: idem.isEmpty ? nil : idem, body: body, timeout: budget)
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
        case "connect": return connect(up, address: q["address"] as? String ?? "", line: q["token"] as? String ?? "", deviceName: q["deviceName"] as? String ?? "")
        case "info": return info(up)
        case "forget":
            secrets.delete(Self.account(up))
            return secrets.get(Self.account(up)) == nil ? [:] : ["error": "the Keychain item could not be removed (is the login keychain locked?)"]
        case "do": return `do`(up, q)
        default: return ["error": "unknown callout"]
        }
    }
}

/// The real transport: the client's own Noise protocol (the upstream's key pinned) over a connection this app opens itself, in process:
/// there is NO listener, so nothing else on this computer can borrow the gateway's device key. One request per connection, written by hand
/// (HTTP/1.1, Connection: close); the answer is read to at most `cap` bytes of body, and a redirect is just an answer (nothing follows it).
public struct NoiseHTTPTransport: UpstreamTransport {
    public var timeout: TimeInterval  // the most any request may take; the caller's own deadline can only make it shorter
    public init(timeout: TimeInterval = 10) { self.timeout = timeout }  // a call of the helper has 30 seconds

    public func send(address: String, routerKey: Data, device: (key: Curve25519.Signing.PrivateKey, name: String), token: String,
                     method: String, path: String, idempotencyKey: String?, body: Data?, timeout asked: TimeInterval) -> UpstreamAnswer {
        let timeout = min(timeout, asked)
        guard let i = address.lastIndex(of: ":"), let port = NWEndpoint.Port(String(address[address.index(after: i)...])), routerKey.count == 32 else { return UpstreamAnswer(status: 0) }
        let host = String(address[..<i]).trimmingCharacters(in: CharacterSet(charactersIn: "[]"))
        var lines: [String?] = [UpstreamRequest.headerLine("Host", "upstream"), UpstreamRequest.headerLine("Authorization", "Bearer " + token),
                                UpstreamRequest.headerLine("X-Minigw-Client", "macOS, minigw gateway"), UpstreamRequest.headerLine("Connection", "close")]
        if let idempotencyKey { lines.append(UpstreamRequest.headerLine("Idempotency-Key", idempotencyKey)) }
        if let body { lines += [UpstreamRequest.headerLine("Content-Type", "application/json"), UpstreamRequest.headerLine("Content-Length", String(body.count))] }
        guard UpstreamRequest.admits(method: method, path: path), !lines.contains(where: { $0 == nil }) else { return UpstreamAnswer(status: 0) }
        let head = "\(method) \(path) HTTP/1.1\r\n" + lines.compactMap { $0 }.joined()
        let request = Data((head + "\r\n").utf8) + (body ?? Data())
        return NoiseExchange(host: NWEndpoint.Host(host), port: port, routerKey: routerKey, device: device, request: request,
                             cap: UpstreamHost.maxAnswer - 1, timeout: timeout).run()  // the body, at most 1 MiB
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
    private var reader: UpstreamHTTPReader  // touched only by the receive callbacks, which run one after another

    init(host: NWEndpoint.Host, port: NWEndpoint.Port, routerKey: Data, device: (key: Curve25519.Signing.PrivateKey, name: String), request: Data, cap: Int, timeout: TimeInterval) {
        self.host = host; self.port = port; self.routerKey = routerKey; self.device = device; self.request = request; self.cap = cap; self.timeout = timeout
        reader = UpstreamHTTPReader(cap: cap)
        reader.cancelled = { [unowned self] in self.isFinished }
    }

    private var isFinished: Bool { lock.lock(); defer { lock.unlock() }; return result != nil }

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
                    readAnswer(c, session.receive, Data())
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

    private func readAnswer(_ c: NWConnection, _ cipher: Noise.Cipher, _ pending: Data) {
        c.receive(minimumIncompleteLength: 1, maximumLength: 256 * 1024) { data, _, end, error in
            if self.isFinished { return }  // a result exists (an answer, or the deadline): nothing more is read or parsed
            var pending = pending + (data ?? Data())
            guard let plain = try? Noise.unframe(&pending, cipher) else { return self.finish(UpstreamAnswer(status: 0)) }
            let closed = end || error != nil
            var outcome = self.reader.feed(plain)  // incremental: the bytes already looked at are never looked at again
            if outcome == .needMore && closed { outcome = self.reader.end() }
            switch outcome {
            case .done(let a): self.finish(a)
            case .failed: self.finish(UpstreamAnswer(status: 0))
            case .needMore: if closed { self.finish(UpstreamAnswer(status: 0)) } else { self.readAnswer(c, cipher, pending) }
            }
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
        // Go accepts only the plain dotted quad (no leading zeros: "010.0.0.1" is octal to some resolvers); inet_pton on the Mac is looser
        if host.allSatisfy({ $0.isASCII && ($0.isNumber || $0 == ".") }),
           host.range(of: "^(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])(\\.(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])){3}$", options: .regularExpression) == nil { return nil }
        if inet_pton(AF_INET, host, &v4) == 1 {
            let b = withUnsafeBytes(of: &v4) { Array($0) }
            if b == [0, 0, 0, 0] || b.starts(with: [169, 254]) || (b.first.map { (224...239).contains($0) } ?? true) { return nil }
        } else if inet_pton(AF_INET6, host, &v6) == 1 {
            let b = withUnsafeBytes(of: &v6) { Array($0) }
            if b.allSatisfy({ $0 == 0 }) || (b.first == 0xfe && (b.dropFirst().first ?? 0) & 0xc0 == 0x80) || b.first == 0xff { return nil }
        } else if host.range(of: "^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$", options: .regularExpression) == nil
                    || host.range(of: "^(0[xX][0-9a-fA-F]+|[0-9]+)(\\.(0[xX][0-9a-fA-F]+|[0-9]+))*$", options: .regularExpression) != nil { return nil }  // an address in disguise
        let h = host.lowercased()
        return h.contains(":") ? "[\(h)]:\(p)" : "\(h):\(p)"
    }
}
