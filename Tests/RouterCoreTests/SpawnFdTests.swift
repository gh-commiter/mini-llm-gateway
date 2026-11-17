import Foundation
import Testing
@testable import RouterCore

// sec-fix F4: does a child that an agent is started with inherit a descriptor that was created without CLOEXEC (the moment
// after `socketpair`/`pipe` in CoreProcess.spawn, before its fcntl)? The marker is such a descriptor; the child is /bin/test
// (a separate process, `test -e /dev/fd/N` is true only when N is open in it).
// Result (macOS 27.2): neither Foundation `Process` (ClaudeCLI, Codex, Grok) nor `Native.spawnGroup` (minigw run, Copilot) passes
// it on: both start the child with POSIX_SPAWN_CLOEXEC_DEFAULT, so only the descriptors they dup2 survive. The test is the proof;
// no spawn lock or earlier FD_CLOEXEC is needed. The control shows that the probe says "open" when a child does inherit.

private func marker() -> (fds: [Int32], close: () -> Void) {
    var p: [Int32] = [0, 0]
    precondition(pipe(&p) == 0)  // no FD_CLOEXEC: as Darwin creates it
    return (p, { close(p[0]); close(p[1]) })
}

private func isOpen(in fd: Int32, _ run: (_ exe: String, _ args: [String]) throws -> Int32) throws -> Bool {
    try run("/bin/test", ["-e", "/dev/fd/\(fd)"]) == 0
}

