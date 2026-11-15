import Foundation
import Testing
@testable import RouterCore

// 0.6.0: the kernel layer under the restricted Claude CLI (Native.claudeRestrictedProfile, ClaudeSeatbelt). Real processes under the real profile with plain tools;
// a temporary home, never the real one. The Claude Code rules are not involved: these rows hold even when they fail open.

/// A layout like the gateway's: an app folder with mounts of two sessions, a Scratch folder with a synced session, a user project, an allowed folder,
/// a home with stand-ins for ~/.ssh and ~/.claude, a private temp folder of the run.
private struct Rig {
    let root: URL
    let home, app, mountA, mountB, scratch, sync, project, allowed, outside, tmp: String
    var hidden: [String] { [app, scratch] }

    init() throws {
        let fm = FileManager.default
        root = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("seatbelt-" + UUID().uuidString.lowercased(), isDirectory: true)
        let r = root.path
        home = r + "/home"; app = home + "/Library/Application Support/Mini Gateway"
        mountA = app + "/mounts/aaaa"; mountB = app + "/mounts/bbbb"
        scratch = r + "/Scratch"; sync = scratch + "/sync/dev/sess"
        project = home + "/Development/proj"; allowed = home + "/Development/other"; outside = r + "/outside"; tmp = scratch + "/tmp/run-req_x"
        for n in Rig.closedClaudeNames + ["dev-mods/m.json", "cache/model-catalog/c.json"] {
            let f = home + "/.claude/" + n
            try fm.createDirectory(atPath: (f as NSString).deletingLastPathComponent, withIntermediateDirectories: true)
            try Data("secret".utf8).write(to: URL(fileURLWithPath: f))
        }
        for d in [project + "/.git/hooks", project + "/sub/.git/modules/m", project + "/sub/.claude", project + "/.claude", mountA, mountB, sync, project, allowed, outside, tmp, home + "/.ssh", home + "/.claude/projects/-other", scratch + "/state", app + "/core",
                  home + "/Library/Keychains/ids"] { try fm.createDirectory(atPath: d, withIntermediateDirectories: true) }
        for f in [mountA + "/a.txt", mountB + "/b.txt", sync + "/s.txt", project + "/p.txt", allowed + "/o.txt", outside + "/x.txt", home + "/.ssh/config",
                  home + "/.claude/history.jsonl", home + "/.claude/projects/-other/t.jsonl", home + "/.claude/settings.json", home + "/.claude/CLAUDE.md",
                  app + "/state.json", app + "/core/db", scratch + "/state/s", scratch + "/sync/dev/other.txt", home + "/Library/Keychains/login.keychain-db",
                  home + "/Library/Keychains/ids/x"] {
            try Data("secret".utf8).write(to: URL(fileURLWithPath: f))
        }
    }

    /// Names under ~/.claude that a restricted run must not read (shell snapshots hold the exported environment, .credentials.json a token, the rest is state of the owner's sessions).
    static let closedClaudeNames = ["shell-snapshots/s.sh", ".credentials.json", "debug/d.txt", "todos/t.json", "plans/p.md", "tasks/t", "ide/i.lock", "teams/t", "statsig/s",
                                    "plugins/installed_plugins.json", "telemetry/t.json", "CLAUDE.md", "settings.json", "settings.local.json", "history.jsonl", "file-history/f",
                                    "paste-cache/p", "backups/b", "sessions/s.json", "session-env/e", "cache/other/c", "projects/-other/t.jsonl", "cache/changelog.md"]

    func profile(session: String, folders: [String] = [], writable: Bool = true, folderSettings: Bool = false, userSettings: Bool = false) throws -> String {
        try #require(Native.claudeRestrictedProfile(session: session, folders: folders, writable: writable, tmp: tmp, exe: "/opt/homebrew/bin/claude", home: home, hidden: hidden,
                                                    folderSettings: folderSettings, userSettings: userSettings))
    }

    /// true when the tool ran with exit 0 under the profile.
    func ok(_ profile: String, _ tool: String, _ args: [String]) throws -> Bool {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/sandbox-exec")
        p.arguments = ["-p", profile, tool] + args
        p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice; p.standardInput = FileHandle.nullDevice
        try p.run(); p.waitUntilExit()
        return p.terminationStatus == 0
    }
    func canRead(_ profile: String, _ path: String) throws -> Bool { try ok(profile, "/bin/cat", [path]) }
    func canWrite(_ profile: String, _ path: String) throws -> Bool {
        let ok = try ok(profile, "/usr/bin/touch", [path])
        return ok && FileManager.default.fileExists(atPath: path)
    }
}

@Test func profileIsDefaultDenyWithAnExplicitList() throws {
    let rig = try Rig()
    defer { try? FileManager.default.removeItem(at: rig.root) }
    let p = try rig.profile(session: rig.project, folders: [rig.allowed])
    let lines = p.split(separator: "\n").map(String.init)
    #expect(Array(lines.prefix(3)) == ["(version 1)", "(allow default)", "(deny file-read* file-write*)"])  // everything denied first
    #expect(!p.contains("(subpath \"/Users\")") && !p.contains("(subpath \"/Library\")") && !p.contains("(subpath \"/private/var/folders\")") && !p.contains("(subpath \"/opt\")"))
    #expect(p.contains("login\\.keychain-db") && p.contains("(literal \"/usr/bin/security\")"))  // the login: the login keychain files, not the folder
    #expect(!p.contains("(subpath \"\(rig.home)/Library/Keychains\")"))
    #expect(p.contains("(deny network-outbound)") && p.contains("(remote tcp \"*:443\")") && p.contains("(deny network-outbound (remote ip \"localhost:*\"))"))
    #expect(p.contains("\(rig.home)/.claude/projects/" + Native.claudeProjectFolder(rig.project)))
}

