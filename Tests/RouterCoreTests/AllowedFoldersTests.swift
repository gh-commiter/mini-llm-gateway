import Foundation
import Testing
@testable import RouterCore

private func allowedTemp() throws -> URL {
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    return try #require(Sync.real(dir))
}

@Test func allowedFolderIdentity() throws {
    let fm = FileManager.default, base = try allowedTemp()
    defer { try? fm.removeItem(at: base) }
    for parent in [false, true] {
        let root = base.appendingPathComponent(UUID().uuidString)
        let folder = parent ? root.appendingPathComponent("child") : root
        let target = base.appendingPathComponent(UUID().uuidString)
        try fm.createDirectory(at: folder, withIntermediateDirectories: true)
        try fm.createDirectory(at: parent ? target.appendingPathComponent("child") : target, withIntermediateDirectories: true)
        let check = AllowedFolders(), p = folder.path
        check.record(p)
        #expect(check.folders(p) == [p])
        try fm.removeItem(at: root)
        try fm.createSymbolicLink(at: root, withDestinationURL: target)
        var logs: [String] = []
        for _ in 0..<2 {
            check.record(p) // An unrelated settings save must not trust a replacement.
            #expect(check.folders(p, log: { logs.append($0) }).isEmpty)
        }
        #expect(logs.count == 1)
        #expect(logs.first?.contains("its real path changed to ") == true)
        check.record("")
        check.record(p)
        #expect(check.folders(p) == [try #require(Sync.real(folder)).path])
    }
}

@Test func allowedFolderAppears() throws {
    let fm = FileManager.default, base = try allowedTemp()
    defer { try? fm.removeItem(at: base) }
    for link in [false, true] {
        let folder = base.appendingPathComponent(UUID().uuidString)
        let check = AllowedFolders(), p = folder.path
        check.record(p)
        #expect(check.folders(p).isEmpty)
        if link { try fm.createSymbolicLink(at: folder, withDestinationURL: base) }
        else { try fm.createDirectory(at: folder, withIntermediateDirectories: false) }
        #expect(check.folders(p).isEmpty == link)
    }
    // A retained alias that disappears must subsequently have no link in its path.
    let alias = base.appendingPathComponent("alias")
    try fm.createSymbolicLink(at: alias, withDestinationURL: base)
    let check = AllowedFolders(), p = alias.path
    check.record(p)
    try fm.removeItem(at: alias)
    #expect(check.folders(p).isEmpty)
    try fm.createSymbolicLink(at: alias, withDestinationURL: base)
    var logs: [String] = []
    #expect(check.folders(p, log: { logs.append($0) }).isEmpty)
    #expect(logs.first?.contains("it is reached through a link to ") == true)
}

@Test func allowedFolderStatError() throws {
    let fm = FileManager.default, base = try allowedTemp()
    defer { try? fm.removeItem(at: base) }
    let alias = base.appendingPathComponent("alias")
    try fm.createSymbolicLink(at: alias, withDestinationURL: base)
    let check = AllowedFolders(), p = alias.path
    check.record(p)
    for code in [EACCES, EIO, ENOTDIR, 0] as [Int32] {
        #expect(check.folders(p, inspect: { _ in (false, code) }).isEmpty)
        #expect(check.folders(p) == [base.path]) // A false missing mark would reject the alias.
    }
}

@Test func allowedFolderForeignOwner() throws {
    let check = AllowedFolders()
    let p = "/usr/bin"
    var st = stat()
    #expect(lstat(p, &st) == 0)
    guard st.st_uid != getuid() else { return }
    check.record(p)
    var logs: [String] = []
    #expect(check.folders(p, log: { logs.append($0) }).isEmpty)
    #expect(logs.first?.contains("it is a link or owned by another user") == true)
}

@Test func allowedFolderExclusions() throws {
    let fm = FileManager.default, base = try allowedTemp()
    defer { try? fm.removeItem(at: base) }
    let folder = base.appendingPathComponent("folder")
    try fm.createDirectory(at: folder, withIntermediateDirectories: false)
    let check = AllowedFolders(), p = folder.path
    check.record(p)
    #expect(check.folders(p, excluding: p).isEmpty)
    #expect(check.folders(p, scratch: folder.appendingPathComponent("scratch")).isEmpty)
    let config = NSHomeDirectory() + "/Library/Preferences"
    check.record(config)
    #expect(check.folders(config).isEmpty)
    #expect(AllowedFolders.paths(" , /, relative").isEmpty)
}

