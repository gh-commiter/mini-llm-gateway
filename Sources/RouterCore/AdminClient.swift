import Darwin
import Foundation

/// Odjemalec skrbniškega API na tem računalniku (minigw router): ena zahteva na novi povezavi Noise neposredno iz
/// procesa, brez lokalnega proxyja, ki bi ga lahko uporabil drug proces (S2). `authorization` dobi h rokovanja te
/// povezave in vrne vrednost glave (dokaz MGW-Admin); skrivnost nikoli ne gre po žici.
public struct AdminClient: Sendable {
    public let port: UInt16
    public let routerKey: Data

    public init(port: UInt16, routerKey: Data) { self.port = port; self.routerKey = routerKey }

    /// (status, telo); 0 = ni povezave ali rokovanje ni uspelo (na vratih ni ta router).
    public func request(_ method: String, _ path: String, _ body: Data?, authorization: (@Sendable (Data) -> String)? = nil,
                        timeout: Int = 60) -> (Int, Data?) {
        let fd = socket(AF_INET, SOCK_STREAM, 0)
        guard fd >= 0 else { return (0, nil) }
        defer { close(fd) }
        var tv = timeval(tv_sec: timeout, tv_usec: 0)
        setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
        setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET); addr.sin_port = port.bigEndian; addr.sin_addr.s_addr = inet_addr("127.0.0.1")
        let ok = withUnsafePointer(to: &addr) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
        guard ok == 0 else { return (0, nil) }
        func send(_ d: Data) -> Bool {
            d.withUnsafeBytes { b in
                var off = 0
                while off < b.count {
                    let n = write(fd, b.baseAddress! + off, b.count - off)
                    if n <= 0 { return false }
                    off += n
                }
                return true
            }
        }
        func recv() -> Data? {
            var buf = [UInt8](repeating: 0, count: 64 << 10)
            let n = read(fd, &buf, buf.count)
            return n > 0 ? Data(buf[0..<n]) : nil
        }
        var hs = Noise.Initiator(routerKey: routerKey, suite: .aesGCM)
        guard let first = try? hs.first(), send(Noise.Suite.aesGCM.magic + first) else { return (0, nil) }
        var got = Data()
        while got.count < Noise.handshakeLength, let d = recv() { got += d }
        guard got.count >= Noise.handshakeLength, let session = try? hs.finish(Data(got.prefix(Noise.handshakeLength))) else { return (0, nil) }
        var head = "\(method) \(path) HTTP/1.1\r\nHost: 127.0.0.1:\(port)\r\nConnection: close\r\nContent-Type: application/json\r\n"
        if let authorization { head += "Authorization: \(authorization(session.hash))\r\n" }
        head += "Content-Length: \((body ?? Data()).count)\r\n\r\n"
        guard send(Noise.frames(Data(head.utf8) + (body ?? Data()), session.send)) else { return (0, nil) }
        var raw = Data(got.dropFirst(Noise.handshakeLength)), plain = Data()
        while true {
            guard let p = try? Noise.unframe(&raw, session.receive) else { return (0, nil) }
            plain += p
            guard plain.count <= 32 << 20, let d = recv() else { break }
            raw += d
        }
        guard let sep = plain.range(of: Data("\r\n\r\n".utf8)),
              let line = String(data: plain[..<sep.lowerBound], encoding: .utf8)?.split(separator: "\r\n").first,
              let status = line.split(separator: " ").dropFirst().first.flatMap({ Int($0) }) else { return (0, nil) }
        return (status, Data(plain[sep.upperBound...]))
    }
}
