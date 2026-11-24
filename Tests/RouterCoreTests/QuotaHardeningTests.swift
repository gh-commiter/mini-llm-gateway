import Foundation
import Testing
@testable import RouterCore

// fix3: the probe's process tree, the Codex log read, the provider response limit, the failure throttle and the provider cache key. Everything is
// fake or temporary: fake claude scripts, planted links in temp folders, a local socket server; no real CLI, network or Codex home.

private func tempDir(_ name: String) -> URL {
    let u = FileManager.default.temporaryDirectory.appendingPathComponent("mghard-\(name)-\(UUID().uuidString.prefix(8))", isDirectory: true).cleanedAtExit()
    try? FileManager.default.createDirectory(at: u, withIntermediateDirectories: true)
    return u
}

private let envelope = #"{"is_error":false,"num_turns":0,"total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0},"result":"Current week (all models): 8% used\n"}"#

private func script(_ dir: URL, _ body: String) throws -> String {
    let p = dir.appendingPathComponent("claude")
    try Data("#!/bin/sh\n\(body)\n".utf8).write(to: p)
    chmod(p.path, 0o755)
    return p.path
}

private func alive(_ pid: pid_t) -> Bool { kill(pid, 0) == 0 }

private func waitGone(_ pid: pid_t, seconds: Double = 3) async -> Bool {
    for _ in 0..<Int(seconds * 50) { if !alive(pid) { return true }; try? await Task.sleep(for: .milliseconds(20)) }
    return !alive(pid)
}

// MARK: Q1 the probe's tree

@Suite(.serialized) struct QuotaProbeTree {
    @Test func aDescendantThatHoldsTheOutputIsKilledAndTheProbeReturns() async throws {
        let dir = tempDir("q1a")
        let exe = try script(dir, "sleep 60 &\necho $! > \"\(dir.path)/child.pid\"\nprintf '%s' '\(envelope)'")  // claude ends, its child keeps stdout
        let t0 = Date()
        let out = try await ClaudeUsageProbe.run(exe: exe, parent: tempDir("p"), limit: 30, drain: 0.5)
        #expect(String(decoding: out, as: UTF8.self).contains("Current week"))
        // the property: it returned by its drain, not by the limit (30 s) and not by the grandchild's own sleep (60 s); the margin is the load of the machine, not a guess at its speed
        #expect(Date().timeIntervalSince(t0) < 25, "took \(Date().timeIntervalSince(t0)) s")
        let pid = pid_t(try String(contentsOf: dir.appendingPathComponent("child.pid"), encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines))!
        #expect(await waitGone(pid), "the grandchild lives")
    }

    @Test func theLimitEndsTheWholeTreeAndTheNextCallWorks() async throws {
        let dir = tempDir("q1b")
        let exe = try script(dir, "sleep 60 &\necho $! > \"\(dir.path)/child.pid\"\necho $$ > \"\(dir.path)/main.pid\"\nsleep 60")
        let t0 = Date()
        do { _ = try await ClaudeUsageProbe.run(exe: exe, parent: tempDir("p"), limit: 1, drain: 0.5); Issue.record("no error") } catch {
            #expect((error as? QuotaError)?.description == "claude /usage failed: timed out after 1 s")
        }
        #expect(Date().timeIntervalSince(t0) < 25, "took \(Date().timeIntervalSince(t0)) s")  // ends by its 1 s limit, far below the 60 s sleeps
        for f in ["child.pid", "main.pid"] {
            let pid = pid_t(try String(contentsOf: dir.appendingPathComponent(f), encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines))!
            #expect(await waitGone(pid), "\(f) lives")
        }
        // the next call works
        let ok = try script(tempDir("q1c"), "printf '%s' '\(envelope)'")
        #expect(String(decoding: try await ClaudeUsageProbe.run(exe: ok, parent: tempDir("p"), limit: 5), as: UTF8.self).contains("Current week"))
    }

