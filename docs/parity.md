# Parity: macOS app, Windows, Linux, Docker

State of beta 0.7.25, by feature group. Columns: the **macOS app** (the Mini Gateway app), **Windows** and **Linux** (both `minigw serve`) and **Docker** (`minigw serve` in the Compose container). The cells say what is built, not what has been tried; a try on a real machine is named in the Notes column and in the Known problems of the [release notes](release-notes.md). The control-by-control list of the settings is [windows-parity.md](windows-parity.md).

Cell values: **yes** (built), **no**, **by design** (left out on purpose); "not tried" next to a yes means built but never run on this platform.

| Feature group (released in) | macOS app | Windows | Linux | Docker | Notes |
|---|---|---|---|---|---|
| Projects, members, grants, usage events, quota (0.6.0) | yes (database in the helper `minigw-core`) | yes | yes | yes | Windows and Linux agents without a sandbox can read the notes: the owner must accept that first (`gw claude --accept-agents-read-notes`); the Mac's agents are sandboxed. |
| Teams and projects: users, teams, members, access, path labels, git identity | yes | yes | yes | yes | `minigw gw ...` on all four. |
| Knowledge base: search, submit, review, faq, team notes | yes | yes | yes | yes | The member commands run in `minigw` on any computer. Only the owner reviews; project admins cannot. |
| KB export and import, copies of a team between gateways, KB tools over MCP (`minigw kb mcp`), hooks (`minigw kb hooks`) | yes | yes | yes | yes | Client side: the platform that matters is the colleague's computer. `kb export` on Windows and the hooks in a real Claude Code session are not tried. |
| Usage labels (`--minigw-phase\|activity\|task`, `gw usage --by`) | yes | yes | yes | yes | Commands only; no pane groups by label on any GUI. Nothing classifies unlabelled calls yet. |
| Admin panels: Users and devices, Teams and projects, Knowledge base, Upstream gateways | yes: native panes (tried only through tests) | yes: settings page (tried in a headless Chromium on one gateway) | yes: settings page (not tried) | no: `docker compose exec ... minigw gw`; the remote page has no core panels | One panel registry draws all of them; same routes on both renderers. |
| Upstream gateways A: add, link, dry run, push as submissions | yes | yes | yes | yes | Tried once between two real gateways (Mac to Windows). |
| Upstream gateways B: automatic push, publish mode, conflicts, `resolve --theirs\|--ours` | yes (the app's timer asks the helper) | yes | yes (not tried) | yes (not tried) | Tried Mac to Windows on 2026-11-18 (auto push, publish, a conflict, `resolve --ours`). One way: nothing is pulled. |
| Remote settings: HTTPS listener, mutual TLS, password login (0.7.24) | by design: the admin UI is the native app | yes | yes (not tried) | yes, through the Compose admin setup | Providers, models and devices only. |
| `gw settings password` (0.7.24) | by design (the Mac has no remote listener) | yes | yes | yes | Exists only in `minigw serve`. Tried on one Windows gateway. |
| Docker Compose deployment: image, volumes, `compose.admin.yaml` | - | - | - | yes | Tried on one Mac (arm64, amd64 by emulation); not on a team server. Agents are not supported in the container. |
| Agents: Claude CLI, Codex CLI | yes (Seatbelt sandbox) | yes (Windows sandbox; Codex needs "Allow commands without a sandbox") | yes (`bwrap`) | no | |
| Agent: Copilot CLI | yes | no (not supported on a Windows gateway) | yes (`bwrap`), not on a headless Linux gateway | no | |
| Agent: Grok CLI | by design: not supported on the Mac gateway (owner decision, 2026-11-16); xAI works as a provider | yes | yes | no | |
| Siri AI, Apple Foundation Models, WebDAV folder mount, native mode, `minigw run` | yes | no | no | no | Mac only, tied to macOS. Foundry Local is the Windows/Linux local-model provider. |
| Subscription quota (`gw quota`) and its Claude probe switch | yes | yes (command) | yes (command) | yes (command) | The "Show the subscription quota" switch is only in the Mac Settings window; elsewhere `gw quota --claude-probe=on\|off`. |
| Plugin-supplied panels | no | no | no | no | Format, texts and renderers exist; no plugin provider does yet (only `builtin`). |
