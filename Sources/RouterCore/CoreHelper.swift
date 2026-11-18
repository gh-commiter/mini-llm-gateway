import CryptoKit
import Darwin
import Foundation
import os

// The app's side of minigw-core, the helper that hosts the 0.6.0 core (client/coreapi). All policy, route parsing and error
// mapping is in Go; this file starts the process, authenticates it, forwards frames and says 503 when it cannot.
// Design and wire form: docs/core-adapter.md, docs/gateway.md, client/cmd/minigw-core/main.go.
//
// The helper is started over an inherited socketpair (fd 3) with a per-start secret on an inherited pipe (fd 4); there is
// no path to bind or steal. It serves only its parent (LOCAL_PEERPID) and only after a mutual HMAC handshake that proves
// the secret both ways (the secret is never sent). Frames are 4-byte big-endian length + JSON (coreapi/wire.go).

/// core.RunCap: the notes capability of one agent run (docs/core-adapter.md). The gateway keeps it; the core checks it.
public struct CoreRunCap: Sendable, Equatable {
    public var runId: String
    public var userId: Int
    public var deviceId: String
    public var projectId: Int
    public var epoch: Int
    public init(runId: String, userId: Int, deviceId: String, projectId: Int, epoch: Int) {
        self.runId = runId; self.userId = userId; self.deviceId = deviceId; self.projectId = projectId; self.epoch = epoch
    }
    var json: [String: Any] { ["runId": runId, "userId": userId, "deviceId": deviceId, "projectId": projectId, "epoch": epoch] }
}

/// What the adapter vouches for (coreapi.Context).
public struct CoreContext: Sendable, Equatable {
    public enum Kind: String, Sendable { case member, admin, run, `internal` }
    public var kind: Kind
    public var device = ""        // member: the device proven in THIS Noise tunnel ("" = none)
    public var approved = false   // member: on the approved list now
    public var agentPeer = false  // member: the caller is a process of one of the gateway's agents
    public var run: CoreRunCap?   // run: the capability of the run port's run (`approved`: its device is on the approved list now)
    public init(kind: Kind, device: String = "", approved: Bool = false, agentPeer: Bool = false, run: CoreRunCap? = nil) {
        self.kind = kind; self.device = device; self.approved = approved; self.agentPeer = agentPeer; self.run = run
    }
    public static let admin = CoreContext(kind: .admin)
    public static let `internal` = CoreContext(kind: .internal)

    var json: [String: Any] {
        var o: [String: Any] = ["kind": kind.rawValue]
        if !device.isEmpty { o["device"] = device }
        if approved { o["approved"] = true }
        if agentPeer { o["agentPeer"] = true }
        if let run { o["run"] = run.json }
        return o
    }
}

/// coreapi.Request. `path` is what Go's adapter passes: the decoded path for member routes, the escaped path below the
/// admin prefix for admin routes.
public struct CoreRequest: Sendable, Equatable {
    public var method: String
    public var path: String
    public var query = ""
    public var header: [String: [String]] = [:]
    public var body = Data()
    public var bodyTooLarge = false
    public var bodyUnreadable = false
    public init(method: String, path: String, query: String = "", header: [String: [String]] = [:], body: Data = Data(),
                bodyTooLarge: Bool = false, bodyUnreadable: Bool = false) {
        self.method = method; self.path = path; self.query = query; self.header = header; self.body = body
        self.bodyTooLarge = bodyTooLarge; self.bodyUnreadable = bodyUnreadable
    }

    var json: [String: Any] {
        var o: [String: Any] = ["method": method, "path": path]
        if !query.isEmpty { o["query"] = query }
        if !header.isEmpty { o["header"] = header }
        if !body.isEmpty { o["body"] = body.base64EncodedString() }
        if bodyTooLarge { o["bodyTooLarge"] = true }
        if bodyUnreadable { o["bodyUnreadable"] = true }
        return o
    }

    init?(json o: [String: Any]) {
        guard let m = o["method"] as? String, let p = o["path"] as? String else { return nil }
        self.init(method: m, path: p, query: o["query"] as? String ?? "", header: o["header"] as? [String: [String]] ?? [:],
                  body: (o["body"] as? String).flatMap { Data(base64Encoded: $0) } ?? Data(),
                  bodyTooLarge: o["bodyTooLarge"] as? Bool ?? false, bodyUnreadable: o["bodyUnreadable"] as? Bool ?? false)
    }
}