    @Test func aHungProbeDoesNotHangLaterReports() async throws {
        let dir = tempDir("q1d")
        let hung = try script(dir, "sleep 60 &\nsleep 60")
        let e = QuotaEngine(runUsage: { exe, parent in try await ClaudeUsageProbe.run(exe: exe, parent: parent, limit: 1, drain: 0.5) }, zone: TimeZone(identifier: "UTC")!)
        let i = QuotaInputs(claudeProbe: true, claudeExe: hung, claudeEnabled: true, providers: [], key: { _ in nil }, tmpParent: tempDir("p"))
        let t0 = Date()
        let first = await e.report(i, now: t0).filter { $0.name == "claude" }
        #expect(first.first?.reason.contains("timed out") == true && Date().timeIntervalSince(t0) < 25)  // ends by its 1 s limit, far below the stub's 60 s sleeps
        _ = try script(dir, "printf '%s' '\(envelope)'")  // the same path now works
        let later = await e.report(i, now: t0.addingTimeInterval(61)).filter { $0.name.hasPrefix("claude") }  // after the back-off
        #expect(later.first?.name == "claude.week.all-models", "\(later)")
    }

    // MARK: Q9 no inherited descriptor, stdin is /dev/null

    @Test(.blocking) func theProbeSpawnInheritsNoDescriptorAndHasNoStdin() async throws {
        var p: [Int32] = [0, 0]
        precondition(pipe(&p) == 0)  // no FD_CLOEXEC, as Darwin creates it
        defer { close(p[0]); close(p[1]) }
        // the probe's exact configuration: spawnGroup, stdin /dev/null (input: false), the probe's environment, an empty private folder
        for fd in p {
            let child = try #require(Native.spawnGroup("/bin/test", ["-e", "/dev/fd/\(fd)"], dir: tempDir("q9"), env: ClaudeUsageProbe.env([:], tmp: "/tmp"), input: false))
            var st: Int32 = 0; waitpid(child.pid, &st, 0)
            #expect((st >> 8) & 0xff != 0, "the probe's spawn passed fd \(fd) on")
        }
        // and through the probe itself: the fake claude reports what it sees (cat would hang on an inherited terminal)
        let dir = tempDir("q9b")
        let exe = try script(dir, "cat > /dev/null\nfor fd in \(p[0]) \(p[1]); do if test -e /dev/fd/$fd; then printf open; else printf closed; fi; done")
        let out = try await ClaudeUsageProbe.run(exe: exe, parent: tempDir("p"), limit: 10)
        #expect(String(decoding: out, as: UTF8.self) == "closedclosed")
    }
}

// MARK: Q3 the Codex log read through links

@Suite struct QuotaCodexLogLinks {
    private let rec = #"{"payload":{"rate_limits":{"primary":{"used_percent":5,"window_minutes":300,"resets_at":4102444800}}}}"# + "\n"

