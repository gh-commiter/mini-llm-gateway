import Foundation

// Forwarding of the 0.6.0 core's routes to the helper (CoreHelper). The router authenticates exactly as the Go gateway does
// (client/serve_core.go projectHTTP, serve_admin.go adminDo) and builds the trusted context; policy, route parsing and
// error mapping stay in Go (client/coreapi). Duties of the adapter: docs/core-adapter.md.

/// The two path tests of coreapi (IsMemberPath, IsAdminPath), mirrored. client/coreapi/testdata/paths.json is asserted
/// against both implementations, so they cannot drift.
enum CoreRoutes {
    static func isMemberPath(_ p: String) -> Bool {
        p == "/minigw/v1/projects" || p.hasPrefix("/minigw/v1/projects/") || p == "/minigw/v1/usage" || p == "/minigw/v1/teams" || p.hasPrefix("/minigw/v1/teams/")
    }

    /// `escapedPath` is the path below the admin prefix.
    static func isAdminPath(_ method: String, _ escapedPath: String) -> Bool {
        var seg: [String] = []
        for part in escapedPath.trimmingCharacters(in: CharacterSet(charactersIn: "/")).split(separator: "/", omittingEmptySubsequences: false) {
            guard let v = part.removingPercentEncoding else { return false }
            seg.append(v)
        }
        guard let first = seg.first, !(seg.count == 1 && first.isEmpty) else { return false }
        switch first {
        case "users", "bindings", "projects", "teams", "kb", "grants", "audit", "core": return true
        case "quota": return seg.count == 2 && seg[1] == "history"
        case "usage": return seg.count == 2 && seg[1] == "events"  // GET /usage is the 0.5.x folder counter of the gateway
        default: return false
        }
    }
}

extension Router {
    /// A process of one of the gateway's own agents (a descendant of the app or sandboxed): may not use the member routes
    /// (L7, as for the admin API). Tests replace it.
    nonisolated(unsafe) static var agentPeer: @Sendable (_ peerPort: UInt16, _ routerPort: UInt16) -> Bool = {
        PeerProcess.refusal(peerPort: $0, routerPort: $1) != nil
    }

    /// The member routes (/minigw/v1/projects…, /minigw/v1/usage). What stays here, as in Go: the gateway token, the exclusion
    /// of requests that come through a run port, the agent-peer check, and the device PROVEN IN THIS TUNNEL with its approval
    /// now. NOT localApp, not deviceAllowed, not allowAnyDevice, not the loopback exceptions. Everything else is coreapi's.
    func memberHTTP(_ req: HTTPRequest, rid: String, writer: ResponseWriter) async -> Int {
        guard AgentRun.current == nil, authorized(req) else { return await coreReply(.noProjectAccess, rid: rid, writer: writer) }
        var cx = CoreContext(kind: .member)
        if RouterServer.isLoopback(req.peer), Self.agentPeer(req.peerPort, config.port) {
            Self.log.notice("\(rid, privacy: .public) project route refused: agent peer")
            cx.agentPeer = true
        }
        if let d = req.device {  // proven in this Noise tunnel; nil for plain HTTP
            cx.device = d.id
            cx.approved = config.devices.contains { $0.id == d.id }  // the approved list now
        }
        // The path as Go's net/http gives it (decoded). The body as Go reads it: at most MaxBody, more sets bodyTooLarge.
        var q = CoreRequest(method: req.method, path: req.path.removingPercentEncoding ?? req.path, query: req.query ?? "")
        if let k = req.header("idempotency-key"), !k.isEmpty { q.header["Idempotency-Key"] = [k] }
        q.body = req.body.prefix(CoreHelper.maxBody)
        q.bodyTooLarge = req.body.count > CoreHelper.maxBody
        return await coreReply(await core?.call(context: cx, request: q) ?? .unavailable, rid: rid, writer: writer)
    }

    /// The core's admin routes, after the admin authentication (adminHTTP), unchanged. Raw passthrough of the core's JSON.
    func coreAdmin(_ req: HTTPRequest, below: String, rid: String, writer: ResponseWriter) async -> Int {
        let r = await core?.call(context: .admin, request: CoreRequest(method: req.method, path: below, query: req.query ?? "", body: req.body)) ?? .unavailable
        if r == .unavailable {  // the helper itself is down: the gateway's own admin error shape (the helper's 503 is passed as is)
            return await adminError(503, "The project database is not available: the project helper (minigw-core) is not running", rid: rid, writer: writer)
        }
        return await coreReply(r, rid: rid, writer: writer, admin: true)
    }

    /// Writes a core response: its status, headers and body, plus the request id (as writeAPI in Go).
    func coreReply(_ r: CoreResponse, rid: String, writer: ResponseWriter, admin: Bool = false) async -> Int {
        var headers: [(String, String)] = []
        for (k, vs) in r.header where k.caseInsensitiveCompare("Content-Length") != .orderedSame {
            for v in vs { headers.append((k, v)) }
        }
        headers.append(("request-id", rid))
        if admin { headers.append(("Cache-Control", "no-store")) }
        try? await writer.write(HTTPResponse.head(status: r.status, headers: headers, contentLength: r.body.count) + r.body)
        return r.status
    }
}
