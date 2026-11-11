import Foundation
import Testing
@testable import RouterCore

/// #79: isti primeri kot Go (client/testdata/claude-code-cases.json, client/claudecode_test.go).
struct ClaudeCodeCases: Decodable {
    struct Proc: Decodable { let exe: String; let args: [String]?; let claude: Bool }
    struct Link: Decodable { let exe: String?; let args: [String]?; let unreadable: Bool?; let loop: Bool?; let gone: Bool? }
    struct Chain: Decodable { let name: String; let terminal: Bool; let claude: Bool; let chain: [Link] }
    struct Policy: Decodable { let name: String; let marker: Bool; let chain: Int; let override: Bool; let checked: Bool }
    struct Args: Decodable { let args: [String]; let refused: Bool }
    struct Continue: Decodable { let args: [String]; let last: String; let out: [String]; let dropped: Bool }
    let procs: [Proc], chains: [Chain], policy: [Policy], args: [Args], `continue`: [Continue]

    static let root = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
    static func load() throws -> ClaudeCodeCases {
        try JSONDecoder().decode(ClaudeCodeCases.self, from: Data(contentsOf: root.appendingPathComponent("client/testdata/claude-code-cases.json")))
    }

    /// Lažna veriga prednikov (PID 1000 je starš minigw), kot Go fakeChain.
    func fake(_ i: Int) -> (pid_t) -> Profiles.ProcInfo? {
        let ch = chains[i].chain
        return { pid in
            let k = Int(pid) - 1000, e = ch[k]
            if e.unreadable == true { return nil }
            if e.gone == true { return (0, "", []) }
            let ppid: pid_t = e.loop == true ? 1000 : k == ch.count - 1 ? 0 : pid + 1
            return (ppid, e.exe ?? "", e.args ?? [])
        }
    }
}

