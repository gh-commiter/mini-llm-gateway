package main

import (
	"regexp"
	"slices"
	"strings"
)

// #125: an agent run (claude, codex, grok, copilot) gets only these variables of the gateway's environment, the names
// the owner added (serveConfig.AgentEnv) and what the gateway sets for the run after the filter (MINIGW_*, CODEX_HOME,
// GROK_HOME, COPILOT_HOME, an explicit agent key, sandbox markers). A gateway started from a shell or by a service manager
// may hold upstream tokens and provider keys in its environment. The same lists as AgentEnv.swift
// (client/testdata/agent-env.json). Names compare without regard to case (Windows; http_proxy and HTTP_PROXY both pass).
var agentEnvBase = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "LANG", "TZ", "TMPDIR", "TMP", "TEMP",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
	"JAVA_HOME", "MAVEN_HOME", "M2_HOME", "MAVEN_OPTS", "GOPATH", "GOROOT", "GOMODCACHE", "GOCACHE", "GOFLAGS", "GOPROXY",
	"CARGO_HOME", "RUSTUP_HOME", "NVM_DIR", "PNPM_HOME", "PYENV_ROOT", "VIRTUAL_ENV", "ANDROID_HOME", "DOTNET_ROOT", "DEVELOPER_DIR", "SDKROOT",
	"CLAUDE_CONFIG_DIR", // claude's login folder when the user moved it (the quota probe passes it too); without it the agent is "not logged in"
	// Windows: what a process needs to start and find the user's folders (ProgramW6432: the 64-bit Program Files a 32-bit process sees)
	"SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA",
	"PROGRAMW6432", "COMMONPROGRAMW6432", "NUMBER_OF_PROCESSORS", "OS", "USERNAME", "USERDOMAIN", "COMPUTERNAME", "PUBLIC", "PSMODULEPATH",
}

// agentEnvBasePrefixes: locale, XDG folders (XDG_RUNTIME_DIR, XDG_CONFIG_HOME: where Linux CLIs keep their login),
// Windows' Program Files (x86 too) and processor variables.
var agentEnvBasePrefixes = []string{"LC_", "XDG_", "PROGRAMFILES", "COMMONPROGRAMFILES", "PROCESSOR_"}

// The denylist (before #125 the only filter): variables that would point the inner CLI back at this router, key agent
// sockets and the D-Bus session (N2: SSH, GPG, GNOME Keyring, Secret Service), GitHub tokens and every COPILOT_*
// (COPILOT_ALLOW_ALL, COPILOT_PROVIDER_API_KEY_COMMAND …), sandbox markers (#67) and API keys (#68: billing; only an
// explicit key, agentKeyEnv). It applies on top of the allowlist, also to names the owner added.
var agentEnvDenyNames = []string{
	"CLAUDECODE", "DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK", "GPG_AGENT_INFO", "GNOME_KEYRING_CONTROL", "CODEX_HOME", "GROK_HOME",
	"MINIGW_AGENT_URL", "MINIGW_AGENT_AUTH_FILE", "GH_TOKEN", "GITHUB_TOKEN",
	"SANDBOX_RUNTIME", "MINIGW_SANDBOXED", "CODEX_SANDBOX", "CODEX_SANDBOX_NETWORK_DISABLED", "MINIGW_DEPS", "MINIGW_NETWORK_OFF",
	"OPENAI_API_KEY", "CODEX_API_KEY", "XAI_API_KEY", "GROK_CODE_XAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY",
}

// M1: GITHUB_COPILOT_PROMPT_MODE_REPO_HOOKS (… _WORKSPACE_MCP, _EXTENSIONS) loads the repository's hooks.
var agentEnvDenyPrefixes = []string{"ANTHROPIC_", "CLAUDE_CODE_", "COPILOT_", "GITHUB_COPILOT_"}
var agentEnvDenySuffixes = []string{"_API_KEY"}

const agentEnvMax = 64

var agentEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func agentEnvDenied(name string) bool {
	n := strings.ToUpper(name)
	return slices.Contains(agentEnvDenyNames, n) || slices.ContainsFunc(agentEnvDenyPrefixes, func(p string) bool { return strings.HasPrefix(n, p) }) ||
		slices.ContainsFunc(agentEnvDenySuffixes, func(s string) bool { return strings.HasSuffix(n, s) })
}

// agentEnv: the denylist alone (kept as the second filter; agentEnvFor applies the allowlist first).
func agentEnv(env []string) []string {
	var out []string
	for _, e := range env {
		if name, _, _ := strings.Cut(e, "="); !agentEnvDenied(name) {
			out = append(out, e)
		}
	}
	return out
}

// agentEnvFor: the environment of an agent run from env (os.Environ()): the base list and the owner's names, then the
// denylist. The gateway appends its own run variables after it.
func agentEnvFor(env, owner []string) []string {
	var out []string
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		n := strings.ToUpper(name)
		if slices.Contains(agentEnvBase, n) || slices.ContainsFunc(agentEnvBasePrefixes, func(p string) bool { return strings.HasPrefix(n, p) }) ||
			slices.ContainsFunc(owner, func(o string) bool { return strings.EqualFold(o, name) }) {
			out = append(out, e)
		}
	}
	return agentEnv(out)
}

// validAgentEnv: the owner's names, checked (as AgentEnv.validate on the Mac); duplicates are dropped.
func validAgentEnv(names []string) ([]string, string) {
	out := []string{}
	for _, n := range names {
		switch {
		case !agentEnvName.MatchString(n):
			return nil, `agentEnv: "` + n + `" is not a variable name (letters, digits and _, not starting with a digit)`
		case agentEnvDenied(n):
			return nil, "agentEnv: " + n + " is never passed to agents (API keys, tokens, sign-in, sandbox and gateway variables)"
		case !slices.Contains(out, n):
			out = append(out, n)
		}
	}
	if len(out) > agentEnvMax {
		return nil, "agentEnv: at most 64 names"
	}
	return out, ""
}
