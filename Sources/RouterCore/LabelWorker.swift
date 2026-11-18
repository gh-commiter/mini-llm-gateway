import CryptoKit
import Foundation

/// A user's Apple Foundation Models request is running (Router.runFM): the label worker never starts a sample then, and cancels the one it
/// is running (the on-device model serves one session at a time; the user wins).
final class FMForeground: @unchecked Sendable {
    static let shared = FMForeground()
    private let lock = NSLock()
    private var count = 0
    private var next = 0
    private var watchers: [Int: @Sendable () -> Void] = [:]

    var active: Bool { lock.withLock { count > 0 } }
    /// Tests only: runs right after a sample was registered and before its task exists (the window a user request can hit).
    nonisolated(unsafe) var afterRegister: (@Sendable () -> Void)?

    /// A user request starts: every watcher (a running sample) is told. Pair with `leave()`.
    func enter() {
        let fire = lock.withLock { () -> [@Sendable () -> Void] in count += 1; return Array(watchers.values) }
        fire.forEach { $0() }
    }

    func leave() { lock.withLock { count = max(0, count - 1) } }

    /// Registers a watcher unless a user request is running now (nil); registered and checked under one lock, so a request cannot slip in between.
    func watch(_ onEnter: @escaping @Sendable () -> Void) -> Int? {
        lock.withLock {
            guard count == 0 else { return nil }
            next += 1
            watchers[next] = onEnter
            return next
        }
    }

    func unwatch(_ id: Int) { lock.withLock { _ = watchers.removeValue(forKey: id) } }
}

/// Usage labels filled by the on-device model (docs/plans/brief-decide-labels.md, B3). Beside `UsageSender`: after a request was dispatched
/// the router hands over a bounded copy of its first prompt; one decision at a time, in the background, decides phase and activity with
/// `Decide` over Apple Foundation Models and sends `event-labels` (or, for a shadow sample, `label-agreement`) to the core helper.
///
/// Privacy: the classified text lives only in memory here (the queue, the cache key is a hash). Nothing of it reaches a log, an error, a
/// helper call or an answer; this file has no logging at all and every failure is a fixed class.
final class LabelWorker: @unchecked Sendable {
    struct Settings: Equatable, Sendable {
        var enabled = false, shadow = true
        var backend = ""
        var threshold = 1.0
        var maxPerHour = 120, shadowRate = 4
        var activities: [String] = []
        var revision: UInt64 = 0
    }
    struct Job {
        var eventID: String, input: String, key: String
        var phase: String, activity: String  // explicit labels (normalized), for the shadow comparison
        var revision: UInt64
        var shadow: Bool
        var retries = 0
    }
    typealias Call = @Sendable (_ name: String, _ body: [String: Any]) async -> (status: Int, body: Data)

    static let queueLimit = 8
    static let cacheLimit = 500
    static let phases: [Decide.Candidate] = [
        .init(id: "plan", criteria: "Plan work before execution."), .init(id: "do", criteria: "Implement or execute the planned work."),
        .init(id: "check", criteria: "Review or verify work and its results."),
        .init(id: "act", criteria: "Correct the plan after a finding during execution or verification.")]
    static let criteria: [String: String] = [
        "feature": "Add new behavior.", "bugfix": "Fix incorrect behavior.", "security": "Protect access, secrets or trust boundaries.",
        "refactor": "Restructure code without changing behavior.", "docs": "Write or update documentation.",
        "test": "Create or run tests.", "release": "Prepare or publish a release.", "research": "Investigate a question.", "ops": "Operate or maintain infrastructure."]

    private let rawCall: Call
    private let available: @Sendable () -> Bool
    private let backend: Decide.Backend
    private let foreground: FMForeground
    private let settingsTTL: Duration
    private let budget: Duration
    private let retryDelays: [Duration]
    private let beforeStore: @Sendable () async -> Void  // the usage events queued so far reached the helper (the root event must exist)
    private let waitStep: Duration

