import Foundation
import Testing
@testable import RouterCore

/// #82 (vmesno, kot folderlock_test.go): zaklep mape v začasni mapi, nikoli v pravi Application Support.
private func tempDir(_ name: String) throws -> URL {
    let u = FileManager.default.temporaryDirectory.appendingPathComponent("minigw-test-\(name)-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: u, withIntermediateDirectories: true)
    return u.resolvingSymlinksInPath()
}

/// GET in PROPFIND, ki dirkata z zamenjavo nadrejene mape pod izključnim zaklepom druge seje (svoj FolderLock, svoja
/// datoteka), nikoli ne bereta zunaj korena.
@Test func folderLockReadNeverRacesParentSwap() throws {
    let fm = FileManager.default
    let root = try tempDir("root"), out = try tempDir("out"), locks = try tempDir("locks")
    defer { for d in [root, out, locks] { try? fm.removeItem(at: d) } }
    try fm.createDirectory(at: root.appendingPathComponent("a"), withIntermediateDirectories: true)
    try Data("inside".utf8).write(to: root.appendingPathComponent("a/f.txt"))
    try Data("CANARY".utf8).write(to: out.appendingPathComponent("f.txt"))
    try Data().write(to: out.appendingPathComponent("CANARY-name"))
    try fm.createSymbolicLink(at: root.appendingPathComponent("link"), withDestinationURL: out)
    let id = try #require(SharedFolders.fileID(root.path))
    let mine = FolderLock(id: id, dir: locks.path), other = FolderLock(id: id, dir: locks.path)
    let p = { (n: String) in root.appendingPathComponent(n).path }
    let stop = NSLock(), state = NSLock()
    nonisolated(unsafe) var stopped = false, swaps = 0, leaks = 0, bad = 0
    let swapper = Thread {
        while !stop.withLock({ stopped }) {
            let did = other.with(exclusive: true) {
                rename(p("a"), p("tmp")); rename(p("link"), p("a"))
                usleep(50)
                rename(p("a"), p("link")); rename(p("tmp"), p("a"))
            }
            if did != nil { state.withLock { swaps += 1 } }
        }
    }
    swapper.start()
    DispatchQueue.concurrentPerform(iterations: 4) { _ in
        for _ in 0..<300 {
            for (m, path) in [("GET", "/s/a/f.txt"), ("PROPFIND", "/s/a")] {
                let r = mine.with(exclusive: false) {
                    WebDAV.handle(method: m, path: path, headers: ["depth": "1"], body: Data(), root: root, prefix: "/s")
                }
                state.withLock {
                    if let r, String(decoding: r.body, as: UTF8.self).contains("CANARY") { leaks += 1 } else if (r?.status ?? 503) >= 300 { bad += 1 }
                }
            }
        }
    }
    stop.withLock { stopped = true }
    while !swapper.isFinished { usleep(1000) }
    #expect(leaks == 0, "reads returned the canary outside the shared folder")
    #expect(bad == 0)
    #expect(swaps > 0, "the folder was never swapped during the reads")
}

/// Bralci skupaj, pisanje čaka bralca in po wait vrne nil (503); po odklepu gre.
@Test func folderLockSharedAndExclusive() throws {
    let locks = try tempDir("locks")
    defer { try? FileManager.default.removeItem(at: locks) }
    let id: [UInt64] = [1, 2]
    let a = FolderLock(id: id, dir: locks.path), b = FolderLock(id: id, dir: locks.path), w = FolderLock(id: id, dir: locks.path, wait: 0.1)
    let inner = a.with(exclusive: false) { b.with(exclusive: false) { w.with(exclusive: true) { 1 } } }
    #expect(inner == .some(.some(nil)), "two readers together; the writer waits and gives up")
    #expect(w.with(exclusive: true) { 1 } == 1)
}

/// Drug proces (lockf(1), flock kot minigw Go) drži izključni zaklep: posel po wait vrne nil; ko je proces ubit, ga
/// jedro sprosti.
@Test func folderLockAcrossProcesses() throws {
    let locks = try tempDir("locks")
    defer { try? FileManager.default.removeItem(at: locks) }
    let id: [UInt64] = [3, 4]
    let lock = FolderLock(id: id, dir: locks.path, wait: 0.3)
    let other = Process()
    other.executableURL = URL(fileURLWithPath: "/usr/bin/lockf")
    other.arguments = ["-k", lock.path, "/bin/sh", "-c", "echo locked; exec sleep 60"]
    let pipe = Pipe()
    other.standardOutput = pipe
    try other.run()
    defer { if other.isRunning { other.terminate() } }
    #expect(pipe.fileHandleForReading.availableData == Data("locked\n".utf8))
    let t0 = Date()
    #expect(lock.with(exclusive: false) { 1 } == nil)
    #expect(Date().timeIntervalSince(t0) >= 0.3)
    kill(other.processIdentifier, SIGKILL)
    other.waitUntilExit()
    #expect(lock.with(exclusive: false) { 1 } == 1)
}

/// L2: ime datoteke zaklepa kot Go (TestFolderLockNameMatchesSwift): st_dev z visokim bitom kot UInt32.
@Test func folderLockNameMatchesGo() {
    #expect(FolderLock.name([UInt64(UInt32(bitPattern: -5)), 0x1234]) == "fffffffb-1234.lock")
}

/// L3: tudi zaklep v procesu le do roka.
@Test func folderLockInProcessTimeout() throws {
    let locks = try tempDir("locks")
    defer { try? FileManager.default.removeItem(at: locks) }
    let l = FolderLock(id: [5, 6], dir: locks.path)
    let t0 = Date()
    let inner = l.with(exclusive: true) { l.with(exclusive: false, wait: 0.1) { 1 } }
    #expect(inner == .some(nil) && Date().timeIntervalSince(t0) >= 0.1)
}

/// L1: mapa zaklepov, v katero sme pisati kdor koli, ni uporabljena (le zaklep v procesu, opozorilo).
@Test func folderLockRefusesSharedLockDir() throws {
    let base = try tempDir("locks"), d = base.appendingPathComponent("locks")
    defer { try? FileManager.default.removeItem(at: base) }
    try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true)
    chmod(d.path, 0o777)
    #expect(FolderLock(id: [7, 8], dir: d.path).with(exclusive: true) { 1 } == 1)
    #expect(try FileManager.default.contentsOfDirectory(atPath: d.path).isEmpty)
}
