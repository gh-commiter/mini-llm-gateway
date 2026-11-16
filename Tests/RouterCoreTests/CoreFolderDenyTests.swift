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

/// A3 (S3): the backup folder is <core>/backups, inside every deny of the core folder (Seatbelt, Codex profile, Claude rules).
@Test func backupFolderIsInsideTheCoreDeny() throws {
    let backups = CoreHelper.defaultStore.appendingPathComponent("backups/core-1.db").path
    for list in [RouterConfig.coreDenied(), RouterConfig.agentDenied(kind: "codex", scratch: nil), RouterConfig.hiddenScratch(nil)] {
        #expect(list.contains { backups == $0 || backups.hasPrefix($0 + "/") }, "\(backups) not covered by \(list)")
    }
    let f = try Fixture(); defer { f.cleanup() }
    let b = f.core.appendingPathComponent("backups"); try FileManager.default.createDirectory(at: b, withIntermediateDirectories: true)
    try Data("SECRET-NOTES".utf8).write(to: b.appendingPathComponent("core-1.db"))
    let profile = try #require(Native.sandboxProfile(dir: f.work.path, home: f.base.path, network: true, hidden: f.denied, writable: []))
    let r = run("/usr/bin/sandbox-exec", ["-p", profile, "/bin/cat", b.path + "/core-1.db"])
    #expect(r.status != 0 && !r.out.contains("SECRET-NOTES"), "\(r)")
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
    // A hard link from an allowed folder is the same inode under another name: the Seatbelt profile (path based) does not
    // see it. THE SANDBOX ALONE DOES NOT STOP IT; the core refuses to open (Go) and every run is refused (Swift check below).
    let alias = f.work.appendingPathComponent("alias.db")
    try FileManager.default.linkItem(at: f.core.appendingPathComponent("core.db"), to: alias)
    let viaAlias = under(["/bin/cat", alias.path])
    #expect(viaAlias.status == 0 && viaAlias.out.contains("SECRET-NOTES"), "the sandbox alone stops a hard link (more than expected): \(viaAlias)")
    let problem = try #require(RouterConfig.coreLinkProblem(store: f.core))
    #expect(problem.contains("core.db") && problem.contains("remove the other link"))
    try FileManager.default.removeItem(at: alias)
    #expect(RouterConfig.coreLinkProblem(store: f.core) == nil)
    // a name inside the folder (a snapshot of a restore) is no alias
    try FileManager.default.linkItem(at: f.core.appendingPathComponent("core.db"), to: f.core.appendingPathComponent("snapshot.db"))
    #expect(RouterConfig.coreLinkProblem(store: f.core) == nil)
    // the profile itself, built with the real home, also covers Application Support (the folder the core lives in)
    let own = try #require(Native.sandboxProfile(dir: f.work.path, network: true, hidden: RouterConfig.hiddenScratch(nil), writable: []))
    for d in RouterConfig.coreDenied() { #expect(own.contains("\"\(d)\""), "\(d)") }
}

@Test func claudeDeniesCoreInEveryMode() throws {
    let f = try Fixture(); defer { f.cleanup() }
    let scratch = f.base.appendingPathComponent("Scratch", isDirectory: true)
    try FileManager.default.createDirectory(at: scratch, withIntermediateDirectories: true)
    let live = RouterConfig.agentDenied(kind: "claude", scratch: scratch)
    #expect(RouterConfig.coreDenied().allSatisfy(live.contains))
    func settings(_ a: [String], _ name: String) throws -> [String: Any] {
        let json = try #require(a.firstIndex(of: "--settings").map { a[$0 + 1] }, "\(name)")
        return try #require((try? JSONSerialization.jsonObject(with: Data(json.utf8))) as? [String: Any], "\(name)")
    }
    // the default mode (no "Allow local tools") has no sandbox: the tool rules are the only deny (#106 S3)
    var restricted = ClaudeCLIConfig(); restricted.allowLocalTools = false
    var local = ClaudeCLIConfig(); local.allowLocalTools = true
    var modes: [(String, [String])] = [
        ("restricted", restricted.agentArgs(cwd: f.work, allTools: [], scratch: scratch, folders: AllowedFolders())),
        ("restricted read-only", restricted.agentArgs(cwd: f.work, allTools: [], scratch: scratch, readOnly: true, folders: AllowedFolders())),
        ("local", local.agentArgs(cwd: f.work, allTools: [], scratch: scratch, folders: AllowedFolders())),
        ("local read-only", local.agentArgs(cwd: f.work, allTools: [], scratch: scratch, readOnly: true, folders: AllowedFolders())),
        ("worktree", local.worktreeArgs(root: f.work, maven: f.tmp, cwd: f.work, allTools: [], network: false, scratch: scratch)),
    ]
    modes.append(("restricted + other tools", restricted.agentArgs(cwd: f.work, allTools: ["Read", "Bash"], scratch: scratch, folders: AllowedFolders())))
    for (name, a) in modes {
        // the deny rules are in the settings (sandboxed modes) or, since 0.5.29, in --disallowedTools (the restricted mode has no settings)
        var deny = a.firstIndex(of: "--settings") == nil ? [] : (((try settings(a, name))["permissions"] as? [String: Any])?["deny"] as? [String] ?? [])
        deny += a.firstIndex(of: "--disallowedTools").map { a[$0 + 1].components(separatedBy: ",") } ?? []
        for d in live { for t in ["Read", "Edit"] { #expect(deny.contains("\(t)(/\(d)/**)"), "\(name) \(t) \(d)") } }
    }
    for (name, a) in modes where name.hasPrefix("local") || name == "worktree" {
        let sb = try #require((try settings(a, name))["sandbox"] as? [String: Any])
        let denyRead = try #require((sb["filesystem"] as? [String: Any])?["denyRead"] as? [String])
        #expect(sb["failIfUnavailable"] as? Bool == true && sb["allowUnsandboxedCommands"] as? Bool == false, "\(name)")
        for d in RouterConfig.coreDenied() + RouterConfig.credentialHomes() { #expect(denyRead.contains(d), "\(name) \(d)") }
    }
}

// MARK: credential homes of the other providers (S3 A5)

@Test func agentsKeepTheirOwnHomeAndNotTheOthers() throws {
    let f = try Fixture(); defer { f.cleanup() }
    let home = f.base.path
    let scratch = f.base.appendingPathComponent("Scratch", isDirectory: true)
    for kind in ["codex", "grok", "copilot", "claude"] {
        let d = RouterConfig.agentDenied(kind: kind, scratch: scratch, home: home, env: [:])
        for (owner, dir) in [("grok", ".grok"), ("codex", ".codex"), ("copilot", ".copilot")] {
            #expect(d.contains(home + "/" + dir) == (owner != kind), "\(kind) \(dir)")
        }
        #expect(d.contains(scratch.path + "/grok-home") == (kind != "grok") && d.contains(scratch.path + "/copilot") == (kind != "copilot"), "\(kind)")
        #expect(f.denied.allSatisfy { RouterConfig.coreDenied(app: f.app, store: f.core).contains($0) })
    }
    // the gateway user's own *_HOME variables are the homes
    let env = RouterConfig.credentialHomes(except: "", home: home, env: ["GROK_HOME": f.tmp.path, "CODEX_HOME": f.work.path])
    #expect(env.contains(f.tmp.path) && env.contains(f.work.path) && !env.contains(home + "/.grok") && env.contains(home + "/.copilot"))
}

@Test func credentialHomesAreDeniedForRealProcesses() throws {
    let f = try Fixture(); defer { f.cleanup() }
    let fm = FileManager.default, home = f.base.path
    for d in [".grok", ".codex", ".copilot"] {
        try fm.createDirectory(atPath: home + "/" + d, withIntermediateDirectories: true)
        try Data("CREDENTIAL".utf8).write(to: URL(fileURLWithPath: home + "/" + d + "/auth.json"))
    }
    let scratch = f.base.appendingPathComponent("Scratch", isDirectory: true)
    try fm.createDirectory(at: scratch.appendingPathComponent("grok-home"), withIntermediateDirectories: true)
    try Data("PROFILE".utf8).write(to: scratch.appendingPathComponent("grok-home/sandbox.toml"))
    // Seatbelt (minigw run, Copilot): the hidden list holds all three homes
    let profile = try #require(Native.sandboxProfile(dir: f.work.path, home: f.base.path, network: true,
                                                     hidden: f.denied + RouterConfig.credentialHomes(except: "", home: home, env: [:]), writable: []))
    for d in [".grok", ".codex", ".copilot"] {
        let r = run("/usr/bin/sandbox-exec", ["-p", profile, "/bin/cat", home + "/" + d + "/auth.json"])
        #expect(r.status != 0 && !r.out.contains("CREDENTIAL"), "\(d): \(r)")
    }
    #expect(run("/usr/bin/sandbox-exec", ["-p", profile, "/bin/cat", f.work.path + "/f.txt"]).out.contains("ALLOWED"))
    // Codex: its own home stays readable, the others' are not
    let exe = try #require(["/opt/homebrew/bin/codex", "/usr/local/bin/codex"].first { fm.isExecutableFile(atPath: $0) }, "codex is installed on the Mac that runs this gate")
    let deny = RouterConfig.agentDenied(kind: "codex", scratch: scratch, home: home, env: [:]) + f.denied
    let cfg = profileConfig(Codex.args(cwd: f.work, extraDirs: [], agent: true, output: f.base.appendingPathComponent("o"), deny: deny))
    func under(_ p: String) -> (status: Int32, out: String) { run(exe, ["sandbox"] + cfg + ["--", "/bin/cat", p]) }
    #expect(under(home + "/.codex/auth.json").out.contains("CREDENTIAL"))  // own home
    for p in [home + "/.grok/auth.json", home + "/.copilot/auth.json", scratch.path + "/grok-home/sandbox.toml"] {
        let r = under(p)
        #expect(r.status != 0 && !r.out.contains("CREDENTIAL") && !r.out.contains("PROFILE"), "\(p): \(r)")
    }
}

// MARK: revocation marks stub

actor NoMarks: CoreRevocationMarks {
    func list() -> [String] { [] }
    func add(_ device: String) {}
    func clear(_ devices: [String]) {}
}
