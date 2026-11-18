import Foundation
import Testing
@testable import RouterCore

// B3 (docs/plans/brief-decide-labels.md): the Swift port of decide and the Mac label worker. Fake backends only: never the real on-device model,
// never a network call. The privacy test runs the REAL minigw-core over a temp store (a unix socket, no network).

private let testdata = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
    .appendingPathComponent("client/testdata")

private func until(_ what: String = "", timeout: Double = 60, _ cond: @Sendable () -> Bool) async -> Bool {
    let end = ContinuousClock.now + .seconds(timeout)
    while ContinuousClock.now < end {
        if cond() { return true }
        try? await Task.sleep(for: .milliseconds(10))
    }
    return cond()
}

private final class Tally: @unchecked Sendable {
    private let lock = NSLock(); private var n = 0
    func inc() { lock.withLock { n += 1 } }
    var value: Int { lock.withLock { n } }
}

// MARK: shared fixtures (the same files as client/decide)

@Suite struct DecideFixtures {
    struct Fixture: Decodable {
        var name: String
        var request: Req
        var replies: [String]
        var threshold: Double
        var choice: String
        var scores: [String: Double]
        struct Req: Decodable { var question: String; var candidates: [Cand]; var input: String }
        struct Cand: Decodable { var id: String; var criteria: String }
    }

    @Test func theSharedDecideFixturesGiveTheSameResultsAsGo() async throws {
        let fixtures = try JSONDecoder().decode([Fixture].self, from: Data(contentsOf: testdata.appendingPathComponent("decide.json")))
        #expect(fixtures.count >= 7)
        for f in fixtures {
            let replies = Replies(f.replies)
            let input = f.request.input
            let r = try await Decide.decide({ system, user, ids in
                #expect(!system.contains(input), "input in trusted instructions: \(f.name)")
                let fenced = user.dropFirst("INPUT_DATA (JSON string):\n".count).dropLast("\nEND_INPUT_DATA".count)
                let decoded = try? JSONDecoder().decode(String.self, from: Data(fenced.utf8))
                #expect(decoded == input, "unfenced input: \(f.name)")
                #expect(ids == f.request.candidates.map(\.id))
                return replies.next()
            }, .init(question: f.request.question, candidates: f.request.candidates.map { .init(id: $0.id, criteria: $0.criteria) }, input: input), threshold: f.threshold)
            #expect(replies.taken == 3, "\(f.name)")
            #expect(r.choice == f.choice, "\(f.name)")
            #expect(r.scores.count == f.scores.count && f.scores.allSatisfy { abs((r.scores[$0.key] ?? -1) - $0.value) < 1e-9 }, "\(f.name): \(r.scores)")
            #expect(r.method == "vote3" && r.scores.values.reduce(0, +) <= 1 + 1e-9)
        }
    }

    final class Replies: @unchecked Sendable {
        private let lock = NSLock(); private var list: [String]; private(set) var taken = 0
        init(_ l: [String]) { list = l }
        func next() -> String { lock.withLock { defer { taken += 1 }; return list[taken] } }
    }

