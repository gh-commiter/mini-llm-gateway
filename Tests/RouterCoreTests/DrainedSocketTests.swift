import Foundation
import Testing
@testable import RouterCore

// #177: macOS marks a socket "draining" for good when ANY descriptor of it is closed while someone else holds that descriptor (in the full
// suite: a spawned child's short-lived copy of the helper's socket, closed at exec while PeerProcess.pids or lsof inspected it). From then on
// every blocking read or write that has to wait fails with EBADF, although our own descriptor is open and refers to the same socket.
// Here the drain is made on purpose: a dup of our end is closed while a thread waits in read on it.

/// Our end `sv[0]` of a fresh socketpair, drained as above; the caller closes both.
private func drainedPair() throws -> [Int32] {
    var sv: [Int32] = [0, 0]
    try #require(socketpair(AF_UNIX, SOCK_STREAM, 0, &sv) == 0)
    let d = dup(sv[0]), done = DispatchSemaphore(value: 0)
    Thread { var c: UInt8 = 0; _ = read(d, &c, 1); done.signal() }.start()
    usleep(100_000)  // the thread waits in read(d)
    close(d)
    try #require(done.wait(timeout: .now() + 5) == .success)
    return sv
}

private func timeouts(_ fd: Int32, _ seconds: Int) {
    var tv = timeval(tv_sec: seconds, tv_usec: 0)
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
}

@Suite struct DrainedSocketTests {
    /// The control: the drain is real (a plain blocking read on our open descriptor fails with EBADF) and our descriptor is still open.
    @Test(.blocking) func theDrainMakesABlockingReadFailWithEBADFOnAnOpenDescriptor() throws {
        let sv = try drainedPair(); defer { close(sv[0]); close(sv[1]) }
        var tv = timeval(tv_sec: 0, tv_usec: 100_000)
        setsockopt(sv[0], SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
        var c: UInt8 = 0
        let n = read(sv[0], &c, 1), e = errno
        #expect(n == -1 && e == EBADF, "read \(n) errno \(e)")
        #expect(fcntl(sv[0], F_GETFD) >= 0)
    }

    /// The helper's framing reads a reply that arrives while it waits, on a drained socket.
    @Test(.blocking) func theHelperFramingReadsFromADrainedSocket() throws {
        let sv = try drainedPair(); defer { close(sv[0]); close(sv[1]) }
        timeouts(sv[0], 5)
        let peer = sv[1]
        Thread { usleep(100_000); _ = "frame".withCString { write(peer, $0, 5) } }.start()
        #expect(String(decoding: try readExactly(sv[0], 5), as: UTF8.self) == "frame")
    }

    /// The helper's framing writes more than the socket buffer holds (it must wait for room) on a drained socket.
    @Test(.blocking) func theHelperFramingWritesToADrainedSocket() throws {
        let sv = try drainedPair(); defer { close(sv[0]); close(sv[1]) }
        timeouts(sv[0], 5)
        let peer = sv[1], total = 1 << 20, got = DispatchSemaphore(value: 0)
        nonisolated(unsafe) var read = 0
        Thread {
            var buf = [UInt8](repeating: 0, count: 65536)
            while read < total { let n = Darwin.read(peer, &buf, buf.count); if n <= 0 { break }; read += n }
            got.signal()
        }.start()
        try writeAll(sv[0], Data(count: total))
        #expect(got.wait(timeout: .now() + 10) == .success)
        #expect(read == total)
    }

    /// The timeout still holds: nothing arrives → "helper timed out" after about the socket's receive timeout.
    @Test(.blocking) func aSilentPeerStillTimesOut() throws {
        var sv: [Int32] = [0, 0]
        try #require(socketpair(AF_UNIX, SOCK_STREAM, 0, &sv) == 0)
        defer { close(sv[0]); close(sv[1]) }
        timeouts(sv[0], 1)
        let start = Date()
        #expect(throws: CoreError.unavailable("helper timed out")) { _ = try readExactly(sv[0], 4) }
        #expect(Date().timeIntervalSince(start) >= 0.9)
    }
}
