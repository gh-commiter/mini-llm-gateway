import Foundation
import Testing
@testable import RouterCore

// V1: under the generated Seatbelt profile (the `minigw run` side) the submodule's config, hooks and info cannot be written. (The profile denies the whole top-level
// `.git` of the folder by design: a commit in the repository or in a submodule is not possible under `minigw run`; the sandbox settings of Claude Code, which allow
// it, are checked with a real `claude -p` run).

private func sh(_ cmd: String, cwd: String, profile: String? = nil) -> (status: Int32, out: String) {
    let p = Process(), pipe = Pipe()
    p.executableURL = URL(fileURLWithPath: profile == nil ? "/bin/sh" : "/usr/bin/sandbox-exec")
    p.arguments = profile.map { ["-p", $0, "/bin/sh", "-c", cmd] } ?? ["-c", cmd]
    p.currentDirectoryURL = URL(fileURLWithPath: cwd)
    p.standardOutput = pipe; p.standardError = pipe
    var env = ProcessInfo.processInfo.environment
    env["GIT_CONFIG_GLOBAL"] = "/dev/null"; env["GIT_CONFIG_SYSTEM"] = "/dev/null"; env["GIT_TERMINAL_PROMPT"] = "0"
    p.environment = env
    guard (try? p.run()) != nil else { return (-1, "cannot run") }
    var data = Data()
    let t = Thread { data = pipe.fileHandleForReading.readDataToEndOfFile() }
    t.start(); p.waitUntilExit(); while t.isExecuting { usleep(5_000) }
    return (p.terminationStatus, String(decoding: data, as: UTF8.self))
}

@Test func submoduleConfigAndHooksAreDeniedInTheSeatbeltProfile() throws {
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("subm-" + UUID().uuidString)
    defer { try? fm.removeItem(at: base) }
    let upstream = base.appendingPathComponent("up"), repo = base.appendingPathComponent("repo"), home = base.appendingPathComponent("home")
    for d in [upstream, repo, home] { try fm.createDirectory(at: d, withIntermediateDirectories: true) }
    let id = "-c user.email=a@b -c user.name=n -c protocol.file.allow=always"
    #expect(sh("git init -q && echo a > a && git add a && git \(id) commit -qm up", cwd: upstream.path).status == 0)
    let setup = sh("git init -q && git \(id) submodule add -q \(upstream.path) sub && git \(id) commit -qm add", cwd: repo.path)
    #expect(setup.status == 0, "\(setup.out)")
    let modules = repo.appendingPathComponent(".git/modules/sub")
    #expect(fm.fileExists(atPath: modules.appendingPathComponent("config").path))
    let profile = try #require(Native.sandboxProfile(dir: repo.path, home: home.path, network: false, hidden: [], writable: [repo.path]))
    let hook = sh("echo 'echo pwned' > .git/modules/sub/hooks/pre-commit", cwd: repo.path, profile: profile)
    let cfg = sh("echo '[core]' >> .git/modules/sub/config", cwd: repo.path, profile: profile)
    let info = sh("mkdir -p .git/modules/sub/info && echo x > .git/modules/sub/info/attributes", cwd: repo.path, profile: profile)
    let common = sh("echo ../../../evil > .git/modules/sub/commondir", cwd: repo.path, profile: profile)  // V5: git loads config and hooks from the directory it names
    let wt = sh("mkdir -p .git/modules/sub/worktrees/x && echo x > .git/modules/sub/worktrees/x/gitdir", cwd: repo.path, profile: profile)
    #expect(hook.status != 0 && cfg.status != 0 && info.status != 0 && common.status != 0 && wt.status != 0, "hook \(hook) config \(cfg) info \(info) commondir \(common) worktrees \(wt)")
    #expect(!fm.fileExists(atPath: modules.appendingPathComponent("commondir").path) && !fm.fileExists(atPath: modules.appendingPathComponent("worktrees").path))
    #expect(!fm.fileExists(atPath: modules.appendingPathComponent("hooks/pre-commit").path))
    #expect(!((try? String(contentsOf: modules.appendingPathComponent("config"), encoding: .utf8)) ?? "").hasSuffix("[core]\n"))
}

// V2: the same on a case-insensitive volume under another letter case (`.GIT`, nested): does the Seatbelt profile still stop the write?
@Test func gitPathsAreDeniedInTheSeatbeltProfileUnderAnotherCase() throws {
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("case-" + UUID().uuidString)
    defer { try? fm.removeItem(at: base) }
    let repo = base.appendingPathComponent("repo"), home = base.appendingPathComponent("home")
    for d in [repo, home] { try fm.createDirectory(at: d, withIntermediateDirectories: true) }
    #expect(sh("git init -q && mkdir nested && cd nested && git init -q", cwd: repo.path).status == 0)
    let profile = try #require(Native.sandboxProfile(dir: repo.path, home: home.path, network: false, hidden: [], writable: [repo.path]))
    guard sh("test -e .GIT", cwd: repo.path).status == 0 else { return }  // a case-sensitive volume: nothing to test
    for cmd in ["echo x >> .GIT/config", "echo x > .Git/hooks/pre-commit", "echo x >> nested/.GIT/config", "echo x > nested/.Git/hooks/pre-commit", "echo x >> nested/.git/config"] {
        let r = sh(cmd, cwd: repo.path, profile: profile)
        #expect(r.status != 0, "\(cmd): \(r)")
    }
    #expect(!((try? String(contentsOf: repo.appendingPathComponent("nested/.git/config"), encoding: .utf8)) ?? "").hasSuffix("x\n"))
    #expect(!fm.fileExists(atPath: repo.appendingPathComponent("nested/.git/hooks/pre-commit").path))
}
