import Foundation
import Testing
@testable import RouterCore

// 0.6.0 task 7a (#106): no agent may read the app's data folder (the core database, its sidecars, backups, usage spool).
// Unit tests of the generated arguments and profiles for every agent kind, and real processes under the real sandbox
// mechanisms of this Mac (Codex's permission profile through `codex sandbox`, the Seatbelt profile of `minigw run` and
// Copilot through sandbox-exec). Claude CLI's own sandbox and Grok are not run for real here: see the report.

private struct Fixture {
    let base: URL, app: URL, core: URL, work: URL, link: URL, tmp: URL
    /// <base>/Mini Gateway/core/core.db (and its sidecars), <base>/work/f.txt, <base>/work/lnk -> core.
    init() throws {
        let fm = FileManager.default
        base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("minigw-deny-" + UUID().uuidString, isDirectory: true)
        app = base.appendingPathComponent("Mini Gateway", isDirectory: true)
        core = app.appendingPathComponent("core", isDirectory: true)
        work = base.appendingPathComponent("work", isDirectory: true)
        tmp = base.appendingPathComponent("tmp", isDirectory: true)
        link = work.appendingPathComponent("lnk")
        for d in [core, work, tmp] { try fm.createDirectory(at: d, withIntermediateDirectories: true) }
        for f in ["core.db", "core.db-wal", "core.db-shm", "core.state", "core.audit.log", "usage.spool"] {
            try Data("SECRET-NOTES".utf8).write(to: core.appendingPathComponent(f))
        }
        try Data("ALLOWED".utf8).write(to: work.appendingPathComponent("f.txt"))
        try fm.createSymbolicLink(at: link, withDestinationURL: core)
    }
    var denied: [String] { RouterConfig.coreDenied(app: app, store: core) }
    func cleanup() { try? FileManager.default.removeItem(at: base) }
}

private func run(_ exe: String, _ args: [String], timeout: Double = 60) -> (status: Int32, out: String) {
    let p = Process(), pipe = Pipe()
    p.executableURL = URL(fileURLWithPath: exe); p.arguments = args; p.standardOutput = pipe; p.standardError = pipe
    guard (try? p.run()) != nil else { return (-1, "cannot run \(exe)") }
    var data = Data()
    let reader = Thread { data = pipe.fileHandleForReading.readDataToEndOfFile() }
    reader.start()
    p.waitUntilExit()
    while reader.isExecuting { usleep(5_000) }
    return (p.terminationStatus, String(decoding: data, as: UTF8.self))
}

// MARK: the list of denied paths

