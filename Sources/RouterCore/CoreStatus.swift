import Foundation

/// The `core` field of the admin status and the line of `minigw gw status` / the Settings window about the projects: the Mac's
/// counterpart of Go's server.coreStatus (client/serve_core_sandbox.go): "projects: on" or "projects: off (why)", "" = nothing to say
/// (no helper configured). The Go-only variants about agents that can read the notes do not exist here (the agents are sandboxed).
public enum CoreStatus {
    /// The start of the helper's own 503 text for an admin call while its store cannot be used (coreapi.Admin).
    static let storePrefix = "The project database is not available: "

    /// The reason in the helper's 503 (an admin call): nil when it gives none (the helper itself is down).
    public static func storeReason(status: Int, body: Data) -> String? {
        guard status == 503, let m = JSONValue.parse(body)?.dict["error"]?.dict["message"]?.string, m.hasPrefix(storePrefix) else { return nil }
        let why = String(m.dropFirst(storePrefix.count))
        return why.isEmpty ? nil : why
    }

    public static func line(helperUp: Bool, failure: String, fatal: CoreError?, storeReason: String?, linkProblem: String?, starting: Bool = false, startingNote: String = "") -> String {
        switch fatal {
        case .versionMismatch(let why)?: return "projects: off (" + why + ")"
        case .peerNotAuthentic?: return "projects: off (the project helper could not prove that it is the one shipped with the app; it was not used)"
        default: break
        }
        if !helperUp && starting && !startingNote.isEmpty {  // the helper said what it is doing: an upgrade, which can take minutes
            return "projects: starting (" + startingNote + "; this can take a few minutes, do not quit the app)"
        }
        if !helperUp && starting {  // the helper process is up and opens (and migrates) the database: not the same as stopped or failed
            return "projects: starting (the project database is being opened or migrated; try again in a few seconds)"
        }
        if !helperUp {
            let why = failure.hasPrefix("unavailable(\"") && failure.hasSuffix("\")") ? String(failure.dropFirst(13).dropLast(2)) : failure  // CoreError's description
            return "projects: off (the project helper (minigw-core) is not running" + (why.isEmpty ? "" : ": " + why) + ")"
        }
        if let storeReason { return "projects: off (" + storeReason + ")" }
        if let linkProblem { return "projects: on; agent runs are refused: " + linkProblem }
        return "projects: on"
    }

    /// The line for a helper: the admin status and the Settings window both use it.
    public static func line(for core: CoreHelper) async -> String {
        var reason: String?
        if core.isReady {  // a helper whose store cannot be opened is up and answers its admin calls with 503 and the reason
            let r = await core.call(context: .admin, request: CoreRequest(method: "GET", path: "/users"))
            reason = storeReason(status: r.status, body: r.body)
        }
        return line(helperUp: core.isReady, failure: core.lastFailure, fatal: core.fatalError, storeReason: reason,
                    linkProblem: RouterConfig.coreLinkProblem(store: core.store), starting: core.isStarting, startingNote: core.startingDetail)
    }
}

extension Router {
    /// "" when no helper is configured (as Go's "" for an offline status).
    func coreStatusLine() async -> String {
        guard let core else { return "" }
        return await CoreStatus.line(for: core)
    }
}
