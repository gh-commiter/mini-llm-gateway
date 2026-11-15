import Foundation
import Testing
@testable import RouterCore

// #114, #116 A2: a remote client's folder lives INSIDE the gateway's own folders (`…/Mini Gateway/mounts/<dir>`, `Scratch/sync/<device>/<proj>`). For every agent
// kind that has a deny of those folders the session folder must stay usable, and a neighbour (another client's mount, the app's own files) must not.

private func run(_ exe: String, _ args: [String]) -> (status: Int32, out: String) {
    let p = Process(), pipe = Pipe()
    p.executableURL = URL(fileURLWithPath: exe); p.arguments = args; p.standardOutput = pipe; p.standardError = pipe
    guard (try? p.run()) != nil else { return (-1, "cannot run \(exe)") }
    var data = Data()
    let reader = Thread { data = pipe.fileHandleForReading.readDataToEndOfFile() }
    reader.start(); p.waitUntilExit()
    while reader.isExecuting { usleep(5_000) }
    return (p.terminationStatus, String(decoding: data, as: UTF8.self))
}

private struct Layout {
    let base: URL, app: URL, s1: URL, tmp: URL
    init() throws {
        let fm = FileManager.default
        base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("inside-" + UUID().uuidString, isDirectory: true)
        app = base.appendingPathComponent("app"); s1 = app.appendingPathComponent("mounts/s1"); tmp = base.appendingPathComponent("tmp")
        for d in ["app/mounts/s1", "app/mounts/s2", "app/core", "tmp"] { try fm.createDirectory(at: base.appendingPathComponent(d), withIntermediateDirectories: true) }
        for (f, c) in [("app/mounts/s1/f.txt", "MINE"), ("app/mounts/s2/x", "OTHER-MOUNT"), ("app/mounts/s1x", "SIBLING-FILE"), ("app/state.json", "STATE"), ("app/core/core.db", "CORE")] {
            try Data(c.utf8).write(to: base.appendingPathComponent(f))
        }
    }
    func cleanup() { try? FileManager.default.removeItem(at: base) }
    var neighbours: [String] { ["app/mounts/s2/x", "app/mounts/s1x", "app/state.json", "app/core/core.db"].map { base.appendingPathComponent($0).path } }
}

