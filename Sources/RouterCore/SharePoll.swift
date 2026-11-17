import Foundation

/// The shared folder's long poll (ShareClient.work): how long to wait before the next request.
/// A usable answer — a job, or a 204 that waited at least like a long poll — resets the growth;
/// anything else (an answer that is not a job, another status, an instant 204) is a failure and
/// waits 0.5 s, doubling up to 30 s, so a gateway that is something else on that port cannot make
/// the client spin.
public enum SharePoll {
    public static let minLongPoll: TimeInterval = 1
    public static let firstDelay: TimeInterval = 0.5
    public static let maxDelay: TimeInterval = 30

    /// wait: seconds before the next request (0 = at once); delay: the delay a further failure
    /// uses. The caller handles 404 (the gateway restarted) itself.
    public static func decide(status: Int, hasJob: Bool, elapsed: TimeInterval, delay: TimeInterval)
        -> (wait: TimeInterval, delay: TimeInterval) {
        if (status == 200 && hasJob) || (status == 204 && elapsed >= minLongPoll) { return (0, 0) }
        let next = min(max(delay * 2, firstDelay), maxDelay)
        return (next, next)
    }
}
