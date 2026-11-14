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
    private var claudeFlight: Task<[QuotaValue], Never>?
    private var remote: [String: (at: Date, vals: [QuotaValue])] = [:]
    private var remoteFlight: [String: Task<[QuotaValue], Never>] = [:]
    private var jobs = 0

    init(runUsage: @escaping UsageRunner = ClaudeUsageProbe.run, http: @escaping HTTP = QuotaHTTP.get, zone: TimeZone = .current) {
        self.runUsage = runUsage; self.http = http; self.zone = zone
    }

    /// All values, Codex first. Claude and the providers are fetched on demand, at most once per 10 minutes.
    func report(_ i: QuotaInputs, now: Date = Date()) async -> [QuotaValue] {
        var out = quotaMarkStale(codex, now: now)
        if out.isEmpty { out = [QuotaValue(name: "codex", source: "unavailable", stale: true, note: "no Codex run since this gateway started")] }
        out += await claudeQuota(i, now: now)
        for p in i.providers { out += await remoteQuota(i, p, now: now) }
        return out
    }

    // MARK: Codex

    /// The reading of a finished Codex run's session log goes to a task: it never waits and never blocks the model answer. A full queue
    /// drops the job (the next run records newer numbers anyway).
    nonisolated func queueCodex(home: String, thread: String) {
        Task.detached { [self] in await recordCodex(home: home, thread: thread) }
    }

    func recordCodex(home: String, thread: String) async {
        guard jobs < 8 else { return }
        jobs += 1
        defer { jobs -= 1 }
        let vals = await Task.detached { () -> [QuotaValue] in
            guard let p = QuotaParse.codexSessionFile(home: home, thread: thread) else { return [] }
            return QuotaParse.lastRateLimits(path: p, observed: Date())
        }.value
        if !vals.isEmpty { codex = vals }
    }

    // MARK: Claude

    private func claudeQuota(_ i: QuotaInputs, now: Date) async -> [QuotaValue] {
        guard i.claudeProbe else { return [.unavailable("claude", "probe off")] }  // opt-in
        guard let exe = i.claudeExe, i.claudeEnabled else { return [.unavailable("claude", "Claude CLI is off or not installed on this gateway")] }
        if !claudeOff.isEmpty { return [.unavailable("claude", claudeOff)] }
        if let c = claude, now.timeIntervalSince(claudeAt) < Self.cacheFor { return quotaMarkStale(c, now: now) }
        if let f = claudeFlight { return await f.value }  // single-flight: one probe at a time; waiters take the result
        let task = Task { await self.probeClaude(exe, i.tmpParent, now) }
        claudeFlight = task
        let r = await task.value
        claudeFlight = nil
        return r
    }

    private func probeClaude(_ exe: String, _ parent: URL, _ now: Date) async -> [QuotaValue] {
        var vals: [QuotaValue] = [], failure: Error?
        do { vals = try QuotaParse.claudeUsage(try await runUsage(exe, parent), now: now, zone: zone) } catch { failure = error }
        if let failure {
            let msg = (failure as? QuotaError)?.description ?? "\(failure)"
            if msg == QuotaParse.errInference {
                claudeOff = msg
                return [.unavailable("claude", msg)]
            }
            if let c = claude {  // the last good values, marked stale
                return quotaMarkStale(c, now: now).map { var v = $0; v.stale = true; v.note = "could not refresh: " + msg; return v }
            }
            return [.unavailable("claude", msg)]
        }
        claude = vals; claudeAt = now
        return vals
    }

    // MARK: OpenRouter and LiteLLM

    static func providerKind(_ p: QuotaInputs.Provider) -> String? {
        if p.name.caseInsensitiveCompare("openrouter") == .orderedSame || p.baseURL.lowercased().contains("openrouter.ai") { return "openrouter" }
        if p.name.lowercased().contains("litellm") { return "litellm" }
        return nil
    }

    private func remoteQuota(_ i: QuotaInputs, _ p: QuotaInputs.Provider, now: Date) async -> [QuotaValue] {
        guard let kind = Self.providerKind(p), p.enabled else { return [] }
        if let c = remote[p.name], now.timeIntervalSince(c.at) < Self.cacheFor { return quotaMarkStale(c.vals, now: now) }
        if let f = remoteFlight[p.name] { return await f.value }  // single-flight per provider
        let task = Task { await self.fetchRemote(i, p, kind, now) }
        remoteFlight[p.name] = task
        let r = await task.value
        remoteFlight[p.name] = nil
        return r
    }

    private func fetchRemote(_ i: QuotaInputs, _ p: QuotaInputs.Provider, _ kind: String, _ now: Date) async -> [QuotaValue] {
        func fail(_ note: String) -> [QuotaValue] {
            if let c = remote[p.name] { return quotaMarkStale(c.vals, now: now).map { var v = $0; v.stale = true; v.note = note; return v } }
            return [.unavailable(kind, note)]
        }
        guard let key = i.key(p.id), !key.isEmpty else { return fail("no key for " + p.name) }
        var base = p.baseURL
        while base.hasSuffix("/") { base.removeLast() }
        guard let url = URL(string: base + (kind == "openrouter" ? "/v1/credits" : "/key/info")) else { return fail("bad provider address") }
        var req = URLRequest(url: url, timeoutInterval: 10)
        req.setValue("Bearer " + key, forHTTPHeaderField: "Authorization")
        guard let r = await http(req) else { return fail("the provider did not answer") }
        guard r.status == 200 else { return fail("the provider answered HTTP \(r.status)") }
        do {
            let vals = kind == "openrouter" ? try QuotaParse.openRouterCredits(r.data, now: now) : try QuotaParse.liteLLMKeyInfo(r.data, now: now)
            remote[p.name] = (now, vals)
            return vals
        } catch { return fail((error as? QuotaError)?.description ?? "\(error)") }
    }
}