@Test func readOnlyRunHasNoWriteGrantOnItsFolders() throws {
    let rig = try Rig()
    defer { try? FileManager.default.removeItem(at: rig.root) }
    let rw = try rig.profile(session: rig.project, folders: [rig.allowed], writable: true)
    let ro = try rig.profile(session: rig.project, folders: [rig.allowed], writable: false)
    #expect(rw.contains("(allow file-write* (subpath \"\(rig.project)\")") && !ro.contains("(allow file-write* (subpath \"\(rig.project)\")"))
    #expect(try rig.canWrite(rw, rig.project + "/new1") && rig.canWrite(rw, rig.allowed + "/new1"))
    #expect(try !rig.canWrite(ro, rig.project + "/new2") && !rig.canWrite(ro, rig.allowed + "/new2"))  // write in a read-only run denied
    #expect(try rig.canRead(ro, rig.project + "/p.txt") && rig.canRead(ro, rig.allowed + "/o.txt"))
}

@Test func localSessionRows() throws {
    let rig = try Rig()
    defer { try? FileManager.default.removeItem(at: rig.root) }
    let p = try rig.profile(session: rig.project, folders: [rig.allowed])
    #expect(try rig.canRead(p, rig.project + "/p.txt"))  // inside the session folder
    #expect(try rig.canRead(p, rig.allowed + "/o.txt"))  // an allowed folder
    #expect(try rig.canWrite(p, rig.project + "/w.txt") && rig.canWrite(p, rig.allowed + "/w.txt") && rig.canWrite(p, rig.tmp + "/w.txt"))
    #expect(try !rig.canRead(p, rig.outside + "/x.txt"))  // outside
    #expect(try !rig.canWrite(p, rig.outside + "/w.txt"))  // write outside
    #expect(try !rig.canRead(p, rig.home + "/.ssh/config"))  // ~/.ssh stand-in
    #expect(try !rig.canRead(p, rig.home + "/.claude/history.jsonl"))  // history
    #expect(try !rig.canRead(p, rig.home + "/.claude/projects/-other/t.jsonl"))  // another project's transcript
    #expect(try rig.canRead(p, rig.home + "/.claude/dev-mods/m.json"))  // what claude needs (Write is refused by Claude Code without it)
    #expect(try rig.canRead(p, rig.home + "/.claude/cache/model-catalog/c.json") && rig.canWrite(p, rig.home + "/.claude/cache/model-catalog/c2.json"))
    #expect(try rig.canRead(p, rig.home + "/Library/Keychains/login.keychain-db"))  // the login (Claude Code rules still forbid it to the Read tool)
    #expect(try rig.canWrite(p, rig.home + "/Library/Keychains/login.keychain-db-wal"))  // a token refresh writes it (else the rotated refresh token is lost)
    #expect(try !rig.canRead(p, rig.home + "/Library/Keychains/ids/x") && !rig.canWrite(p, rig.home + "/Library/Keychains/other.db") && !rig.canWrite(p, rig.home + "/.claude/projects/-other/n.jsonl"))
    #expect(try !rig.canRead(p, rig.app + "/state.json") && !rig.canRead(p, rig.scratch + "/state/s"))  // the gateway's folders
    #expect(try !rig.canRead(p, "/private/etc/passwd"))  // not world-readable system files
    // claude's own state: this session's transcript folder is written, session-env too
    let slug = rig.home + "/.claude/projects/" + Native.claudeProjectFolder(rig.project)
    try FileManager.default.createDirectory(atPath: slug, withIntermediateDirectories: true)
    try FileManager.default.createDirectory(atPath: rig.home + "/.claude/session-env", withIntermediateDirectories: true)
    #expect(try rig.canWrite(p, slug + "/t.jsonl") && rig.canWrite(p, rig.home + "/.claude/session-env/x"))
}