    private let lock = NSCondition()
    private var inflight = 0
    private var tracked: [UUID: Task<Void, Never>] = [:]
    private var settings: Settings?
    private var settingsAt = ContinuousClock.now
    private var fetching = false
    private var closed = false
    private var queue: [Job] = []
    private var preparing = 0
    private var running = false
    private var sampled = 0
    private var cancelCurrent: (@Sendable () -> Void)?
    private var cache: [String: (results: [Decide.Result], at: Date)] = [:]
    private var cacheRevision: UInt64 = 0
    private var starts: [Date] = []
    private var sampleBusy = false  // one on-device sample at a time, whether the worker or an evaluation asks (tryTakeSlot)
    private var evaluating = false
    private var errorRevision: UInt64?
    private var errorShown = false
    private var _skipped = 0, _failed = 0, _done = 0, _samples = 0

    var skipped: Int { lock.withLock { _skipped } }
    var failed: Int { lock.withLock { _failed } }
    var done: Int { lock.withLock { _done } }
    var samplesStarted: Int { lock.withLock { _samples } }
    var queued: Int { lock.withLock { queue.count + preparing } }
    var isIdle: Bool { lock.withLock { queue.isEmpty && preparing == 0 && !running } }

    private static let liveLock = NSLock()
    nonisolated(unsafe) private static var live: [Weak] = []
    private struct Weak { weak var worker: LabelWorker? }

    init(call: @escaping Call, available: @escaping @Sendable () -> Bool, backend: @escaping Decide.Backend,
         foreground: FMForeground = .shared, settingsTTL: Duration = .seconds(10), budget: Duration = .seconds(20),
         retryDelays: [Duration] = [.milliseconds(300), .seconds(1), .seconds(3)], waitStep: Duration = .milliseconds(25),
         beforeStore: @escaping @Sendable () async -> Void = {}) {
        self.rawCall = call; self.available = available; self.backend = backend; self.foreground = foreground
        self.settingsTTL = settingsTTL; self.budget = budget; self.retryDelays = retryDelays; self.waitStep = waitStep; self.beforeStore = beforeStore
        Self.liveLock.withLock { Self.live = Self.live.filter { $0.worker != nil } + [Weak(worker: self)] }
    }

    deinit { stop() }

    /// The app is quitting (UsageLoss.persistPendingAtQuit): no work after this.
    static func stopAll() {
        let all = liveLock.withLock { live.compactMap(\.worker) }
        all.forEach { $0.stop() }
    }

    /// Clean stop: the queue is dropped, the running decision is cancelled, nothing is started or stored afterwards.
    func stop() {
        let (cancel, tasks) = lock.withLock { () -> ((@Sendable () -> Void)?, [Task<Void, Never>]) in
            closed = true; queue.removeAll(); cache.removeAll()
            return (cancelCurrent, Array(tracked.values))
        }
        cancel?()
        tasks.forEach { $0.cancel() }
        // `closed` is checked under the same lock before every helper call starts, so only calls already on their way remain: wait for them,
        // a few seconds at most, then abandon (nothing new is called either way).
        lock.lock()
        let end = Date().addingTimeInterval(3)
        while inflight > 0, lock.wait(until: end) {}
        lock.unlock()
    }

    /// Every helper call goes through here: once stopped, none starts (status 0, no body).
    private func call(_ name: String, _ body: [String: Any]) async -> (status: Int, body: Data) {
        guard lock.withLock({ () -> Bool in if closed { return false }; inflight += 1; return true }) else { return (0, Data()) }
        defer { lock.withLock { inflight -= 1; lock.broadcast() } }
        return await rawCall(name, body)
    }

    /// Background work of the worker, tracked so that `stop()` cancels it.
    private func spawn(_ work: @escaping @Sendable () async -> Void) {
        lock.withLock {
            guard !closed else { return }
            let id = UUID()
            tracked[id] = Task.detached(priority: .utility) { [self] in
                await work()
                lock.withLock { _ = tracked.removeValue(forKey: id) }
            }
        }
    }

    // MARK: admission (the request path: synchronous, never waits)

