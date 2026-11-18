import Foundation
import Testing
@testable import RouterCore

// B3 (docs/plans/brief-decide-labels.md): the Swift port of decide and the Mac label worker. Fake backends only: never the real on-device model,
// never a network call. The privacy test runs the REAL minigw-core over a temp store (a unix socket, no network).

private let testdata = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
    .appendingPathComponent("client/testdata")

private func until(_ what: String = "", timeout: Double = 8, _ cond: @Sendable () -> Bool) async -> Bool {
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
            #expect(core.log.allSatisfy { $0.name == "label-settings" })
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
        #expect(started.value == 2 && core.count("event-labels") == 0 && core.count("label-decision") == 0)
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
