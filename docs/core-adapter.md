# The core adapter: what a gateway must do itself

Everything that turns "a request and who is asking" into "status, headers, body" for projects, notes, usage and the
core's admin routes is in the Go package `client/coreapi`. `minigw serve` calls it in-process; the Mac helper
(`minigw-core`) hosts the same package behind a socket, and the Mac app forwards requests to it with a trusted context. A gateway
(the **adapter**) does only what is listed under "Stays in the adapter"; everything else goes through `coreapi`.

## Entry point

```go
func (a *API) Handle(ctx context.Context, c Context, r Request) Response
```

| Type | Fields (JSON) |
|---|---|
| `Context` | `kind` (`member`, `admin`, `run`, `internal`), `device`, `approved`, `agentPeer`, `run` (a `RunCap`) |
| `Request` | `method`, `path`, `query`, `header` (only `Idempotency-Key` is read), `body`, `bodyTooLarge`, `bodyUnreadable` |
| `Response` | `status`, `header`, `body` (JSON; empty for 204). Admin: `status` 0 = not a core route, the adapter goes on |
| `RunCap` | `runId`, `userId`, `deviceId`, `projectId`, `epoch` |

What the adapter vouches for in the `Context`:

- **member**: `device` is the device key proven in this tunnel (empty = none); `approved` = that device is in the approved list **now**
  (not the model routes' loopback or "allow any device" exceptions); `agentPeer` = the caller is a process of one of the gateway's agents.
  Routes: `GET /minigw/v1/projects[?archived=1]`, `GET /minigw/v1/projects/{id-or-slug}`, `POST …/{id-or-slug}/link-check`,
  `…/projects/{id}/notes[/{note}]` (GET, POST, PUT, DELETE), `GET /minigw/v1/usage?project=&days=`. Shapes: docs/admin-api.md.
- **admin**: the owner has been authenticated (loopback, Host, `MGW-Admin` proof, agent peers refused). `path` is the escaped path **below**
  the admin prefix. Core routes: users, bindings, projects, members, grants, audit, `core/backup`, `usage/events`.
  `coreapi.IsAdminPath(method, path)` says whether a path is the core's.
- **run**: the run port's token and peer checks passed; `run` is the capability, `approved` the device's approval now. Routes:
  `GET /minigw/v1/notes[?class=&after=&limit=]` and `GET /minigw/v1/notes/{id}`; nothing else.
- **internal**: a call of the gateway itself, `POST /internal/<name>` with a JSON body (below).

Inside `coreapi` (not the adapter's business): the principal (binding, active user), authorization order (visible, allowed, archived,
revision, rate limit, size), 404 before validation, the fixed `no_project_access` body, `Retry-After`, the error envelope
`{"error":{"type","message"}}`, body limits (`coreapi.MaxBody` = 512 KiB; the adapter reads at most that and sets `bodyTooLarge`),
idempotency, rate limits, usage reports and gap visibility, the usage recorder (queue, spool file, replay, retention).

## Internal calls

All answers are 200 with the JSON shown, unless noted. Errors use the member envelope.

| Call | Request | Answer |
|---|---|---|
| `epoch` | `{}` | `{"epoch":N}` |
| `admit` | `{"device","approved","project":"<header value or empty>"}` | `{"ok":true,"user","project","runId","epoch"}` or `{"ok":false,"status":403\|429\|503,"type","message","retryAfter"}`. Empty `project`: `{"ok":true}`. A refused agent request must **never** run without the project; the adapter shows the refusal in its own error shape |
| `scope` | `{"site":"agent"\|"cli","fields":[…],"project":{"user","project","epoch"}?}` | `{"key"}`: the conversation key. No project: the fields joined by `\|` (the 0.5.26 key). With a project: its own namespace (`\x00project\x00` + JSON array) |
| `agent-home` | `{"kind","user","project","epoch"}` | `{"dirName","relative":"proj/<u>-<p>-e<e>/<kind>","prefix":"<u>-<p>-e","epoch","maxHomes":20}`: the home of an agent run with a project; homes with `prefix` and a lower epoch are dead |
| `device-revoked` | `{"device"}` | `{"ok":true}`; raises the epoch (500 if it cannot) |
| `reconcile` | `{"devices":[…]}` | `{"done":[…]}`: epoch raised for each, in order; the first failure stops the list |
| `ensure-owner` | `{"name":"<OS account>"}` | `{"ok":true}`: creates the owner user once, when there is no user |
| `bound-user` | `{"device"}` | `{"user":N}`: attribution only, from an in-memory snapshot (the last binding seen when the core is down) |
| `event-lost` | `{"count","from","to"}` | `202 {"queued":true}`: events the adapter could not deliver (its own queue overflowed while the helper was down); shown as a gap like those the recorder drops itself. Count under 1 is refused (400) |
| `event-begin`, `event-finish` | `{"event":{"id","parentId","start","end","userId","projectId","deviceId","provider","model","outcome","tokensIn","tokensOut","tokenSource"}}` | `202 {"queued":true}`; never blocks. `tokensIn/out` -1 = unknown; `outcome` `ok\|error\|cancelled`; `tokenSource` `reported\|estimated\|unknown` |

`coreapi.API.Call(ctx, name, in, out)` makes these calls in-process through the same path.

## Wire form (helper socket)

Length-prefixed JSON frames, in order, one request at a time per connection: a 4-byte big-endian length, then the JSON.
Request frame `{"v":1,"id":N,"context":Context,"request":Request}`, response frame `{"v":1,"id":N,"response":Response,"error":""}`.
`v` is `coreapi.ProtocolVersion`; a frame over `coreapi.MaxFrame` (4 MiB) is refused before its payload is read; the answer
carries the id of its request. Go: `coreapi.WriteRequest/ReadRequest/WriteResponse/ReadResponse`, `API.Serve(ctx, conn)`,
`NewClient(conn).Do(ctx, context, request)`. A body travels base64-encoded inside the JSON.

## Stays in the adapter (the Mac app's list)

1. **Tunnel and device proof**: the Noise tunnel, the Ed25519 device proof, so that `device` is trustworthy.
2. **The approved-device list** and its changes; on removal call `device-revoked` before answering the admin call, and keep a
   durable pending-revocation mark next to the device list when the core cannot be reached; at the next core open call
   `reconcile` with the pending list **before** project routes are enabled, then clear the marks that are `done`.
3. **Admin authentication** (loopback, Host, the proof bound to the tunnel, agents refused) and all admin routes that are not the core's
   (devices, providers, settings, status, quota, 0.5.x counters).
4. **The gateway token** for member routes, the exclusion of requests that come through a run port, and the detection of agent peers
   (`agentPeer`).
5. **Reading the body** with a limit of `coreapi.MaxBody` and a deadline, then building the `Request`; writing the `Response` (headers, status, body).
6. **Agent runs**: launch and sandbox; the run port (listener, token, process-tree check, GET-only paths) and the run's lifetime; the
   run's `RunCap` comes from `admit`; a nested run inherits the root's. Notes are reachable only where the sandbox allows the run port.
7. **Project admission of an agent request**: read the client's project header (set by minigw's own proxy), call `admit`, refuse in
   the gateway's own error shape, put the result into the run. Conversation keys through `scope`, agent homes through
   `agent-home` (the registries and the folders themselves are the adapter's).
8. **Usage events**: decide when a model request is *dispatched* (never for a local refusal), freeze user (`bound-user`),
   device, project and parent at admission, and send `event-begin` and `event-finish`. Token counting and streams are the adapter's.
9. **Quota** probes and their settings (they run CLIs and provider HTTP).
10. **Opening the store** (`core.Open` with the private-folder and backup-file protection of the platform) and closing it; the
    OS account name for `ensure-owner`.
11. **Keeping agents out of the core data folder** (#106): every agent launch denies the folder by its real path (read and
    write, all files in it) or the agent kind does not run; allowed folders around it are refused; where an agent cannot be
    denied (no sandbox) the store opens only with the owner's `acceptAgentsCanReadNotes`, and the check is repeated at each
    launch. See gateway.md, "The core data folder is closed to every agent".

## The Mac column: which duties are done

The Mac app is the adapter of the helper `minigw-core` (`client/cmd/minigw-core`; Swift: `Sources/RouterCore/CoreHelper.swift`,
`CoreForward.swift`). The helper is started with fd 3 = a socketpair end and fd 4 = a pipe with a 32-byte per-start
secret (both fixed); it checks that the peer of fd 3 is its parent (`LOCAL_PEERPID`, `LOCAL_PEERCRED`, parent pid
captured at start), then a mutual HMAC handshake proves the secret both ways and negotiates the protocol version
(`hello`, `hello_ok`, `auth`, `ready`; frames of at most 1 KiB), then `coreapi` frames follow. It has no listener, logs
reasons only (never bodies), answers a request within 30 s or with the `core_unavailable` 503, and exits on EOF (0),
when its parent changes (3), or when the peer is refused (1). A store that cannot be opened is not an exit: every
request is answered with the 503. `testdata/unavailable.json` and `testdata/no_project_access.json` (the two bodies the
app may answer itself) and `testdata/paths.json` (`IsMemberPath`, `IsAdminPath`) are asserted by the Go and the Swift tests.

| # | Duty | Mac, 0.6.0 task "core helper" |
|---|---|---|
| 1 | Tunnel and device proof | existing Noise code; `device` of a member request is the one proven in this tunnel |
| 2 | Approved list, revocation | done: `AppState.apply` marks every removal in the same save (`RouterConfig.pendingRevocations`), the admin route calls `device-revoked` before it answers (`CoreHelper.revoke`), the next ready sends `reconcile` before project routes are served |
| 3 | Admin authentication, non-core admin routes | unchanged; core routes (`IsAdminPath`) are forwarded after it |
| 4 | Token for member routes, run port exclusion, `agentPeer` | done (`Router.memberHTTP`; a request that comes through a run port never reaches it: `AgentRun.current`) |
| 5 | Body reading and the `Request` | done (`MaxBody`, `bodyTooLarge`; `bodyUnreadable` never occurs because the body is complete when routing starts) |
| 6 | Agent runs, run port, `RunCap` | done: `AgentRun` (`Sources/RouterCore/AgentRun.swift`) opens the port when the agent may ask other models **or** the run has a project; the `run` kind (`Router.runNotes`, `ProjectRun.swift`) serves `GET /minigw/v1/notes[/{id}]` only, forwarded with the run's `RunCap` and `approved` = the device on the approved list now; every other method or path is the 404 of the run port, the models' routes only with "May ask other models"; the capability ends with the run (`stop()`: port closed, token file removed); token and process-tree checks unchanged. A nested run inherits the root's capability; the project header is dropped from a nested call |
| 7 | Project admission of an agent request, `scope`, `agent-home` | done: `Router.admitProject` reads `x-minigw-project` (set by minigw's own proxy; refused with the fixed 403 `no_project_access` or 503 `core_unavailable` in the Anthropic error shape, never run without the project), in `Router.syncRun` for all four agent kinds after `count_tokens`; `conversationScope` (`scope`) for Claude CLI, Codex, Grok and Copilot; `projectHome` (`agent-home`): `<Scratch>/state/<device hash>/proj/<user>-<project>-e<epoch>/<kind>` for Codex (`CODEX_HOME`, login and settings as links), Grok (`GROK_HOME` with its `minigw-core` profile) and Copilot (its session store); Claude CLI keeps its home and is keyed by the scope only (as Go). The folder `<Scratch>/state` is in every agent's deny list |
| 8 | Usage events | done: `RequestState` (task-local) holds the admission context (`bound-user`, parent id of a nested call); `Router.dispatched` sends `event-begin` where the work really starts (provider request, agent process, Foundry, Apple FM, Siri), `endEvent` sends `event-finish` (outcome, tokens, token source `reported`/`estimated`); a local refusal never dispatches. `UsageSender` queues off the request path (4096, oldest dropped, `event-lost` when the helper is back); the helper now starts the usage recorder (bounded spool in the core folder, replay by event id) when its store is open |
| 9 | Quota | not part of the core |
| 10 | Opening the store | done in the helper (`core.Open`, folder 0700 made by the app); `ensure-owner` at the first ready |
| 11 | Agents out of the data folder | done: Codex always in its permission profile with the folder denied, the folder in the hidden list of Claude CLI, `minigw run` and Copilot, Grok runs only in its kernel-enforced `minigw-core` profile in a gateway-owned `GROK_HOME` (`Grok.prepareCoreHome`); allowed folders around the folder refused (`RouterConfig.overlapsCoreFolder`), hard-link aliases refused at open and at every run (`RouterConfig.coreLinkProblem` in `syncRun`), backups only in `<core>/backups` |

Known differences from `minigw serve`:

- While the helper is down an agent peer gets the 503 (the helper is what would answer 403 first); the gateway has the store in-process and answers 403.
- A request with a project header while the helper is down is refused with the 503 (never run without the project); models without a project keep working.
- Usage events of requests made while the helper is down wait in the app's memory (at most 4096; an overflow becomes a gap) and are sent when it is back; an app that quits meanwhile loses them without a gap. In Go the recorder is in the gateway process and spools them to disk.
- `approved` of a run is read from the settings the Router was built with; a settings change restarts the Router, and a removed device ends its runs through the epoch bump (`device-revoked`, before the admin call answers), not through this flag.

## What resisted the split

- Tests that pause a request after admission (the revocation barrier) use `Options.AfterAdmission`, a Go hook; the helper has none.
- The member envelope and the Anthropic-shaped refusal of an agent request differ; `admit` therefore returns the refusal as data.

## Known limits

- The Mac helper serves one request at a time (one serial queue, 35 s timeout per call), so a slow admin call (backup, restore, audit) makes the `admit` of every project run wait behind it; runs fail with 503 `core_unavailable` only after that timeout.
