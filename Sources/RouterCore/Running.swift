import Foundation
import os

/// #131: the requests that run on this gateway now, per device (GET /minigw/v1/running; the same as the Go gateway's
/// activityLog.running, shared fixture client/testdata/running.json). A device sees only its own; never a prompt.
public final class RunningRegistry: Sendable {
    struct Run {
        var id: String, owner: String, model: String, folder: String
        var started: Date
        var steps = 0
        var step = ""
        var stepAt: Date?
        var sink: (@Sendable (String) -> Void)?  // sends each step to the response stream (replyWhenDone)
    }
    private let runs = OSAllocatedUnfairLock<[Run]>(initialState: [])
    public init() {}

    func start(id: String, owner: String, model: String, folder: String, at: Date = Date()) {
        runs.withLock { $0.insert(Run(id: id, owner: owner, model: model, folder: Self.shortLine(folder, 300), started: at), at: 0) }
    }

    func finish(id: String) { runs.withLock { $0.removeAll { $0.id == id } } }

    func step(id: String, text: String, at: Date = Date()) {
        let sink: (@Sendable (String) -> Void)? = runs.withLock {
            guard let i = $0.firstIndex(where: { $0.id == id }) else { return nil }
            $0[i].step = text; $0[i].stepAt = at; $0[i].steps += 1
            return $0[i].sink
        }
        sink?(text)
    }

    func onStep(id: String, _ f: (@Sendable (String) -> Void)?) {
        runs.withLock { if let i = $0.firstIndex(where: { $0.id == id }) { $0[i].sink = f } }
    }

    /// The reply: only the requests of `owner` (the device id, or "local"); an empty owner sees nothing.
    func reply(owner: String, now: Date = Date()) -> [String: Any] {
        guard !owner.isEmpty else { return ["running": [[String: Any]]()] }
        let mine = runs.withLock { $0.filter { $0.owner == owner } }
        return ["running": mine.map { r -> [String: Any] in
            var o: [String: Any] = ["model": r.model, "elapsedSec": Int(now.timeIntervalSince(r.started)), "steps": r.steps]
            if !r.folder.isEmpty { o["folder"] = r.folder }
            if !r.step.isEmpty { o["lastStep"] = r.step }
            if let at = r.stepAt, Int(now.timeIntervalSince(at)) != 0 { o["stepAgoSec"] = Int(now.timeIntervalSince(at)) }
            return o
        }]
    }

    /// The owner of a request as in the Go gateway: the proven device, "local" for loopback without one, else "".
    static func owner(_ req: HTTPRequest) -> String {
        req.device?.id ?? (RouterServer.isLoopback(req.peer) ? "local" : "")
    }

    /// One line of at most n characters, then "…" (the client's shortLine).
    static func shortLine(_ s: String, _ n: Int) -> String {
        let l = ProjectText.oneLine(s)
        return l.unicodeScalars.count > n ? String(String.UnicodeScalarView(l.unicodeScalars.prefix(n))) + "…" : l
    }

    /// A step of an agent for the client: the shared folder's path made relative, other paths under the gateway's scratch
    /// folder hidden (the same as the Go gateway's relStep; shared fixture client/testdata/step-marks.json).
    static func relStep(_ st: String, dir: String, scratch: String) -> String {
        func trimmed(_ p: String) -> String {
            var p = p
            while let l = p.last, l == "/" || l == "\\" { p.removeLast() }
            return p
        }
        var s = st
        let d = trimmed(dir), sc = trimmed(scratch)
        if !d.isEmpty { for sep in ["/", "\\"] { s = s.replacingOccurrences(of: d + sep, with: "") } }
        if !sc.isEmpty { s = s.replacingOccurrences(of: sc, with: "…") }
        return s
    }
}

extension StreamTranslator {
    /// A step of an agent as a progress line for minigw (Go translator.stepMark): "⏺ name detail" as one text delta.
    /// The name is the step's first word, the detail one printable line of at most 160 characters. It is not part of t.text.
    static func stepMark(_ step: String) -> Data {
        let trimmed = step.trimmingCharacters(in: .whitespacesAndNewlines)
        var name = String(trimmed.prefix { $0 != " " }), detail = String(trimmed.dropFirst(name.count).dropFirst())
        if !Profiles.StreamText.validName(name) { name = "tool"; detail = step }
        detail = RunningRegistry.shortLine(detail, 160)
        if !detail.isEmpty { name += " " + detail }
        return sse("content_block_delta", ["type": "content_block_delta", "index": 0, "delta": ["type": "text_delta", "text": "⏺ " + name + "\n"]])
    }
}
