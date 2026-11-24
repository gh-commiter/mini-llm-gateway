import Foundation
import Synchronization
import Testing
@testable import RouterCore

/// #150: the accept loop of a test's own loopback server. A failed accept(2) is not the end of the server: a client that aborted a queued
/// connection (ECONNABORTED), a signal (EINTR) or a moment without descriptors (EMFILE) used to end the loop for good, the listening socket stayed
/// open, the connections piled up in its backlog and nothing was answered. Only `stop()` ends the loop; any other ending is recorded as a test issue
/// with the errno.
final class TestAcceptLoop: @unchecked Sendable {
    enum Verdict: Equatable { case again, pauseAndAgain, listenerClosed, unexpected }

    /// What a failed accept with this errno means (a pure function: the unit test checks the table).
    static func verdict(_ e: Int32) -> Verdict {
        switch e {
        case EINTR, ECONNABORTED, EAGAIN, EPROTO: return .again  // EWOULDBLOCK is EAGAIN on Darwin
        case EMFILE, ENFILE, ENOBUFS, ENOMEM: return .pauseAndAgain
        case EBADF, EINVAL: return .listenerClosed
        default: return .unexpected
        }
    }

    static func name(_ e: Int32) -> String {
        let known: [Int32: String] = [EINTR: "EINTR", ECONNABORTED: "ECONNABORTED", EAGAIN: "EAGAIN", EPROTO: "EPROTO", EMFILE: "EMFILE", ENFILE: "ENFILE",
                                      ENOBUFS: "ENOBUFS", ENOMEM: "ENOMEM", EBADF: "EBADF", EINVAL: "EINVAL"]
        return (known[e] ?? "errno \(e)") + " (" + String(cString: strerror(e)) + ")"
    }

    private struct State { var stopped = false; var ended: Int32?; var logged = Set<Int32>(); var wakeOpen = false }
    private let state = Mutex(State())
    private let finished = DispatchGroup()
    private var wake: [Int32] = [-1, -1]
    let label: String
    init(_ label: String) { self.label = label; _ = Self.raiseFileLimit }

    /// The descriptor limit of a test process can be as low as 256 (launchd's default for what xcodebuild starts) while dozens of suites run in parallel, each with sockets
    /// and pipes: a transient EMFILE then makes a client's connect fail ("no answer") in a test that is fine. Raised once, and printed, so a failure log shows the limit.
    static let raiseFileLimit: Void = {
        var l = rlimit()
        guard getrlimit(RLIMIT_NOFILE, &l) == 0 else { return }
        let before = l.rlim_cur
        l.rlim_cur = min(l.rlim_max, 10_240)  // (macOS refuses more than OPEN_MAX for the soft limit)
        if l.rlim_cur > before { _ = setrlimit(RLIMIT_NOFILE, &l) }
        print("[#150] descriptor limit of the test process: soft \(before)\(l.rlim_cur > before ? " -> \(l.rlim_cur)" : "") (hard \(l.rlim_max))")
    }()

    /// The errno that ended the loop although the test did not stop it (nil: running, or stopped by the test).
    var endedErrno: Int32? { state.withLock { $0.ended } }

    /// Ends the loop and returns when the listening socket is closed (at most 2 s); calling it again is harmless. The LOOP closes the listener, never the test: a
    /// closed descriptor number is handed to the next socket of any parallel test at once, and a loop that still called accept(2) on it, or a second close, would
    /// take that other test's connections or socket.
    @discardableResult
    func stop() -> Bool {
        // The write and the loop's close of the pipe exclude each other (the same lock, and the loop clears wakeOpen before it closes): a second stop(), or a stop() after the loop ended on its
        // own, must not write to a number that is closed, or already handed to the next socket of another test (found 2026-11-21 with a close() interposer: stop() twice wrote EBADF/into strangers).
        var wrote = "no wake-up pipe"
        state.withLock { s in
            s.stopped = true
            guard s.wakeOpen else { wrote = wake[1] >= 0 ? "wake-up pipe already closed" : wrote; return }
            var b: UInt8 = 1; wrote = write(wake[1], &b, 1) == 1 ? "wake-up byte written" : "wake-up write failed with \(Self.name(errno))"
        }
        let closed = finished.wait(timeout: .now() + 2) == .success
        stopNote.withLock { $0 = "stop: \(wrote), loop \(closed ? "ended" : "STILL RUNNING after 2 s")" + (state.withLock { $0.ended }.map { " (ended on \(Self.name($0)))" } ?? "") }
        return closed
    }