@Suite(.serialized) struct SpawnFdTests {
    @Test func theControlSeesAnInheritedDescriptor() throws {
        // a fork+exec that closes nothing (the child inherits it): proves the probe can say "open"
        let m = marker(); defer { m.close() }
        #expect(try isOpen(in: m.fds[0]) { exe, args in
            var pid: pid_t = 0
            let argv = ([exe] + args).map { strdup($0) } + [nil]
            defer { argv.forEach { free($0) } }
            guard posix_spawn(&pid, exe, nil, nil, argv, environ) == 0 else { return -1 }  // default flags: inherits
            var st: Int32 = 0; waitpid(pid, &st, 0)
            return (st >> 8) & 0xff
        })
    }

    @Test func foundationProcessDoesNotInheritIt() throws {
        let m = marker(); defer { m.close() }
        for fd in m.fds {
            let open = try isOpen(in: fd) { exe, args in
                let p = Process()
                p.executableURL = URL(fileURLWithPath: exe); p.arguments = args
                p.standardOutput = Pipe(); p.standardError = Pipe(); p.standardInput = Pipe()  // as the agents are started
                try p.run(); p.waitUntilExit()
                return p.terminationStatus
            }
            #expect(!open, "Foundation Process: fd \(fd) inherited")
        }
    }

    /// The helper's connections are socketpairs (the first and the upstream lane), made without CLOEXEC for a moment as the pipes are:
    /// a child that another thread starts in that moment (an agent CLI, git, a probe) must not get them either. Every spawn site of the app
    /// is Foundation Process (CLOEXEC_DEFAULT, tested below) or posix_spawn with POSIX_SPAWN_CLOEXEC_DEFAULT (Native.spawnGroup,
    /// Worktree, CoreProcess); the only ones that are not are in the `minigw` command line tool, a different process.
    @Test func aSocketpairWithoutCloexecIsNotInheritedEither() throws {
        var sv: [Int32] = [0, 0]
        precondition(socketpair(AF_UNIX, SOCK_STREAM, 0, &sv) == 0)
        defer { close(sv[0]); close(sv[1]) }
        for fd in sv {
            let viaProcess = try isOpen(in: fd) { exe, args in
                let p = Process()
                p.executableURL = URL(fileURLWithPath: exe); p.arguments = args
                p.standardOutput = Pipe(); p.standardError = Pipe(); p.standardInput = Pipe()
                try p.run(); p.waitUntilExit()
                return p.terminationStatus
            }
            #expect(!viaProcess, "Foundation Process: socketpair fd \(fd) inherited")
            let child = try #require(Native.spawnGroup("/bin/test", ["-e", "/dev/fd/\(fd)"], dir: URL(fileURLWithPath: "/tmp"), env: [:], input: false))
            var st: Int32 = 0; waitpid(child.pid, &st, 0)
            #expect((st >> 8) & 0xff != 0, "spawnGroup: socketpair fd \(fd) inherited")
        }
    }

    /// What a Foundation `Process` child really has open: with a socketpair that has NO close-on-exec open in this process (the state of the
    /// helper's connections for a moment after `socketpair`), the child is `/bin/ls /dev/fd`, which lists the descriptors it has. The control
    /// starts the same program with plain `posix_spawn` (no CLOEXEC_DEFAULT), which does list them, so the probe can see an inheritance.
    /// Proves, on the macOS this runs on: a Process child has only 0, 1, 2 and ls's own directory handle; it does not get descriptors above 2.
    @Test func aProcessChildListsOnlyItsOwnDescriptors() throws {
        var sv: [Int32] = [0, 0]
        precondition(socketpair(AF_UNIX, SOCK_STREAM, 0, &sv) == 0)
        let high = sv.map { fcntl($0, F_DUPFD, 100) }  // above any descriptor the child could get by chance; F_DUPFD leaves close-on-exec off
        for fd in sv { close(fd) }
        defer { for fd in high { close(fd) } }
        for fd in high { #expect(fd >= 100 && fcntl(fd, F_GETFD) & FD_CLOEXEC == 0, "the descriptor must be inheritable for the test to mean anything") }
        func listed(_ out: String) -> Set<Int32> { Set(out.split(whereSeparator: \.isWhitespace).compactMap { Int32($0) }) }

        let p = Process()
        let outPipe = Pipe()
        p.executableURL = URL(fileURLWithPath: "/bin/ls"); p.arguments = ["/dev/fd"]
        p.standardOutput = outPipe; p.standardError = Pipe(); p.standardInput = Pipe()  // as the agents are started
        try p.run()
        let out = String(decoding: outPipe.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
        p.waitUntilExit()
        let child = listed(out)
        #expect(child.isDisjoint(with: Set(high)) && child.allSatisfy { $0 < 100 } && child.isSubset(of: [0, 1, 2, 3, 4]), "a Process child has descriptors \(child.sorted())")

        // control: a child started with default posix_spawn flags does inherit them
        var o: [Int32] = [0, 0]
        precondition(pipe(&o) == 0)
        var fa: posix_spawn_file_actions_t?
        posix_spawn_file_actions_init(&fa); defer { posix_spawn_file_actions_destroy(&fa) }
        posix_spawn_file_actions_adddup2(&fa, o[1], 1)
        let args: [String] = ["/bin/ls", "/dev/fd"]
        let argv: [UnsafeMutablePointer<CChar>?] = args.map { strdup($0) } + [nil]
        defer { argv.forEach { free($0) } }
        var pid: pid_t = 0
        precondition(posix_spawn(&pid, "/bin/ls", &fa, nil, argv, environ) == 0)
        close(o[1])
        var data = Data(), buf = [UInt8](repeating: 0, count: 4096)
        while case let k = read(o[0], &buf, buf.count), k > 0 { data += buf[0..<k] }
        close(o[0])
        var st: Int32 = 0; waitpid(pid, &st, 0)
        #expect(!listed(String(decoding: data, as: UTF8.self)).isDisjoint(with: Set(high)), "the control child did not see the inheritable descriptors: the probe proves nothing")
    }

    @Test func spawnGroupDoesNotInheritIt() throws {
        let m = marker(); defer { m.close() }
        for fd in m.fds {
            let r = Native.spawnGroup("/bin/test", ["-e", "/dev/fd/\(fd)"], dir: URL(fileURLWithPath: "/tmp"), env: [:], input: true)
            let child = try #require(r)
            var st: Int32 = 0; waitpid(child.pid, &st, 0)
            #expect((st >> 8) & 0xff != 0, "spawnGroup: fd \(fd) inherited")
        }
    }

    /// The quota probe (ClaudeUsageProbe.run) starts claude with spawnGroup, stdin /dev/null (input: false), its own environment and an empty folder:
    /// the same reasoning holds (POSIX_SPAWN_CLOEXEC_DEFAULT); the probe's whole run is also tested in QuotaHardeningTests.
    @Test func theQuotaProbeSpawnDoesNotInheritIt() throws {
        let m = marker(); defer { m.close() }
        for fd in m.fds {
            let r = Native.spawnGroup("/bin/test", ["-e", "/dev/fd/\(fd)"], dir: URL(fileURLWithPath: "/tmp"), env: ClaudeUsageProbe.env([:], tmp: "/tmp"), input: false)
            let child = try #require(r)
            #expect(child.input == nil)  // no stdin pipe: /dev/null
            var st: Int32 = 0; waitpid(child.pid, &st, 0)
            #expect((st >> 8) & 0xff != 0, "the probe's spawn: fd \(fd) inherited")
        }
    }
}
