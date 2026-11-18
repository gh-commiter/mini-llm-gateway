import CryptoKit
import Foundation

// The quota report of the gateway (client/serve_quota.go: quotaReport, claudeQuota, remoteQuota, recordCodexQuota). The data lock is never held
// across a slow call (the probe, an HTTP request, a file read); the slow refreshes are single-flight per source; a stuck probe never delays a
// model answer or the recording of Codex numbers (recording runs in its own task).

/// What a report needs from the gateway's configuration (the engine itself knows nothing of the app).
struct QuotaInputs {
    struct Provider { var id: String, name: String, baseURL: String, enabled: Bool }
    var claudeProbe: Bool
    var claudeExe: String?     // nil = not installed
    var claudeEnabled: Bool
    var providers: [Provider]
    var key: (String) -> String?  // by provider id; never logged, never returned
    var tmpParent: URL         // a private folder of the app (the probe's empty working folders are made under it)
}

actor QuotaEngine {
    static let cacheFor: TimeInterval = 10 * 60

    /// Runs `claude -p /usage`; tests replace it (no real CLI in tests).
    typealias UsageRunner = @Sendable (_ exe: String, _ parent: URL) async throws -> Data
    /// One GET with a bearer key; (status, body) or nil when the provider did not answer. Tests replace it.
    typealias HTTP = @Sendable (URLRequest) async -> (status: Int, data: Data)?

    private let runUsage: UsageRunner
    private let http: HTTP
    private let zone: TimeZone
    private var codex: [QuotaValue] = []
    private var claude: [QuotaValue]?
    private var claudeAt = Date.distantPast
    private var claudeOff = ""   // set when `claude /usage` turned out to do more than it should: not run again
    private var claudeBad = FailState()  // a failed probe is not run again before the back-off is over
    private var claudeFlight: Task<[QuotaValue], Never>?
    private var remote: [String: (at: Date, vals: [QuotaValue])] = [:]  // by remoteKey
    private var remoteBad: [String: FailState] = [:]
    private var remoteFlight: [String: Task<[QuotaValue], Never>] = [:]
    private var jobs = 0
    private var identities: [String: String] = [:]  // provider -> cache identity last recorded (memory only)
    typealias Record = @Sendable ([String: Any], Date) -> Void
    private func persist(_ provider: String, _ identity: String, _ now: Date, _ vals: [QuotaValue], _ kind: String, _ record: Record) {
        // The core assigns a stable random scope per provider; the identity only tells it that the credential or address changed during this run.
        var rescope = false
        if !identity.isEmpty {
            rescope = identities[provider].map { $0 != identity } ?? false
            identities[provider] = identity
        }
        record(Self.probeJSON(provider: provider, rescope: rescope, now: now, vals: vals, kind: kind), now)
    }
    static func probeJSON(provider: String, rescope: Bool = false, now: Date, vals: [QuotaValue], kind: String) -> [String: Any] {
        let values: [[String: Any]] = kind.isEmpty ? vals.compactMap { v in
            guard let value = v.value, let observed = v.observedAt else { return nil }
            var o: [String: Any] = ["name": v.name, "unit": v.unit, "value": value, "observedAt": QuotaTime.format(observed), "source": v.source]
            if let reset = v.resetsAt { o["resetsAt"] = QuotaTime.format(reset) }
            return o
        } : []
        var o: [String: Any] = ["id": "qp_" + UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased(), "provider": provider,
                                "probedAt": QuotaTime.format(now), "status": kind.isEmpty ? "ok" : "error", "values": values]
        if !kind.isEmpty { o["errorKind"] = kind }
        if rescope { o["rescope"] = true }
        return o
    }

    init(runUsage: @escaping UsageRunner = ClaudeUsageProbe.run, http: @escaping HTTP = QuotaHTTP.get, zone: TimeZone = .current) {
        self.runUsage = runUsage; self.http = http; self.zone = zone
    }

    /// A failed refresh is not tried again at once (the 10-minute throttle holds for failures too): after 1 minute, then doubling up to the cache time.
    struct FailState {
        var n = 0, msg = "", until = Date.distantPast
        mutating func fail(_ now: Date, _ m: String) {
            n += 1; msg = m
            until = now.addingTimeInterval(min(QuotaEngine.cacheFor, 60 * Double(1 << min(n - 1, 4))))
        }
        func waiting(_ now: Date) -> Bool { n > 0 && now < until }
    }

    /// All values, Codex first. Claude and the providers are fetched on demand, at most once per 10 minutes. fetch false reports what is known and
    /// starts no probe and no request (the answer of the settings change that turns the probe on). Entries of providers that are gone, or whose address or
    /// key changed, are dropped.
    func report(_ i: QuotaInputs, now: Date = Date(), fetch: Bool = true, record: @escaping Record = { _, _ in }) async -> [QuotaValue] {
        var out = quotaMarkStale(codex, now: now)
        if out.isEmpty { out = [QuotaValue(name: "codex", source: "unavailable", stale: true, note: "no Codex run since this gateway started")] }
        out += await claudeQuota(i, now: now, fetch: fetch, record: record)
        var live = Set<String>()
        for p in i.providers {
            let (vals, key) = await remoteQuota(i, p, now: now, fetch: fetch, record: record)
            out += vals
            if let key { live.insert(key) }
        }
        remote = remote.filter { live.contains($0.key) }
        remoteBad = remoteBad.filter { live.contains($0.key) }
        return out
    }

    // MARK: Codex

    /// The reading of a finished Codex run's session log goes to a task: it never waits and never blocks the model answer. A full queue
    /// drops the job (the next run records newer numbers anyway).
    nonisolated func queueCodex(home: String, thread: String, record: @escaping Record = { _, _ in }) {
        Task.detached { [self] in await recordCodex(home: home, thread: thread, record: record) }
    }

    func recordCodex(home: String, thread: String, record: @escaping Record = { _, _ in }) async {
        guard jobs < 8 else { return }
        jobs += 1
        defer { jobs -= 1 }
        let now = Date()
        let vals = await Task.detached { () -> [QuotaValue] in
            QuotaParse.lastRateLimits(home: home, thread: thread, observed: now)
        }.value
        persist("codex", "", now, vals, vals.isEmpty ? "unavailable" : "", record)
        if !vals.isEmpty { codex = vals }
    }

    // MARK: Claude

    private func claudeQuota(_ i: QuotaInputs, now: Date, fetch: Bool, record: @escaping Record) async -> [QuotaValue] {
        guard i.claudeProbe else { return [.unavailable("claude", "probe off")] }  // opt-in
        guard let exe = i.claudeExe, i.claudeEnabled else { return [.unavailable("claude", "Claude CLI is off or not installed on this gateway")] }
        if !claudeOff.isEmpty { return [.unavailable("claude", claudeOff)] }
        if let c = claude, now.timeIntervalSince(claudeAt) < Self.cacheFor { return quotaMarkStale(c, now: now) }
        if claudeBad.waiting(now) { return claudeFailed(now) }  // a failed probe is not repeated at once
        if let f = claudeFlight { return await f.value }  // single-flight: one probe at a time; waiters take the result
        guard fetch else { return [.unavailable("claude", "not probed yet")] }
        let task = Task { await self.probeClaude(exe, i.tmpParent, now, record) }
        claudeFlight = task
        let r = await task.value
        claudeFlight = nil
        return r
    }

    /// The last good values marked stale with the reason, or "unavailable".
    private func claudeFailed(_ now: Date) -> [QuotaValue] {
        if let c = claude {
            return quotaMarkStale(c, now: now).map { var v = $0; v.stale = true; v.note = "could not refresh: " + claudeBad.msg; return v }
        }
        return [.unavailable("claude", claudeBad.msg)]
    }

    private func probeClaude(_ exe: String, _ parent: URL, _ now: Date, _ record: Record) async -> [QuotaValue] {
        var vals: [QuotaValue] = [], failure: Error?
        do { vals = try QuotaParse.claudeUsage(try await runUsage(exe, parent), now: now, zone: zone) } catch { failure = error }
        persist("claude", "", now, vals, failure == nil ? "" : (failure as? QuotaError)?.description == QuotaParse.errInference ? "unsafe" : "probe", record)
        if let failure {
            let msg = (failure as? QuotaError)?.description ?? "\(failure)"
            if msg == QuotaParse.errInference {
                claudeOff = msg
                return [.unavailable("claude", msg)]
            }
            claudeBad.fail(now, msg)
            return claudeFailed(now)
        }
        claude = vals; claudeAt = now; claudeBad = FailState()
        return vals
    }

    // MARK: OpenRouter and LiteLLM

    static func providerKind(_ p: QuotaInputs.Provider) -> String? {
        if p.name.caseInsensitiveCompare("openrouter") == .orderedSame || p.baseURL.lowercased().contains("openrouter.ai") { return "openrouter" }
        if p.name.lowercased().contains("litellm") { return "litellm" }
        return nil
    }

    /// The cache key of one provider: its id, where it is asked and a digest of the credential (kept in memory only, never logged or
    /// returned). A changed address or key is another entry.
    static func remoteKey(_ p: QuotaInputs.Provider, key: String) -> String {
        p.id + "\n" + p.baseURL + "\n" + SHA256.hash(data: Data(key.utf8)).prefix(8).map { String(format: "%02x", $0) }.joined()
    }

    /// The values and the cache key used (nil = this provider has no quota).
    private func remoteQuota(_ i: QuotaInputs, _ p: QuotaInputs.Provider, now: Date, fetch: Bool, record: @escaping Record) async -> ([QuotaValue], String?) {
        guard let kind = Self.providerKind(p), p.enabled else { return ([], nil) }
        let key = i.key(p.id) ?? ""
        let ck = Self.remoteKey(p, key: key)
        func failed(_ note: String) -> [QuotaValue] {
            if let c = remote[ck] { return quotaMarkStale(c.vals, now: now).map { var v = $0; v.stale = true; v.note = note; return v } }
            return [.unavailable(kind, note)]
        }
        if let c = remote[ck], now.timeIntervalSince(c.at) < Self.cacheFor { return (quotaMarkStale(c.vals, now: now), ck) }
        if let bad = remoteBad[ck], bad.waiting(now) { return (failed(bad.msg), ck) }  // a failed request is not repeated at once
        if key.isEmpty { return (failed("no key for " + p.name), ck) }
        if let f = remoteFlight[ck] { return (await f.value, ck) }  // single-flight per provider
        guard fetch else { return ([.unavailable(kind, "not fetched yet")], ck) }
        let task = Task { await self.fetchRemote(p, kind, key, ck, now, record) }
        remoteFlight[ck] = task
        let r = await task.value
        remoteFlight[ck] = nil
        return (r, ck)
    }

    private func fetchRemote(_ p: QuotaInputs.Provider, _ kind: String, _ key: String, _ ck: String, _ now: Date, _ record: Record) async -> [QuotaValue] {
        var errorKind = "unreachable"
        func fail(_ note: String) -> [QuotaValue] {
            persist(p.id, ck, now, [], errorKind, record)
            var b = remoteBad[ck] ?? FailState()
            b.fail(now, note)
            remoteBad[ck] = b
            if let c = remote[ck] { return quotaMarkStale(c.vals, now: now).map { var v = $0; v.stale = true; v.note = note; return v } }
            return [.unavailable(kind, note)]
        }
        var base = p.baseURL
        while base.hasSuffix("/") { base.removeLast() }
        guard let url = URL(string: base + (kind == "openrouter" ? "/v1/credits" : "/key/info")) else { errorKind = "invalid"; return fail("bad provider address") }
        var req = URLRequest(url: url, timeoutInterval: 10)
        req.setValue("Bearer " + key, forHTTPHeaderField: "Authorization")
        guard let r = await http(req) else { return fail("the provider did not answer") }
        guard r.status == 200 else { errorKind = "http"; return fail("the provider answered HTTP \(r.status)") }
        do {
            let vals = kind == "openrouter" ? try QuotaParse.openRouterCredits(r.data, now: now) : try QuotaParse.liteLLMKeyInfo(r.data, now: now)
            persist(p.id, ck, now, vals, "", record)
            remote[ck] = (now, vals); remoteBad[ck] = nil
            return vals
        } catch { errorKind = "invalid"; return fail((error as? QuotaError)?.description ?? "\(error)") }
    }
}

