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
    var upstreamModel = ""
    var costUsdNanos: Int64?
    var costSource = "unknown"
    var billingMode = "unknown"
    var priceSnapshot: PriceSnapshot?
    var unpricedDimensions = false
    var cacheReadTokens: Int?
    var cacheWriteTokens: Int?
    var tokenSource = "unknown"  // reported, estimated, unknown

    var json: [String: Any] {
        var o: [String: Any] = ["id": id, "start": Self.iso(start), "provider": provider, "model": model, "outcome": outcome,
                                "tokensIn": tokensIn, "tokensOut": tokensOut, "tokenSource": tokenSource]
        o["billingMode"] = billingMode
        if let cacheReadTokens { o["cacheReadTokens"] = cacheReadTokens }
        if let cacheWriteTokens { o["cacheWriteTokens"] = cacheWriteTokens }
        if let priceSnapshot { o["priceSnapshot"] = priceSnapshot.json; o["priceSnapshotId"] = priceSnapshot.id }
        if unpricedDimensions { o["unpricedDimensions"] = true }
        if !upstreamModel.isEmpty { o["upstreamModel"] = upstreamModel }
        if let costUsdNanos { o["costUsdNanos"] = costUsdNanos; o["costSource"] = costSource }
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
    func dispatch(provider: String, upstreamModel: String = "", price: PriceSnapshot? = nil) -> UsageEvent? {
        lock.withLock {
            guard var e = _event, !_dispatched else { return nil }
            _dispatched = true
            e.projectId = _project?.project ?? e.projectId
            e.provider = provider
            e.upstreamModel = upstreamModel
            e.priceSnapshot = price
            e.billingMode = upstreamModel.isEmpty ? (["claude-cli", "claude", "codex", "grok", "copilot"].contains(provider) ? "subscription" : "unknown") : "metered"
            if ["apple-fm", "siri-ai", "foundry"].contains(provider) { e.billingMode = "local" }
            _event = e
            return e
        }
    }
    var wasDispatched: Bool { lock.withLock { _dispatched } }
}

/// The events that were lost on the way to the helper, as gaps (id, count and time span), and the file that keeps them: a loss is always shown
/// as a gap (contract section 7), also when the app quits with events still queued or the queue dropped some. The app attaches one journal (a
/// file in its own state folder) at start; the file is read then, and the gaps in it are sent as `event-lost` at the next ready.
///
/// Delivery is exactly-once by id: a gap gets its id when it is created and keeps it in the file; the core stores a gap by id, so a resend after a
/// lost answer counts once, and the helper answers OK only when the gap is durable (core or spool). A gap is removed from the journal only on that
/// OK. Losses after a send was attempted go into a NEW gap (a gap that may already be stored keeps its count). One send at a time (a Router restart
/// makes a second sender).
final class UsageGapJournal: @unchecked Sendable {
    struct Gap: Equatable {
        var id: String, count: Int, from: Date, to: Date
        var attempted = false  // sent (or possibly sent) once: no loss is added to it any more
    }

    /// The app's journal (nil in tests and tools: then every sender has a memory-only one).
    nonisolated(unsafe) static var attached: UsageGapJournal?

    private let lock = NSLock()
    private let file: URL?
    private var gaps: [Gap] = []
    private var inFlight: String?

    init(file: URL? = nil) {
        self.file = file
        guard let file, let d = try? Data(contentsOf: file), let o = try? JSONSerialization.jsonObject(with: d) as? [String: Any] else { return }
        func gap(_ g: [String: Any]) -> Gap? {
            guard let n = g["count"] as? Int, n > 0, let a = (g["from"] as? String).flatMap(ProjectText.parseTime), let b = (g["to"] as? String).flatMap(ProjectText.parseTime) else { return nil }
            return Gap(id: g["id"] as? String ?? Self.newID(), count: n, from: a, to: b, attempted: true)  // an older file has one gap without an id
        }
        gaps = (o["gaps"] as? [[String: Any]])?.compactMap(gap) ?? gap(o).map { [$0] } ?? []
    }

