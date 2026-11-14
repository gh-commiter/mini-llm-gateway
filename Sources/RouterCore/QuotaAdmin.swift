import Foundation

extension Router {
    /// The quota report of this process (the numbers a Codex run recorded outlive a Router restart; tests replace it).
    nonisolated(unsafe) static var quota = QuotaEngine()

    /// The Codex home of the gateway's user (where the session logs of a run without a project are).
    static var realCodexHome: String { ProcessInfo.processInfo.environment["CODEX_HOME"].flatMap { $0.isEmpty ? nil : $0 } ?? NSHomeDirectory() + "/.codex" }

    /// GET and PATCH /quota (client/serve_core_admin.go quotaAdmin): the report and the opt-in of the Claude probe. Needs no core.
    func quotaAdmin(_ method: String, body: Data, hooks: AdminHooks) async throws -> (Int, Any) {
        var c = config
        if method == "PATCH" {  // {"claudeProbe": bool}
            guard let o = (try? JSONSerialization.jsonObject(with: body)) as? [String: Any], o.keys.sorted() == ["claudeProbe"],
                  let n = o["claudeProbe"] as? NSNumber, CFGetTypeID(n) == CFBooleanGetTypeID() else { throw AdminError(400, "bad JSON body (claudeProbe)") }
            let on = n.boolValue
            c = try await hooks.update { $0.claudeQuotaProbe = on }
        }
        let inputs = QuotaInputs(claudeProbe: c.claudeQuotaProbe, claudeExe: await ClaudeCLI.executable(c.claudeCLI.executable), claudeEnabled: c.claudeCLI.enabled,
                                 providers: c.providers.map { .init(id: $0.id, name: $0.name, baseURL: $0.baseURL, enabled: $0.enabled) },
                                 key: { [secrets] in secrets.get($0) },
                                 tmpParent: GatewayHistory.defaultFile.deletingLastPathComponent().appendingPathComponent("quota-tmp", isDirectory: true))
        let values = await Self.quota.report(inputs)
        return (200, ["quota": values.map(\.json), "claudeProbe": c.claudeQuotaProbe])
    }
}