    @Test func linksAndOddFilesAreNotRead() throws {
        let home = tempDir("home"), other = tempDir("other")
        let fm = FileManager.default
        try fm.createDirectory(at: other.appendingPathComponent("2026/11/14"), withIntermediateDirectories: true)
        try Data(rec.utf8).write(to: other.appendingPathComponent("2026/11/14/rollout-x-thread-a.jsonl"))
        try fm.createDirectory(at: home.appendingPathComponent("sessions"), withIntermediateDirectories: true)
        // a linked year folder
        try fm.createSymbolicLink(at: home.appendingPathComponent("sessions/2026"), withDestinationURL: other.appendingPathComponent("2026"))
        #expect(QuotaParse.codexSessionLog(home: home.path, thread: "thread-a") == nil, "entered a linked year folder")
        // a linked day folder
        try fm.createDirectory(at: home.appendingPathComponent("sessions/2027/01"), withIntermediateDirectories: true)
        try fm.createSymbolicLink(at: home.appendingPathComponent("sessions/2027/01/02"), withDestinationURL: other.appendingPathComponent("2026/11/14"))
        #expect(QuotaParse.codexSessionLog(home: home.path, thread: "thread-a") == nil, "entered a linked day folder")
        try fm.removeItem(at: home.appendingPathComponent("sessions/2027/01/02"))
        let day = home.appendingPathComponent("sessions/2027/01/02")
        try fm.createDirectory(at: day, withIntermediateDirectories: true)
        // a linked log, a link to /dev/zero, a FIFO
        try fm.createSymbolicLink(at: day.appendingPathComponent("rollout-y-thread-a.jsonl"), withDestinationURL: other.appendingPathComponent("2026/11/14/rollout-x-thread-a.jsonl"))
        try fm.createSymbolicLink(at: day.appendingPathComponent("rollout-z-thread-b.jsonl"), withDestinationURL: URL(fileURLWithPath: "/dev/zero"))
        #expect(mkfifo(day.appendingPathComponent("rollout-f-thread-c.jsonl").path, 0o644) == 0)
        for th in ["thread-a", "thread-b", "thread-c"] {
            let fd = QuotaParse.codexSessionLog(home: home.path, thread: th)
            if let fd { close(fd) }
            #expect(fd == nil, "took \(th)")
        }
        #expect(QuotaParse.lastRateLimits(path: day.appendingPathComponent("rollout-y-thread-a.jsonl").path, observed: Date()).isEmpty)  // a link as the path itself
        #expect(QuotaParse.lastRateLimits(path: day.appendingPathComponent("rollout-z-thread-b.jsonl").path, observed: Date()).isEmpty)
        #expect(QuotaParse.lastRateLimits(path: day.appendingPathComponent("rollout-f-thread-c.jsonl").path, observed: Date()).isEmpty)  // does not block on the FIFO
        // the real log is found, and only its tail is read
        try Data(rec.utf8).write(to: day.appendingPathComponent("rollout-r-thread-a.jsonl"))
        let v = QuotaParse.lastRateLimits(home: home.path, thread: "thread-a", observed: Date())
        #expect(v.count == 1 && v[0].value == 5)
    }

    @Test func onlyTheLastBytesOfAHugeLogAreRead() throws {
        let day = tempDir("big").appendingPathComponent("sessions/2026/11/14")
        try FileManager.default.createDirectory(at: day, withIntermediateDirectories: true)
        let path = day.appendingPathComponent("rollout-b-thread-big.jsonl").path
        FileManager.default.createFile(atPath: path, contents: nil)
        let h = try FileHandle(forWritingTo: URL(fileURLWithPath: path))
        try h.truncate(atOffset: 300 << 20)  // a sparse 300 MB file: reading it whole would show
        try h.seekToEnd()
        try h.write(contentsOf: Data(("\n" + rec).utf8))
        try h.close()
        let t0 = Date()
        let v = QuotaParse.lastRateLimits(path: path, observed: Date())
        // the result is the property (the last record of a 300 MB file, found); the time is only a ceiling against a pathological scan: a tight one only measures the machine's load
        #expect(v.count == 1 && Date().timeIntervalSince(t0) < 20, "took \(Date().timeIntervalSince(t0)) s")
    }

    @Test func aFolderOfMillionsOfNamesCostsABoundedAmount() throws {
        let day = tempDir("many").appendingPathComponent("sessions/2026/11/14")
        try FileManager.default.createDirectory(at: day, withIntermediateDirectories: true)
        for i in 0..<(QuotaParse.maxEntries + 200) { FileManager.default.createFile(atPath: day.appendingPathComponent("rollout-\(i)-t.jsonl").path, contents: nil) }
        let home = day.deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let fd = QuotaParse.codexSessionLog(home: home.path, thread: "nothing")
        #expect(fd == nil)  // finished, having read at most maxEntries names of that folder
    }
}

// MARK: Q4 the provider response limit