    /// `body` is the request's JSON body (a reference, not a copy); a detached task extracts the capped first prompt and drops it.
    func enqueue(_ e: UsageEvent, body: Data?) {
        guard let body, e.parentId.isEmpty else { return }
        let phase = Self.safeWord(e.phase), activity = Self.safeWord(e.activity)
        let (admitted, refresh) = lock.withLock { () -> (Job?, Bool) in
            guard !closed else { return (nil, false) }
            let stale = !fetching && (settings == nil || ContinuousClock.now - settingsAt >= settingsTTL)
            if stale { fetching = true }
            guard let s = settings, s.enabled, s.backend == "apple-fm" else { return (nil, stale) }
            if queue.count + preparing >= Self.queueLimit { _skipped += 1; return (nil, stale) }
            var shadow = false
            if !phase.isEmpty || !activity.isEmpty {
                sampled += 1
                shadow = s.shadow && s.shadowRate > 0 && sampled % s.shadowRate == 0
                if !shadow && !phase.isEmpty && !activity.isEmpty { return (nil, stale) }
            }
            preparing += 1
            return (Job(eventID: e.id, input: "", key: "", phase: phase, activity: activity, revision: s.revision, shadow: shadow), stale)
        }
        if refresh { spawn { [self] in await fetchSettings() } }
        guard var job = admitted else { return }
        let (device, task) = (e.deviceId, e.task)
        spawn { [self] in
            (job.input, job.key) = Self.input(body: body, device: device, task: task)
            let start = lock.withLock { () -> Bool in
                preparing -= 1
                if closed { return false }
                if job.input.isEmpty { _skipped += 1; return false }
                queue.append(job)
                if running { return false }
                running = true
                return true
            }
            if start { await runQueue() }
        }
    }

    // MARK: input (port of client/serve_labels.go labelInput)

    static func safeWord(_ s: String) -> String {
        let w = s.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        return Decide.isWord(w) ? w : ""
    }

    static func safeTask(_ s: String) -> String {
        let t = s.trimmingCharacters(in: .whitespacesAndNewlines)
        let u = Array(t.utf8)
        let ok = (1...64).contains(u.count) && u.allSatisfy { ($0 >= 48 && $0 <= 57) || ($0 >= 65 && $0 <= 90) || ($0 >= 97 && $0 <= 122) || [46, 95, 35, 47, 45].contains($0) }
        return ok ? t : ""
    }

    /// The first user message (system reminders stripped), with the task label in front, capped at 2000 code points; the key is the SHA-256 of the
    /// whole first message with device and task (never stored). ("", "") when there is no user text.
    static func input(body: Data, device: String, task: String) -> (String, String) {
        guard let o = try? JSONSerialization.jsonObject(with: body) as? [String: Any], let messages = o["messages"] as? [[String: Any]] else { return ("", "") }
        for m in messages where m["role"] as? String == "user" {
            var text = ""
            if let s = m["content"] as? String { text = s }
            else if let blocks = m["content"] as? [[String: Any]] {
                text = blocks.compactMap { $0["type"] as? String == "text" ? $0["text"] as? String : nil }.joined(separator: "\n")
            } else { return ("", "") }
            text = AppleFM.stripReminders(text)
            let key = SHA256.hash(data: Data((device + "\0" + text + "\0" + task).utf8)).map { String(format: "%02x", $0) }.joined()
            let t = safeTask(task)
            return (Decide.capInput((t.isEmpty ? "" : "task: " + t + "\n") + text), key)
        }
        return ("", "")
    }

    // MARK: settings

    /// The settings from the core (a failure keeps the old ones). A changed revision cancels the decision in flight.
    func fetchSettings() async {
        let r = await call("label-settings", [:])
        let parsed = r.status == 200 ? Self.parse(r.body) : nil
        let cancel = lock.withLock { () -> (@Sendable () -> Void)? in
            fetching = false
            guard let parsed else { return nil }
            settingsAt = .now
            let changed = settings?.revision != parsed.revision || settings?.enabled != parsed.enabled || settings?.backend != parsed.backend
            settings = parsed
            return changed ? cancelCurrent : nil
        }
        cancel?()
        // an enabled worker whose stored backend this Mac cannot run (written by an older version or a restore): say so once per revision
        if let p = parsed, p.enabled, !p.backend.isEmpty, p.backend != "apple-fm",
           lock.withLock({ defer { errorRevision = p.revision }; return errorRevision != p.revision }) {
            _ = await call("label-error", ["message": "backend not available on this Mac: choose apple-fm"])
            lock.withLock { errorShown = true }
        }
    }

