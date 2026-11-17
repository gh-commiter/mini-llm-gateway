import Foundation

// The answer of an upstream gateway, read from the plain bytes of the encrypted connection. The peer is an adversary here (it passed the
// pinned handshake, but a hostile or broken gateway can send anything), so this is one small incremental state machine written to never trap
// and never do more than linear work:
//   * one buffer with a read index; bytes are looked at once (the search for the end of a line resumes where it stopped), nothing is
//     copied per chunk;
//   * every number comes from `number(_:radix:)`: digits only, 1 to 8 of them, no sign, no space, so it cannot overflow;
//   * every cap is checked BEFORE bytes are kept (raw bytes, head, body, lines, trailers, headers, interim answers);
//   * anything unexpected is `.failed` (the caller says "no answer"), never a crash. No force unwrap, no literal subscript.
// There is no other response parser in the project to reuse: HTTPParser (HTTP.swift) reads REQUESTS and refuses chunked bodies; the client's
// own calls hand the answer to URLSession.

/// What the reader says after the bytes it was given.
enum UpstreamReadOutcome: Equatable {
    case needMore
    case done(UpstreamAnswer)
    case failed
}

struct UpstreamHTTPReader {
    static let maxHead = 64 << 10
    static let maxLine = 1 << 10         // a chunk-size line or a trailer line
    static let maxTrailers = 4 << 10     // all trailer bytes together
    static let maxHeaders = 100
    static let maxInterim = 3            // "100 Continue" and the like, then it is refused
    static let maxChunks = 4096          // chunks of one body: a body of tiny chunks is a way to burn time, not to send data

    private enum State { case head, bodyLength(Int), bodyUntilClose, chunkSize, chunkData(Int), chunkEnd, trailers, finished }

    private let cap: Int
    private let method: String
    private var buf: [UInt8] = []
    private var pos = 0                  // consumed up to here
    private var scan = 0                 // the search for CRLF resumes here (never behind it)
    private var rawTotal = 0
    private var state = State.head
    private var interim = 0
    private var trailerBytes = 0
    private var status = 0, retryAfter = 0
    private var body: [UInt8] = []
    private var done = false
    private var failedFlag = false
    private var chunks = 0
    /// Asked at every step: true = the request is over (its deadline passed), so the work stops here instead of going on in the background.
    var cancelled: (() -> Bool)?
    /// Operations counted (bytes looked at or copied): the tests assert that they stay linear in the input.
    private(set) var operations = 0

    /// cap: the most body bytes kept. method: "HEAD" has no body whatever the head says.
    init(cap: Int, method: String = "GET") { self.cap = cap; self.method = method }

    /// Raw bytes accepted in total: the head, the framing of the largest body made of one-byte chunks (6 bytes each) and some room.
    private var rawCap: Int { Self.maxHead + 8 * cap + (64 << 10) }

    /// The strict number: ASCII digits of the radix only, 1 to 8 of them. Nil for anything else (empty, sign, space, too long).
    static func number(_ s: ArraySlice<UInt8>, radix: Int) -> Int? {
        guard !s.isEmpty, s.count <= 8 else { return nil }
        var v = 0
        for b in s {
            let d: Int
            switch b {
            case 0x30...0x39: d = Int(b) - 0x30
            case 0x61...0x66 where radix == 16: d = Int(b) - 0x61 + 10
            case 0x41...0x46 where radix == 16: d = Int(b) - 0x41 + 10
            default: return nil
            }
            guard d < radix else { return nil }
            v = v * radix + d  // at most 16^8: cannot overflow
        }
        return v
    }

    mutating func feed(_ data: Data) -> UpstreamReadOutcome {
        if failedFlag || cancelled?() == true { return fail() }
        if done { return finished() }
        guard rawTotal + data.count <= rawCap else { return fail() }
        rawTotal += data.count
        buf.append(contentsOf: data)
        operations += data.count
        return step()
    }

    /// The connection ended: an answer without a length is complete now; every other incomplete one is no answer.
    mutating func end() -> UpstreamReadOutcome {
        if failedFlag { return .failed }
        if done { return finished() }
        let o = step()
        if o != .needMore { return o }
        if case .bodyUntilClose = state { done = true; state = .finished; return finished() }
        return fail()
    }

