import Foundation
import Testing
@testable import RouterCore

// The app's side of the project core: the REAL minigw-core process (built from client/cmd/minigw-core), a temp store, no
// network, nothing of the real app's preferences, Application Support or Keychain. Needs `go` (the project's build tool).

private let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
private let clientDir = repo.appendingPathComponent("client")

private func run(_ args: [String], in dir: URL, env: [String: String] = [:]) throws -> String {
    let p = Process()
    p.executableURL = URL(fileURLWithPath: "/usr/bin/env")
    p.arguments = args
    p.currentDirectoryURL = dir
    var e = ProcessInfo.processInfo.environment
    e["PATH"] = (e["PATH"] ?? "/usr/bin:/bin") + ":/opt/homebrew/bin:/usr/local/go/bin"
    e.merge(env) { $1 }
    p.environment = e
    let out = Pipe()
    p.standardOutput = out; p.standardError = out
    try p.run()
    let data = out.fileHandleForReading.readDataToEndOfFile()
    p.waitUntilExit()
    let text = String(decoding: data, as: UTF8.self)
    guard p.terminationStatus == 0 else { throw NSError(domain: "run", code: Int(p.terminationStatus), userInfo: [NSLocalizedDescriptionKey: args.joined(separator: " ") + ": " + text]) }
    return text
}

private func tempDir(_ name: String) -> URL {
    let u = FileManager.default.temporaryDirectory.appendingPathComponent("mgcore-\(name)-\(UUID().uuidString.prefix(8))", isDirectory: true)
    try? FileManager.default.createDirectory(at: u, withIntermediateDirectories: true)
    return u
}

/// The helper built once per test run into a temp folder (never into the worktree).
private let helperBinary: URL = {
    let out = tempDir("bin").appendingPathComponent("minigw-core")
    do { _ = try run(["go", "build", "-o", out.path, "./cmd/minigw-core"], in: clientDir, env: ["CGO_ENABLED": "0"]) } catch { fatalError("\(error)") }
    return out
}()

private actor MemoryMarks: CoreRevocationMarks {
    var items: [String] = []
    func list() -> [String] { items }
    func add(_ device: String) { if !items.contains(device) { items.append(device) } }
    func clear(_ devices: [String]) { items.removeAll { devices.contains($0) } }
}

private func helper(_ binary: URL? = helperBinary, store: URL = tempDir("store").appendingPathComponent("core"), marks: MemoryMarks = MemoryMarks(),
                    versions: ClosedRange<Int> = 1...1, backoff: [Double] = [0.05, 0.1, 0.2]) throws -> CoreHelper {
    try CoreHelper.prepareStoreFolder(store)
    return CoreHelper(binary: binary, store: store, ownerName: "tester", marks: marks, versions: versions, backoff: backoff, requestTimeout: 10)
}

private func fixture(_ name: String) throws -> Data { try Data(contentsOf: clientDir.appendingPathComponent("coreapi/testdata/" + name)) }

private func json(_ r: CoreResponse) -> [String: Any] { (try? JSONSerialization.jsonObject(with: r.body) as? [String: Any]) ?? [:] }

private func adm(_ h: CoreHelper, _ method: String, _ path: String, _ body: String = "") async -> CoreResponse {
    await h.call(context: .admin, request: CoreRequest(method: method, path: path, body: Data(body.utf8)))
}

@Suite(.serialized) struct CoreHelperTests {
    @Test func pathTestsMirrorGoThroughTheSharedFixture() throws {
        let rows = try JSONSerialization.jsonObject(with: fixture("paths.json")) as! [[String: Any]]
        #expect(rows.count > 20)
        for r in rows {
            let (kind, method, path, want) = (r["kind"] as! String, r["method"] as! String, r["path"] as! String, r["want"] as! Bool)
            let got = kind == "member" ? CoreRoutes.isMemberPath(path) : CoreRoutes.isAdminPath(method, path)
            #expect(got == want, "\(kind) \(method) \(path)")
        }
    }

