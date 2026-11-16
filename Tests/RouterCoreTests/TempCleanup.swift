import Foundation
import os

// Temp folders made by test helpers that return only a path (no place for a `defer`) were never removed: this Mac's T folder held 184,000 of them,
// and every `minigw run` lists T (Native.saveCopies), so the tests slowed themselves down until the router's handler could not get a pool thread in 30 s.
// A folder registered here is removed when the test process ends.
private let cleanupList = OSAllocatedUnfairLock(initialState: [URL]())
private let cleanupHook: Void = { atexit { for u in cleanupList.withLock({ $0 }) { try? FileManager.default.removeItem(at: u) } } }()

extension URL {
    func cleanedAtExit() -> URL {
        _ = cleanupHook
        cleanupList.withLock { $0.append(self) }
        return self
    }
}
