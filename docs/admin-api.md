# Mini Gateway admin API (v1)

One API to configure a router on its own computer. The Go router (`minigw serve`) and the Mac app implement it;
[Mac differences](#mac-app) are listed at the end. `minigw router …`, the Windows tray and the settings page use it. Changes apply at once,
except where a response says `"restartRequired": true`.

## Access

- Base path: `/minigw/admin/v1`, on the router's normal port.
- **Only from the router computer.** The server checks the TCP peer address of the connection (loopback), never
  a header. A request from another address gets `403`, even with the right secret.
- **Host header** must be `127.0.0.1:<port>`, `localhost:<port>` or `[::1]:<port>` (the port the connection came
  in on). Anything else gets `403`. This blocks DNS rebinding from a web page.
- **Admin secret, never sent:** 32 random bytes, base64url without padding (43 characters), in the file
  `admin.secret` (mode 0600) in the router's config folder (the folder agents may not read; the Mac app keeps it in
  its Keychain instead, see [Mac app](#mac-app)). The router creates it on first start. A client speaks MGW2 (Noise NK) to `127.0.0.1` with the router's public key and sends
  `Authorization: MGW-Admin <base64url(HMAC-SHA256(key = the secret as stored, "minigw-admin-v1" || h))>`, where
  `h` is that connection's Noise handshake hash. The router computes the same value from its own `h` and compares
  in constant time. A fake server (its own key, its own handshake) gets a proof that is valid nowhere else; a
  request without Noise, or with `Bearer`, gets `401`. The router's client token is not the admin secret.
- **The client finds the config without the environment:** `minigw router` resolves the default config folder from
  the user database (Unix) or the RoamingAppData known folder (Windows), not from `HOME`, `XDG_CONFIG_HOME` or
  `%APPDATA%`, so another process cannot point it at a fake `serve.json` with a fake router key.
- **Settings page session** (instead of the secret), without cookies: browsers send a `127.0.0.1` cookie to every
  port, so any local web server would get it. The tray or `POST /settings/login` (below) opens
  `/minigw/settings#t=<one-time token>`; the token is in the fragment, so it never reaches a server in a request
  line. The page is static and holds no secret. Its script drops the fragment, sends
  `POST /minigw/settings/login {"t": …}` (valid 60 s, once) and gets `{"session": id}`, which it keeps in
  `sessionStorage` (scheme, host and port) and sends as `X-Minigw-Session` on every API call. A page from another
  origin cannot send that header without a preflight, which fails (no CORS). Missing or wrong: `401`. A session ends
  after 30 minutes without use, with `POST /minigw/settings/logout`, or when the router token is rotated.
- **Agents are refused** (Go router, Linux and Windows): an admin request (`MGW-Admin`, a settings page session, or
  `POST /minigw/settings/login`) from a process that the router started, or one of its descendants, gets `403`,
  even with the right secret.
  - Linux: the client's socket in `/proc/net/tcp{,6}` → the processes holding it → their parents up to PID 1.
    Every agent (claude, codex, grok) runs under a small helper (`minigw __minigw-agent`) that is a child
    subreaper: a descendant whose parent exits (double fork) moves to the helper, not to init, so the chain to the
    router stays. The helper reaps its own subtree and kills what is left when the agent ends. Under the systemd
    unit the unit's cgroup is checked too. The router is not dumpable (`PR_SET_DUMPABLE 0`), so agents cannot read
    its memory or environment.
  - Windows: every agent runs in one Job object of the router (created suspended, added, then resumed); the
    client's PID comes from `GetExtendedTcpTable` and `IsProcessInJob` decides.
  - If the process cannot be found or read, the request is refused.
- Every admin API response has `Cache-Control: no-store`.
- Loopback skips the device check and encryption for normal API calls; the admin API still needs the secret.
- **Clients authenticate the router first.** Something else may listen on the port (the
  router is stopped, another program took it). The CLI therefore speaks MGW2 (Noise NK) to `127.0.0.1` with the
  router's public key from the config; a program without the router's private key cannot finish the handshake and
  never sees the secret or a gateway key. A loopback Noise connection is still a loopback peer.
- The router must listen on `127.0.0.1` as well: `address` is empty (all), `0.0.0.0`, `::` or `127.0.0.1`.
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
Device   {"id": string, "name": string, "address"?: string, "time"?: string}
         id: base64url of the device's 32-byte Ed25519 public key (43 characters), as the refusal message shows it.
         address and time (RFC 3339, UTC) only for pending devices.
Model    {"id": string, "model": string, "window"?: number, "enabled": bool}
         id: the ID at the gateway; model: the name for /model (the id, or "claude-" + id when the id contains
         neither "claude" nor "anthropic"); window: context window in tokens, if known.
Gateway  {"name": string, "baseURL": string, "insecure": bool, "models": [string] | null, "hasKey": bool, "ready": bool,
          "enabled": bool, "aliases": string}
         models: enabled gateway IDs; null = all discovered, now and later. ready: model list loaded.
Claude   {"path": string, "found": bool, "tools": string, "allowUnsandboxedBash": bool, "folder": string,
          "exe"?: string, "version"?: string}
         path: "" = from PATH; tools: comma-separated tool names; folder: "" = only synced copies; exe: the
         claude that was found; version: its `claude --version` (both only when found; Go router only).
General  {"port": number, "address": string, "connect": string, "restartRequired"?: true, "allowedClients": string,
          "allowPlainRemote": bool, "allowAnyDevice": bool, "scratch": string}
         address: "" = all addresses; connect: the "minigw connect …" line for clients (contains the token).
```

## Resources

| Method and path | Body | Response |
|---|---|---|
| `GET /status` | – | `{"running", "version", "port", "address", "installed", "devices": n, "pending": n, "claude": Claude, "gateways": [Gateway], "os", "installDir"?, "tray", "autostart", "agents": n, "agentsUnsandboxed"}` |
| `GET /devices` | – | `{"devices": [Device], "pending": [Device], "allowAnyDevice": bool}` |
| `POST /devices` | `{"id", "name"}` | `{"device": Device, "added": bool}`; removes it from pending |
| `DELETE /devices/{id or name}` | – | `{"device": Device}`; a pending device is only removed from pending (`"pending": true`); a name must be unique |
| `GET /gateways` | – | `{"gateways": [Gateway]}` |
| `POST /gateways` | `{"name", "baseURL", "key", "insecure"}` | `{"gateway": Gateway, "keyStore": string, "models": [Model]}` |
| `PATCH /gateways/{name}` | any of `{"name", "enabled", "baseURL", "insecure", "aliases"}` | `{"gateway": Gateway}`; a new name moves the key; the key stays for a new URL |
| `DELETE /gateways/{name}` | – | `{"name"}`; also deletes the key |
| `GET /gateways/{name}/models?filter=text` | – | `{"models": [Model]}`; filter: part of `model`, any case |
| `POST /gateways/{name}/models` | `{"all"?: bool, "enable"?: [id or model], "disable"?: [id or model]}` | `{"models": [Model], "changed": [Model]}` |
| `GET /agents/claude` | – | Claude |
| `GET /agents/claude/tools` | – | `{"tools": [string]}`: the tools of this Claude Code (for the exclude table with local tools) |
| `PATCH /agents/claude` | any of `{"path", "tools", "allowUnsandboxedBash", "folder"}` | Claude |
| `GET /general` | – | General |
| `PATCH /general` | any of `{"port", "address", "startAtSignIn", "allowedClients", "allowPlainRemote", "allowAnyDevice", "scratch"}` | General; `"restartRequired": true` for port or address |
| `POST /general/token` | – | General with the new token in `connect`; the old token stops working at once |
| `POST /settings/login` | – | `{"url"}`: a one-time link to the settings page (`…/minigw/settings#t=…`, 60 s); `503` when the router does not run |

Notes:

- `POST /devices` takes only a valid device ID. `pending` lists the last 20 devices that were refused since the
  router started (in memory), newest last.
- `POST /gateways` adds or replaces a gateway. Name: letters, digits, `. _ -`, up to 32. `baseURL` without `/v1`;
  it must be `https`, or `http` to localhost or a private IP with `"insecure": true`. The router lists the models
  with the key before it saves anything, and stores the key in the system's secret store (Credential Manager,
  Keychain or Secret Service), never in the config. `keyStore` says where. A new gateway with more than 20 models
  starts with none enabled. The key is never returned.
- `status`: `os` is `windows`, `linux` or `darwin` (the settings page uses it for commands without a sandbox),
  `installDir` the folder of `minigw serve install` (only when minigw is installed there), `tray` whether the
  Windows tray runs. `autostart` is `"boot"` (Linux with linger: the router starts without anyone signing in),
  `"sign-in"`, or `""` (it does not start on its own). `agents` counts the claude, codex and grok processes that
  run right now; an update stops them, so check it first. `agentsUnsandboxed` (Go router): Codex and Grok have no sandbox here that hides the router's secrets
  (Windows, Linux without a working `bwrap`), so they run only with `allowUnsandboxedBash`. `PATCH /general {"startAtSignIn"}` sets the Windows Run
  value for the running router and, on the Mac, Open at Login; Linux answers `400` (use `minigw serve install`).
  The Mac app leaves `os`, `installDir` and `tray` out.
- `POST /gateways/{name}/models`: `all` first (true = all, false = none), then `enable`, then `disable`.
- The Go router's Claude also has `"enabled", "folders"` (comma-separated allowed folders), `"allowLocalTools",
  "disabledTools", "permissionMode", "settingSources"`, as on the Mac; see [windows-parity.md](windows-parity.md).
- `PATCH /agents/claude`: the router checks the path, that the folder exists and is not near the config folder,
  and that Bash on Windows has `allowUnsandboxedBash`.
- Agents other than Claude get their own `agents/{name}` resource when a router has them (the Mac: `codex`,
  `grok`); a router answers `404` for agents it does not have, and for `siri`.

| Method and path | Body | Response |
|---|---|---|
| `GET /agents/codex`, `PATCH` | any of `{"enabled", "path", "write", "network", "models"}` | `{"enabled", "path", "found", "write", "network", "models": [string] \| null}` |
| `GET /agents/codex/models`, `GET /agents/grok/models` | – | `{"models": [{"slug", "name", "description"?, "window"?, "enabled"}]}`: the account's models (Go router) |
| `GET /agents/grok`, `PATCH` | any of `{"enabled", "path", "write", "models"}` | `{"enabled", "path", "found", "write", "models"}` |
| `GET /siri`, `PATCH` | any of `{"enabled", "timeout", "allow": [id], "deny": [id]}` | `{"enabled", "timeout", "devices": [{"id", "name", "siri"}]}` |

`write`: the agent's sandbox may write in the session's folder (else read-only); `network`: commands in Codex's
sandbox may use the network (default: off); `models: null` = all the account's models. `allow`/`deny` switch Siri for known
devices (Siri uses this Mac's personal data).

## Mac app

The Mac app keeps its settings in its preferences and Keychain; the Settings window shows API changes at once.

- **Admin secret and the router's identity:** one item in the app's Keychain (account `admin`: JSON with
  `secret`, `port` and the router's public `key`), not a file: Codex and Grok agents on the Mac may read files
  anywhere, but not the Keychain, and a file or environment variable (`CFFIXED_USER_HOME`) could be faked by another
  process. `minigw router` reads the item with the user's consent (macOS asks once), refuses to run with
  `CFFIXED_USER_HOME` set, speaks Noise directly to `127.0.0.1:<port>` with that key (no local proxy) and sends only
  the `MGW-Admin` proof; the secret never leaves the Keychain.
- **No agents (defence in depth, after the proof):** the app finds the process behind each admin connection (libproc:
  the process with that TCP socket, both addresses loopback in the socket's family; `LOCAL_PEERPID` works only for
  Unix sockets). It refuses (`403` "admin requests from agent processes are not allowed") when there is not exactly
  one such process, when it is sandboxed (`sandbox_check`; a child that double-forks away stays sandboxed), when it
  descends from the app (parents via `sysctl KERN_PROC_PID`), or when the chain of parents cannot be read up to
  PID 1. So an agent that runs `minigw router` gets nothing, even after "Always Allow" in the Keychain.
- **Order of checks:** loopback peer → Host (plain HTTP only; over Noise there is no browser) → `MGW-Admin` proof →
  process check.
- **No offline mode:** without the running app, `minigw router` stops with a message (settings live in the app).
- **Devices:** the Mac accepts every device with the token and adds it to the list itself; the list decides Siri.
  `GET /devices` returns `"allowAnyDevice": true`, `"pending": []` and a `siri` flag per device. `POST` adds a
  device; `DELETE` removes it, but it comes back on its next connection (it only loses Siri). To lock a device out,
  rotate the token. The admin API can add a device ID that never connected and allow Siri for it (`PATCH /siri`);
  only someone with the admin secret on this Mac can do that, and that device still needs the token.
- **Gateways:** each has `"enabled"`. `name` is the gateway's name in Settings. `POST` turns the gateway on and
  applies the same URL rule as Go; gateways added in Settings may still use any `http` URL. The key goes to the
  Keychain only after the config change succeeds, and before the app restarts the router (the router reads gateway
  keys when it starts). Gateways with manual
  aliases (Settings → Advanced) list their discovered models here, but route by their aliases.
- **agents/claude:** `{"enabled", "path", "found", "tools", "folders", "allowLocalTools", "disabledTools"}`.
  `folders` is the comma-separated list of allowed folders. There is no `folder` or `allowUnsandboxedBash`: with
  `allowLocalTools`, Bash runs in Claude Code's sandbox.
- **general:** also `allowedClients` (IP addresses, comma-separated; empty = all) and `allowPlainRemote`. `address`
  may be `0.0.0.0`, `::` or `127.0.0.1`. A port or address change restarts the listener, so there is no
  `restartRequired`.
- **status:** `pending` is always 0; `installed` is Open at Login.
- **Not on the Mac:** the settings page and its sessions (`/minigw/settings`, `POST /settings/login`): the app has
  its own Settings window, so only the admin secret (`minigw router`) opens the API, and it never sets cookies.