    static func parse(_ body: Data) -> Settings? {
        guard let o = try? JSONSerialization.jsonObject(with: body) as? [String: Any] else { return nil }
        func int(_ v: Any?) -> Int? { (v as? Int) ?? (v as? String).flatMap { Int($0) } }
        var s = Settings()
        s.enabled = o["enabled"] as? Bool ?? false
        s.backend = o["backend"] as? String ?? ""
        s.threshold = (o["threshold"] as? String) == "0.67" ? 0.67 : 1.0
        s.maxPerHour = int(o["maxPerHour"]) ?? 120
        s.shadow = o["shadow"] as? Bool ?? true
        s.shadowRate = int(o["shadowRate"]) ?? 4
        s.activities = (o["activities"] as? [String] ?? []).filter(Decide.isWord)
        s.revision = (o["revision"] as? NSNumber)?.uint64Value ?? 0
        return s
    }

    // MARK: the queue

    private func runQueue() async {
        while true {
            let job = lock.withLock { () -> Job? in
                guard !closed, !queue.isEmpty else { running = false; return nil }
                return queue.removeFirst()
            }
            guard let job else { return }
            await process(job)
        }
    }

    private static func eventID() -> String { "ev_" + UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased().prefix(16) }

    private func isClosed() -> Bool { lock.withLock { closed } }
    private func skip() { lock.withLock { _skipped += 1 } }

    private final class Flag: @unchecked Sendable {
        private let lock = NSLock(); private var v = false
        func set() { lock.withLock { v = true } }
        var value: Bool { lock.withLock { v } }
    }

    private func process(_ first: Job) async {
        var job = first
        while !isClosed() {
            await fetchSettings()
            guard let s = lock.withLock({ settings }), s.enabled, s.backend == "apple-fm", s.revision == job.revision, !isClosed() else { return skip() }
            guard available() else {  // no fallback to any other model: say so once per settings revision, and idle
                let first = lock.withLock { () -> Bool in defer { errorRevision = s.revision }; return errorRevision != s.revision }
                if first { _ = await call("label-error", ["message": "on-device model unavailable"]); lock.withLock { errorShown = true } }
                return skip()
            }
            guard !job.input.isEmpty else { return skip() }
            let now = Date()
            let hit = lock.withLock { () -> [Decide.Result]? in
                if cacheRevision != s.revision { cache.removeAll(); cacheRevision = s.revision }
                cache = cache.filter { now.timeIntervalSince($0.value.at) < 6 * 3600 }
                return cache[job.key]?.results
            }
            var results: [Decide.Result]
            if let hit { results = hit } else {
                let allowed = lock.withLock { () -> Bool in
                    starts.removeAll { now.timeIntervalSince($0) >= 3600 }
                    guard starts.count < s.maxPerHour else { return false }
                    starts.append(now)
                    return true
                }
                guard allowed else { return skip() }
                let outcome = await decision(job.input, s, threshold: s.threshold)
                guard !isClosed() else { return }
                if outcome.preempted {
                    if outcome.tokensIn > 0 {  // inference began: the attempt was real work, recorded without any text or error
                        _ = await call("label-decision", ["id": Self.eventID(), "provider": "apple-fm", "model": "apple-fm", "start": UsageEvent.iso(now),
                                                          "end": UsageEvent.iso(Date()), "tokensIn": outcome.tokensIn, "tokensOut": outcome.tokensOut,
                                                          "failed": false, "preempted": true])
                    }
                    if job.retries == 0 { job.retries += 1; continue }
                    return skip()
                }
                // the internal usage event of the decision: no user, never any text
                _ = await call("label-decision", ["id": Self.eventID(), "provider": "apple-fm", "model": "apple-fm", "start": UsageEvent.iso(now), "end": UsageEvent.iso(Date()),
                                                  "tokensIn": outcome.tokensIn, "tokensOut": outcome.tokensOut, "failed": outcome.results == nil])
                guard let r = outcome.results else {
                    guard lock.withLock({ !closed && settings?.revision == s.revision && settings?.enabled == true }) else { return skip() }
                    lock.withLock { _failed += 1 }
                    _ = await call("label-error", ["message": "label decision failed"]); lock.withLock { errorShown = true }
                    return
                }
                results = r
                lock.withLock {
                    if cache.count >= Self.cacheLimit, let oldest = cache.min(by: { $0.value.at < $1.value.at })?.key { cache.removeValue(forKey: oldest) }
                    cache[job.key] = (r, now)
                }
            }
            guard !isClosed(), lock.withLock({ settings?.revision == s.revision && settings?.enabled == true }) else { return }
            await beforeStore()
            guard !isClosed() else { return }
            await store(job, results, s)
            return
        }
    }

