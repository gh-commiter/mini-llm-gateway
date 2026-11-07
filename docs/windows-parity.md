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
| – | Claude CLI · Allow commands without a sandbox; folder for agents without sync | `.allowUnsandboxedBash`, `.folder` | Windows has no sandbox for commands; minigw serve only. |