    /// What the last stop() saw (for a failure message): whether the wake-up byte went out and whether the loop ended within 2 s.
    var stopDiagnostics: String { stopNote.withLock { $0 } }
    private let stopNote = Mutex("stop() was not called")

    /// Runs the loop on a thread of its own; it takes over `listener` (closes it when `stop()` is called). `handle` gets each accepted socket (and must not block:
    /// start a thread for it).
    func start(listener: Int32, handle: @escaping (Int32) -> Void) {
        guard pipe(&wake) == 0 else { Issue.record("\(label): no wake-up pipe: \(Self.name(errno))"); return }
        let (wakeRead, wakeWrite) = (wake[0], wake[1])
        _ = fcntl(listener, F_SETFL, fcntl(listener, F_GETFL) | O_NONBLOCK)
        finished.enter()
        state.withLock { $0.wakeOpen = true }
        Thread { [self] in
            defer { state.withLock { $0.wakeOpen = false }; close(listener); close(wakeRead); close(wakeWrite); finished.leave() }
            var fds = [pollfd(fd: listener, events: Int16(POLLIN), revents: 0), pollfd(fd: wakeRead, events: Int16(POLLIN), revents: 0)]
            while !state.withLock({ $0.stopped }) {
                if poll(&fds, 2, -1) < 0 { if errno == EINTR { continue } else { end(errno); return } }
                if fds[1].revents != 0 { return }  // stop()
                if fds[0].revents & Int16(POLLNVAL | POLLERR) != 0 { end(EBADF); return }
                if fds[0].revents & Int16(POLLIN) == 0 { continue }
                let c = accept(listener, nil, nil)
                if c >= 0 {  // non-blocking, as the listener (#177): the handlers use Sock.read / Sock.write / sendAll
                    handle(c); continue
                }
                let e = errno
                switch Self.verdict(e) {
                case .again, .pauseAndAgain:
                    if state.withLock({ $0.logged.insert(e).inserted }) { print("[#150] \(label): accept failed with \(Self.name(e)); the loop goes on") }
                    usleep(Self.verdict(e) == .again ? 1_000 : 50_000)
                case .listenerClosed, .unexpected:
                    end(e); return
                }
            }
        }.start()
    }

    private func end(_ e: Int32) {
        state.withLock { $0.ended = e }
        Issue.record("\(label): the accept loop ended on \(Self.name(e)) although the test did not stop the server")
    }
}

/// #177: all of `d` through Sock.write (a test server's socket may be "draining": a blocking write that waits fails with EBADF there).
@discardableResult
func sendAll(_ c: Int32, _ d: Data) -> Bool {
    d.withUnsafeBytes { b in
        var off = 0
        while off < b.count { let n = Sock.write(c, b.baseAddress! + off, b.count - off); if n <= 0 { return false }; off += n }
        return true
    }
}

