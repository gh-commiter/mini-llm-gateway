import Foundation
import Testing
@testable import RouterCore

// 0.6.0 M4 (contract section 7: a loss is always shown as a gap): events queued while the helper is down are not lost silently when the app
// quits. The REAL minigw-core over temp stores; the journal file is a temp file, never the app's.

private func tempURL(_ name: String) -> URL {
    let d = FileManager.default.temporaryDirectory.appendingPathComponent("mgloss-\(UUID().uuidString.prefix(8))", isDirectory: true)
    try? FileManager.default.createDirectory(at: d, withIntermediateDirectories: true)
    return d.appendingPathComponent(name)
}

private func event(_ i: Int, ago: TimeInterval) -> UsageEvent {
    var e = UsageEvent(id: "ev_quit\(i)", start: Date().addingTimeInterval(-ago), model: "m-quit")
    e.outcome = "ok"; e.end = Date(); e.provider = "p"
    return e
}

private func savedCount(_ file: URL) throws -> Int {
    let o = try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as! [String: Any]
    return (o["gaps"] as! [[String: Any]]).reduce(0) { $0 + ($1["count"] as! Int) }
}

private func gaps(_ h: CoreHelper) async -> [(count: Int, from: String, to: String)] {
    let r = await h.call(context: .admin, request: CoreRequest(method: "GET", path: "/usage/events", query: "days=7"))
    let o = (try? JSONSerialization.jsonObject(with: r.body) as? [String: Any]) ?? [:]
    return (o["gaps"] as? [[String: Any]] ?? []).map { ($0["count"] as? Int ?? 0, $0["from"] as? String ?? "", $0["to"] as? String ?? "") }
}

@Suite(.serialized) struct UsageLossAtQuit {
    @Test func aQueueLeftAtQuitIsReportedOnceAtTheNextReady() async throws {
        let file = tempURL("usage-lost.json")
        // run 1: the helper never comes up; three events wait in the queue; the app quits
        let down = CoreHelper(binary: nil, store: tempURL("core1"), marks: MemoryMarks())
        let first = UsageSender(core: down, limit: 100, retry: .milliseconds(20), journal: UsageGapJournal(file: file))
        for i in 0..<3 { first.finish(event(i, ago: Double(100 - i))) }
        first.begin(event(2, ago: 1))  // the begin of an event that also has its finish queued counts once
        #expect(first.pending == 4)
        first.lostWithTheQueue()  // what applicationWillTerminate does (UsageLoss.persistPendingAtQuit)
        #expect(try savedCount(file) == 3)

        // run 2: a new app start, a ready helper: the gap is sent once, with the time span of the lost events
        let store = tempURL("core2").deletingLastPathComponent().appendingPathComponent("core")
        try CoreHelper.prepareStoreFolder(store)
        let h = CoreHelper(binary: try await HelperBuild.shared.binary(), store: store, ownerName: "tester", marks: MemoryMarks(), backoff: [0.05, 0.1], requestTimeout: 10)
        h.start(); defer { h.stop() }
        let second = UsageSender(core: h, limit: 100, retry: .milliseconds(20), journal: UsageGapJournal(file: file))
        #expect(await h.waitReady(timeout: 20))
        #expect(await second.flush())
        var got: [(count: Int, from: String, to: String)] = []
        for _ in 0..<200 {
            got = await gaps(h)
            if !got.isEmpty { break }
            try await Task.sleep(for: .milliseconds(30))
        }
        #expect(got.count == 1 && got[0].count == 3, "\(got)")
        #expect(!FileManager.default.fileExists(atPath: file.path))  // accepted: the file is gone

        // run 3: nothing is reported again
        let third = UsageSender(core: h, limit: 100, retry: .milliseconds(20), journal: UsageGapJournal(file: file))
        #expect(await third.flush())
        try await Task.sleep(for: .milliseconds(300))
        #expect((await gaps(h)).count == 1)
    }

    @Test func aDroppedEventIsOnDiskAtOnce() throws {
        let file = tempURL("usage-lost.json")
        let down = CoreHelper(binary: nil, store: tempURL("core3"), marks: MemoryMarks())
        let s = UsageSender(core: down, limit: 2, retry: .milliseconds(20), journal: UsageGapJournal(file: file))
        for i in 0..<5 { s.finish(event(i, ago: 10)) }  // the queue holds 2, three are dropped
        #expect(try savedCount(file) == 3)
    }

