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
    let exe = try requireCodex()
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
