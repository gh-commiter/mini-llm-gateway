# Mini Gateway admin API (v1)

One API to configure a router on its own computer. The Go router (`minigw serve`) implements it; the Mac app will
implement the same API. `minigw router …`, the Windows tray and the settings page use it. Changes apply at once,
except where a response says `"restartRequired": true`.

## Access

- Base path: `/minigw/admin/v1`, on the router's normal port.
- **Only from the router computer.** The server checks the TCP peer address of the connection (loopback), never
  a header. A request from another address gets `403`, even with the right secret.
- **Host header** must be `127.0.0.1:<port>`, `localhost:<port>` or `[::1]:<port>` (the port the connection came
  in on). Anything else gets `403`. This blocks DNS rebinding from a web page.
- **Admin secret, never sent:** 32 random bytes, base64url without padding (43 characters), in the file
  `admin.secret` (mode 0600) in the router's config folder (the folder agents may not read). The router creates it
  on first start. A client speaks MGW2 (Noise NK) to `127.0.0.1` with the router's public key and sends
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
Gateway  {"name": string, "baseURL": string, "insecure": bool, "models": [string] | null, "hasKey": bool, "ready": bool}
         models: enabled gateway IDs; null = all discovered, now and later. ready: model list loaded.
Claude   {"path": string, "found": bool, "tools": string, "allowUnsandboxedBash": bool, "folder": string}
         path: "" = from PATH; tools: comma-separated tool names; folder: "" = only synced copies.
General  {"port": number, "address": string, "connect": string, "restartRequired"?: true}
         address: "" = all addresses; connect: the "minigw connect …" line for clients (contains the token).
```

## Resources

| Method and path | Body | Response |
|---|---|---|
| `GET /status` | – | `{"running", "version", "port", "address", "installed", "devices": n, "pending": n, "claude": Claude, "gateways": [Gateway]}` |
| `GET /devices` | – | `{"devices": [Device], "pending": [Device], "allowAnyDevice": bool}` |
| `POST /devices` | `{"id", "name"}` | `{"device": Device, "added": bool}`; removes it from pending |
| `DELETE /devices/{id or name}` | – | `{"device": Device}`; a pending device is only removed from pending (`"pending": true`); a name must be unique |
| `GET /gateways` | – | `{"gateways": [Gateway]}` |
| `POST /gateways` | `{"name", "baseURL", "key", "insecure"}` | `{"gateway": Gateway, "keyStore": string, "models": [Model]}` |
| `DELETE /gateways/{name}` | – | `{"name"}`; also deletes the key |
| `GET /gateways/{name}/models?filter=text` | – | `{"models": [Model]}`; filter: part of `model`, any case |
| `POST /gateways/{name}/models` | `{"all"?: bool, "enable"?: [id or model], "disable"?: [id or model]}` | `{"models": [Model], "changed": [Model]}` |
| `GET /agents/claude` | – | Claude |
| `PATCH /agents/claude` | any of `{"path", "tools", "allowUnsandboxedBash", "folder"}` | Claude |
| `GET /general` | – | General |
| `PATCH /general` | any of `{"port", "address"}` | General with `"restartRequired": true` |
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
- `POST /gateways/{name}/models`: `all` first (true = all, false = none), then `enable`, then `disable`.
- `PATCH /agents/claude`: the router checks the path, that the folder exists and is not near the config folder,
  and that Bash on Windows has `allowUnsandboxedBash`.
- Agents (Codex, Grok, Copilot) get their own `agents/{name}` resource when a router supports them; a router
  answers `404` for agents it does not have.
