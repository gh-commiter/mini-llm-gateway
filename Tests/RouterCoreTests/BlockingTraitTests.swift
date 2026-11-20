import Foundation
import Testing

/// `.blocking` (BlockingTrait.swift): a test that blocks runs on a thread of its own. Whether the code runs on a thread of the Swift concurrency
/// pool shows in the label of the dispatch queue the thread is on ("...cooperative").
@Suite struct BlockingTraitTests {
    private static func onPool() -> Bool { String(cString: __dispatch_queue_get_label(nil)).contains("cooperative") }

    @Test(.blocking) func aSynchronousTestRunsOffThePool() {
        #expect(!Self.onPool())
    }

    @Test(.blocking) func anAsyncTestRunsOffThePoolBeforeAndAfterASuspension() async throws {
        #expect(!Self.onPool())
        try await Task.sleep(for: .milliseconds(20))
        #expect(!Self.onPool())
    }

    @Test(.blocking, arguments: [1, 2]) func aTestWithArgumentsRunsOffThePool(_ n: Int) {
        #expect(!Self.onPool() && n > 0)
    }

    @Test func aTestWithoutTheTraitStaysOnThePool() async {
        #expect(Self.onPool())  // the control: the check above can fail
    }
}