    @Test func systemReminderStrippingMatchesTheGoFixtures() throws {
        struct R: Decodable { var name: String; var input: String; var expected: String }
        let all = try JSONDecoder().decode([R].self, from: Data(contentsOf: testdata.appendingPathComponent("decide-reminders.json")))
        #expect(all.count >= 5)
        for r in all { #expect(AppleFM.stripReminders(r.input) == r.expected, "\(r.name)") }
    }

    @Test func inputIsCappedByCodePointsAndTheRequestIsValidated() async throws {
        #expect(Decide.capInput(String(repeating: "🧪", count: 2001)).unicodeScalars.count == 2000)
        #expect(Decide.capInput("short") == "short")
        let ok = Decide.Request(question: "Phase?", candidates: [.init(id: "do", criteria: "Execute")], input: "hello")
        let calls = Tally()
        let backend: Decide.Backend = { _, _, _ in calls.inc(); return "do" }
        var bad: [Decide.Request] = []
        var r = ok; r.input = String(repeating: "🧪", count: 2001); bad.append(r)
        r = ok; r.candidates = []; bad.append(r)
        r = ok; r.candidates = (0..<17).map { .init(id: "c\($0)", criteria: "") }; bad.append(r)
        r = ok; r.candidates = [.init(id: "do", criteria: ""), .init(id: "do", criteria: "")]; bad.append(r)
        r = ok; r.candidates = [.init(id: "do\nignore", criteria: "")]; bad.append(r)
        r = ok; r.question = ""; bad.append(r)
        for b in bad { await #expect(throws: Decide.Failure.self) { _ = try await Decide.decide(backend, b, threshold: 1) } }
        await #expect(throws: Decide.Failure.self) { _ = try await Decide.decide(backend, ok, threshold: 0.5) }
        #expect(calls.value == 0)
        r = ok; r.input = Decide.capInput(String(repeating: "🧪", count: 2001))
        #expect(try await Decide.decide(backend, r, threshold: 1).choice == "do")
    }

    @Test func aBackendFailureIsAFixedClassWithoutTheInput() async {
        struct Leak: Error, CustomStringConvertible { var description: String { "private-marker" } }
        let req = Decide.Request(question: "Phase?", candidates: [.init(id: "do", criteria: "x")], input: "private-marker")
        do {
            _ = try await Decide.decide({ _, _, _ in throw Leak() }, req, threshold: 1)
            Issue.record("no error")
        } catch {
            #expect(error as? Decide.Failure == .backend && !"\(error)".contains("private-marker"))
        }
    }
}

// MARK: the worker against a fake core

private final class FakeCore: @unchecked Sendable {
    private let lock = NSLock()
    private var calls: [(name: String, body: [String: Any])] = []
    private var settings: [String: Any]
    var eventLabelsStatus = 200
    private var held: String?, released = false
    /// Calls named `name` are logged and then held until `release()` (a helper call that is on its way when the app quits).
    func hold(_ name: String) { lock.withLock { held = name; released = false } }
    func release() { lock.withLock { released = true } }
    init(enabled: Bool = true, backend: String = "apple-fm", revision: Int = 7, shadow: Bool = false, maxPerHour: Int = 120, threshold: String = "1.0") {
        settings = ["enabled": enabled, "backend": backend, "threshold": threshold, "maxPerHour": String(maxPerHour), "shadow": shadow, "shadowRate": "1",
                    "activities": ["feature", "bugfix", "docs"], "revision": revision]
    }
    func set(_ k: String, _ v: Any) { lock.withLock { settings[k] = v } }
    var log: [(name: String, body: [String: Any])] { lock.withLock { calls } }
    func count(_ name: String) -> Int { log.filter { $0.name == name }.count }
    func last(_ name: String) -> [String: Any]? { log.last { $0.name == name }?.body }
    var call: LabelWorker.Call {
        { [self] name, body in
            lock.withLock { calls.append((name, body)) }
            while lock.withLock({ held == name && !released }) { try? await Task.sleep(for: .milliseconds(5)) }
            switch name {
            case "label-settings": return (200, (try? JSONSerialization.data(withJSONObject: lock.withLock { settings })) ?? Data())
            case "event-labels": return (eventLabelsStatus, Data("{}".utf8))
            default: return (200, Data("{}".utf8))
            }
        }
    }
}

private func body(_ text: String) -> Data {
    try! JSONSerialization.data(withJSONObject: ["model": "claude-x", "messages": [["role": "user", "content": text]]])
}

private func rootEvent(_ id: String = "ev_root00000001", phase: String = "", activity: String = "", task: String = "", parent: String = "") -> UsageEvent {
    var e = UsageEvent(id: id, start: Date(), model: "claude-x")
    e.deviceId = "dev1"; e.phase = phase; e.activity = activity; e.task = task; e.parentId = parent
    return e
}

/// A backend that answers by the question (the ids tell phase from activity) and counts the samples.
private func answering(_ phase: String = "do", _ activity: String = "bugfix", count: Tally = Tally()) -> Decide.Backend {
    { _, _, ids in count.inc(); return ids.contains("plan") ? phase : activity }
}

private func worker(_ core: FakeCore, available: Bool = true, backend: @escaping Decide.Backend, foreground: FMForeground = FMForeground(),
                    budget: Duration = .seconds(20), ttl: Duration = .milliseconds(1)) -> LabelWorker {
    LabelWorker(call: core.call, available: { available }, backend: backend, foreground: foreground, settingsTTL: ttl, budget: budget,
                retryDelays: [.milliseconds(5)], waitStep: .milliseconds(5))
}

/// The settings are read asynchronously (never on the request path): the first admission after start only triggers the read.
private func primed(_ w: LabelWorker) async { await w.fetchSettings() }

@Suite(.serialized) struct LabelsWorker {
    @Test func offByDefaultAndForOtherBackendsNothingRuns() async throws {
        for core in [FakeCore(enabled: false), FakeCore(enabled: true, backend: "foundry:phi")] {
            let samples = Tally()
            let w = worker(core, backend: answering(count: samples))
            await primed(w)
            w.enqueue(rootEvent(), body: body("Fix the crash"))
            try await Task.sleep(for: .milliseconds(150))
            #expect(samples.value == 0 && w.isIdle && w.queued == 0)
            // nothing but the settings read; an enabled worker with a backend this Mac cannot run says so once (label-error), nothing else
            #expect(core.log.allSatisfy { $0.name == "label-settings" || ($0.name == "label-error" && core.count("label-error") == 1) })
        }
    }

    @Test func anUnavailableModelRecordsOneErrorAndDoesNothingElse() async throws {
        let core = FakeCore()
        let samples = Tally()
        let w = worker(core, available: false, backend: answering(count: samples))
        await primed(w)
        for i in 0..<3 { w.enqueue(rootEvent("ev_root0000000\(i)"), body: body("Fix the crash \(i)")) }
        #expect(await until { w.skipped == 3 })
        #expect(samples.value == 0)
        #expect(core.count("label-error") == 1 && (core.last("label-error")?["message"] as? String) == "on-device model unavailable")
        #expect(core.log.allSatisfy { ["label-settings", "label-error"].contains($0.name) })  // no event-labels, no decision event, no other provider
    }

    @Test func aDecisionStoresLabelsWithScoresAndTheRevision() async throws {
        let core = FakeCore()
        let samples = Tally()
        let w = worker(core, backend: answering("do", "bugfix", count: samples))
        await primed(w)
        w.enqueue(rootEvent(), body: body("Fix the crash in the share poll"))
        #expect(await until { core.count("event-labels") == 1 })
        let b = try #require(core.last("event-labels"))
        #expect(b["id"] as? String == "ev_root00000001" && b["phase"] as? String == "do" && b["activity"] as? String == "bugfix")
        #expect(b["phaseScore"] as? Int == 1000 && b["activityScore"] as? Int == 1000 && b["revision"] as? UInt64 == 7)
        #expect(samples.value == 6)
        // its own usage event: internal, apple-fm, no user, no text
        let d = try #require(core.last("label-decision"))
        #expect(d["provider"] as? String == "apple-fm" && d["failed"] as? Bool == false && d["userId"] == nil)
    }

    @Test func twoOfThreeIsStoredAs667() async throws {
        let core = FakeCore(threshold: "0.67")
        let n = Tally()
        let w = worker(core, backend: { _, _, ids in n.inc(); return ids.contains("plan") ? (n.value % 3 == 0 ? "check" : "do") : "docs" })
        await primed(w)
        w.enqueue(rootEvent(), body: body("Write the docs"))
        #expect(await until { core.count("event-labels") == 1 })
        let b = try #require(core.last("event-labels"))
        #expect(b["phase"] as? String == "do" && b["phaseScore"] as? Int == 667 && b["activityScore"] as? Int == 1000)
    }

    @Test func nothingDecidedStoresNothing() async throws {
        let core = FakeCore()
        let w = worker(core, backend: { _, _, _ in "I think it is a bugfix, probably" })
        await primed(w)
        w.enqueue(rootEvent(), body: body("???"))
        #expect(await until { w.done == 1 })
        #expect(core.count("event-labels") == 0)
    }

    @Test func shadowModeStoresTallysOnlyAndOnlyForExplicitlyLabelledCalls() async throws {
        let core = FakeCore(shadow: true)  // shadowRate 1: every explicitly labelled call is sampled
        let w = worker(core, backend: answering("do", "bugfix"))
        await primed(w)
        w.enqueue(rootEvent(phase: "do", activity: "docs"), body: body("Fix the crash"))
        #expect(await until { core.count("label-agreement") == 2 })
        let rows = core.log.filter { $0.name == "label-agreement" }.map { "\($0.body["kind"]!)/\($0.body["bucket"]!)/\($0.body["agree"]!)/\($0.body["revision"]!)" }
        #expect(rows == ["phase/1000/true/7", "activity/1000/false/7"])
        #expect(core.count("event-labels") == 0)
        // an unlabelled call in shadow mode is still labelled, as for the shadow-off case
        w.enqueue(rootEvent("ev_root00000002"), body: body("Another prompt"))
        #expect(await until { core.count("event-labels") == 1 })
        // fully labelled and not sampled (rate 4): nothing at all
        core.set("shadowRate", "4"); await primed(w)
        let before = core.log.count
        w.enqueue(rootEvent("ev_root00000003", phase: "do", activity: "docs"), body: body("Third prompt"))
        try await Task.sleep(for: .milliseconds(100))
        #expect(core.log.dropFirst(before).allSatisfy { $0.name == "label-settings" })
    }

    @Test func childrenAndNonRootEventsAreNeverClassified() async throws {
        let core = FakeCore()
        let samples = Tally()
        let w = worker(core, backend: answering(count: samples))
        await primed(w)
        w.enqueue(rootEvent(parent: "ev_other"), body: body("child"))
        w.enqueue(rootEvent(), body: nil)
        try await Task.sleep(for: .milliseconds(100))
        #expect(samples.value == 0 && w.isIdle)
    }

    @Test func aStaleResultIsDiscardedWhenTheSettingsChanged() async throws {
        let core = FakeCore()
        let gate = Gate()
        let w = worker(core, backend: { _, _, ids in try await gate.wait(); return ids.contains("plan") ? "do" : "bugfix" })
        await primed(w)
        w.enqueue(rootEvent(), body: body("Fix it"))
        #expect(await until { gate.waiting >= 1 })
        core.set("revision", 8)  // the owner changed something while the model was working
        await w.fetchSettings()
        gate.open()
        #expect(await until { w.isIdle })
        #expect(core.count("event-labels") == 0 && core.count("label-agreement") == 0)
    }

    @Test func theQueueIsBoundedAndAdmissionNeverWaits() async throws {
        let core = FakeCore()
        let gate = Gate()
        let w = worker(core, backend: { _, _, ids in try await gate.wait(); return "do" })
        await primed(w)
        let t0 = ContinuousClock.now
        for i in 0..<40 { w.enqueue(rootEvent(String(format: "ev_root%08d", i)), body: body("prompt \(i)")) }
        #expect(ContinuousClock.now - t0 < .milliseconds(500))  // the user's request is not held by a blocked model
        #expect(w.queued <= LabelWorker.queueLimit && w.skipped >= 40 - LabelWorker.queueLimit - 1)
        #expect(await until { gate.waiting == 1 })  // one decision at a time
        try await Task.sleep(for: .milliseconds(100))
        #expect(gate.waiting == 1)
        w.stop()
    }

    @Test func theHourlyCapIsRespectedAndRepeatedFirstPromptsUseTheCache() async throws {
        let core = FakeCore(maxPerHour: 2)
        let samples = Tally()
        let w = worker(core, backend: answering(count: samples))
        await primed(w)
        w.enqueue(rootEvent("ev_root00000001"), body: body("same first prompt"))
        #expect(await until { w.done == 1 })
        w.enqueue(rootEvent("ev_root00000002"), body: body("same first prompt"))  // cached: no new samples, does not use the cap
        #expect(await until { w.done == 2 })
        #expect(samples.value == 6 && core.count("event-labels") == 2)
        w.enqueue(rootEvent("ev_root00000003"), body: body("another prompt"))
        #expect(await until { w.done == 3 })
        w.enqueue(rootEvent("ev_root00000004"), body: body("a third prompt"))  // the cap of 2 decisions per hour is used up
        #expect(await until { w.skipped == 1 })
        #expect(samples.value == 12)
    }

    @Test func aUserRequestIsNeverDelayedByTheWorker() async throws {
        let core = FakeCore()
        let fg = FMForeground()
        let started = Tally(), cancelled = Tally()
        let w = worker(core, backend: { _, _, _ in
            started.inc()
            do { try await Task.sleep(for: .seconds(60)) } catch { cancelled.inc(); throw error }  // a blocking fake: runs until cancelled
            return "do"
        }, foreground: fg)
        await primed(w)
        w.enqueue(rootEvent(), body: body("Fix the crash"))
        #expect(await until { started.value == 1 })
        fg.enter()  // a user's Apple FM request arrives
        #expect(await until { cancelled.value == 1 })  // the running sample was cancelled at once
        try await Task.sleep(for: .milliseconds(200))
        #expect(started.value == 1)  // and no sample starts while the user's request runs
        fg.leave()
        #expect(await until { started.value == 2 })  // one retry afterwards
        fg.enter()  // the user is back
        #expect(await until { cancelled.value == 2 && w.skipped == 1 })  // second time: unknown, nothing stored
        fg.leave()
        try await Task.sleep(for: .milliseconds(150))
        #expect(started.value == 2 && core.count("event-labels") == 0)
        #expect(core.log.filter { $0.name == "label-decision" }.allSatisfy { $0.body["preempted"] as? Bool == true })  // both interrupted attempts are recorded as work
        #expect(!fg.active)
        w.stop()
    }

    @Test func aSampleNeverStartsWhileAUserRequestRuns() async throws {
        let core = FakeCore()
        let fg = FMForeground()
        let started = Tally()
        fg.enter()
        let w = worker(core, backend: { _, _, ids in started.inc(); return ids.contains("plan") ? "do" : "bugfix" }, foreground: fg)
        await primed(w)
        w.enqueue(rootEvent(), body: body("Fix the crash"))
        try await Task.sleep(for: .milliseconds(250))
        #expect(started.value == 0 && w.samplesStarted == 0)
        fg.leave()
        #expect(await until { core.count("event-labels") == 1 })
        #expect(started.value == 6)
    }

    @Test func aDecisionOverTheBudgetFailsWithAClassAndNeverBlocks() async throws {
        let core = FakeCore()
        let w = worker(core, backend: { _, _, _ in try await Task.sleep(for: .seconds(60)); return "do" }, budget: .milliseconds(150))
        await primed(w)
        w.enqueue(rootEvent(), body: body("slow"))
        #expect(await until { w.failed == 1 })
        #expect((core.last("label-error")?["message"] as? String) == "label decision failed")
        #expect(core.last("label-decision")?["failed"] as? Bool == true && core.count("event-labels") == 0)
    }

    @Test func nothingHappensAfterStop() async throws {
        let core = FakeCore()
        let gate = Gate()
        let samples = Tally()
        let w = worker(core, backend: { _, _, ids in samples.inc(); try await gate.wait(); return "do" })
        await primed(w)
        for i in 0..<3 { w.enqueue(rootEvent("ev_root0000000\(i)"), body: body("prompt \(i)")) }
        #expect(await until { gate.waiting == 1 })
        w.stop()  // the app quits
        let calls = core.log.count
        gate.open()
        try await Task.sleep(for: .milliseconds(300))
        #expect(samples.value == 1 && core.log.count == calls)  // not one more sample, not one more helper call
        w.enqueue(rootEvent("ev_root00000009"), body: body("late"))
        try await Task.sleep(for: .milliseconds(100))
        #expect(samples.value == 1 && core.log.count == calls)
    }

    @Test func anExplicitLabelIsNotTheWorkersToOverwrite() async throws {
        // the worker sends only a choice for the labels the core still has empty; the core refuses the rest (Go: TestSetModelLabels*). Here:
        // an explicit phase and no activity: the call is made, the core keeps the given phase.
        let core = FakeCore()
        let w = worker(core, backend: answering("check", "docs"))
        await primed(w)
        w.enqueue(rootEvent(phase: "do"), body: body("Write the docs"))
        #expect(await until { core.count("event-labels") == 1 })
    }

    @Test func theInputIsTheCappedFirstUserMessageWithoutReminders() {
        let blocks: [String: Any] = ["model": "m", "messages": [["role": "user", "content": [["type": "text", "text": "<system-reminder>secret</system-reminder>First"], ["type": "text", "text": String(repeating: "🧪", count: 3000)]]],
                                                                  ["role": "user", "content": "second message"]]]
        let (text, key) = LabelWorker.input(body: try! JSONSerialization.data(withJSONObject: blocks), device: "d", task: "fix-145-labels")
        #expect(text.hasPrefix("task: fix-145-labels\nFirst\n") && !text.contains("secret") && text.unicodeScalars.count == 2000)
        #expect(key.count == 64)
        #expect(LabelWorker.input(body: Data("[]".utf8), device: "d", task: "").0.isEmpty)
        #expect(LabelWorker.input(body: body("x"), device: "d", task: "has space").0 == "x")  // not a safe task label: left out
    }
}

private func filesUnder(_ dir: URL) -> [URL] {
    var out: [URL] = []
    if let en = FileManager.default.enumerator(at: dir, includingPropertiesForKeys: [.isRegularFileKey]) {
        for case let f as URL in en where (try? f.resourceValues(forKeys: [.isRegularFileKey]).isRegularFile) == true { out.append(f) }
    }
    return out
}

/// Holds every caller until opened (a blocking fake that also reacts to cancellation).
private final class Gate: @unchecked Sendable {
    private let lock = NSLock(); private var isOpen = false; private var _waiting = 0
    var waiting: Int { lock.withLock { _waiting } }
    func open() { lock.withLock { isOpen = true } }
    func wait() async throws {
        lock.withLock { _waiting += 1 }
        defer { lock.withLock { _waiting -= 1 } }
        while !lock.withLock({ isOpen }) { try await Task.sleep(for: .milliseconds(5)) }
    }
}

// MARK: the real helper: privacy, the settings choice, the internal usage event

@Suite(.serialized) struct LabelsPrivacy {
    @Test func theClassifiedTextIsNeverStoredLoggedOrAnswered() async throws {
        let marker = "ZQXMARKER-\(UUID().uuidString.prefix(8))-private-prompt"
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("mglabels-\(UUID().uuidString.prefix(8))", isDirectory: true).cleanedAtExit()
        let store = dir.appendingPathComponent("core")
        try CoreHelper.prepareStoreFolder(store)
        let h = CoreHelper(binary: try await HelperBuild.shared.binary(), store: store, ownerName: "tester", marks: MemoryMarks(), backoff: [0.05, 0.1], requestTimeout: 10)
        h.start(); defer { h.stop() }
        #expect(await h.waitReady(timeout: 30))
        func adm(_ m: String, _ p: String, _ b: String = "") async -> CoreResponse {
            let parts = p.split(separator: "?", maxSplits: 1).map(String.init)
            return await h.call(context: .admin, request: CoreRequest(method: m, path: parts[0], query: parts.count > 1 ? parts[1] : "", body: Data(b.utf8)))
        }

        // the settings choice on the Mac: apple-fm only, a clear refusal otherwise
        let refused = await adm("PUT", "/usage/labels/settings", #"{"enabled":true,"backend":"foundry:phi-4-mini"}"#)
        #expect(refused.status == 400 && String(decoding: refused.body, as: UTF8.self).contains("choose apple-fm"))
        let on = await adm("PUT", "/usage/labels/settings", #"{"enabled":true,"backend":"apple-fm","shadow":false}"#)
        #expect(on.status == 200)

        // a root event of the router (a finished one is stored whole)
        var e = UsageEvent(id: "ev_priv0000001", start: Date().addingTimeInterval(-2), model: "claude-x")
        e.end = Date(); e.outcome = "ok"; e.provider = "claude-cli"; e.deviceId = "dev1"
        _ = await h.internalCall("event-finish", ["event": e.json])

        // every call to the helper and every answer is captured
        let seen = Seen()
        let call: LabelWorker.Call = { name, body in
            seen.add(try! JSONSerialization.data(withJSONObject: body))
            let r = await h.internalCall(name, body)
            seen.add(r.body)
            return (r.status, r.body)
        }
        let boom = Tally()
        struct Leak: Error, CustomStringConvertible { let text: String; var description: String { text } }
        let w = LabelWorker(call: call, available: { true }, backend: { _, user, ids in
            boom.inc()
            if boom.value > 6 { throw Leak(text: user) }  // the second prompt: an error that carries the prompt
            return ids.contains("plan") ? "do" : "bugfix"
        }, foreground: FMForeground(), settingsTTL: .milliseconds(1), retryDelays: [.milliseconds(50), .milliseconds(200), .milliseconds(500)], waitStep: .milliseconds(5))
        await w.fetchSettings()
        w.enqueue(e, body: body("Fix the crash. \(marker)"))
        #expect(await until(timeout: 20) { w.done == 1 })
        var e2 = e; e2.id = "ev_priv0000002"
        _ = await h.internalCall("event-finish", ["event": e2.json])
        w.enqueue(e2, body: body("Another one. \(marker)"))
        #expect(await until(timeout: 20) { w.failed == 1 })

        // the labels are there, with the model source, the decision event is internal and has no user
        let events = String(decoding: await adm("GET", "/usage/events?days=3&labels=1").body, as: UTF8.self)
        #expect(events.contains(#""phaseSource":"model""#) && events.contains(#""activitySource":"model""#), "\(events)")
        #expect(events.contains(#""provider":"apple-fm""#) && events.contains(#""activitySource":"internal""#), "\(events)")
        let settings = String(decoding: await adm("GET", "/usage/labels/settings").body, as: UTF8.self)
        let agreement = String(decoding: await adm("GET", "/usage/labels/agreement").body, as: UTF8.self)
        let audit = String(decoding: await adm("GET", "/audit").body, as: UTF8.self)
        #expect(settings.contains("label decision failed"))
        for (name, text) in [("events", events), ("settings", settings), ("agreement", agreement), ("audit", audit), ("calls", seen.text)] {
            #expect(!text.contains(marker), "marker in \(name)")
            #expect(!text.contains("ZQXMARKER"), "marker in \(name)")
        }
        h.stop()
        try await Task.sleep(for: .milliseconds(200))
        // nothing in the helper's folder (database, journal, spool, logs)
        var scanned = 0
        for f in filesUnder(dir) {
            guard let d = try? Data(contentsOf: f) else { continue }
            scanned += 1
            #expect(d.range(of: Data("ZQXMARKER".utf8)) == nil, "marker in \(f.lastPathComponent)")
        }
        #expect(scanned > 0)
        // the worker itself keeps no log (os_log, print): this file has none; the source is checked as a test too
        let src = try String(contentsOf: URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent("Sources/RouterCore/LabelWorker.swift"), encoding: .utf8)
        for word in ["print(", "NSLog(", "Logger(", "os_log", "debugPrint(", "dump("] { #expect(!src.contains(word), "\(word) in LabelWorker.swift") }
    }

    final class Seen: @unchecked Sendable {
        private let lock = NSLock(); private var data = Data()
        func add(_ d: Data) { lock.withLock { data.append(d) } }
        var text: String { lock.withLock { String(decoding: data, as: UTF8.self) } }
    }
}


// MARK: review fixes: the start race, the lifecycle, preempted work, a stored backend this Mac cannot run

@Suite(.serialized) struct LabelsLifecycle {
    @Test func aUserRequestBetweenRegistrationAndStartNeverLetsASampleRun() async throws {
        let core = FakeCore()
        let fg = FMForeground()
        let entered = Tally(), enteredWhileForeground = Tally()
        fg.afterRegister = { if !fg.active { fg.enter() } }  // the user's request arrives in the window after the sample was registered
        let w = worker(core, backend: { _, _, ids in
            entered.inc(); if fg.active { enteredWhileForeground.inc() }
            return ids.contains("plan") ? "do" : "bugfix"
        }, foreground: fg)
        await primed(w)
        w.enqueue(rootEvent(), body: body("Fix the crash"))
        try await Task.sleep(for: .milliseconds(300))
        #expect(entered.value == 0 && w.samplesStarted == 0)  // prevented, not merely cancelled afterwards
        fg.afterRegister = nil
        fg.leave()
        #expect(await until { core.count("event-labels") == 1 })  // the retry after the user finished
        #expect(enteredWhileForeground.value == 0)
    }

    @Test func nothingIsCalledAfterStopDuringTheRetrySleep() async throws {
        let core = FakeCore(); core.eventLabelsStatus = 404
        // the injected retry pause: reports that the worker is in it, then lasts until the worker's task is cancelled (stop) – no wall time involved
        let entered = AsyncStream<Void>.makeStream(), left = AsyncStream<Void>.makeStream()
        let w = LabelWorker(call: core.call, available: { true }, backend: answering(), foreground: FMForeground(), settingsTTL: .milliseconds(1),
                            retryDelays: [.milliseconds(400), .milliseconds(400)], waitStep: .milliseconds(5),
                            sleep: { _ in entered.continuation.yield(); try? await Task.sleep(for: .seconds(3600)); left.continuation.yield() })
        await primed(w)
        w.enqueue(rootEvent(), body: body("Fix the crash"))
        var it = entered.stream.makeAsyncIterator()
        _ = await it.next()  // now in the retry sleep (the first event-labels call was answered 404)
        #expect(core.count("event-labels") == 1)
        w.stop()
        var gone = left.stream.makeAsyncIterator()
        _ = await gone.next()  // the sleep ended by the cancel: the retry would fire right now
        let n = core.log.count
        try await Task.sleep(for: .milliseconds(300))  // a late look only strengthens the check
        #expect(core.log.count == n && core.count("event-labels") == 1 && core.count("label-error") == 0)
    }

    @Test func nothingIsCalledAfterStopDuringAShadowWrite() async throws {
        let core = FakeCore(shadow: true)
        let w = worker(core, backend: answering())
        await primed(w)
        core.hold("label-agreement")
        w.enqueue(rootEvent(phase: "do", activity: "docs"), body: body("Fix the crash"))
        #expect(await until { core.count("label-agreement") == 1 })  // the first write is on its way
        let stopper = Task.detached { w.stop() }  // waits for it, a few seconds at most
        try await Task.sleep(for: .milliseconds(100))
        core.release()
        await stopper.value
        let n = core.log.count
        try await Task.sleep(for: .milliseconds(300))
        #expect(core.log.count == n && core.count("label-agreement") == 1 && core.count("label-error") == 0)
    }

    @Test func nothingIsCalledAfterStopDuringASettingsRefresh() async throws {
        let core = FakeCore(backend: "foundry:phi")  // a result that would make the refresh call label-error
        let w = worker(core, backend: answering())
        core.hold("label-settings")
        w.enqueue(rootEvent(), body: body("Fix the crash"))  // the first admission starts a refresh in the background
        #expect(await until { core.count("label-settings") == 1 })
        let stopper = Task.detached { w.stop() }
        try await Task.sleep(for: .milliseconds(100))
        core.release()
        await stopper.value
        let n = core.log.count
        try await Task.sleep(for: .milliseconds(300))
        #expect(core.log.count == n && core.count("label-error") == 0)
    }

    @Test func aPreemptedDecisionThatRanInferenceIsRecorded() async throws {
        let core = FakeCore()
        let fg = FMForeground()
        let started = Tally()
        let w = worker(core, backend: { _, _, _ in
            started.inc()
            try await Task.sleep(for: .seconds(60)); return "do"
        }, foreground: fg)
        await primed(w)
        w.enqueue(rootEvent(), body: body("Fix the crash"))
        #expect(await until { started.value == 1 })
        fg.enter()
        #expect(await until { core.count("label-decision") == 1 })
        let d = try #require(core.last("label-decision"))
        #expect(d["preempted"] as? Bool == true && d["failed"] as? Bool == false && d["provider"] as? String == "apple-fm" && (d["tokensIn"] as? Int ?? 0) > 0)
        #expect(!"\(d)".contains("Fix the crash"))  // no prompt text
        fg.leave(); w.stop()
    }

    @Test func aStoredBackendThisMacCannotRunIsReportedOncePerRevision() async throws {
        let core = FakeCore(backend: "foundry:phi-4-mini")
        let w = worker(core, backend: answering())
        await primed(w); await primed(w)
        #expect(core.count("label-error") == 1)
        #expect((core.last("label-error")?["message"] as? String) == "backend not available on this Mac: choose apple-fm")
        core.set("revision", 8); await primed(w)
        #expect(core.count("label-error") == 2)
    }
}


// MARK: B4: minigw gw labels eval (the Mac gateway side; the Go gateway has the same tests in client/labels_eval_test.go)

private func evalBackend(_ calls: Tally = Tally()) -> Decide.Backend {
    let scripts: [String: [String]] = [
        "alpha/phase": ["do", "do", "do"], "alpha/activity": ["bugfix", "bugfix", "bugfix"],
        "beta/phase": ["do", "do", "plan"], "beta/activity": ["docs", "docs", "docs"],
        "gamma/phase": ["plan", "plan", "plan"], "gamma/activity": ["nonsense", "nonsense", "nonsense"],
        "delta/activity": ["feature", "feature", "feature"]]
    let seen = Replies2()
    return { _, user, ids in
        calls.inc()
        let q = ids.contains("plan") ? "phase" : "activity"
        for k in ["alpha", "beta", "gamma", "delta"] where user.contains(k) { return scripts[k + "/" + q]![seen.next(k + "/" + q) % 3] }
        throw Decide.Failure.backend
    }
}

private final class Replies2: @unchecked Sendable {
    private let lock = NSLock(); private var n: [String: Int] = [:]
    func next(_ k: String) -> Int { lock.withLock { defer { n[k, default: 0] += 1 }; return n[k, default: 0] } }
}

private let evalExamples: [LabelWorker.EvalExample] = [
    .init(text: "alpha fix the crash", phase: "do", activity: "bugfix"), .init(text: "beta two of three", phase: "do", activity: "docs"),
    .init(text: "gamma wrong and unsure", phase: "check", activity: "test"), .init(text: "delta only an activity", phase: "", activity: "ops")]

private func evalCore() -> FakeCore {
    let core = FakeCore(enabled: false)  // the feature stays OFF: an evaluation needs a backend, not the worker
    core.set("activities", ["feature", "bugfix", "docs", "test", "ops"])
    return core
}

private func evalReply(_ r: (Int, Data)) -> (Int, [String: Any]) { (r.0, (try? JSONSerialization.jsonObject(with: r.1) as? [String: Any]) ?? [:]) }
private func message(_ o: [String: Any]) -> String { ((o["error"] as? [String: Any])?["message"] as? String) ?? "" }

@Suite(.serialized) struct LabelsEval {
    @Test func numbersAreTheSameAsGoAndNothingIsStored() async throws {
        let core = evalCore()
        let samples = Tally()
        let w = worker(core, backend: evalBackend(samples))
        let (status, o) = evalReply(await w.evaluate(evalExamples))
        #expect(status == 200)
        let e = try #require(o["eval"] as? [String: Any])
        #expect(e["backend"] as? String == "apple-fm" && e["examples"] as? Int == 4 && e["run"] as? Int == 4 && e["notRun"] as? Int == 0)
        func cell(_ k: String, _ t: String) -> [Int] { let c = ((e[k] as! [String: Any])[t] as! [String: Any]); return [c["decided"] as! Int, c["correct"] as! Int] }
        #expect((e["phase"] as! [String: Any])["scored"] as? Int == 3 && cell("phase", "t067") == [3, 2] && cell("phase", "t10") == [2, 1])
        #expect((e["activity"] as! [String: Any])["scored"] as? Int == 4 && cell("activity", "t067") == [3, 2] && cell("activity", "t10") == [3, 2])
        let m = e["mistakes"] as! [String: [[String: Any]]]
        #expect(m["phase"]?.map { "\($0["expected"]!)>\($0["got"]!)#\($0["n"]!)" } == ["check>plan#1"])
        #expect(m["activity"]?.map { "\($0["expected"]!)>\($0["got"]!)#\($0["n"]!)" } == ["ops>feature#1"])
        let wrong = (e["wrong"] as! [[String: Any]]).map { "\($0["kind"]!):\($0["line"]!):\($0["expected"]!)>\($0["got"]!)" }
        #expect(wrong == ["phase:3:check>plan", "activity:3:test>", "activity:4:ops>feature"])
        #expect(samples.value == 21)  // 3 examples x 6 samples + the activity-only one x 3
        // stores nothing: no label, no agreement counter; the one record is the worker's own internal event, once for the run
        #expect(core.log.allSatisfy { ["label-settings", "label-decision"].contains($0.name) })
        #expect(core.count("label-decision") == 1)
        let d = try #require(core.last("label-decision"))
        #expect(d["provider"] as? String == "apple-fm" && d["failed"] as? Bool == false && d["userId"] == nil && (d["tokensIn"] as? Int ?? 0) > 0)
    }

    @Test func theLimitsAndRefusalsAreTheGoOnes() async throws {
        let core = evalCore()
        let w = worker(core, backend: evalBackend())
        func ex(_ t: String, _ p: String, _ a: String) -> LabelWorker.EvalExample { .init(text: t, phase: p, activity: a) }
        for (name, list, want) in [
            ("empty text", [ex("  ", "do", "")], "example 1: text is empty"),
            ("bad phase", [ex("alpha", "do", ""), ex("alpha", "doing", "")], "example 2: phase is one of plan, do, check, act"),
            ("bad activity", [ex("alpha", "", "gardening")], "example 1: phase is one of"),
            ("no label", [ex("alpha", "", "")], "example 1: needs a phase or an activity")] {
            let (st, o) = evalReply(await w.evaluate(list))
            #expect(st == 400 && message(o).contains(want), "\(name): \(st) \(message(o))")
        }
        // case does not matter, a text is capped like the worker caps it (2000 code points)
        let seen = LongestUser()
        let capped = worker(core, backend: { _, user, ids in seen.note(user); return ids.contains("plan") ? "do" : "bugfix" })
        let (st, _) = evalReply(await capped.evaluate([ex("alpha " + String(repeating: "é", count: 5000), "DO", "Bugfix")]))
        #expect(st == 200 && seen.longest > 3000 && seen.longest < 2000 * 2 + 100)
        // no backend chosen / a backend this Mac cannot run / the model unavailable
        for (c, avail, want) in [(FakeCore(enabled: false, backend: ""), true, "choose a backend"), (FakeCore(enabled: false, backend: "foundry:phi"), true, "label backend unavailable on this gateway"),
                                 (evalCore(), false, "on-device model unavailable")] {
            let (st, o) = evalReply(await worker(c, available: avail, backend: evalBackend()).evaluate([ex("alpha", "do", "")]))
            #expect(st == 409 && message(o).contains(want), "\(st) \(message(o))")
            #expect(c.log.allSatisfy { $0.name == "label-settings" })
        }
        // the route is not a core route: no member and no internal call reaches it
        #expect(!CoreRoutes.isAdminPath("POST", "/usage/labels/eval") && !CoreRoutes.isMemberPath("/minigw/v1/usage/labels/eval"))
    }

    final class LongestUser: @unchecked Sendable {
        private let lock = NSLock(); private var n = 0
        func note(_ s: String) { lock.withLock { n = max(n, s.utf8.count) } }
        var longest: Int { lock.withLock { n } }
    }

    @Test func oneEvaluationAtATime() async throws {
        let core = evalCore()
        let started = Tally()
        let gate = Gate()
        let w = worker(core, backend: { _, _, ids in started.inc(); await gate.wait(); return ids.contains("plan") ? "do" : "bugfix" })
        let first = Task { await w.evaluate([.init(text: "alpha", phase: "do", activity: "")]) }
        #expect(await until { started.value == 1 })
        let (st, o) = evalReply(await w.evaluate(evalExamples))
        #expect(st == 429 && message(o).contains("Another label evaluation is running"))
        await gate.open()
        #expect(evalReply(await first.value).0 == 200)
        #expect(evalReply(await w.evaluate([.init(text: "alpha", phase: "do", activity: "")])).0 == 200)  // released
    }

    actor Gate {
        private var isOpen = false
        func open() { isOpen = true }
        func wait() async { while !isOpen { try? await Task.sleep(for: .milliseconds(5)) } }
    }

    @Test func aUserRequestKeepsPriorityAndAnEndedSampleIsTriedAgain() async throws {
        let core = evalCore()
        let fg = FMForeground()
        let samples = Tally()
        fg.enter()
        let w = worker(core, backend: evalBackend(samples), foreground: fg)
        let run = Task { await w.evaluate([.init(text: "alpha", phase: "do", activity: "")]) }
        try await Task.sleep(for: .milliseconds(150))
        #expect(samples.value == 0)  // no sample while a user's request runs
        fg.leave()
        let (st, o) = evalReply(await run.value)
        #expect(st == 200 && ((o["eval"] as! [String: Any])["run"] as? Int) == 1 && samples.value == 3)
        // a sample in flight is cancelled by the arrival of a user's request, and the example is tried again
        let started = Tally(), cancelled = Tally()
        let w2 = worker(core, backend: { _, _, _ in
            started.inc()
            if started.value == 1 {
                do { try await Task.sleep(for: .seconds(60)) } catch { cancelled.inc(); throw error }
            }
            return "do"
        }, foreground: fg)
        let run2 = Task { await w2.evaluate([.init(text: "alpha", phase: "do", activity: "")]) }
        #expect(await until { started.value == 1 })
        fg.enter()
        #expect(await until { cancelled.value == 1 })
        fg.leave()
        let (st2, o2) = evalReply(await run2.value)
        #expect(st2 == 200 && ((o2["eval"] as! [String: Any])["notRun"] as? Int) == 0)
        #expect(!fg.active)
    }

    @Test func aBackendThatFailsEveryTimeIsAClearNoAnswerWithoutText() async throws {
        let core = evalCore()
        struct Leak: Error, CustomStringConvertible { let text: String; var description: String { text } }
        let w = worker(core, backend: { _, user, _ in throw Leak(text: user) }, budget: .milliseconds(200))
        let (st, o) = evalReply(await w.evaluate(evalExamples))
        #expect(st == 502 && message(o).contains("no answer for any example"))
        #expect(core.count("label-decision") == 1 && core.last("label-decision")?["failed"] as? Bool == true)
    }

    @Test func theExampleTextIsNeverInACallAnAnswerOrTheSource() async throws {
        let marker = "EVALMARKER-\(UUID().uuidString.prefix(8))-private"
        let core = evalCore()
        let w = worker(core, backend: { _, user, ids in
            if user.contains("fail-") { throw Decide.Failure.backend }
            return ids.contains("plan") ? "do" : "bugfix"
        })
        for text in ["fine \(marker)", "fail-\(marker)"] {
            let (st, o) = evalReply(await w.evaluate([.init(text: text, phase: "do", activity: "bugfix"), .init(text: "ok", phase: "do", activity: "")]))
            #expect(st == 200)
            #expect(!String(decoding: (try? JSONSerialization.data(withJSONObject: o)) ?? Data(), as: UTF8.self).contains(marker))
        }
        let all = core.log.map { String(decoding: (try? JSONSerialization.data(withJSONObject: $0.body)) ?? Data(), as: UTF8.self) }.joined()
        #expect(!all.contains(marker) && !all.contains("EVALMARKER"))
        let src = try String(contentsOf: URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent("Sources/RouterCore/LabelWorker.swift"), encoding: .utf8)
        for word in ["print(", "NSLog(", "Logger(", "os_log", "debugPrint(", "dump("] { #expect(!src.contains(word), "\(word) in LabelWorker.swift") }
    }

    @Test func aDisconnectEndsTheEvaluationAtOnceAndFreesTheSlot() async throws {
        let core = evalCore()
        let started = Tally()
        let slow = Flag2()
        let w = worker(core, backend: { _, _, ids in
            started.inc()
            if slow.on { try await Task.sleep(for: .seconds(60)) }  // runs until cancelled
            return ids.contains("plan") ? "do" : "bugfix"
        })
        slow.on = true
        let run = Task { await w.evaluate(evalExamples) }
        #expect(await until { started.value == 1 })
        run.cancel()  // the caller went away
        let (st, _) = evalReply(await run.value)
        #expect(st == 502 || st == 200)
        let n = started.value
        try await Task.sleep(for: .milliseconds(150))
        #expect(started.value == n && n == 1, "a sample started after the disconnect")
        slow.on = false
        #expect(evalReply(await w.evaluate([.init(text: "alpha", phase: "do", activity: "")])).0 == 200)  // the slot was free at once
    }

    @Test func stopDuringAnEvaluationWaitingForAUserRequestStartsNothing() async throws {
        let core = evalCore()
        let fg = FMForeground()
        let samples = Tally()
        fg.enter()
        let w = worker(core, backend: evalBackend(samples), foreground: fg)
        let run = Task { await w.evaluate(evalExamples) }
        try await Task.sleep(for: .milliseconds(150))
        let before = core.log.count
        w.stop()  // returns only when the evaluation has ended
        let (st, _) = evalReply(await run.value)
        #expect(st == 502)
        fg.leave()
        try await Task.sleep(for: .milliseconds(200))
        #expect(samples.value == 0 && w.samplesStarted == 0)
        #expect(core.log.count == before, "a helper call after stop")
        #expect(evalReply(await w.evaluate(evalExamples)).0 == 503)  // none starts afterwards
    }

    final class Flag2: @unchecked Sendable {
        private let lock = NSLock(); private var v = false
        var on: Bool { get { lock.withLock { v } } set { lock.withLock { v = newValue } } }
    }
}