@Test func allowedFolderSnapshotAfterLinkSwap() throws {
    let fm = FileManager.default, base = try allowedTemp()
    defer { try? fm.removeItem(at: base) }
    for parent in [false, true] {
        let root = base.appendingPathComponent(UUID().uuidString)
        let target = root.appendingPathComponent("first")
        let other = root.appendingPathComponent("second")
        let alias = root.appendingPathComponent("alias")
        for dir in [target, other] {
            try fm.createDirectory(at: dir.appendingPathComponent("child"), withIntermediateDirectories: true)
        }
        try fm.createSymbolicLink(at: alias, withDestinationURL: target)
        let folder = parent ? alias.appendingPathComponent("child") : alias
        let real = parent ? target.appendingPathComponent("child") : target
        let check = AllowedFolders()
        check.record(folder.path)
        let checked = check.checked(folder.path)
        #expect(checked.paths == [real.path])
        // Swap after validation and before building sandbox arguments.
        try fm.removeItem(at: alias)
        try fm.createSymbolicLink(at: alias, withDestinationURL: other)
        var cfg = ClaudeCLIConfig()
        cfg.allowLocalTools = true
        cfg.allowedRoots = folder.path
        let args = cfg.agentArgs(cwd: base, allTools: [], folders: check, checkedPaths: checked.paths)
        #expect(args.contains(real.path))
        #expect(!args.joined(separator: " ").contains(alias.path))
        #expect(!args.joined(separator: " ").contains(other.path))
        let held = try checked.open()
        defer { for handle in held { try? handle.close() } }
        try fm.moveItem(at: real, to: real.appendingPathExtension("old"))
        try fm.createDirectory(at: real, withIntermediateDirectories: false)
        #expect(throws: (any Error).self) { _ = try checked.open() }
    }
}

@Test func allowedFolderOpenRejectsFinalLink() throws {
    let fm = FileManager.default, base = try allowedTemp()
    defer { try? fm.removeItem(at: base) }
    let folder = base.appendingPathComponent("folder")
    let moved = base.appendingPathComponent("moved")
    try fm.createDirectory(at: folder, withIntermediateDirectories: false)
    let check = AllowedFolders()
    check.record(folder.path)
    let checked = check.checked(folder.path)
    #expect(checked.paths == [folder.path])
    try fm.moveItem(at: folder, to: moved)
    try fm.createSymbolicLink(at: folder, withDestinationURL: moved)
    // The same inode behind a new final link is still refused.
    #expect(throws: (any Error).self) { _ = try checked.open() }
}

@Test func siriRefusesNestedFolderReplacedByLink() throws {
    let fm = FileManager.default, base = try allowedTemp()
    defer { try? fm.removeItem(at: base) }
    let parent = base.appendingPathComponent("parent"), folder = parent.appendingPathComponent("folder")
    let target = base.appendingPathComponent("target")
    try fm.createDirectory(at: folder, withIntermediateDirectories: true)
    try fm.createDirectory(at: target, withIntermediateDirectories: true)
    var c = RouterConfig()
    c.claudeCLI.allowedRoots = folder.path
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    #expect(router.localMentionFolder(folder.path)?.path == folder.path)
    try fm.removeItem(at: folder)
    try fm.createSymbolicLink(at: folder, withDestinationURL: target)
    #expect(router.localMentionFolder(folder.path) == nil)
    #expect(ClaudeCLI.resolveCwd(folder.path, roots: AllowedFolders.list(c.claudeCLI.allowedRoots)) != nil)  // the raw list would have trusted it
}

