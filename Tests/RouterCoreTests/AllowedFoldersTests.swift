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
    #expect(ClaudeCLI.resolveCwd(folder.path, roots: c.claudeCLI.allowedRoots) != nil)  // the raw list would have trusted it
}
