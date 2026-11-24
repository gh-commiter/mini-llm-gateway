import Foundation

/// #125: an agent run (Claude CLI, Codex, Copilot, minigw run) gets only these variables of the gateway's environment, the names the owner added
/// (`RouterConfig.agentEnv`) and what the gateway sets for the run after the filter (MINIGW_*, CODEX_HOME, COPILOT_HOME, sandbox markers…).
/// The gateway's own environment may hold upstream tokens and provider keys. The same lists as client/agentenv.go
/// (client/testdata/agent-env.json). Names compare without regard to case (as on Windows: http_proxy and HTTP_PROXY both pass).
public enum AgentEnv {
    static let baseNames = [
        "PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "LANG", "TZ", "TMPDIR", "TMP", "TEMP",
        "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
        "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
        "JAVA_HOME", "MAVEN_HOME", "M2_HOME", "MAVEN_OPTS", "GOPATH", "GOROOT", "GOMODCACHE", "GOCACHE", "GOFLAGS", "GOPROXY",
        "CARGO_HOME", "RUSTUP_HOME", "NVM_DIR", "PNPM_HOME", "PYENV_ROOT", "VIRTUAL_ENV", "ANDROID_HOME", "DOTNET_ROOT", "DEVELOPER_DIR", "SDKROOT",
        "GOPRIVATE", "GONOSUMDB", "GONOPROXY", "GRADLE_USER_HOME", "NVM_HOME", "NVM_SYMLINK",  // folders and module routing (no code loading, no credentials)
        "CLAUDE_CONFIG_DIR",  // claude's login folder when the user moved it; without it the agent is "not logged in"
        // Windows (the Go gateway): what a process needs to start and find the user's folders
        "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA",
        "PROGRAMW6432", "COMMONPROGRAMW6432", "NUMBER_OF_PROCESSORS", "OS", "USERNAME", "USERDOMAIN", "COMPUTERNAME", "PUBLIC", "PSMODULEPATH",
    ]
    static let basePrefixes = ["LC_", "XDG_", "PROGRAMFILES", "COMMONPROGRAMFILES", "PROCESSOR_"]
    /// The denylist on top (also for the owner's names): routing back into this router, key agents, GitHub tokens, every COPILOT_*, sandbox markers, API keys.
    static let denyNames = [
        "CLAUDECODE", "DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK", "GPG_AGENT_INFO", "GNOME_KEYRING_CONTROL", "CODEX_HOME", "GROK_HOME",
        "MINIGW_AGENT_URL", "MINIGW_AGENT_AUTH_FILE", "GH_TOKEN", "GITHUB_TOKEN",
        "SANDBOX_RUNTIME", "MINIGW_SANDBOXED", "CODEX_SANDBOX", "CODEX_SANDBOX_NETWORK_DISABLED", "MINIGW_DEPS", "MINIGW_NETWORK_OFF",
        "OPENAI_API_KEY", "CODEX_API_KEY", "XAI_API_KEY", "GROK_CODE_XAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY",
        "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "CLAUDE_ENV_FILE", "BASH_ENV", "ENV",
    ]
    static let denyPrefixes = ["ANTHROPIC_", "CLAUDE_CODE_", "COPILOT_", "GITHUB_COPILOT_", "MINIGW_"]
    static let denySuffixes = ["_API_KEY"]
    static let maxNames = 64

    public static func denied(_ name: String) -> Bool {
        let n = name.uppercased()
        return denyNames.contains(n) || denyPrefixes.contains { n.hasPrefix($0) } || denySuffixes.contains { n.hasSuffix($0) }
    }

    /// The environment of an agent run from `env` (the gateway's): the base list and the owner's names, without a denied name.
    public static func environment(_ env: [String: String] = ProcessInfo.processInfo.environment, owner: [String]) -> [String: String] {
        let own = Set(owner.map { $0.uppercased() })
        return env.filter { k, _ in
            let n = k.uppercased()
            return (baseNames.contains(n) || basePrefixes.contains { n.hasPrefix($0) } || own.contains(n)) && !denied(n)
        }
    }

    /// The names in the Settings field (`RouterConfig.agentEnv`: separated by commas or spaces).
    public static func names(_ field: String) -> [String] {
        field.split(whereSeparator: { $0 == "," || $0.isWhitespace }).map(String.init)
    }

    /// The owner's names that `env` (the gateway's own environment) does not hold: they reach no agent (GET agents/env "unset"; Go's agentEnvUnset).
    public static func unset(_ names: [String], env: [String: String] = ProcessInfo.processInfo.environment) -> [String] {
        let have = Set(env.keys.map { $0.uppercased() })
        return names.filter { !have.contains($0.uppercased()) }
    }

    /// What Settings stores for the field: the typed text when it is valid, else the stored one (an invalid entry is not saved, as in the admin API).
    public static func saved(_ typed: String, stored: String) -> String {
        validate(names(typed)).error == nil ? typed : stored
    }

    /// The owner's names, checked (as validAgentEnv in Go); duplicates are dropped. On failure the reason (the same text in Go).
    public static func validate(_ names: [String]) -> (names: [String], error: String?) {
        var out: [String] = []
        for n in names {
            if n.range(of: "^[A-Za-z_][A-Za-z0-9_]*$", options: .regularExpression) == nil {
                return ([], "agentEnv: \"\(n)\" is not a variable name (letters, digits and _, not starting with a digit)")
            }
            if denied(n) { return ([], "agentEnv: \(n) is never passed to agents (API keys, tokens, sign-in, sandbox and gateway variables)") }
            if !out.contains(n) { out.append(n) }
        }
        return out.count > maxNames ? ([], "agentEnv: at most 64 names") : (out, nil)
    }
}
