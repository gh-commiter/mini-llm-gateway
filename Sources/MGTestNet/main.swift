// mg-test-net: the one signed program that tests use whenever they need a socket outside 127.0.0.1 (the macOS firewall asks for permission per
// unsigned program that accepts connections; a test process must not do that itself). It does network things only: no file access, no command
// execution, no way to be turned into a general tool. One JSON object per line on stdout. Ends on stdin EOF or after the deadline.
//
//   mg-test-net listen <ipv4> <port|0> [--mode close|echo|hold] [--accept N] [--deadline SECONDS]
//       binds and listens ("listening" with the port), then by mode: close = accept up to N connections and close each ("accepted"),
//       echo = accept connections and send back what they send, hold = listen and never accept. --accept 0 = unlimited.
//   mg-test-net bind <ipv4> <port> [--reuseaddr] [--reuseport]
//       tries one bind (no listen) and reports "bind" with ok and errno; used to prove that a port cannot be shared.
import Darwin

setvbuf(stdout, nil, _IOLBF, 0)

func quoted(_ v: String) -> String {
    var out = "\""
    for c in v.unicodeScalars { if c == "\"" || c == "\\" { out += "\\" + String(c) } else if c.value < 32 { out += " " } else { out += String(c) } }
    return out + "\""
}

func emit(_ pairs: [(String, String)]) {
    // values are numbers, true/false or short strings made here (an address, an errno text)
    print("{" + pairs.map { k, v in quoted(k) + ":" + (Int(v) != nil || v == "true" || v == "false" ? v : quoted(v)) }.joined(separator: ",") + "}")
}

func fail(_ why: String, code: Int32 = 2) -> Never {
    emit([("event", "error"), ("message", why)])
    exit(code)
}

func address(_ ip: String, _ port: UInt16) -> sockaddr_in {
    var a = sockaddr_in()
    a.sin_len = UInt8(MemoryLayout<sockaddr_in>.size); a.sin_family = sa_family_t(AF_INET); a.sin_port = port.bigEndian
    guard inet_pton(AF_INET, ip, &a.sin_addr) == 1 else { fail("not an IPv4 address: \(ip)") }
    return a
}

func bindSocket(_ fd: Int32, _ a: sockaddr_in) -> Int32 {
    var a = a
    return withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
}

func boundPort(_ fd: Int32) -> UInt16 {
    var a = sockaddr_in(), n = socklen_t(MemoryLayout<sockaddr_in>.size)
    _ = withUnsafeMutablePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(fd, $0, &n) } }
    return UInt16(bigEndian: a.sin_port)
}

func peerText(_ a: sockaddr_in) -> String {
    var a = a, buf = [CChar](repeating: 0, count: Int(INET_ADDRSTRLEN))
    inet_ntop(AF_INET, &a.sin_addr, &buf, socklen_t(buf.count))
    return String(cString: buf) + ":" + String(UInt16(bigEndian: a.sin_port))
}

var args = Array(CommandLine.arguments.dropFirst())
guard args.count >= 3, ["listen", "bind"].contains(args[0]) else { fail("usage: mg-test-net listen <ipv4> <port> [--mode close|echo|hold] [--accept N] [--deadline S] | bind <ipv4> <port> [--reuseaddr] [--reuseport]") }
let command = args.removeFirst(), ip = args.removeFirst()
guard let port = UInt16(args.removeFirst()) else { fail("bad port") }

var mode = "close", accepts = 1, deadline = 60, reuseAddr = false, reusePort = false
while !args.isEmpty {
    let flag = args.removeFirst()
    func value() -> String { if args.isEmpty { fail("\(flag) needs a value") }; return args.removeFirst() }
    switch (command, flag) {
    case ("listen", "--mode"): mode = value(); guard ["close", "echo", "hold"].contains(mode) else { fail("bad mode") }
    case ("listen", "--accept"): guard let n = Int(value()), (0...1000).contains(n) else { fail("bad --accept") }; accepts = n
    case ("listen", "--deadline"): guard let n = Int(value()), (1...300).contains(n) else { fail("bad --deadline (1...300)") }; deadline = n
    case ("bind", "--reuseaddr"): reuseAddr = true
    case ("bind", "--reuseport"): reusePort = true
    default: fail("unknown argument \(flag)")
    }
}

let fd = socket(AF_INET, SOCK_STREAM, 0)
guard fd >= 0 else { fail("socket: errno \(errno)") }
var one: Int32 = 1
if command == "bind" {
    if reuseAddr { setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, 4) }
    if reusePort { setsockopt(fd, SOL_SOCKET, SO_REUSEPORT, &one, 4) }
    let rc = bindSocket(fd, address(ip, port)), e = errno
    emit([("event", "bind"), ("ok", rc == 0 ? "true" : "false"), ("errno", String(rc == 0 ? 0 : e)), ("error", rc == 0 ? "" : String(cString: strerror(e)))])
    exit(0)
}

setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, 4)
guard bindSocket(fd, address(ip, port)) == 0 else { fail("bind: \(String(cString: strerror(errno))) (errno \(errno))", code: 3) }
guard listen(fd, 128) == 0 else { fail("listen: errno \(errno)", code: 3) }  // the backlog holds every probe: connections that are never accepted must not be refused
emit([("event", "listening"), ("addr", ip), ("port", String(boundPort(fd))), ("mode", mode)])

let end = time(nil) + deadline
var accepted = 0, open: [Int32] = []
while time(nil) < end {
    var fds = [pollfd(fd: 0, events: Int16(POLLIN), revents: 0), pollfd(fd: fd, events: Int16(POLLIN), revents: 0)] + open.map { pollfd(fd: $0, events: Int16(POLLIN), revents: 0) }
    if poll(&fds, nfds_t(fds.count), 200) <= 0 { continue }
    if fds[0].revents != 0 {  // stdin: data is ignored, EOF (or an error) ends the program
        var b = [UInt8](repeating: 0, count: 64)
        if read(0, &b, 64) <= 0 { break }
    }
    if fds[1].revents & Int16(POLLIN) != 0, mode != "hold" {
        var peer = sockaddr_in(), n = socklen_t(MemoryLayout<sockaddr_in>.size)
        let c = withUnsafeMutablePointer(to: &peer) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { accept(fd, $0, &n) } }
        if c >= 0 {
            accepted += 1
            emit([("event", "accepted"), ("peer", peerText(peer)), ("n", String(accepted))])
            if mode == "close" { close(c) } else { open.append(c) }
        }
        if accepts > 0 && accepted >= accepts && mode == "close" { emit([("event", "done"), ("accepted", String(accepted))]) }
    }
    for (i, p) in fds.dropFirst(2).enumerated() where p.revents != 0 {  // echo
        var b = [UInt8](repeating: 0, count: 4096)
        let k = read(p.fd, &b, b.count)
        if k <= 0 { close(p.fd); open.removeAll { $0 == p.fd }; _ = i } else { _ = write(p.fd, b, k) }
    }
}
emit([("event", "exit"), ("accepted", String(accepted))])
