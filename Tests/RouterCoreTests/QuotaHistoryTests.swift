import Foundation
import Testing
@testable import RouterCore

private let historyRepo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
private func historyFixture() throws -> [String: Any] {
    try JSONSerialization.jsonObject(with: Data(contentsOf: historyRepo.appendingPathComponent("client/testdata/quota-history.json"))) as! [String: Any]
}
private final class ProbeRecords: @unchecked Sendable {
    private let lock = NSLock()
    private var items: [[String: Any]] = []
    func add(_ p: [String: Any], _ at: Date) { lock.withLock { items.append(p) } }
    var all: [[String: Any]] { lock.withLock { items } }
}

@Suite struct QuotaHistoryTests {
    @Test func codexFinishedRunAndCachedReport() async throws {
        let home = FileManager.default.temporaryDirectory.appendingPathComponent("quota-history-codex-" + UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: home) }
        let day = home.appendingPathComponent("sessions/2026/11/14")
        try FileManager.default.createDirectory(at: day, withIntermediateDirectories: true)
        try Data((#"{"payload":{"rate_limits":{"primary":{"used_percent":0,"window_minutes":300,"resets_at":4102444800},"secondary":{"used_percent":82,"window_minutes":10080,"resets_at":4102444800}}}}"# + "\n").utf8)
            .write(to: day.appendingPathComponent("rollout-2026-11-14T10-00-00-history-thread.jsonl"))
        let records = ProbeRecords(), engine = QuotaEngine()
        await engine.recordCodex(home: home.path, thread: "history-thread", record: records.add)
        #expect(records.all.count == 1)
        let values = records.all.first?["values"] as? [[String: Any]] ?? []
        #expect(values.count == 2 && values[0]["value"] as? Double == 0 && values[1]["value"] as? Double == 82)
        #expect(values.allSatisfy { $0["resetsAt"] as? String == "2100-01-01T00:00:00Z" })
        let inputs = QuotaInputs(claudeProbe: false, claudeEnabled: false, providers: [], key: { _ in nil }, tmpParent: home)
        _ = await engine.report(inputs, fetch: false, record: records.add)
        #expect(records.all.count == 1)
    }

    @Test func sharedHistoryTextAndCommands() throws {
        let fx = try historyFixture(), data = try JSONSerialization.data(withJSONObject: fx["history"]!)
        #expect(QuotaHistory.text(data) == fx["text"] as? String)
        for json in [false, true] {
            var paths: [String] = [], output = ""
            let cmd = AdminCommands(call: { method, path, body in
                paths.append(method + " " + path)
                return (200, data)
            }, out: { output += $0 })
            try cmd.run(["quota"], AdminOpts(provider: "codex", json: json, history: true))
            #expect(paths == ["GET /quota/history?days=7&provider=codex"])
            if json {
                #expect(try (JSONSerialization.jsonObject(with: Data(output.utf8)) as AnyObject).isEqual(fx["history"]!))
            } else { #expect(output == fx["text"] as? String) }
        }
    }

    @Test func sharedWirePreservesStaleObservationAndReset() throws {
        let fx = try historyFixture(), wire = fx["wire"] as! [String: Any]
        let value = (wire["values"] as! [[String: Any]])[0]
        let v = QuotaValue(name: value["name"] as! String, unit: value["unit"] as! String, value: value["value"] as? Double,
                           source: value["source"] as! String, observedAt: QuotaTime.parse(value["observedAt"] as! String),
                           resetsAt: QuotaTime.parse(value["resetsAt"] as! String), stale: true)
        var got = QuotaEngine.probeJSON(provider: "codex",
                                        now: QuotaTime.parse(fx["now"] as! String)!, vals: [v], kind: "")
        got["id"] = wire["id"]
        #expect((got as NSDictionary).isEqual(to: wire))
    }

    @Test func cachedReadsOptOutBackoffAndAccountChanges() async throws {
        let records = ProbeRecords(), now = Date()
        let engine = QuotaEngine(runUsage: { _, _ in throw QuotaError("SECRET raw error") }, http: { _ in
            (200, Data(#"{"info":{"spend":3.5}}"#.utf8))
        })
        var key = "SECRET1"
        func inputs(_ probe: Bool = false) -> QuotaInputs {
            QuotaInputs(claudeProbe: probe, claudeExe: "/fake/claude", claudeEnabled: true,
                        providers: [.init(id: "remote-id", name: "LiteLLM", baseURL: "https://example.test", enabled: true)],
                        key: { _ in key }, tmpParent: FileManager.default.temporaryDirectory)
        }
        _ = await engine.report(inputs(), now: now, fetch: false, record: records.add)
        #expect(records.all.isEmpty)
        await withTaskGroup(of: Void.self) { group in
            for _ in 0..<8 { group.addTask { _ = await engine.report(inputs(), now: now, record: records.add) } }
        }
        #expect(records.all.count == 1)
        _ = await engine.report(inputs(), now: now.addingTimeInterval(5), record: records.add)
        #expect(records.all.count == 1)
        _ = await engine.report(inputs(), now: now.addingTimeInterval(700), record: records.add)
        #expect(records.all.count == 2)
        #expect(records.all[0]["rescope"] == nil && records.all[1]["rescope"] == nil)
        key = "SECRET2"
        _ = await engine.report(inputs(true), now: now.addingTimeInterval(710), record: records.add)
        _ = await engine.report(inputs(true), now: now.addingTimeInterval(715), record: records.add)
        #expect(records.all.count == 4)
        let remote = records.all.filter { $0["provider"] as? String == "remote-id" }
        #expect(remote.count == 3 && remote[0]["rescope"] == nil && remote[1]["rescope"] == nil && remote[2]["rescope"] as? Bool == true)
        let failed = records.all.filter { $0["status"] as? String == "error" }
        #expect(failed.count == 1 && failed[0]["errorKind"] as? String == "probe")
        let encoded = String(decoding: try JSONSerialization.data(withJSONObject: records.all), as: UTF8.self)
        #expect(!encoded.contains("SECRET"))
        await engine.recordCodex(home: FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString).path, thread: "missing", record: records.add)
        #expect(records.all.last?["errorKind"] as? String == "unavailable")
    }

    @Test func helperReplayAndQuotaLossStaySeparateFromUsage() async throws {
        let base = FileManager.default.temporaryDirectory.appendingPathComponent("quota-history-" + UUID().uuidString)
        let store = base.appendingPathComponent("core")
        try CoreHelper.prepareStoreFolder(store)
        let helper = CoreHelper(binary: try await HelperBuild.shared.binary(), store: store, marks: MemoryMarks())
        helper.start(); defer { helper.stop(); try? FileManager.default.removeItem(at: base) }
        #expect(await helper.waitReady(timeout: 20))
        let now = Date(), journalFile = base.appendingPathComponent("quota-lost.json")
        let down = CoreHelper(binary: nil, store: store, marks: MemoryMarks())
        let sender = UsageSender(core: down, limit: 2, journal: UsageGapJournal(file: journalFile), quota: true)
        var probe = QuotaEngine.probeJSON(provider: "codex", now: now,
                                          vals: [QuotaValue(name: "weekly", unit: "percent", value: 82, source: "codex-session-log", observedAt: now)], kind: "")
        for i in 0..<3 { probe["id"] = "qp_overflow\(i)"; sender.probe(probe, at: now) }
        sender.lostWithTheQueue()
        let restored = UsageSender(core: helper, journal: UsageGapJournal(file: journalFile), quota: true)
        #expect(await restored.flush())
        probe["id"] = "qp_replayed"
        for _ in 0..<2 { let r = await helper.internalCall("quota-probe", ["probe": probe]); #expect(r.status == 202) }
        var answer: [String: Any] = [:]
        for _ in 0..<100 {
            let r = await helper.call(context: .admin, request: CoreRequest(method: "GET", path: "/quota/history"))
            answer = (try? JSONSerialization.jsonObject(with: r.body)) as? [String: Any] ?? [:]
            if (answer["probes"] as? [Any])?.count == 1 { break }
            try await Task.sleep(for: .milliseconds(20))
        }
        #expect((answer["probes"] as? [Any])?.count == 1 && (answer["gaps"] as? [Any])?.count == 1)  // the overflow drop and the queue loss merge into one unsent gap, as for usage events
        #expect(!String(describing: answer).contains("accountScope"))
        let usage = await helper.call(context: .admin, request: CoreRequest(method: "GET", path: "/usage/events"))
        let u = try JSONSerialization.jsonObject(with: usage.body) as! [String: Any]
        #expect((u["gaps"] as? [Any])?.isEmpty == true)
        // A permanently refused probe is a quota gap, not a silent loss.
        probe["id"] = "qp_refused"
        probe["status"] = "error"
        probe["errorKind"] = "raw error is forbidden"
        probe["values"] = []
        restored.probe(probe, at: now)
        #expect(await restored.flush())
        let refused = await helper.call(context: .admin, request: CoreRequest(method: "GET", path: "/quota/history"))
        let q = try JSONSerialization.jsonObject(with: refused.body) as! [String: Any]
        #expect((q["gaps"] as? [Any])?.count == 2)
    }
}

@Suite(.serialized) struct QuotaHistoryAuthorization {
    @Test func sameOwnerProofAsUsageEvents() async {
        let router = FakeApp(RouterConfig()).router()
        #expect(await admin(router, "GET", "/quota/history", auth: nil).status == 401)
        #expect(await admin(router, "GET", "/quota/history").status == 503)
    }
}
