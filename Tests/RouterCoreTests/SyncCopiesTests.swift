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
    // Sessions and agents elsewhere do not matter: another copy's session (and a running agent anywhere) does not block.
    _ = await hub.register(session: "other", cwd: "/other", key: nil, copy: owner.appendingPathComponent(copies[5]))
    _ = try await hub.syncCopies(root, device: "local", remove: copies[2])
    // A session on this very copy does, with the text the Go side prints.
    _ = await hub.register(session: "session", cwd: "/folder", key: nil, copy: owner.appendingPathComponent(copies[1]))
    let fixed = String(decoding: try Data(contentsOf: URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        .appendingPathComponent("client/testdata/sync_copy_in_use.txt")), as: UTF8.self)
    do {
        _ = try await hub.syncCopies(root, device: "local", remove: copies[1])
        Issue.record("removed a copy during an active session")
    } catch let e as AdminError { #expect(e.status == 409 && e.message == fixed, "\(e.status) \(e.message)") }
    #expect(FileManager.default.fileExists(atPath: owner.appendingPathComponent(copies[1]).path))
    #expect(!FileManager.default.fileExists(atPath: owner.appendingPathComponent(copies[2]).path))
}