/// F1: ~/.claude is an allow list; every other name is closed to reading (one real sandbox-exec row per name).
@Test func claudeHomeIsAnAllowList() throws {
    let rig = try Rig()
    defer { try? FileManager.default.removeItem(at: rig.root) }
    let p = try rig.profile(session: rig.project)
    for n in Rig.closedClaudeNames { #expect(try !rig.canRead(p, rig.home + "/.claude/" + n), "\(n) is readable") }
    #expect(try !rig.canRead(p, rig.home + "/.claude/projects/-other/t.jsonl"))
    // session-env and sessions are written, not read
    #expect(try rig.canWrite(p, rig.home + "/.claude/session-env/new") && rig.canWrite(p, rig.home + "/.claude/sessions/new"))
    #expect(try !rig.canWrite(p, rig.home + "/.claude/shell-snapshots/new") && !rig.canWrite(p, rig.home + "/.claude/.credentials.json") && !rig.canWrite(p, rig.home + "/.claude/settings.json"))
    // a granted folder around the whole home (the gateway refuses it; the kernel does not rely on that) does not open ~/.claude
    let wide = try rig.profile(session: rig.project, folders: [rig.home])
    for n in ["shell-snapshots/s.sh", ".credentials.json", "history.jsonl", "projects/-other/t.jsonl", "CLAUDE.md"] { #expect(try !rig.canRead(wide, rig.home + "/.claude/" + n), "wide: \(n)") }
    #expect(try rig.canRead(wide, rig.home + "/.claude/dev-mods/m.json"))
}

/// F5: with the setting source `user`, Claude Code reads ~/.claude/settings.json (hooks, permissions): readable then, never writable; otherwise closed.
@Test func userSettingsAreReadableOnlyWithTheUserSource() throws {
    let rig = try Rig()
    defer { try? FileManager.default.removeItem(at: rig.root) }
    let off = try rig.profile(session: rig.project), on = try rig.profile(session: rig.project, userSettings: true)
    #expect(try !rig.canRead(off, rig.home + "/.claude/settings.json") && rig.canRead(on, rig.home + "/.claude/settings.json"))
    #expect(try !rig.canWrite(on, rig.home + "/.claude/settings.json") && !rig.canRead(on, rig.home + "/.claude/settings.local.json"))
    #expect(ClaudeSeatbelt.grants(args: ["--setting-sources", "user"]).sources == "user" && ClaudeSeatbelt.grants(args: ["-p"]).sources == "")
}

/// F2: the file tools never touch `.git` (the rule layer's gitDataEditDenies) nor, with project/local sources, `.claude`: the kernel refuses it too, in every writable granted folder.
@Test func gitAndClaudeFoldersAreClosedForWriting() throws {
    let rig = try Rig()
    defer { try? FileManager.default.removeItem(at: rig.root) }
    let p = try rig.profile(session: rig.project, folders: [rig.allowed], writable: true, folderSettings: true)
    #expect(try rig.canWrite(p, rig.project + "/a.txt") && rig.canWrite(p, rig.project + "/sub/b.txt") && rig.canWrite(p, rig.allowed + "/a.txt"))
    #expect(try !rig.canWrite(p, rig.project + "/.git/config") && !rig.canWrite(p, rig.project + "/.git/hooks/x") && !rig.canWrite(p, rig.project + "/sub/.git/hooks/x"))
    // every name the rule layer lists, top-level and as a submodule's git folder
    for n in ClaudeCLIConfig.gitDenies { #expect(try !rig.canWrite(p, rig.project + "/.git/" + n), "\(n)") }
    for n in ClaudeCLIConfig.submoduleDenies { #expect(try !rig.canWrite(p, rig.project + "/.git/modules/m/" + n) && !rig.canWrite(p, rig.project + "/sub/.git/modules/m/" + n), "\(n)") }
    #expect(try !rig.canWrite(p, rig.project + "/.git/HEAD") && !rig.canWrite(p, rig.project + "/.git/refs-x"))  // the rest of `.git` too: the file tools never write it
    // `.git` replaced by a file (a gitdir redirect) is an entry of its own
    try Data("gitdir: /elsewhere".utf8).write(to: URL(fileURLWithPath: rig.allowed + "/.git"))
    #expect(try !rig.canWrite(p, rig.allowed + "/.git") && rig.canWrite(p, rig.allowed + "/gitignored.txt"))
    #expect(try !rig.canWrite(p, rig.project + "/.claude/settings.json") && !rig.canWrite(p, rig.project + "/sub/.claude/settings.local.json"))  // folder settings are read
    let plain = try rig.profile(session: rig.project, writable: true)
    #expect(try rig.canWrite(plain, rig.project + "/.claude/x") && !rig.canWrite(plain, rig.project + "/.git/config"))  // `.claude` only with those sources
    // a read-only run has no write at all (the rows above, plus `.git`)
    let ro = try rig.profile(session: rig.project, writable: false)
    #expect(try !rig.canWrite(ro, rig.project + "/a2.txt"))
}

@Test func sessionInsideTheAppFolderSeesOnlyItself() throws {  // #114: a remote client's mount lives inside the app folder
    let rig = try Rig()
    defer { try? FileManager.default.removeItem(at: rig.root) }
    let p = try rig.profile(session: rig.mountA)
    #expect(try rig.canRead(p, rig.mountA + "/a.txt") && rig.canWrite(p, rig.mountA + "/w.txt"))
    #expect(try !rig.canRead(p, rig.mountB + "/b.txt"))  // another session's mount
    #expect(try !rig.canRead(p, rig.app + "/state.json") && !rig.canRead(p, rig.app + "/core/db"))  // the app folder's files
    #expect(try !rig.canWrite(p, rig.app + "/w.txt") && !rig.canWrite(p, rig.mountB + "/w.txt"))
    #expect(try !rig.canRead(p, rig.scratch + "/state/s"))
}

@Test func sessionInsideScratchSeesOnlyItself() throws {
    let rig = try Rig()
    defer { try? FileManager.default.removeItem(at: rig.root) }
    let p = try rig.profile(session: rig.sync, writable: false)
    #expect(try rig.canRead(p, rig.sync + "/s.txt"))
    #expect(try !rig.canRead(p, rig.scratch + "/sync/dev/other.txt") && !rig.canRead(p, rig.scratch + "/state/s"))  // Scratch, a sibling session
    #expect(try !rig.canWrite(p, rig.sync + "/w.txt"))  // read-only
    #expect(try rig.canWrite(p, rig.tmp + "/w.txt"))  // the run's temp folder (also in Scratch) is writable
    #expect(try !rig.canRead(p, rig.app + "/state.json"))
}

@Test func hiddenAndSecretFoldersInsideAGrantedFolderStayDenied() throws {
    let rig = try Rig()
    defer { try? FileManager.default.removeItem(at: rig.root) }
    // an allowed folder that contains Scratch and the home's secrets (the gateway refuses such folders; the kernel layer does not depend on that)
    let p = try rig.profile(session: rig.project, folders: [rig.root.path])
    #expect(try rig.canRead(p, rig.outside + "/x.txt"))  // inside the granted folder
    #expect(try !rig.canRead(p, rig.scratch + "/state/s") && !rig.canRead(p, rig.app + "/state.json") && !rig.canRead(p, rig.home + "/.ssh/config"))
    #expect(try !rig.canRead(p, rig.home + "/.claude/history.jsonl"))
}

@Test func projectFolderIsClaudeCodesFolderName() throws {
    #expect(Native.claudeProjectFolder("/Users/uros/Development/Claude/ClaudeMini") == "-Users-uros-Development-Claude-ClaudeMini")
    #expect(Native.claudeProjectFolder("/Volumes/Fast SSD/a.b_c") == "-Volumes-Fast-SSD-a-b-c")
    #expect(Native.claudeProjectFolder("/p/Šola") == "-p--ola")
    #expect(Native.claudeProjectFolder("/p/😀") == "-p---")  // two UTF-16 units
    // over 200 characters (checked with a real claude: the name of this 266-character path was `…-forq4m`): the first 200 characters, "-" and the hash of the path
    let long = "/private/tmp/mgwseatbelt/" + String(repeating: "a", count: 120) + "/" + String(repeating: "b", count: 120)
    let name = Native.claudeProjectFolder(long)
    #expect(name.count == 207 && name.hasSuffix("-forq4m") && name.hasPrefix("-private-tmp-mgwseatbelt-aaaa"))
    // a sibling session with the same first 200 characters has another folder, and the profile of one does not open the other
    let sibling = long + "c"
    #expect(Native.claudeProjectFolder(sibling) != name && Native.claudeProjectFolder(sibling).hasPrefix(String(name.prefix(200))))
    let rig = try Rig()
    defer { try? FileManager.default.removeItem(at: rig.root) }
    for n in [name, Native.claudeProjectFolder(sibling)] {
        try FileManager.default.createDirectory(atPath: rig.home + "/.claude/projects/" + n, withIntermediateDirectories: true)
        try Data("t".utf8).write(to: URL(fileURLWithPath: rig.home + "/.claude/projects/" + n + "/t.jsonl"))
    }
    let p = try rig.profile(session: long)
    #expect(try rig.canRead(p, rig.home + "/.claude/projects/" + name + "/t.jsonl") && rig.canWrite(p, rig.home + "/.claude/projects/" + name + "/n.jsonl"))
    #expect(try !rig.canRead(p, rig.home + "/.claude/projects/" + Native.claudeProjectFolder(sibling) + "/t.jsonl"))
}

@Test func grantsComeFromTheRestrictedArguments() {
    var c = ClaudeCLIConfig(); c.tools = "Read,Grep"
    let ro = ClaudeSeatbelt.grants(args: c.agentArgs(cwd: URL(fileURLWithPath: "/s"), allTools: [], folders: AllowedFolders(), checkedPaths: ["/w1", "/w2"]))
    #expect(ro.folders == ["/w1", "/w2"] && !ro.writable && ro.sources == "")
    c.tools = "Read,Edit"
    #expect(ClaudeSeatbelt.grants(args: c.agentArgs(cwd: URL(fileURLWithPath: "/s"), allTools: [], folders: AllowedFolders(), checkedPaths: [])).writable)
}

@Test func confineMakesTheRunsTempFolderPrivate() {
    var env = ["TMPDIR": "/var/folders/x/T/", "CLAUDE_CODE_TMPDIR": "/tmp/claude-501"]
    ClaudeSeatbelt.confine(&env, tmp: "/scratch/tmp/run-req_1")
    #expect(env["CLAUDE_CODE_TMPDIR"] == "/scratch/tmp/run-req_1" && env["TMPDIR"] == "/scratch/tmp/run-req_1/" && env["TMP"] == env["CLAUDE_CODE_TMPDIR"])
}

/// The kernel rows of the self-check (no model): a profile that works passes, one that lets the outside read through is reported with its reason.
@Test func kernelRowsOfTheSelfCheck() throws {
    let rig = try Rig()
    defer { try? FileManager.default.removeItem(at: rig.root) }
    try Data("CANARY".utf8).write(to: URL(fileURLWithPath: rig.project + "/canary.txt"))
    try Data("OUTSIDE".utf8).write(to: URL(fileURLWithPath: rig.outside + "/secret.txt"))
    let good = try rig.profile(session: rig.project)
    #expect(ClaudeSeatbelt.kernelRows(profile: good, session: rig.project, outside: rig.outside, canary: "CANARY") == nil)
    let leaky = good + "(allow file-read* file-write* (subpath \"\(rig.outside)\"))\n"  // a profile that would let the outside through
    #expect(ClaudeSeatbelt.kernelRows(profile: leaky, session: rig.project, outside: rig.outside, canary: "CANARY")?.contains("does not stop a read") == true)
    let closed = "(version 1)\n(allow default)\n(deny file-read* file-write*)\n(allow file-read* (literal \"/\"))\n"  // claude's folder not readable either
    #expect(ClaudeSeatbelt.kernelRows(profile: closed, session: rig.project, outside: rig.outside, canary: "CANARY") != nil)
}

/// A fake claude: starts under the profile, reads canary.txt of its cwd and answers it, or fails as a broken profile would.
private func fakeClaude(_ dir: URL, body: String) throws -> String {
    let f = dir.appendingPathComponent("claude")
    try Data("#!/bin/sh\nif [ \"$1\" = --version ]; then echo '9.9.9 (Claude Code)'; exit 0; fi\n\(body)\n".utf8).write(to: f)
    chmod(f.path, 0o755)
    return f.path
}

@Test func selfCheckPassesFailsAndSaysWhyWithAFakeClaude() async throws {
    let rig = try Rig()
    defer { try? FileManager.default.removeItem(at: rig.root) }
    let base = URL(fileURLWithPath: rig.scratch + "/tmp")
    let canaryRead = "cat canary.txt; cat >/dev/null"  // answers the canary: reads it through the profile
    let good = try fakeClaude(rig.root, body: canaryRead)
    #expect(ClaudeSeatbelt.checkNow(exe: good, tmpBase: base, home: rig.home) == .pass)
    // claude that cannot start under the profile but works without it (an update needs a path the profile does not allow): a failure with its reason
    let broken = try fakeClaude(rig.root, body:
        "if [ -n \"$CLAUDE_CODE_TMPDIR\" ]; then echo 'EPERM: operation not permitted, open /needed/by/new/claude' >&2; exit 1; fi\n" + canaryRead)
    if case .fail(let why) = ClaudeSeatbelt.checkNow(exe: broken, tmpBase: base, home: rig.home) { #expect(why.contains("EPERM") && why.contains("under the profile")) }
    else { Issue.record("expected .fail") }
    // fails the same way without the profile (network, login): not the profile's fault
    let down = try fakeClaude(rig.root, body: "echo 'Not logged in' ; exit 1")
    #expect(ClaudeSeatbelt.checkNow(exe: down, tmpBase: base, home: rig.home) == .inconclusive)
    #expect((try FileManager.default.contentsOfDirectory(atPath: base.path)).filter { $0.hasPrefix("run-seatbelt") }.isEmpty)  // its temp folder is gone
}

private final class Clock: @unchecked Sendable {
    private let lock = NSLock()
    private var t = Date(timeIntervalSince1970: 1_000_000)
    var now: Date { lock.lock(); defer { lock.unlock() }; return t }
    func advance(_ s: TimeInterval) { lock.lock(); t += s; lock.unlock() }
}

/// F3: an outage or an expired login (the control run fails too) is not checked again on every request: for ten minutes per binary the run just uses the profile; then once more.
@Test func inconclusiveCheckIsRememberedForTenMinutes() async throws {
    let rig = try Rig()
    defer { try? FileManager.default.removeItem(at: rig.root) }
    let base = URL(fileURLWithPath: rig.scratch + "/tmp")
    let counter = rig.root.appendingPathComponent("runs.txt")
    let exe = try fakeClaude(rig.root, body: "echo x >> \"\(counter.path)\"; echo 'Not logged in'; exit 1")  // fails with and without the profile
    let clock = Clock()
    let seatbelt = ClaudeSeatbelt(home: rig.home, now: { clock.now })
    let cache = rig.root.appendingPathComponent("cache.json")
    func runs() -> Int { ((try? String(contentsOf: counter, encoding: .utf8)) ?? "").split(separator: "\n").count }
    let first = await seatbelt.ensure(exe: exe, tmpBase: base, cacheFile: cache)
    #expect(first && runs() == 1)  // the check: the control run counts (the run under the profile cannot write the counter outside its folders)
    for _ in 0..<3 { let again = await seatbelt.ensure(exe: exe, tmpBase: base, cacheFile: cache); #expect(again) }
    #expect(runs() == 1)  // not again
    clock.advance(ClaudeSeatbelt.inconclusiveFor - 1)
    _ = await seatbelt.ensure(exe: exe, tmpBase: base, cacheFile: cache)
    #expect(runs() == 1)
    clock.advance(2)
    _ = await seatbelt.ensure(exe: exe, tmpBase: base, cacheFile: cache)
    #expect(runs() == 2)  // after the ten minutes: checked once more
    #expect(await seatbelt.state == .unchecked)  // never turned off by an outage
}

@Test func ensureCachesOnDiskAndReportsAFailureOnce() async throws {
    let rig = try Rig()
    defer { try? FileManager.default.removeItem(at: rig.root) }
    let base = URL(fileURLWithPath: rig.scratch + "/tmp")
    let cache = rig.root.appendingPathComponent("claude-seatbelt.json")
    let exe = try fakeClaude(rig.root, body: "cat canary.txt; cat >/dev/null")
    let first = ClaudeSeatbelt(home: rig.home)
    let firstOK = await first.ensure(exe: exe, tmpBase: base, cacheFile: cache)
    let firstState = await first.state
    #expect(firstOK && firstState == .on)
    #expect(FileManager.default.fileExists(atPath: cache.path))
    // a second process with the cache: no check at all (a check in a folder that cannot be created would fail)
    let second = ClaudeSeatbelt(home: rig.home)
    let secondOK = await second.ensure(exe: exe, tmpBase: URL(fileURLWithPath: "/nonexistent-seatbelt/tmp"), cacheFile: cache)
    let secondState = await second.state
    #expect(secondOK && secondState == .on)
    // the binary changed (an update): checked again; a claude that fails only under the profile turns the layer off with the reason
    try Data("#!/bin/sh\nif [ \"$1\" = --version ]; then echo '9.9.10 (Claude Code)'; exit 0; fi\nif [ -n \"$CLAUDE_CODE_TMPDIR\" ]; then echo 'EPERM new path' >&2; exit 1; fi\ncat canary.txt; cat >/dev/null\n".utf8).write(to: URL(fileURLWithPath: exe))
    let third = ClaudeSeatbelt(home: rig.home)
    let thirdOK = await third.ensure(exe: exe, tmpBase: base, cacheFile: cache)
    #expect(!thirdOK)
    let reason = await third.offReason
    #expect(reason.contains("EPERM new path"))
    #expect(ClaudeSeatbelt.offLine(reason).hasPrefix("Claude CLI without local tools runs without the system sandbox: "))
    let thirdAgain = await third.ensure(exe: exe, tmpBase: URL(fileURLWithPath: "/nonexistent-seatbelt/tmp"), cacheFile: cache)
    #expect(!thirdAgain)  // remembered for this binary, not checked per request
}

// MARK: real claude (gated)

/// Real `claude -p`: the weak 0.5.27 argv (bare tool names, acceptEdits: Claude Code's rules let any path through) under the profile must get EPERM from the kernel for
/// a file outside the session folder. The one allowed skip: claude is not installed or not logged in.
@Test func realClaudeWithWeakRulesIsStoppedByTheKernel() async throws {
    guard let exe = await ClaudeCLI.executable("") else { try Test.cancel("claude is not installed") }
    let home = NSHomeDirectory()
    let base = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("mgw-seatbelt-real-test")  // a fixed path: claude keeps one transcript folder for it in ~/.claude/projects, not one per run
    try FileManager.default.createDirectory(at: base, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: base) }
    let session = base.appendingPathComponent("session"), outside = base.appendingPathComponent("outside"), tmp = base.appendingPathComponent("tmp")
    for d in [session, outside, tmp] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
    let secret = "OUTSIDE-" + UUID().uuidString.lowercased()
    try Data(secret.utf8).write(to: outside.appendingPathComponent("secret.txt"))
    try Data("INSIDE".utf8).write(to: session.appendingPathComponent("in.txt"))
    let real = try #require(Sync.real(URL(fileURLWithPath: exe))?.path)
    var env = ProcessInfo.processInfo.environment
    for k in env.keys where k.hasPrefix("ANTHROPIC_") || k.hasPrefix("CLAUDE_CODE_") || k == "CLAUDECODE" { env[k] = nil }
    env["PATH"] = (env["PATH"] ?? "") + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
    let weak = ["-p", "--output-format", "json", "--model", "haiku", "--strict-mcp-config", "--tools", "Read,Grep,Glob,Edit,Write",
                "--allowedTools", "Read,Grep,Glob,Edit,Write", "--permission-mode", "acceptEdits", "--setting-sources", ""]
    let ask = "Use the Read tool on \(outside.path)/secret.txt and on in.txt. Report exactly what each returned, including errors."
    // not logged in (or no network): the only skip
    let control = ClaudeSeatbelt.capture(exe, weak, env: env, cwd: session.path, input: ask, timeout: 120, log: tmp)
    guard control.out.contains(secret) else { try Test.cancel("claude is not logged in or not reachable here (the unsandboxed control run did not answer): \(control.err.prefix(120)) \(control.out.prefix(120))") }
    let profile = try #require(Native.claudeRestrictedProfile(session: Sync.real(session)!.path, folders: [], writable: true, tmp: Sync.real(tmp)!.path, exe: real, home: home,
                                                              hidden: RouterConfig.hiddenScratch(nil)))
    var boxed = env
    ClaudeSeatbelt.confine(&boxed, tmp: Sync.real(tmp)!.path)
    let r = ClaudeSeatbelt.capture("/usr/bin/sandbox-exec", ["-p", profile, exe] + weak, env: boxed, cwd: session.path, input: ask, timeout: 120, log: tmp)
    #expect(!r.out.contains(secret), "the kernel let the outside file through: \(r.out.prefix(300))")
    #expect(r.out.contains("EPERM") || r.out.contains("operation not permitted"), "\(r.out.prefix(400)) \(r.err.prefix(200))")
    #expect(r.out.contains("INSIDE"))  // the session folder itself is readable
    // and the check itself passes with this claude
    #expect(ClaudeSeatbelt.checkNow(exe: exe, tmpBase: base.appendingPathComponent("tmp"), home: home) == .pass)
}

/// A token refresh stores the new token in the login keychain through `security`: under the profile that must work (a refresh that cannot be saved loses the rotated refresh
/// token), and the profile must still not let the process write another file of the Keychains folder. One throw-away item (unique service name), removed afterwards.
@Test func keychainItemCanBeWrittenUnderTheProfile() throws {
    let home = NSHomeDirectory()
    guard FileManager.default.fileExists(atPath: home + "/Library/Keychains/login.keychain-db") else { try Test.cancel("no login keychain on this Mac") }
    let rig = try Rig()
    defer { try? FileManager.default.removeItem(at: rig.root) }
    let profile = try #require(Native.claudeRestrictedProfile(session: rig.project, folders: [], writable: false, tmp: rig.tmp, exe: "/opt/homebrew/bin/claude", home: home, hidden: rig.hidden))
    let service = "mgw-seatbelt-test-" + UUID().uuidString.lowercased()
    func security(_ args: [String], boxed: Bool) throws -> Int32 {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: boxed ? "/usr/bin/sandbox-exec" : "/usr/bin/security")
        p.arguments = (boxed ? ["-p", profile, "/usr/bin/security"] : []) + args
        p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice; p.standardInput = FileHandle.nullDevice
        try p.run(); p.waitUntilExit()
        return p.terminationStatus
    }
    defer { _ = try? security(["delete-generic-password", "-s", service], boxed: false) }
    #expect(try security(["add-generic-password", "-s", service, "-a", "seatbelt", "-w", "x"], boxed: true) == 0)
    #expect(try security(["find-generic-password", "-s", service], boxed: false) == 0)  // really stored
    #expect(try rig.ok(profile, "/usr/bin/touch", [home + "/Library/Keychains/mgw-seatbelt-not-allowed"]) == false)
}

// MARK: the launch of a restricted run (Router.handle, a fake claude)

/// Serialized: both tests read and set the process-wide self-check state (`Router.claudeSeatbelt`).
@Suite(.serialized) struct RestrictedLaunchTests {

    /// A restricted run through the router: the fake claude starts under the profile (self-check first), reports what the kernel lets it do, its private temp folder is
    /// gone afterwards, and a claude that fails only under the profile leaves the run on the rules alone with the reason in the status.
    @Test func restrictedRunGoesUnderTheProfileAndCleansUpItsTempFolder() async throws {
        let base = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("seatbelt-run-" + UUID().uuidString.lowercased(), isDirectory: true)
        let project = base.appendingPathComponent("proj"), other = base.appendingPathComponent("other"), bin = base.appendingPathComponent("bin")
        for d in [project, other, bin] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
        defer { try? FileManager.default.removeItem(at: base) }
        try Data("outside".utf8).write(to: other.appendingPathComponent("o.txt"))
        try Data("inside".utf8).write(to: project.appendingPathComponent("p.txt"))
        let exe = bin.appendingPathComponent("claude")
        try Data("""
            #!/bin/sh
            [ "$1" = --version ] && { echo '2.1.0 (Claude Code)'; exit 0; }
            cat > /dev/null
            if [ -f canary.txt ]; then cat canary.txt; exit 0; fi
            { echo "tmp=$CLAUDE_CODE_TMPDIR"
              cat "\(other.path)/o.txt" > /dev/null 2>&1 && echo outside=READ || echo outside=DENIED
              cat p.txt > /dev/null 2>&1 && echo inside=READ || echo inside=DENIED
              touch "$CLAUDE_CODE_TMPDIR/t" 2>/dev/null && echo tmpwrite=OK || echo tmpwrite=DENIED
              touch w.txt 2>/dev/null && echo write=OK || echo write=DENIED
            } > report.txt
            echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
            echo '{"type":"result","subtype":"success","usage":{"output_tokens":3}}'

            """.utf8).write(to: exe)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: exe.path)
        var c = RouterConfig()
        c.scratchFolder = base.appendingPathComponent("scratch").path
        c.claudeCLI.enabled = true; c.claudeCLI.executable = exe.path; c.claudeCLI.tools = "Read,Edit"; c.claudeCLI.allowedRoots = project.path
        let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
        let h = ["Authorization": "Bearer tok", "x-claude-mini-session": UUID().uuidString, "x-claude-mini-cwd": project.path]
        let w = FakeWriter()
        await router.handle(request("POST", "/v1/messages", headers: h, body: #"{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}"#), writer: w)
        #expect(w.status == 200, "\(w.text.prefix(300))")
        let report = (try? String(contentsOf: project.appendingPathComponent("report.txt"), encoding: .utf8)) ?? ""
        #expect(report.contains("outside=DENIED") && report.contains("inside=READ") && report.contains("tmpwrite=OK") && report.contains("write=OK"), "\(report)")
        let tmp = try #require(report.split(separator: "\n").first { $0.hasPrefix("tmp=") }).dropFirst(4)
        #expect(tmp.contains("/Mini Gateway Scratch/tmp/run-req_"), "\(tmp)")
        #expect(!FileManager.default.fileExists(atPath: String(tmp)), "\(tmp)")  // private, in Scratch/tmp, removed after the run
        #expect(await Router.claudeSeatbelt.state == .on)
    }

    /// A claude that does not run under the profile: the run still answers (rules alone), the log/status carry the reason, and nothing is wrapped.
    @Test func failedSelfCheckFallsBackToTheRulesAndSaysSo() async throws {
        let base = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("seatbelt-fb-" + UUID().uuidString.lowercased(), isDirectory: true)
        let project = base.appendingPathComponent("proj"), bin = base.appendingPathComponent("bin")
        for d in [project, bin] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
        defer { try? FileManager.default.removeItem(at: base) }
        let exe = bin.appendingPathComponent("claude")
        try Data("""
            #!/bin/sh
            [ "$1" = --version ] && { echo '2.1.1 (Claude Code)'; exit 0; }
            cat > /dev/null
            [ -n "$CLAUDE_CODE_TMPDIR" ] && { echo 'EPERM: operation not permitted, open /new/path' >&2; exit 1; }
            if [ -f canary.txt ]; then cat canary.txt; exit 0; fi
            echo "unwrapped" > report.txt
            echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
            echo '{"type":"result","subtype":"success","usage":{"output_tokens":3}}'

            """.utf8).write(to: exe)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: exe.path)
        var c = RouterConfig()
        c.scratchFolder = base.appendingPathComponent("scratch").path
        c.claudeCLI.enabled = true; c.claudeCLI.executable = exe.path; c.claudeCLI.allowedRoots = project.path
        let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
        let h = ["Authorization": "Bearer tok", "x-claude-mini-session": UUID().uuidString, "x-claude-mini-cwd": project.path]
        let w = FakeWriter()
        await router.handle(request("POST", "/v1/messages", headers: h, body: #"{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}"#), writer: w)
        #expect(w.status == 200, "\(w.text.prefix(300))")
        #expect((try? String(contentsOf: project.appendingPathComponent("report.txt"), encoding: .utf8)) == "unwrapped\n")
        let reason = await Router.claudeSeatbelt.offReason
        #expect(reason.contains("EPERM") && reason.contains("under the profile"), "\(reason)")
        // F4: the status line is there at once (admin status of a router that never ran it), no restart
        let st = await admin(FakeApp(c).router(), "GET", "/status")
        let json = try #require(st.text.firstIndex(of: "{").map { String(st.text[$0...]) })
        let body = try #require(try JSONSerialization.jsonObject(with: Data(json.utf8)) as? [String: Any])
        #expect(body["claudeSandbox"] as? String == "off" && (body["claudeSandboxReason"] as? String)?.contains("EPERM") == true, "\(st.text.prefix(400))")
    }

    /// Real claude through the router, no fake: the restricted argv of production under the profile reads, writes, and resumes the conversation. The one allowed skip:
    /// claude is not installed or not logged in (an unsandboxed one-word probe does not answer).
    @Test func realRestrictedRunReadsWritesAndResumes() async throws {
        guard let exe = await ClaudeCLI.executable("") else { try Test.cancel("claude is not installed") }
        let base = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("mgw-seatbelt-real-run")  // fixed: one transcript folder in ~/.claude/projects
        let project = base.appendingPathComponent("proj"), logDir = base.appendingPathComponent("log")
        for d in [project, logDir] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
        defer { try? FileManager.default.removeItem(at: base) }
        var env = ProcessInfo.processInfo.environment
        for k in env.keys where k.hasPrefix("ANTHROPIC_") || k.hasPrefix("CLAUDE_CODE_") || k == "CLAUDECODE" { env[k] = nil }
        let probe = ClaudeSeatbelt.capture(exe, ["-p", "--model", "haiku", "--tools", "", "--no-session-persistence"], env: env, cwd: logDir.path, input: "Reply with the word ok.", timeout: 120, log: logDir)
        guard probe.out.lowercased().contains("ok") else { try Test.cancel("claude is not logged in or not reachable here: \(probe.err.prefix(120)) \(probe.out.prefix(120))") }
        let token = "TOKEN-" + UUID().uuidString.lowercased().prefix(8)
        try Data(token.utf8).write(to: project.appendingPathComponent("p.txt"))
        var c = RouterConfig()
        c.scratchFolder = base.appendingPathComponent("scratch").path
        c.claudeCLI.enabled = true; c.claudeCLI.tools = "Read,Grep,Glob,Edit,Write"; c.claudeCLI.allowedRoots = project.path
        let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
        let h = ["Authorization": "Bearer tok", "x-claude-mini-session": UUID().uuidString, "x-claude-mini-cwd": project.path]
        func turn(_ messages: [[String: String]]) async -> (Int, String) {
            let body = (try? JSONSerialization.data(withJSONObject: ["model": "claude-cli-haiku", "tools": [["name": "x"]], "messages": messages] as [String: Any])) ?? Data()
            let w = FakeWriter()
            await router.handle(request("POST", "/v1/messages", headers: h, body: String(decoding: body, as: UTF8.self)), writer: w)
            let text = ((try? JSONSerialization.jsonObject(with: Data(w.text.utf8)) as? [String: Any])?["content"] as? [[String: Any]])?.first?["text"] as? String ?? w.text
            return (w.status, text)
        }
        let q1 = "Use the Read tool on p.txt and reply with its exact content, then use the Write tool to create w.txt containing the word WRITTEN."
        let (s1, a1) = await turn([["role": "user", "content": q1]])
        #expect(s1 == 200 && a1.contains(token), "\(s1) \(a1.prefix(300))")
        #expect((try? String(contentsOf: project.appendingPathComponent("w.txt"), encoding: .utf8))?.contains("WRITTEN") == true)
        let on = await Router.claudeSeatbelt.state
        #expect(on == .on, "\(on)")
        let (s2, a2) = await turn([["role": "user", "content": q1], ["role": "assistant", "content": a1], ["role": "user", "content": "Without using any tool: which file did you just create?"]])
        #expect(s2 == 200 && a2.contains("w.txt"), "\(s2) \(a2.prefix(300))")  // the conversation was resumed
    }
}