@Test func codexKeepsTheSessionInsideTheAppFolderAndNotItsNeighbours() throws {
    let exe = try #require(["/opt/homebrew/bin/codex", "/usr/local/bin/codex"].first { FileManager.default.isExecutableFile(atPath: $0) })
    let l = try Layout(); defer { l.cleanup() }
    let a = Codex.args(cwd: l.s1, extraDirs: [], agent: true, output: l.base.appendingPathComponent("o"), write: true, deny: [l.app.path], tmp: l.tmp.path)
    var cfg: [String] = []
    for (i, x) in a.enumerated() where x == "-c" && i + 1 < a.count && (a[i + 1].hasPrefix("default_permissions") || a[i + 1].hasPrefix("permissions.")) { cfg += ["-c", a[i + 1]] }
    func under(_ p: String) -> (status: Int32, out: String) { run(exe, ["sandbox"] + cfg + ["--", "/bin/cat", p]) }
    #expect(under(l.s1.path + "/f.txt").out.contains("MINE"))
    for n in l.neighbours { let r = under(n); #expect(r.status != 0 && !r.out.contains("OTHER") && !r.out.contains("SIBLING") && !r.out.contains("STATE") && !r.out.contains("CORE"), "\(n): \(r)") }
}

@Test func seatbeltProfileKeepsTheSessionInsideTheAppFolderAndNotItsNeighbours() throws {
    let l = try Layout(); defer { l.cleanup() }
    let profile = try #require(Native.sandboxProfile(dir: l.s1.path, home: l.base.path, network: false, hidden: [l.app.path], writable: [l.s1.path], visible: l.s1.path))
    func under(_ p: String) -> (status: Int32, out: String) { run("/usr/bin/sandbox-exec", ["-p", profile, "/bin/cat", p]) }
    #expect(under(l.s1.path + "/f.txt").out.contains("MINE"))
    for n in l.neighbours { let r = under(n); #expect(r.status != 0 && !r.out.contains("OTHER") && !r.out.contains("SIBLING") && !r.out.contains("STATE") && !r.out.contains("CORE"), "\(n): \(r)") }
}

/// Grok reads everywhere, so the app folder is replaced by globs for every other entry on the way down (a real `grok` run with this profile read the session's file and
/// was refused every neighbour, also a file created afterwards: see the report).
@Test func grokDeniesTheNeighboursOfTheSessionInsideTheAppFolder() throws {
    let l = try Layout(); defer { l.cleanup() }
    let deny = try #require(Grok.scopedDenies(hidden: [l.app.path, l.app.path + "/core", "/Users/x/.codex"], scope: l.s1.path))
    #expect(deny.contains("/Users/x/.codex") && !deny.contains(l.app.path) && !deny.contains(l.app.path + "/core"))  // the core is covered by the app folder's entries
    func denied(_ path: String) -> Bool {  // entry itself or a parent matched by a glob (the profile adds `/**` for contents)
        deny.contains { g in fnmatch(g, path, FNM_PATHNAME) == 0 || path.hasPrefix(g + "/") }
    }
    for n in l.neighbours + [l.app.path + "/mounts/later-client/x", l.app.path + "/newfile"] { #expect(denied(n), "\(n)") }
    #expect(!denied(l.s1.path) && !denied(l.s1.path + "/f.txt") && !denied(l.s1.path + "/deep/er/file"))
    // a name that cannot be written as globs: refused (nil), not run unprotected
    #expect(Grok.scopedDenies(hidden: [l.app.path], scope: l.app.path + "/mounts/s[1]") == nil)
    // no scope inside: the roots as they are
    #expect(Grok.scopedDenies(hidden: [l.app.path], scope: l.base.path + "/tmp") == [l.app.path])
}

@Test func claudeKeepsTheSessionInsideTheAppFolderAndOnlyThat() throws {
    let app = GatewayHistory.defaultFile.deletingLastPathComponent().standardizedFileURL.path
    let cwd = app + "/mounts/s1"
    var local = ClaudeCLIConfig(); local.allowLocalTools = true; local.tools = "Read,Grep,Glob,Edit,Write"
    var restricted = ClaudeCLIConfig(); restricted.allowLocalTools = false; restricted.tools = "Read,Grep,Glob,Edit,Write"
    let sandboxed = local.agentArgs(cwd: URL(fileURLWithPath: cwd), allTools: [], folders: AllowedFolders(), checkedPaths: [])
    let json = try #require(sandboxed.firstIndex(of: "--settings").map { sandboxed[$0 + 1] })
    let st = try #require(try JSONSerialization.jsonObject(with: Data(json.utf8)) as? [String: Any])
    let fs = try #require((st["sandbox"] as? [String: Any])?["filesystem"] as? [String: Any])
    #expect(fs["allowRead"] as? [String] == [cwd] && fs["allowWrite"] as? [String] == [cwd])  // the neighbours are outside the allow side
    let r = restricted.agentArgs(cwd: URL(fileURLWithPath: cwd), allTools: [], folders: AllowedFolders(), checkedPaths: [])
    let allowed = try #require(r.firstIndex(of: "--allowedTools").map { r[$0 + 1] })
    #expect(allowed.contains("Read(./**)") && !allowed.contains("mounts/s2") && !allowed.contains("//\(app)/**"))
}

/// The profile's matching for the test: a glob names the entry, `<glob>/**` and a literal path name everything below.
func grokDenies(_ deny: [String], _ path: String) -> Bool {
    deny.contains { g in
        if g.hasSuffix("/**") { let base = String(g.dropLast(3)); return fnmatch(base, path, FNM_PATHNAME) == 0 || path.split(separator: "/").indices.contains { fnmatch(base, "/" + path.split(separator: "/").prefix($0 + 1).joined(separator: "/"), FNM_PATHNAME) == 0 } }
        return fnmatch(g, path, FNM_PATHNAME) == 0 || path.hasPrefix(g + "/")
    }
}

/// Production size (a real `grok` fails to start above about 35 long entries, so this stays small): a mount named by a 36-character id and a copy named by a
/// 43-character device and a 16-character project, with siblings that share a long prefix, and a sibling made after the list was built.
@Test func grokDenyListAtProductionSizeIsSmallDeterministicAndRight() throws {
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("grok-size-" + UUID().uuidString, isDirectory: true).path
    defer { try? fm.removeItem(atPath: base) }
    let u = "0f8e7d6c-5b4a-4938-8271-6a5b4c3d2e1f", dev = String(repeating: "d", count: 43), proj = "0123456789abcdef"
    for d in ["app/mounts/" + u, "app/mounts/" + u.dropLast() + "0", "app/mounts/" + u + "a", "app/mounts/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", "app/core",
              "scratch/sync/" + dev + "/" + proj, "scratch/sync/" + dev + "/0123456789abcdee", "scratch/sync/" + dev.dropLast() + "e/" + proj, "scratch/state"] {
        try fm.createDirectory(atPath: base + "/" + d, withIntermediateDirectories: true)
    }
    try Data("x".utf8).write(to: URL(fileURLWithPath: base + "/app/state.json"))
    for (hidden, scope, siblings, later) in [([base + "/app", base + "/app/core"], base + "/app/mounts/" + u,
                                              ["app/mounts/" + u.dropLast() + "0/x", "app/mounts/" + u + "a/x", "app/mounts/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d/x", "app/state.json", "app/core/core.db"],
                                              ["app/mounts/99999999-aaaa-4bbb-8ccc-eeeeeeeeeeee/x", "app/newfile"]),
                                             ([base + "/scratch", base + "/scratch/state"], base + "/scratch/sync/" + dev + "/" + proj,
                                              ["scratch/sync/" + dev + "/0123456789abcdee/x", "scratch/sync/" + dev.dropLast() + "e/" + proj + "/x", "scratch/state/s"],
                                              ["scratch/sync/" + dev + "/fedcba9876543210/x", "scratch/sync/" + String(repeating: "z", count: 43) + "/y/x"])] as [([String], String, [String], [String])] {
        let deny = try #require(Grok.scopedDenies(hidden: hidden, scope: scope))
        #expect(deny.filter(Grok.isGlob).count <= Grok.maxGlobEntries, "\(deny.count) entries")
        #expect(deny == Grok.scopedDenies(hidden: hidden, scope: scope), "deterministic")
        for n in siblings + later { #expect(grokDenies(deny, base + "/" + n), "\(n)") }
        #expect(!grokDenies(deny, scope) && !grokDenies(deny, scope + "/f.txt") && !grokDenies(deny, scope + "/deep/er/file"))
    }
}

/// Finding 1 of the 0.6.0 review, round 2: Grok's list holds the hidden Scratch roots too (the sync copies, worktrees and mirrors of other clients), with the way down to
/// the session carved out, for every layout a session lives in, and it stays under what a real grok starts with (measured: about 41 globs; literals are cheap). The list
/// below is the shape of the gateway's own (`Grok.deniedRoots`: agentDenied + hiddenScratch, each root literal and real) with a Scratch folder on another volume, its
/// `.old-` neighbour, the default Scratch, the app folder, the core, the credential homes.
@Test func grokDenyListFitsEveryLayoutAWorktreeSyncMountOrLocalFolderSessionLivesIn() throws {
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("grok-hidden-" + UUID().uuidString, isDirectory: true).path
    defer { try? fm.removeItem(atPath: base) }
    let dev = String(repeating: "d", count: 43), dev2 = String(repeating: "e", count: 43), proj = "0123456789abcdef", clone = "minigw-0123456789abcdef-1", u = "0f8e7d6c-5b4a-4938-8271-6a5b4c3d2e1f"
    let scratch = base + "/vol/Mini Gateway Scratch"
    for d in ["sync/" + dev + "/" + proj, "sync/" + dev + "/0123456789abcdee", "sync/" + dev2 + "/" + proj, "worktrees/" + dev + "/" + proj + "/" + clone, "worktrees/" + dev + "/" + proj + "/minigw-0123456789abcdef-2",
              "worktrees/" + dev2 + "/" + proj + "/" + clone, "state", "copilot", "grok-home"].map({ "vol/Mini Gateway Scratch/" + $0 }) +
        ["vol/Mini Gateway Scratch.old-2026-11-01", "caches/Scratch", "caches/Mini Gateway Scratch", "app/mounts/" + u, "app/mounts/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", "app/core", "home/.codex", "home/.copilot", "mine/proj"] {
        try fm.createDirectory(atPath: base + "/" + d, withIntermediateDirectories: true)
    }
    let hidden = [scratch, scratch + "/state", scratch + "/copilot", base + "/app", base + "/app/core", base + "/home/.codex", base + "/home/.copilot",
                  base + "/caches/Mini Gateway Scratch", base + "/caches/Scratch", scratch + ".old-2026-11-01"]
    let neighbours = [scratch + "/sync/" + dev + "/0123456789abcdee/x", scratch + "/sync/" + dev2 + "/" + proj + "/x", scratch + "/worktrees/" + dev2 + "/" + proj + "/" + clone + "/x",
                      scratch + "/worktrees/" + dev + "/" + proj + "/minigw-0123456789abcdef-2/x", scratch + "/sync/later/x", scratch + "/worktrees/later/x", scratch + "/state/s", scratch + "/owner.json",
                      base + "/app/mounts/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d/x", base + "/app/mounts/later/x", base + "/app/state.json", base + "/app/core/core.db",
                      base + "/home/.codex/auth.json", scratch + ".old-2026-11-01/x"]
    for scope in [scratch + "/sync/" + dev + "/" + proj, scratch + "/worktrees/" + dev + "/" + proj + "/" + clone, base + "/app/mounts/" + u, base + "/mine/proj"] {
        let deny = try #require(Grok.scopedDenies(hidden: hidden, scope: scope), "\(scope)")
        let globs = deny.filter(Grok.isGlob).count
        #expect(globs <= Grok.maxGlobEntries, "\(globs) globs in \(scope): a real grok does not start above about 41")
        for n in neighbours where !n.hasPrefix(scope + "/") { #expect(Grok.denies(deny, n), "\(n) for \(scope)") }
        #expect(!Grok.denies(deny, scope) && !Grok.denies(deny, scope + "/f.txt") && !Grok.denies(deny, scope + "/deep/er/file"), "\(scope)")
    }
}

/// Round 2, H1: a nested root is dropped only when the entries above deny it. Roots `/scratch` and `/scratch/sync/abc` with the session in `/scratch/sync/abcdefgh/project`:
/// the folder `abc` is a proper prefix of our name and must stay denied, and so must a sibling that shares our first characters and is made after the list.
@Test func grokDenyListKeepsANestedRootTheAncestorDoesNotReallyCover() throws {
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("grok-nested-" + UUID().uuidString, isDirectory: true).path
    defer { try? fm.removeItem(atPath: base) }
    for d in ["scratch/sync/abc", "scratch/sync/abcdefgh/project", "scratch/sync/abcdefgi", "scratch/other"] { try fm.createDirectory(atPath: base + "/" + d, withIntermediateDirectories: true) }
    let deny = try #require(Grok.scopedDenies(hidden: [base + "/scratch", base + "/scratch/sync/abc"], scope: base + "/scratch/sync/abcdefgh/project"))
    for n in ["scratch/sync/abc/x", "scratch/sync/abcdefgi/x", "scratch/other/x", "scratch/sync/zzz/x"] { #expect(Grok.denies(deny, base + "/" + n), "\(n)") }
    #expect(!Grok.denies(deny, base + "/scratch/sync/abcdefgh/project/f"))
    // a root that is inside the session folder is not covered by anything and stays
    let inner = try #require(Grok.scopedDenies(hidden: [base + "/scratch/sync/abcdefgh/project/.cache"], scope: base + "/scratch/sync/abcdefgh/project"))
    #expect(inner == [base + "/scratch/sync/abcdefgh/project/.cache"])
}

/// The roots Grok is given: the hidden Scratch roots are in, Grok's own login home is not.
@Test func grokDeniedRootsHoldTheHiddenScratchButNotItsOwnLoginHome() throws {
    let scratch = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("grok-roots-" + UUID().uuidString, isDirectory: true)
    try FileManager.default.createDirectory(at: scratch, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: scratch) }
    let roots = Grok.deniedRoots(scratch: scratch)
    #expect(RouterConfig.hiddenScratch(scratch).filter { !RouterConfig.credentialHomes(except: "grok").contains($0) && RouterConfig.credentialHomes().contains($0) }.allSatisfy { !roots.contains($0) })
    #expect(roots.contains(scratch.standardizedFileURL.path) && Set(RouterConfig.agentDenied(kind: "grok", scratch: scratch)).isSubset(of: Set(roots)))
    #expect(Set(RouterConfig.hiddenScratch(scratch)).subtracting(RouterConfig.credentialHomes()).isSubset(of: Set(roots)))
}

/// A list grok cannot start with is never given to it: a session so deep that even the shortest class prefixes need more globs than fit is refused.
@Test func grokDenyListThatCannotFitIsRefused() {
    let deep = "/nowhere/root" + (0..<14).map { "/level\($0)xx" }.joined()
    #expect(Grok.scopedDenies(hidden: ["/nowhere/root"], scope: deep) == nil)
    #expect(Grok.scopedDenies(hidden: ["/nowhere/root"], scope: "/elsewhere/s1") == ["/nowhere/root"])
    // R-2: an owner-configured root with a glob character is refused, and a root reached through a link is listed as what it resolves to too
    #expect(Grok.scopedDenies(hidden: ["/nowhere/root*"], scope: "/elsewhere/s1") == nil)
    #expect(Grok.scopedDenies(hidden: (0..<Grok.maxGlobEntries + 20).map { "/nowhere/root-\($0)" }, scope: "/elsewhere/s1")?.count == Grok.maxGlobEntries + 20, "literals are cheap")
}

/// A real grok, for the four layouts a session lives in (sync copy, worktree clone, app-folder mount, a folder outside the gateway's own): the list is built as the gateway builds
/// it, written as the profile, and grok runs a few commands under it. Own file readable and writable, every neighbour's (also one made after the list) unreadable. It uses the
/// owner's grok login (a few short prompts), so it runs only with MG_REAL_GROK=1.
@Test(.enabled(if: ProcessInfo.processInfo.environment["MG_REAL_GROK"] == "1")) func realGrokKeepsEveryLayoutApartFromItsNeighbours() throws {
    let exe = try #require(["/opt/homebrew/bin/grok", "/usr/local/bin/grok"].first { FileManager.default.isExecutableFile(atPath: $0) })
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("real-grok-" + UUID().uuidString, isDirectory: true).path
    let dev = String(repeating: "d", count: 43), dev2 = String(repeating: "e", count: 43), proj = "0123456789abcdef", clone = "minigw-0123456789abcdef-1", u = "0f8e7d6c-5b4a-4938-8271-6a5b4c3d2e1f"
    let scratch = base + "/vol/Mini Gateway Scratch"
    let own = ["sync": scratch + "/sync/" + dev + "/" + proj, "worktree": scratch + "/worktrees/" + dev + "/" + proj + "/" + clone, "mount": base + "/app/mounts/" + u, "local": base + "/mine/proj"]
    let other = [scratch + "/sync/" + dev2 + "/" + proj, scratch + "/worktrees/" + dev2 + "/" + proj + "/" + clone, scratch + "/worktrees/" + dev + "/" + proj + "/minigw-0123456789abcdef-2",
                 base + "/app/mounts/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", base + "/home/.codex", scratch + "/state", base + "/app/core"]
    for d in Array(own.values) + other { try fm.createDirectory(atPath: d, withIntermediateDirectories: true) }
    for d in own.values { try Data("OWN-OK-\(d.hashValue)".utf8).write(to: URL(fileURLWithPath: d + "/own.txt")) }
    for d in other { try Data("NEIGHBOUR-SECRET".utf8).write(to: URL(fileURLWithPath: d + "/secret.txt")) }
    let hidden = [scratch, scratch + "/state", scratch + "/copilot", base + "/app", base + "/app/core", base + "/home/.codex", base + "/home/.copilot",
                  base + "/caches/Mini Gateway Scratch", base + "/caches/Scratch"]
    for (layout, cwd) in own.sorted(by: { $0.key < $1.key }) {
        let deny = try #require(Grok.scopedDenies(hidden: hidden, scope: cwd), "\(layout)")
        // a neighbour made after the list was built
        let later = (cwd.hasPrefix(scratch + "/sync/") ? scratch + "/sync/later-client" : cwd.hasPrefix(scratch + "/worktrees/") ? scratch + "/worktrees/later-client" : base + "/app/mounts/later-client")
        try fm.createDirectory(atPath: later, withIntermediateDirectories: true)
        try Data("NEIGHBOUR-SECRET".utf8).write(to: URL(fileURLWithPath: later + "/secret.txt"))
        let home = URL(fileURLWithPath: base + "/grok-home-" + layout)
        let env = try #require(Grok.prepareCoreHome(home, deny: deny), "\(layout)")
        let targets = Array(other.map { $0 + "/secret.txt" }) + [later + "/secret.txt"]
        let prompt = "Run each of these with the shell, separately, and paste the raw output or error of each, nothing else:\n" + ([cwd + "/own.txt"] + targets).map { "cat '\($0)'" }.joined(separator: "\n") +
                     "\nThen: echo wrote > '\(cwd)/new.txt' && cat '\(cwd)/new.txt'"
        try Data(prompt.utf8).write(to: URL(fileURLWithPath: base + "/prompt-" + layout))
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = Grok.args(cwd: URL(fileURLWithPath: cwd), extraDirs: [], agent: true, model: nil, write: true, core: true).map { $0 == "/dev/stdin" ? base + "/prompt-" + layout : $0 }
        p.environment = ProcessInfo.processInfo.environment.merging(env) { $1 }
        let pipe = Pipe(); p.standardOutput = pipe; p.standardError = pipe; p.standardInput = FileHandle.nullDevice
        try p.run()
        let out = String(decoding: pipe.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self); p.waitUntilExit()
        #expect(p.terminationStatus == 0, "\(layout): grok did not start: \(out.prefix(300))")
        #expect(out.contains("OWN-OK-"), "\(layout): its own file was not read: \(out.suffix(600))")
        #expect(!out.contains("NEIGHBOUR-SECRET"), "\(layout): a neighbour's file was read")
        #expect(fm.fileExists(atPath: cwd + "/new.txt"), "\(layout): it could not write its own folder")
    }
}

@Test func grokDenyListNamesARootBehindALinkByWhatItResolvesTo() throws {
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("grok-link-" + UUID().uuidString, isDirectory: true).path
    defer { try? fm.removeItem(atPath: base) }
    try fm.createDirectory(atPath: base + "/real/secret", withIntermediateDirectories: true)
    try fm.createSymbolicLink(atPath: base + "/link", withDestinationPath: base + "/real")
    let deny = try #require(Grok.scopedDenies(hidden: [base + "/link/secret"], scope: base + "/elsewhere/s1"))
    #expect(deny.contains(base + "/link/secret") && deny.contains(base + "/real/secret"), "\(deny)")
}

/// R-1: the class prefix is not shortened below 6 characters; a session that needs less is refused (never a weaker list), and the prefix chosen is told.
@Test func grokDenyListDoesNotShortenTheClassPrefixBelowSix() {
    #expect(Grok.minPrefix == 6)
    let deep = "/nowhere/root" + (0..<9).map { "/level\($0)xxxx" }.joined()  // 9 levels: fits at 3 characters (about 30 globs), not at 6
    #expect(Grok.scopedDeniesWithPrefix(hidden: ["/nowhere/root"], scope: deep) == nil)
    #expect(Grok.scopedDeniesWithPrefix(hidden: ["/nowhere/root"], scope: "/nowhere/root/sync/device-id-0123456789/project-0123456789")?.prefix == 12)
}

/// Astra 5b90d47 finding 3: a hidden root that is a link to a path with glob characters: the resolved name is validated too (a glob would not match the literal path), so the run is refused.
@Test func grokDenyListRefusesARootWhoseResolvedPathHasGlobCharacters() throws {
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("grok-meta-" + UUID().uuidString, isDirectory: true).path
    defer { try? fm.removeItem(atPath: base) }
    try fm.createDirectory(atPath: base + "/secret[1]", withIntermediateDirectories: true)
    try fm.createSymbolicLink(atPath: base + "/link", withDestinationPath: base + "/secret[1]")
    #expect(Grok.scopedDenies(hidden: [base + "/link"], scope: base + "/elsewhere/s1") == nil)
    #expect(Grok.scopedDenies(hidden: [base + "/secret[1]"], scope: base + "/elsewhere/s1") == nil)
}
