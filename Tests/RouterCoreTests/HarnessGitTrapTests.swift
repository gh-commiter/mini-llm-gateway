import Foundation
import Testing
@testable import RouterCore

// #187: in direct and sync mode the agent harness (Claude CLI with local tools, Codex) runs git itself, outside its sandbox, in the client's folder, whose
// `.git/config`, `.git/info/attributes`, `.gitattributes` and `.gitmodules` the client writes. A scratch repository carries a trap in every place git could run a
// command from; a trap only creates a marker file in the test's temp folder. The harness environment comes from the function the gateway uses at launch.

/// The repository with the traps and the marker folder.
struct TrapRepo {
    let base: URL, repo: URL, markers: URL

    init() throws {
        base = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("mgw-harness-git-" + UUID().uuidString.lowercased(), isDirectory: true)
        repo = base.appendingPathComponent("r"); markers = base.appendingPathComponent("m")
        for d in [repo, markers, base.appendingPathComponent("home"), base.appendingPathComponent("hooks")] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
        let r = TrapRepo.run(["/bin/sh", "-c", TrapRepo.setup], cwd: repo, env: ["PATH": TrapRepo.path, "HOME": base.appendingPathComponent("home").path, "M": markers.path,
                                                                             "H": base.appendingPathComponent("hooks").path, "GIT_CONFIG_NOSYSTEM": "1"])
        guard r.status == 0 else { throw Failure(text: "setup: \(r.out)") }
    }
    struct Failure: Error { let text: String }
    func cleanup() { try? FileManager.default.removeItem(at: base) }

    static let path = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
    /// What the harness could run: status, diffs, log with patch and signature, show, add --dry-run, stash list.
    static let commands = [["status"], ["diff"], ["diff", "--cached"], ["log", "-p", "-1"], ["show"], ["add", "-n", "."], ["stash", "list"]]