/// The providers' endpoints: GET, 10 seconds, no redirect (a redirect would carry the key elsewhere), at most 1 MB.
enum QuotaHTTP {
    private final class NoRedirect: NSObject, URLSessionTaskDelegate, @unchecked Sendable {
        func urlSession(_ s: URLSession, task: URLSessionTask, willPerformHTTPRedirection r: HTTPURLResponse, newRequest: URLRequest,
                        completionHandler: @escaping (URLRequest?) -> Void) { completionHandler(nil) }
    }
    private static let session = URLSession(configuration: .ephemeral, delegate: NoRedirect(), delegateQueue: nil)

    @Sendable static func get(_ req: URLRequest) async -> (status: Int, data: Data)? {
        guard let (data, resp) = try? await session.data(for: req), let h = resp as? HTTPURLResponse else { return nil }
        return (h.statusCode, data.prefix(1 << 20))
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

    static let args = ["-p", "/usage", "--model", "haiku", "--setting-sources", "", "--strict-mcp-config", "--mcp-config", #"{"mcpServers":{}}"#,
                       "--output-format", "json", "--no-session-persistence", "--tools", ""]

    @Sendable static func run(exe: String, parent: URL) async throws -> Data { try await run(exe: exe, parent: parent, limit: 30) }

    static func run(exe: String, parent: URL, limit: Double) async throws -> Data {
        let fm = FileManager.default
        try fm.createDirectory(at: parent, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let dir = parent.appendingPathComponent("probe-" + UUID().uuidString.prefix(8), isDirectory: true)
        try fm.createDirectory(at: dir, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        defer { try? fm.removeItem(at: dir) }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = args
        p.currentDirectoryURL = dir
        p.environment = env(ProcessInfo.processInfo.environment, tmp: dir.path)
        let out = Pipe()
        p.standardOutput = out; p.standardError = FileHandle.nullDevice; p.standardInput = FileHandle.nullDevice
        let timedOut = Flag(), reader = Reader(out.fileHandleForReading)
        let status: Int32 = try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Int32, Error>) in
            p.terminationHandler = { cont.resume(returning: $0.terminationStatus) }
            do { try p.run() } catch { cont.resume(throwing: QuotaError("claude /usage failed: \(error.localizedDescription)")); return }
            reader.start()  // concurrently: a long output must not block the process before the limit
            let pid = p.processIdentifier
            Task.detached {  // 30 s at most; the process is ours alone (it has no children it needs)
                try? await Task.sleep(nanoseconds: UInt64(limit * 1e9))
                guard p.isRunning else { return }
                timedOut.set()
                p.terminate()
                try? await Task.sleep(nanoseconds: 2_000_000_000)
                if p.isRunning { kill(pid, SIGKILL) }
            }
        }
        let data = reader.wait()
        guard !timedOut.value else { throw QuotaError("claude /usage failed: timed out after \(Int(limit)) s") }
        guard status == 0 else { throw QuotaError("claude /usage failed: " + (p.terminationReason == .uncaughtSignal ? "signal \(status)" : "exit status \(status)")) }
        return data.prefix(1 << 20)
    }

    /// Reads a pipe to its end on its own thread, keeping at most 1 MB.
    private final class Reader: @unchecked Sendable {
        private let handle: FileHandle, done = DispatchSemaphore(value: 0)
        private var data = Data()
        init(_ h: FileHandle) { handle = h }
        func start() {
            Thread {
                while let c = try? self.handle.read(upToCount: 64 << 10), !c.isEmpty { if self.data.count < 1 << 20 { self.data.append(c) } }
                self.done.signal()
            }.start()
        }
        func wait() -> Data { done.wait(); return data }
    }

    private final class Flag: @unchecked Sendable {
        private let lock = NSLock(); private var on = false
        func set() { lock.withLock { on = true } }
        var value: Bool { lock.withLock { on } }
    }
}