    @Test func fixedBodiesAreTheOnesCoreapiAsserts() throws {
        #expect(CoreResponse.unavailable.body == (try fixture("unavailable.json")) && CoreResponse.unavailable.status == 503)
        #expect(CoreResponse.noProjectAccess.body == (try fixture("no_project_access.json")) && CoreResponse.noProjectAccess.status == 403)
    }

    @Test func memberRequestRoundTripThroughTheRealHelper() async throws {
        let h = try helper(); h.start(); defer { h.stop() }
        #expect(await h.waitReady())
        // ensure-owner ran before project routes: the owner is the OS account name we gave
        let users = json(await adm(h, "GET", "/users"))["users"] as? [[String: Any]]
        #expect(users?.count == 1 && users?[0]["name"] as? String == "tester" && users?[0]["owner"] as? Bool == true)
        #expect((await adm(h, "PUT", "/bindings/dev-A", #"{"user":1}"#)).status == 200)
        #expect((await adm(h, "POST", "/projects", #"{"slug":"fakture","name":"Fakture"}"#)).status == 200)
        #expect((await adm(h, "PUT", "/projects/1/members/1", #"{"role":"developer"}"#)).status == 200)
        let ask = { (dev: String, approved: Bool) in
            await h.call(context: CoreContext(kind: .member, device: dev, approved: approved), request: CoreRequest(method: "GET", path: "/minigw/v1/projects"))
        }
        let ok = await ask("dev-A", true)
        #expect(ok.status == 200 && ok.value("Content-Type") == "application/json")
        let list = try JSONSerialization.jsonObject(with: ok.body) as? [[String: Any]]
        #expect(list?.count == 1 && list?[0]["slug"] as? String == "fakture" && list?[0]["role"] as? String == "developer")
        let notApproved = await ask("dev-A", false), noDevice = await ask("", true)
        #expect(notApproved == CoreResponse.noProjectAccess && noDevice == CoreResponse.noProjectAccess)  // the helper's own bytes equal the fixture
        let nope = await h.call(context: CoreContext(kind: .member, device: "dev-A", approved: true), request: CoreRequest(method: "GET", path: "/minigw/v1/projects/nope"))
        #expect(nope.status == 404)
        let epoch = await h.epoch()
        #expect(epoch != nil)
    }