/// A tiny HTTP server on 127.0.0.1: for each connection it reads the request head and calls `serve` with the socket.
private final class SocketServer: @unchecked Sendable {
    private var fd: Int32 = -1
    private let loop = TestAcceptLoop("SocketServer")
    private(set) var port: UInt16 = 0
    init(_ serve: @escaping @Sendable (Int32) -> Void) {
        fd = socket(AF_INET, SOCK_STREAM, 0)
        var one: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, socklen_t(MemoryLayout<Int32>.size))
        var a = sockaddr_in()
        a.sin_family = sa_family_t(AF_INET); a.sin_addr.s_addr = inet_addr("127.0.0.1")
        _ = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
        listen(fd, 8)
        var l = socklen_t(MemoryLayout<sockaddr_in>.size)
        _ = withUnsafeMutablePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(fd, $0, &l) } }
        port = UInt16(bigEndian: a.sin_port)
        let lfd = fd
        loop.start(listener: lfd) { c in
            var on: Int32 = 1
            setsockopt(c, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))  // a client that goes away is an error, not a signal
            Thread {
                var buf = [UInt8](repeating: 0, count: 4096), got = Data()
                while got.range(of: Data("\r\n\r\n".utf8)) == nil {
                    let n = Sock.read(c, &buf, buf.count)
                    if n <= 0 { break }
                    got.append(buf, count: n)
                }
                serve(c)
                close(c)
            }.start()
        }
    }
    func stop() { loop.stop() }
}

/// What the fake server managed to write, and whether it saw the client close the connection.
private final class Served: @unchecked Sendable {
    private let l = NSLock()
    private var wrote = 0, wasClosed = false
    let done = DispatchSemaphore(value: 0)
    func finish(wrote n: Int, closed: Bool) { l.withLock { wrote = n; wasClosed = closed }; done.signal() }
    var bytes: Int { l.withLock { wrote } }
    var closed: Bool { l.withLock { wasClosed } }
}

@Suite(.serialized) struct QuotaHTTPLimits {
    /// The cut is proven by what was kept and by the server seeing the connection go: at most the limit (one chunk more if
    /// the reader takes chunks) out of a body of three chunks. The cap is small so the test is quick; a cap whose pass
    /// depends on a wall clock measures the machine, not the cut.
    @Test func aBodyLargerThanTheLimitIsCutNotBuffered() async throws {
        let limit = 16 << 10, chunk = 16 << 10
        let served = Served()
        let server = SocketServer { c in
            _ = SocketServer.raw(c, Data("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n".utf8))
            let body = Data(repeating: 0x61, count: chunk)
            var wrote = 0
            for _ in 0..<3 {
                if !SocketServer.raw(c, body) { break }
                wrote += chunk
            }
            var one: UInt8 = 0
            served.finish(wrote: wrote, closed: Sock.read(c, &one, 1) <= 0)  // the client's close, not the rest of the body
        }
        defer { server.stop() }
        let t0 = Date()
        let r = await QuotaHTTP.get(URLRequest(url: URL(string: "http://127.0.0.1:\(server.port)/x")!), limit: limit, deadline: 60)
        let got = try #require(r)
        #expect(got.status == 200 && got.data.count >= limit && got.data.count <= limit + chunk, "\(got.data.count) bytes of a \(3 * chunk)-byte body")
        var sawClose = false  // polled from async code: the server's write loop runs on its own thread
        for _ in 0..<100 where !sawClose { sawClose = served.done.wait(timeout: .now()) == .success; if !sawClose { try await Task.sleep(for: .milliseconds(50)) } }
        #expect(sawClose && served.closed && served.bytes >= limit, "the server did not see the client close the connection (wrote \(served.bytes))")
        // A hang guard only: the byte count and the server's close above are the proof, not this number.
        #expect(Date().timeIntervalSince(t0) < 120, "took \(Date().timeIntervalSince(t0)) s")
    }

