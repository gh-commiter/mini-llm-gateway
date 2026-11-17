import Foundation

// The timer of the automatic upstream push on the Mac. The core (Go, in the helper) knows which links are dirty and due; the helper has no
// clock of its own that may call out and no network, so this app asks it: `POST /upstream-auto` on the upstream lane pushes ONE due link
// (through the callouts, as a manual push does) and says how long until the next one can be due. The smallest thing that works: one loop,
// one request at a time (never two outstanding), a pause that follows the helper's own answer, async sleeps (no thread of the cooperative
// pool is ever held), stopped when the app quits. It contacts nothing by itself: the helper only pushes links the owner set to auto, to
// upstreams the owner added. The outbox is in the core's database, so a restart of the app or the helper loses nothing.
public final class UpstreamAutoDriver: @unchecked Sendable {
    public typealias Call = @Sendable () async -> CoreResponse
    public typealias Sleep = @Sendable (Double) async -> Void

    /// How long to wait after an answer: soon again after a push (more may be due), what the helper says otherwise (clamped), and a slow
    /// poll when nothing is dirty, the helper is down or the routes are not available.
    static func pause(after r: CoreResponse) -> Double {
        guard r.status == 200, let o = try? JSONSerialization.jsonObject(with: r.body) as? [String: Any] else { return 60 }  // 501, 503: not now
        if o["ran"] as? Bool == true { return 1 }
        let next = (o["next"] as? Int) ?? 0
        return next > 0 ? Double(min(max(next, 5), 60)) : 30
    }

    private let call: Call, sleep: Sleep
    private let lock = NSLock()
    private var task: Task<Void, Never>?
    public private(set) var requests = 0  // for tests

    public init(call: @escaping Call, sleep: @escaping Sleep = { try? await Task.sleep(nanoseconds: UInt64($0 * 1_000_000_000)) }) {
        self.call = call; self.sleep = sleep
    }

    public func start() {
        lock.lock(); defer { lock.unlock() }
        guard task == nil else { return }
        task = Task { [self] in
            await sleep(10)  // let the helper come up
            while !Task.isCancelled {
                let r = await call()
                lock.lock(); requests += 1; lock.unlock()
                await sleep(Self.pause(after: r))
            }
        }
    }

    /// Stops the loop (a request in flight is not interrupted: the helper bounds it with its own time budget).
    public func stop() {
        lock.lock(); let t = task; task = nil; lock.unlock()
        t?.cancel()
    }
}