@Test func coreDeniedHasRealPathAndLink() throws {
    let f = try Fixture(); defer { f.cleanup() }
    // by its real path: the folder, the app folder around it, and the real target when the store is reached through a link
    #expect(f.denied.contains(f.core.path) && f.denied.contains(f.app.path))
    let viaLink = RouterConfig.coreDenied(app: f.app, store: f.link)
    #expect(viaLink.contains(f.core.path))  // the link resolves to the real folder
    // the live paths (the default app folder) are in every hidden list of the sandboxes
    let hidden = RouterConfig.hiddenScratch(nil)
    for d in RouterConfig.coreDenied() { #expect(hidden.contains(d), "\(d)") }
    #expect(RouterConfig.coreDenied().contains { $0.hasSuffix("/Mini Gateway/core") } && RouterConfig.coreDenied().contains { $0.hasSuffix("/Mini Gateway") })
}

@Test func allowedFolderContainingTheCoreIsRefused() throws {
    let appSupport = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
    for p in [appSupport.path, appSupport.appendingPathComponent("Mini Gateway").path, CoreHelper.defaultStore.path, NSHomeDirectory() + "/Library", "/"] {
        #expect(RouterConfig.overlapsCoreFolder(p), "\(p)")
    }
    let f = try Fixture(); defer { f.cleanup() }
    #expect(!RouterConfig.overlapsCoreFolder(f.work.path))
    // an agent's cwd inside the data folder does not resolve
    #expect(ClaudeCLI.resolveCwd(CoreHelper.defaultStore.path, roots: [NSHomeDirectory()]) == nil)
}

// MARK: Codex: the permission profile in every mode

@Test func codexDeniesCoreInEveryMode() throws {
    let f = try Fixture(); defer { f.cleanup() }
    let out = f.base.appendingPathComponent("o.txt")
    let gits = [f.work.path + "/.git"]
    let modes: [(String, [String])] = [
        ("agent write", Codex.args(cwd: f.work, extraDirs: [f.tmp.path], agent: true, output: out, write: true, network: true, deny: f.denied, tmp: f.tmp.path)),
        ("agent read-only", Codex.args(cwd: f.work, extraDirs: [], agent: true, output: out, write: false, deny: f.denied)),
        ("chat", Codex.args(cwd: f.work, extraDirs: [], agent: false, output: out, deny: f.denied)),
        ("resume", Codex.args(cwd: f.work, extraDirs: [], agent: true, output: out, resume: "t1", deny: f.denied)),
        ("tree", Codex.args(cwd: f.work, extraDirs: [], agent: true, output: out, write: true, gits: gits, path: "/usr/bin", deny: f.denied)),
        ("tree read-only", Codex.args(cwd: f.work, extraDirs: [], agent: true, output: out, write: false, gits: gits, path: "/usr/bin", deny: f.denied)),
    ]
    for (name, a) in modes {
        let all = a.joined(separator: " ")
        for d in f.denied { #expect(all.contains("\"\(d)\"=\"none\""), "\(name): \(d)") }
        #expect(all.contains(#"default_permissions="minigw""#), "\(name)")
        #expect(!all.contains("workspace-write") && !all.contains("--sandbox") && !all.contains("danger") && !all.contains("bypass"), "\(name)")
    }
    // the default deny list (nothing passed) is the real app folder: a caller cannot forget it
    let dflt = Codex.args(cwd: f.work, extraDirs: [], agent: true, output: out).joined(separator: " ")
    for d in RouterConfig.coreDenied() { #expect(dflt.contains("\"\(d)\"=\"none\"")) }
    // a writable folder never lifts the deny of the folder inside it
    let inside = Codex.args(cwd: f.base, extraDirs: [], agent: true, output: out, deny: f.denied).joined(separator: " ")
    #expect(inside.contains("\"\(f.base.path)\"=\"write\"") && inside.contains("\"\(f.core.path)\"=\"none\""))
}

/// The `-c` pairs of Codex.args that make the profile, for `codex sandbox` (same engine as `codex exec`'s commands).
private func profileConfig(_ a: [String]) -> [String] {
    var o: [String] = []
    for (i, x) in a.enumerated() where x == "-c" && i + 1 < a.count && (a[i + 1].hasPrefix("default_permissions") || a[i + 1].hasPrefix("permissions.")) { o += ["-c", a[i + 1]] }
    return o
}

@Test func codexProfileDeniesCoreForRealProcesses() throws {
    let exe = try #require(["/opt/homebrew/bin/codex", "/usr/local/bin/codex"].first { FileManager.default.isExecutableFile(atPath: $0) }, "codex is installed on the Mac that runs this gate")
    let f = try Fixture(); defer { f.cleanup() }
    let out = f.base.appendingPathComponent("o.txt")
    let modes: [(String, [String])] = [
        ("write", Codex.args(cwd: f.work, extraDirs: [], agent: true, output: out, write: true, deny: f.denied, tmp: f.tmp.path)),
        ("read-only", Codex.args(cwd: f.work, extraDirs: [], agent: true, output: out, write: false, deny: f.denied)),
        ("chat", Codex.args(cwd: f.work, extraDirs: [], agent: false, output: out, deny: f.denied)),
    ]
    for (name, a) in modes {
        let cfg = profileConfig(a)
        func under(_ cmd: [String]) -> (status: Int32, out: String) { run(exe, ["sandbox"] + cfg + ["--"] + cmd) }
        // the allowed file reads, the core does not: by its real path, through the link in the allowed folder, its sidecars
        let ok = under(["/bin/cat", f.work.appendingPathComponent("f.txt").path])
        #expect(ok.status == 0 && ok.out.contains("ALLOWED"), "\(name): \(ok)")
        for file in ["core.db", "core.db-wal", "core.db-shm", "core.state", "core.audit.log", "usage.spool"] {
            for path in [f.core.appendingPathComponent(file).path, f.link.path + "/" + file] {
                let r = under(["/bin/cat", path])
                #expect(r.status != 0 && !r.out.contains("SECRET-NOTES"), "\(name) \(path): \(r)")
            }
        }
        #expect(under(["/bin/ls", f.core.path]).status != 0, "\(name)")
        #expect(under(["/bin/ls", f.app.path]).status != 0, "\(name)")  // state.json too
        // no write, no new file, no rename out of the folder
        #expect(under(["/bin/sh", "-c", "echo x > '\(f.core.path)/new'"]).status != 0 && !FileManager.default.fileExists(atPath: f.core.path + "/new"), "\(name)")
        #expect(under(["/bin/mv", f.core.appendingPathComponent("core.db").path, f.work.path + "/stolen"]).status != 0 && !FileManager.default.fileExists(atPath: f.work.path + "/stolen"), "\(name)")
        // writing in the session folder: only with writes (and .git stays read-only)
        let w = under(["/bin/sh", "-c", "echo x > '\(f.work.path)/w-\(name)'"])
        #expect((w.status == 0) == (name == "write"), "\(name): \(w)")
    }
    // the folder that holds the core is writable and the deny still stands (an allowed root above it)
    let inside = profileConfig(Codex.args(cwd: f.base, extraDirs: [], agent: true, output: out, deny: f.denied))
    #expect(run(exe, ["sandbox"] + inside + ["--", "/bin/cat", f.core.path + "/core.db"]).status != 0)
    #expect(run(exe, ["sandbox"] + inside + ["--", "/bin/sh", "-c", "echo x > '\(f.base.path)/ok'"]).status == 0)
}

// MARK: Seatbelt (minigw run, Copilot) and Claude CLI

@Test func seatbeltProfileDeniesCoreForRealProcesses() throws {
    let f = try Fixture(); defer { f.cleanup() }
    // hidden = what the callers pass (RouterConfig.hiddenScratch): the core folders by real path
    let profile = try #require(Native.sandboxProfile(dir: f.work.path, home: f.base.path, network: true, hidden: f.denied, writable: []))
    func under(_ cmd: [String]) -> (status: Int32, out: String) { run("/usr/bin/sandbox-exec", ["-p", profile] + cmd) }
    let ok = under(["/bin/cat", f.work.appendingPathComponent("f.txt").path])
    #expect(ok.status == 0 && ok.out.contains("ALLOWED"), "\(ok)")
    for file in ["core.db", "core.db-wal", "core.state", "usage.spool"] {
        for path in [f.core.appendingPathComponent(file).path, f.link.path + "/" + file] {
            let r = under(["/bin/cat", path])
            #expect(r.status != 0 && !r.out.contains("SECRET-NOTES"), "\(path): \(r)")
        }
    }
    #expect(under(["/bin/sh", "-c", "echo x > '\(f.core.path)/new'"]).status != 0 && !FileManager.default.fileExists(atPath: f.core.path + "/new"))
    // the profile itself, built with the real home, also covers Application Support (the folder the core lives in)
    let own = try #require(Native.sandboxProfile(dir: f.work.path, network: true, hidden: RouterConfig.hiddenScratch(nil), writable: []))
    for d in RouterConfig.coreDenied() { #expect(own.contains("\"\(d)\""), "\(d)") }
}

@Test func claudeSandboxSettingsDenyCore() throws {
    let f = try Fixture(); defer { f.cleanup() }
    let scratch = f.base.appendingPathComponent("Scratch", isDirectory: true)
    try FileManager.default.createDirectory(at: scratch, withIntermediateDirectories: true)
    let live = RouterConfig.coreDenied()
    var cfg = ClaudeCLIConfig(); cfg.allowLocalTools = true
    let plain = cfg.agentArgs(cwd: f.work, allTools: [], scratch: scratch, folders: AllowedFolders())
    let tree = cfg.worktreeArgs(root: f.work, maven: f.tmp, cwd: f.work, allTools: [], network: false, scratch: scratch)
    for (name, a) in [("plain", plain), ("worktree", tree)] {
        let json = try #require(a.firstIndex(of: "--settings").map { a[$0 + 1] }, "\(name)")
        let sb = try #require(((try? JSONSerialization.jsonObject(with: Data(json.utf8))) as? [String: Any])?["sandbox"] as? [String: Any])
        let fs = try #require(sb["filesystem"] as? [String: Any])
        let deny = try #require(fs["denyRead"] as? [String])
        #expect(sb["failIfUnavailable"] as? Bool == true && sb["allowUnsandboxedCommands"] as? Bool == false, "\(name)")
        for d in live { #expect(deny.contains(d), "\(name) \(d)") }
    }
}

// MARK: Grok does not run while the core is on

private actor NoMarks: CoreRevocationMarks {
    func list() -> [String] { [] }
    func add(_ device: String) {}
    func clear(_ devices: [String]) {}
}

@Test func grokRefusedWhenTheCoreIsOn() async throws {
    let f = try Fixture(); defer { f.cleanup() }
    var cfg = RouterConfig()
    cfg.grok.enabled = true
    let helper = CoreHelper(binary: nil, store: f.core, marks: NoMarks())
    let body = #"{"model":"claude-grok","messages":[{"role":"user","content":"hi"}]}"#
    let withCore = Router(config: cfg, token: "tok", secrets: FakeSecrets(values: [:]), core: helper)
    let w = FakeWriter()
    let st = await withCore.runGrok(request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok"], body: body), body: ["model": "claude-grok"], model: nil, rid: "r1", writer: w)
    #expect(st == 403 && w.status == 403 && w.text.contains(Grok.coreRefusal))
    let none = Router(config: cfg, token: "tok", secrets: FakeSecrets(values: [:]))
    let w2 = FakeWriter()
    _ = await none.runGrok(request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok"], body: body), body: ["model": "claude-grok"], model: nil, rid: "r2", writer: w2)
    #expect(!w2.text.contains(Grok.coreRefusal))  // without the core Grok works as before (here: stops at the missing last user message or program)
}