    private struct Outcome { var results: [Decide.Result]?; var preempted = false; var tokensIn = 0, tokensOut = 0 }
    /// The running sample, cancellable before it exists (a user request that arrives while it is being created).
    private final class SampleBox: @unchecked Sendable {
        private let lock = NSLock(); private var task: Task<String, Error>?; private var cancelled = false
        func set(_ t: Task<String, Error>) { lock.withLock { task = t; if cancelled { t.cancel() } } }
        func cancel() { lock.withLock { cancelled = true; task?.cancel() } }
        /// The first thing a sample does, before the model is touched: a user request that arrived since registration ends it here.
        func enter() throws { try lock.withLock { if cancelled { throw CancellationError() } } }
    }
    private final class Tokens: @unchecked Sendable {
        private let lock = NSLock(); private var i = 0, o = 0
        var input: Int { lock.withLock { i } }
        var output: Int { lock.withLock { o } }
        func add(input n: Int) { lock.withLock { i += n } }
        func add(output n: Int) { lock.withLock { o += n } }
    }

    private func takeSlot() -> Bool { lock.withLock { if sampleBusy { return false }; sampleBusy = true; return true } }
    private func freeSlot() { lock.withLock { sampleBusy = false } }

    /// Phase, then activity: three samples each, every sample only while no user request runs on the on-device model. The worker asks for both
    /// and registers its cancel (a settings change ends it); an evaluation (`evaluate`) asks only for the kinds it has hand labels for.
    private func decision(_ input: String, _ s: Settings, threshold: Double, phase wantPhase: Bool = true, activity wantActivity: Bool = true,
                          registerCancel: Bool = true) async -> Outcome {
        let preempted = Flag(), tokens = Tokens()
        let backend = self.backend, foreground = self.foreground, step = waitStep
        let counted: Decide.Backend = { [self] system, user, ids in
            await Task.yield()  // between the samples
            while true {  // never start a sample while a user request runs
                if Task.isCancelled { preempted.set(); throw CancellationError() }
                let box = SampleBox()
                if takeSlot() {
                    defer { freeSlot() }
                    if let id = foreground.watch({ preempted.set(); box.cancel() }) {  // registered before the model is touched
                        defer { foreground.unwatch(id) }
                        foreground.afterRegister?()
                        let sample = Task<String, Error> { [self] in
                            try box.enter()
                            lock.withLock { _samples += 1 }  // counted only once inference really starts
                            tokens.add(input: (system.utf8.count + user.utf8.count + 3) / 4)
                            return try await backend(system, user, ids)
                        }
                        box.set(sample)
                        let reply = try await withTaskCancellationHandler { try await sample.value } onCancel: { sample.cancel() }
                        if preempted.value { throw CancellationError() }
                        tokens.add(output: (reply.utf8.count + 3) / 4)
                        return reply
                    }
                }
                try? await Task.sleep(for: step)  // ponytail: polling; a notification when many workers wait
            }
        }
        let activities = s.activities.map { Decide.Candidate(id: $0, criteria: Self.criteria[$0] ?? $0) }
        let work = Task<[Decide.Result]?, Never> {
            var p = Decide.Result(), a = Decide.Result()
            if wantPhase {
                guard let r = try? await Decide.decide(counted, .init(question: "Which phase describes this work?", candidates: Self.phases, input: input), threshold: threshold) else { return nil }
                p = r
            }
            if wantActivity {
                guard let r = try? await Decide.decide(counted, .init(question: "Which activity describes this work?", candidates: activities, input: input), threshold: threshold) else { return nil }
                a = r
            }
            return [p, a]
        }
        if registerCancel { lock.withLock { cancelCurrent = { work.cancel() } } }
        let timer = Task { [budget] in try? await Task.sleep(for: budget); work.cancel() }
        if isClosed() { work.cancel() }
        let r = await work.value
        timer.cancel()
        if registerCancel { lock.withLock { cancelCurrent = nil } }
        // a cancel by the budget, the user or a settings change gives no result; only the user (or waiting for the user) is a retry
        return Outcome(results: r, preempted: r == nil && preempted.value, tokensIn: tokens.input, tokensOut: tokens.output)
    }

