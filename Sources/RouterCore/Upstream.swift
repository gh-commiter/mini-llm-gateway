import CryptoKit
import Foundation
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

/// One request to an upstream. Blocks the calling thread until it ends: never call it from the cooperative pool (CoreHelper calls it
/// on its own queue). Tests give a fake.
public protocol UpstreamTransport: Sendable {
    func send(address: String, routerKey: Data, device: (key: Curve25519.Signing.PrivateKey, name: String), token: String,
              method: String, path: String, headers: [String: String], body: Data?) -> UpstreamAnswer
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
        let a = transport.send(address: s.address, routerKey: key, device: (k, name), token: s.token, method: q["method"] as? String ?? "GET",
                               path: q["path"] as? String ?? "", headers: q["header"] as? [String: String] ?? [:], body: body)
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

/// The real transport: a proxy that speaks Noise to the upstream, for this one request, on a port chosen by the system; the request goes
/// through it with a session that follows no redirect and reads at most `cap` bytes.
public struct NoiseHTTPTransport: UpstreamTransport {
    public var timeout: TimeInterval
    public init(timeout: TimeInterval = 10) { self.timeout = timeout }  // a call of the helper has 30 seconds

    public func send(address: String, routerKey: Data, device: (key: Curve25519.Signing.PrivateKey, name: String), token: String,
                     method: String, path: String, headers: [String: String], body: Data?) -> UpstreamAnswer {
        guard let proxy = NoiseProxy(routerURL: "http://" + address, routerKey: routerKey, device: device, suite: .aesGCM) else { return UpstreamAnswer(status: 0) }
        do { try proxy.startEphemeral() } catch { return UpstreamAnswer(status: 0) }
        defer { proxy.stop() }
        var h = headers
        h["authorization"] = "Bearer " + token
        h["X-Minigw-Client"] = "macOS, minigw gateway"
        if body != nil { h["content-type"] = "application/json" }
        return CappedFetch.send(url: proxy.localURL + path, method: method, headers: h, body: body, timeout: timeout, cap: UpstreamHost.maxAnswer)
    }
}

/// A request that follows no redirect and reads at most `cap` bytes of the answer (more: the task is cancelled and the answer is
/// "no usable answer", status 0). Blocks the calling thread; the delegate queue is userInitiated so that this cannot starve (Profiles.session).
enum CappedFetch {
    final class Delegate: NSObject, URLSessionDataDelegate, @unchecked Sendable {
        let cap: Int
        private let lock = NSLock()
        private var data = Data(), response: HTTPURLResponse?, tooBig = false
        let done = DispatchSemaphore(value: 0)
        init(cap: Int) { self.cap = cap }
        func urlSession(_ s: URLSession, task: URLSessionTask, willPerformHTTPRedirection r: HTTPURLResponse, newRequest: URLRequest,
                        completionHandler: @escaping (URLRequest?) -> Void) { completionHandler(nil) }  // the redirect itself is the answer
        func urlSession(_ s: URLSession, dataTask: URLSessionDataTask, didReceive r: URLResponse, completionHandler: @escaping (URLSession.ResponseDisposition) -> Void) {
            lock.lock(); response = r as? HTTPURLResponse; lock.unlock()
            completionHandler(.allow)
        }
        func urlSession(_ s: URLSession, dataTask: URLSessionDataTask, didReceive d: Data) {
            lock.lock(); data += d; let big = data.count > cap; if big { tooBig = true }; lock.unlock()
            if big { dataTask.cancel() }
        }
        func urlSession(_ s: URLSession, task: URLSessionTask, didCompleteWithError e: Error?) { done.signal() }
        var result: UpstreamAnswer {
            lock.lock(); defer { lock.unlock() }
            guard let r = response, !tooBig else { return UpstreamAnswer(status: 0) }
            return UpstreamAnswer(status: r.statusCode, body: data, retryAfter: Int(r.value(forHTTPHeaderField: "Retry-After") ?? "") ?? 0)
        }
    }

    static func send(url: String, method: String, headers: [String: String], body: Data?, timeout: TimeInterval, cap: Int) -> UpstreamAnswer {
        guard let u = URL(string: url) else { return UpstreamAnswer(status: 0) }
        var r = URLRequest(url: u, timeoutInterval: timeout)
        r.httpMethod = method
        r.httpBody = body
        for (k, v) in headers { r.setValue(v, forHTTPHeaderField: k) }
        let d = Delegate(cap: cap)
        let q = OperationQueue(); q.qualityOfService = .userInitiated
        let s = URLSession(configuration: .ephemeral, delegate: d, delegateQueue: q)
        defer { s.invalidateAndCancel() }
        s.dataTask(with: r).resume()
        d.done.wait()
        return d.result
    }
}