@Suite struct TestAcceptLoopTests {
    @Test func transientAcceptErrorsGoOnResourceOnesPauseAndOnlyAClosedListenerEnds() {
        for e in [EINTR, ECONNABORTED, EAGAIN, EWOULDBLOCK, EPROTO] { #expect(TestAcceptLoop.verdict(e) == .again, "\(TestAcceptLoop.name(e))") }
        for e in [EMFILE, ENFILE, ENOBUFS, ENOMEM] { #expect(TestAcceptLoop.verdict(e) == .pauseAndAgain, "\(TestAcceptLoop.name(e))") }
        for e in [EBADF, EINVAL] { #expect(TestAcceptLoop.verdict(e) == .listenerClosed) }
        for e in [EIO, ENOTSOCK, EOPNOTSUPP, EFAULT] { #expect(TestAcceptLoop.verdict(e) == .unexpected, "\(TestAcceptLoop.name(e))") }
    }

    /// For a failure message: the process (pid, command) that listens on the port now, from lsof ("" when none or lsof fails).
    static func listenerOf(_ port: UInt16) -> String {
        let p = Process(), out = Pipe()
        p.executableURL = URL(fileURLWithPath: "/usr/sbin/lsof")
        p.arguments = ["-nP", "-iTCP:\(port)", "-sTCP:LISTEN", "-Fpc"]
        p.standardOutput = out; p.standardError = FileHandle.nullDevice
        guard (try? p.run()) != nil else { return "" }
        let data = out.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        return String(decoding: data, as: UTF8.self).split(separator: "\n").joined(separator: " ")
    }

    @Test(.blocking) func aLoopServesConnectionsAndEndsQuietlyWhenTheTestStopsIt() {
        let l = socket(AF_INET, SOCK_STREAM, 0)
        var a = sockaddr_in(), n = socklen_t(MemoryLayout<sockaddr_in>.size)
        a.sin_family = sa_family_t(AF_INET); a.sin_len = UInt8(n); a.sin_addr.s_addr = inet_addr("127.0.0.1")
        _ = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(l, $0, n) } }
        listen(l, 4)
        _ = withUnsafeMutablePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(l, $0, &n) } }
        let served = Mutex(0), loop = TestAcceptLoop("unit")
        loop.start(listener: l) { c in close(c); served.withLock { $0 += 1 } }
        for _ in 0..<2 {
            let s = socket(AF_INET, SOCK_STREAM, 0)
            _ = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(s, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
            close(s)
        }
        let end = Date().addingTimeInterval(5)
        while served.withLock({ $0 }) < 2 && Date() < end { usleep(5_000) }
        #expect(served.withLock { $0 } == 2)
        let t0 = ContinuousClock.now
        let firstStop = loop.stop(), firstNote = loop.stopDiagnostics
        let secondStop = loop.stop()  // twice is harmless
        let took = Stages.s(ContinuousClock.now - t0)
        #expect(loop.endedErrno == nil)
        // (not close(l) == EBADF: a parallel test may have been given that descriptor number already) nothing listens on the port any more
        let s = socket(AF_INET, SOCK_STREAM, 0)
        defer { close(s) }
        let r = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(s, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
        let e = errno  // read at once: nothing in between may overwrite it
        var mine = sockaddr_in(), mineLen = socklen_t(MemoryLayout<sockaddr_in>.size)
        _ = withUnsafeMutablePointer(to: &mine) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(s, $0, &mineLen) } }
        let source = UInt16(bigEndian: mine.sin_port), target = UInt16(bigEndian: a.sin_port)  // source == target: TCP connected the socket to ITSELF (no listener there)
        // #150 (failed 2 of 14 full runs on 2026-11-20): when the connect did not fail, say WHO answered: our loop (it is still running: the stop was too slow or lost) or
        // another test's listener that was given this port after ours closed (our loop ended, and the connection was counted by nobody here)
        let owner = r == 0 ? Self.listenerOf(target) : ""  // asked at once: the other listener may be gone a moment later
        let answeredByUs = r == 0 ? { usleep(300_000); return served.withLock { $0 } > 2 }() : false
        // A refused connect is the proof that nothing listens. macOS reports it as ECONNREFUSED or, now and then on loopback, as ECONNRESET
        // (gate of 0.9.9: "-1 / errno 54", stops true/true, nobody listening on the port, our loop did not count a connection): both mean no listener.
        // A connect that SUCCEEDS proves nothing about our loop in a parallel run: the port number, free again, may already belong to another
        // test's listener (seen three times on 2026-11-20: "connect gave 0", our loop counted nothing, nobody listening a moment later). What this
        // test claims is that OUR loop ended and serves no more: then such a connection is not counted by our handler.
        #expect((r == -1 && (e == ECONNREFUSED || e == ECONNRESET)) || (r == 0 && !answeredByUs), "the loop did not close the listener: connect gave \(r) / \(TestAcceptLoop.name(e)); stops \(firstStop)/\(secondStop) in \(took) s; \(firstNote); our loop counted the new connection: \(answeredByUs); source port \(source), target port \(target)\(source == target ? " (a TCP self-connect: nothing listens there)" : ""); listening on the target port at once after: \(owner.isEmpty ? "nobody found" : owner); \(PoolLagGuard.text)")
    }
}
