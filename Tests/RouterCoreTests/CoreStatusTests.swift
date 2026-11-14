import Foundation
import Testing
@testable import RouterCore

// 0.6.0 M3: the `core` field of the admin status (the line `minigw gw status` and the Settings window show), over the REAL minigw-core
// (HelperBuild of CoreHelperTests) with temp stores only.

private func store(_ name: String) throws -> URL {
    let u = FileManager.default.temporaryDirectory.appendingPathComponent("mgstatus-\(name)-\(UUID().uuidString.prefix(8))", isDirectory: true).appendingPathComponent("core")
    try CoreHelper.prepareStoreFolder(u)
    return u
}

private func started(_ binary: URL?, _ store: URL) async -> CoreHelper {
    let h = CoreHelper(binary: binary, store: store, ownerName: "tester", marks: MemoryMarks(), backoff: [0.05, 0.1, 0.2], requestTimeout: 10)
    h.start()
    return h
}

@Suite(.serialized) struct CoreStatusLine {
    @Test func theLineForEachState() {
        func l(up: Bool = true, failure: String = "", fatal: CoreError? = nil, store: String? = nil, link: String? = nil) -> String {
            CoreStatus.line(helperUp: up, failure: failure, fatal: fatal, storeReason: store, linkProblem: link)
        }
        #expect(l() == "projects: on")
        #expect(l(up: false) == "projects: off (the project helper (minigw-core) is not running)")
        #expect(l(up: false, failure: #"unavailable("minigw-core not found")"#) == "projects: off (the project helper (minigw-core) is not running: minigw-core not found)")
        #expect(l(fatal: .versionMismatch("the helper speaks protocol 2")) == "projects: off (the helper speaks protocol 2)")
        #expect(l(fatal: .peerNotAuthentic).hasPrefix("projects: off (the project helper could not prove"))
        #expect(l(store: "core database is in use by another process") == "projects: off (core database is in use by another process)")
        #expect(l(link: "/x has another name") == "projects: on; agent runs are refused: /x has another name")
        #expect(CoreStatus.storeReason(status: 503, body: Data(#"{"error":{"message":"The project database is not available: newer schema"}}"#.utf8)) == "newer schema")
        #expect(CoreStatus.storeReason(status: 503, body: CoreResponse.unavailable.body) == nil)  // the helper itself is down
        #expect(CoreStatus.storeReason(status: 200, body: Data()) == nil)
    }

    @Test func noHelperConfiguredHasNoCoreField() async throws {
        let w = await admin(FakeApp().router(), "GET", "/status")
        #expect(w.status == 200 && !w.text.contains(#""core""#))
    }

    @Test func aHelperThatIsNotRunningIsSaid() async throws {
        let h = CoreHelper(binary: nil, store: try store("down"), marks: MemoryMarks())  // configured, never started
        let w = await admin(FakeApp().router(core: h), "GET", "/status")
        #expect(w.status == 200 && w.text.contains(#""core":"projects: off (the project helper (minigw-core) is not running)""#), "\(w.text)")
    }

    @Test func aReadyHelperIsOnAndAnUnopenableStoreSaysWhy() async throws {
        let bin = try await HelperBuild.shared.binary()
        let dir = try store("ok")
        let first = await started(bin, dir); defer { first.stop() }
        #expect(await first.waitReady())
        let w = await admin(FakeApp().router(core: first), "GET", "/status")
        #expect(w.text.contains(#""core":"projects: on""#), "\(w.text)")

        // a second helper on the same store cannot open it (the first holds the lock): up, 503 with the reason
        let second = await started(bin, dir); defer { second.stop() }
        #expect(await second.waitReady())
        let line = await CoreStatus.line(for: second)
        #expect(line.hasPrefix("projects: off (") && line.contains("in use by another process"), "\(line)")
        let w2 = await admin(FakeApp().router(core: second), "GET", "/status")
        #expect(w2.text.contains(#""core":"projects: off ("#), "\(w2.text)")
    }

    @Test func aHardLinkMadeWhileRunningIsShown() async throws {
        let bin = try await HelperBuild.shared.binary()
        let dir = try store("link")
        let h = await started(bin, dir); defer { h.stop() }
        #expect(await h.waitReady())
        let elsewhere = FileManager.default.temporaryDirectory.appendingPathComponent("mgstatus-alias-\(UUID().uuidString.prefix(8))")
        try FileManager.default.linkItem(at: dir.appendingPathComponent("core.db"), to: elsewhere)
        defer { try? FileManager.default.removeItem(at: elsewhere) }
        let line = await CoreStatus.line(for: h)
        #expect(line.hasPrefix("projects: on; agent runs are refused: ") && line.contains("another name outside the core folder"), "\(line)")
    }
}