    @Test func aJournalSurvivesARestartAndMergesLosses() throws {
        let file = tempURL("usage-lost.json")
        let a = UsageGapJournal(file: file)
        let t = Date(timeIntervalSince1970: 1_700_000_000)
        a.add(count: 2, from: t, to: t.addingTimeInterval(10))
        a.add(count: 1, from: t.addingTimeInterval(-5), to: t.addingTimeInterval(3))
        let b = UsageGapJournal(file: file)  // the next start
        let g = try #require(b.beginSend())
        #expect(g.count == 3 && g.from == t.addingTimeInterval(-5) && g.to == t.addingTimeInterval(10))
        #expect(b.beginSend() == nil)  // one send in flight at a time
        b.settle(sent: false)  // refused for now (503): it stays, also on disk
        #expect(UsageGapJournal(file: file).hasPending)
        _ = b.beginSend(); b.settle(sent: true)
        #expect(!b.hasPending && !FileManager.default.fileExists(atPath: file.path))
    }

    // Q5: the id of a gap is chosen once and kept in the file; the core stores it by id, so a lost answer and a resend are one gap.
    @Test func aGapKeepsItsIDAcrossRestartsAndLaterLossesAreAnotherGap() throws {
        let file = tempURL("usage-lost.json")
        let t = Date(timeIntervalSince1970: 1_700_000_000)
        let a = UsageGapJournal(file: file)
        a.add(count: 2, from: t, to: t.addingTimeInterval(5))
        let first = try #require(a.beginSend())
        #expect(first.id.hasPrefix("gap_") && first.id.count == 16 && first.count == 2)
        a.add(count: 4, from: t, to: t.addingTimeInterval(9))  // while the first may already be stored: a second gap, never a bigger first
        a.settle(sent: false)  // the answer was lost
        let b = UsageGapJournal(file: file)  // the app restarted
        let again = try #require(b.beginSend())
        #expect(again.id == first.id && again.count == 2)  // the same gap, the same id
        b.settle(sent: true)
        let second = try #require(b.beginSend())
        #expect(second.id != first.id && second.count == 4)
        // an older file (one gap, no id) still loads, with an id of its own
        let old = tempURL("old.json")
        try Data(#"{"count":5,"from":"2026-11-01T10:00:00.000Z","to":"2026-11-01T11:00:00.000Z"}"#.utf8).write(to: old)
        let g = try #require(UsageGapJournal(file: old).beginSend())
        #expect(g.count == 5 && g.id.count == 16)
    }

    @Test func aResendAfterALostAnswerIsStoredOnce() async throws {
        let file = tempURL("usage-lost.json")
        let store = tempURL("core5").deletingLastPathComponent().appendingPathComponent("core")
        try CoreHelper.prepareStoreFolder(store)
        let h = CoreHelper(binary: try await HelperBuild.shared.binary(), store: store, ownerName: "tester", marks: MemoryMarks(), backoff: [0.05, 0.1], requestTimeout: 10)
        h.start(); defer { h.stop() }
        #expect(await h.waitReady(timeout: 20))
        // the app sent this gap and the answer was lost: the helper has it, the app's journal still holds it (same id)
        let j = UsageGapJournal(file: file)
        j.add(count: 3, from: Date().addingTimeInterval(-600), to: Date())
        let g = try #require(j.beginSend())
        let r = await h.internalCall("event-lost", ["id": g.id, "count": g.count, "from": UsageEvent.iso(g.from), "to": UsageEvent.iso(g.to)])
        #expect(r.status == 200, "stored means durable: \(r.status)")
        j.settle(sent: false)  // ... and the app never saw the 200
        let sender = UsageSender(core: h, limit: 100, retry: .milliseconds(20), journal: UsageGapJournal(file: file))  // the next start sends it again
        #expect(await sender.flush())
        try await Task.sleep(for: .milliseconds(300))
        let total = (await gaps(h)).reduce(0) { $0 + $1.count }
        #expect(total == 3, "counted \(total), want 3 once")
        #expect(!FileManager.default.fileExists(atPath: file.path))  // removed on the OK
    }
}
