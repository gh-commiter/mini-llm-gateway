import Foundation
import Testing
@testable import RouterCore

/// Hostile repositories (issue 144): each test builds a repository whose own config tries to run a command (a marker file proves it
/// ran) and calls the product function that touches such a repository. The positive control is the same trap fired by an unguarded
/// git call. The Go client has the same tests (client/gitguard_audit_test.go).
@Suite struct GitGuardAudit {
    static func plain(_ dir: String, _ args: [String], env: [String: String] = [:]) -> (Int32, String) {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: Worktree.git)
        p.arguments = args
        p.currentDirectoryURL = URL(fileURLWithPath: dir)
        var e = ProcessInfo.processInfo.environment
        e.merge(["GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0", "GIT_AUTHOR_NAME": "a", "GIT_AUTHOR_EMAIL": "a@a",
                 "GIT_COMMITTER_NAME": "a", "GIT_COMMITTER_EMAIL": "a@a"]) { $1 }
        e.merge(env) { $1 }
        p.environment = e
        let out = Pipe()
        p.standardOutput = out; p.standardError = out
        try? p.run()
        let data = out.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        return (p.terminationStatus, String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines))
    }

    struct Hostile {
        let root: String, top: String, marker: String
        init() throws {
            root = NSTemporaryDirectory() + "gitguard-" + UUID().uuidString
            top = root + "/top"; marker = root + "/MARKER"
            try FileManager.default.createDirectory(atPath: top, withIntermediateDirectories: true)
            _ = GitGuardAudit.plain(top, ["init", "-q"])
            try "one\n".write(toFile: top + "/f.txt", atomically: true, encoding: .utf8)
            _ = GitGuardAudit.plain(top, ["add", "f.txt"])
            _ = GitGuardAudit.plain(top, ["commit", "-q", "-m", "x"])
        }
        var ran: Bool { FileManager.default.fileExists(atPath: marker) }
        func reset() { try? FileManager.default.removeItem(atPath: marker) }
        var trap: String { "touch " + marker }
    }

    /// url.<x>.insteadOf turns the path of the gateway's bundle into an ext:: command that the same config allows.
    @Test(.blocking) func fetchOfTheGatewaysBundleIgnoresARepositoryRedirect() throws {
        let h = try Hostile()
        let src = h.root + "/src"
        try FileManager.default.createDirectory(atPath: src, withIntermediateDirectories: true)
        _ = Self.plain(src, ["init", "-q", "-b", "s"]); _ = Self.plain(src, ["commit", "-q", "--allow-empty", "-m", "s"])
        let bundle = h.root + "/in.bundle"
        _ = Self.plain(src, ["bundle", "create", bundle, "HEAD"])
        _ = Self.plain(h.top, ["config", "protocol.ext.allow", "always"])
        _ = Self.plain(h.top, ["config", "url.ext::sh -c touch% \(h.marker)% #.insteadOf", "/"])
        _ = Self.plain(h.top, ["fetch", "--no-tags", "--quiet", bundle, "+HEAD:refs/minigw/c"])
        #expect(h.ran, "positive control")
        h.reset()
        _ = Worktree.fetch(top: h.top, bundle: URL(fileURLWithPath: bundle), branch: "minigw/claude-0123abcd-1")
        #expect(!h.ran, "Worktree.fetch ran a command of the repository's config")
    }

    /// core.alternateRefsCommand with objects/info/alternates: the local branch move (fetch from ".") starts upload-pack.
    @Test(.blocking) func localFetchIgnoresAnAlternateRefsCommand() throws {
        let h = try Hostile()
        let alt = h.root + "/alt"
        try FileManager.default.createDirectory(atPath: alt, withIntermediateDirectories: true)
        _ = Self.plain(alt, ["init", "-q", "--bare"])
        try (alt + "/objects\n").write(toFile: h.top + "/.git/objects/info/alternates", atomically: true, encoding: .utf8)
        _ = Self.plain(h.top, ["config", "core.alternateRefsCommand", h.trap + "; true"])
        _ = Self.plain(h.top, ["update-ref", "refs/minigw/incoming", "HEAD"])
        _ = Self.plain(h.top, ["fetch", "--no-tags", "--quiet", ".", "refs/minigw/incoming:refs/heads/probe"])
        #expect(h.ran, "positive control")
        h.reset()
        _ = Worktree.run(["fetch", "--no-tags", "--quiet", ".", "refs/minigw/incoming:refs/heads/probe2"], cwd: h.top, safe: false, hooks: false)
        #expect(!h.ran, "a client-side git call ran the alternate refs command")
    }

    /// The nested-repository snapshot (no attributes) never runs a clean filter; a snapshot of the user's own top repository does,
    /// on purpose (Git LFS): that is a design decision, recorded here.
    @Test(.blocking) func snapshotFilters() throws {
        let h = try Hostile()
        try "*.txt filter=x\n".write(toFile: h.top + "/.gitattributes", atomically: true, encoding: .utf8)
        _ = Self.plain(h.top, ["config", "filter.x.clean", "sh -c '\(h.trap); cat'"])
        try "changed\n".write(toFile: h.top + "/f.txt", atomically: true, encoding: .utf8)
        _ = Self.plain(h.top, ["add", "f.txt"])
        #expect(h.ran, "positive control")
        h.reset()
        try "changed again\n".write(toFile: h.top + "/f.txt", atomically: true, encoding: .utf8)
        _ = Worktree.snapshot(top: h.top, noAttr: true)
        #expect(!h.ran, "a nested snapshot ran a clean filter")
    }

    /// The queries the client makes of a user's repository run nothing, whatever its config says.
    @Test(.blocking) func queriesOfAHostileRepositoryRunNothing() throws {
        let h = try Hostile()
        try "*.txt filter=x diff=x\n".write(toFile: h.top + "/.git/info/attributes", atomically: true, encoding: .utf8)
        for (k, v) in ["core.fsmonitor": h.trap + "; echo", "diff.external": h.trap, "diff.x.textconv": h.trap + "; cat", "filter.x.clean": h.trap + "; cat",
                       "core.pager": h.trap + "; cat", "credential.helper": "!" + h.trap, "core.sshCommand": h.trap, "gpg.program": h.trap] {
            _ = Self.plain(h.top, ["config", k, v])
        }
        try "changed\n".write(toFile: h.top + "/f.txt", atomically: true, encoding: .utf8)
        _ = Self.plain(h.top, ["remote", "add", "origin", "https://example.invalid/x.git"])
        for args in [["rev-parse", "--verify", "-q", "HEAD^{commit}"], ["symbolic-ref", "-q", "HEAD"], ["for-each-ref"], ["rev-parse", "--git-path", "objects"],
                     ["merge-base", "--is-ancestor", "HEAD", "HEAD"], ["config", "--local", "--includes", "--get-regexp", #"^filter\."#]] {
            _ = Worktree.run(args, cwd: h.top, safe: false, hooks: false)
            #expect(!h.ran, "\(args[0]) ran a command")
        }
        #expect(ProjectEnv.gitOrigin(h.top) == "https://example.invalid/x.git")
        #expect(!h.ran, "gitOrigin ran a command")
    }
}