/// The providers' endpoints: GET, no redirect (a redirect would carry the key elsewhere), the whole transfer within 10 seconds (the resource timeout
/// covers the body, not only the first byte) and at most 1 MB: the body is taken a chunk per delegate call and the transfer is cancelled at the limit,
/// nothing larger is buffered.
enum QuotaHTTP {
    private typealias Answer = (status: Int, data: Data)?

    /// Takes the body in the chunks URLSession delivers (one async step per byte would cost a million steps for a 1 MB answer) and cancels the
    /// task as soon as the limit is reached, so the rest of the body is never read.
    private final class Body: NSObject, URLSessionDataDelegate, @unchecked Sendable {
        private let limit: Int
        private let lock = NSLock()
        private var data = Data()
        private var cut = false  // the limit was reached and the task cancelled: what was kept is the answer
        private var continuation: CheckedContinuation<Answer, Never>?

        init(_ limit: Int) { self.limit = limit }

        func urlSession(_ s: URLSession, task: URLSessionTask, willPerformHTTPRedirection r: HTTPURLResponse, newRequest: URLRequest,
                        completionHandler: @escaping (URLRequest?) -> Void) { completionHandler(nil) }

        func urlSession(_ s: URLSession, dataTask: URLSessionDataTask, didReceive d: Data) {
            let full: Bool = lock.withLock { () -> Bool in
                guard !cut else { return true }
                data.append(d.prefix(max(0, limit - data.count)))  // exactly the limit, as the byte loop kept
                if data.count >= limit { cut = true; return true }
                return false
            }
            if full { dataTask.cancel() }
        }