    private func store(_ job: Job, _ results: [Decide.Result], _ s: Settings) async {
        func score(_ r: Decide.Result) -> Int { Int(((r.scores[r.choice] ?? 0) * 1000).rounded()) }
        var ok = true
        if job.shadow {
            for (i, kind) in ["phase", "activity"].enumerated() {
                let given = i == 0 ? job.phase : job.activity
                if given.isEmpty { continue }
                let bucket = ["667": "667", "1000": "1000"][String(score(results[i]))] ?? "unknown"
                let r = await call("label-agreement", ["kind": kind, "bucket": bucket, "agree": !results[i].choice.isEmpty && results[i].choice == given, "revision": s.revision])
                if r.status >= 400 && r.status != 503 { ok = false }
            }
        } else if !results[0].choice.isEmpty || !results[1].choice.isEmpty {
            let body: [String: Any] = ["id": job.eventID, "phase": results[0].choice, "activity": results[1].choice, "phaseScore": score(results[0]),
                                       "activityScore": score(results[1]), "revision": s.revision]
            var r = await call("event-labels", body)
            for d in retryDelays where (r.status == 404 || r.status == 503) && !isClosed() {  // the root event may not be stored yet
                try? await Task.sleep(for: d)
                r = await call("event-labels", body)
            }
            ok = r.status == 200
        }
        if isClosed() { return }
        if !ok {
            lock.withLock { _failed += 1 }
            _ = await call("label-error", ["message": "label storage failed"]); lock.withLock { errorShown = true }
            return
        }
        lock.withLock { _done += 1 }
        if lock.withLock({ defer { errorShown = false }; return errorShown }) { _ = await call("label-error", ["message": ""]) }
    }
}

/// `minigw gw labels eval` on the Mac (docs/usage.md): the owner's check of the on-device label model against hand-labelled examples. The
/// examples arrive in the body of `POST /usage/labels/eval` (the app's own admin route, never a core route, never a member or remote one);
/// each goes through the same decision the worker makes (`decision`, with the same foreground coordination and the same strict `Decide`),
/// and only numbers come back. Nothing is stored: no label, no agreement counter, no text. The one record is the worker's own accounting rule:
/// one internal usage event for the whole run (`label-decision`: no user, source internal, tokens estimated). The text is in memory for the run
/// and never reaches a log, an error or an answer; the client keeps its own copy for `--show-errors`.
extension LabelWorker {
    struct EvalExample: Sendable { var text: String, phase: String, activity: String }

    static let evalMaxExamples = 500
    static let evalBudget: Duration = .seconds(30 * 60)
    static let evalAttempts = 3
    static let evalMaxWrong = 100
    static let evalMaxConfused = 5

    private static func evalError(_ status: Int, _ message: String) -> (Int, Data) {
        (status, (try? JSONSerialization.data(withJSONObject: ["type": "error", "error": ["type": Router.errorType(status), "message": message]])) ?? Data())
    }