    static func newID() -> String { "gap_" + UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased().prefix(12) }

    var hasPending: Bool { lock.withLock { !gaps.isEmpty } }
    var pendingCount: Int { lock.withLock { gaps.reduce(0) { $0 + $1.count } } }

    func add(count: Int, from: Date, to: Date) {
        guard count > 0 else { return }
        lock.withLock {
            if let last = gaps.last, !last.attempted {
                gaps[gaps.count - 1].count += count
                gaps[gaps.count - 1].from = min(last.from, from)
                gaps[gaps.count - 1].to = max(last.to, to)
            } else {
                gaps.append(Gap(id: Self.newID(), count: count, from: from, to: to))
            }
            save()
        }
    }

    /// The gap to send now (the oldest; it is marked attempted); nil when there is none or one is on its way.
    func beginSend() -> Gap? {
        lock.withLock {
            guard inFlight == nil, !gaps.isEmpty else { return nil }
            gaps[0].attempted = true
            inFlight = gaps[0].id
            return gaps[0]
        }
    }

    /// The helper stored it (or refused it for good): it is gone; otherwise it stays, with its id, to be sent again.
    func settle(sent: Bool) {
        lock.withLock {
            if sent, let id = inFlight { gaps.removeAll { $0.id == id } }
            inFlight = nil
            save()
        }
    }

    private func save() {  // under the lock
        guard let file else { return }
        guard !gaps.isEmpty else { try? FileManager.default.removeItem(at: file); return }
        let o: [String: Any] = ["gaps": gaps.map { ["id": $0.id, "count": $0.count, "from": UsageEvent.iso($0.from), "to": UsageEvent.iso($0.to)] as [String: Any] }]
        if let d = try? JSONSerialization.data(withJSONObject: o) { try? d.write(to: file, options: .atomic) }
    }
}

/// The app's side of the journal: attached once at start (the file is in the app's own state folder), asked to record what is still queued
/// when the app quits.
public enum UsageLoss {
    public static func attach(file: URL) { UsageGapJournal.attached = UsageGapJournal(file: file) }
    public static func persistPendingAtQuit() { UsageSender.persistPendingAtQuit() }
}

/// Sends events to the helper off the request path. One instance per Router; its task outlives a Router restart until the queue is empty.
final class UsageSender: @unchecked Sendable {
    private let core: CoreHelper?
    private let lock = NSLock()
    private var queue: [Item] = []
    private var seq = 0
    private struct Item { let seq: Int, finish: Bool, start: Date, body: [String: Any], name: String }
    private let journal: UsageGapJournal
    private var busy = false
    private let limit: Int
    private let retry: Duration
    /// events and the begin/finish of the last 24 h of the bound users, as in Go (Recorder.bound): attribution only, never an access decision.
    private var bound: [String: Int] = [:]

    /// Senders alive, for `persistPendingAtQuit`.
    private static let liveLock = NSLock()
    nonisolated(unsafe) private static var live: [Weak] = []
    private struct Weak { weak var sender: UsageSender? }

    init(core: CoreHelper?, limit: Int = 4096, retry: Duration = .milliseconds(500), journal: UsageGapJournal = UsageGapJournal.attached ?? UsageGapJournal()) {
        self.core = core; self.limit = limit; self.retry = retry; self.journal = journal
        guard core != nil else { return }
        Self.liveLock.withLock { Self.live = Self.live.filter { $0.sender != nil } + [Weak(sender: self)] }
        if journal.hasPending {  // a gap from the last run: sent at the first ready, with no event needed to start it
            lock.withLock { busy = true }
            Task.detached { [self] in await drain() }
        }
    }

    /// The app is quitting: every event still in a queue never reaches the helper. Each one is recorded in the journal (its count and
    /// time span) so that the next start reports it as a gap. A queue that dropped events recorded those when it dropped them.
    static func persistPendingAtQuit() {
        let senders = liveLock.withLock { live.compactMap(\.sender) }
        for s in senders { s.lostWithTheQueue() }
    }

