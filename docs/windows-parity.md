# Mac Settings and the Windows/Linux settings page

Every control of the Mac app's Settings window (Sources/MiniGateway/Settings.swift) and its counterpart in
`minigw serve` (the settings page `/minigw/settings`, the admin API in [admin-api.md](admin-api.md) and
`serve.json`). Siri AI and Apple Foundation Models exist only on the Mac.

| Mac pane · control | Windows/Linux | API field | Note |
|---|---|---|---|
| Sidebar: search, items with icons, + (LiteLLM, OpenRouter, other) / − | same | – | Devices is its own pane (on the Mac it is part of Siri AI). |
| General · Open at Login | General · Start at sign-in | `PATCH general {startAtSignIn}` | Windows: Run value for the tray. Linux: `minigw serve install`. |
| General · Address, Port | same | `general.address`, `general.port` | Address only empty, `0.0.0.0`, `::` or `127.0.0.1` (the admin API needs 127.0.0.1); a change needs a restart. |
| General · Allowed clients (IP; empty = all) | same | `general.allowedClients` | Connections from other IPs are closed; this computer is always allowed. |
| General · Allow unencrypted connections from other Macs | Allow unencrypted connections from other computers | `general.allowPlainRemote` | Such clients have no device key: they also need "Allow any device" (Devices). |
| General · Token: Show, Copy Token, New Token | same | `GET general.connect`, `POST general/token` | A new token also ends settings page sessions. |
| General · Scratch folder: Choose…, Default, Show in Finder | Scratch folder: path field, Default | `general.scratch` | A browser cannot pick folders or open Explorer; a new folder applies to new sessions, existing copies are not moved. |
| General · About: Version (build), License | same | `status.version`, `status.build` | |
| Gateway · Enabled (off without a key) | same | `PATCH gateways/{name} {enabled}` | |
| Gateway · Name, Base URL | same | `PATCH gateways/{name} {name, baseURL, insecure}` | Renaming moves the key; `http` only to localhost or a private IP with "Allow http". |
| Gateway · API key (saved; enter to replace) | same | `POST gateways {name, baseURL, key, insecure}` | Key in Credential Manager / Secret Service, never shown. |
| Gateway · Models: search, N of M enabled, Enable All, Disable All, refresh, a switch per model, context size | same | `GET/POST gateways/{name}/models` | Enable All = all, also future models. |
| Gateway · Aliases (advanced) | same | `PATCH gateways/{name} {aliases}` | `alias = upstream ID, window` per line; when set they define the models. |
| Sidebar + / − for gateways | same | `POST gateways`, `DELETE gateways/{name}` | |
| Siri AI · Devices (Siri per device) | Devices: allowed, waiting (Allow/Deny), Remove, Allow any device | `devices`, `general.allowAnyDevice` | minigw serve has an allow-list; the Mac admits every device with the token. |
| Claude CLI · Enabled | same | `agents/claude.enabled` | |
| Claude CLI · Executable, Choose…, detected path and version | Executable, Find, detected path and version | `agents/claude.path`, `.exe`, `.version` | A browser cannot pick files: the path is typed, Find auto-detects. |
| Claude CLI · Allowed folders (+/−) | same | `agents/claude.folders` | Absolute paths, not near the config folder; also `--add-dir` and the sandbox. |
| Claude CLI · Allow local tools, tool table (exclude) | same | `.allowLocalTools`, `.disabledTools`, `GET agents/claude/tools` | On Windows it needs "Allow commands without a sandbox" (no sandbox for Bash). |
| Claude CLI · Tools (text), Permission mode | Tools (switches), Permission mode | `.tools`, `.permissionMode` | Only without local tools, as on the Mac. |
| Claude CLI · Setting sources | same | `.settingSources` | Default empty on minigw serve (the Mac: `project,local`): hooks from a client's folder would run on the router. |
| Claude CLI · Command-line tool: Install, Download… | Command-line tool: status, Download… | `status.installDir` | Installed with `minigw serve install`. |
| Codex CLI · Enabled, Executable | same | `agents/codex.enabled`, `.path`, `.found` | Off by default. Models `claude-codex` (default model) and `claude-codex-<slug>`. Windows: the native `codex.exe` (also the one inside the npm package). |
| Codex CLI · Models: a switch per model, Enable All, Disable All | same | `agents/codex.models`, `GET agents/codex/models` | From `codex debug models`; null = all, also future ones. |
| Codex CLI · Allowed folders | same (shared with Claude CLI) | `agents/claude.folders` | Passed as `--add-dir` in workspace-write. |
| Codex CLI · Sandbox: Allow writing, Allow network access | same | `.write`, `.network` | `workspace-write` with `sandbox_workspace_write.network_access`, else `read-only`; resume with `codex exec resume <thread>`. Network is off by default (Mac and minigw serve). On Linux Codex uses Landlock and seccomp, which do not limit reads, so the router also refuses admin requests from agent processes, and Codex and Grok run under `bwrap` with the home and config folders empty (tmpfs), only the program folders (read-only), `~/.codex` or `~/.grok`, the synced copy and the allowed folders mounted back, and without the D-Bus session (Secret Service, `/tmp/dbus-*`). Without a working `bwrap` they run only with "Allow commands without a sandbox", as on Windows. Sync mode only, also for requests without tools (then read-only). |
| Grok CLI · Enabled, Executable | same | `agents/grok.enabled`, `.path`, `.found` | Off by default. Models `claude-grok` (default model) and `claude-grok-<slug>`. Windows: only a native `grok.exe` (no npm/cmd wrapper). |
| Grok CLI · Models: a switch per model, Enable All, Disable All | same | `agents/grok.models`, `GET agents/grok/models` | From `grok models`; null = all, also future ones. |
| Grok CLI · Sandbox: Allow writing | same | `.write` | `--sandbox workspace --always-approve`, else `--sandbox read-only`; resume with `-r <session>`. Grok has no network switch (`PATCH {network}` is 400); read-only also has `--disable-web-search`. The config folder is also denied with `--deny Read/Edit/Write(<config>/**)`; that is defence in depth only (Grok matches paths in its tools and commands, an obfuscated path can get past it): the protection is bwrap on Linux, the opt-in on Windows and the admin API check. Sync mode only. |
| – | Codex CLI, Grok CLI on Windows, and on Linux without `bwrap` | `agents/claude.allowUnsandboxedBash`, `status.agentsUnsandboxed` | Without "Allow commands without a sandbox" they do not run at all: there they can read and change everything the user can, including the router's settings. Devices that use Codex or Grok can read every file this user can read. |
| – | Claude CLI · Allow commands without a sandbox; folder for agents without sync | `.allowUnsandboxedBash`, `.folder` | Windows has no sandbox for commands; minigw serve only. |
