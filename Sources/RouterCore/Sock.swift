import Darwin
import Foundation

/// Raw socket calls that a signal may interrupt: EINTR is not an error and not the end of the stream (#109: AdminClient
/// returned 0 "no router here" for a handshake that was only interrupted).
///
/// #177: none of them waits inside a read, write, accept or connect. macOS marks a socket "draining" for good when ANY descriptor of it is
/// closed while someone else holds that descriptor: also a spawned child's short-lived copy (posix_spawn copies the parent's descriptors
/// before exec drops them, POSIX_SPAWN_CLOEXEC_DEFAULT too) while any process inspects the child's sockets (PeerProcess.pids on every admin
/// request, lsof). From then on every BLOCKING read, write or accept that has to wait fails with EBADF although our descriptor is open; a
/// marked listener passes the mark to every connection it accepts. poll and calls on an O_NONBLOCK socket are not affected, so these
/// helpers make the socket non-blocking and wait with poll. The wait is limited by the socket's SO_RCVTIMEO / SO_SNDTIMEO as a blocking
/// call would be (none set = no limit); a timeout is -1 with EAGAIN, as from a blocking call.
public enum Sock {
    /// Repeats `call` while it fails with EINTR; any other result (including -1 with another errno) is returned as is.
    public static func retry<T: SignedInteger>(_ call: () -> T) -> T {
        while true {
            let r = call()
            if r < 0 && errno == EINTR { continue }
            return r
        }
    }

    public static func nonBlocking(_ fd: Int32) {
        let fl = fcntl(fd, F_GETFL)
        if fl >= 0 && fl & O_NONBLOCK == 0 { _ = fcntl(fd, F_SETFL, fl | O_NONBLOCK) }
    }

    /// Waits with poll until `events` are ready: 0, or -1 with EAGAIN after the socket's timeout `option` (SO_RCVTIMEO / SO_SNDTIMEO),
    /// or -1 with poll's errno. Each call starts the full timeout again (as each blocking call does).
    public static func wait(_ fd: Int32, _ events: Int16, _ option: Int32) -> Int32 {
        var tv = timeval(), len = socklen_t(MemoryLayout<timeval>.size)
        getsockopt(fd, SOL_SOCKET, option, &tv, &len)
        let ms = tv.tv_sec == 0 && tv.tv_usec == 0 ? -1 : Int32(min(tv.tv_sec, 86_400)) * 1000 + tv.tv_usec / 1000
        var p = pollfd(fd: fd, events: events, revents: 0)
        while true {
            let r = poll(&p, 1, ms)
            if r > 0 { return 0 }
            if r == 0 { errno = EAGAIN; return -1 }
            if errno != EINTR { return -1 }
        }
    }

    /// read(2) of a blocking socket, without the blocking: at most `count` bytes, 0 at the end of the stream, -1 with errno.
    public static func read(_ fd: Int32, _ buf: UnsafeMutableRawPointer, _ count: Int) -> Int {
        nonBlocking(fd)
        while true {
            let n = recv(fd, buf, count, 0)
            if n >= 0 { return n }
            if errno == EINTR { continue }
            if errno != EAGAIN || wait(fd, Int16(POLLIN), SO_RCVTIMEO) < 0 { return -1 }
        }
    }

    /// write(2) of a blocking socket, without the blocking: at least one byte written (the count), or -1 with errno.
    public static func write(_ fd: Int32, _ buf: UnsafeRawPointer, _ count: Int) -> Int {
        nonBlocking(fd)
        while true {
            let n = send(fd, buf, count, 0)
            if n >= 0 { return n }
            if errno == EINTR { continue }
            if errno != EAGAIN || wait(fd, Int16(POLLOUT), SO_SNDTIMEO) < 0 { return -1 }
        }
    }

    /// accept(2) of a blocking listener, without the blocking (the wait has no limit unless the listener has SO_RCVTIMEO). The accepted
    /// socket is non-blocking: a marked listener marks it too, and only non-blocking I/O (these helpers) works on it then.
    public static func accept(_ fd: Int32) -> Int32 {
        nonBlocking(fd)
        while true {
            let c = Darwin.accept(fd, nil, nil)
            if c >= 0 { nonBlocking(c); return c }
            if errno == EINTR || errno == ECONNABORTED { continue }
            if errno != EAGAIN || wait(fd, Int16(POLLIN), SO_RCVTIMEO) < 0 { return -1 }
        }
    }

    /// connect(2) of a blocking socket, without the blocking: the socket is made non-blocking (and stays so: use these helpers on it), then
    /// the connection is awaited with poll and its outcome read from SO_ERROR. An interrupted connect carries on in the kernel, so it is
    /// not repeated (that gives EALREADY/EISCONN). `call` is the syscall (a test replaces it); `waitMs`: how long to wait (-1 = no limit).
    public static func connect(_ fd: Int32, _ addr: UnsafePointer<sockaddr>, _ len: socklen_t, waitMs: Int32 = 30_000,
                               call: (Int32, UnsafePointer<sockaddr>, socklen_t) -> Int32 = { Darwin.connect($0, $1, $2) }) -> Int32 {
        nonBlocking(fd)
        if call(fd, addr, len) == 0 { return 0 }
        guard errno == EINTR || errno == EINPROGRESS else { return -1 }
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