    /// (status, JSON): the report `{"eval": …}` (the same fields as the Go gateway's) or an error body. `examples` have their text capped, the labels
    /// are validated here against the settings (a hand label is a phase word or one of the configured activities).
    func evaluate(_ raw: [EvalExample]) async -> (Int, Data) {
        await fetchSettings()
        guard let s = lock.withLock({ settings }) else { return Self.evalError(503, "The project database is not available") }
        guard !s.backend.isEmpty else { return Self.evalError(409, "choose a backend first (Settings, Model Labels), then evaluate it") }
        guard s.backend == "apple-fm" else { return Self.evalError(409, "label backend unavailable on this gateway") }
        guard available() else { return Self.evalError(409, "on-device model unavailable") }
        var examples: [EvalExample] = []
        for (i, e) in raw.enumerated() {
            let n = i + 1
            if e.text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { return Self.evalError(400, "example \(n): text is empty") }
            let phase = Self.safeWord(e.phase), activity = Self.safeWord(e.activity)
            if (!e.phase.isEmpty && !Self.phases.contains { $0.id == phase }) || (!e.activity.isEmpty && !s.activities.contains(activity)) {
                return Self.evalError(400, "example \(n): phase is one of \(Self.phases.map(\.id).joined(separator: ", ")), activity one of the configured list (\(s.activities.joined(separator: ", ")))")
            }
            if phase.isEmpty && activity.isEmpty { return Self.evalError(400, "example \(n): needs a phase or an activity") }
            examples.append(EvalExample(text: Decide.capInput(e.text), phase: phase, activity: activity))
        }
        guard lock.withLock({ if evaluating || closed { return false }; evaluating = true; return true }) else {
            return Self.evalError(429, "Another label evaluation is running on this gateway: wait for it to finish, then run this one again")
        }
        defer { lock.withLock { evaluating = false } }

        let start = Date(), deadline = ContinuousClock.now + Self.evalBudget
        var run = 0, tokensIn = 0, tokensOut = 0
        var cells: [String: [String: Int]] = ["phase": [:], "activity": [:]]  // scored, two (decided, correct), one (decided, correct)
        var confused: [String: [[String]: Int]] = ["phase": [:], "activity": [:]]
        var wrong: [[String: Any]] = []
        func bump(_ kind: String, _ key: String) { cells[kind, default: [:]][key, default: 0] += 1 }
        for (i, e) in examples.enumerated() {
            var done = false
            var attempt = 0
            while attempt < Self.evalAttempts, !done, ContinuousClock.now < deadline, !isClosed() {
                attempt += 1
                let outcome = await decision(e.text, s, threshold: 0.67, phase: !e.phase.isEmpty, activity: !e.activity.isEmpty, registerCancel: false)
                tokensIn += outcome.tokensIn; tokensOut += outcome.tokensOut
                if outcome.preempted {  // the user's request has priority; the example waits a moment and is tried again
                    try? await Task.sleep(for: .milliseconds(200))
                    continue
                }
                guard let results = outcome.results else { break }  // a failed example is not repeated: the run goes on
                done = true
                run += 1
                for (k, kind) in ["phase", "activity"].enumerated() {
                    let given = k == 0 ? e.phase : e.activity
                    if given.isEmpty { continue }
                    let r = results[k]
                    let score = Int(((r.scores[r.choice] ?? 0) * 1000).rounded())
                    bump(kind, "scored")
                    if !r.choice.isEmpty {
                        bump(kind, "twoDecided")
                        if score == 1000 { bump(kind, "oneDecided") }
                        if r.choice == given {
                            bump(kind, "twoCorrect")
                            if score == 1000 { bump(kind, "oneCorrect") }
                        }
                    }
                    if r.choice != given {
                        if !r.choice.isEmpty { confused[kind, default: [:]][[given, r.choice], default: 0] += 1 }
                        if wrong.count < Self.evalMaxWrong { wrong.append(["kind": kind, "line": i + 1, "expected": given, "got": r.choice]) }
                    }
                }
            }
        }
        // The worker's own accounting, once for the run; its failure never fails the evaluation.
        _ = await call("label-decision", ["id": Self.eventID(), "provider": "apple-fm", "model": "apple-fm", "start": UsageEvent.iso(start), "end": UsageEvent.iso(Date()),
                                          "tokensIn": tokensIn, "tokensOut": tokensOut, "failed": run == 0])
        guard run > 0 else { return Self.evalError(502, "The label backend gave no answer for any example: check that the model is available") }
        func kind(_ k: String) -> [String: Any] {
            let c = cells[k] ?? [:]
            return ["scored": c["scored"] ?? 0, "t067": ["decided": c["twoDecided"] ?? 0, "correct": c["twoCorrect"] ?? 0],
                    "t10": ["decided": c["oneDecided"] ?? 0, "correct": c["oneCorrect"] ?? 0]]
        }
        func mistakes(_ k: String) -> [[String: Any]] {
            (confused[k] ?? [:]).sorted { a, b in
                a.value != b.value ? a.value > b.value : (a.key[0] != b.key[0] ? a.key[0] < b.key[0] : a.key[1] < b.key[1])
            }.prefix(Self.evalMaxConfused).map { ["expected": $0.key[0], "got": $0.key[1], "n": $0.value] }
        }
        let report: [String: Any] = ["backend": s.backend, "examples": examples.count, "run": run, "notRun": examples.count - run, "phase": kind("phase"), "activity": kind("activity"),
                                     "mistakes": ["phase": mistakes("phase"), "activity": mistakes("activity")], "wrong": wrong]
        return (200, (try? JSONSerialization.data(withJSONObject: ["eval": report])) ?? Data())
    }
}