/// Every place of the product that starts git carries the shared guard (Worktree.noExecPins/noExecEnv or the named watch and hook
/// guards) near the invocation. A new git call elsewhere fails here: add the guard, or list it with the reason it needs none.
@Suite struct GitInvocationScan {
    @Test func everyGitInvocationCarriesAGuard() throws {
        let sources = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().appendingPathComponent("Sources")
        let allowed: [String: String] = [  // file -> the guard that must appear near the invocation
            "Worktree.swift": "noExecPins",  // run(): the one place that runs git for the router and the client
            "KBHook.swift": "gitGuardEnv",  // hook state: rev-parse and ls-files with the agent guard environment
            "ProjectIdentity.swift": "noExecPins",  // origin lookup: config --local --get
        ]
        let invocation = try NSRegularExpression(pattern: #"executable\("git"|sandbox == nil \? git|fileURLWithPath: Worktree\.git"#)
        var found = 0
        for case let url as URL in FileManager.default.enumerator(at: sources, includingPropertiesForKeys: nil)! where url.pathExtension == "swift" {
            let text = try String(contentsOf: url, encoding: .utf8)
            let lines = text.components(separatedBy: "\n")
            for (i, line) in lines.enumerated() where invocation.firstMatch(in: line, range: NSRange(line.startIndex..., in: line)) != nil {
                found += 1
                let guardName = allowed[url.lastPathComponent]
                let window = lines[max(0, i - 25)..<min(lines.count, i + 12)].joined(separator: "\n")
                #expect(guardName != nil && window.contains(guardName!), "\(url.lastPathComponent):\(i + 1) starts git without a listed guard near it: \(line.trimmingCharacters(in: .whitespaces))")
            }
        }
        #expect(found >= 3, "the scan found no git invocations: the pattern is stale")
    }
}
