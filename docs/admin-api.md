# Mini Gateway admin API (v1)

One API to configure a gateway on its own computer. The Go gateway (`minigw serve`) and the Mac app implement it;
[Mac differences](#mac-app) are listed at the end. `minigw gw …` (and `minigw gw settings` in a terminal, through `minigw gw api METHOD PATH`, body on stdin), the Windows tray and the settings page use it. Changes apply at once,
except where a response says `"restartRequired": true`.

## Access

- Base path: `/minigw/admin/v1`, on the gateway's normal port.
- **Only from the gateway computer.** The server checks the TCP peer address of the connection (loopback), never
  a header. A request from another address gets `403`, even with the right secret.
- **Host header** must be `127.0.0.1:<port>`, `localhost:<port>` or `[::1]:<port>` (the port the connection came
  in on). Anything else gets `403`. This blocks DNS rebinding from a web page.
- **Admin secret, never sent:** 32 random bytes, base64url without padding (43 characters), in the file
  `admin.secret` (mode 0600) in the gateway's config folder (the folder agents may not read; the Mac app keeps it in
  its Keychain instead, see [Mac app](#mac-app)). The gateway creates it on first start. A client speaks MGW2 (Noise NK) to `127.0.0.1` with the gateway's public key and sends
  `Authorization: MGW-Admin <base64url(HMAC-SHA256(key = the secret as stored, "minigw-admin-v1" || h))>`, where
  `h` is that connection's Noise handshake hash. The gateway computes the same value from its own `h` and compares
  in constant time. A fake server (its own key, its own handshake) gets a proof that is valid nowhere else; a
  request without Noise, or with `Bearer`, gets `401`. The gateway's client token is not the admin secret.
- **The client finds the config without the environment:** `minigw gw` resolves the default config folder from
  the user database (Unix) or the RoamingAppData known folder (Windows), not from `HOME`, `XDG_CONFIG_HOME` or
  `%APPDATA%`, so another process cannot point it at a fake `serve.json` with a fake gateway key.
- **Settings page session** (instead of the secret), without cookies: browsers send a `127.0.0.1` cookie to every
  port, so any local web server would get it. The tray or `POST /settings/login` (below) opens
  `/minigw/settings#t=<one-time token>`; the token is in the fragment, so it never reaches a server in a request
  line. The page is static and holds no secret. Its script drops the fragment, sends
  `POST /minigw/settings/login {"t": …}` (valid 60 s, once) and gets `{"session": id}`, which it keeps in
  `sessionStorage` (scheme, host and port) and sends as `X-Minigw-Session` on every API call. A page from another
  origin cannot send that header without a preflight, which fails (no CORS). Missing or wrong: `401`. A session ends
  after 30 minutes without use, with `POST /minigw/settings/logout`, or when the gateway token is rotated.
- **Agents are refused** (Go gateway, Linux and Windows): an admin request (`MGW-Admin`, a settings page session, or
  `POST /minigw/settings/login`) from a process that the gateway started, or one of its descendants, gets `403`,
  even with the right secret.
  - Linux: the client's socket in `/proc/net/tcp{,6}` → the processes holding it → their parents up to PID 1.
    Every agent (claude, codex, grok) runs under a small helper (`minigw __minigw-agent`) that is a child
    subreaper: a descendant whose parent exits (double fork) moves to the helper, not to init, so the chain to the
    gateway stays. The helper reaps its own subtree and kills what is left when the agent ends. Under the systemd
    unit the unit's cgroup is checked too. The gateway is not dumpable (`PR_SET_DUMPABLE 0`), so agents cannot read
    its memory or environment.
  - Windows: every agent runs in one Job object of the gateway (created suspended, added, then resumed); the
    client's PID comes from `GetExtendedTcpTable` and `IsProcessInJob` decides.
  - If the process cannot be found or read, the request is refused.
- **Old names (until 0.5.14):** `/gateways…` is an alias of `/providers…`; responses also carry the old field names
  (`"gateways"` next to `"providers"`, `"gateway"` next to `"provider"`), and `minigw router …` is an alias of
  `minigw gw …`. They go away in a later release.
- Every admin API response has `Cache-Control: no-store`.
- Loopback skips the device check and encryption for normal API calls; the admin API still needs the secret.
- **Clients authenticate the gateway first.** Something else may listen on the port (the
  gateway is stopped, another program took it). The CLI therefore speaks MGW2 (Noise NK) to `127.0.0.1` with the
  gateway's public key from the config; a program without the gateway's private key cannot finish the handshake and
  never sees the secret or a provider key. A loopback Noise connection is still a loopback peer.
- The gateway must listen on `127.0.0.1` as well: `address` is empty (all), `0.0.0.0`, `::` or `127.0.0.1`.
- No CORS headers, on any response. A browser page cannot read responses or send the header without a preflight,
  which fails.
- Bodies are JSON, at most 1 MB. Unknown fields in a body are an error.

## Errors

Every error is JSON with an HTTP status of 400 or more:

```json
{"type": "error", "error": {"type": "invalid_request_error", "message": "port must be 1-65535"}}
```

`error.type` follows the status: 400 `invalid_request_error`, 401 `authentication_error`, 403 `permission_error`,
404 `not_found_error`, 413 `request_too_large`, 429 `rate_limit_error`, 5xx `api_error`. The message is meant for
the user.

## Types

```text
Device   {"id": string, "name": string, "address"?: string, "client"?: string, "time"?: string, "lastSeen"?: string}
         id: base64url of the device's 32-byte Ed25519 public key (43 characters), as the refusal message shows it.
         address, client (OS and minigw version, from newer clients) and time (RFC 3339, UTC) only for pending
         devices; lastSeen only for allowed devices (kept across restarts). name is a label
         (control and format characters removed, at most 64 characters); only the ID identifies a device.
Connection {"time": string, "address": string, "client"?: string}
         A connection is the first request of an allowed device after 10 minutes of silence; the last 20 are kept.
Session  {"id": string, "device"?: string, "deviceName"?: string, "deviceRemoved"?: true, "start": string,
          "last": string, "duration": number, "folder"?: string, "os"?: string, "role"?: string, "mode": string,
          "readOnly"?: true, "branch"?: string, "models": [Use]}
         id: the client's session ID (x-claude-mini-session; an ID over 128 bytes or with control characters is not
         recorded), or "t-…" for requests without one (per device, a new record after 10 minutes of silence). device absent = this computer without a device
         key. folder: the client's cwd without `.`, `..` and a trailing separator, Windows paths (`C:\…`, `\\server\…`)
         with `/`, case kept; it is the key for later per-project settings. os: the client's OS from its client info
         (macOS, Windows, Linux); the folder filter ignores case for macOS and Windows clients. role: the role of the
         latest request, empty clears it (`--minigw-role`, header `x-claude-mini-role`; 1-32 of `A-Z a-z 0-9 . _ -`,
         any other value is ignored).
         mode: `chat` (no shared folder), `direct` (Mac: the folder mounted over WebDAV), `sync` (a copy on the
         gateway), `native` (Mac: a git repository on the gateway's SMB share; branch: the agent's branch there), `native-direct` (Mac: agents work in the gateway's folder itself: `--minigw-work=native-direct`, or a folder without git), `worktree` (a clone; branch: the agent's
         branch of that request), `refused` (minigw serve: the client asked for direct, which it does not offer). duration in seconds.
         The last 200 sessions are kept, at most 50 per device (a device's oldest go first); before saving, the oldest
         go until the file is under 4 MB. Folder ≤ 1 KiB, branch ≤ 255 B (UTF-8, control, C1 and bidi
         characters removed). Never prompt or answer text.
Use      {"model": string, "role"?: string, "via"?: string, "readOnly"?: true, "tools"?: string, "cap"?: string, "requests": number,
          "errors": number, "refused": number, "seconds": number, "inputTokens": number, "outputTokens": number}
         One entry per model, role and via (sorted by them; at most 32, then "other"). model ≤ 64 B; a request for
         an unknown or refused model (404) counts as "unknown". via: the agent or chain that
         called it (agent calls, e.g. `claude` or `claude→codex`, taken from the gateway's run, never from a header;
         the nested request also keeps the caller's session, device and role); absent = the client. readOnly and tools: the latest effective restrictions: readOnly = a read-only share
         or `--minigw-ro` (header `x-claude-mini-ro`, also without a share; a nested request keeps its caller's); Claude CLI's
         tools (read-only: only Read, Grep, Glob, LS, WebSearch, WebFetch of them) or "local" (Allow local tools; read-only:
         "local,read-only"), Codex, Grok and Copilot's switches `write`, `network`, `commands` as they applied (empty = read
         only; read-only drops `write` and `commands`; Codex `network` only with `write`). An agent's run records what it
         applied, not values recomputed afterwards: also read-only from an agent without writing in worktree mode, and from
         the agent's `work` cap; `cap` (`sync` or `worktree`) is the cap that made the run read-only (`minigw gw sessions`
         prints "read-only (cap worktree)"). errors: 5xx and aborted; refused: 4xx; seconds:
         total request time (for agents their run time); tokens from the response's usage.
Project  {"device"?: string, "folder": string, "os"?: string, "day": string, "agent": string, "requests": number,
          "errors": number, "refused": number, "seconds": number, "inputTokens": number, "outputTokens": number}
         Counters per device, folder, UTC day (`YYYY-MM-DD`) and agent kind (`claude`, `codex`, `grok`, `copilot`, or
         `other` for provider models); they stay when sessions are dropped. folder: the session's folder (a nested agent
         call counts under its caller's project; at most 256 bytes); requests without a folder are not counted. Kept 31 days, at most 5000
         rows (the oldest day goes first), at most 1250 per device (its oldest first, so one device cannot push out the
         others; at the 4 MB file limit the device with the most rows loses its oldest first). Tokens are what the provider reports and are not checked. Never prompt or answer text.
Running  {"id": string, "time": string, "model": string, "step"?: string}
         A model request (`POST /v1/messages`) still running. id: its request ID; model: the client's model name
         (`"unknown"` when over 200 bytes); step: the agent's current step (a tool, a file it reads or a command with
         its arguments), never the agent's text. The Mac lists a request here before its device is checked.
Request  {"time": string, "method": string, "path": string, "model": string, "status": number, "ms": number,
         "bytes": number, "device"?: string, "deviceName"?: string}
         A finished request that reached a model (`/v1/messages`, `/v1/messages/count_tokens`; not admin, share or
         health requests). model: `"unknown"` for an unknown or refused model (404) or a name over 200 bytes, never the
         client's string (as in Session). time: its start (RFC 3339, UTC); bytes: the request body's size; device absent = this
         computer without a device key (as in Session). The last 200; `minigw serve` keeps them in memory (empty after
         a restart), the Mac app in its activity file (kept across restarts; entries from before 0.5.21 have
         `"bytes": 0` and no device). Never prompt or answer text; the step may contain a command's arguments.
Model    {"id": string, "model": string, "window"?: number, "enabled": bool}
         id: the ID at the provider; model: the name for /model (the id itself when it starts with "claude-" in any letter case,
         else "claude-" + id; the original id also works in requests); window: context window in tokens, if known.
Provider  {"name": string, "baseURL": string, "insecure": bool, "models": [string] | null, "hasKey": bool, "ready": bool,
          "enabled": bool, "aliases": string}
         models: enabled provider IDs; null = all discovered, now and later. ready: model list loaded.
Claude   {"path": string, "found": bool, "tools": string, "allowUnsandboxedBash": bool,
          "exe"?: string, "version"?: string, "off"?: string, "leftOut"?: [string],
          "leftOutUnchecked"?: [string], "offErrors"?: [string], "login"?: Login}
         path: "" = from PATH; tools: comma-separated tool names; exe: the
         claude that was found; version: its `claude --version` (both only when found; Go gateway only).
         off: why Claude CLI and Codex were turned off at startup (an allowed folder contains or is inside the scratch
         folder or Copilot's package cache), while Claude CLI is still off; cleared when Claude CLI or Codex is turned
         on again (Go gateway only: the Mac ignores such folders and keeps Claude CLI on). leftOut: those folders, still
         in serve.json (Go gateway only); cleared when the allowed folders are saved. leftOutUnchecked: those of them
         that cannot be checked (cannot be resolved); offErrors: the resolve errors in `off` (both Go gateway only,
         cleared with leftOut; the settings page builds the reason in its language from them).
Login    {"state": string, "hint": string, "hasToken"?: bool, "apiKey"?: bool, "apiKeyNote"?: string}
         Go gateway only (#68), in GET /agents/{claude,codex,grok,copilot}: the agent's login, never a value. state:
         Claude "set" / "not set" (the stored token); Copilot "signed in (copilot login)" / "not available: <reason>"
         (#81; also in the Mac app); Codex from ~/.codex/auth.json ("logged in (ChatGPT)",
         "logged in (API key)", "not logged in (codex login)"); Grok "logged in (may need to log in again in Grok after
         it refreshes)" / "not logged in (grok login)"; with an API key set "API key (billed through the provider's
         API)". hint: one line on how to get the login. hasToken: Claude; apiKey and apiKeyNote: Claude,
         Codex and Grok (the opt-in "Use an API key instead").
General  {"port": number, "address": string, "connect": string, "restartRequired"?: true, "allowedClients": string,
          "allowPlainRemote": bool, "allowAnyDevice": bool, "allowLocalApps": bool, "scratch": string,
          "scratchState"?: "in-use" | "stale", "scratchOthers"?: [{"name": string, "state": "in-use" | "stale"}]}
         address: "" = all addresses; connect: the "minigw connect …" line for clients (contains the token).
```

## Resources

| Method and path | Body | Response |
|---|---|---|
| `GET /status` | – | `{"running", "version", "port", "address", "installed", "devices": n, "pending": n, "claude": Claude, "providers": [Provider], "os", "installDir"?, "tray", "autostart", "agents": n, "agentsUnsandboxed", "agentSandbox"?}` |
| `GET /devices` | – | `{"devices": [Device], "pending": [Device], "allowAnyDevice": bool}` |
| `POST /devices` | `{"id", "name"}` | `{"device": Device, "added": bool}`; removes it from pending; without a name, the pending device's name |
| `PATCH /devices/{id}` | `{"name"}` | `{"device": Device}`; renames an allowed device |
| `DELETE /devices/{id}` | – | `{"device": Device}`; by ID only; a pending device is only removed from pending (`"pending": true`), which is not a block list. Removing an allowed device deletes its connection history; its sessions stay, with `"deviceRemoved": true` |
| `GET /devices/{id}/history` | – | `{"id", "name", "history": [Connection]}`, newest first; 404 for a device that is not allowed |
| `GET /sessions` | query `device`, `folder` (that folder and its subfolders) | `{"sessions": [Session]}`, newest first |
| `GET /sessions/{id}` | – | Session |
| `DELETE /sessions` | – | `{"cleared": true}`; clears the session history and the project counters (connection histories stay) |
| `GET /usage` | query `device`, `folder` (that folder and its subfolders), `days` (1-31, default 7; else 400 "days: 1-31") | `{"usage": [Project]}`: today and the days before, newest day first, then folder, device, agent |
| `GET /activity` | – | `{"inflight": [Running], "requests": [Request]}`, newest first: what the Mac app's Activity window shows |
| `GET /providers` | – | `{"providers": [Provider]}` |
| `POST /providers` | `{"name", "baseURL", "key", "insecure"}` | `{"provider": Provider, "keyStore": string, "models": [Model]}` |
| `PATCH /providers/{name}` | any of `{"name", "enabled", "baseURL", "insecure", "aliases"}` | `{"provider": Provider}`; a new name moves the key; the key stays for a new URL |
| `DELETE /providers/{name}` | – | `{"name"}`; also deletes the key |
| `GET /providers/{name}/models?filter=text` | – | `{"models": [Model]}`; filter: part of `model`, any case |
| `POST /providers/{name}/models` | `{"all"?: bool, "enable"?: [id or model], "disable"?: [id or model]}` | `{"models": [Model], "changed": [Model]}` |
| `GET /agents/claude` | – | Claude |
| `GET /agents/claude/tools` | – | `{"tools": [string]}`: the tools of this Claude Code (for the exclude table with local tools) |
| `PATCH /agents/claude` | any of `{"path", "tools", "allowUnsandboxedBash", "token", "apiKey"}` | Claude |
| `GET /general` | – | General |
| `PATCH /general` | any of `{"port", "address", "startAtSignIn", "allowedClients", "allowPlainRemote", "allowAnyDevice", "allowLocalApps", "scratch", "worktreeNetwork"}` | General; `"restartRequired": true` for port or address; `allowAnyDevice: true` only while no device is allowed (400) |
| `POST /general/token` | – | General with the new token in `connect`; the old token stops working at once |
| `POST /general/scratch/move` | `{"to": "D:\\minigw-scratch"}` (a base folder; empty: the proposed default) | Go gateway (0.5.23): moves the whole Scratch folder to `<to>/<instance>` (0.5.24; with the owner file) and switches `scratch`; while the Scratch folder belongs to another gateway (`scratchState`) nothing is moved, the gateway starts its own folder there; General (without `connect`), plus `warning` when the old folder could not be removed. 409 while minigw sessions or agents are active, 400 for a folder the Scratch rules refuse or that is not empty, 500 when the move failed (the old Scratch stays). Client requests during the move get 503 |
| `POST /general/scratch/take` | – or `{"name"}` (a folder from `scratchOthers`) | Both gateways (0.5.24): Take over. Without `name` only when `scratchState` is `stale`; with `name` (only a name: a real folder directly in the base, another gateway's, not `*.old-*`, else 400) the stale sibling, for example after a reinstall. The other gateway's folder is renamed to `<base>/<instance>` of this gateway and gets its owner ID; this gateway's current folder (and one already named `<instance>`) is deleted when it has no conversations, else kept as `<instance>.old-<date>` (named in `warning`); nothing is merged. Logged. General (without `connect`). 409 when the folder is this gateway's own, `in-use` (the other gateway runs), or minigw sessions or agents are active; 500 when the rename failed (the folder stays) |
| `POST /settings/login` | – | `{"url"}`: a one-time link to the settings page (`…/minigw/settings#t=…`, 60 s); `503` when the gateway does not run |

Notes:

- A remote client without a device key (minigw before 0.4.4, unencrypted clients) is allowed only while the device list
  is empty and `allowAnyDevice` is on; otherwise 403 "This gateway needs a device key". The same on the Mac and Go.
- `allowLocalApps` (default off): `GET /v1/models` and `POST /v1/messages` (and `count_tokens`) need no token when the
  client is a process of the gateway's own user on this computer (loopback; macOS libproc, Linux `/proc/net/tcp`,
  Windows the TCP table and the process SID), not one of the gateway's agents, sends `anthropic-version` or `x-api-key`,
  has a loopback `Host` and no `Origin`. Everything else still needs the token (401); device rules are unchanged.
  Apps calling from a web view send `Origin` and need the token. Sandboxed processes of this user need the token, as
  the gateway's agents do: on the Mac App Sandbox and Seatbelt (including commands in Claude Code's sandboxed Bash), on
  Linux Flatpak (`/.flatpak-info` in the process's root, or an unreadable root) and snap (`snap.*` cgroup), on Windows
  AppContainer (`TokenIsAppContainer`). Docker and other containers, VMs and tunnels (`ssh -R`, ngrok…) running as you also count as this user's apps: with the switch on they can use agents in your allowed folders without a token. Browser extensions with permission for localhost can remove `Origin` and add headers, so they can use it too.
  `minigw serve` on macOS (development only) never skips the token.
- Last seen, connections and sessions are kept on the gateway's computer in a 0600 file next to the settings
  (`state.json` beside `serve.json`; on the Mac in Application Support/Mini Gateway), written at most once a minute and
  when the gateway stops. A larger file than 4 MB is not read.
- `POST /devices` takes only a valid device ID. `pending` lists the last 20 devices that were refused since the
  gateway started (in memory), newest last.
- `POST /providers` adds or replaces a provider. Name: letters, digits, `. _ -`, up to 32. `baseURL` without `/v1`;
  it must be `https`, or `http` to localhost, a private or loopback IP or a `.local` name (not a name without a dot).
  A `.local` name or a private address is no proof that the provider is on your LAN (another network, a VPN or a
  spoofed mDNS answer can answer it), so `http` also needs the explicit `"insecure": true` / Allow http switch
  with `"insecure": true`. LiteLLM and OpenRouter are built in: always listed, `DELETE` and renaming are refused. The gateway lists the models
  with the key before it saves anything, and stores the key in the system's secret store (Credential Manager,
  Keychain or Secret Service), never in the config. `keyStore` says where. A new provider with more than 20 models
  starts with none enabled. The key is never returned.
- `status`: `os` is `windows`, `linux` or `darwin` (the settings page uses it for commands without a sandbox),
  `installDir` the folder of `minigw serve install` (only when minigw is installed there), `tray` whether the
  Windows tray runs. `autostart` is `"boot"` (Linux with linger: the gateway starts without anyone signing in),
  `"sign-in"`, or `""` (it does not start on its own). `agents` counts the claude, codex and grok processes that
  run right now; an update stops them, so check it first. `agentsUnsandboxed` (Go gateway): Codex and Grok have no sandbox here that hides the gateway's secrets
  (Windows without the Windows sandbox, Linux without a working `bwrap`), so they run only with `allowUnsandboxedBash`.
  `scratchSuggest` (Go gateway on Windows, 0.5.23): the proposed Scratch folder when the current one is inside the home folder, else absent.
  `scratch` names a base folder in `PATCH /general` and the move (0.5.24): the gateway's Scratch is `<base>/<instance>`; `GET` returns the folder in use.
  `scratchState` (both gateways, 0.5.24): absent when the Scratch folder is this gateway's (its owner file holds this gateway's instance ID, or there is none yet); `"in-use"`: it belongs to another gateway that is running (holds its lock); `"stale"`: another gateway that is not running. In both cases agents are off (client requests and the probes `GET /agents/claude/tools` and `GET /agents/{codex,grok}/models` get 503 naming the Scratch setting) and nothing in the folder is cleaned up.
  `scratchOthers` (both gateways, 0.5.24): other gateways' folders next to the Scratch folder in its base (a real folder with another instance's owner file; `*.old-*` and folders without an owner file are left out), with the same states; absent when there are none. `connect` (the line with the gateway token) is only in `GET /general`; `PATCH /general` and `POST /general/token` leave it out (Go gateway). `agentSandbox` (Go gateway on Windows, #68): `{"kind": "mxc", "version": "1.0.1"}` when agents run in the Windows
  sandbox (plus `warning` when Scratch is inside the home folder), else `{"kind": "", "reason": "…"}`. When the sandbox cannot reach 127.0.0.1 it also has
  `"askOthers": "agents in the sandbox cannot reach 127.0.0.1, so May ask other models does not work"`.
  `GET /agents/{codex,grok,copilot}` also has `sandboxed`: the agent has a sandbox without `allowUnsandboxedBash`
  (on Windows Codex is always false: it cannot start in the Windows sandbox, and its own elevated sandbox can read the
  gateway's settings; Copilot is not supported on Windows, see below). `GET/PATCH /general`
  `worktreeNetwork` (Go gateway too, default true): "Network for builds" of Claude CLI in worktree mode; on Windows
  without the Windows sandbox it has no effect. `PATCH /general {"startAtSignIn"}` sets the Windows Run
  value for the running gateway and, on the Mac, Open at Login; Linux answers `400` (use `minigw serve install`).
  The Mac app leaves `os`, `installDir` and `tray` out.
- `POST /providers/{name}/models`: `all` first (true = all, false = none), then `enable`, then `disable`.
- The Go gateway's Claude also has `"enabled", "folders"` (comma-separated allowed folders), `"allowLocalTools",
  "disabledTools", "permissionMode", "settingSources", "askOthers", "work"`, as on the Mac; see [windows-parity.md](windows-parity.md).
- `work` (every agent, `GET` and `PATCH`): the highest work mode in which the agent may change the client's folder:
  `"direct"` (default: any mode), `"sync"` (sync, worktree and native with git) or `"worktree"` (only modes that return
  a branch: worktree, native with git); any other value is 400 "work: direct, sync or worktree". When the session's
  mode is above the cap, the agent runs read-only (a capped sync run copies nothing back to the client) and the
  client gets, once per session and agent, the note "Mini Gateway lets <Agent> change files
  only in worktree mode, so it only read the folder (in a git repository, run minigw --minigw-work=worktree)" (the gateway checks the
  session it registered, never a header; a nested call is capped by the asked agent's kind in the caller's session).
  `chat` (no shared folder) is not capped. `minigw serve` has only sync and worktree, so there `sync` acts as `direct`.
  The config stores direct as empty (`claudeWork`, `codex.work`, … in serve.json; `work` in the Mac config).
  `minigw gw claude|codex|grok|copilot --work=direct|sync|worktree` sets it; without `--work` it shows it.
- `PATCH /agents/claude`: the gateway checks the path, that the folder exists and is not near the config folder,
  and that Bash on Windows has `allowUnsandboxedBash`. While `leftOut` is not empty, every PATCH without `"folders"`
  returns 400 "first save the allowed folders without …" (Go gateway only).
- Logins (Go gateway, #68): agents use subscription logins, never API keys, unless you set one. `token` (Claude CLI
  only; write-only, `""` removes it) is stored in the key store (Credential Manager, Secret Service), never in
  `serve.json` or an answer. Claude's token is the long-lived one from `claude setup-token` (`sk-ant-…`, your Claude
  subscription, valid one year), passed as `CLAUDE_CODE_OAUTH_TOKEN`; without it a Claude request in the Windows sandbox
  is refused (`403` "Claude CLI in the Windows sandbox needs a long-lived sign-in token: run claude setup-token, then
  minigw gw claude token"). `apiKey` (Claude CLI, Codex, Grok; write-only, `""` goes back to the login) is passed only
  when set, as `ANTHROPIC_API_KEY`, `CODEX_API_KEY` or `XAI_API_KEY`, and is billed through the provider's API, not the
  subscription; Copilot answers `400`. API-key variables in the gateway's own environment (`ANTHROPIC_*`,
  `OPENAI_API_KEY`, `CODEX_API_KEY`, `XAI_API_KEY`, `GROK_CODE_XAI_API_KEY`, `GEMINI_API_KEY`, `GOOGLE_API_KEY`) never
  reach an agent run. `minigw gw <agent> login`, `minigw gw claude token [remove]` and
  `minigw gw claude|codex|grok apikey [remove]` use these fields. The Mac app has no Login field.
- `askOthers` (every agent, off by default): the agent may ask another model of the same gateway with minigw, through a
  loopback port and token valid only for that run (`MINIGW_AGENT_URL`; the token is in a 0600 file of the run, `MINIGW_AGENT_AUTH_FILE`, never in the environment). Claude uses it only with
  `allowLocalTools` (it needs Bash to run minigw); the value is kept either way. Codex needs `network`. The run token
  works only for processes of that agent run (its process tree, also Claude Code's sandbox proxy in it), for at most 2
  requests at a time per run and 6 nested runs on the gateway (`429` otherwise); a nested agent works only in a folder the
  client shared. Log lines of nested requests carry `via=<chain>`.
- Agents other than Claude get their own `agents/{name}` resource when a gateway has them (the Mac: `codex`,
  `grok`); a gateway answers `404` for agents it does not have, and for `siri`.

| Method and path | Body | Response |
|---|---|---|
| `GET /agents/codex`, `PATCH` | any of `{"enabled", "path", "write", "network", "models", "askOthers", "work", "apiKey"}` | `{"enabled", "path", "found", "write", "network", "models": [string] \| null, "askOthers", "work", "login"?: Login}` |
| `GET /agents/codex/models`, `GET /agents/grok/models` | – | `{"models": [{"slug", "name", "description"?, "window"?, "enabled"}]}`: the account's models |
| `GET /foundry`, `PATCH` | any of `{"enabled", "path", "instructions", "models"}` | `{"enabled", "path", "found", "exe", "running", "error", "instructions", "models": [string] \| null}`: Foundry Local on this computer |
| `GET /foundry/models` | – | `{"models": [{"name", "vision", "enabled"}]}`: models downloaded in Foundry Local; `503` when it is not installed or does not answer (Go gateway and the Mac app) |
| `GET /agents/grok`, `PATCH` | any of `{"enabled", "path", "write", "models", "askOthers", "work", "apiKey"}` | `{"enabled", "path", "found", "write", "models", "askOthers", "work", "login"?: Login}` |
| `GET /agents/copilot`, `PATCH` | any of `{"enabled", "path", "write", "network", "commands", "models", "askOthers", "work"}` | `{"enabled", "path", "found", "write", "network", "commands", "models": [string], "askOthers", "work", "login": Login, "loginProblem"?, "commandsUnavailable"?}` |
| `GET /appleFM`, `PATCH` (Mac) | any of `{"enabled", "instructions"}` | `{"enabled", "instructions"}`: Apple Foundation Models (Settings → Apple Foundation Models); instructions up to 8000 characters |
| `GET /siri`, `PATCH` | any of `{"enabled", "timeout", "allow": [id], "deny": [id]}` | `{"enabled", "timeout", "devices": [{"id", "name", "siri"}]}` |
| `GET /run`, `PATCH` (Mac) | any of `{"commands", "allow": [id], "deny": [id], "allowNetwork": [id], "denyNetwork": [id]}` | `{"commands", "devices": [{"id", "name", "run", "network"}]}` |

`write`: the agent's sandbox may write in the session's folder (else read-only); `network`: commands in Codex's
sandbox may use the network (default: off); `models: null` = all the account's models. `allow`/`deny` switch Siri for known
devices (Siri uses this Mac's personal data). `run` allows `minigw run` for a device (off by default): it runs the
listed commands (comma-separated, first argument only) in a native folder on the Mac, so it is code execution there.
Copilot: `write` lets it edit files in the session's folder (default: off, read-only), `network` allows its web tool,
`commands` is always `false` (#81 H1, both gateways: `PATCH {"commands": true}` is 400 and `commandsUnavailable`
says why: a command could read Copilot's sign-in from the system keyring; a stored `true` is ignored). On a Windows gateway Copilot is not supported
in this version: `GET` has `"unsupported": true`, `"enabled": false` and `reason` "Copilot is not supported on a Windows
gateway in this version"; `PATCH {"enabled": true}` and requests to `claude-copilot*` get 400 with that text, and the
models are not listed. On the Mac Copilot always runs in a `sandbox-exec` profile and on Linux under bwrap, each request with a fresh
`COPILOT_HOME` that is deleted afterwards; only the session state is kept between turns. Sign-in (#81): only the
gateway user's `copilot login`, no token (`PATCH {"token"}` is 400). The fresh home gets a `config.json` with only
`loggedInUsers` and `lastLoggedInUser` from `~/.copilot/config.json`; the token stays in the Keychain or Secret Service
and Copilot reads it itself. When that login cannot be used (not signed in, a token stored in plain text, an unexpected
form, on Linux no Secret Service on the D-Bus session or no `xdg-dbus-proxy`), `loginProblem` and `status.copilot` (when
Copilot is on) give the reason and requests get `503` with it. Only `events.jsonl` and `workspace.yaml` of a session are kept between turns. When the Copilot binary
changes or its package cache is missing, the gateway runs `copilot --version` once outside the sandbox (no prompt, no
model call, no sign-in) so that Copilot can unpack its package, which the sandbox can only read. `models` lists model names
(Copilot has no model list; empty = only `claude-copilot`, Copilot's default); other names are refused (`404`), and a name
that is not a valid model name is a `400`.

## Mac app

The Mac app keeps its settings in its preferences and Keychain; the Settings window shows API changes at once.

- **Admin secret and the gateway's identity:** one item in the app's Keychain (account `admin`: JSON with
  `secret`, `port` and the gateway's public `key`), not a file: Codex and Grok agents on the Mac may read files
  anywhere, but not the Keychain, and a file or environment variable (`CFFIXED_USER_HOME`) could be faked by another
  process. The item trusts the app and minigw signed by the app's team (identifier `dev.trojar.minigateway.cli`,
  same as "Always Allow", but it lasts through updates), so any program of this user can run `minigw gw …` without a
  prompt; agents of the app are still refused (see below). minigw never lets macOS ask: without access it stops with
  an error. `minigw gw` refuses to run with
  `CFFIXED_USER_HOME` set, speaks Noise directly to `127.0.0.1:<port>` with that key (no local proxy) and sends only
  the `MGW-Admin` proof; the secret never leaves the Keychain.
- **No agents (defence in depth, after the proof):** the app finds the process behind each admin connection (libproc:
  the process with that TCP socket, both addresses loopback in the socket's family; `LOCAL_PEERPID` works only for
  Unix sockets). It refuses (`403` "admin requests from agent processes are not allowed") when there is not exactly
  one such process, when it is sandboxed (`sandbox_check`; a child that double-forks away stays sandboxed), when it
  descends from the app (parents via `sysctl KERN_PROC_PID`), or when the chain of parents cannot be read up to
  PID 1. So an agent that runs `minigw gw` gets nothing, even after "Always Allow" in the Keychain.
- **Order of checks:** loopback peer → Host (plain HTTP only; over Noise there is no browser) → `MGW-Admin` proof →
  process check.
- **No offline mode:** without the running app, `minigw gw` stops with a message (settings live in the app).
- **Devices:** as in Go: an unknown device waits for approval (`pending`, also in the menu bar and Settings →
  Devices), unless the list is empty and `allowAnyDevice` is on. Settings from before approval keep working: listed
  devices stay allowed, and an empty list keeps `allowAnyDevice` on. `GET /devices` also has a `siri` flag per
  device. This Mac (loopback) is always allowed.
- **Providers:** each has `"enabled"`. `name` is the provider's name in Settings. `POST` turns the provider on and
  applies the same URL rule as Go, and so do providers set in Settings ("Allow http"); providers with an `http` URL
  from before this rule have Allow http on, and those at other hosts are refused until they use `https`. `DELETE` of
  the built-in LiteLLM and OpenRouter returns 400 as in Go. The key goes to the
  Keychain only after the config change succeeds, and before the app restarts the gateway (the gateway reads provider
  keys when it starts). Providers with manual
  aliases (Settings → Advanced) list their discovered models here, but route by their aliases.
  `PATCH /providers/{name}` takes the same fields as on Go. The key stays under the provider's ID, so a new name keeps it.
- **agents/claude:** `{"enabled", "path", "found", "tools", "folders", "allowLocalTools", "disabledTools",
  "permissionMode", "settingSources"}`; `PATCH` takes the last two with the same rules as Go. `GET /agents/claude/tools`
  and `GET /agents/{codex,grok}/models` answer as on Go.
  `folders` is the comma-separated list of allowed folders. There is no `allowUnsandboxedBash`: with
  `allowLocalTools`, Bash runs in Claude Code's sandbox.
- **general:** also `worktreeNetwork` and `worktreeCodexNetwork` (Settings → General → Worktree mode: Network for builds,
  Network for Codex builds; `GET` and `PATCH`), and `allowedClients` (IP addresses, comma-separated; empty = all), `allowPlainRemote` and `allowAnyDevice`. `address`
  may be `0.0.0.0`, `::` or `127.0.0.1`. A port or address change restarts the listener, so there is no
  `restartRequired`. `scratch` is an existing folder: as in Settings, the scratch content moves into a folder named
  after this gateway's instance inside it (0.5.24; before: `Mini Gateway Scratch`, kept by existing installs until
  they move; `""` = the default folder); `GET` returns the folder in use. A path
  that contains a quotation mark (`"`), also after resolving links, is refused (400).
- **status:** `installed` is Open at Login.
- **Not on the Mac:** the settings page and its sessions (`/minigw/settings`, `POST /settings/login`): the app has
  its own Settings window, so only the admin secret (`minigw gw`) opens the API, and it never sets cookies.

