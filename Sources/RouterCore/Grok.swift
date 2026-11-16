import Foundation

/// Grok is not supported on the Mac gateway (it stays available through `minigw serve` on Windows and Linux): every run is refused.
public enum Grok {
    static let prefix = "claude-grok-"

    /// The model name is Grok's (`claude-grok`, `claude-grok-<model>`).
    static func isGrok(_ model: String) -> Bool { model == "claude-grok" || (model.hasPrefix(prefix) && model.count > prefix.count) }

    static let notSupported = "Grok is not supported on the Mac gateway. Use another agent, or Grok through minigw serve on Windows or Linux."
}

extension Router {
    /// Ključ za nadaljevanje seje agenta: naprava + mapa odjemalca. ID seje routerja in pot priklopa
    /// se pri `minigw -c`/`--resume` zamenjata, zato nista v ključu.
    /// 0.6.0: the parts of the key; `conversationScope` joins them (no project: the 0.5.26 key byte for byte) or gives the project's namespace.
    static func agentScopeFields(_ provider: String, _ req: HTTPRequest, cwd: URL) -> [String] {
        [provider, req.device?.id ?? "local", req.header("x-claude-mini-cwd") ?? cwd.path]
    }

    /// The refusal of an agent request whose project the core cannot serve any more (the helper went down after admission): it does not run.
    func projectDown(rid: String, writer: ResponseWriter) async -> Int {
        guard case .refused(let s, let t, let m, _) = Self.projectUnavailable else { return 503 }
        return await refuseProject(s, t, m, retryAfter: 0, rid: rid, writer: writer)
    }

    /// 400, like the other permanent refusals (a 502/503 would get "try again" from the client), and no retry.
    func refuseGrok(rid: String, writer: ResponseWriter) async -> Int {
        await error(400, "invalid_request_error", Grok.notSupported, rid: rid, extra: [("x-should-retry", "false")], writer: writer)
    }
}
