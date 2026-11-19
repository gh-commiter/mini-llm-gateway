import Foundation
import Synchronization
import Testing

/// #150: the accept loop of a test's own loopback server. A failed accept(2) is not the end of the server: a client that aborted a queued
/// connection (ECONNABORTED), a signal (EINTR) or a moment without descriptors (EMFILE) used to end the loop for good, the listening socket stayed
/// open, the connections piled up in its backlog and nothing was answered. Only a listener the test closed itself ends the loop; any other
/// ending is recorded as a test issue with the errno.
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

    private struct State { var stopped = false; var ended: Int32?; var logged = Set<Int32>() }
    private let state = Mutex(State())
    let label: String
    init(_ label: String) { self.label = label }

    /// The errno that ended the loop although the test did not stop it (nil: running, or stopped by the test).
    var endedErrno: Int32? { state.withLock { $0.ended } }

    /// Call BEFORE closing the listening socket: the loop then ends quietly on the error that follows.
    func markStopped() { state.withLock { $0.stopped = true } }

    /// Runs the loop on a thread of its own; `handle` gets each accepted socket (and must not block: start a thread for it).
    func start(listener: Int32, handle: @escaping (Int32) -> Void) {
        Thread { [self] in
            while !state.withLock({ $0.stopped }) {
                let c = accept(listener, nil, nil)
                if c >= 0 { handle(c); continue }
                let e = errno
                if state.withLock({ $0.stopped }) { return }
                switch Self.verdict(e) {
                case .again, .pauseAndAgain:
                    if state.withLock({ $0.logged.insert(e).inserted }) { print("[#150] \(label): accept failed with \(Self.name(e)); the loop goes on") }
                    usleep(Self.verdict(e) == .again ? 1_000 : 50_000)
                case .listenerClosed, .unexpected:
                    state.withLock { $0.ended = e }
                    Issue.record("\(label): the accept loop ended on \(Self.name(e)) although the test did not stop the server")
                    return
                }
            }
        }.start()
    }
}

@Suite struct TestAcceptLoopTests {
    @Test func transientAcceptErrorsGoOnResourceOnesPauseAndOnlyAClosedListenerEnds() {
        for e in [EINTR, ECONNABORTED, EAGAIN, EWOULDBLOCK, EPROTO] { #expect(TestAcceptLoop.verdict(e) == .again, "\(TestAcceptLoop.name(e))") }
        for e in [EMFILE, ENFILE, ENOBUFS, ENOMEM] { #expect(TestAcceptLoop.verdict(e) == .pauseAndAgain, "\(TestAcceptLoop.name(e))") }
        for e in [EBADF, EINVAL] { #expect(TestAcceptLoop.verdict(e) == .listenerClosed) }
        for e in [EIO, ENOTSOCK, EOPNOTSUPP, EFAULT] { #expect(TestAcceptLoop.verdict(e) == .unexpected, "\(TestAcceptLoop.name(e))") }
    }

    @Test func aLoopServesConnectionsAndEndsQuietlyWhenTheTestStopsIt() {
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
        loop.markStopped(); close(l)
        usleep(100_000)
        #expect(loop.endedErrno == nil)
    }
}