    private mutating func fail() -> UpstreamReadOutcome { failedFlag = true; buf = []; body = []; return .failed }
    private func finished() -> UpstreamReadOutcome { .done(UpstreamAnswer(status: status, body: Data(body), retryAfter: retryAfter)) }
    private var available: Int { buf.count - pos }

    private mutating func compact() {
        if pos > (64 << 10) && pos * 2 > buf.count { buf.removeFirst(pos); scan -= pos; pos = 0 }  // amortised: each byte moves at most once
    }

    /// The end of the line that starts at `pos`: the index of its CR, or nil while the line is not complete. Resumes where it stopped.
    private enum Line { case incomplete, tooLong, at(Int) }

    private mutating func lineEnd(limit: Int) -> Line {
        if scan < pos { scan = pos }
        while scan + 1 < buf.count {
            operations += 1
            if buf[scan] == 0x0d && buf[scan + 1] == 0x0a { return .at(scan) }
            scan += 1
            if scan - pos > limit { return .tooLong }
        }
        return .incomplete
    }

    private mutating func step() -> UpstreamReadOutcome {
        while true {
            if cancelled?() == true { return fail() }
            switch state {
            case .finished:
                return finished()
            case .head:
                guard let found = headEnd() else {
                    return available > Self.maxHead ? fail() : .needMore
                }
                guard found - pos <= Self.maxHead, parseHead(buf[pos..<found]) else { return fail() }
                pos = found + 4; scan = pos
                if (100...199).contains(status) {  // an interim answer: skipped a few times, then refused (101 switches protocols: never)
                    interim += 1
                    if status == 101 || interim > Self.maxInterim { return fail() }
                    state = .head
                    continue
                }
                if status == 204 || status == 304 || method == "HEAD" { state = .finished; done = true; continue }
                switch bodyKind {
                case .length(let n):
                    if n == 0 { state = .finished; done = true } else { state = .bodyLength(n) }
                case .chunked: state = .chunkSize
                case .close: state = .bodyUntilClose
                case .bad: return fail()
                }
            case .bodyLength(let n):
                guard available >= n else { return .needMore }
                body.append(contentsOf: buf[pos..<pos + n])
                operations += n
                pos += n
                state = .finished; done = true
            case .bodyUntilClose:
                guard available + body.count <= cap else { return fail() }
                body.append(contentsOf: buf[pos...])
                operations += available
                pos = buf.count; scan = pos
                buf.removeAll(keepingCapacity: true); pos = 0; scan = 0
                return .needMore
            case .chunkSize:
                let end: Int
                switch lineEnd(limit: Self.maxLine) {
                case .incomplete: return .needMore
                case .tooLong: return fail()
                case .at(let e): end = e
                }
                let line = buf[pos..<end]
                let digits = line.prefix { $0 != 0x3b }  // up to a ';' (an extension, which is only checked to be printable)
                guard let size = Self.number(digits, radix: 16), line.dropFirst(digits.count).allSatisfy({ $0 >= 0x20 && $0 < 0x7f }) else { return fail() }
                pos = end + 2; scan = pos
                if size == 0 { state = .trailers } else {
                    chunks += 1
                    guard chunks <= Self.maxChunks, body.count + size <= cap else { return fail() }  // before any byte of it is kept
                    state = .chunkData(size)
                }
            case .chunkData(let n):
                guard available >= n else { return .needMore }
                body.append(contentsOf: buf[pos..<pos + n])
                operations += n
                pos += n
                state = .chunkEnd
                compact()
            case .chunkEnd:
                guard available >= 2 else { return .needMore }
                guard buf[pos] == 0x0d && buf[pos + 1] == 0x0a else { return fail() }
                pos += 2; scan = pos
                state = .chunkSize
            case .trailers:
                let end: Int
                switch lineEnd(limit: Self.maxLine) {
                case .incomplete: return .needMore
                case .tooLong: return fail()
                case .at(let e): end = e
                }
                trailerBytes += end - pos + 2
                guard trailerBytes <= Self.maxTrailers else { return fail() }
                let empty = end == pos
                pos = end + 2; scan = pos
                if empty { state = .finished; done = true }
            }
        }
    }

