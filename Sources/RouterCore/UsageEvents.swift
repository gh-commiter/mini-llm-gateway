import Foundation

// 0.6.0, adapter duty 8 (docs/core-adapter.md): usage events of model requests. The adapter decides WHEN a request was dispatched
// (never for a local refusal), freezes user, device, project and parent at admission and sends `event-begin` and `event-finish`;
// the helper's recorder (coreapi) stores them, spools them (bounded, in the core folder) when a write fails, replays by event id and shows
// gaps. Never on the request path: a request only appends to a bounded in-memory queue; one task sends it. While the helper is down the
// queue waits (bounded; an overflow drops the oldest and is reported as a gap, `event-lost`, when the helper is back). Same shape as
// client/serve_usage.go.

/// One usage event (core.UsageEvent). `tokensIn/Out` -1 = unknown.
struct UsageEvent: Sendable {
    var id: String
    var parentId = ""
    var start: Date
    var end: Date?
    var userId = 0
    var projectId = 0
    var deviceId = ""
    var provider = ""
    var model: String
    var outcome = "open"      // open (begun), ok, error, cancelled
    var tokensIn = -1
    var tokensOut = -1
    var tokenSource = "unknown"  // reported, estimated, unknown

    var json: [String: Any] {
        var o: [String: Any] = ["id": id, "start": Self.iso(start), "provider": provider, "model": model, "outcome": outcome,
                                "tokensIn": tokensIn, "tokensOut": tokensOut, "tokenSource": tokenSource]
        if !parentId.isEmpty { o["parentId"] = parentId }
        if let end { o["end"] = Self.iso(end) }
        if userId != 0 { o["userId"] = userId }
        if projectId != 0 { o["projectId"] = projectId }
        if !deviceId.isEmpty { o["deviceId"] = deviceId }
        return o
    }
}

/// The state of one request that the launchers and the usage code share (a task-local of Router.handle): the project the request was
/// admitted with and the admission context of its usage event.
final class RequestState: @unchecked Sendable {
    private let lock = NSLock()
    private var _project: ProjectRun?
    private var _event: UsageEvent?
    private var _dispatched = false
    private var _tokenSource = ""
    var project: ProjectRun? { get { lock.withLock { _project } } set { lock.withLock { _project = newValue } } }
    var event: UsageEvent? { lock.withLock { _event } }
    var tokenSource: String { get { lock.withLock { _tokenSource } } set { lock.withLock { _tokenSource = newValue } } }
    func admit(_ e: UsageEvent) { lock.withLock { _event = e } }
    /// The first caller gets the event (with the project it has now); later ones nil: one begin per request.
    func dispatch(provider: String) -> UsageEvent? {
        lock.withLock {
            guard var e = _event, !_dispatched else { return nil }
            _dispatched = true
            e.projectId = _project?.project ?? e.projectId
            e.provider = provider
            _event = e
            return e
        }
    }
    var wasDispatched: Bool { lock.withLock { _dispatched } }
}

/// Sends events to the helper off the request path. One instance per Router; its task outlives a Router restart until the queue is empty.
final class UsageSender: @unchecked Sendable {
    private let core: CoreHelper?
    private let lock = NSLock()
    private var queue: [Item] = []
    private var seq = 0
    private struct Item { let seq: Int, finish: Bool, start: Date, body: [String: Any], name: String }
    private var lost = 0, lostFrom = Date.distantFuture, lostTo = Date.distantPast
    private var busy = false
    private let limit: Int
    private let retry: Duration
    /// events and the begin/finish of the last 24 h of the bound users, as in Go (Recorder.bound): attribution only, never an access decision.
    private var bound: [String: Int] = [:]

    init(core: CoreHelper?, limit: Int = 4096, retry: Duration = .milliseconds(500)) { self.core = core; self.limit = limit; self.retry = retry }

    func begin(_ e: UsageEvent) { enqueue("event-begin", e, finish: false) }
    func finish(_ e: UsageEvent) { enqueue("event-finish", e, finish: true) }

    private func enqueue(_ name: String, _ e: UsageEvent, finish: Bool) {
        guard core != nil else { return }
        lock.withLock {
            if queue.count >= limit {  // a lost begin whose finish is stored loses nothing (the finish carries the whole event): drop those first
                let i = queue.firstIndex { !$0.finish } ?? 0
                let d = queue.remove(at: i)
                if d.finish { noteLoss(d.start, Date()) }
            }
            seq += 1
            queue.append(Item(seq: seq, finish: finish, start: e.start, body: ["event": e.json], name: name))
            if busy { return }
            busy = true
            Task.detached { [self] in await drain() }
        }
    }