    /// The production default (1 MB) on a body of 3 MB: the answer is bounded by the limit, taken in chunks rather than one async step per
    /// byte. The clock is only a hang guard, not the proof (on a busy machine the byte loop this replaces took about a minute).
    @Test func theProductionLimitCutsAThreeMegabyteBody() async throws {
        let limit = 1 << 20, chunk = 64 << 10, body = 3 << 20
        let server = SocketServer { c in
            _ = SocketServer.raw(c, Data("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n".utf8))
            let piece = Data(repeating: 0x61, count: chunk)
            for _ in 0..<(body / chunk) {
                if !SocketServer.raw(c, piece) { break }  // the client cut the transfer
            }
        }
        defer { server.stop() }
        let t0 = Date()
        let r = await QuotaHTTP.get(URLRequest(url: URL(string: "http://127.0.0.1:\(server.port)/x")!))  // the production limit and deadline
        let got = try #require(r)
        #expect(got.status == 200 && got.data.count >= limit && got.data.count <= limit + (64 << 10),
                "\(got.data.count) bytes of a \(body)-byte body")
        #expect(Date().timeIntervalSince(t0) < 120, "the hang guard: took \(Date().timeIntervalSince(t0)) s")
    }

    @Test(.blocking) func aSlowBodyHitsTheDeadlineForTheWholeTransfer() async throws {
        let server = SocketServer { c in
            _ = SocketServer.raw(c, Data("HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n".utf8))
            for _ in 0..<600 { if !SocketServer.raw(c, Data("x".utf8)) { return }; usleep(200_000) }  // a byte every 0.2 s for two minutes: no request timeout would ever fire
        }
        defer { server.stop() }
        let t0 = Date()
        let r = await QuotaHTTP.get(URLRequest(url: URL(string: "http://127.0.0.1:\(server.port)/x")!), limit: 1 << 20, deadline: 1.5)
        #expect(r == nil, "a trickle past the deadline was accepted")
        #expect(Date().timeIntervalSince(t0) < 60, "took \(Date().timeIntervalSince(t0)) s")  // ends by its 1.5 s deadline, far below the 120 s the trickle lasts
    }

    @Test func aRedirectIsNotFollowed() async throws {
        let hits = Hits()
        let target = SocketServer { c in hits.add(); _ = SocketServer.raw(c, Data("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok".utf8)) }
        defer { target.stop() }
        let server = SocketServer { c in _ = SocketServer.raw(c, Data("HTTP/1.1 302 Found\r\nLocation: http://127.0.0.1:\(target.port)/y\r\nContent-Length: 0\r\n\r\n".utf8)) }
        defer { server.stop() }
        let r = await QuotaHTTP.get(URLRequest(url: URL(string: "http://127.0.0.1:\(server.port)/x")!), limit: 1 << 20, deadline: 5)
        #expect(r?.status == 302 && hits.n == 0)
    }

    /// L1: a provider request (the default and the proxy-free session) gets the 302 itself; the second host never sees a request.
    @Test func aProviderRequestDoesNotFollowARedirect() async throws {
        let hits = Hits()
        let target = SocketServer { c in hits.add(); _ = SocketServer.raw(c, Data("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok".utf8)) }
        defer { target.stop() }
        let server = SocketServer { c in _ = SocketServer.raw(c, Data("HTTP/1.1 302 Found\r\nLocation: http://127.0.0.1:\(target.port)/y\r\nContent-Length: 0\r\n\r\n".utf8)) }
        defer { server.stop() }
        var r = URLRequest(url: URL(string: "http://127.0.0.1:\(server.port)/v1/messages")!)
        r.httpMethod = "POST"; r.httpBody = Data("prompt".utf8)
        for s in [Router.defaultSession, Router.noProxySession] {
            let (_, resp) = try await s.data(for: r)
            #expect((resp as? HTTPURLResponse)?.statusCode == 302)
        }
        #expect(hits.n == 0)
    }

    /// L2: an http provider gets a session without any proxy; https keeps the default one.
    @Test func anHttpProviderUsesAProxyFreeSession() {
        let http = Router.session(Router.defaultSession, for: URL(string: "http://litellm.local:4000/v1/messages")!)
        #expect(http === Router.noProxySession)
        #expect((http.configuration.connectionProxyDictionary ?? ["x": 1]).isEmpty)
        #expect(Router.session(Router.defaultSession, for: URL(string: "https://api.example.com/v1")!) === Router.defaultSession)
        #expect(Router.defaultSession.configuration.connectionProxyDictionary == nil)
    }

    final class Hits: @unchecked Sendable { private let l = NSLock(); private var c = 0; func add() { l.withLock { c += 1 } }; var n: Int { l.withLock { c } } }
}

