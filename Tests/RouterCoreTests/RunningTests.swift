import CryptoKit
import Foundation
import Testing
@testable import RouterCore

private func deviceID() -> String { Noise.base64url(Curve25519.Signing.PrivateKey().publicKey.rawRepresentation) }

/// #131: the same cases as the Go gateway (client/testdata/running.json, step-marks.json; client/serve_running_test.go).
@Test func runningRegistrySharedWithGo() throws {
    let root = ClaudeCodeCases.root.appendingPathComponent("client/testdata/")
    let fx = try JSONSerialization.jsonObject(with: Data(contentsOf: root.appendingPathComponent("running.json"))) as! [String: Any]
    let base = Date(timeIntervalSince1970: 1_700_000_000)
    let reg = RunningRegistry()
    for r in fx["runs"] as! [[String: Any]] {  // oldest first: start() inserts at the front, as the Go test does
        let id = r["id"] as! String
        reg.start(id: id, owner: r["owner"] as! String, model: r["model"] as! String, folder: r["folder"] as! String,
                  at: base.addingTimeInterval(Double(r["startSec"] as! Int)))
        for s in r["steps"] as! [[String: Any]] { reg.step(id: id, text: s["text"] as! String, at: base.addingTimeInterval(Double(s["at"] as! Int))) }
    }
    let now = base.addingTimeInterval(Double(fx["nowSec"] as! Int))
    let expect = fx["expect"] as! [String: Any]
    #expect(!expect.isEmpty)
    for (owner, want) in expect {
        #expect(NSDictionary(dictionary: reg.reply(owner: owner, now: now)) == NSDictionary(dictionary: want as! [String: Any]), "owner \(owner)")
    }

    let sm = try JSONSerialization.jsonObject(with: Data(contentsOf: root.appendingPathComponent("step-marks.json"))) as! [String: Any]
    for c in sm["cases"] as! [[String: String]] {
        let step = RunningRegistry.relStep(c["step"]!, dir: c["dir"]!, scratch: c["scratch"]!)
        let sse = String(decoding: StreamTranslator.stepMark(step), as: UTF8.self)
        let data = sse.components(separatedBy: "data: ")[1].trimmingCharacters(in: .whitespacesAndNewlines)
        let text = (((try? JSONSerialization.jsonObject(with: Data(data.utf8))) as? [String: Any])?["delta"] as? [String: Any])?["text"] as? String
        #expect(text == c["mark"]!, "\(c["name"]!): \(text ?? "nil")")
    }
}

/// #131: GET /minigw/v1/running lists only the asking device's requests; no prompt; a device that is not allowed gets nothing.
@Test func runningRouteIsPerDevice() async throws {
    let a = deviceID(), b = deviceID(), stranger = deviceID()
    var c = RouterConfig()
    c.devices = [DeviceConfig(id: a, name: "A"), DeviceConfig(id: b, name: "B")]
    let router = FakeApp(c).router()
    router.running.start(id: "req_a", owner: a, model: "claude-codex", folder: "/home/a/proj")
    router.running.step(id: "req_a", text: "read_file x.go")
    router.running.start(id: "req_b", owner: b, model: "claude-copilot", folder: "/home/b/other")
    func ask(_ dev: String?, peer: String = "192.0.2.9", token: String = "tok") async -> FakeWriter {
        var q = request("GET", "/minigw/v1/running", headers: ["Authorization": "Bearer " + token])
        q.peer = peer
        q.device = dev.map { Noise.Device(id: $0, name: "x") }
        let w = FakeWriter()
        await router.handle(q, writer: w)
        return w
    }
    let mine = await ask(a)
    #expect(mine.status == 200 && mine.text.contains("claude-codex") && mine.text.contains("read_file x.go") && !mine.text.contains("claude-copilot") && !mine.text.contains("/home/b"))
    let theirs = await ask(b)
    #expect(theirs.status == 200 && theirs.text.contains("claude-copilot") && !theirs.text.contains("claude-codex"))
    #expect(await ask(stranger).status == 403)  // not on the list: refused before the route
    #expect(await ask(a, token: "wrong").status == 401)
    let anon = await ask(nil)  // no device proof from another computer: nothing
    #expect(anon.status != 200 || anon.text.contains(#""running":[]"#))
    router.running.finish(id: "req_a")
    #expect(await ask(a).text.contains(#""running":[]"#))
}

/// #131: steps of a Codex/Copilot run reach the stream as they happen; one before the head waits for it, so that an early error
/// is still a real HTTP status; the final text carries the answer after the marks.
@Test func replyWhenDoneStreamsSteps() async throws {
    let router = FakeApp(RouterConfig()).router()
    router.running.start(id: "req_s", owner: "local", model: "claude-codex", folder: "")
    let w = FakeWriter()
    let st = await router.replyWhenDone(body: ["model": "claude-codex", "stream": true], model: "claude-codex", rid: "req_s", writer: w, headDelay: .milliseconds(30)) {
        router.onActivity(.step(id: "req_s", text: "read_file src/a.go"))  // before the head
        try await Task.sleep(for: .milliseconds(300))
        router.onActivity(.step(id: "req_s", text: "grep x"))  // after it
        try await Task.sleep(for: .milliseconds(50))
        return ("answer", [])
    }
    #expect(st == 200)
    let t = w.text
    let i1 = t.range(of: "⏺ read_file src/a.go")?.lowerBound, i2 = t.range(of: "⏺ grep x")?.lowerBound, h = t.range(of: "message_start")?.lowerBound
    #expect(i1 != nil && i2 != nil && h != nil && h! < i1! && i1! < i2! && t.contains("answer"))

    let early = FakeWriter()
    router.running.start(id: "req_e", owner: "local", model: "claude-codex", folder: "")
    let est = await router.replyWhenDone(body: ["model": "claude-codex", "stream": true], model: "claude-codex", rid: "req_e", writer: early, headDelay: .seconds(30)) {
        router.onActivity(.step(id: "req_e", text: "read_file a"))
        throw Codex.Failure(message: "codex failed (exit 1)")
    }
    #expect(est == 502 && early.status == 502 && !early.text.contains("⏺"))
}