        func urlSession(_ s: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
            let (cont, answer) = lock.withLock { () -> (CheckedContinuation<Answer, Never>?, Answer) in
                let c = continuation
                continuation = nil
                guard let h = task.response as? HTTPURLResponse, cut || error == nil else { return (c, nil) }  // the deadline, or the connection broke
                return (c, (h.statusCode, data))
            }
            cont?.resume(returning: answer)
        }

        func start(_ c: CheckedContinuation<Answer, Never>, _ task: URLSessionDataTask) {
            lock.withLock { continuation = c }
            task.resume()
        }
    }

    @Sendable static func get(_ req: URLRequest) async -> (status: Int, data: Data)? { await get(req, limit: 1 << 20, deadline: 10) }

    static func get(_ req: URLRequest, limit: Int, deadline: TimeInterval) async -> (status: Int, data: Data)? {
        let cfg = URLSessionConfiguration.ephemeral
        cfg.timeoutIntervalForRequest = deadline
        cfg.timeoutIntervalForResource = deadline
        if req.url?.scheme?.lowercased() == "http" { cfg.connectionProxyDictionary = [:] }  // L2: an http provider's key never goes to a proxy
        let body = Body(limit)
        let session = URLSession(configuration: cfg, delegate: body, delegateQueue: nil)
        defer { session.invalidateAndCancel() }  // also ends a transfer that was cut at the limit
        return await withCheckedContinuation { c in body.start(c, session.dataTask(with: req)) }
    }
}

