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

    @Test func spawnGroupDoesNotInheritIt() throws {
        let m = marker(); defer { m.close() }
        for fd in m.fds {
            let r = Native.spawnGroup("/bin/test", ["-e", "/dev/fd/\(fd)"], dir: URL(fileURLWithPath: "/tmp"), env: [:], input: true)
            let child = try #require(r)
            var st: Int32 = 0; waitpid(child.pid, &st, 0)
            #expect((st >> 8) & 0xff != 0, "spawnGroup: fd \(fd) inherited")
        }
    }
}
