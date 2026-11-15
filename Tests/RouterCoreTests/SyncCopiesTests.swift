import Foundation
import Testing
@testable import RouterCore

@Test func syncCopiesManagement() async throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let hub = ShareHub()
    let owner = root.appendingPathComponent("local")
    for i in 0..<Sync.maxCopies {
        let copy = owner.appendingPathComponent(String(format: "%016x", i))
        try FileManager.default.createDirectory(at: copy, withIntermediateDirectories: true)
        ShareHub.touchUsed(copy)
    }
    let next = owner.appendingPathComponent("ffffffffffffffff")
    #expect(Sync.tooManyCopies(next))
    let copies = try await hub.syncCopies(root, device: "local")
    #expect(copies.count == Sync.maxCopies)
    _ = try await hub.syncCopies(root, device: "local", remove: copies[0])
    #expect(!Sync.tooManyCopies(next))
    #expect(!FileManager.default.fileExists(atPath: owner.appendingPathComponent(copies[0] + ".used").path))
    do {
        _ = try await hub.syncCopies(root, device: "local", remove: "../outside")
        Issue.record("accepted invalid copy ID")
    } catch let e as AdminError { #expect(e.status == 400) }
    let linked = root.appendingPathComponent("linked")
    try FileManager.default.createSymbolicLink(at: linked, withDestinationURL: owner)
    do {
        _ = try await hub.syncCopies(root, device: "linked")
        Issue.record("accepted linked owner")
    } catch let e as AdminError { #expect(e.status == 400) }
    _ = await hub.register(session: "session", cwd: "/folder", key: nil)
    do {
        _ = try await hub.syncCopies(root, device: "local", remove: copies[1])
        Issue.record("removed a copy during an active session")
    } catch let e as AdminError { #expect(e.status == 409) }
}
