import Foundation
import Testing

/// A task executor whose every job runs on a thread of its own, never on one of the Swift concurrency pool (SE-0417).
/// A test that blocks (a semaphore wait, a sleep, waitUntilExit, a blocking socket call) and runs under this executor holds one of these threads,
/// not a pool thread: the router's handler `Task` under test keeps getting pool threads, however many such tests run at once (#150/#147/#140).
final class BlockingExecutor: TaskExecutor, @unchecked Sendable {
    static let shared = BlockingExecutor()
    func enqueue(_ job: consuming ExecutorJob) {
        let unowned = UnownedJob(job)
        let thread = Thread { unowned.runSynchronously(on: self.asUnownedTaskExecutor()) }
        thread.stackSize = 4 << 20
        thread.start()
    }
}

/// `@Test(.blocking)` or `@Suite(.blocking)`: the body of the test (synchronous or async) runs on `BlockingExecutor`.
struct BlockingTrait: TestTrait, SuiteTrait, TestScoping {
    var isRecursive: Bool { true }
    func provideScope(for test: Test, testCase: Test.Case?, performing function: @Sendable () async throws -> Void) async throws {
        guard testCase != nil else { return try await function() }
        try await withTaskExecutorPreference(BlockingExecutor.shared) { try await function() }
    }
}

extension Trait where Self == BlockingTrait { static var blocking: Self { Self() } }
