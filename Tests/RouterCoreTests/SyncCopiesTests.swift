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

@Test func syncCopyLeftoverOfACrashedRemovalIsNeitherListedNorCountedAndIsCleaned() async throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let owner = root.appendingPathComponent("local"), fm = FileManager.default
    for i in 0..<(Sync.maxCopies - 1) { try fm.createDirectory(at: owner.appendingPathComponent(String(format: "%016x", i)), withIntermediateDirectories: true) }
    let left = owner.appendingPathComponent("0123456789abcdef.removing-1700000000000000000")
    let leftRO = owner.appendingPathComponent("fedcba9876543210-ro.removing-5")
    let decoy = owner.appendingPathComponent("0123456789abcdef.removing-x")
    for d in [left, leftRO, decoy] { try fm.createDirectory(at: d.appendingPathComponent("sub"), withIntermediateDirectories: true) }
    ShareHub.touchUsed(owner.appendingPathComponent("0123456789abcdef"))
    let outside = root.appendingPathComponent("outside")
    try fm.createDirectory(at: outside, withIntermediateDirectories: true)
    try Data("k".utf8).write(to: outside.appendingPathComponent("keep"))
    let link = owner.appendingPathComponent("aaaaaaaaaaaaaaaa.removing-9")
    try fm.createSymbolicLink(at: link, withDestinationURL: outside)
    #expect(!Sync.tooManyCopies(owner.appendingPathComponent("ffffffffffffffff")))
    let hub = ShareHub()
    #expect(try await hub.syncCopies(root, device: "local").count == Sync.maxCopies - 1)
    ShareHub.removeLeftovers(owner: owner)
    #expect(!fm.fileExists(atPath: left.path) && !fm.fileExists(atPath: leftRO.path))
    #expect(!fm.fileExists(atPath: owner.appendingPathComponent("0123456789abcdef.used").path))
    #expect(fm.fileExists(atPath: decoy.path))
    #expect((try? fm.destinationOfSymbolicLink(atPath: link.path)) != nil && fm.fileExists(atPath: outside.appendingPathComponent("keep").path))
    #expect(!ShareHub.isLeftoverName("0123456789abcdef") && !ShareHub.isLeftoverName("0123456789abcdef-ro") && ShareHub.isLeftoverName("0123456789abcdef-ro.removing-1"))
}

/// Astra finding 2 (Mac side): syncManifest suspends in its scan; the session is unregistered meanwhile and the copy removed.
/// Controlled interleaving with a hook in the scan (no sleeps).
@Test func syncCopyRemovalWaitsForAManifestScanOfAnUnregisteredSession() async throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let hub = ShareHub()
    let id = "0123456789abcdef"
    let copy = root.appendingPathComponent("dev").appendingPathComponent(id)
    try FileManager.default.createDirectory(at: copy, withIntermediateDirectories: true)
    let key = try #require(await hub.register(session: "s", cwd: "/c", key: nil, copy: copy, device: "dev"))
    let entered = AsyncStream<Void>.makeStream(), release = AsyncStream<Void>.makeStream()
    await hub.setScanHook {
        entered.continuation.yield()
        for await _ in release.stream { break }
    }
    let manifest = Task { await hub.syncManifest(key: key, [], device: "dev") }
    for await _ in entered.stream { break }  // the manifest is inside its scan, off the actor
    await hub.unregister(key: key)
    do {
        _ = try await hub.syncCopies(root, device: "dev", remove: id)
        Issue.record("removed a copy under a running manifest scan")
    } catch let e as AdminError { #expect(e.status == 409) }
    #expect(FileManager.default.fileExists(atPath: copy.path))
    release.continuation.yield()
    _ = await manifest.value
    _ = try await hub.syncCopies(root, device: "dev", remove: id)  // after the scan ended the copy is free
    #expect(!FileManager.default.fileExists(atPath: copy.path))
}

/// The leftover cleanup runs detached and may delete the renamed folder (and its marker) while the removal deletes it: that is not an error.
@Test func syncCopyRemovalSurvivesTheLeftoverCleanupDeletingTheRenamedFolder() async throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let hub = ShareHub(), id = "0123456789abcdef"
    let owner = root.appendingPathComponent("dev"), copy = owner.appendingPathComponent(id)
    try FileManager.default.createDirectory(at: copy, withIntermediateDirectories: true)
    ShareHub.touchUsed(copy)
    await hub.setRemoveHook { gone in
        try? FileManager.default.removeItem(at: gone)  // the cleaner got there first ...
        try? FileManager.default.removeItem(at: copy.appendingPathExtension("used"))  // ... and took the marker of the gone copy too
    }
    _ = try await hub.syncCopies(root, device: "dev", remove: id)
    #expect(!FileManager.default.fileExists(atPath: copy.path))
}

/// Security review (LOW): the delete of a removed copy runs off the shares actor, so another share operation completes while it is still running
/// (controlled with a hook, no sleeps); the removed copy is not listed meanwhile (the renamed folder is neither listed nor counted) and the removal ends when released.
@Test func syncCopyRemovalDeletesOffTheActor() async throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let hub = ShareHub(), id = "0123456789abcdef", other = "fedcba9876543210"
    let owner = root.appendingPathComponent("dev"), copy = owner.appendingPathComponent(id)
    for d in [id, other] { try FileManager.default.createDirectory(at: owner.appendingPathComponent(d), withIntermediateDirectories: true) }
    let entered = DispatchSemaphore(value: 0), release = DispatchSemaphore(value: 0)
    await hub.setDeleteHook { _ in entered.signal(); release.wait() }  // the slow delete
    let removal = Task { try await hub.syncCopies(root, device: "dev", remove: id) }
    await withCheckedContinuation { c in Thread { entered.wait(); c.resume() }.start() }  // the delete has started, off the actor
    let listed = try await hub.syncCopies(root, device: "dev")  // another operation on the actor completes meanwhile
    #expect(listed == [other])
    #expect(!FileManager.default.fileExists(atPath: copy.path))  // renamed already
    release.signal()
    _ = try await removal.value
    await hub.setDeleteHook(nil)
    #expect(try FileManager.default.contentsOfDirectory(atPath: owner.path).sorted() == [other])
}