    /// Two commits (the second signed, so that log.showSignature calls gpg.program), a staged change, two same-size changes with an old mtime (git must read them,
    /// through the clean filter), then the traps. Every trap: `touch $M/<name>`.
    static let setup = #"""
        set -e
        g() { git -c core.hooksPath=/dev/null "$@"; }
        g init -q . && g config user.name t && g config user.email t@example.com
        printf 'a\n' > a.txt; printf 'b\n' > b.dat; printf 'c\n' > c.inc; printf 'd\n' > d.txt
        g add . && g commit -qm one
        printf 'A\n' > a.txt; printf 'B\n' > b.dat; printf '*.dat filter=p diff=y\n*.inc filter=z diff=y\n' > .gitattributes
        g add . && g commit -qm two
        signed=$(g cat-file commit HEAD | awk '{print} /^committer /{print "gpgsig -----BEGIN PGP SIGNATURE-----"; print " x"; print " -----END PGP SIGNATURE-----"}' | g hash-object -t commit -w --stdin)
        g update-ref HEAD "$signed"
        printf 'C\n' > c.inc && g add c.inc
        printf 'Z\n' > a.txt; printf 'Y\n' > b.dat; touch -t 200001010000 a.txt b.dat
        printf '[submodule "s"]\n\tpath = s\n\turl = ./nowhere\n\tupdate = !touch "%s/gitmodules.update"\n' "$M" > .gitmodules
        mkdir -p .git/info && printf '*.txt filter=x diff=x\n' > .git/info/attributes
        for h in post-index-change pre-commit pre-push post-checkout reference-transaction; do printf '#!/bin/sh\ntouch "%s/hook.%s"\n' "$M" "$h" > "$H/$h"; chmod +x "$H/$h"; done
        printf '#!/bin/sh\ntouch "%s/fsmonitor"\n' "$M" > "$H/fsmonitor"; chmod +x "$H/fsmonitor"
        c() { git config "$1" "$2"; }
        c filter.x.clean "touch '$M/filter.clean'; cat"
        c filter.x.smudge "touch '$M/filter.smudge'; cat"
        c filter.p.process "touch '$M/filter.process'; exit 1"
        c diff.external "touch '$M/diff.external'; true"
        c diff.x.textconv "touch '$M/diff.textconv'; cat"
        c core.fsmonitor "$H/fsmonitor"
        c core.hooksPath "$H"
        c core.pager "touch '$M/core.pager'; cat"
        c core.sshCommand "touch '$M/core.sshCommand'; false"
        c gpg.program "$H/gpg"; printf '#!/bin/sh\ntouch "%s/gpg.program"\n' "$M" > "$H/gpg"; chmod +x "$H/gpg"
        c log.showSignature true
        c core.alternateRefsCommand "touch '$M/core.alternateRefsCommand'; true"
        c include.path included.cfg
        printf '[diff "y"]\n\ttextconv = touch \\"%s/include.textconv\\"; cat\n[filter "z"]\n\tclean = touch \\"%s/include.filter.clean\\"; cat\n' "$M" "$M" > .git/included.cfg
        """#

    /// Runs every command with `env` (the harness environment over a bare PATH and HOME) and gives, per command, the traps that fired (empty: none).
    func fired(_ env: [String: String]) -> [String: [String]] {
        var out: [String: [String]] = [:]
        let full = ["PATH": TrapRepo.path, "HOME": base.appendingPathComponent("home").path].merging(env) { $1 }
        for cmd in TrapRepo.commands {
            _ = TrapRepo.run(["/usr/bin/env", "git"] + cmd, cwd: repo, env: full)
            let hit = ((try? FileManager.default.contentsOfDirectory(atPath: markers.path)) ?? []).sorted()
            for h in hit { try? FileManager.default.removeItem(at: markers.appendingPathComponent(h)) }
            if !hit.isEmpty { out[cmd.joined(separator: " ")] = hit }
        }
        return out
    }

    /// A process with a 30 s ceiling (a filter that never answers must not hang the suite).
    static func run(_ argv: [String], cwd: URL, env: [String: String]) -> (status: Int32, out: String) {
        let p = Process(), pipe = Pipe()
        p.executableURL = URL(fileURLWithPath: argv[0]); p.arguments = Array(argv.dropFirst()); p.currentDirectoryURL = cwd; p.environment = env
        p.standardOutput = pipe; p.standardError = pipe; p.standardInput = FileHandle.nullDevice
        guard (try? p.run()) != nil else { return (-1, "cannot start \(argv[0])") }
        let killer = DispatchWorkItem { p.terminate() }
        DispatchQueue.global().asyncAfter(deadline: .now() + 30, execute: killer)
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit(); killer.cancel()
        return (p.terminationStatus, String(decoding: data, as: UTF8.self))
    }
}

/// The traps work (control): git with no protection runs them; otherwise the checks below would prove nothing.
@Test(.blocking) func theGitTrapsFireWithoutProtection() throws {
    let t = try TrapRepo(); defer { t.cleanup() }
    let f = t.fired([:])
    let all = Set(f.values.joined())
    for trap in ["filter.clean", "diff.external", "diff.textconv", "gpg.program", "include.textconv"] { #expect(all.contains(trap), "\(trap) never fired: \(f)") }
}

/// #187: the Claude CLI harness of a direct-mode (mount) and a sync-mode run with local tools runs nothing from the client's repository.
@Test(.blocking, arguments: [true, false]) func theClaudeHarnessRunsNothingFromTheClientsRepository(mount: Bool) throws {
    let t = try TrapRepo(); defer { t.cleanup() }
    let env = ClaudeCLIConfig.harnessGitEnv(mount: mount, cwd: t.repo, envFile: t.base.appendingPathComponent("env.sh").path)
    let f = t.fired(env)
    #expect(f.isEmpty, "mount=\(mount): traps fired: \(f)")
}

/// #187: the Codex harness of a direct or sync run (the same environment in both) runs nothing from the client's repository.
@Test(.blocking) func theCodexHarnessRunsNothingFromTheClientsRepository() throws {
    let t = try TrapRepo(); defer { t.cleanup() }
    let f = t.fired(Codex.directEnv(tmp: t.base.appendingPathComponent("tmp"), cwd: t.repo))
    #expect(f.isEmpty, "traps fired: \(f)")
}

/// #187: the agent's Bash (Claude Code runs CLAUDE_ENV_FILE ahead of each command) and Codex's commands (shell_environment_policy) find the folder's repository as
/// before; only the harness has none. The Bash's git runs under the agent's sandbox (not tested here).
@Test(.blocking) func theAgentsCommandsKeepTheFoldersRepository() throws {
    let t = try TrapRepo(); defer { t.cleanup() }
    let file = t.base.appendingPathComponent("env.sh")
    try Data(ClaudeCLIConfig.bashGitEnvScript.utf8).write(to: file)
    let env = ["PATH": TrapRepo.path, "HOME": t.base.appendingPathComponent("home").path].merging(ClaudeCLIConfig.harnessGitEnv(mount: true, cwd: t.repo, envFile: file.path)) { $1 }
    let harness = TrapRepo.run(["/usr/bin/env", "git", "rev-parse", "--show-toplevel"], cwd: t.repo, env: env)
    #expect(harness.status != 0, "the harness found a repository: \(harness.out)")
    let bash = TrapRepo.run(["/bin/sh", "-c", #". "$CLAUDE_ENV_FILE"; git rev-parse --show-toplevel"#], cwd: t.repo, env: env)
    #expect(bash.status == 0 && bash.out.trimmingCharacters(in: .whitespacesAndNewlines) == t.repo.path, "\(bash)")
    for write in [true, false] {
        let a = Codex.args(cwd: t.repo, extraDirs: [], agent: true, output: t.base.appendingPathComponent("o"), write: write, deny: [], tmp: nil, directGit: write)
        #expect(a.contains(#"shell_environment_policy.exclude=["GIT_DIR","GIT_CEILING_DIRECTORIES"]"#), "write=\(write): \(a)")
    }
}