// M2: the record of an allowed folder is saved with the settings and enforced at every start.
@Test func allowedFolderRecordAcrossRestart() throws {
    let fm = FileManager.default, base = try allowedTemp()
    defer { try? fm.removeItem(at: base) }
    let folder = base.appendingPathComponent("folder"), other = base.appendingPathComponent("other")
    try fm.createDirectory(at: folder, withIntermediateDirectories: false)
    try fm.createDirectory(at: other, withIntermediateDirectories: false)
    let p = folder.path

    // An old setting without records is migrated once, then enforced.
    var cfg = try JSONDecoder().decode(ClaudeCLIConfig.self, from: Data(#"{"allowedRoots":"\#(p)"}"#.utf8))
    #expect(cfg.allowedRecords.isEmpty)
    #expect(cfg.recordFolders().map(\.folder) == [p])
    #expect(cfg.recordFolders().isEmpty)
    let saved = try JSONDecoder().decode(ClaudeCLIConfig.self, from: JSONEncoder().encode(cfg))
    #expect(saved.allowedRecords[p]?.real == p)

    // Unchanged: accepted after a restart (a new AllowedFolders).
    let again = AllowedFolders()
    again.record(p, records: saved.allowedRecords)
    #expect(again.folders(p) == [p])

    // Retargeted while stopped: refused, logged once; the record is not rewritten.
    try fm.removeItem(at: folder)
    try fm.createSymbolicLink(at: folder, withDestinationURL: other)
    var logs: [String] = []
    let restarted = AllowedFolders()
    restarted.record(p, records: saved.allowedRecords)
    for _ in 0..<2 { #expect(restarted.folders(p, log: { logs.append($0) }).isEmpty) }
    #expect(logs.count == 1 && logs[0].contains("its real path changed to "))
    var kept = saved
    kept.recordFolders()
    #expect(kept.allowedRecords == saved.allowedRecords)

    // Removed and added again: accepted with the new target.
    cfg = saved
    cfg.allowedRoots = ""
    cfg.recordFolders()
    #expect(cfg.allowedRecords.isEmpty)
    cfg.allowedRoots = p
    #expect(cfg.recordFolders().first?.real == other.path)
    let readded = AllowedFolders()
    readded.record(p, records: cfg.allowedRecords)
    #expect(readded.folders(p) == [other.path])
}

@Test func allowedFolderIdentityChangedAtStart() throws {
    let fm = FileManager.default, base = try allowedTemp()
    defer { try? fm.removeItem(at: base) }
    let folder = base.appendingPathComponent("folder")
    try fm.createDirectory(at: folder, withIntermediateDirectories: false)
    var cfg = ClaudeCLIConfig()
    cfg.allowedRoots = folder.path
    cfg.recordFolders()
    try fm.moveItem(at: folder, to: base.appendingPathComponent("old"))
    try fm.createDirectory(at: folder, withIntermediateDirectories: false)
    var logs: [String] = []
    let restarted = AllowedFolders()
    restarted.record(folder.path, records: cfg.allowedRecords)
    #expect(restarted.folders(folder.path, log: { logs.append($0) }).isEmpty)
    #expect(logs.first?.contains("its identity changed") == true)
}

// A1: the recorded identity is enforced on every check, also for a folder that was missing at start.
@Test func allowedFolderIdentityEnforcedAtRunTime() throws {
    let fm = FileManager.default, base = try allowedTemp()
    defer { try? fm.removeItem(at: base) }
    let folder = base.appendingPathComponent("folder"), old = base.appendingPathComponent("old"), p = folder.path
    try fm.createDirectory(at: folder, withIntermediateDirectories: false)
    var cfg = ClaudeCLIConfig()
    cfg.allowedRoots = p
    cfg.recordFolders()
    let check = AllowedFolders()
    check.record(p, records: cfg.allowedRecords)
    #expect(check.folders(p) == [p])
    // Swapped after start: refused (logged once) until the original is back.
    try fm.moveItem(at: folder, to: old)
    try fm.createDirectory(at: folder, withIntermediateDirectories: false)
    var logs: [String] = []
    for _ in 0..<2 { #expect(check.folders(p, log: { logs.append($0) }).isEmpty) }
    #expect(logs.count == 1 && logs[0].contains("its identity changed"))
    try fm.removeItem(at: folder)
    try fm.moveItem(at: old, to: folder)
    #expect(check.folders(p) == [p])
    // Missing at start, another folder appears: refused; the original returns: accepted.
    try fm.moveItem(at: folder, to: old)
    let restarted = AllowedFolders()
    restarted.record(p, records: cfg.allowedRecords)
    #expect(restarted.folders(p).isEmpty)
    try fm.createDirectory(at: folder, withIntermediateDirectories: false)
    #expect(restarted.folders(p, log: { _ in }).isEmpty)
    try fm.removeItem(at: folder)
    try fm.moveItem(at: old, to: folder)
    #expect(restarted.folders(p) == [p])
}

// B1: the identity is the volume UUID plus the inode (never st_dev); no UUID: the inode alone.
@Test func allowedFolderRecordUsesVolumeUUIDAndInode() throws {
    let fm = FileManager.default, base = try allowedTemp()
    defer { try? fm.removeItem(at: base) }
    let folder = base.appendingPathComponent("folder"), p = folder.path
    try fm.createDirectory(at: folder, withIntermediateDirectories: false)
    var cfg = ClaudeCLIConfig()
    cfg.allowedRoots = p
    cfg.recordFolders(volumeID: { _ in "UUID-A" })
    #expect(cfg.allowedRecords[p]?.volume == "UUID-A" && cfg.allowedRecords[p]?.ino != 0)
    func check(_ uuid: String?) -> [String] {
        let c = AllowedFolders(volumeID: { _ in uuid })
        c.record(p, records: cfg.allowedRecords)
        return c.folders(p)
    }
    #expect(check("UUID-A") == [p])  // the same volume, whatever st_dev says after a reconnect
    #expect(check("UUID-B").isEmpty)  // another disk
    #expect(check(nil) == [p])  // no UUID now: inode only
    // An old record (dev and ino, no volume) still reads and is compared by inode.
    let old = try JSONDecoder().decode(FolderRecord.self, from: Data(#"{"real":"\#(p)","dev":7,"ino":\#(cfg.allowedRecords[p]!.ino)}"#.utf8))
    #expect(old.volume == nil && old.ino == cfg.allowedRecords[p]?.ino)
    // Another folder (another inode) at the same path is refused.
    try fm.moveItem(at: folder, to: base.appendingPathComponent("old"))
    try fm.createDirectory(at: folder, withIntermediateDirectories: false)
    #expect(check("UUID-A").isEmpty)
}

// B2: a record without identity adopts the identity at its first accepted use (and tells its owner to save it).
@Test func allowedFolderRecordWithoutIdentityIsAdopted() throws {
    let fm = FileManager.default, base = try allowedTemp()
    defer { try? fm.removeItem(at: base) }
    let folder = base.appendingPathComponent("folder"), p = folder.path
    try fm.createDirectory(at: folder, withIntermediateDirectories: false)
    let check = AllowedFolders(volumeID: { _ in "UUID-A" })
    final class Saved: @unchecked Sendable { var rec: FolderRecord? }
    let saved = Saved()
    check.onAdopt = { _, rec in saved.rec = rec }
    check.record(p, records: [p: FolderRecord(real: p)])
    #expect(check.folders(p) == [p])
    #expect(saved.rec?.volume == "UUID-A" && saved.rec?.ino != 0)
    try fm.moveItem(at: folder, to: base.appendingPathComponent("old"))
    try fm.createDirectory(at: folder, withIntermediateDirectories: false)
    #expect(check.folders(p, log: { _ in }).isEmpty)
}

// The checked real path of an allowed folder may contain a comma: it must not be split like the configured list.
@Test func checkedRootWithCommaIsKept() throws {
    let fm = FileManager.default, base = try allowedTemp()
    defer { try? fm.removeItem(at: base) }
    let real = base.appendingPathComponent("a,b"), alias = base.appendingPathComponent("alias")
    try fm.createDirectory(at: real.appendingPathComponent("proj"), withIntermediateDirectories: true)
    try fm.createSymbolicLink(at: alias, withDestinationURL: real)
    var c = RouterConfig()
    c.claudeCLI.allowedRoots = alias.path
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    #expect(router.checkedRoots == [real.path])
    let proj = real.appendingPathComponent("proj").path
    #expect(router.localMentionFolder(proj)?.path == proj)
    #expect(ClaudeCLI.resolveCwd(proj, roots: router.checkedRoots) != nil)
    #expect(Native.resolve(share: "S", path: "proj", shares: ["S": real.path], roots: router.checkedRoots)?.path == proj)
}