private extension SocketServer {
    static func raw(_ c: Int32, _ d: Data) -> Bool {
        d.withUnsafeBytes { b in
            var off = 0
            while off < b.count { let n = Sock.write(c, b.baseAddress! + off, b.count - off); if n <= 0 { return false }; off += n }
            return true
        }
    }
}

// MARK: Q6 and Q7 the throttle and the cache key

private final class Counted: @unchecked Sendable {
    private let l = NSLock()
    private var n = 0, r: Result<Data, Error>
    init(_ r: Result<Data, Error>) { self.r = r }
    var calls: Int { l.withLock { n } }
    func call() throws -> Data { try l.withLock { n += 1; return try r.get() } }
    func set(_ x: Result<Data, Error>) { l.withLock { r = x } }
}

private final class Requests: @unchecked Sendable {
    private let l = NSLock()
    private var urls: [String] = [], auths: [String] = []
    private var answer: (Int, String) = (200, "")
    func add(_ r: URLRequest) { l.withLock { urls.append(r.url!.absoluteString); auths.append(r.value(forHTTPHeaderField: "Authorization") ?? "") } }
    var count: Int { l.withLock { urls.count } }
    var allURLs: [String] { l.withLock { urls } }
    func set(_ s: Int, _ b: String) { l.withLock { answer = (s, b) } }
    var reply: (Int, String) { l.withLock { answer } }
}

@Suite struct QuotaThrottleAndCache {
    let t0 = QuotaTime.parse("2026-11-14T10:00:00Z")!
    func inputs(probe: Bool = true, providers: [QuotaInputs.Provider] = [], key: @escaping (String) -> String? = { _ in nil }) -> QuotaInputs {
        QuotaInputs(claudeProbe: probe, claudeExe: "/fake/claude", claudeEnabled: true, providers: providers, key: key, tmpParent: tempDir("p"))
    }