    func lostWithTheQueue() {
        let (ids, from) = lock.withLock { () -> (Set<String>, Date) in
            var ids = Set<String>(), from = Date.distantFuture
            for i in queue { ids.insert((i.body["event"] as? [String: Any])?["id"] as? String ?? "seq\(i.seq)"); from = min(from, i.start) }
            return (ids, from)
        }
        journal.add(count: ids.count, from: from, to: Date())
    }

    func begin(_ e: UsageEvent) { enqueue("event-begin", e, finish: false) }
    func finish(_ e: UsageEvent) { enqueue("event-finish", e, finish: true) }

    private func enqueue(_ name: String, _ e: UsageEvent, finish: Bool) {
        guard core != nil else { return }
        lock.withLock {
            if queue.count >= limit {  // a lost begin whose finish is stored loses nothing (the finish carries the whole event): drop those first
                let i = queue.firstIndex { !$0.finish } ?? 0
                let d = queue.remove(at: i)
                if d.finish { journal.add(count: 1, from: d.start, to: Date()) }
            }
            seq += 1
            queue.append(Item(seq: seq, finish: finish, start: e.start, body: ["event": e.json], name: name))
            if busy { return }
            busy = true
            Task.detached { [self] in await drain() }
        }
    }

    private func drain() async {
        guard let core else { return }
        while true {
            let (item, gapWaits) = lock.withLock { () -> (Item?, Bool) in
                let head = queue.first, gap = journal.hasPending
                if head == nil && !gap { busy = false }
                return (head, gap)
            }
            guard item != nil || gapWaits else { return }
            guard core.isReady else { try? await Task.sleep(for: retry); continue }  // the helper is down: wait, nothing is blocked
            if let gap = journal.beginSend() {  // a gap first (before anything that comes after it)
                let r = await core.internalCall("event-lost", ["id": gap.id, "count": gap.count, "from": UsageEvent.iso(gap.from), "to": UsageEvent.iso(gap.to)])
                if r.status == 503 { journal.settle(sent: false); try? await Task.sleep(for: retry); continue }
                journal.settle(sent: true)  // accepted, or refused for good
            }
            guard let item else { continue }
            let r = await core.internalCall(item.name, item.body)
            if r.status == 503 { try? await Task.sleep(for: retry); continue }  // down or not ready: keep it
            lock.withLock { queue.removeAll { $0.seq == item.seq } }  // sent, or refused for good (a bad body would never go through)
        }
    }

    /// Tests, shutdown: waits until everything queued so far has been sent.
    func flush(timeout: Duration = .seconds(20)) async -> Bool {
        let end = ContinuousClock.now + timeout
        while ContinuousClock.now < end {
            if lock.withLock({ queue.isEmpty && !busy }) && !journal.hasPending { return true }
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
    func dispatched(_ provider: String, upstreamModel: String = "", price: PriceSnapshot? = nil) {
        if let e = Self.reqState?.dispatch(provider: provider, upstreamModel: upstreamModel, price: price) { usageSender.begin(e) }
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
        if let m = counts?.eventMetrics, state.tokenSource != "estimated" {
            e.unpricedDimensions = m.cacheUsed
            e.cacheReadTokens = m.cacheRead; e.cacheWriteTokens = m.cacheWrite
            var (input, output) = (m.input, m.output)
            if e.upstreamModel.isEmpty, input == 0, output == 0 { (input, output) = (nil, nil) }  // an agent's message_start carries 0/0 before any count exists: an aborted run is unknown
            if input != nil || output != nil {  // otherwise the pattern counts set above stay (nesting, a usage object over 64 KB, usage elsewhere)
                e.tokensIn = input ?? -1; e.tokensOut = output ?? -1
                e.tokenSource = "reported"
            }
            if !e.upstreamModel.isEmpty, let cost = m.cost { e.costUsdNanos = cost; e.costSource = "reported" }
        }
        usageSender.finish(e)
    }
}