/// `claude -p /usage` with nothing around it: an EMPTY private working folder (under the app's own folder, never a client folder or Scratch),
/// no tools, no settings sources, no MCP servers, no saved session, a 30 s limit, and an environment that is an allowlist. The envelope is
/// checked on every run (QuotaParse.claudeUsage); a run that did inference switches the feature off until the gateway restarts.
enum ClaudeUsageProbe {
    /// The only variables the probe gets; each one is what the CLI needs to start and find its own login. Nothing that routes requests
    /// elsewhere (ANTHROPIC_*), nothing that selects settings or behaviour (CLAUDE_CODE_*, except the login token).
    static let envAllow: [String: String] = [
        "PATH": "find the claude program's own helpers",
        "HOME": "the user's folder, where the login (~/.claude) is",
        "XDG_CONFIG_HOME": "where ~/.claude may live",
        "CLAUDE_CONFIG_DIR": "the login folder, when the user moved it",
        "CLAUDE_CODE_OAUTH_TOKEN": "the login by token (claude setup-token)",
        "LANG": "text encoding", "LC_ALL": "text encoding",
        "TZ": "the time zone the reset times are printed in",
    ]

    static func env(_ env: [String: String], tmp: String) -> [String: String] {
        var out = env.filter { envAllow[$0.key] != nil }
        out["PATH"] = (out["PATH"].map { $0 + ":" } ?? "") + "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"  // a script's node, as the agent runs have it
        for k in ["TMPDIR", "TEMP", "TMP"] { out[k] = tmp }
        return out
    }

