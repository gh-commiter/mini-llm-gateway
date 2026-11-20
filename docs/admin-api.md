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
          "enabled": bool, "aliases": string, "tools": "none"|"read"|"edit", "modelTools": {model: level}}
         models: enabled provider IDs; null = all discovered, now and later. ready: model list loaded.
Claude   {"path": string, "found": bool, "tools": string, "allowUnsandboxedBash": bool, "acceptAgentsCanReadNotes"?: bool,
          "exe"?: string, "version"?: string, "off"?: string, "leftOut"?: [string],
          "leftOutUnchecked"?: [string], "offErrors"?: [string], "refused"?: [string], "login"?: Login}
         path: "" = from PATH; tools: comma-separated tool names; exe: the
         claude that was found; version: its `claude --version` (both only when found; Go gateway only).
         off: why Claude CLI and Codex were turned off at startup (an allowed folder contains or is inside the scratch
         folder or Copilot's package cache), while Claude CLI is still off; cleared when Claude CLI or Codex is turned
         on again (Go gateway only: the Mac ignores such folders and keeps Claude CLI on). leftOut: those folders, still
         in serve.json (Go gateway only); cleared when the allowed folders are saved. leftOutUnchecked: those of them
         that cannot be checked (cannot be resolved); offErrors: the resolve errors in `off` (both Go gateway only,
         cleared with leftOut; the settings page builds the reason in its language from them). refused: allowed folders
         (still in the list) whose real path or identity differs from the record saved when they were added, refused
         at start or at a run; a folder leaves the list when it passes the checks again or is removed from the allowed
         folders (both gateways; the reason is logged once).
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
| `GET /status` | – | `{"running", "version", "port", "address", "installed", "devices": n, "pending": n, "claude": Claude, "providers": [Provider], "os", "installDir"?, "tray", "autostart", "secrets": "where secrets are kept (one line, never a value)", "remotePassword"?: "file <path>", "agents": n, "agentsUnsandboxed", "agentSandbox"?, "claudeSandbox"?, "claudeSandboxReason"?}` |
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
| `PATCH /providers/{name}` | any of `{"name", "enabled", "baseURL", "insecure", "aliases"}`, or `{"tools": none\|read\|edit\|"", "model": "<name>"}` (the highest tool level of the provider's models, or of one model with `model`; `""` = back to the default, read) | `{"provider": Provider}`; a new name moves the key; the key stays for a new URL |
| `DELETE /providers/{name}` | – | `{"name"}`; also deletes the key |
| `GET /providers/{name}/models?filter=text` | – | `{"models": [Model]}`; filter: part of `model`, any case |
| `POST /providers/{name}/models` | `{"all"?: bool, "enable"?: [id or model], "disable"?: [id or model]}` | `{"models": [Model], "changed": [Model]}` |
| `GET /agents/claude` | – | Claude |
| `GET /agents/claude/tools` | – | `{"tools": [string]}`: the tools of this Claude Code (for the exclude table with local tools) |
| `PATCH /agents/claude` | any of `{"path", "tools", "allowUnsandboxedBash", "acceptAgentsCanReadNotes", "token", "apiKey"}` | Claude. `acceptAgentsCanReadNotes` (Go gateway, 0.6.0): the owner accepts that agents without a sandbox can read all project notes, so the core opens on a Windows or Linux gateway without a sandbox (a restart applies it; the Mac has no such field: its agents are sandboxed). `tools` without "Allow local tools" refuses `Task`, MCP tools and the like (400); on the Mac also `Bash` |
| `GET /general` | – | General |
| `PATCH /general` | any of `{"port", "address", "startAtSignIn", "allowedClients", "allowPlainRemote", "allowAnyDevice", "allowLocalApps", "scratch", "worktreeNetwork"}` | General; `"restartRequired": true` for port or address; `allowAnyDevice: true` only while no device is allowed (400) |
| `POST /general/token` | – | General with the new token in `connect`; the old token stops working at once |
| `POST /general/scratch/move` | `{"to": "D:\\minigw-scratch"}` (a base folder; empty: the proposed default) | Go gateway (0.5.23): moves the whole Scratch folder to `<to>/<instance>` (0.5.24; with the owner file) and switches `scratch`; while the Scratch folder belongs to another gateway (`scratchState`) nothing is moved, the gateway starts its own folder there; General (without `connect`), plus `warning` when the old folder could not be removed. 409 while minigw sessions or agents are active, 400 for a folder the Scratch rules refuse or that is not empty, 500 when the move failed (the old Scratch stays). Client requests during the move get 503 |
| `POST /general/scratch/take` | – or `{"name"}` (a folder from `scratchOthers`) | Both gateways (0.5.24): Take over. Without `name` only when `scratchState` is `stale`; with `name` (only a name: a real folder directly in the base, another gateway's, not `*.old-*`, else 400) the stale sibling, for example after a reinstall. The other gateway's folder is renamed to `<base>/<instance>` of this gateway and gets its owner ID; this gateway's current folder (and one already named `<instance>`) is deleted when it has no conversations, else kept as `<instance>.old-<date>` (named in `warning`); nothing is merged. Logged. General (without `connect`). 409 when the folder is this gateway's own, `in-use` (the other gateway runs), or minigw sessions or agents are active; 500 when the rename failed (the folder stays) |
| `POST /settings/login` | – | `{"url"}`: a one-time link to the settings page (`…/minigw/settings#t=…`, 60 s); `503` when the gateway does not run |

### Core: users, bindings, projects, members, grants, audit, backup (0.6.0, both gateways)

Projects, members and notes live in the gateway's core database (`minigw serve`: `<config folder>/core`; Mac app:
`~/Library/Application Support/Mini Gateway/core`, kept by the helper `minigw-core`; see gateway.md). The routes are the
same and answer the same on both: the Mac app authenticates the owner and forwards them to the helper. They need the
running gateway; while the core cannot be opened they answer `503` (on the Mac also while the helper is not running: the
message then says `The project database is not available: the project helper (minigw-core) is not running`). Ids are numbers. The caller is the owner (admin authentication above); the `owner` mark on a user gives no
extra power, and a project `admin` role does not authorize this API. Every call writes an audit event; a call that can
take access away raises the epoch (subsequent note reads and conversation reuse are refused; running processes
continue), in the same transaction.

| Method and path | Body | Response |
|---|---|---|
| `GET /users` | – | `{"users": [{"id", "name", "active", "owner"}]}`; also on the remote settings listener, read-only. |
| `POST /users` | `{"name"}` | `{"user"}`; names are unique, any case; 400 for a name in use |
| `PATCH /users/{id}` | any of `{"name", "active", "owner"}` | `{"ok": true}`; `active: false` raises the epoch |
| `GET /bindings` | – | `{"bindings": [{"device", "user"}]}`; also on the remote settings listener, read-only. |
| `PUT /bindings/{device id}` | `{"user"}` | `{"ok": true}`; a device has at most one user; changing it to another user raises the epoch |
| `DELETE /bindings/{device id}` | – | `{"ok": true}`; raises the epoch. Removing a device from the approved list (`DELETE /devices/{id}`) raises the epoch too but keeps the binding |
| `GET /projects` | – | `{"projects": [{"id", "slug", "name", "archived", "granted": []}]}`; also on the remote settings listener, read-only. |
| `POST /projects` | `{"slug", "name"}` | `{"project"}`; slug `[a-z0-9][a-z0-9-]{1,62}`, not only digits; the id is never reused |
| `PATCH /projects/{id}` | `{"archived": bool}` | `{"ok": true}`; archived: nobody writes |
| `GET /projects/{id}/remotes` | – | `{"remotes": [identity], "entries": [{"remote": identity}]}`: the registered git identities (`host/path`), also as rows |
| `POST /projects/{id}/remotes` | `{"add": [url], "remove": [url]}`; the owner route also takes one value as `{"addRemote": url}` or `{"removeRemote": url}` (a form sends one value) | `{"remotes": [...], "added": [...], "removed": [...]}`; one transaction, audit `project.remotes`; at most 8 per project, the same identity may belong to several projects; an archived project: 403 `archived` |
| `GET /projects/{id}/members` | – | `{"members": [{"user", "name", "role", "origins", "directRole", "fromTeams"}]}`; `directRole` is the role given directly (`none` when there is none) and `fromTeams` the teams that give one as display text (`acme (reader), web (developer)`; `teams` has the same as `[{team, role}]`), so a role and where it comes from can be shown flat; also on the remote settings listener, read-only (one numeric id). |
| `PUT /projects/{id}/members/{user}` | `{"role": "reader"\|"developer"\|"admin"}` | `{"ok": true}`; lowering the role raises the epoch |
| `DELETE /projects/{id}/members/{user}` | – | `{"ok": true}`; raises the epoch |
| `GET /grants` | – | `{"grants": [{"source", "reader", "class", "id", "sourceName", "readerName"}]}` |
| `POST /grants` | `{"source", "reader", "class": "structure"\|"data"}` | `{"ok": true}`: the members of `reader` read that class of `source` |
| `DELETE /grants?source=&reader=&class=` | – | `{"ok": true}`; raises the epoch |
| `GET /audit?after=&limit=` | – | `{"events": [{"id", "time", "actor", "action", "subject", "detail"}], "next"?}`: oldest first, `next` is the last id (pass it as `after`); limit 1-500, default 100; metadata only |
| `POST /core/backup` | `{"name": "<name>"}` | `{"name", "file"}`: a consistent copy while the gateway runs, written to `<core>/backups/<name>` (name: letters, digits, `.` `_` `-`, at most 64, no path; a path or `file` is `400`); the file must not exist, it is readable by the user only |

Errors of these routes use the envelope above (`400` invalid, `404` not found, `503` core unavailable).

### Panels (both gateways)

`GET /panels` returns the admin panels as declarative descriptions that a renderer maps to its own controls; the Mac
app draws them as panes of the Settings window. The descriptions carry no translated text, only string keys
(`builtin.users.title`); the texts come from `GET /panels/strings?lang=<locale>` (below). Owner admin authentication only; it is not a member route and the run
port does not serve it (`POST` and the other methods answer `405`). It needs no database, so it answers while the core
is down.

```json
{"format": 1, "panels": [{"provider": "builtin", "description": {"format": 1, "id": "builtin.users", "provider": "builtin", "title": "builtin.users.title", "...": "..."}}]}
```

- Each description comes under `provider`, the id the registry knows (`builtin` is reserved for the gateway's own code).
  A renderer takes the routes of THAT id and refuses a description whose own `provider` field differs.
- **Elements** (format 1): list, text and `record` (one object of an answer as label and value pairs: `source` is a path,
  `columns` as in a list; inside a detail page it may carry `when`, a field of the opened row that must equal a value, and
  is not requested otherwise). A list may carry `scope`: a picker over a `refs` entry whose chosen key is sent to the list's
  source as the query parameter `param` (the first entry is chosen at the start; nothing is requested while there is none).
  A `multiline` value is shown wrapped in the page's own scroll, never in an inner scroll area, cut at 20000 characters with
  a visible note. An action with `"body": {}` sends the empty JSON object `{}`.
- **Secret fields and results:** a field of type `secret` is a password field. Its value goes only in the request body of an action
  whose route the provider registered as taking a secret (`PanelProvider.SecretRoutes`; the built-in one: `PUT upstreams/{}`); the
  validator refuses a secret field in any other route, in a path or a query template, with a default, with options or a ref, or one
  that no body key sends. A renderer never shows it again, clears it as soon as the request has been sent (success and failure), keeps
  it in no state, and never shows an error text that holds it (or a part of a token line): it shows the status instead. An action may
  have a `result`: the answer shown after it succeeded, as rows (`items` names the array in the answer) or, without `items`, as one row,
  with `columns` as in a list, read-only, plain text.
- A text field's `maxLength` is in UTF-8 bytes, as the core counts a user name (128), not in characters.
- A description is data, never code: no scripts, HTML, Markdown, links or images. Every text position holds a key
  (`[a-z0-9._-]`, at most 120 characters, inside the panel's namespace `<provider>.<panel>.`); the validator refuses
  anything else, so an inline text or another provider's key is refused. A renderer shows every text and every value of
  an answer as plain text.
- **Texts:** `GET /panels/strings?lang=<locale>` answers `{"format": 1, "locale": "sl_SI", "strings": {key: text}}`.
  `lang` is `C`, a language such as `sl` (it gets that language's table, `sl_SI`) or a locale such as `sl_SI` (anything else: `400`; none: `C`). `C` is the base table and holds every key;
  `en_US` and `sl_SI` lie on top of it. The answer is merged: the exact locale, then another table of the same language,
  then `C`; a locale nobody has is answered with `C`, and `locale` says which one was returned. A text is plain, at most
  600 characters, and may hold `{field}` placeholders that the renderer fills from the row's own fields as plain text; a
  key without a text is shown as the key. The built-in tables are `client/coreapi/panels/strings/{C,en_US,sl_SI}.json`
  (flat `{"key": "text"}`); a plugin ships its own per-language files the same way.
- `format` is the only version. A renderer that meets a higher format says "needs a newer Mini Gateway" and shows nothing
  else; an unknown element inside format 1 is shown as "cannot be shown".
- One validator (`client/coreapi/panels.go`, `ValidatePanel`) checks every description before it is served; a panel that
  fails is left out and logged. It refuses unknown keys, more than 64 KiB, more than 12 sections, 10 columns, 12 actions,
  10 form fields or 5 refs, labels over 120 characters and messages over 600, control and format characters in strings,
  a route its provider did not register, a path template with anything but whole segments (`{row.f}`, `{field.f}`,
  `{parent.f}` fill exactly one segment each), a `DELETE` or `destructive` action without a confirmation, and a provider id
  that does not match. The renderer validates again and also refuses a value that contains `/`, `..`, `?`, `#` or a
  control character, or is empty, for a placeholder.
- The built-in provider (`builtin`) registers `GET users`, `POST users`, `PATCH users/{}`, `GET bindings`, `PUT bindings/{}`,
  `DELETE bindings/{}`, the gateway's `GET devices`, and for the knowledge base `GET kb/submissions`, `GET kb/submissions/{}`,
  `POST kb/submissions/{}/accept`, `POST kb/submissions/{}/reject`, `GET kb/notes`, `GET kb/notes/{}`, `GET kb/faq`, `GET projects`
  `GET teams`, and for the teams panel `POST teams`, `PATCH teams/{}`, `GET|PUT|DELETE teams/{}/members[/{}]`, `GET|PUT|DELETE teams/{}/projects[/{}]`, `GET|PUT|DELETE teams/{}/assignments`, `POST projects`, `PATCH projects/{}`, `GET|PUT|DELETE projects/{}/members[/{}]`, `GET|POST projects/{}/remotes`, `GET|PUT|DELETE projects/{}/depends[/{}]`, `PUT|DELETE projects/{}/depends/teams/{}` and `GET|POST|DELETE grants`. Its panel `builtin.teams` ("Teams and projects") is described in gateway.md; its panel `builtin.kb` ("Knowledge base") is described under the Mac Settings window in gateway.md. Its panel `builtin.users` ("Users and devices"), never sends
  `owner`; a user name is at most 128 bytes, the server's own limit, and the server stays the authority.
- Design and format: `docs/plans/plugin-panels-design.md`. The Go and Swift sides decode the same fixtures
  (`client/testdata/panels*.json`).

### Usage events and quota (0.6.0, both gateways)

| Method and path | Body | Response |
|---|---|---|
| `GET /usage/events?days=&user=&project=&provider=&model=&phase=&activity=&task=&labels=1` | – | `{"days": [{"day", "provider", "model", "user"?, "userName"?, "project"?, "events", "tokensIn", "tokensOut", "cacheReadTokens", "cacheWriteTokens", "eventsWithoutTokens", "nestedEvents", "failedEvents", "reportedCostUsd", "estimatedCostUsd", "costUsd", "estimatedCostEvents", "subscriptionEvents", "unknownBillingEvents", "eventsWithoutCost", "errorEvents", "cancelledEvents", "interruptedEvents", "durationMs", "failedDurationMs", "unknownDurationEvents"}], "gaps": [{"count", "from", "to"}], "timezone":"UTC"}`: every model invocation, per day (UTC), provider, model, user and project, newest first; days 1-3650 (default 7), detail rows older than 400 days are daily totals; also events without a user or project. **Not** `GET /usage` (0.5.x folder counters, unchanged). `phase`, `activity`, `task` select events by label (folded totals carry no labels and are left out); `labels=1` splits the rows by label and adds `phase`, `activity`, `task` and `phaseSource`, `activitySource`, `taskSource` (`given`, `branch`; `model` for a phase or activity a model chose, `internal` for the gateway's own classification events) to each row; unlabelled rows have none. `label_source=given|branch|model` keeps the events that have a label (phase, activity or task) with that source (anything else is 400; it only narrows, as the other label filters; the member route `GET /minigw/v1/usage` takes it too). A session (`/usage/sessions`) also carries `phaseScore` and `activityScore` (per mille of agreement, 667 or 1000) for a label a model chose. Labels are data, not permissions. The member routes `GET /minigw/v1/usage` and `GET /minigw/v1/projects/{id}/sessions` (and the owner's `GET /usage/sessions`) take the same filters, a session shows the labels of its first request, and a developer still sees only the own events |
| `GET /quota` | – | `{"quota": [{"name", "unit", "value"?, "source", "observedAt"?, "resetsAt"?, "resetsText"?, "stale", "note"?, "reason"?}], "claudeProbe": bool}`: Codex windows (`codex-session-log`), the Claude subscription (`claude-usage`, only when the probe is on), OpenRouter credit (`openrouter-api`), LiteLLM spend (`litellm-api`); `source: "unavailable"` with a `reason` (for example `probe off`) when a value cannot be had; works without the core |
| `PATCH /quota` | `{"claudeProbe": bool}` | as `GET`; turns the Claude quota probe on or off (default off, see gateway.md) |
| `GET /usage/labels/settings`, `PUT /usage/labels/settings` | `{"enabled","backend","threshold","maxPerHour","shadow","shadowRate","activities"}` (all optional; a number may also be a string; `activities` a list or one comma separated line) | Owner only (admin channel; no member or run route reaches it). `{"settings":[{"id":"labels","enabled":false,"backend":"","threshold":"1.0","maxPerHour":"120","shadow":true,"shadowRate":"4","activities":[...],"activitiesText":"feature, bugfix, ...","lastError":""}]}`: one row, so a panel can edit it. Off by default; `backend` is `apple-fm` or `foundry:<model>`; `threshold` `1.0` (three samples agree) or `0.67`; `maxPerHour` 1-1000; `shadowRate` 1-100; `activities` up to 16 words. Turning it on needs a backend (400). `lastError` is the classification worker's and is not writable. `revision`, when present, is a read-only settings generation; the worker atomically discards labels and shadow counters completed under older settings. Each change is audited. Each system accepts only the backend it can run (the row's `backends` names them): the Go gateway (Windows, Linux) `foundry:<model>` and answers 400 "backend: not available on this system; choose foundry:<model>" to `apple-fm`, the Mac `apple-fm`; `backendProblem` says when a stored backend is not one of them. The Windows Go gateway classifies through owner-selected Foundry Local; Linux reports the backend unavailable. See [usage labels](usage.md) for the input sent, in-memory handling, queue, rate and agreement semantics |
| `GET /usage/labels/agreement?days=N` | – | Owner only. `{"agreement":[{"kind":"phase\|activity","bucket":"667\|1000\|unknown","agree":N,"n":N,"id":"phase/1000","percent":"87 %","count":"N","days":"2026-11-04 - 2026-11-18"}]}`: counters of the shadow comparison (model against explicit labels) over the last N days (1-400, default 30); numbers only, never any text. `id`, `percent`, `count` and `days` (the first and last day with counters) are text for the Model Labels panel's list; agreement is not calibrated accuracy |
| `POST /usage/labels/eval` | `{"examples":[{"text","phase","activity"}]}` (1 to 500; `text` capped to 2000 code points, `phase` plan\|do\|check\|act, `activity` one of the configured list, at least one of the two; unknown keys refused) | Owner only, and the gateway's own route (the configured model is there): it is not a core route, so no member, internal or Mac-helper call reaches it, and the remote settings listener does not list it (404). Classifies each example with the configured backend through the worker's own decision (one at a time: a second answers 429; a user's own requests keep priority; at most 30 minutes) and answers `{"eval":{"backend","examples","run","notRun","phase":{"scored","t067":{"decided","correct"},"t10":{...}},"activity":{...},"mistakes":{"phase":[{"expected","got","n"}],"activity":[...]},"wrong":[{"kind","line","expected","got"}]}}`: numbers only (`t067` = two of three agree, `t10` = all three; `line` is the example's place in the request, `got` empty = the model did not decide). It stores nothing and never returns the text; one internal usage event records the model time. 409: no backend chosen or not runnable here; 502: no example got an answer |
| `GET /quota/history?days=N&provider=P` | – | Owner only, same authorization as `/usage/events`. Default 7 days, range 1–400; optional exact provider filter. `{"probes":[{"id","provider","probedAt","status","errorKind"?,"values":[{"name","unit","value","resetsAt"?}]}],"gaps":[{"from","to"}]}`. Newest 1000 probes and at most 1000 gaps; HTTP 206 when either list is truncated, otherwise 200. No probe is run. Requires the core. |

Execution time (0.6.7): `durationMs` is the sum of the time from dispatch (the upstream request or the agent process
starts) to the end, in milliseconds, of the events whose duration is known; admission and queue time are not in it.
`failedDurationMs` is the part of it that belongs to events with outcome `error` (an upstream error, a stream that broke
after HTTP 200, an agent that exited non-zero or hit the 30 minute limit of `minigw serve`), and `errorEvents`, `cancelledEvents` and
`interruptedEvents` count the outcomes (`failedEvents` is their sum, as before). Events recorded before schema 7 and runs
recovered after a crash have no duration: `unknownDurationEvents` counts them, they are never added as zero. The event
also stores the status the upstream answered (null for agents) and, for an error, one of `http`, `limit`, `timeout`,
`unreachable`, `stream`, `exit`, `other`; no error text is stored. A client that goes away is `cancelled`, a run cut off
by a stop of the gateway becomes `interrupted` at the next start.

Usage event rows also include `provider`, `reportedCostUsd`, `estimatedCostUsd` and `eventsWithoutCost`;
the response includes `timezone: "UTC"`. Money is a decimal USD string with nine fractional digits, or `null`
when no amount is known. Sums cover known amounts only: missing costs are counted separately, never treated as free.
An aggregate cost outside the integer nanodollar range fails the report rather than returning a partial or wrapped total.

`cacheReadTokens` and `cacheWriteTokens` are the sums of the input tokens a provider read from and wrote to its
prompt cache; an event without them counts as zero. Daily totals folded in after 400 days have no cache sums (0).
If retention folding would overflow, its transaction is rolled back and detail is retained so the core can still start.
Reported provider `usage.cost` wins when present. Otherwise metered calls with complete reported input/output
counts use an immutable dispatch-time price snapshot. `costUsd` is the known reported plus estimated sum;
`estimatedCostEvents`, `subscriptionEvents` and `unknownBillingEvents` describe coverage and billing mode.
Subscription CLI rows have null money. Old events are never repriced. Estimates share the per-event ceiling of
1e15 nanodollars and round the exact decimal sum once. Unsupported prices or incomplete usage stay null. No recorded OpenRouter Anthropic Messages cost fixture exists in the repository, so that field's
presence is not assumed or requested from the upstream. The shared parser fixtures describe supported shapes,
not captured OpenRouter responses.

The model filter is an exact match in both detail and folded totals; the provider filter ignores case. Reports show
the provider in lowercase, so rows stored as `OpenRouter` (Go gateway, configuration name) and `openrouter` (Swift
gateway, configuration ID) are one provider; stored rows are not rewritten. Provider remains part of the grouping
when no filter is set. The two gateways still store different keys when a provider's name and ID differ by more than
case. Identical model names across providers remain separate. Requested
model names remain unchanged; the routed upstream model is frozen separately at dispatch.

Detail older than 400 days is folded into daily totals. Folded totals currently survive indefinitely and reports
can request up to 3650 days. Clear History still clears legacy sessions/counters only; it does not delete core usage
accounting. There is no separate accounting deletion endpoint yet. No prompts, answers or raw upstream errors are
stored in usage accounting.

`GET /quota/history` and `GET /usage/events` are core routes (they answer `503` like the others); `GET` and `PATCH /quota` are the gateway's own and
work without the core. On the Mac app the quota routes are in the app (Codex session log, provider credit, the opt-in
Claude probe); the Claude probe is also the Settings → Claude CLI switch "Show the subscription quota".

Members read their own usage on the member route `GET /minigw/v1/usage?project=<id or slug>&days=<1-400>` (same
shape; a reader, a grant-only user or a stranger gets 404; `gaps` is always empty there: lost-event records concern the
whole gateway, not a project, and are shown only on the owner's `GET /usage/events`).

### Team directory (both gateways)

The following routes use owner admin authentication and the same prefix and core failure handling as `/projects`.
Names use the existing display-name validation (nonblank, at most 128 UTF-8 bytes), with case-insensitive
uniqueness as for users. Project display names are not unique; projects have separately validated unique slugs.

| Method and path | Body | Response |
|---|---|---|
| `GET /upstreams[?project=<id>\|team=<id>]` | – | `{upstreams: [{name, address, device}], links: [{project, team, upstream, remote, classes, withPrivate, lastPush, lastError, label, pending, total, auto, mode, dirtySince?, lastOk?, lastFail?, failCount?, nextAttempt?, notPropagated?, conflicts?, read, readClasses?, readSources?, readAccepted?, lastPull?, lastPullOk?, lastPullAge?, pullError?, mirror, mirrorBytes, stale?, pullEvery, pullNext?, pullFailCount?, pullTimerOff?, revisionBackwards?, id, place, linkId, projectId, teamId, classesText, conflictCount, readClassesText, readSourcesText, pullEveryMin, lastPullAgeText}]}`; `pullEvery` is the interval of the automatic pull in seconds (15 minutes when the owner set none), `pullNext` the time of the next attempt (while reading and the timer are on), `pullFailCount` the failures in a row, `pullTimerOff` true after the upstream refused (the timer stays off until the owner accepts the reach again; a pull by hand never switches it on, and a refusal switches it off whoever started the pull), `skippedNotes` and `skippedList` (up to 50 `id:reason`, reasons `gone`, `refused`, `invalid`, `large`) the notes of the last traversal the upstream answered about definitively and that were skipped (they do not make the mirror stale), `revisionBackwards` the local notes whose upstream copy has a lower revision than our push recorded; the last ones are for a panel: a unique id, `project` or `team`, the id to name the link in a path (a TEAM id for a team, never the hidden segment), the same split as two numbers for a push body, and the classes as text; `auto` (bool): the gateway pushes the link by itself; `mode`: `submit` (default) or `publish` (notes written on the upstream directly; `conflicts` lists the notes that changed or vanished there); `device` is the public ID of this gateway's device key for it |
| `PUT /upstreams/{name}` / `DELETE` | `{address, token}` (token = the upstream's Copy Token line) | PUT: `{name, address, device, where, probe}`; the secret is kept in the credential store, never answered; an address of this same gateway is refused (400); DELETE removes the links and receipts and the credentials; 400 while any link of this upstream is being pushed (credentials are kept). 501 where the host cannot (a helper whose app does not answer callouts) |
| `GET /upstreams/{name}/probe` | – | `{name, address, device, state, probe}`: the first contact of an ordinary client, made when you ask (a panel asks when its detail page opens); `state` is `unreachable`, `token`, `waiting`, `notbound`, `connected` or `other`, `probe` the text of the CLI. Writes nothing |
| `PUT /upstreams/{name}/links/{id}` / `DELETE` | `{team?, remote, classes?, withPrivate?, auto?, mode?, read?, readClasses?, readSources?, acceptReach?, readOnly?, pullEvery?}` (`id` is a project, or a team when `team`; a DELETE may name a team's link as `?team=true` instead of a body; an empty `remote` on PUT means the project's short name, or the team's name, as the CLI does; `auto` true\|false, omitted keeps it; `mode` `submit`\|`publish`, refused once the link has sent notes) | `{ok: true}`; `private` needs `withPrivate`; at most 8 links per project; DELETE and PUT are refused (400) while this link is being pushed or pulled. Reading off forgets the classes and sources, so reading on again starts from `structure` with no extra sources. Reading (step C): `read` true turns it on, false off (the mirror is deleted); `read: true`, or any `readClasses` (`structure` or `structure,data`; `private` is refused) or `readSources` (labels `project:<slug>`, `team:<name>`, at most 16; omitted keeps the current ones, `""` means NO extra sources) needs `acceptReach: true` (400 otherwise). A change of what is read deletes the mirror in the same transaction. The acceptance is bound to the exact reach (upstream, remote project or team, classes, sources: stored as `read_reach`, named in the audit row `upstream.link` as `readAccepted`); a PUT that changes `remote` (or the team) of a link that reads, without a `read`/`readClasses`/`readSources` + `acceptReach` of its own, turns reading off and deletes the mirror, and the answer says `readOff: true` (every PUT answers `{ok, readOff}`). `readOnly: true` leaves the push settings of an existing link untouched (`remote`, `classes`, `withPrivate`, `auto`, `mode`); a new link then gets the defaults. `pullEvery` (minutes, 5 to 1440, as a number or as a string of digits; 0 means the default of 15) sets the interval of the automatic pull of the link: it is not part of the reach (no `acceptReach`), does not touch the mirror, and a shorter interval brings the next pull forward. Unlink and removing the upstream delete the mirror |
| `GET /upstream-links/{id}` | – | one link as in `GET /upstreams` (`id` is its `id`, e.g. `project-3-hub`, `team-2-hub`) plus `conflictCount` and `conflictRows: [{upstream, note, title, what (changed\|deleted), projectId, teamId}]`; `rejectedCount` and `rejectedRows: [{note, title, submission, reason}]` (notes whose exact text the upstream rejected); reads only this gateway's store (no call to the upstream); 400 for a bad id, 404 for no such link. `conflictCount` is also in each link of `GET /upstreams` |
| `POST /upstream-push` | `{project?\|team?, dryRun?}` | `{pushes: [{label, upstream, remote, lines, linesText, created, skipped, replaced, refused, stopped?, dryRun, more?, wait?}]}`; may take long (waits out the upstream's rate limit); behind a helper a call is limited to about 10 seconds and answers `more: true` (and `wait`, seconds) when notes are left: call again; `dryRun` writes nothing. A second push of a link while one is running is refused (400, "a push or pull of this link is running") |
| `POST /upstream-pull` | `{project?\|team?, upstream?, dryRun?}` | `{pulls: [{label, upstream, remote, lines, linesText, total, truncated, stopped?, dryRun, project, team, added, updated, removed, skipped, mirror, purged?, more?, wait?, next?}]}`. `dryRun: true`: asks each named link's upstream for its manifest (`GET /minigw/v1/projects/{id}/kb/manifest`) and reports counts per source, class and kind; writes nothing. Real pull (only links with `read` on; none: 400): resolves the remote project, takes the manifest, fetches the notes that are new or changed (the project itself and the sources named on the link, in the accepted classes; ids ascending above the link's cursor) with at most 200 `GET .../notes/{id}` per run, and writes them into the link's mirror (`upstream_mirror`, never `notes`) in ONE transaction together with the deletions and the status. What the upstream says is untrusted data: answers over 1 MiB, malformed JSON, a note whose id, revision, class or kind differs from the manifest, a `private` class, text over 256 bytes (title) or 64 KiB (body), and text that is not valid UTF-8 (not repaired) are not taken (`skipped`, with a line); controls, format characters and other invisible characters (variation selectors, tag block, fillers) are removed from title and body; a manifest that is unusable stores nothing and keeps the previous mirror. A note an upstream no longer lists is deleted only from a manifest that was not `truncated`; what the link no longer takes (a class or source not accepted) is deleted either way. 401/403 (or the project or team gone: 404 on the lookup) deletes the link's mirror at once (`purged: true`); a manifest 404/501 or `v` other than 1 means the upstream does not support reading: nothing stored. Bounds: 5000 notes per link, 64 MiB of titles and bodies per gateway (over them new notes are skipped as "mirror full"), 30 seconds per request and 5 minutes per run. `more: true` (with `next`, the cursor, and `wait` seconds when the upstream's read limit is the reason): the run used its budget (note requests, the time of a helper call of about 10 seconds): call again; the cursor is kept in the link. With no `project`/`team` all links are pulled in one call: the ones after a used-up time budget come back as `more` with `wait: 1`, the reports of those done stay. Waits for the upstream's read limit end with the request. A pull of a link that is being pushed or pulled is refused (400, "a push or pull of this link is running"). It needs no `acceptReach` (it reads only within the accepted reach) but is refused when the stored acceptance is not for the link's current upstream, remote, classes and sources, and it never switches the timer on (a refusal switches it off). The answer from the upstream is untrusted data. Owner only; 501 where the host cannot call out |
| `POST /upstream-auto` | – | one step of the automatic push and pull: `{ran, push?: {...as one entry of /upstream-push}, pull?: {...as one entry of /upstream-pull}, next}`; does ONE link: pushes the one due link (auto, dirty, quiet for the debounce, past its back-off) or pulls the one due link (reading on, timer on, `pullNext` reached; at most 45 seconds, then `more` and the cursor), the two kinds in turn so neither starves the other; `next` is the seconds until another can be due (0: none). A pull records its outcome on the link for the timer: success sets `pullNext` one interval ahead and clears `pullFailCount`; `more` with progress is due again in a second; a failure or a run with no progress backs off (1 minute doubling to 1 hour, with jitter); a refusal (401/403, project gone) deletes the mirror and switches the link's timer off (`pullTimerOff`) until the owner accepts the reach again; the audit action is `upstream.auto-pull` (counts only; one row for a run that did something, else one per change of outcome). Used by a host whose timer lives outside the process with the network (the Mac app drives the helper's lane with it); the gateway's own timer calls the store directly. A link whose manual push is running is skipped. 501 where the host cannot call out |
| `POST /upstream-reconcile` | `{project\|team, upstream?, dryRun?, note?, confirm?, new?, skip?, all?, listed?}` | `{lines, notes, unknowns, candidates, unknown, cleared}`; after a restore: reads the linked project on the upstream afresh and sorts the local notes (known, candidate, unknown); a decision names one note (`confirm` with the remote id, `new` or `skip`) or, with `all`, the ids in `listed`; clears the link's restore mark only when nothing is left undecided. `GET /upstream-reconcile?project=\|team=&upstream=` answers `{unknowns}` of the last listing (local only). Owner only |
| `POST /upstream-resolve` | `{project\|team, note, ours, upstream?}` | `{ok: true}`; settles a publish-mode conflict of one note: `ours: false` keeps the upstream's version, `ours: true` sends ours as a submission for the upstream's reviewers (never an overwrite; later edits of the note also go as submissions). 400 when the note has no conflict on that link, or has conflicts on several links without an `upstream` (the response lists their names). Omitting `upstream` remains valid for exactly one conflict |
| `GET /projects/{id}/depends` | – | `{depends: [{id, slug, name}], usedBy: [...], teams: [{id, name, archived}]}` |
| `PUT /projects/{id}/depends/teams/{team}` / `DELETE` | – | `{ok: true}`; the project depends on the notes of a team (the listing above then has `teams: [{id, name, archived}]`); counts toward the same cap of 16; refused for an archived team and for the project's own team (400 with the reason); the notes are searched only for callers on the team's roster, never in a run |
| `PUT /projects/{id}/depends/{other}` / `DELETE` | – | `{ok: true}`; at most 16 per project, not itself, not an archived project (remove is always allowed); a team's notes (segment ids) are never a side |
| `GET /teams` | – | `{teams: [{id, name, archived, path, segment, memberCount, projectCount}]}` including archived teams; `segment` is the id of the team's own notes Also on the remote settings listener, read-only. |
| `POST /teams` | `{name}` | `{team: {id, name, archived, path, segment}}` |
| `GET /teams/{id}` | – | `{id, name, archived, path, segment, members: [{user, name}], projects: [Project]}`; also on the remote settings listener, read-only (one numeric id). |
| `PATCH /teams/{id}` | `{name}`, `{archived: bool}` or `{path: string}` (one of them) | `{ok: true}`; archive instead of hard delete. `path` is a free-text label (folded to lower case, at most 128 bytes, no control characters, no leading or trailing `/`; `""` clears); a label never gives access |
| `GET /teams/{id}/members` | – | `{members: [{user, name}]}` |
| `PUT /teams/{id}/members/{user}` | – | `{ok: true}` |
| `DELETE /teams/{id}/members/{user}` | – | `{ok: true}` |
| `GET /teams/{id}/projects` | – | `{projects: [Project]}` |
| `PUT /teams/{id}/projects/{project}` | – | `{ok: true}`; at most one team per project |
| `DELETE /teams/{id}/projects/{project}` | – | `{ok: true}` |

Every successful mutation writes an audit event. Team membership alone does not change project
memberships, grants or note access. Archiving preserves attachments and readable details but deletes team assignments and blocks new
attachments; removals remain available. Move a project by removing its old attachment first.

Member routes use `/minigw/v1`: `GET /teams[?archived=1]` returns an array of the caller's teams (each with its `path`);
`GET /teams/{id-or-name}` returns the detail shape above. Numeric references select IDs. Only team members
see its name and member names. Attached projects are filtered by existing project membership and explicit
grants. Missing and invisible teams both return the existing 404; an unbound device or inactive user gets
`no_project_access`. Gateway-agent peers cannot use these routes. Directory operations do not change the
project authorization epoch. Team-generated project roles are future work.

### Member routes (not admin routes)

Under `/minigw/v1/projects` the gateway serves the members' own API on the encrypted device connection (token
and device key; see the contracts in docs/plans/contracts-0.6.0.md, section 6). Error body
`{"error": {"type", "message"}}`: 403 `no_project_access` (no valid token or device key, or its user is inactive: one
fixed body, also for plain loopback and the "allow any device" and local-app exceptions that apply to the model routes;
when the device itself is the reason the message names it — not on the gateway's device list, with `minigw gw devices allow
DEVICE-ID`, or not bound to a user, with `minigw gw bind DEVICE-ID USER` — and gives only the caller's own id),
404 `not_found` (missing or not visible, one
body), 403 `forbidden`, 403 `archived`, 409 `revision_mismatch` (with `revision`) or `idempotency_conflict`, 413
`too_large`, 429 `limit` with `Retry-After`, 400 `invalid`, 503 `core_unavailable`.

| Route | Result |
|---|---|
| `GET /projects[?archived=1]` | `[{id, slug, name, archived, role?, granted: [class]}]` |
| `GET /projects/{id-or-slug}` | one project; digits are always an id |
| `POST /projects/{id-or-slug}/link-check` | the project, only if the user has a direct role (a grant alone: 403 `forbidden`); for `minigw project link` |
| `GET /projects/{id}/notes?class=&after=&limit=` | `{notes: [{id, project, class, title, author, editor (only when someone other than the author last changed the note), created, updated, revision}], next?}`, updated descending then id, default 50, max 100 |
| `GET /projects/{id}/notes/{note}/history` | `{note: {id, revision, class, title, team?}, history: [{revision, saved, savedBy, class, entryKind, bytes, title, updated, writtenBy, replacedBy}]} (`updated` is when this version was written, `writtenBy` who wrote it; `saved` is when it was replaced, `replacedBy` who or what replaced it: a user, `an accepted submission (reviewer ...)`, `an import`, `the owner`, with ` (restore)` after a restore; a mirrored note answers 400 with the upstream's name)`; newest first, at most 100, no cursor; current note and each saved class must be readable through the same source |
| `GET /projects/{id}/notes/{note}/history/{rev}` | One saved row with `body` and decoded stored `provenance`; missing and unreadable rows both return 404 |
| `POST /projects/{id}/notes/{note}/restore` | Admins only: exact body `{"revision": saved, "expectedRevision": current, "class": "current or saved class"}`; 200 NoteJSON, 409 on a stale current revision; new revision, original author, saved text and kind |
| `GET /projects/{id}/notes/{note}` | the note with `body` |
| `POST /projects/{id}/notes` | 201; header `Idempotency-Key` (1-64 bytes); body `{"class", "title", "body"}`; a repeat of the same key and payload within 24 h reauthorizes the current note before answering the first result; deleted or reclassified notes return 404, including after a class is changed back (header `Idempotent-Replay: true`) |
| `PUT /projects/{id}/notes/{note}` | 200; body `{"revision", "class", "title", "body"}` |
| `DELETE /projects/{id}/notes/{note}?revision=N` | 204 |
| `GET /projects?path=<prefix>` | the projects the caller sees that belong to a non-archived team the caller is a member of whose path label equals the prefix or starts with `prefix/`; no permission is inherited from a label |
| `POST /projects/by-remote` | body `{"remote": url}` (credentials already removed by the client); 200 `{"identity", "projects": [Project]}`: only the caller's direct-member, non-archived projects registered for that identity; 400 `invalid` for a local path, `file://` or a missing host; for `minigw project link` without a slug |
| `GET /projects/{id}/remotes` | `{"remotes": [identity]}`; any direct member (403 for a grant alone, 404 when the project is not visible) |
| `POST /projects/{id}/remotes` | body `{"add": [url], "remove": [url]}`; project admin only (effective role, team-derived counts): 403 for reader and developer, 404 for a non-member, 403 `archived`; `{"remotes", "added", "removed"}` |
| `GET /projects/{id}/kb/faq[?after=cursor]` | `{"entries": [{id, title, class, answer, updated, from?, upstream?, source?, text?, asOf?}], "next": ""}`, newest first, at most 50 per page and 64 KiB; the visibility of the note list (private only for its author and the admins); pending submissions never; not on the run port. The first page (no `after`) adds at most 20 entries (16 KiB) of an upstream mirror after the local ones, with `from` (`hub:<upstream> (there: <source>)`), `upstream`, `source`, `asOf` and the plain `text`; `answer` is the quoted, framed rendering (see the search API) |

Request bodies are limited to 512 KiB before they are parsed (413 `too_large`); title 256 bytes and body 64 KiB,
10 000 notes and 100 MiB per project; 60 writes and 600 reads a minute per user.

Note authorization uses metadata before loading a body, including run reads and edit/delete checks.
For an owner/member setup with two developers, one reader and an explicit structure grant, see
[Three people, two projects](usage.md#three-people-two-projects).

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
- `status`: `claudeSandbox` (Mac app, 0.6.0): `"on"` or `"off"` (with `claudeSandboxReason`) for the kernel layer under the Claude CLI without "Allow local tools"; absent while not checked yet and when the Claude CLI has local tools. `core` (both gateways, only when the gateway has a core) is one line about the project database: `projects: on`, or
  `projects: off (reason)`; on `minigw serve` it also says when agents without a sandbox can read all notes (see
  `acceptAgentsCanReadNotes` below) or that old project data on disk keeps such agents out; on the Mac the reason can be
  that the helper is not running or a hard link to a core file. `os` is `windows`, `linux` or `darwin` (the settings page uses it for commands without a sandbox),
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
  `minigw gw claude|codex|grok apikey [remove]` use these fields. The Mac app has a Login field only for Copilot; Claude uses your Keychain login.
- `askOthers` (every agent, off by default): the agent may ask another model of the same gateway with minigw, through a
  loopback port and token valid only for that run (`MINIGW_AGENT_URL`; the token is in a 0600 file of the run, `MINIGW_AGENT_AUTH_FILE`, never in the environment). Claude uses it only with
  `allowLocalTools` (it needs Bash to run minigw); the value is kept either way. Codex needs `network`. The run token
  works only for processes of that agent run (its process tree, also Claude Code's sandbox proxy in it), for at most 2
  requests at a time per run and 6 nested runs on the gateway (`429` otherwise); a nested agent works only in a folder the
  client shared. Log lines of nested requests carry `via=<chain>`.
- Agents other than Claude get their own `agents/{name}` resource when a gateway has them (the Mac: `codex`,
  `grok`); a gateway answers `404` for agents it does not have, and for `siri`. The Mac gateway answers `400` "Grok is not supported on the Mac gateway. Use another agent, or Grok through minigw serve on Windows or Linux." for every `/agents/grok` request (GET, PATCH, models).

| Method and path | Body | Response |
|---|---|---|
| `GET /agents/codex`, `PATCH` | any of `{"enabled", "path", "write", "network", "models", "askOthers", "work", "apiKey"}` | `{"enabled", "path", "found", "write", "network", "models": [string] \| null, "askOthers", "work", "login"?: Login}` |
| `GET /agents/codex/models`, `GET /agents/grok/models` (Go gateway; the Mac answers 400 for grok) | – | `{"models": [{"slug", "name", "description"?, "window"?, "enabled"}]}`: the account's models |
| `GET /foundry`, `PATCH` | any of `{"enabled", "path", "instructions", "models"}` | `{"enabled", "path", "found", "exe", "running", "error", "instructions", "models": [string] \| null}`: Foundry Local on this computer |
| `GET /foundry/models` | – | `{"models": [{"name", "vision", "enabled"}]}`: models downloaded in Foundry Local; `503` when it is not installed or does not answer (Go gateway and the Mac app) |
| `GET /agents/grok`, `PATCH` (Go gateway; the Mac answers 400) | any of `{"enabled", "path", "write", "models", "askOthers", "work", "apiKey"}` | `{"enabled", "path", "found", "write", "models", "askOthers", "work", "login"?: Login}` |
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
- **Core routes (0.6.0):** `users`, `bindings`, `projects`, `grants`, `audit`, `core/backup`, `usage/events` and `quota/history` are not
  implemented in Swift: the app authenticates the owner as for every admin call and passes the request to the helper
  `minigw-core`, whose answer (status, headers, body) goes back unchanged. While the helper is not running the app
  answers `503` with `The project database is not available: the project helper (minigw-core) is not running`; the model
  routes and the other admin routes keep working. When the helper is up but cannot open the database, its own `503`
  carries the reason. There is no admin route for a restore: `minigw gw core restore` runs with the app quit.
- **Quota:** `GET` and `PATCH /quota` are the app's own (see above) and work without the helper.
- **tools without "Allow local tools":** `PATCH /agents/claude` refuses a list with `Bash`, `Task`, an MCP tool or any
  other tool that the file deny rules do not cover (400): the Mac has no sandbox for that mode.
- **Not on the Mac:** the settings page and its sessions (`/minigw/settings`, `POST /settings/login`): the app has
  its own Settings window, so only the admin secret (`minigw gw`) opens the API, and it never sets cookies.


### Synced copies

`GET /sync?device=ID` returns `{"copies":["<copy id>"]}` in sorted order.
`DELETE /sync/<copy id>?device=ID` removes that copy and its usage marker, freeing a slot.
Use a device ID from `minigw gw devices` (including a revoked device's ID), or `local` for local Mac copies.
Removal refuses with 409 while sessions or agents are active. Stop them first; copy changes not pulled to the client are discarded.
For temporary-folder tests, record the copy IDs before and after the run and remove newly created copies after stopping its client.

### Team assignments (owner only)

| Route | Body | Result |
|---|---|---|
| `GET /teams/{id}/assignments` | – | `{assignments: [{project, user, role, id, userName, projectName}]}`, sorted by project/user; also on the remote settings listener, read-only (one numeric id). |
| `PUT /teams/{id}/assignments` | `{project, user, role}` | `{ok: true}` |
| `DELETE /teams/{id}/assignments` | `{project, user}`, or without a body `?project=&user=` (both together: 400) | `{ok: true}`; absent assignment is 404 |

Roles are reader, developer and admin. Set requires an unarchived team, its project and its member;
invalid assignments return 400. Member authentication never permits owner writes.
A team assignment creates the same project membership as a direct one; the strongest wins;
removing a team assignment does not remove a direct one. Team member/project removal and archive
remove matching assignments atomically. Unarchive does not recreate them. Every mutation is audited;
effective narrowing invalidates existing run capabilities through the global epoch.

Existing project-member PUT/DELETE modify only direct origins. Their responses additionally include
surviving `role` and `origins` when membership survives. Member lists include all origins:
`{kind: "direct", role}` or `{kind: "team", team: id, name, role}`.
The member project list includes the caller's origins alongside the effective role. Runtime permission
checks still read only `memberships`, including grant and note-class checks. No new member write API is added.
## KB submissions API

Member routes require an approved authenticated device bound to an active user:

| Method | Path | Result |
|---|---|---|
| POST | `/minigw/v1/projects/{id}/kb/submissions` | Submit `{class,title,body,replaces,revision,entryKind?}`; `entryKind` is `finding` (default) or `faq` (title at most 256 bytes, body at most 2 KiB; a replacement without it keeps the replaced note's kind); required `Idempotency-Key`; 201 |
| GET | `/minigw/v1/projects/{id}/kb/submissions?after=0` | Own metadata, `{submissions,next}` |
| GET | `/minigw/v1/projects/{id}/kb/submissions/{id}` | Own submission with body |
| GET | `/minigw/v1/projects/{id}/kb/review?after=0` | Project admins: the pending submissions of the project (or of a team's own notes, by its `segment`) that the caller may review, never their own, `{submissions,next}`; project `0` means every project and team the caller reviews now |
| GET | `/minigw/v1/projects/{id}/kb/review/{id}` | One submission with body and the writer's `authorName`; project `0` means the submission's own project |
| POST | `/minigw/v1/projects/{id}/kb/review/{id}/accept` | Publish; JSON `{"reason":"","reviewHash":"…"}` with the `reviewHash` of the read; project `0` as above |
| POST | `/minigw/v1/projects/{id}/kb/review/{id}/reject` | Reject; JSON `{"reason":"text"}`; project `0` as above |

Owner routes use the existing owner admin authentication and base URL:

| Method | Path | Result |
|---|---|---|
| GET | `/kb/submissions?project=0&after=0` | Pending metadata; project 0 means all; also on the remote settings listener, without a query (the first 100), read-only. |
| GET | `/kb/submissions/{id}` | Submission with body; also on the remote settings listener, without a query (the first 100), read-only. |
| GET | `/kb/submissions/{id}/versions/{reviewHash}` | The same, only while the submission is that version (400 "the submission changed since you read it" otherwise): the panel's detail of a list row |
| POST | `/kb/submissions/{id}/accept` | Publish; JSON `{"reviewHash":"…"}` (and, for a replacement, `"currentRevision":N`, the revision of the replaced note whose text was shown: without it 400 "this minigw is too old to accept a replacement (it cannot show the note's current text): update minigw (0.9.0 or newer); review it in the Mini Gateway app or in the remote settings, or reject it"), the `reviewHash` of the list row or the read (required); also on the remote settings listener: the body keys are exact (one key, spelled exactly), 30 decisions per 10 minutes. |
| POST | `/kb/submissions/{id}/reject` | Reject; JSON `{"reason":"text"}`; also on the remote settings listener: the body keys are exact (one key, spelled exactly), 30 decisions per 10 minutes. |
| GET | `/kb/notes?project={id}&class=&kind=&after=&limit=` | The notes of a project or of a team's own notes, every class (`private` too), 50 per page by default, `{notes, next}` (`next` absent on the last page) |
| GET | `/kb/notes/{id}/history` | Same history shape as the member route, every class; no list audit |
| GET | `/kb/notes/{id}/history/{rev}` | Saved row with body and provenance; `kb.owner.read` once per read when the row or the current note is private, detail `{project or team, revision}` |
| POST | `/kb/notes/{id}/restore` | Same exact payload and response as member restore; owner only, active project, mirrors refused |
| GET | `/kb/notes/{id}` | One note with body and provenance, any class |
| GET | `/kb/faq?project={id}&after=` | The `faq` notes of a project or a team, newest first, `{entries: [{id, title, class, answer, updated}], next}` |

The three owner read routes (admin channel only: they are not member routes and the run port does not serve them)
let the owner read what a project or a team holds, **including `private` notes**; the owner could already read all of it
through backup and audit. Reading ONE `private` note by id writes an audit row (`kb.owner.read`: the note id and the project id or the team's name, never the content), once per read; lists and notes of other classes are not audited. `project` is a project id or a team's `segment` (from `GET /teams`); a team's own notes show the
team's name in `team` and `project` 0 (also in `provenance`), never the hidden id; the same holds for the team submissions in the review answers. Pending submissions are not notes and never appear. A bad or
missing `project` answers 400, an unknown one 404. The review answers (`GET /kb/submissions`, `/kb/submissions/{id}`) carry,
besides the stored fields, `authorName`, `scopeKind` (`project` or `team`), `scopeName` (the project's or the team's name),
`received` and `isReplacement`, so a panel needs no joins.

Pages contain at most 100 rows; follow nonzero `next` as `after`. Same-outcome
review retries do not publish twice; reversing a review is refused. Submission
keys retain the existing 24-hour scope. Identity/provenance claims in payloads are
rejected. Authorization precedes body retrieval, with invisible/missing 404s.
Called agents cannot submit or review.

**Review by project admins.** The `kb/review` routes are member routes (the member API and the member context; the admin
channel never serves them). Only a member whose role in the submission's project is `admin` (directly or through a team
assignment, as the effective membership computes it; a grant never) may use them; for a team's own notes, whoever may write
them (an admin of one of the team's projects who is on the team's roster). Everyone else, and a submission that does not
exist or lies elsewhere, gets the same 404 as a project they cannot see. A reviewer never reviews their own submission
(four eyes: 400 "your own submission"); the owner still reviews anything. Accept also checks that the reviewer could write
the note themselves (its class, and the note a replacement changes), on top of the writer's checks above. An archived
project answers 403 `archived`; an archived team's notes are not visible (404); a deactivated user or an unbound device is
refused at once (403 `no_project_access`). Accept and reject are audited (`kb.accepted`, `kb.rejected`) with the reviewing
user (`user:<id>`) as the actor, and the published revision's provenance names that user as `reviewer`; the note's author
stays the writer. `private` submissions follow the private rule: the author and the project admins. The member project
list (`GET /minigw/v1/projects`) carries `pendingReview`, the number of submissions the caller may review, for projects
where the caller is admin (absent when 0). The review list (`GET …/kb/review`) also carries `waiting`: the number of pending submissions the caller may review in that project or team segment (not only on the page; the same check as the list, so a caller who may not review gets the list's 404).

**An accept names what was read.** Every review read (the owner's `GET /kb/submissions` rows and `GET /kb/submissions/{id}`,
the member's `GET …/kb/review/{id}`) carries `reviewHash`, a hash of everything an accept publishes (class, title, kind,
text, what it replaces and at which revision). Accept requires it, owner and member alike: without it 400 "name the
version you read", and when the author changed the pending submission since (a `kb import` merges into it) 400 "the
submission changed since you read it: review it again". The Knowledge base panel sends the hash of the row it shows.
When the writer may no longer write the note at accept time (deactivated, device unbound, role or note changed), the
answer is 409 `writer_lost`, never the writer's own 403 or 404. An admin of an archived project does not write or review
the team's own notes (when every project of a team is archived, only the owner does). `reviewHash` is the whole SHA-256 of
the submission id, class, title, kind, body, replaces, revision and its stored provenance, compared whole. For a
submission that replaces a note the hash is versioned (`review/2`) and also covers the note's revision, raw title and text,
class and kind (or `missing`); a submission that replaces nothing hashes exactly as before.

**The replaced note in a review read.** The owner's list rows and detail, the version-bound detail and the member's
detail carry `targetState` (`none`, `ok`, `stale` when the note's revision is not the one the submission expected,
`missing` when it is deleted, of another project, a mirror or not readable by the reviewer), `currentRevision` (0 for
`none` and `missing`), and for `ok` and `stale` `currentTitle`, `currentClass`, `currentKind`, `currentHidden`,
`currentReviewCheck` (`ok`, `hidden`, `long`), the sizes `currentChars`, `currentLines`, `newChars`, `newLines` (of the texts as shown: runes and lines; also on list rows; `new*` for every replacement) and, in a detail only, `currentBody`. `proposalCheck` is the check of the new
text, `reviewCheck` the overall one (`ok` only when the new text is ok, the current text is not long and the note is unchanged; else `hidden`
(the new text), `long` or `target`; hidden characters in the current text are only counted in `currentHidden`). An accept refuses a missing or
changed target with its own 400 reason. Rejecting a stale or missing replacement with no reason stores a default one. The admin submission also
carries `linesChange` and `charsChange` ("current -> new") for the panel. The member list carries
none of this. "Long" (a text over 20000 characters, the current text as well as the new one) is a rule of the panel and the remote
page, enforced for the remote listener by its own check of the read; the core does not refuse it and the owner CLI shows the
full text and may accept. The Mac panel's queue row offers Accept for a new note (`acceptInPanel`: `reviewCheck` is `ok` and nothing is replaced) and, for a
replacement whose note is unchanged and whose two texts are ok, only "Accept replacement" (`acceptReplacementInPanel`), which asks
for a confirmation first and sends `reviewHash` and `currentRevision` from the row; a stale, missing or long replacement
offers neither (the detail shows both texts; `gw kb accept` and the remote page also work). The remote listener refuses, before its limiter counts and with a `denied` audit row, an accept of a
replacement without `currentRevision` (or with 0) and an accept of a submission that replaces nothing that carries one.

Acceptance rechecks the authenticated writer binding, current project permissions,
archive status, note limits and replacement revision. Pending entries are separate
from notes and excluded from note counts and run reads. Pending limits: 100 / 5 MiB
per user, 500 / 20 MiB per project. Reasons are at most 1024 bytes; reasons and
submission titles reject control characters. Existing request/note/rate limits apply.
Notes, submissions and search hits carry `entryKind` (`finding` or `faq`); `note push` and `note edit` ignore an `entryKind` in their body.
Note read JSON gains `provenance` with actorKind, user, device, project, sourceKind,
sourceReference, contentHash, authored, received, reviewer and reviewed.
Legacy device/source are explicitly `UNKNOWN`. Schema 11 uses snapshot-first
migration; backups retain submissions and revision provenance. No automatic expiry
or customer-data detection is implemented.

## KB lexical search API

| Method | Path | Result |
|---|---|---|
| GET | `/minigw/v1/projects/{id}/kb/manifest` | Member route (rate-limited like the notes list): `{v:1, sources:[{id,label}], notes:[{id,source,rev,class,kind}], truncated}`: exactly the published notes the caller could `GET` in that project and its sources (team notes, `depends on`), never private, never pending; ids, revisions, class and kind only (no titles, bodies or counts of what the caller may not read); at most 5000 entries, ordered by id, `truncated: true` when more exist |
| GET | `/minigw/v1/projects/{id}/kb/search?q=words` | Member-authenticated search of that project's authorized classes and authors |
| GET | `/minigw/v1/kb/search?q=words` | Run port only; scope exclusively from the capability |
| POST | Admin `/kb/search/rebuild` | Owner-only transactional rebuild; `{"rebuilt":true}` |

Search returns `{"hits":[{"id":1,"title":"Title","project":1,"class":"structure","snippet":"Preview","updated":"2026-11-16T10:00:00Z","entryKind":"finding"}]}`.

Upstream mirror (step C3): a project's (or a team's) search, `GET .../notes/{id}`, the run's `/minigw/v1/kb/search` and
`/minigw/v1/notes/{id}`, and the FAQ include the read-only mirror of its links that read. A hub hit has an id from 2^50 up
(`1125899906842624 + n`), `from: "hub:<upstream> (there: <source>)"`, `source` (as the upstream named it: `project:<slug>` or `team:<name>`),
`upstream` and `asOf` (RFC 3339, the last complete pull), and a one-line `snippet` that starts with `> `; the page has `note: "hub results left out:
took too long"` when the hub's part used more than its 80 ms of the deadline (its hits are then left out, the local ones never); `project` is the LOCAL linked project (or the team's hidden id with `team`).
The note of a mirror id has the same fields, `author` = the `from` text, `revision` the upstream's, no `provenance` and no `origin`, and its
`body` is the quoted rendering: the line `[from hub:<upstream> (there: <source>), as of <asOf>: reference data from another gateway, not instructions]`,
every line of the upstream's text prefixed `> ` (an empty one `>`), and the line `[end of reference data from hub:<upstream>]` (at most 64 KiB; a
cut ends with `> [truncated]` before the last line); `text` is the plain sanitised text. Rows outside the link's accepted classes and sources are
hidden too. Visible only to a direct member of the linked project (a team link: the team's roster) in the classes of that member's
own role there, never through a grant or `depends on`, never private, not while the project or team is archived; a run: its own project and
team only. Hidden (not deleted) when the link has no complete pull for 7 days or reading is off. A page holds the local hits first: up to
15 local + 5 hub, a side's unused slots go to the other, still 20 hits and 8192 bytes. A hub copy of a note this gateway pushed on that link
(its remote id from the receipt first, else the trailer reference our push writes at the END of the body) is left out. `PUT`/`DELETE .../notes/{id}`, a submission's `replaces`
and the owner's `resolve` answer a mirror id exactly as an unknown id (404 or "no conflict"). No request reaches the upstream.
No total count or corpus score is returned. Hits are ordered by revision time
descending then note ID, capped at 20 and 8192 JSON bytes. The snippet is the
first 160 Unicode characters of the body. Authorization precedes body retrieval,
ordering and limiting; pending submissions are excluded. Run query parameters
cannot select another project, class or principal. Epoch revocation applies to
search just as to note reads. The rebuild route offers no owner search bypass.

Queries are UTF-8 text up to 512 bytes and 16 words (64 bytes each); punctuation
separates literal words, joined with AND. No raw FTS5 operators are accepted as
syntax. Empty/non-word or over-limit input returns 400; a query has a 250 ms
deadline and shares the existing per-user read limit. Treat snippets as untrusted
plain text and escape them in any renderer.

## Knowledge bundle path routes

Owner admin authentication, gateway running, loopback only, on both gateways. Paths below the existing
admin prefix:

| Method | Path | Body |
|---|---|---|
| POST | `/export` | `{"team":"Engineering","projects":["fakture"],"with_private":false,"path":"/absolute/local/team.zip"}` |
| POST | `/import` | `{"path":"/absolute/local/team.zip","source":"engineering","dry_run":true,"map":{"ana":"Ana"},"adopt":["project:shop","user:alice"]}` |

The body names a local path on the gateway computer; it never contains zip bytes. The normal 512 KiB
request cap remains. Export refuses an existing destination. Responses contain `{"lines":[...]}`;
a refusal also includes `error.message` with a non-2xx status. Import requires `source`: the owner-chosen
name of the bundle's origin on this gateway (1–64 characters, using team-name validation). The first import
under a source records the bundle's `origin_gateway`; a later import under that source from another gateway
is refused, as is a bundle whose `origin_gateway` is this gateway. Receipts are keyed by source name,
project slug and source note id, never by the bundle's own strings.

An import merges only into projects, teams and users that the same source created (the table `import_objects`) or that the
owner adopted for it. `adopt` (optional) lists local objects as `project:<slug>`, `team:<name>` or `user:<name>`; a name in
the bundle that matches any other local object refuses the whole import (400, before any write) with one line per conflict
in `error.message`; a malformed entry, or one that matches no object of the bundle, is refused too. A user is adopted
only when listed. An adopted object keeps its roles (no raise), grants from an adopted project are added only when the project
is listed, and the `kb.import` audit event carries `adopted` (the listed objects used). A dry run answers with the same refusal.

Import validates the complete archive before decompressing note bodies: it bounds the archive size, the
central directory, the entry count, each declared entry size and the total, the manifest size and nesting,
and the sizes of the manifest's collections. It strictly decodes the version 1 manifest, rejects unsafe
paths, non-regular entries and names that collide after Unicode normalization and case folding, then
applies one transaction. Dry-run does no writes. A repeat with no changes writes no audit; an applied
bundle writes one `kb.import` event.

Each imported note records a trusted local provenance: actor kind `import`, no user id, the target project,
source kind `import`, source reference `<source>/<project slug>/<source note id>`, the receive time and no
reviewer. The mapped author name is for display; it grants no own-note access. A trusted local import flag
enforces that separately from source metadata. What the bundle claims about the note's history is stored
separately (at most 64 KiB) and returned by note routes as `unverifiedSource`; it is not used by permission,
provenance or review logic. The receipt
keeps the note revision and class written by the last import; a local revision change, including a class
change, makes later imports skip the entry instead of overwriting it, and an incoming class change applies
only to a note not changed locally. Names match after Unicode NFC normalization and case folding; two target
accounts that normalize to one name, or two bundle users that map to one target account, refuse the import.
No route calls Backup or Restore.

The member route `POST /minigw/v1/projects/{id}/kb/import` accepts one
`{"markdown":"...","path":"relative/file.md","class":"","submit":false}` and returns
`{"action":"create|merge|skip","key":"...","note":123,"submission":456,"reason":"..."}`
(IDs and reason omitted when absent). It derives identity and permissions from the authenticated member,
never front matter. Create permissions, update ownership, class transitions, archive checks, pending
limits, body limits and the normal write rate apply. Imports persist receipts beyond the push TTL.
Member exports page the existing list route and fetch each note with the existing GET route.
The client opens each file once without following links, checks the open descriptor is a regular file and
reads fewer than 96 KiB; a folder import allows at most 10,000 total entries and 32 path components.
Folder reads use a held root directory so replacing a parent with an external link cannot redirect reads.

Note restore keeps the current class when the client omits `--class`; the API requires an explicit current or saved class. It writes provenance `sourceKind: restored` and `restoredFromRevision`, and audits `note.restore` with scope, `from`, new `revision`, target `class` and saved `fromClass`, never title or body. Normal note and project limits apply; history budgets evict as on an edit. Unchanged title/body/class/kind returns 400 `revision N has the same text as the current note: nothing to restore`. History bytes count title plus body, not the storage budget charge; `savedBy` is the overwriting user name, empty for a review or import. History and restore are refused on the run and internal channels and on the remote settings listener.
