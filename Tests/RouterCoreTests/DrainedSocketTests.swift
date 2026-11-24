import Foundation
import Synchronization
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

/// The same drain for a loopback listener: a dup is closed while a thread waits in accept on it. Returns the listener and its port.
private func drainedListener() throws -> (fd: Int32, port: UInt16) {
    let l = socket(AF_INET, SOCK_STREAM, 0)
    var a = sockaddr_in(), n = socklen_t(MemoryLayout<sockaddr_in>.size)
    a.sin_family = sa_family_t(AF_INET); a.sin_len = UInt8(n); a.sin_addr.s_addr = inet_addr("127.0.0.1")
    try #require(withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(l, $0, n) } } == 0 && listen(l, 4) == 0)
    _ = withUnsafeMutablePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(l, $0, &n) } }
    let d = dup(l), done = DispatchSemaphore(value: 0)
    Thread { _ = accept(d, nil, nil); done.signal() }.start()
    usleep(100_000)  // the thread waits in accept(d)
    close(d)
    try #require(done.wait(timeout: .now() + 5) == .success)
    return (l, UInt16(bigEndian: a.sin_port))
}

/// A blocking client connected to 127.0.0.1:port (the caller closes it).
private func client(_ port: UInt16) throws -> Int32 {
    let s = socket(AF_INET, SOCK_STREAM, 0)
    var a = sockaddr_in(), n = socklen_t(MemoryLayout<sockaddr_in>.size)
    a.sin_family = sa_family_t(AF_INET); a.sin_len = UInt8(n); a.sin_addr.s_addr = inet_addr("127.0.0.1"); a.sin_port = port.bigEndian
    try #require(withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(s, $0, n) } } == 0)
    return s
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

    /// Sock.read and Sock.write (AdminClient, minigw -p, the test servers) on a drained socket: the reply arrives while it waits, and a write
    /// larger than the socket buffer has to wait for room.
    @Test(.blocking) func sockReadAndWriteWorkOnADrainedSocket() throws {
        let sv = try drainedPair(); defer { close(sv[0]); close(sv[1]) }
        timeouts(sv[0], 5)
        let peer = sv[1], total = 1 << 20, got = DispatchSemaphore(value: 0)
        nonisolated(unsafe) var read = 0
        Thread {
            usleep(100_000); _ = "ping".withCString { write(peer, $0, 4) }
            var buf = [UInt8](repeating: 0, count: 65536)
            while read < total { let n = Darwin.read(peer, &buf, buf.count); if n <= 0 { break }; read += n }
            got.signal()
        }.start()
        var b = [UInt8](repeating: 0, count: 4)
        #expect(Sock.read(sv[0], &b, 4) == 4 && b == Array("ping".utf8))
        let out = Data(count: total)
        var off = 0
        while off < total {
            let n = out.withUnsafeBytes { Sock.write(sv[0], $0.baseAddress! + off, total - off) }
            guard n > 0 else { Issue.record("write: errno \(errno)"); break }
            off += n
        }
        #expect(got.wait(timeout: .now() + 10) == .success)
        #expect(read == total)
    }

    /// Sock.read keeps the socket's receive timeout: nothing arrives → -1 with EAGAIN after about that time, as a blocking read.
    @Test(.blocking) func sockReadTimesOutAsABlockingRead() throws {
        var sv: [Int32] = [0, 0]
        try #require(socketpair(AF_UNIX, SOCK_STREAM, 0, &sv) == 0)
        defer { close(sv[0]); close(sv[1]) }
        timeouts(sv[0], 1)
        let start = Date()
        var b: UInt8 = 0
        let n = Sock.read(sv[0], &b, 1), e = errno
        #expect(n == -1 && e == EAGAIN, "read \(n) errno \(e)")
        #expect(Date().timeIntervalSince(start) >= 0.9)
    }

    /// The control for listeners: on a drained listener a blocking accept that waits fails (ECONNABORTED when the connection comes), and the
    /// connection taken without waiting is drained too: a blocking read on it fails with EBADF.
    @Test(.blocking) func aDrainedListenerFailsABlockingAcceptAndMarksTheConnection() throws {
        let (l, port) = try drainedListener(); defer { close(l) }
        let first = Mutex<Int32>(-1)
        Thread { usleep(200_000); first.withLock { $0 = (try? client(port)) ?? -1 } }.start()
        let r = accept(l, nil, nil), e = errno
        defer { if r >= 0 { close(r) }; first.withLock { if $0 >= 0 { close($0) } } }
        #expect(r == -1 && (e == ECONNABORTED || e == EBADF), "accept \(r) errno \(e)")
        let s = try client(port); defer { close(s) }
        var p = pollfd(fd: l, events: Int16(POLLIN), revents: 0)
        try #require(poll(&p, 1, 5000) == 1)
        Sock.nonBlocking(l)
        let c = accept(l, nil, nil)
        try #require(c >= 0, "accept errno \(errno)")
        defer { close(c) }
        _ = fcntl(c, F_SETFL, fcntl(c, F_GETFL) & ~O_NONBLOCK)
        var tv = timeval(tv_sec: 0, tv_usec: 200_000)
        setsockopt(c, SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
        var b: UInt8 = 0
        let n = read(c, &b, 1), e2 = errno
        #expect(n == -1 && e2 == EBADF, "read \(n) errno \(e2)")
    }

    /// Sock.accept on a drained listener waits for the connection, and the accepted (also drained) socket reads and writes through Sock.
    @Test(.blocking) func sockAcceptServesADrainedListener() throws {
        let (l, port) = try drainedListener(); defer { close(l) }
        let peer = Mutex<Int32>(-1)
        Thread { usleep(200_000); peer.withLock { $0 = (try? client(port)) ?? -1 } }.start()
        let c = Sock.accept(l)
        try #require(c >= 0, "accept errno \(errno)")
        defer { close(c); peer.withLock { if $0 >= 0 { close($0) } } }
        timeouts(c, 5)
        let s = peer.withLock { $0 }
        try #require(s >= 0)
        Thread { usleep(100_000); _ = "ping".withCString { write(s, $0, 4) } }.start()
        var b = [UInt8](repeating: 0, count: 4)
        #expect(Sock.read(c, &b, 4) == 4 && b == Array("ping".utf8))
        #expect("pong".withCString { Sock.write(c, $0, 4) } == 4)
        var r = [UInt8](repeating: 0, count: 4)
        #expect(read(s, &r, 4) == 4 && r == Array("pong".utf8))
    }
}
