import Foundation
import Testing

/// Guard for the #150/#147/#140 family: a Swift Testing test that blocks a thread of the Swift concurrency pool (semaphore wait, blocking socket
/// call, waitUntilExit) leaves the router's handler `Task` without a thread, and the whole suite stalls. One sampler for the whole run measures
/// how long a `Task` waits for a pool thread, every 100 ms; the worst wait over the run is printed at the end ("pool lag: worst ...") and a wait
/// above `PoolLagGuard.limit` is recorded as an issue of the run, naming this guard. The sampler starts when the run is planned (the trait of
/// the test below is prepared for every selected run, before any test body), so it covers the tests that run before the guard test does.
enum PoolLagGuard {
    /// Far above a healthy run (see the numbers in docs/release-notes.md / the commit message); MG_POOL_LAG_LIMIT (seconds) overrides it.
    static let limit: Double = Double(ProcessInfo.processInfo.environment["MG_POOL_LAG_LIMIT"] ?? "") ?? .infinity

    private static let lock = NSLock()
    nonisolated(unsafe) private static var worst = 0.0, samples = 0, reported = false, started = false

    static var snapshot: (worst: Double, samples: Int) { lock.withLock { (worst, samples) } }
    /// For a failure message: a failure at a moment of a large lag points at starvation, a small one rules it out.
    static var text: String { let s = snapshot; return "pool lag so far: worst \(String(format: "%.1f", s.worst)) s over \(s.samples) samples" }

    static func start() {
        guard lock.withLock({ let s = started; started = true; return !s }) else { return }
        atexit {
            let s = PoolLagGuard.snapshot
            FileHandle.standardError.write(Data("pool lag: worst \(String(format: "%.3f", s.worst)) s over \(s.samples) samples (limit \(PoolLagGuard.limit) s)\n".utf8))
        }
        Thread {
            while true {
                let t0 = ContinuousClock.now, done = DispatchSemaphore(value: 0)
                Task { done.signal() }
                var sampled = false
                while done.wait(timeout: .now() + 1) == .timedOut {
                    let waited = Double((ContinuousClock.now - t0).components.seconds)
                    // reported while it waits: a stall that never ends still fails the run
                    let over = lock.withLock { () -> Bool in
                        worst = max(worst, waited)
                        let first = waited > limit && !reported
                        if first { reported = true }
                        return first
                    }
                    if over { Issue.record("PoolLagGuard: a Task has waited \(Int(waited)) s for a Swift concurrency pool thread (limit \(limit) s): a test blocks pool threads (give it the .blocking trait, or use offPool)") }
                    // diagnosis (MG_POOL_LAG_SAMPLE_DIR): after 5 s take the stacks of all threads of this process while the Task waits, to see which test code holds the pool threads
                    guard waited >= 5, !sampled, let dir = ProcessInfo.processInfo.environment["MG_POOL_LAG_SAMPLE_DIR"] else { continue }
                    sampled = true
                    let p = Process()
                    p.executableURL = URL(fileURLWithPath: "/usr/bin/sample")
                    p.arguments = [String(getpid()), "2", "-file", dir + "/sample-\(Int(Date().timeIntervalSince1970)).txt"]
                    p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice
                    try? p.run(); p.waitUntilExit()
                }
                let d = ContinuousClock.now - t0
                let sec = Double(d.components.seconds) + Double(d.components.attoseconds) / 1e18
                lock.withLock { samples += 1; worst = max(worst, sec) }
                Thread.sleep(forTimeInterval: 0.1)
            }
        }.start()
    }
}

/// Starts the sampler while the run is planned.
struct PoolLagSampler: TestTrait {
    func prepare(for test: Test) async throws { PoolLagGuard.start() }
}

extension Trait where Self == PoolLagSampler { static var poolLagSampler: Self { Self() } }

@Test("the run's worst wait of a Task for a pool thread stays under the limit (#150)", .poolLagSampler)
func poolLagStaysUnderTheLimit() {
    PoolLagGuard.start()
    let s = PoolLagGuard.snapshot
    #expect(s.worst <= PoolLagGuard.limit, "pool lag: worst \(s.worst) s over \(s.samples) samples")
}
