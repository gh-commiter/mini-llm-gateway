import Darwin
import Foundation

/// Raw socket calls that a signal may interrupt: EINTR is not an error and not the end of the stream (#109: AdminClient
/// returned 0 "no router here" for a handshake that was only interrupted).
public enum Sock {
    /// Repeats `call` while it fails with EINTR; any other result (including -1 with another errno) is returned as is.
    public static func retry<T: SignedInteger>(_ call: () -> T) -> T {
        while true {
            let r = call()
            if r < 0 && errno == EINTR { continue }
            return r
        }
    }

    /// connect(2) on a blocking socket. An interrupted connect carries on in the kernel, so it is not repeated (that gives
    /// EALREADY/EISCONN): wait for the socket to become writable, then read the outcome from SO_ERROR.
    /// `call` is the syscall (a test replaces it); `waitMs`: how long to wait for the connection (-1 = without limit).
    public static func connect(_ fd: Int32, _ addr: UnsafePointer<sockaddr>, _ len: socklen_t, waitMs: Int32 = 30_000,
                               call: (Int32, UnsafePointer<sockaddr>, socklen_t) -> Int32 = { Darwin.connect($0, $1, $2) }) -> Int32 {
        if call(fd, addr, len) == 0 { return 0 }
        guard errno == EINTR else { return -1 }
        var p = pollfd(fd: fd, events: Int16(POLLOUT), revents: 0)
        var r = poll(&p, 1, waitMs)
        while r < 0 && errno == EINTR { r = poll(&p, 1, waitMs) }
        if r == 0 { errno = ETIMEDOUT; return -1 }
        if r < 0 { return -1 }
        var err: Int32 = 0, n = socklen_t(MemoryLayout<Int32>.size)
        guard getsockopt(fd, SOL_SOCKET, SO_ERROR, &err, &n) == 0 else { return -1 }
        if err != 0 { errno = err; return -1 }
        return 0
    }
}
