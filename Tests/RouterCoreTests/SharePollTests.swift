import Foundation
import Testing
@testable import RouterCore

@Suite struct SharePollTests {
    /// Given the last answer, how long the share long poll waits before the next request.
    @Test func decidesByAnswer() {
        // status, hasJob, elapsed, previous delay -> wait, next delay
        let cases: [(Int, Bool, TimeInterval, TimeInterval, TimeInterval, TimeInterval)] = [
            (200, true, 0, 0, 0, 0),        // a job: at once
            (200, true, 0, 8, 0, 0),        // a job resets the growth
            (204, false, 1, 0, 0, 0),       // a 204 that waited like a long poll
            (204, false, 40, 8, 0, 0),
            (204, false, 0.9, 0, 0.5, 0.5), // an instant 204 is a failure
            (204, false, 0, 8, 16, 16),
            (200, false, 0, 0, 0.5, 0.5),   // a 200 that is not a job is a failure
            (200, false, 0, 8, 16, 16),
            (500, false, 0, 0, 0.5, 0.5),   // another status is a failure
            (0, false, 0, 0, 0.5, 0.5),
            (500, false, 0, 16, 30, 30),    // the cap
            (500, false, 0, 30, 30, 30),
        ]
        for c in cases {
            let d = SharePoll.decide(status: c.0, hasJob: c.1, elapsed: c.2, delay: c.3)
            #expect(d.wait == c.4 && d.delay == c.5, "status \(c.0) job \(c.1) elapsed \(c.2) delay \(c.3): \(d)")
        }
    }
}
