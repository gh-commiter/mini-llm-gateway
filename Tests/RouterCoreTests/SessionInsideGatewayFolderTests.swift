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
    #expect(deny.contains(l.app.path + "/core") && deny.contains("/Users/x/.codex") && !deny.contains(l.app.path))
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
private func grokDenies(_ deny: [String], _ path: String) -> Bool {
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
        #expect(deny.count <= 40, "\(deny.count) entries")
        #expect(deny == Grok.scopedDenies(hidden: hidden, scope: scope), "deterministic")
        for n in siblings + later { #expect(grokDenies(deny, base + "/" + n), "\(n)") }
        #expect(!grokDenies(deny, scope) && !grokDenies(deny, scope + "/f.txt") && !grokDenies(deny, scope + "/deep/er/file"))
    }
}