    static let args = ["-p", "/usage", "--model", "haiku", "--tools", "", "--setting-sources", "", "--strict-mcp-config", "--mcp-config", #"{"mcpServers":{}}"#,
                       "--output-format", "json", "--no-session-persistence"]

    @Sendable static func run(exe: String, parent: URL) async throws -> Data { try await run(exe: exe, parent: parent, limit: 30, drain: 2) }

    /// Starts claude with Native.spawnGroup: its own process group (the whole tree can be ended), stdin /dev/null, no inherited descriptor
    /// (POSIX_SPAWN_CLOEXEC_DEFAULT: only the two pipes it is given). A thread of its own supervises it, so no cooperative thread ever waits. On the
    /// limit, or when a descendant keeps the output open after claude has ended, the group and every holder of the pipes are killed, so the result is
    /// bounded by limit + drain whatever the tree does.
    static func run(exe: String, parent: URL, limit: Double, drain: Double = 2, environment: [String: String] = ProcessInfo.processInfo.environment) async throws -> Data {
        let fm = FileManager.default
        try fm.createDirectory(at: parent, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let dir = parent.appendingPathComponent("probe-" + UUID().uuidString.prefix(8), isDirectory: true)
        try fm.createDirectory(at: dir, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        defer { try? fm.removeItem(at: dir) }
        guard let child = Native.spawnGroup(exe, args, dir: dir, env: env(environment, tmp: dir.path)) else {
            throw QuotaError("claude /usage failed: cannot start " + exe)
        }
        return try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Data, Error>) in
            Thread { cont.resume(with: Result { try supervise(child.pid, child.out, child.err, limit: limit, drain: drain) }) }.start()
        }
    }

    private final class Drain: @unchecked Sendable {
        let done = DispatchSemaphore(value: 0)
        private let lock = NSLock()
        private var buf = Data()
        let keep: Bool
        init(_ h: FileHandle, keep: Bool) {
            self.keep = keep
            Thread {
                while let c = try? h.read(upToCount: 64 << 10), !c.isEmpty { if keep { self.lock.withLock { if self.buf.count < 1 << 20 { self.buf.append(c) } } } }
                self.done.signal()
            }.start()
        }
        var data: Data { lock.withLock { buf } }
        func finished(within s: Double) -> Bool { done.wait(timeout: .now() + s) == .success }
    }

    private static func supervise(_ pid: pid_t, _ out: FileHandle, _ err: FileHandle, limit: Double, drain: Double) throws -> Data {
        let stdout = Drain(out, keep: true), stderr = Drain(err, keep: false)  // the handles stay alive with their threads
        var status: Int32 = 0, exited = false, timedOut = false
        let end = Date().addingTimeInterval(limit)
        while !exited {
            let r = waitpid(pid, &status, WNOHANG)
            if r == pid { exited = true } else if r < 0 && errno != EINTR { exited = true; status = 0 }
            else if Date() >= end { timedOut = true; break }
            else { usleep(10_000) }
        }
        if timedOut {  // the whole tree and every holder of the pipes
            Native.killTree(pid, pipes: [out, err], timeout: 2)
            while waitpid(pid, &status, 0) == -1 && errno == EINTR {}
        } else if !stdout.finished(within: drain) {  // claude has ended, a descendant still holds the output
            Native.killTree(pid, pipes: [out, err], timeout: 2)
        }
        _ = stdout.finished(within: 2); _ = stderr.finished(within: 0.2)  // EOF follows the kill; a holder that cannot be killed is given up on, its thread stays
        if timedOut { throw QuotaError("claude /usage failed: timed out after \(Int(limit)) s") }
        guard status & 0x7f == 0 else { throw QuotaError("claude /usage failed: signal \(status & 0x7f)") }
        guard (status >> 8) & 0xff == 0 else { throw QuotaError("claude /usage failed: exit status \((status >> 8) & 0xff)") }
        return stdout.data
    }
}