    /// The index of the CRLFCRLF that ends the head, searching only bytes not looked at before.
    private mutating func headEnd() -> Int? {
        if scan < pos { scan = pos }
        while scan + 3 < buf.count {
            operations += 1
            if buf[scan] == 0x0d && buf[scan + 1] == 0x0a && buf[scan + 2] == 0x0d && buf[scan + 3] == 0x0a { return scan }
            scan += 1
            if scan - pos > Self.maxHead { return nil }
        }
        return nil
    }

    private enum BodyKind { case length(Int), chunked, close, bad }
    private var bodyKind = BodyKind.close

    /// The status line and the headers. Strict: HTTP/1.x, a three-digit status, header names of token characters, no folded line, no
    /// control or non-ASCII byte, at most `maxHeaders` headers, one Content-Length and one Transfer-Encoding at most, never both.
    private mutating func parseHead(_ head: ArraySlice<UInt8>) -> Bool {
        var lines: [ArraySlice<UInt8>] = []
        var from = head.startIndex
        var i = head.startIndex
        while i < head.endIndex {
            operations += 1
            let b = head[i]
            if b == 0x0d {
                guard i + 1 < head.endIndex, head[i + 1] == 0x0a else { return false }
                lines.append(head[from..<i]); i += 2; from = i; continue
            }
            if b == 0x0a || b == 0 || (b < 0x20 && b != 0x09) || b >= 0x7f { return false }
            i += 1
        }
        lines.append(head[from..<head.endIndex])
        guard let first = lines.first, lines.count <= Self.maxHeaders + 1, first.count >= 12 else { return false }
        let status = first.dropFirst(9).prefix(3)
        guard first.starts(with: Array("HTTP/1.".utf8)), first[first.startIndex + 7] == 0x30 || first[first.startIndex + 7] == 0x31, first[first.startIndex + 8] == 0x20,
              let code = Self.number(status, radix: 10), status.count == 3, (100...599).contains(code),
              first.count == 12 || first[first.startIndex + 12] == 0x20 else { return false }
        self.status = code
        var length: Int?, chunked = false, sawLength = false, sawEncoding = false
        for l in lines.dropFirst() {
            guard let colon = l.firstIndex(of: 0x3a), colon > l.startIndex, l.first != 0x20, l.first != 0x09 else { return false }
            let name = l[l.startIndex..<colon]
            guard name.allSatisfy(Self.isToken) else { return false }
            let value = trim(l[(colon + 1)...])
            switch String(decoding: name, as: UTF8.self).lowercased() {
            case "content-length":
                guard !sawLength, let n = Self.number(value, radix: 10), n <= cap else { return false }  // a body above the cap is refused at the head
                sawLength = true; length = n
            case "transfer-encoding":
                guard !sawEncoding, String(decoding: value, as: UTF8.self).lowercased() == "chunked" else { return false }
                sawEncoding = true; chunked = true
            case "retry-after":
                retryAfter = Self.number(value, radix: 10) ?? 0
            default: break
            }
        }
        if sawLength && sawEncoding { return false }
        bodyKind = chunked ? .chunked : (length.map { .length($0) } ?? .close)
        return true
    }

    private func trim(_ s: ArraySlice<UInt8>) -> ArraySlice<UInt8> {
        var a = s.startIndex, b = s.endIndex
        while a < b, s[a] == 0x20 || s[a] == 0x09 { a += 1 }
        while b > a, s[b - 1] == 0x20 || s[b - 1] == 0x09 { b -= 1 }
        return s[a..<b]
    }

    private static func isToken(_ b: UInt8) -> Bool {
        switch b {
        case 0x30...0x39, 0x41...0x5a, 0x61...0x7a: return true
        case 0x21, 0x23, 0x24, 0x25, 0x26, 0x27, 0x2a, 0x2b, 0x2d, 0x2e, 0x5e, 0x5f, 0x60, 0x7c, 0x7e: return true
        default: return false
        }
    }
}

/// One-shot form of the reader for the tests and for callers that have all the bytes: nil while the answer is incomplete and the connection
/// is still open.
enum UpstreamHTTPAnswer {
    static func parse(_ raw: Data, closed: Bool, cap: Int) -> UpstreamAnswer? {
        var r = UpstreamHTTPReader(cap: cap)
        var o = r.feed(raw)
        if o == .needMore && closed { o = r.end() }
        switch o {
        case .done(let a): return a
        case .failed: return UpstreamAnswer(status: 0)
        case .needMore: return nil
        }
    }
}