    @Test func aFailingProbeIsNotRunAgainAtOnceAndTheBackoffGrows() async {
        let cli = Counted(.failure(QuotaError("no luck")))
        let e = QuotaEngine(runUsage: { _, _ in try cli.call() }, zone: TimeZone(identifier: "UTC")!)
        let i = inputs()
        for k in 0..<5 {
            let v = await e.report(i, now: t0.addingTimeInterval(Double(k) * 5)).filter { $0.name == "claude" }
            #expect(v.count == 1 && v[0].reason == "no luck")
        }
        #expect(cli.calls == 1)  // five reports inside the back-off: one probe
        _ = await e.report(i, now: t0.addingTimeInterval(61)); #expect(cli.calls == 2)
        _ = await e.report(i, now: t0.addingTimeInterval(61 + 90)); #expect(cli.calls == 2)  // the second failure doubled it: 2 minutes
        _ = await e.report(i, now: t0.addingTimeInterval(61 + 130)); #expect(cli.calls == 3)
        cli.set(.success(Data(#"{"result":"Current week: 8% used"}"#.utf8)))
        let ok = await e.report(i, now: t0.addingTimeInterval(3600)).filter { $0.name.hasPrefix("claude") }
        #expect(ok.first?.name == "claude.week" && ok.first?.stale == false)  // a success clears it
    }

    @Test func theReportAfterAPatchRunsNothing() async {
        let cli = Counted(.success(Data(#"{"result":"Current week: 8% used"}"#.utf8)))
        let e = QuotaEngine(runUsage: { _, _ in try cli.call() }, zone: TimeZone(identifier: "UTC")!)
        let rep = await e.report(inputs(), now: t0, fetch: false).filter { $0.name == "claude" }
        #expect(cli.calls == 0 && rep.first?.reason == "not probed yet")
        _ = await e.report(inputs(), now: t0)
        #expect(cli.calls == 1)
        let after = await e.report(inputs(), now: t0.addingTimeInterval(5), fetch: false).filter { $0.name.hasPrefix("claude") }
        #expect(cli.calls == 1 && after.first?.name == "claude.week")  // what is known is shown
    }

    @Test func providerFailuresAreThrottledAndAChangedAddressOrKeyIsAnotherEntry() async {
        let seen = Requests()
        seen.set(200, #"{"info":{"spend":1}}"#)
        let e = QuotaEngine(runUsage: { _, _ in Data() }, http: { r in
            seen.add(r)
            let (st, body) = seen.reply
            return (st, Data((r.url!.host == "b.test" ? #"{"info":{"spend":2}}"# : body).utf8))
        }, zone: TimeZone(identifier: "UTC")!)
        nonisolated(unsafe) var key = "k1"
        func report(_ p: [QuotaInputs.Provider], at: Double) async -> [QuotaValue] { await e.report(inputs(probe: false, providers: p, key: { _ in key }), now: t0.addingTimeInterval(at)) }
        func spend(_ r: [QuotaValue]) -> [String] { r.filter { $0.name == "litellm.spend" }.map { "\($0.value!)\($0.stale ? "!" : "")" } }
        let a = QuotaInputs.Provider(id: "litellm", name: "LiteLLM", baseURL: "http://a.test", enabled: true)
        let b = QuotaInputs.Provider(id: "litellm", name: "LiteLLM", baseURL: "http://b.test", enabled: true)
        #expect(spend(await report([a], at: 0)) == ["1.0"])
        let n = seen.count
        _ = await report([a], at: 30); #expect(seen.count == n)  // cached
        #expect(spend(await report([b], at: 60)) == ["2.0"])  // another address: the first account's credit is not shown
        key = "k2"
        _ = await report([b], at: 90); #expect(seen.count == n + 2)  // another key: a new request, not the old cache
        // two providers with one name and two addresses are two entries
        #expect(spend(await report([a, b], at: 120)).sorted() == ["1.0", "2.0"])
        // failures: one request, then the back-off
        seen.set(500, "no")
        key = "k3"
        let before = seen.count
        for k in 0..<4 { let r = await report([a], at: 200 + Double(k)); #expect(r.contains { $0.name == "litellm" && $0.reason.contains("HTTP 500") }) }
        #expect(seen.count == before + 1)
        // the secret is not in the report
        let text = String(describing: await report([a], at: 210))
        #expect(!text.contains("k3") && !text.contains("Bearer"))
        // a provider that is gone takes its cache with it: coming back asks again
        _ = await report([], at: 220)
        seen.set(200, #"{"info":{"spend":1}}"#)
        let c = seen.count
        _ = await report([a], at: 230)
        #expect(seen.count == c + 1)
    }

    @Test func theCacheKeyHoldsNoKeyText() {
        let p = QuotaInputs.Provider(id: "litellm", name: "LiteLLM", baseURL: "http://a.test", enabled: true)
        let k = QuotaEngine.remoteKey(p, key: "SECRET-KEY-VALUE")
        #expect(!k.contains("SECRET") && k != QuotaEngine.remoteKey(p, key: "other") && k.hasPrefix("litellm\nhttp://a.test\n"))
    }
}