@Test func claudeCodeDetection() throws {
    let c = try ClaudeCodeCases.load()
    for p in c.procs { #expect(Profiles.isClaudeProc(exe: p.exe, args: p.args ?? []) == p.claude, "\(p.exe) \(p.args ?? [])") }
    for (i, ch) in c.chains.enumerated() { #expect(Profiles.claudeCaller(ppid: 1000, info: c.fake(i), terminal: ch.terminal) == ch.claude, "\(ch.name)") }
    let me = try #require(Profiles.procInfo(getpid()))
    #expect(me.ppid == getppid() && !Profiles.isClaudeProc(exe: me.exe, args: me.args), "\(me)")
}

/// Označen klicatelj (CLAUDECODE ali claude med predniki) brez MINIGW_ALLOW_UNSAFE_FLAGS=1 → seznam claudeSafe (kot Go).
@Test func claudeCodeArgsPolicy() throws {
    let c = try ClaudeCodeCases.load()
    for p in c.policy {
        var env: [String: String] = [:]
        if p.marker || Profiles.claudeCaller(ppid: 1000, info: c.fake(p.chain), terminal: c.chains[p.chain].terminal) { env["CLAUDECODE"] = "1" }
        if p.override { env["MINIGW_ALLOW_UNSAFE_FLAGS"] = "1" }
        #expect(Profiles.checkClaudeArgs(env) == p.checked, "\(p.name)")
        for a in c.args { #expect((Profiles.unsafeClaudeFlag(a.args, env: env) != nil) == (p.checked && a.refused), "\(p.name) \(a.args)") }
    }
}

@Test func continueSessionCases() throws {
    let c = try ClaudeCodeCases.load()
    for k in c.continue {
        let r = Profiles.continueSession(k.args, last: k.last)
        #expect(r.args == k.out && r.dropped == k.dropped, "\(k.args) \(k.last)")
    }
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: dir) }
    let f = dir.appendingPathComponent("minigw/last-session.json"), id = "0c3b5a52-4f0e-4c4b-9d7e-2b1f1a6e9c01"
    #expect(Profiles.lastSession(file: f, cwd: "/a") == "")
    Profiles.saveLastSession(file: f, cwd: "/a", sid: id)
    Profiles.saveLastSession(file: f, cwd: "/b", sid: "--dangerously-skip-permissions")
    #expect(Profiles.lastSession(file: f, cwd: "/a") == id && Profiles.lastSession(file: f, cwd: "/b") == "")
    try Data(#"{"/b":"-x"}"#.utf8).write(to: f)
    #expect(Profiles.lastSession(file: f, cwd: "/b") == "")
}

/// »env -u CLAUDECODE minigw … --continue« pod procesom claude (povezava claude → /bin/sh) minigw zavrne, preden zažene
/// claude; MINIGW_ALLOW_UNSAFE_FLAGS=1 ga zažene. MiniGW je iz »swift build« (kot ga poženeta vrata izdaje in CI).
@Test func claudeAncestorRefusesBeforeClaude() throws {
    let minigw = ClaudeCodeCases.root.appendingPathComponent(".build/debug/MiniGW")
    try #require(FileManager.default.isExecutableFile(atPath: minigw.path), "run swift build first")
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString), bin = dir.appendingPathComponent("bin")
    defer { try? FileManager.default.removeItem(at: dir) }
    try FileManager.default.createDirectory(at: bin, withIntermediateDirectories: true)
    let parent = dir.appendingPathComponent("claude"), log = dir.appendingPathComponent("claude.log")
    try FileManager.default.createSymbolicLink(atPath: parent.path, withDestinationPath: "/bin/sh")
    let fake = bin.appendingPathComponent("claude")
    try Data("#!/bin/sh\necho \"$@\" > \"$MINIGW_TEST_CLAUDE_LOG\"\n".utf8).write(to: fake)
    try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: fake.path)
    func run(_ args: [String], extra: [String: String] = [:]) throws -> (String, Bool) {
        try? FileManager.default.removeItem(at: log)
        let p = Process(), out = Pipe()
        p.executableURL = extra["MINIGW_TEST_DIRECT"] == nil ? parent : URL(fileURLWithPath: "/bin/sh")  // brez starša claude
        p.arguments = ["-c", "\"$@\"; r=$?; exit $r", "sh", minigw.path] + args
        var env = ProcessInfo.processInfo.environment.filter { $0.key != "CLAUDECODE" && $0.key != "MINIGW_ALLOW_UNSAFE_FLAGS" }
        env["PATH"] = bin.path + ":" + (env["PATH"] ?? "/usr/bin:/bin")
        env["MINIGW_TEST_CLAUDE_LOG"] = log.path
        p.environment = env.merging(extra) { $1 }
        p.standardOutput = out; p.standardError = out; p.standardInput = FileHandle.nullDevice
        try p.run()
        let text = String(decoding: out.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
        p.waitUntilExit()
        return (text, FileManager.default.fileExists(atPath: log.path))
    }
    for a in [["-h", "--continue"], ["-p", "--continue", "x"], ["-p", "-c", "x"], ["-p", "--resume", "0c3b5a52-4f0e-4c4b-9d7e-2b1f1a6e9c01", "x"]] {
        let (text, spawned) = try run(a)
        #expect(!spawned && text.contains("is not accepted when minigw runs inside Claude Code"), "\(a): \(text)")
    }
    let (text, spawned) = try run(["-h", "--continue"], extra: ["MINIGW_ALLOW_UNSAFE_FLAGS": "1"])
    #expect(spawned, "override: \(text)")
    // kontrola: brez prednika claude (test ne teče v Claude Code, npr. CI) minigw claude zažene
    if !Profiles.claudeCaller(ppid: getpid(), info: Profiles.procInfo, terminal: false) {
        let (text, spawned) = try run(["-h", "--continue"], extra: ["MINIGW_TEST_DIRECT": "1"])
        #expect(spawned, "without a claude parent: \(text)")
    }
}