/// coreapi.Response.
public struct CoreResponse: Sendable, Equatable {
    public var status: Int
    public var header: [String: [String]]
    public var body: Data
    public init(status: Int, header: [String: [String]] = [:], body: Data = Data()) {
        self.status = status; self.header = header; self.body = body
    }

    init?(json o: [String: Any]) {
        guard let s = o["status"] as? Int else { return nil }
        self.init(status: s, header: o["header"] as? [String: [String]] ?? [:],
                  body: (o["body"] as? String).flatMap { Data(base64Encoded: $0) } ?? Data())
    }

    /// First value of a header (canonical Go names: "Content-Type", "Retry-After").
    public func value(_ name: String) -> String? { header.first { $0.key.caseInsensitiveCompare(name) == .orderedSame }?.value.first }

    /// coreapi.Unavailable(): the 503 while the helper is down or unavailable. The body is asserted against
    /// client/coreapi/testdata/unavailable.json by the Go and the Swift tests.
    public static let unavailable = CoreResponse(status: 503, header: ["Content-Type": ["application/json"]],
        body: Data(#"{"error":{"message":"The project database is not available","type":"core_unavailable"}}"#.utf8))
    /// coreapi.NoProjectAccess(): the fixed 403 for a request that fails the token or comes through a run port (testdata/no_project_access.json).
    public static let noProjectAccess = CoreResponse(status: 403, header: ["Content-Type": ["application/json"]],
        body: Data(#"{"error":{"message":"No project access","type":"no_project_access"}}"#.utf8))
}

public enum CoreError: Error, Equatable {
    case unavailable(String)       // helper missing, down, killed mid-request, timed out
    case versionMismatch(String)   // refused for good: restarting the same binary cannot fix it
    case peerNotAuthentic          // the other end could not prove the per-start secret: nothing was sent
}

/// Pending device revocations that survive a restart of the app (RouterConfig.pendingRevocations; tests use memory).
public protocol CoreRevocationMarks: Sendable {
    func list() async -> [String]
    func add(_ device: String) async
    func clear(_ devices: [String]) async
}

public enum CoreRevokeResult: Equatable, Sendable {
    case done                 // the epoch is raised, the mark is cleared
    case deferred             // the helper or its store is not usable: the mark stays, reconcile does it at the next ready
    case failed(String)       // the helper answered an error: the mark stays
}

/// What the app does for the helper while it handles one request (client/coreapi/callout.go): the helper has no network and no
/// credential store. `callout` gets the CalloutRequest as JSON and returns the CalloutResult; it runs on the helper's own queue (a
/// plain thread, not the cooperative pool), so it may block.
public protocol CoreCalloutHandler: Sendable {
    func callout(_ request: [String: Any]) -> [String: Any]
}

public final class CoreHelper: @unchecked Sendable {
    public static let protocolVersion = 1
    public static let maxFrame = 4 << 20  // coreapi.MaxFrame
    public static let maxBody = 512 << 10  // coreapi.MaxBody
    static let log = Logger(subsystem: "dev.trojar.minigateway", category: "core")

    /// Contents/Helpers/minigw-core of the running app bundle (build.sh puts it there); nil outside a bundle.
    public static func bundled() -> URL? {
        let u = Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/minigw-core")
        return FileManager.default.isExecutableFile(atPath: u.path) ? u : nil
    }

    /// <Application Support>/Mini Gateway/core: the store folder, 0700 (created by `prepareStoreFolder`).
    public static var defaultStore: URL {
        FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0].appendingPathComponent("Mini Gateway/core", isDirectory: true)
    }

    let store: URL  // the folder in use (RouterConfig.coreLinkProblem checks this one before every agent run)
    private let binary: URL?, versions: ClosedRange<Int>, backoff: [Double], requestTimeout: Double
    /// How long the app waits for the helper's hello reply (5 s), and how long once the helper has said it is upgrading the database (the
    /// helper opens, snapshots and migrates the store before it answers): a long upgrade must not be killed and retried in a loop.
    private let helloTimeout: Double, upgradeTimeout: Double
    private var startingNote = ""  // what the helper said it is doing before its hello reply ("upgrading the database (schema 8 to 24)"); "" otherwise
    private let ownerName: String, marks: CoreRevocationMarks
    private let cond = NSCondition()
    private var conn: CoreProcess?            // set after the handshake
    private var ready = false                 // set after ensure-owner and reconcile: project routes are served only now
    private var stopping = false, finished = false, fatal: CoreError?
    private var nextID: UInt64 = 0
    /// Why the upstream lane is down (nil = it works). A lane that failed is closed and NOT replaced while the helper runs, and the helper is
    /// NOT restarted for it: the first connection is healthy and the core keeps serving. The upstream routes answer 503 with this reason
    /// until the next helper start, which makes a new lane.
    private var laneBroken: String?
    private var spawned = false  // a helper process is running (set at spawn, cleared when it ends): before its handshake it is opening the database
    private var thread: Thread?
    private let io = DispatchQueue(label: "dev.trojar.minigateway.core.io")  // one request at a time
    /// The upstream routes have a connection and a queue of their own (the helper's lane): they wait for the network and the Keychain, and a
    /// slow upstream must never hold up a member read, a KB read or a device revocation on the first connection.
    private let laneIO = DispatchQueue(label: "dev.trojar.minigateway.core.upstream")
    public private(set) var spawnTimes: [Date] = []
    public private(set) var lastExit: Int32?
    public private(set) var lastFailure = ""
    /// Set before start(): the helper is told in the handshake that this app answers callouts (the upstream routes need it).
    public var callouts: CoreCalloutHandler?
    private var calloutRefusalLogged = false  // L3: one log line, not one per refused callout (only the lane's serial queue touches it)

    public init(binary: URL?, store: URL, ownerName: String = NSUserName(), marks: CoreRevocationMarks,
                versions: ClosedRange<Int> = CoreHelper.protocolVersion...CoreHelper.protocolVersion,
                backoff: [Double] = [0.1, 0.2, 0.4, 0.8, 1.6, 3.2, 5], requestTimeout: Double = 35,
                helloTimeout: Double = 5, upgradeTimeout: Double = 3600) {
        self.helloTimeout = helloTimeout; self.upgradeTimeout = upgradeTimeout
        self.binary = binary; self.store = store; self.ownerName = ownerName; self.marks = marks
        self.versions = versions; self.backoff = backoff; self.requestTimeout = requestTimeout
    }

    var binaryPath: String { binary?.path ?? "" }  // for test diagnostics
    public var pid: pid_t? { cond.lock(); defer { cond.unlock() }; return conn?.pid }
    public var isReady: Bool { cond.lock(); defer { cond.unlock() }; return ready }
    /// The helper process is up but not ready yet (it opens and migrates the database, then the owner and the marks): starting, not stopped.
    public var isStarting: Bool { cond.lock(); defer { cond.unlock() }; return spawned && !ready && fatal == nil }
    /// What the starting helper reports it is doing (an upgrade of the database); "" when it says nothing.
    public var startingDetail: String { cond.lock(); defer { cond.unlock() }; return spawned && !ready ? startingNote : "" }
    public var restarts: Int { cond.lock(); defer { cond.unlock() }; return max(0, spawnTimes.count - 1) }
    /// A refusal that no restart can fix (protocol version, unauthentic helper); nil while all is well.
    public var fatalError: CoreError? { cond.lock(); defer { cond.unlock() }; return fatal }

    /// The store folder is private to the app: 0700, created here (the helper's core.Open also makes it 0700).
    public static func prepareStoreFolder(_ url: URL) throws {
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: url.path)
    }

    public func start() {
        cond.lock(); defer { cond.unlock() }
        guard thread == nil else { return }
        let t = Thread { [self] in supervise() }
        t.name = "minigw-core supervisor"
        thread = t
        t.start()
    }

    /// Quit: close our end (the helper sees EOF and exits 0); SIGTERM to our own child only if it lingers.
    public func stop() {
        cond.lock()
        stopping = true
        ready = false
        if let c = conn { shutdown(c.fd, SHUT_RDWR); if let l = c.laneFd { shutdown(l, SHUT_RDWR) } }
        let deadline = Date().addingTimeInterval(2)
        while thread != nil && !finished && cond.wait(until: deadline) {}
        if let c = conn { kill(c.pid, SIGTERM) }
        cond.unlock()
    }

    /// Waits until project routes can be served (or the helper is refused for good).
    @discardableResult
    public func waitReady(timeout: Double = 10) async -> Bool {
        let end = Date().addingTimeInterval(timeout)
        while Date() < end {
            if isReady { return true }
            if fatalError != nil { return false }
            try? await Task.sleep(nanoseconds: 20_000_000)
        }
        return isReady
    }

    // MARK: calls

    /// Asks the core. Never throws: a helper that is missing, down, killed meanwhile or too slow gives the 503
    /// core_unavailable response (a write may or may not have happened: use the Idempotency-Key). The helper itself
    /// answers 503 with its reason while its store is unusable.
    public func call(context: CoreContext, request: CoreRequest) async -> CoreResponse {
        await send(context, request, needReady: true)
    }

    private func send(_ context: CoreContext, _ request: CoreRequest, needReady: Bool) async -> CoreResponse {
        let lane = Self.isUpstreamRoute(context, request) && callouts != nil  // no callouts, no lane: the first connection says 501
        return await withCheckedContinuation { cont in
            (lane ? laneIO : io).async { cont.resume(returning: self.exchange(context, request, needReady: needReady, lane: lane)) }
        }
    }

    /// The owner's upstream routes (client/coreapi/upstream.go): the only requests that may wait for the app.
    static func isUpstreamRoute(_ context: CoreContext, _ request: CoreRequest) -> Bool {
        context.kind == .admin && (request.path == "/upstream-push" || request.path == "/upstream-pull" || request.path == "/upstream-auto" || request.path == "/upstreams" || request.path.hasPrefix("/upstreams/"))
    }

    /// An internal call (POST /internal/<name>, docs/core-adapter.md).
    public func internalCall(_ name: String, _ body: [String: Any] = [:]) async -> CoreResponse {
        await send(.internal, CoreRequest(method: "POST", path: "/internal/" + name, body: (try? JSONSerialization.data(withJSONObject: body)) ?? Data("{}".utf8)), needReady: true)
    }

    /// The core's epoch (raised by every revocation); nil when it cannot be read.
    public func epoch() async -> Int? {
        let r = await internalCall("epoch")
        guard r.status == 200, let o = try? JSONSerialization.jsonObject(with: r.body) as? [String: Any] else { return nil }
        return o["epoch"] as? Int
    }

    private func exchange(_ cx: CoreContext, _ rq: CoreRequest, needReady: Bool, lane: Bool = false) -> CoreResponse {
        cond.lock()
        guard fatal == nil, let c = conn, ready || !needReady else { cond.unlock(); return .unavailable }
        if lane, let why = laneBroken { cond.unlock(); return Self.laneDown(why) }
        nextID += 1
        let id = nextID
        cond.unlock()
        guard let fd = lane ? c.laneFd : c.fd else { return Self.laneDown("no connection for upstream calls") }
        do {
            setTimeout(fd, requestTimeout)
            try sendFrame(fd, ["v": Self.protocolVersion, "id": id, "context": cx.json, "request": rq.json])
            var f = try recvFrame(fd)
            while let q = f["callout"] as? [String: Any] {  // the helper asks the app for something while it handles this request
                guard lane, let handler = callouts, f["v"] as? Int == Self.protocolVersion, (f["id"] as? NSNumber)?.uint64Value == id else {
                    throw CoreError.unavailable("the helper asked for something this app does not do")
                }
                var answer: [String: Any]
                if Self.calloutAllowed(rq, q) {
                    answer = handler.callout(q)
                } else {  // L3: a callout the running route never makes (a compromised helper asking for the network or for a credential's removal)
                    answer = ["error": "the callout is not part of this request"]
                    if !calloutRefusalLogged { calloutRefusalLogged = true; Self.log.error("refused an upstream callout the running route does not make (\(q["op"] as? String ?? "?", privacy: .public) during \(rq.method, privacy: .public) \(rq.path, privacy: .public))") }
                }
                answer["seq"] = q["seq"] ?? 0  // the number of the callout this answers: the helper drops any other
                try sendFrame(fd, ["v": Self.protocolVersion, "id": id, "callout_result": answer])
                setTimeout(fd, requestTimeout)
                f = try recvFrame(fd)
            }
            guard f["v"] as? Int == Self.protocolVersion, (f["id"] as? NSNumber)?.uint64Value == id,
                  (f["error"] as? String ?? "").isEmpty, let r = (f["response"] as? [String: Any]).flatMap(CoreResponse.init(json:)) else {
                throw CoreError.unavailable("the helper answered out of step: " + (f["error"] as? String ?? "bad frame"))
            }
            return r
        } catch {
            if lane {  // only the lane is lost: close it, say so, and leave the helper and its first connection alone
                cond.lock(); laneBroken = "\(error)"; shutdown(fd, SHUT_RDWR); cond.unlock()
                Self.log.error("upstream lane failed, upstream routes are down until the helper restarts: \(error.localizedDescription, privacy: .public)")
                return Self.laneDown("\(error)")
            }
            // the stream state is unknown: end this helper, the supervisor starts a new one
            cond.lock(); lastFailure = "\(error)"; ready = false; kill(c.pid, SIGKILL); cond.unlock()
            Self.log.error("helper request failed, restarting it: \(error.localizedDescription, privacy: .public)")
            return .unavailable
        }
    }

    /// L3: which callouts a running upstream route may make. The app accepts a callout only while it waits for the answer of the request it sent
    /// (the frame id is the app's own per-request number, echoed by the helper and checked above) and only one this route makes, for its upstream:
    /// `PUT /upstreams/{n}` connects, forgets after a failed add and probes; `DELETE /upstreams/{n}` only forgets; `GET /upstreams/{n}/probe` probes;
    /// `GET /upstreams` reads the device of each upstream; the push, the automatic push and the pull contact the upstreams their links name. Every admin route that needs callouts is listed with its kinds in client/testdata/upstream-callouts.json (asserted here and by the Go test that finds the routes calling the host).
    static func calloutAllowed(_ rq: CoreRequest, _ q: [String: Any]) -> Bool {
        guard let op = q["op"] as? String, let up = q["upstream"] as? String else { return false }
        let seg = rq.path.split(separator: "?", maxSplits: 1, omittingEmptySubsequences: false).first.map { $0.split(separator: "/").map(String.init) } ?? []
        switch (rq.method, seg.first ?? "") {
        case ("POST", "upstream-push"), ("POST", "upstream-auto"), ("POST", "upstream-pull"):
            return seg.count == 1 && (op == "do" || op == "info")
        case ("GET", "upstreams") where seg.count == 1:
            return op == "info"
        case ("GET", "upstreams") where seg.count == 3 && seg[2] == "probe":
            return (op == "do" || op == "info") && up == seg[1]
        case ("PUT", "upstreams") where seg.count == 2:
            return ["connect", "forget", "do", "info"].contains(op) && up == seg[1]
        case ("DELETE", "upstreams") where seg.count == 2:
            return op == "forget" && up == seg[1]
        default:
            return false
        }
    }

    /// The answer of an upstream route while the lane is down: a clean 503 with the reason; nothing else is affected.
    static func laneDown(_ why: String) -> CoreResponse {
        let msg = "upstream routes are unavailable (\(why)): quit and open Mini Gateway to try again; everything else works"
        let body = (try? JSONSerialization.data(withJSONObject: ["error": ["message": msg, "type": "upstream_unavailable"]])) ?? Data()
        return CoreResponse(status: 503, header: ["Content-Type": ["application/json"]], body: body)
    }

    /// Test hook: close the upstream lane as a failure would.
    func breakLaneForTest() { cond.lock(); if let l = conn?.laneFd { shutdown(l, SHUT_RDWR) }; cond.unlock() }

    // MARK: device revocation (docs/core-adapter.md, duty 2)

    /// A device was removed from the approved list: mark it durably, then raise the epoch. Never loses the removal: when the
    /// helper cannot do it now the mark stays and `reconcile` sends it at the next ready.
    public func revoke(_ device: String) async -> CoreRevokeResult {
        await marks.add(device)
        guard isReady else { return .deferred }
        let r = await internalCall("device-revoked", ["device": device])
        switch r.status {
        case 200: await marks.clear([device]); return .done
        case 503: return .deferred
        default: return .failed("the core epoch could not be raised yet (\(r.status))")
        }
    }

    /// Sends every pending mark (after a removal that could not reach the helper, and at each ready). `done` ones are cleared.
    @discardableResult
    public func flush() async -> Bool {
        let pending = await marks.list()
        if pending.isEmpty { return true }
        let r = await send(.internal, CoreRequest(method: "POST", path: "/internal/reconcile",
            body: (try? JSONSerialization.data(withJSONObject: ["devices": pending])) ?? Data()), needReady: false)
        guard r.status == 200, let o = try? JSONSerialization.jsonObject(with: r.body) as? [String: Any], let done = o["done"] as? [String] else { return r.status == 503 }
        await marks.clear(done)
        return done.count == pending.count
    }

    /// First thing after the handshake, before project routes are served: the owner user (once), then the pending marks.
    /// false = something failed that a restart can fix. A helper whose store is unavailable (503) is up and stays so.
    private func prepare() async -> Bool {
        let owner = await send(.internal, CoreRequest(method: "POST", path: "/internal/ensure-owner",
            body: (try? JSONSerialization.data(withJSONObject: ["name": ownerName])) ?? Data()), needReady: false)
        if owner.status == 503 { return true }
        guard owner.status == 200 else { lastFailure = "ensure-owner answered \(owner.status)"; return false }
        return await flush()
    }

    // MARK: supervisor

    private func supervise() {
        var attempt = 0
        while true {
            cond.lock(); let quit = stopping; cond.unlock()
            if quit { break }
            let began = Date()
            cond.lock(); spawnTimes.append(began); cond.unlock()
            var up = false
            do {
                guard let binary, FileManager.default.isExecutableFile(atPath: binary.path) else { throw CoreError.unavailable("minigw-core not found") }
                let p = try CoreProcess.spawn(binary: binary, store: store, lane: callouts != nil)
                cond.lock(); laneBroken = nil; spawned = true; startingNote = ""; cond.unlock()  // a new helper has a new lane
                do {
                    // the first connection never gets callouts; the helper may first report an upgrade, which is waited for
                    _ = try handshake(fd: p.fd, secret: p.secret, versions: versions, helloTimeout: helloTimeout, upgradeTimeout: upgradeTimeout) { note in
                        self.cond.lock(); self.startingNote = note; self.cond.unlock()
                    }
                    if let l = p.laneFd { _ = try handshake(fd: l, secret: p.secret, versions: versions, callouts: true, helloTimeout: helloTimeout, upgradeTimeout: upgradeTimeout) }
                    cond.lock(); conn = p; cond.unlock()
                    let sem = DispatchSemaphore(value: 0)
                    nonisolated(unsafe) var prepared = false
                    Task { prepared = await self.prepare(); sem.signal() }
                    sem.wait()
                    cond.lock()
                    if prepared && !stopping { ready = true; up = true; cond.broadcast() } else { kill(p.pid, SIGKILL) }
                    cond.unlock()
                    if up { Task { await self.flush() } }  // a removal that arrived while we were preparing
                } catch let e as CoreError {
                    cond.lock(); lastFailure = "\(e)"; cond.unlock()
                    // wrong version or unauthentic helper: restarting the same binary cannot fix it
                    if e == .peerNotAuthentic || { if case .versionMismatch = e { return true }; return false }() {
                        cond.lock(); fatal = e; cond.broadcast(); cond.unlock()
                    }
                    kill(p.pid, SIGKILL)
                } catch { kill(p.pid, SIGKILL) }
                var status: Int32 = 0
                waitpid(p.pid, &status, 0)  // blocks until exit: crash, kill or EOF after stop()
                cond.lock(); conn = nil; ready = false; spawned = false; startingNote = ""; lastExit = status; close(p.fd); if let l = p.laneFd { close(l) }; cond.broadcast(); cond.unlock()
            } catch {
                cond.lock(); lastFailure = "\(error)"; cond.unlock()
            }
            cond.lock(); let done = stopping || fatal != nil; cond.unlock()
            if done { break }
            if up && Date().timeIntervalSince(began) > 10 { attempt = 0 }  // a healthy run: start the backoff over
            let wake = Date().addingTimeInterval(backoff[min(attempt, backoff.count - 1)])
            cond.lock(); while !stopping && cond.wait(until: wake) {}; cond.unlock()  // stop() interrupts the wait
            attempt += 1
        }
        cond.lock(); finished = true; cond.broadcast(); cond.unlock()
    }
}

// MARK: process

struct CoreProcess {
    let pid: pid_t
    let fd: Int32
    let laneFd: Int32?  // the second connection (fd 5 of the helper, -lane): the upstream routes
    let secret: Data

    /// posix_spawn with CLOEXEC_DEFAULT: the child gets exactly fd 0 and 1 (/dev/null), 2 (our stderr), 3 (socketpair end),
    /// 4 (secret pipe). Nothing else is inherited; the secret is not in argv or the environment (only TMPDIR is passed).
    static func spawn(binary: URL, store: URL, secret: Data? = nil, lane: Bool = false) throws -> CoreProcess {
        let secret = secret ?? Data(SymmetricKey(size: .bits256).withUnsafeBytes { Data($0) })
        var sv: [Int32] = [0, 0], pp: [Int32] = [0, 0]
        guard socketpair(AF_UNIX, SOCK_STREAM, 0, &sv) == 0, pipe(&pp) == 0 else { throw CoreError.unavailable("socketpair/pipe failed") }
        // keep our ends away from the fd 3/4 collision and from other children
        let theirSock = fcntl(sv[1], F_DUPFD_CLOEXEC, 10), theirPipe = fcntl(pp[0], F_DUPFD_CLOEXEC, 10)
        close(sv[1]); close(pp[0])
        _ = fcntl(sv[0], F_SETFD, FD_CLOEXEC); _ = fcntl(pp[1], F_SETFD, FD_CLOEXEC)
        var one: Int32 = 1
        setsockopt(sv[0], SOL_SOCKET, SO_NOSIGPIPE, &one, 4)
        defer { close(theirSock); close(theirPipe) }
        var lv: [Int32] = [-1, -1], theirLane: Int32 = -1
        if lane {
            guard socketpair(AF_UNIX, SOCK_STREAM, 0, &lv) == 0 else { close(sv[0]); close(pp[1]); throw CoreError.unavailable("socketpair failed") }
            theirLane = fcntl(lv[1], F_DUPFD_CLOEXEC, 10); close(lv[1])
            _ = fcntl(lv[0], F_SETFD, FD_CLOEXEC)
            setsockopt(lv[0], SOL_SOCKET, SO_NOSIGPIPE, &one, 4)
        }
        defer { if theirLane >= 0 { close(theirLane) } }

        var fa: posix_spawn_file_actions_t? = nil
        posix_spawn_file_actions_init(&fa); defer { posix_spawn_file_actions_destroy(&fa) }
        posix_spawn_file_actions_addopen(&fa, 0, "/dev/null", O_RDONLY, 0)
        posix_spawn_file_actions_addopen(&fa, 1, "/dev/null", O_WRONLY, 0)
        posix_spawn_file_actions_adddup2(&fa, 2, 2)
        posix_spawn_file_actions_adddup2(&fa, theirSock, 3)
        posix_spawn_file_actions_adddup2(&fa, theirPipe, 4)
        if lane { posix_spawn_file_actions_adddup2(&fa, theirLane, 5) }
        var at: posix_spawnattr_t? = nil
        posix_spawnattr_init(&at); defer { posix_spawnattr_destroy(&at) }
        posix_spawnattr_setflags(&at, Int16(POSIX_SPAWN_CLOEXEC_DEFAULT))

        let argv = [binary.path, "-store", store.path] + (lane ? ["-lane"] : [])
        let cargs: [UnsafeMutablePointer<CChar>?] = argv.map { strdup($0) } + [nil]
        defer { cargs.forEach { free($0) } }
        let env: [UnsafeMutablePointer<CChar>?] = (ProcessInfo.processInfo.environment["TMPDIR"].map { ["TMPDIR=" + $0] } ?? []).map { strdup($0) } + [nil]
        defer { env.forEach { free($0) } }
        var pid: pid_t = 0
        let rc = posix_spawn(&pid, binary.path, &fa, &at, cargs, env)
        guard rc == 0 else { close(sv[0]); close(pp[1]); if lane { close(lv[0]) }; throw CoreError.unavailable("cannot start helper: \(String(cString: strerror(rc)))") }
        let w = secret.withUnsafeBytes { write(pp[1], $0.baseAddress, secret.count) }
        close(pp[1])
        guard w == secret.count else { kill(pid, SIGKILL); waitpid(pid, nil, 0); close(sv[0]); if lane { close(lv[0]) }; throw CoreError.unavailable("secret delivery failed") }
        return CoreProcess(pid: pid, fd: sv[0], laneFd: lane ? lv[0] : nil, secret: secret)
    }
}

// MARK: framing (4-byte big-endian length + JSON), blocking with SO_RCVTIMEO / SO_SNDTIMEO

private func hex(_ d: some Sequence<UInt8>) -> String { d.map { String(format: "%02x", $0) }.joined() }

private func mac(_ secret: Data, _ label: String, _ version: Int, _ a: String, _ b: String) -> String {
    hex(HMAC<SHA256>.authenticationCode(for: Data("\(label)|\(version)|\(a)|\(b)".utf8), using: SymmetricKey(data: secret)))
}

private func macEqual(_ a: String, _ b: String) -> Bool {
    Data(a.utf8).withUnsafeBytes { x in Data(b.utf8).withUnsafeBytes { y in x.count == y.count && timingsafe_bcmp(x.baseAddress, y.baseAddress, x.count) == 0 } }
}

private func setTimeout(_ fd: Int32, _ seconds: Double) {
    var tv = timeval(tv_sec: Int(seconds), tv_usec: Int32((seconds - Double(Int(seconds))) * 1_000_000))
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
}

private func writeAll(_ fd: Int32, _ d: Data) throws {
    var off = 0
    while off < d.count {
        let n = d.withUnsafeBytes { send(fd, $0.baseAddress! + off, d.count - off, 0) }  // send: EPIPE as an error, not SIGPIPE
        if n <= 0 { if n < 0 && errno == EINTR { continue }; throw CoreError.unavailable("write: \(String(cString: strerror(errno)))") }
        off += n
    }
}

private func readExactly(_ fd: Int32, _ count: Int) throws -> Data {
    var buf = [UInt8](repeating: 0, count: count), off = 0
    while off < count {
        let n = buf.withUnsafeMutableBytes { read(fd, $0.baseAddress! + off, count - off) }
        if n == 0 { throw CoreError.unavailable("helper closed the connection") }
        if n < 0 { if errno == EINTR { continue }; throw CoreError.unavailable(errno == EAGAIN ? "helper timed out" : "read: \(String(cString: strerror(errno)))") }
        off += n
    }
    return Data(buf)
}

private func sendFrame(_ fd: Int32, _ obj: [String: Any]) throws {
    let body = try JSONSerialization.data(withJSONObject: obj)
    guard body.count <= CoreHelper.maxFrame else { throw CoreError.unavailable("request frame too large") }
    var len = UInt32(body.count).bigEndian
    try writeAll(fd, Data(bytes: &len, count: 4) + body)
}

private func recvFrame(_ fd: Int32, max: Int = CoreHelper.maxFrame) throws -> [String: Any] {
    let h = try readExactly(fd, 4)
    let n = Int(h.withUnsafeBytes { $0.loadUnaligned(as: UInt32.self).bigEndian })
    guard n <= max else { throw CoreError.unavailable("helper sent an oversized frame") }
    return try JSONSerialization.jsonObject(with: try readExactly(fd, n)) as? [String: Any] ?? [:]
}

/// The client side of the handshake in client/cmd/minigw-core/serve.go: the app checks the helper's MAC before it sends
/// anything derived from the secret.
private func handshake(fd: Int32, secret: Data, versions: ClosedRange<Int>, callouts: Bool = false, helloTimeout: Double = 5,
                       upgradeTimeout: Double = 3600, onState: (String) -> Void = { _ in }) throws -> Int {
    let cn = hex((0..<16).map { _ in UInt8.random(in: 0...255) })
    setTimeout(fd, helloTimeout)
    var hello: [String: Any] = ["type": "hello", "min": versions.lowerBound, "max": versions.upperBound, "nonce": cn]
    if callouts { hello["callouts"] = true }
    try sendFrame(fd, hello)
    var r = try recvFrame(fd, max: 1 << 10)
    while r["type"] as? String == "state" {  // the helper is upgrading the database before it answers: wait for it, with its own limit
        onState(String((r["detail"] as? String ?? "upgrading the database").prefix(200)))
        setTimeout(fd, upgradeTimeout)
        r = try recvFrame(fd, max: 1 << 10)
    }
    setTimeout(fd, helloTimeout)
    if r["type"] as? String == "error" {
        if r["code"] as? String == "version_mismatch" {
            throw CoreError.versionMismatch("helper supports protocol \(r["server_min"] ?? "?")..\(r["server_max"] ?? "?"), app needs \(versions.lowerBound)..\(versions.upperBound)")
        }
        throw CoreError.unavailable("helper refused: \(r["code"] ?? "?")")
    }
    guard let v = r["version"] as? Int, let sn = r["nonce"] as? String, let m = r["mac"] as? String,
          versions.contains(v), macEqual(m, mac(secret, "srv", v, cn, sn)) else { throw CoreError.peerNotAuthentic }
    try sendFrame(fd, ["type": "auth", "mac": mac(secret, "cli", v, sn, cn)])
    guard try recvFrame(fd, max: 1 << 10)["type"] as? String == "ready" else { throw CoreError.unavailable("helper did not accept the secret") }
    return v
}
