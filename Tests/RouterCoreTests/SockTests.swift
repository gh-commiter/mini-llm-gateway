import Foundation
import Testing
@testable import RouterCore

private func loopback(_ port: UInt16) -> sockaddr_in {
    sockaddr_in(sin_len: UInt8(MemoryLayout<sockaddr_in>.size), sin_family: sa_family_t(AF_INET), sin_port: port.bigEndian,
                sin_addr: in_addr(s_addr: inet_addr("127.0.0.1")), sin_zero: (0, 0, 0, 0, 0, 0, 0, 0))
}

/// A connect that a signal interrupted: the kernel goes on connecting (`started` does the real call, non-blocking), the caller sees EINTR.
private func interrupted(_ started: @escaping (Int32, UnsafePointer<sockaddr>, socklen_t) -> Void) -> (Int32, UnsafePointer<sockaddr>, socklen_t) -> Int32 {
    { fd, a, n in started(fd, a, n); errno = EINTR; return -1 }
}

@Suite struct SockTests {
    @Test func retryRepeatsACallThatWasInterrupted() {
        var calls = 0
        let r = Sock.retry { () -> Int in calls += 1; if calls < 3 { errno = EINTR; return -1 }; return 7 }
        #expect(r == 7 && calls == 3)
    }

    @Test func retryReturnsAnotherErrorAtOnce() {
        var calls = 0
        let r = Sock.retry { () -> Int in calls += 1; errno = EPIPE; return -1 }
        #expect(r == -1 && calls == 1 && errno == EPIPE)
    }

    @Test func interruptedConnectThatCompletesIsASuccess() throws {
        let l = socket(AF_INET, SOCK_STREAM, 0), c = socket(AF_INET, SOCK_STREAM, 0)
        defer { close(l); close(c) }
        var a = loopback(0), n = socklen_t(MemoryLayout<sockaddr_in>.size)
        _ = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(l, $0, n) } }
        listen(l, 4)
        _ = withUnsafeMutablePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(l, $0, &n) } }
        let r = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { p in
            Sock.connect(c, p, n, call: interrupted { fd, a, n in
                _ = fcntl(fd, F_SETFL, fcntl(fd, F_GETFL) | O_NONBLOCK); _ = Darwin.connect(fd, a, n)
            })
        } }
        #expect(r == 0)
    }

    @Test func interruptedConnectThatIsRefusedReportsTheError() {
        let c = socket(AF_INET, SOCK_STREAM, 0)
        defer { close(c) }
        var a = loopback(freeTCPPort())
        let r = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { p in
            Sock.connect(c, p, socklen_t(MemoryLayout<sockaddr_in>.size), call: interrupted { fd, a, n in
                _ = fcntl(fd, F_SETFL, fcntl(fd, F_GETFL) | O_NONBLOCK); _ = Darwin.connect(fd, a, n)
            })
        } }
        #expect(r == -1 && errno == ECONNREFUSED)
    }
}
