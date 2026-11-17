import CryptoKit
import Foundation
import Testing
@testable import RouterCore

// The upstream gateway on the Mac (docs/gateway.md, "An upstream gateway"): the owner's commands proven against the Go client by the
// shared fixture client/testdata/upstream-commands.json; the host (Keychain item, same-gateway refusal, answers as data); the capped,
// redirect-free fetch; and the real minigw-core helper asking this app for the network through callouts. The transport to the upstream is
// a fake in these tests; the Noise connection itself is covered by the existing proxy tests (RouterCoreTests encryptedProxyToRouter).

private let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()

@Suite struct UpstreamCommandParity {
    @Test func everyCommandMatchesTheGoOutput() throws {
        let fx = try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("client/testdata/upstream-commands.json"))) as! [String: Any]
        let cases = fx["cases"] as! [[String: Any]]
        #expect(cases.count >= 15)
        for c in cases {
            let name = c["name"] as! String
            let responses = c["responses"] as! [[String: Any]]
            nonisolated(unsafe) var requests: [[String]] = []
            nonisolated(unsafe) var outText = ""
            let cmd = AdminCommands(call: { method, path, body in
                requests.append([method, path, body.map { String(decoding: $0, as: UTF8.self) } ?? ""])
                guard let r = responses.first(where: { ($0["match"] as! String) == method + " " + path }) else {
                    return (599, Data(#"{"error":{"message":"no such response in the fixture"}}"#.utf8))
                }
                return (r["status"] as! Int, Data((r["body"] as! String).utf8))
            }, out: { outText += $0 })
            cmd.zone = TimeZone(identifier: "UTC")!
            cmd.pause = { _ in }
            let o = c["opts"] as! [String: Any]
            var opts = AdminOpts(team: o["team"] as? String ?? "")
            opts.deviceName = o["deviceName"] as? String ?? ""; opts.remoteProject = o["remoteProject"] as? String ?? ""; opts.remoteTeam = o["remoteTeam"] as? String ?? ""; opts.classes = o["class"] as? String ?? ""
            opts.withPrivate = o["withPrivate"] as? Bool ?? false; opts.dryRun = o["dryRun"] as? Bool ?? false
            let stdin = c["stdin"] as? String ?? ""
            opts.tokenInput = { stdin }
            var exit: Int32 = 0, errText = ""
            do { try cmd.run(c["args"] as! [String], opts) } catch let e as AdminCLIError { exit = e.code; errText = "minigw: " + e.message + "\n" }
            let want = c["want"] as! [String: Any]
            #expect(outText == want["out"] as! String, "\(name): stdout")
            #expect(errText == want["err"] as! String, "\(name): stderr")
            #expect(Int(exit) == want["exit"] as! Int, "\(name): exit")
            #expect(requests == want["requests"] as! [[String]], "\(name): requests")
        }
    }

    @Test func aLongPushAsksAgainForTheLinkWhereItStoppedAndOnlyWhileItGetsFurther() throws {
        nonisolated(unsafe) var calls = 0
        nonisolated(unsafe) var waits: [Double] = []
        nonisolated(unsafe) var bodies: [String] = []
        nonisolated(unsafe) var outText = ""
        let proj = #"{"projects":[{"ID":3,"Slug":"shop","Name":"Shop","Archived":false}]}"#
        let cmd = AdminCommands(call: { method, path, body in
            if path == "/projects" { return (200, Data(proj.utf8)) }
            calls += 1
            bodies.append(String(decoding: body ?? Data(), as: UTF8.self))
            let more = calls < 3
            return (200, Data(#"{"pushes":[{"label":"shop","upstream":"hub","remote":"shop","project":3,"lines":["create \#(calls) n"],"created":1,"skipped":0,"replaced":0,"refused":0,"dryRun":false\#(more ? #","more":true,"wait":7,"next":\#(calls * 100),"stopped":"the upstream's write limit: push again in 7 seconds""# : "")}]}"#.utf8))
        }, out: { outText += $0 })
        cmd.pause = { waits.append($0) }
        try cmd.run(["upstream", "push", "shop"], AdminOpts())
        #expect(calls == 3 && waits == [7, 7])
        #expect(bodies[1] == #"{"after":100,"dryRun":false,"project":3,"upstream":"hub"}"# && bodies[2] == #"{"after":200,"dryRun":false,"project":3,"upstream":"hub"}"#, "\(bodies)")
        #expect(outText.components(separatedBy: "create ").count == 4)
        // a gateway that says "more" without getting further is not asked for ever
        calls = 0; outText = ""
        let stuck = AdminCommands(call: { method, path, _ in
            if path == "/projects" { return (200, Data(proj.utf8)) }
            calls += 1
            return (200, Data(#"{"pushes":[{"label":"shop","upstream":"hub","remote":"shop","project":3,"lines":[],"more":true,"next":50}]}"#.utf8))
        }, out: { outText += $0 })
        stuck.pause = { _ in }
        try stuck.run(["upstream", "push", "shop"], AdminOpts())
        #expect(calls == 2 && outText.contains("no progress"))
    }

    @Test func theTokenIsCheckedLikeAProviderKey() {
        #expect((try? AdminCommands.secretLine(" tok#key\n", what: "x")) == "tok#key")
        #expect(throws: AdminCLIError("no x (pipe it on stdin or type it at the prompt)")) { try AdminCommands.secretLine("  \n", what: "x") }
        #expect(throws: AdminCLIError("the x must be printable ASCII without spaces")) { try AdminCommands.secretLine("a b", what: "x") }
    }
}

final class MemorySecrets: UpstreamSecretStore, @unchecked Sendable {
    private let lock = NSLock()
    private var m: [String: String] = [:]
    func get(_ a: String) -> String? { lock.lock(); defer { lock.unlock() }; return m[a] }
    func set(_ a: String, _ v: String) { lock.lock(); m[a] = v; lock.unlock() }
    func delete(_ a: String) { lock.lock(); m[a] = nil; lock.unlock() }
    var all: [String: String] { lock.lock(); defer { lock.unlock() }; return m }
}

final class FakeTransport: UpstreamTransport, @unchecked Sendable {
    struct Call { var address: String; var key: Data; var device: String; var deviceName: String; var token: String; var method: String; var path: String; var idempotencyKey: String?; var body: Data?; var timeout: TimeInterval }
    private let lock = NSLock()
    private var _calls: [Call] = []
    var handler: @Sendable (Call) -> UpstreamAnswer = { _ in UpstreamAnswer(status: 200, body: Data("{}".utf8)) }
    var calls: [Call] { lock.lock(); defer { lock.unlock() }; return _calls }
    func send(address: String, routerKey: Data, device: (key: Curve25519.Signing.PrivateKey, name: String), token: String, method: String, path: String,
              idempotencyKey: String?, body: Data?, timeout: TimeInterval) -> UpstreamAnswer {
        let c = Call(address: address, key: routerKey, device: Noise.base64url(device.key.publicKey.rawRepresentation), deviceName: device.name, token: token, method: method, path: path, idempotencyKey: idempotencyKey, body: body, timeout: timeout)
        lock.lock(); _calls.append(c); lock.unlock()
        return handler(c)
    }
}

@Suite struct UpstreamHostTests {
    let ownKey = Noise.newKey().publicKey.rawRepresentation
    func host(_ secrets: MemorySecrets, _ t: FakeTransport = FakeTransport()) -> UpstreamHost {
        let own = ownKey
        return UpstreamHost(secrets: secrets, ownKey: { own }, ownPort: { 8765 }, transport: t, computerName: "my mac")
    }
    func line(_ key: Data = Noise.newKey().publicKey.rawRepresentation) -> String { "SECRET-TOKEN#" + Noise.base64url(key) }

    @Test func connectKeepsTheSecretInTheStoreAndAnswersOnlyThePublicDeviceId() throws {
        let s = MemorySecrets()
        let h = host(s)
        let r = h.callout(["op": "connect", "deviceName": "gw test", "upstream": "hub", "address": "hub.example:8765", "token": line()])
        #expect(r["error"] == nil && r["where"] as? String == "the Keychain")
        let device = try #require(r["device"] as? String)
        #expect(device.count == 43)
        let stored = try #require(s.all["upstream:hub"])
        #expect(stored.contains("SECRET-TOKEN") && Set(s.all.keys) == ["upstream:hub"])
        let answer = String(decoding: try JSONSerialization.data(withJSONObject: r), as: UTF8.self)
        #expect(!answer.contains("SECRET-TOKEN"))
        let seed = try #require((try JSONSerialization.jsonObject(with: Data(stored.utf8)) as? [String: Any])?["device"] as? String)
        #expect(!answer.contains(seed))
        #expect(h.callout(["op": "info", "upstream": "hub"])["device"] as? String == device)
        // forget removes the item
        _ = h.callout(["op": "forget", "upstream": "hub"])
        #expect(s.all.isEmpty && h.callout(["op": "info", "upstream": "hub"])["error"] != nil)
    }

    @Test func aTokenOrAnAddressOfThisSameGatewayIsRefused() {
        let s = MemorySecrets()
        let h = host(s)
        for (address, token, why) in [("hub.example:8765", line(ownKey), "token"), ("127.0.0.1:8765", line(), "address"), ("localhost:8765", line(), "address"), ("[::1]:8765", line(), "address")] {
            let r = h.callout(["op": "connect", "deviceName": "gw test", "upstream": "me", "address": address, "token": token])
            #expect((r["error"] as? String)?.contains("this same gateway") == true, "\(why) \(address)")
        }
        // another port on this computer is another gateway
        #expect(h.callout(["op": "connect", "deviceName": "gw test", "upstream": "other", "address": "127.0.0.1:9000", "token": line()])["error"] == nil)
        for bad in ["just-a-token", "#", "tok#", "#key", "tok#short"] {
            #expect(h.callout(["op": "connect", "deviceName": "gw test", "upstream": "bad", "address": "hub.example:8765", "token": bad])["error"] != nil, Comment(rawValue: bad))
        }
        #expect(s.all.keys.sorted() == ["upstream:other"])
    }

    @Test func doSendsToTheConfiguredAddressAsTheGatewaysOwnDeviceAndPassesTheAnswerAsData() throws {
        let s = MemorySecrets(), t = FakeTransport()
        let h = host(s, t)
        let key = Noise.newKey().publicKey.rawRepresentation
        let device = try #require(h.callout(["op": "connect", "deviceName": "gw test", "upstream": "hub", "address": "hub.example:9000", "token": line(key)])["device"] as? String)
        t.handler = { _ in UpstreamAnswer(status: 201, body: Data(#"{"id":5}"#.utf8), retryAfter: 3) }
        let r = h.callout(["op": "do", "upstream": "hub", "method": "POST", "path": "/minigw/v1/projects/7/kb/submissions", "header": ["Idempotency-Key": "k"], "body": Data("{}".utf8).base64EncodedString()])
        #expect(r["status"] as? Int == 201 && r["retryAfter"] as? Int == 3)
        #expect((r["body"] as? String).flatMap { Data(base64Encoded: $0) } == Data(#"{"id":5}"#.utf8))
        let c = try #require(t.calls.first)
        #expect(c.address == "hub.example:9000" && c.key == key && c.device == device && c.token == "SECRET-TOKEN" && c.path == "/minigw/v1/projects/7/kb/submissions")
        // an upstream that does not exist here: no request at all
        let none = h.callout(["op": "do", "upstream": "nope", "method": "GET", "path": "/x"])
        #expect(none["error"] != nil && t.calls.count == 1)
        // an answer above the cap is not passed on
        t.handler = { _ in UpstreamAnswer(status: 200, body: Data(count: UpstreamHost.maxAnswer + 1)) }
        #expect(h.callout(["op": "do", "upstream": "hub", "method": "GET", "path": "/x"])["error"] != nil)
        #expect(h.callout(["op": "frobnicate", "upstream": "hub"])["error"] != nil && h.callout([:])["error"] != nil)
    }
}

/// The requests the helper may ask the app to send are a closed set: the helper is the untrusted party.
@Suite struct UpstreamRequestsAreAClosedSet {
    func host(_ t: FakeTransport) -> UpstreamHost {
        let s = MemorySecrets()
        let own = Noise.newKey().publicKey.rawRepresentation
        let h = UpstreamHost(secrets: s, ownKey: { own }, ownPort: { 8765 }, transport: t, computerName: "mac")
        _ = h.callout(["op": "connect", "deviceName": "gw test", "upstream": "hub", "address": "hub.example:8765", "token": "SECRET-TOKEN#" + Noise.base64url(Noise.newKey().publicKey.rawRepresentation)])
        return h
    }

    @Test func hostilePathsMethodsAndHeadersNeverReachTheTransport() {
        let t = FakeTransport(), h = host(t)
        let paths = ["@evil.example/x", "/minigw/v1/../admin", "/minigw/v1/a/../b", "/minigw/v1/./x", "/minigw/v1//x", "/minigw/v1/a b", "/minigw/v1/x#y", "/minigw/v1/x\\y",
                     "/minigw/v1/%2e%2e/x", "/minigw/v1/x@evil.example", "http://evil.example/minigw/v1/x", "//evil.example/minigw/v1/x", "/admin/users", "minigw/v1/x",
                     "/minigw/v1/x\r\nHost: evil", "/minigw/v1/x?a=1?b=2", "/minigw/v1/x?q=a b", "/minigw/v1/" + String(repeating: "a", count: 600), ""]
        for p in paths {
            let r = h.callout(["op": "do", "upstream": "hub", "method": "GET", "path": p])
            #expect(r["error"] != nil && r["status"] == nil, Comment(rawValue: "path \(p.debugDescription)"))
        }
        for m in ["PUT", "DELETE", "PATCH", "CONNECT", "get", "", "GET /x HTTP/1.1\r\nX: y"] {
            #expect(h.callout(["op": "do", "upstream": "hub", "method": m, "path": "/minigw/v1/projects"])["error"] != nil, Comment(rawValue: "method \(m.debugDescription)"))
        }
        #expect(h.callout(["op": "do", "upstream": "hub", "method": "POST", "path": "/minigw/v1/projects/7/kb/submissions", "header": ["Idempotency-Key": "a b"]])["error"] != nil)
        #expect(h.callout(["op": "do", "upstream": "hub", "method": "POST", "path": "/minigw/v1/projects/7/kb/submissions", "header": ["Idempotency-Key": String(repeating: "k", count: 65)]])["error"] != nil)
        let big = Data(count: UpstreamRequest.maxBody + 1).base64EncodedString()
        #expect(h.callout(["op": "do", "upstream": "hub", "method": "POST", "path": "/minigw/v1/projects/7/kb/submissions", "body": big])["error"] != nil)
        #expect(t.calls.isEmpty, "a refused request reached the transport")
    }

    @Test func thePushRequestsPassAndOnlyTheIdempotencyKeyIsForwarded() {
        let t = FakeTransport(), h = host(t)
        for (m, p) in [("GET", "/minigw/v1/projects/shop"), ("GET", "/minigw/v1/teams"), ("GET", "/minigw/v1/projects/7/kb/submissions/12"), ("GET", "/minigw/v1/projects/7/notes/55"),
                       ("POST", "/minigw/v1/projects/7/kb/submissions"), ("GET", "/minigw/v1/projects?archived=1")] {
            #expect(h.callout(["op": "do", "upstream": "hub", "method": m, "path": p, "header": ["Idempotency-Key": "up-0123abc", "Authorization": "Bearer evil", "Host": "evil.example", "X-Other": "1"]])["error"] == nil, Comment(rawValue: p))
        }
        #expect(t.calls.count == 6 && t.calls.allSatisfy { $0.idempotencyKey == "up-0123abc" && $0.token == "SECRET-TOKEN" && $0.address == "hub.example:8765" })
    }

    @Test func theDeviceNameIsTheOwnersChoiceAndTheBudgetIsTheHelpers() throws {
        let t = FakeTransport(), s = MemorySecrets()
        let own = Noise.newKey().publicKey.rawRepresentation
        let h = UpstreamHost(secrets: s, ownKey: { own }, ownPort: { 8765 }, transport: t, computerName: "my-secret-hostname")
        let line = "t#" + Noise.base64url(Noise.newKey().publicKey.rawRepresentation)
        for bad in ["", String(repeating: "n", count: 65), "tab\there", "caf\u{e9}", " lead", "trail "] {
            #expect(h.callout(["op": "connect", "upstream": "x", "deviceName": bad, "address": "hub.example:8765", "token": line])["error"] != nil, Comment(rawValue: bad.debugDescription))
        }
        #expect(s.all.isEmpty)
        #expect(h.callout(["op": "connect", "upstream": "hub", "deviceName": "Uros laptop", "address": "hub.example:8765", "token": line])["error"] == nil)
        _ = h.callout(["op": "do", "upstream": "hub", "method": "GET", "path": "/minigw/v1/projects", "budgetMs": 4000])
        _ = h.callout(["op": "do", "upstream": "hub", "method": "GET", "path": "/minigw/v1/projects", "budgetMs": 600_000])
        _ = h.callout(["op": "do", "upstream": "hub", "method": "GET", "path": "/minigw/v1/projects"])
        let c = t.calls
        #expect(c.map(\.deviceName) == ["Uros laptop", "Uros laptop", "Uros laptop"])  // never the computer's name
        #expect(c.map(\.timeout) == [4, 15, 10])
        #expect(!(s.all["upstream:hub"] ?? "").contains("my-secret-hostname"))
    }

    @Test func aKeychainItemThatCannotBeRemovedIsReported() {
        final class Stuck: UpstreamSecretStore, @unchecked Sendable {
            func get(_ a: String) -> String? { #"{"token":"t","key":"k","device":"d","address":"a:1"}"# }
            func set(_ a: String, _ v: String) {}
            func delete(_ a: String) {}  // a locked keychain: the item stays
        }
        let own = Noise.newKey().publicKey.rawRepresentation
        let h = UpstreamHost(secrets: Stuck(), ownKey: { own }, ownPort: { 8765 }, transport: FakeTransport(), computerName: "mac")
        #expect((h.callout(["op": "forget", "upstream": "hub"])["error"] as? String)?.contains("could not be removed") == true)
    }

    @Test func anUnknownCalloutKindIsRefused() {
        let h = host(FakeTransport())
        for op in ["frobnicate", "exec", "fetch", "", "DO"] { #expect(h.callout(["op": op, "upstream": "hub", "method": "GET", "path": "/minigw/v1/x"])["error"] != nil, Comment(rawValue: op)) }
        #expect(h.callout(["upstream": "hub"])["error"] != nil && h.callout(["op": "do"])["error"] != nil)
    }

    @Test func theAddressTheHelperNamesIsCheckedAgainAsTheCoreChecksIt() throws {
        let fx = try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("client/testdata/upstream-addresses.json"))) as! [String: Any]
        for (input, want) in fx["valid"] as! [String: String] { #expect(UpstreamAddress.normalize(input) == want, Comment(rawValue: input)) }
        for input in fx["invalid"] as! [String] { #expect(UpstreamAddress.normalize(input) == nil, Comment(rawValue: input.debugDescription)) }
        // and `connect` refuses a bad address with nothing stored
        let s = MemorySecrets()
        let own = Noise.newKey().publicKey.rawRepresentation
        let h = UpstreamHost(secrets: s, ownKey: { own }, ownPort: { 8765 }, transport: FakeTransport(), computerName: "mac")
        let line = "t#" + Noise.base64url(Noise.newKey().publicKey.rawRepresentation)
        for bad in ["evil.example/x", "user@evil.example", "https://evil.example", "0.0.0" + ".0:1"] {
            #expect(h.callout(["op": "connect", "deviceName": "gw test", "upstream": "x", "address": bad, "token": line])["error"] != nil, Comment(rawValue: bad))
        }
        #expect(s.all.isEmpty)
    }
}

@Suite struct UpstreamHTTPAnswerParsing {
    func parse(_ s: String, closed: Bool = false, cap: Int = 100) -> UpstreamAnswer? { UpstreamHTTPAnswer.parse(Data(s.utf8), closed: closed, cap: cap) }

    @Test func contentLengthChunkedAndCloseDelimitedAnswers() {
        #expect(parse("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi") == UpstreamAnswer(status: 200, body: Data("hi".utf8)))
        #expect(parse("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhi") == nil)  // incomplete: wait
        #expect(parse("HTTP/1.1 429 Too Many\r\nRetry-After: 12\r\nContent-Length: 0\r\n\r\n") == UpstreamAnswer(status: 429, body: Data(), retryAfter: 12))
        #expect(parse("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n2\r\nde\r\n0\r\n\r\n") == UpstreamAnswer(status: 200, body: Data("abcde".utf8)))
        #expect(parse("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nab") == nil)
        #expect(parse("HTTP/1.1 200 OK\r\n\r\nrest", closed: true) == UpstreamAnswer(status: 200, body: Data("rest".utf8)))
        #expect(parse("HTTP/1.1 200 OK\r\n\r\nrest") == nil)  // no length, still open
        // a redirect is an answer like any other
        #expect(parse("HTTP/1.1 302 Found\r\nLocation: http://evil.example/\r\nContent-Length: 0\r\n\r\n")?.status == 302)
    }

    @Test func garbageAndOversizeAreNoAnswer() {
        for bad in ["", "garbage\r\n\r\n", "HTTP/1.1 abc\r\n\r\n", "HTTP/1.1 99 X\r\n\r\n", "HTTP/1.1 200 OK\r\nno colon\r\n\r\n", "SMTP/1.1 200 OK\r\n\r\n"] {
            let a = parse(bad, closed: true)
            #expect(a == nil || a?.status == 0, Comment(rawValue: bad.debugDescription))
        }
        #expect(parse("HTTP/1.1 200 OK\r\nContent-Length: 101\r\n\r\n", cap: 100)?.status == 0)
        #expect(parse("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" + String(repeating: "ff\r\n" + String(repeating: "a", count: 255) + "\r\n", count: 3), cap: 100)?.status == 0)
        #expect(parse("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n-5\r\nabc\r\n", cap: 100)?.status == 0)
    }
}

/// A gateway that speaks the encrypted protocol and answers with whatever the test says: the real NoiseHTTPTransport against it. No local
/// listener of the app is involved (there is none); this server is the test's, on loopback.
final class ScriptedNoiseServer: @unchecked Sendable {
    let key = Noise.newKey()
    private(set) var port: UInt16 = 0
    private var fd: Int32 = -1
    private let lock = NSLock()
    private var _heads: [String] = [], _devices: [Noise.Device] = [], _accepted = 0
    var heads: [String] { lock.withLock { _heads } }
    var devices: [Noise.Device] { lock.withLock { _devices } }
    var accepted: Int { lock.withLock { _accepted } }
    let reply: @Sendable (String) -> Data
    let trickle: TimeInterval  // > 0: the answer comes one byte in a frame, one frame per this many seconds (for as long as it lasts)

    init(trickle: TimeInterval = 0, reply: @escaping @Sendable (String) -> Data) {
        self.reply = reply; self.trickle = trickle
        fd = socket(AF_INET, SOCK_STREAM, 0)
        var one: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, socklen_t(MemoryLayout<Int32>.size))
        var a = sockaddr_in()
        a.sin_family = sa_family_t(AF_INET); a.sin_addr.s_addr = inet_addr("127.0.0.1")
        _ = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
        listen(fd, 8)
        var l = socklen_t(MemoryLayout<sockaddr_in>.size)
        _ = withUnsafeMutablePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(fd, $0, &l) } }
        port = UInt16(bigEndian: a.sin_port)
        let listener = fd
        Thread { [self] in
            while true {
                let c = accept(listener, nil, nil)
                if c < 0 { return }
                lock.withLock { _accepted += 1 }
                Thread { self.serve(c) }.start()
            }
        }.start()
    }
    func stop() { close(fd) }

    private func serve(_ c: Int32) {
        defer { close(c) }
        var one: Int32 = 1
        setsockopt(c, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))  // a client that gave up must not kill the test process
        var buf = [UInt8](repeating: 0, count: 65536), hs = Data()
        while hs.count < 4 + Noise.handshakeLength {
            let k = read(c, &buf, min(buf.count, 4 + Noise.handshakeLength - hs.count))
            if k <= 0 { return }
            hs += buf[0..<k]
        }
        guard let (r, session) = try? Noise.respond(Data(hs.dropFirst(4)), key: key, suite: .aesGCM) else { return }
        _ = r.withUnsafeBytes { write(c, $0.baseAddress, $0.count) }
        var raw = Data(), plain = Data(), deviceDone = false
        while true {
            if !deviceDone, case .device(let d) = Noise.readDevice(&plain, hash: session.hash) { lock.withLock { _devices.append(d) }; deviceDone = true }
            if deviceDone, plain.range(of: Data("\r\n\r\n".utf8)) != nil { break }
            let k = read(c, &buf, buf.count)
            if k <= 0 { return }
            raw += buf[0..<k]
            if let p = try? Noise.unframe(&raw, session.receive) { plain += p }
        }
        let head = String(decoding: plain, as: UTF8.self)
        lock.withLock { _heads.append(head) }
        if trickle > 0 {
            for b in reply(head) {
                let one = Noise.frames(Data([b]), session.send)
                if one.withUnsafeBytes({ write(c, $0.baseAddress, $0.count) }) <= 0 { return }
                Thread.sleep(forTimeInterval: trickle)
            }
            return
        }
        let out = Noise.frames(reply(head), session.send)
        _ = out.withUnsafeBytes { write(c, $0.baseAddress, $0.count) }
    }
}

@Suite(.serialized) struct UpstreamNoiseTransport {
    func send(_ srv: ScriptedNoiseServer, key: Data? = nil, device: Curve25519.Signing.PrivateKey = Curve25519.Signing.PrivateKey(), method: String = "GET",
              path: String = "/minigw/v1/projects/shop", idem: String? = nil, body: Data? = nil, timeout: TimeInterval = 10) -> UpstreamAnswer {
        NoiseHTTPTransport(timeout: timeout).send(address: "127.0.0.1:\(srv.port)", routerKey: key ?? srv.key.publicKey.rawRepresentation, device: (device, "minigw gateway test"),
                                                 token: "TOKEN-1", method: method, path: path, idempotencyKey: idem, body: body, timeout: timeout)
    }

    @Test func requestAndAnswerOverTheEncryptedConnectionAsTheGatewaysDevice() throws {
        let srv = ScriptedNoiseServer { _ in Data("HTTP/1.1 201 Created\r\nContent-Type: application/json\r\nContent-Length: 8\r\nConnection: close\r\n\r\n{\"id\":5}".utf8) }
        defer { srv.stop() }
        let dev = Curve25519.Signing.PrivateKey()
        let a = send(srv, device: dev, method: "POST", path: "/minigw/v1/projects/7/kb/submissions", idem: "up-abc", body: Data("{}".utf8))
        #expect(a.status == 201 && a.body == Data(#"{"id":5}"#.utf8))
        #expect(srv.devices == [Noise.Device(id: Noise.base64url(dev.publicKey.rawRepresentation), name: "minigw gateway test")])
        let head = try #require(srv.heads.first)
        #expect(head.hasPrefix("POST /minigw/v1/projects/7/kb/submissions HTTP/1.1\r\n") && head.contains("Authorization: Bearer TOKEN-1\r\n") && head.contains("Idempotency-Key: up-abc\r\n") && head.contains("Content-Length: 2\r\n"))
        #expect(!head.lowercased().contains("evil"))
    }

    @Test func aRedirectIsTheAnswerAndNothingFollowsIt() {
        let other = ScriptedNoiseServer { _ in Data("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n".utf8) }
        defer { other.stop() }
        let srv = ScriptedNoiseServer { _ in Data("HTTP/1.1 302 Found\r\nLocation: http://127.0.0.1:\(other.port)/x\r\nContent-Length: 0\r\nConnection: close\r\n\r\n".utf8) }
        defer { srv.stop() }
        #expect(send(srv).status == 302)
        #expect(srv.accepted == 1 && other.accepted == 0, "the redirect was followed")
    }

    @Test func aWrongGatewayKeyAnOversizedAnswerAndAServerThatDiesAreNoAnswer() {
        let srv = ScriptedNoiseServer { _ in Data("HTTP/1.1 200 OK\r\nContent-Length: 2000000\r\n\r\n".utf8) }
        defer { srv.stop() }
        #expect(send(srv, key: Noise.newKey().publicKey.rawRepresentation, timeout: 3).status == 0)  // pinned key does not match: no session
        #expect(send(srv).status == 0)                                                                 // above the cap
        let dead = ScriptedNoiseServer { _ in Data() }
        let port = dead.port; dead.stop()
        #expect(NoiseHTTPTransport(timeout: 3).send(address: "127.0.0.1:\(port)", routerKey: Noise.newKey().publicKey.rawRepresentation, device: (Curve25519.Signing.PrivateKey(), "x"),
                                                    token: "t", method: "GET", path: "/minigw/v1/x", idempotencyKey: nil, body: nil, timeout: 3).status == 0)
    }

    @Test func aServerThatTricklesIsStoppedAtTheTotalDeadline() {
        // one byte every 0.2 s, an answer that would take minutes: an idle timeout would never fire, the total deadline does
        let srv = ScriptedNoiseServer(trickle: 0.2) { _ in Data(("HTTP/1.1 200 OK\r\nContent-Length: 100000\r\n\r\n" + String(repeating: "x", count: 100000)).utf8) }
        defer { srv.stop() }
        let start = Date()
        let a = send(srv, timeout: 1.5)
        #expect(a.status == 0)
        #expect(Date().timeIntervalSince(start) < 3, "waited \(Date().timeIntervalSince(start)) s")
    }

    @Test func theHostHasNoListenerToBorrow() throws {
        let all = try String(contentsOf: repo.appendingPathComponent("Sources/RouterCore/Upstream.swift"), encoding: .utf8)
        let src = all.split(separator: "\n").filter { !$0.trimmingCharacters(in: .whitespaces).hasPrefix("//") }.joined(separator: "\n")  // code, not the comments
        for word in ["NWListener", "NoiseProxy", "startEphemeral", "listen(", "bind("] { #expect(!src.contains(word), Comment(rawValue: word)) }
    }
}

/// The REAL minigw-core helper as the local gateway; the upstream is a fake transport (what a Noise connection would carry). The core
/// decides, the app host does the network and the credentials: the whole path of a push, over the callout frames.
@Suite(.serialized) struct UpstreamThroughTheHelper {
    func start(callouts: UpstreamHost?) async throws -> (CoreHelper, URL) {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("mgup-\(UUID().uuidString.prefix(8))", isDirectory: true).cleanedAtExit()
        let store = dir.appendingPathComponent("core")
        try CoreHelper.prepareStoreFolder(store)
        let bin = try await HelperBuild.shared.binary()
        let h = CoreHelper(binary: bin, store: store, ownerName: "tester", marks: MemoryMarks(), versions: 1...1, backoff: [0.05, 0.1, 0.2], requestTimeout: 15)
        h.callouts = callouts
        h.start()
        #expect(await h.waitReady())
        return (h, store)
    }
    func adm(_ h: CoreHelper, _ method: String, _ path: String, _ body: String = "") async -> CoreResponse {
        await h.call(context: .admin, request: CoreRequest(method: method, path: path, body: Data(body.utf8)))
    }
    func obj(_ r: CoreResponse) -> [String: Any] { (try? JSONSerialization.jsonObject(with: r.body) as? [String: Any]) ?? [:] }

    func fakeUpstream() -> FakeTransport {
        let t = FakeTransport()
        let next = NSLock(); nonisolated(unsafe) var n = 0
        t.handler = { c in
            switch (c.method, c.path) {
            case ("GET", "/minigw/v1/projects"): return UpstreamAnswer(status: 403, body: Data(#"{"error":{"message":"no project access: this device is not on the gateway's device list"}}"#.utf8))
            case ("GET", "/minigw/v1/projects/shop"): return UpstreamAnswer(status: 200, body: Data(#"{"id":7,"slug":"shop"}"#.utf8))
            case ("POST", "/minigw/v1/projects/7/kb/submissions"):
                next.lock(); n += 1; let id = n; next.unlock()
                return UpstreamAnswer(status: 201, body: Data(#"{"id":\#(id),"state":"pending"}"#.utf8))
            default: return UpstreamAnswer(status: 404, body: Data(#"{"error":{"message":"not found"}}"#.utf8))
            }
        }
        return t
    }

    @Test func addLinkDryRunPushAndPushAgainThroughTheCallouts() async throws {
        let secrets = MemorySecrets(), t = fakeUpstream()
        let own = Noise.newKey().publicKey.rawRepresentation
        let (h, store) = try await start(callouts: UpstreamHost(secrets: secrets, ownKey: { own }, ownPort: { 8765 }, transport: t, computerName: "mac"))
        defer { h.stop() }
        let up = Noise.newKey().publicKey.rawRepresentation
        let token = "SECRET-TOKEN#" + Noise.base64url(up)
        let added = await adm(h, "PUT", "/upstreams/hub", #"{"address":"hub.example","token":"\#(token)"}"#)
        #expect(added.status == 200, "\(String(decoding: added.body, as: UTF8.self))")
        let a = obj(added)
        #expect(a["device"] as? String == (secrets.all["upstream:hub"].flatMap { try? JSONSerialization.jsonObject(with: Data($0.utf8)) as? [String: Any] }?["device"] as? String).flatMap { seed in
            Noise.fromBase64url(seed).flatMap { try? Curve25519.Signing.PrivateKey(rawRepresentation: $0) }.map { Noise.base64url($0.publicKey.rawRepresentation) } })
        #expect((a["probe"] as? String)?.contains("device list") == true && a["where"] as? String == "the Keychain")
        #expect(!String(decoding: added.body, as: UTF8.self).contains("SECRET-TOKEN"))
        // a project with two notes, written as a member
        _ = await adm(h, "PUT", "/bindings/dev-A", #"{"user":1}"#)
        _ = await adm(h, "POST", "/projects", #"{"slug":"shop","name":"Shop"}"#)
        _ = await adm(h, "PUT", "/projects/1/members/1", #"{"role":"developer"}"#)
        for (i, c) in ["structure", "data"].enumerated() {
            let r = await h.call(context: CoreContext(kind: .member, device: "dev-A", approved: true), request: CoreRequest(method: "POST", path: "/minigw/v1/projects/1/notes",
                header: ["Idempotency-Key": ["k\(i)"]], body: Data(#"{"class":"\#(c)","title":"Note \#(i)","body":"text \#(i)"}"#.utf8)))
            #expect(r.status == 201)
        }
        #expect((await adm(h, "PUT", "/upstreams/hub/links/1", #"{"remote":"shop"}"#)).status == 200)
        let before = t.calls.count
        let dry = obj(await adm(h, "POST", "/upstream-push", #"{"project":1,"dryRun":true}"#))
        let dp = (dry["pushes"] as? [[String: Any]])?.first
        #expect(dp?["created"] as? Int == 2 && dp?["dryRun"] as? Bool == true)
        #expect(t.calls.dropFirst(before).allSatisfy { $0.method == "GET" }, "a dry run wrote something")
        let st = obj(await adm(h, "GET", "/upstreams"))
        #expect(((st["links"] as? [[String: Any]])?.first?["pending"] as? Int) == 2)
        let real = obj(await adm(h, "POST", "/upstream-push", #"{"project":1}"#))
        let rp = (real["pushes"] as? [[String: Any]])?.first
        #expect(rp?["created"] as? Int == 2 && rp?["refused"] as? Int == 0, "\(real)")
        #expect(t.calls.filter { $0.method == "POST" }.count == 2)
        let again = obj(await adm(h, "POST", "/upstream-push", #"{"project":1}"#))
        #expect(((again["pushes"] as? [[String: Any]])?.first?["skipped"] as? Int) == 2 && t.calls.filter { $0.method == "POST" }.count == 2)
        // every request of the helper carried the bound credentials of the store, to the address given at add
        #expect(t.calls.allSatisfy { $0.address == "hub.example:8765" && $0.token == "SECRET-TOKEN" && $0.key == up })
        // remove: links, receipts and the Keychain item are gone
        #expect((await adm(h, "DELETE", "/upstreams/hub")).status == 200 && secrets.all.isEmpty)
        // the secret was never written next to the core: not in its files
        h.stop()
        let files = FileManager.default.enumerator(at: store, includingPropertiesForKeys: nil)?.compactMap { $0 as? URL } ?? []
        for f in files where (try? f.resourceValues(forKeys: [.isRegularFileKey]).isRegularFile) == true {
            let d = (try? Data(contentsOf: f)) ?? Data()
            #expect(d.range(of: Data("SECRET-TOKEN".utf8)) == nil, "the token is in \(f.lastPathComponent)")
        }
    }

    @Test func aWriteLimitThatCannotBeWaitedOutEndsTheCallWithMore() async throws {
        let secrets = MemorySecrets(), t = fakeUpstream()
        let own = Noise.newKey().publicKey.rawRepresentation
        let (h, _) = try await start(callouts: UpstreamHost(secrets: secrets, ownKey: { own }, ownPort: { 8765 }, transport: t, computerName: "mac"))
        defer { h.stop() }
        _ = await adm(h, "PUT", "/upstreams/hub", #"{"address":"hub.example","token":"tok#\#(Noise.base64url(Noise.newKey().publicKey.rawRepresentation))"}"#)
        _ = await adm(h, "PUT", "/bindings/dev-A", #"{"user":1}"#)
        _ = await adm(h, "POST", "/projects", #"{"slug":"shop","name":"Shop"}"#)
        _ = await adm(h, "PUT", "/projects/1/members/1", #"{"role":"developer"}"#)
        _ = await h.call(context: CoreContext(kind: .member, device: "dev-A", approved: true), request: CoreRequest(method: "POST", path: "/minigw/v1/projects/1/notes",
            header: ["Idempotency-Key": ["k"]], body: Data(#"{"class":"data","title":"N","body":"t"}"#.utf8)))
        _ = await adm(h, "PUT", "/upstreams/hub/links/1", #"{"remote":"shop"}"#)
        let ok = t.handler
        t.handler = { c in c.method == "POST" ? UpstreamAnswer(status: 429, body: Data("{}".utf8), retryAfter: 90) : ok(c) }
        let r = obj(await adm(h, "POST", "/upstream-push", #"{"project":1}"#))
        let p = (r["pushes"] as? [[String: Any]])?.first
        #expect(p?["more"] as? Bool == true && p?["wait"] as? Int == 90 && p?["refused"] as? Int == 0, "\(r)")
    }

    /// F4: a slow upstream must not hold up the rest of the core. The upstream routes have their own connection to the helper (the lane), so a
    /// member read, a KB read and a device revocation are served at their normal speed while a push waits for the network.
    @Test func aSlowUpstreamNeverHoldsUpMemberReadsOrARevocation() async throws {
        let secrets = MemorySecrets(), t = fakeUpstream()
        let entered = DispatchSemaphore(value: 0), release = DispatchSemaphore(value: 0)
        let ok = t.handler
        t.handler = { c in
            if c.method == "POST" { entered.signal(); _ = release.wait(timeout: .now() + 20) }  // the upstream is slow
            return ok(c)
        }
        let own = Noise.newKey().publicKey.rawRepresentation
        let (h, _) = try await start(callouts: UpstreamHost(secrets: secrets, ownKey: { own }, ownPort: { 8765 }, transport: t, computerName: "mac"))
        defer { h.stop() }
        _ = await adm(h, "PUT", "/upstreams/hub", #"{"address":"hub.example","token":"tok#\#(Noise.base64url(Noise.newKey().publicKey.rawRepresentation))"}"#)
        _ = await adm(h, "PUT", "/bindings/dev-A", #"{"user":1}"#)
        _ = await adm(h, "POST", "/projects", #"{"slug":"shop","name":"Shop"}"#)
        _ = await adm(h, "PUT", "/projects/1/members/1", #"{"role":"developer"}"#)
        let member = CoreContext(kind: .member, device: "dev-A", approved: true)
        _ = await h.call(context: member, request: CoreRequest(method: "POST", path: "/minigw/v1/projects/1/notes", header: ["Idempotency-Key": ["k"]], body: Data(#"{"class":"data","title":"N","body":"t"}"#.utf8)))
        _ = await adm(h, "PUT", "/upstreams/hub/links/1", #"{"remote":"shop"}"#)
        let push = Task { await self.adm(h, "POST", "/upstream-push", #"{"project":1}"#) }
        var inCallout = false  // wait without blocking a thread of the cooperative pool: the push below needs it
        for _ in 0..<300 where !inCallout { inCallout = entered.wait(timeout: .now()) == .success; if !inCallout { try await Task.sleep(for: .milliseconds(50)) } }
        #expect(inCallout)  // the push is now waiting for the upstream, inside a callout
        let start = Date()
        let read = await h.call(context: member, request: CoreRequest(method: "GET", path: "/minigw/v1/projects/1/notes"))
        let search = await h.call(context: member, request: CoreRequest(method: "GET", path: "/minigw/v1/projects/1/kb/search", query: "q=t"))
        let revoked = await h.revoke("dev-X")
        let epoch = await h.epoch()
        #expect(read.status == 200 && search.status == 200 && revoked == .done && epoch != nil)
        #expect(Date().timeIntervalSince(start) < 3, "held up for \(Date().timeIntervalSince(start)) s")
        release.signal()
        let r = await push.value
        #expect(r.status == 200)
    }

    @Test func withoutAnAppThatAnswersCalloutsTheRoutesSay501() async throws {
        let (h, _) = try await start(callouts: nil)
        defer { h.stop() }
        let r = await adm(h, "PUT", "/upstreams/hub", #"{"address":"hub.example","token":"t#k"}"#)
        #expect(r.status == 501)
        #expect((await adm(h, "POST", "/upstream-push", #"{}"#)).status == 501)
    }
}
