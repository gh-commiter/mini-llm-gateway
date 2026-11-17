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
            opts.remoteProject = o["remoteProject"] as? String ?? ""; opts.remoteTeam = o["remoteTeam"] as? String ?? ""; opts.classes = o["class"] as? String ?? ""
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

    @Test func aLongPushAsksAgainWhileTheHelperSaysMore() throws {
        nonisolated(unsafe) var calls = 0
        nonisolated(unsafe) var waits: [Double] = []
        nonisolated(unsafe) var outText = ""
        let proj = #"{"projects":[{"ID":3,"Slug":"shop","Name":"Shop","Archived":false}]}"#
        let cmd = AdminCommands(call: { method, path, _ in
            if path == "/projects" { return (200, Data(proj.utf8)) }
            calls += 1
            let more = calls < 3
            return (200, Data(#"{"pushes":[{"label":"shop","upstream":"hub","remote":"shop","lines":["create \#(calls) n"],"created":1,"skipped":0,"replaced":0,"refused":0,"dryRun":false\#(more ? #","more":true,"wait":7,"stopped":"the upstream's write limit: push again in 7 seconds""# : "")}]}"#.utf8))
        }, out: { outText += $0 })
        cmd.pause = { waits.append($0) }
        try cmd.run(["upstream", "push", "shop"], AdminOpts())
        #expect(calls == 3 && waits == [7, 7])
        #expect(outText.components(separatedBy: "create ").count == 4)
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
    struct Call { var address: String; var key: Data; var device: String; var token: String; var method: String; var path: String; var body: Data? }
    private let lock = NSLock()
    private var _calls: [Call] = []
    var handler: @Sendable (Call) -> UpstreamAnswer = { _ in UpstreamAnswer(status: 200, body: Data("{}".utf8)) }
    var calls: [Call] { lock.lock(); defer { lock.unlock() }; return _calls }
    func send(address: String, routerKey: Data, device: (key: Curve25519.Signing.PrivateKey, name: String), token: String, method: String, path: String,
              headers: [String: String], body: Data?) -> UpstreamAnswer {
        let c = Call(address: address, key: routerKey, device: Noise.base64url(device.key.publicKey.rawRepresentation), token: token, method: method, path: path, body: body)
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
        let r = h.callout(["op": "connect", "upstream": "hub", "address": "hub.example:8765", "token": line()])
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
            let r = h.callout(["op": "connect", "upstream": "me", "address": address, "token": token])
            #expect((r["error"] as? String)?.contains("this same gateway") == true, "\(why) \(address)")
        }
        // another port on this computer is another gateway
        #expect(h.callout(["op": "connect", "upstream": "other", "address": "127.0.0.1:9000", "token": line()])["error"] == nil)
        for bad in ["just-a-token", "#", "tok#", "#key", "tok#short"] {
            #expect(h.callout(["op": "connect", "upstream": "bad", "address": "hub.example:8765", "token": bad])["error"] != nil, Comment(rawValue: bad))
        }
        #expect(s.all.keys.sorted() == ["upstream:other"])
    }

    @Test func doSendsToTheConfiguredAddressAsTheGatewaysOwnDeviceAndPassesTheAnswerAsData() throws {
        let s = MemorySecrets(), t = FakeTransport()
        let h = host(s, t)
        let key = Noise.newKey().publicKey.rawRepresentation
        let device = try #require(h.callout(["op": "connect", "upstream": "hub", "address": "hub.example:9000", "token": line(key)])["device"] as? String)
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

/// The fetch behind the real transport, against a plain loopback server (the Noise proxy in front of it is the existing one).
@Suite(.serialized) struct UpstreamFetchTests {
    @Test func aRedirectIsTheAnswerAndIsNotFollowed() {
        let other = ProjectFakeGateway { _ in .init(status: 200, body: "followed") }
        defer { other.stop() }
        let gw = ProjectFakeGateway { _ in .init(status: 302, headers: ["Location": other.url + "/elsewhere"]) }
        defer { gw.stop() }
        let a = CappedFetch.send(url: gw.url + "/x", method: "GET", headers: [:], body: nil, timeout: 10, cap: 1000)
        #expect(a.status == 302)
        #expect(other.requests.isEmpty, "the redirect was followed")
    }

    @Test func anAnswerAboveTheCapIsNoAnswer() {
        let gw = ProjectFakeGateway { _ in .init(status: 200, body: String(repeating: "a", count: 5000)) }
        defer { gw.stop() }
        #expect(CappedFetch.send(url: gw.url + "/x", method: "GET", headers: [:], body: nil, timeout: 10, cap: 1000).status == 0)
        let ok = CappedFetch.send(url: gw.url + "/x", method: "GET", headers: [:], body: nil, timeout: 10, cap: 5000)
        #expect(ok.status == 200 && ok.body.count == 5000)
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

    @Test func withoutAnAppThatAnswersCalloutsTheRoutesSay501() async throws {
        let (h, _) = try await start(callouts: nil)
        defer { h.stop() }
        let r = await adm(h, "PUT", "/upstreams/hub", #"{"address":"hub.example","token":"t#k"}"#)
        #expect(r.status == 501)
        #expect((await adm(h, "POST", "/upstream-push", #"{}"#)).status == 501)
    }
}