    @Test func routerForwardsMemberRoutesWithTheTunnelDeviceAndTheOrderOfGo() async throws {
        let saved = Router.agentPeer; Router.agentPeer = { _, _ in false }; defer { Router.agentPeer = saved }
        let h = try helper(); h.start(); defer { h.stop() }
        #expect(await h.waitReady())
        _ = await adm(h, "PUT", "/bindings/dev-A", #"{"user":1}"#)
        _ = await adm(h, "POST", "/projects", #"{"slug":"fakture","name":"Fakture"}"#)
        _ = await adm(h, "PUT", "/projects/1/members/1", #"{"role":"developer"}"#)
        var cfg = RouterConfig()
        cfg.devices = [DeviceConfig(id: "dev-A", name: "A")]
        let router = Router(config: cfg, token: "tok", secrets: FakeSecrets(values: [:]), core: h)
        func ask(_ target: String = "/minigw/v1/projects", token: String? = "tok", device: String? = "dev-A", peer: String = "127.0.0.1", method: String = "GET",
                 body: String = "", extra: [String: String] = [:]) async -> FakeWriter {
            var q = request(method, target, headers: (token.map { ["Authorization": "Bearer " + $0] } ?? [:]).merging(extra) { $1 }, body: body)
            q.peer = peer
            q.device = device.map { Noise.Device(id: $0, name: "x") }
            let w = FakeWriter()
            await router.handle(q, writer: w)
            return w
        }
        let good = await ask()
        #expect(good.status == 200 && good.text.contains(#""slug":"fakture""#) && good.text.contains("request-id: req_"))
        // before the generic refusals, as in Go: no token is the fixed 403 (the models' 401), an unknown device on another
        // computer is the fixed 403 from the core (the models' device refusal text), and plain HTTP without a device has none
        let noToken = await ask(token: nil)
        #expect(noToken.status == 403 && noToken.text.hasSuffix(String(decoding: CoreResponse.noProjectAccess.body, as: UTF8.self)))
        let stranger = await ask(device: "dev-X", peer: "192.0.2.5")
        #expect(stranger.status == 403 && stranger.text.contains("no_project_access") && !stranger.text.contains("Settings"))
        let noDevice = await ask(device: nil, peer: "192.0.2.5")
        #expect(noDevice.text.contains("no_project_access"))
        // the loopback exception of the model routes does not apply: a device that is not on the approved list gets nothing
        let unlisted = await ask(device: "dev-B"), thisMac = await ask(device: nil)
        #expect(unlisted.text.contains("no_project_access") && thisMac.text.contains("no_project_access"))  // this Mac without a device: no exception
        // an agent of this gateway may not use the routes
        Router.agentPeer = { _, _ in true }
        let agent = await ask()
        #expect(agent.text.contains("no_project_access"))
        Router.agentPeer = { _, _ in false }
        // the path is the decoded one, the query and the Idempotency-Key travel, the body is bounded
        let encoded = await ask("/minigw/v1/projects/fak%74ure"), withQuery = await ask("/minigw/v1/projects?archived=1")
        #expect(encoded.text.contains(#""slug":"fakture""#) && withQuery.status == 200)
        let big = await ask("/minigw/v1/projects/1/notes", method: "POST", body: String(repeating: "x", count: CoreHelper.maxBody + 10), extra: ["Idempotency-Key": "k1"])
        #expect(big.status == 413 && big.text.contains("too_large"))
        // the generic checks still apply to the model routes
        let models = await ask("/v1/models", token: nil)
        #expect(models.status == 401)
    }

    @Test func adminRoutesOfTheCoreAreForwardedAfterAdminAuthentication() async throws {
        let h = try helper(); h.start(); defer { h.stop() }
        #expect(await h.waitReady())
        let app = FakeApp()
        func router() -> Router {
            Router(config: app.config, token: "tok", secrets: FakeSecrets(values: [:]),
                   admin: AdminHooks(secret: adminSecret, connect: "t", version: "9", installed: { false }, commit: { f, _ in var c = app.config; try f(&c); app.config = c; return c }, rotateToken: { "x" }), core: h)
        }
        let users = await admin(router(), "GET", "/users"), noAuth = await admin(router(), "GET", "/users", auth: nil)
        let remote = await admin(router(), "GET", "/users", peer: "192.168.1.9"), status = await admin(router(), "GET", "/status")
        #expect(users.status == 200 && users.text.contains(#""name":"tester""#) && users.text.contains("no-store"))
        #expect(noAuth.status == 401 && remote.status == 403)  // authentication first, unchanged
        #expect(status.status == 200)                           // not a core route: the gateway's own
        let down = Router(config: app.config, token: "tok", secrets: FakeSecrets(values: [:]),
                          admin: AdminHooks(secret: adminSecret, connect: "t", version: "9", installed: { false }, commit: { _, _ in app.config }, rotateToken: { "x" }), core: nil)
        let r = await admin(down, "GET", "/users")
        #expect(r.status == 503 && r.text.contains("not running"))
    }

    @Test func helperMissingIsA503AndNothingCrashes() async throws {
        let h = try helper(URL(fileURLWithPath: "/nonexistent/minigw-core")); h.start(); defer { h.stop() }
        try await Task.sleep(nanoseconds: 400_000_000)
        #expect(h.isReady == false && h.pid == nil && h.restarts >= 2)  // keeps trying with a backoff
        let r = await h.call(context: .admin, request: CoreRequest(method: "GET", path: "/users"))
        #expect(r == .unavailable)
        let router = Router(config: RouterConfig(), token: "tok", secrets: FakeSecrets(values: [:]), core: h)
        var q = request("GET", "/minigw/v1/projects", headers: ["Authorization": "Bearer tok"]); q.device = Noise.Device(id: "d", name: "d")
        let w = FakeWriter(); await router.handle(q, writer: w)
        #expect(w.status == 503 && w.text.contains("core_unavailable"))
        let none = Router(config: RouterConfig(), token: "tok", secrets: FakeSecrets(values: [:]))  // no helper at all
        let w2 = FakeWriter(); await none.handle(q, writer: w2)
        #expect(w2.status == 503)
    }

    @Test func helperKilledMidRequestGives503ThenRecovers() async throws {
        let h = try helper(); h.start(); defer { h.stop() }
        #expect(await h.waitReady())
        let first = try #require(h.pid)
        kill(first, SIGSTOP)  // frozen: the request below waits for an answer that does not come
        async let answer = h.call(context: .admin, request: CoreRequest(method: "GET", path: "/users"))
        try await Task.sleep(nanoseconds: 300_000_000)
        kill(first, SIGKILL)  // our own child dies mid-request
        let dead = await answer
        let recovered = await h.waitReady()
        #expect(dead == .unavailable && recovered)  // the supervisor started a new one
        #expect(h.pid != nil && h.pid != first && h.restarts >= 1)
        let again = await adm(h, "GET", "/users")
        #expect(again.status == 200 && String(decoding: again.body, as: UTF8.self).contains("tester"))  // same store: the owner is still there
    }

    @Test func versionMismatchRefusesForGoodWithoutARestartLoop() async throws {
        let h = try helper(versions: 2...3); h.start(); defer { h.stop() }
        try await Task.sleep(nanoseconds: 1_200_000_000)
        guard case .versionMismatch(let why)? = h.fatalError else { Issue.record("fatal: \(String(describing: h.fatalError))"); return }
        #expect(why.contains("supports protocol 1..1") && h.restarts == 0 && h.isReady == false)  // one start, no loop
        let r = await h.call(context: .admin, request: CoreRequest(method: "GET", path: "/users"))
        #expect(r == .unavailable)
    }

    @Test func deviceRemovedWhileTheHelperIsDownIsReconciledAtTheNextStart() async throws {
        let store = tempDir("store").appendingPathComponent("core"), marks = MemoryMarks()
        let a = try helper(store: store, marks: marks); a.start()
        #expect(await a.waitReady())
        let before = try #require(await a.epoch())
        a.stop()
        // the helper is down: the removal is durable (the mark) and says so
        let down = try helper(URL(fileURLWithPath: "/nonexistent/minigw-core"), store: store, marks: marks); down.start()
        let deferred = await down.revoke("dev-gone"), pending = await marks.list()
        #expect(deferred == .deferred && pending == ["dev-gone"])
        down.stop()
        // the next start sends reconcile before project routes are served
        let c = try helper(store: store, marks: marks); c.start(); defer { c.stop() }
        #expect(await c.waitReady())
        let left = await marks.list(), epoch1 = await c.epoch()
        #expect(left.isEmpty && epoch1 == before + 1)
        // with the helper up, a removal raises the epoch before revoke returns
        let done = await c.revoke("dev-gone-2"), epoch2 = await c.epoch(), left2 = await marks.list()
        #expect(done == .done && epoch2 == before + 2 && left2.isEmpty)
    }

    @Test func adminDeleteDeviceRevokesBeforeItAnswersAndKeepsTheMarkWhenTheHelperIsDown() async throws {
        let saved = Router.agentPeer; Router.agentPeer = { _, _ in false }; defer { Router.agentPeer = saved }
        let marks = MemoryMarks()
        let h = try helper(marks: marks); h.start(); defer { h.stop() }
        #expect(await h.waitReady())
        let id = String(repeating: "A", count: 43)
        var c = RouterConfig(); c.devices = [DeviceConfig(id: id, name: "Air")]
        let app = FakeApp(c)
        func router(_ core: CoreHelper) -> Router {
            Router(config: app.config, token: "tok", secrets: FakeSecrets(values: [:]),
                   admin: AdminHooks(secret: adminSecret, connect: "t", version: "9", installed: { false },
                                     commit: { f, _ in var n = app.config; try f(&n); app.config = RouterConfig.markingRemovals(n, old: app.config).config; return n }, rotateToken: { "x" }),
                   core: core)
        }
        let e0 = try #require(await h.epoch())
        let r = await admin(router(h), "DELETE", "/devices/" + id)
        #expect(r.status == 200)
        let e1 = await h.epoch(), m1 = await marks.list()
        #expect(e1 == e0 + 1 && m1.isEmpty)  // raised before the answer
        #expect(app.config.devices.isEmpty && app.config.pendingRevocations.isEmpty == false)  // the save carried the mark; the helper cleared it in its own marks
        // helper down: still 200, the mark stays
        let id2 = String(repeating: "B", count: 43)
        app.config.devices = [DeviceConfig(id: id2, name: "PC")]
        let down = try helper(URL(fileURLWithPath: "/nonexistent/minigw-core"), marks: marks); down.start(); defer { down.stop() }
        let r2 = await admin(router(down), "DELETE", "/devices/" + id2)
        let m2 = await marks.list()
        #expect(r2.status == 200 && m2 == [id2])
    }

    @Test func removalsGetADurableMarkInTheSameSaveAsTheRemoval() {
        var old = RouterConfig(); old.devices = [DeviceConfig(id: "A", name: "a"), DeviceConfig(id: "B", name: "b")]; old.pendingRevocations = ["Z"]
        var new = old; new.devices = [DeviceConfig(id: "B", name: "b")]; new.pendingRevocations = []  // a stale copy's marks are not trusted
        let r = RouterConfig.markingRemovals(new, old: old)
        #expect(r.removed == ["A"] && r.config.pendingRevocations == ["Z", "A"] && r.config.devices.map(\.id) == ["B"])
        let again = RouterConfig.markingRemovals(r.config, old: r.config)
        #expect(again.removed.isEmpty && again.config.pendingRevocations == ["Z", "A"])
        let back = try? JSONDecoder().decode(RouterConfig.self, from: JSONEncoder().encode(r.config))
        #expect(back?.pendingRevocations == ["Z", "A"])
    }

    @Test func tableOfRequestsGivesByteIdenticalBodiesThroughSwiftAndTheHelper() async throws {
        let out = tempDir("parity")
        _ = try run(["go", "test", "-count=1", "./coreapi", "-run", "TestParityTableIsServedInProcess"], in: clientDir, env: ["MG_PARITY_OUT": out.path])
        let rows = try JSONSerialization.jsonObject(with: Data(contentsOf: out.appendingPathComponent("cases.json"))) as! [[String: Any]]
        let h = try helper(store: out.appendingPathComponent("store")); h.start(); defer { h.stop() }
        #expect(await h.waitReady())
        var checked = 0, skippedRun = 0
        var seen = Set<Int>()
        for row in rows {
            let cx = row["context"] as! [String: Any]
            let kind = cx["kind"] as! String
            guard kind != "run" else { skippedRun += 1; continue }  // the run port's context is a later task
            let context = CoreContext(kind: CoreContext.Kind(rawValue: kind)!, device: cx["device"] as? String ?? "", approved: cx["approved"] as? Bool ?? false,
                                      agentPeer: cx["agentPeer"] as? Bool ?? false)
            let request = try #require(CoreRequest(json: row["request"] as! [String: Any]))
            let want = try #require(CoreResponse(json: row["response"] as! [String: Any]))
            let got = await h.call(context: context, request: request)
            #expect(got.status == want.status && got.header == want.header && got.body == want.body, "\(row["name"] as! String)")
            seen.insert(want.status); checked += 1
        }
        #expect(checked >= 30 && skippedRun >= 5 && rows.count == checked + skippedRun)
        #expect(seen.isSuperset(of: [200, 400, 403, 404, 413]))  // incl. a no-device 403 and a 404
        #expect(rows.contains { ($0["name"] as! String) == "member: no device" })
    }
}