    private func noteLoss(_ from: Date, _ to: Date) {  // under the lock
        lost += 1; lostFrom = min(lostFrom, from); lostTo = max(lostTo, to)
    }

    private func drain() async {
        guard let core else { return }
        while true {
            let (item, gap) = lock.withLock { () -> (Item?, [String: Any]?) in
                if queue.isEmpty { busy = false }
                return (queue.first, lost > 0 ? ["count": lost, "from": UsageEvent.iso(lostFrom), "to": UsageEvent.iso(lostTo)] : nil)
            }
            guard let item else { return }
            guard core.isReady else { try? await Task.sleep(for: retry); continue }  // the helper is down: wait, nothing is blocked
            if let gap {  // a gap first (before anything that comes after it)
                let r = await core.internalCall("event-lost", gap)
                if r.status == 503 { try? await Task.sleep(for: retry); continue }
                lock.withLock { lost = 0; lostFrom = .distantFuture; lostTo = .distantPast }  // accepted, or refused for good
            }
            let r = await core.internalCall(item.name, item.body)
            if r.status == 503 { try? await Task.sleep(for: retry); continue }  // down or not ready: keep it
            lock.withLock { queue.removeAll { $0.seq == item.seq } }  // sent, or refused for good (a bad body would never go through)
        }
    }

    /// Tests, shutdown: waits until everything queued so far has been sent.
    func flush(timeout: Duration = .seconds(20)) async -> Bool {
        let end = ContinuousClock.now + timeout
        while ContinuousClock.now < end {
            if lock.withLock({ queue.isEmpty && !busy }) { return true }
            try? await Task.sleep(for: .milliseconds(10))
        }
        return false
    }

    var pending: Int { lock.withLock { queue.count } }

    /// The user a device is bound to (attribution only), from the core's snapshot; the last one seen while the core is slow or down; 0 = unknown.
    /// Never waits long: a request does not wait for usage.
    func user(of device: String) async -> Int {
        guard let core, !device.isEmpty else { return 0 }
        let task = Task { () -> Int? in
            let r = await core.internalCall("bound-user", ["device": device])
            guard r.status == 200, let u = (try? JSONSerialization.jsonObject(with: r.body) as? [String: Any])?["user"] as? Int else { return nil }
            self.lock.withLock { if u != 0 { self.bound[device] = u } }
            return u
        }
        if let u = await firstOf(task, within: .milliseconds(250)), let u, u != 0 { return u }
        return lock.withLock { bound[device] ?? 0 }
    }
}

extension UsageEvent {
    static func iso(_ d: Date) -> String {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f.string(from: d)
    }
}

extension Router {
    @TaskLocal static var reqState: RequestState?

    /// (A5) The immutable admission context of a model request, built once when it is admitted. Nothing is sent yet: an event exists only
    /// once the work was dispatched (`dispatched`), so a local refusal never becomes one.
    func admitEvent(_ req: HTTPRequest, model: String, state: RequestState) async {
        guard core != nil, req.path == "/v1/messages" else { return }
        var e = UsageEvent(id: "ev_" + UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased().prefix(16), start: Date(), model: model)
        if let d = req.device {
            e.deviceId = d.id
            e.userId = await usageSender.user(of: d.id)
        }
        if let run = AgentRun.current {  // a nested call: the caller's event is the parent; its user and project are inherited
            e.parentId = run.evID; e.userId = run.evUser
            e.projectId = run.project?.project ?? 0
        }
        state.admit(e)
    }

    /// The moment execution or the upstream request really begins (a provider request, an agent process, a Foundry request): the event
    /// is sent as begun, with its project (an interrupted run keeps it).
    func dispatched(_ provider: String) {
        if let e = Self.reqState?.dispatch(provider: provider) { usageSender.begin(e) }
    }

    /// Completes the event of a request that was dispatched; any status then is an outcome (an upstream 4xx is an error event). A request that
    /// never reached dispatch has no event.
    func endEvent(_ state: RequestState, status: Int, usage counts: UsageWriter?) {
        guard state.wasDispatched, var e = state.event else { return }
        e.end = Date()
        if e.provider.isEmpty { e.provider = "unknown" }
        switch true {
        case status == 499 || Task.isCancelled: e.outcome = "cancelled"
        case status >= 400 || counts?.errorSeen == true: e.outcome = "error"
        default: e.outcome = "ok"
        }
        if let u = counts?.usage, u.input > 0 || u.output > 0 {
            e.tokensIn = u.input; e.tokensOut = u.output
            e.tokenSource = state.tokenSource.isEmpty ? "reported" : state.tokenSource  // estimated: Codex, Grok and Copilot give no counts
            if e.tokensIn == 0 && e.tokenSource == "estimated" { e.tokensIn = -1 }
        }
        usageSender.finish(e)
    }
}
