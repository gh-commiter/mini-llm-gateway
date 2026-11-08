# Release notes

## Unreleased

- Docs: sharing and sync send the whole folder, hidden files included; keep credentials out or list them in `.gitignore`.
- Windows: the tray started by the installer now gets the user's full environment (as from Explorer), so CLIs whose PATH entry uses `%LOCALAPPDATA%` (Codex) are found without restarting the tray.
- Codex, Grok and Copilot agents are told the full path of `minigw` for asking other models (Codex's Windows sandbox runs commands without the gateway's PATH, so `minigw` was not found).
- Windows tray: Settings opens in its own app window (Edge, Chrome, Brave or Vivaldi; otherwise the default browser), and clicking "Settings…" again brings it to the front instead of opening a new tab. A window whose session expired is replaced by a signed-in one.
- Windows and Linux gateway (`minigw serve`): `minigw` without `--minigw-work` (and no `MINIGW_WORK` or saved mode) no
  longer fails with 400 "this gateway supports --minigw-work=sync and worktree": when the gateway refuses direct, the
  client picks worktree (the folder is the root of a git repository with a commit) or sync, as `auto` does. An explicit `direct` is still an error.
  Both clients (plugin 0.5.16).
- Settings page, Claude CLI pane: an allowed folder that the gateway left out at start (it contains or is inside the
  scratch folder) is listed, marked, and can be removed; after saving the folders the switch turns Claude CLI on without
  restarting the gateway. Turning it on before that gives a clear error and the switch goes back off. The reason names
  each folder once.
- Windows MSI: reinstalling the same version no longer shows "Mini Gateway is already running" (the installer starts the
  tray with `--quiet-if-running`; a tray started by hand still shows the message).
- Windows tray and the settings page: **Copy Token** copies only the token (`token#key`), as on the Mac; the tray item was
  "Copy connect line". `gw token` still prints the whole `minigw connect …` line.
- **No console windows on Windows:** the tray and every program it starts (claude, git, agents …) run without a console
  window; GUI windows such as the installer's progress stay visible. The MSI closes the tray with
  `minigw-tray.exe quit` instead of `taskkill` in a cmd window. Updating from 0.5.18 or older still shows one cmd window
  once (the old package's uninstall step). `minigw serve install` on Windows now fails if `minigw-tray.exe` is not next
  to `minigw.exe` and cannot be downloaded, and Start at sign-in (`gw autostart on`, the tray menu, Settings) needs
  `minigw-tray.exe`: there is no fallback to `minigw.exe` at sign-in, which opened a console window. `minigw tray quit`
  stops only `minigw-tray.exe` of this user's Windows session.
- **Windows test report fixes (gateway):** the tray started by the MSI now finds `claude`, `codex`, `git` … (PATH also
  from the registry and `%USERPROFILE%\.local\bin`). Agent calls on Windows use `minigw.exe` (not the tray) as
  `minigw -p …`, pre-approved for Bash and PowerShell (`Bash(minigw -p:*)`, `PowerShell(minigw -p *)`); other Bash commands of an agent still need approval there (no
  sandbox). An invalid `serve.json` is written to the log and shown in a message box in front; `gw status` says the
  settings must be fixed. When an allowed folder contains the scratch folder, the reason why Claude CLI and Codex are off
  is in the log, `gw status`, `gw claude` and the Claude CLI pane; the tray log now also has the startup warnings.
- `minigw gw devices deny <id>` refuses options such as `--name` (it used to remove the device). **Allow any device with
  the token** can be turned on only while no device is allowed (page, Mac Settings, admin API 400).
- `gw status` on Windows says `minigw gw autostart off`; `gw autostart` without an argument shows the state (both CLIs; plugin 0.5.15).
  Scratch folder errors say that it must not contain the config folder (named).
- Sessions: a device on 127.0.0.1 that was never approved shows as this computer, not "(device removed)"; an allowed
  device has connection history also over 127.0.0.1; a request with an invalid session ID counts in a `t-…` session;
  streamed Claude CLI answers count input tokens. `minigw status` lists the providers of a `minigw serve` gateway; `/health/ready` names the providers only to this computer or a caller with the token (Mac too). No
  empty `.maven` folder stays after the last worktree clone. The Claude CLI pane shows changes from `gw claude`.
- `minigw` started inside Claude Code or Claude Desktop no longer gets 401 from the gateway: the inner `claude` no
  longer inherits the parent's `ANTHROPIC_*`, `CLAUDECODE` and `CLAUDE_CODE_*` variables (session, OAuth, base URL;
  `CLAUDE_CODE_GIT_BASH_PATH` stays). Both clients.
- `minigw --model <model>@<profile>` uses that profile for the whole run (folder share, sync, worktree, native), as
  `--minigw-profile <profile>` does; before, the folder went to the default gateway. Both clients.
- Windows and Linux: a gateway on the same computer (`minigw serve`, e.g. profile `local` on 127.0.0.1) gets the project
  folder shared automatically, as a remote one does (before only with `MINIGW_SHARE=1`). On a Mac the app on the same
  Mac still works in the folder itself.
- `--minigw-work=auto` or `worktree` in a git repository without a commit (also a folder inside one) uses sync at once,
  with one line why, instead of failing on every agent request with "cannot take a snapshot". Both clients.
- `minigw --version` prints only minigw's version (Claude Code's: `claude --version`); `minigw gw status` shows it on one
  line again.
- Windows MSI: the user PATH gets Mini Gateway once (without a trailing `\`, the form `minigw serve install` uses), and
  reinstalling the same version replaces the earlier install instead of adding a second one.
- **Codex on a Mac gateway shared by several devices:** Codex's own sandbox can read every file of the gateway's user,
  so Codex agents are not isolated between devices for reads (other devices' synced copies and worktree clones in
  Scratch, and an agent run's token file there). Allow Codex only for devices you trust with that.
- **Native mode on a git repository works on branches (new default):** each agent run gets its own clone on the gateway
  and a branch `minigw/<agent>-<session>-<n>` from your current state (also uncommitted changes); the branch then appears
  in your repository (fast-forward only, no hooks, fsck), while your files, index and checked-out branch stay as they
  are. So parallel agents never edit the same files. **Changed behaviour:** to let agents change the folder itself as
  before, use `--minigw-work=native-direct` (or `minigw share add --work=native-direct`). In folders that are not a git
  repository, with `native-direct` and in direct mode, only one writing agent at a time works in a folder (also a folder
  inside it); the others wait up to 5 minutes, and `minigw run` takes the same lock for its build. Grok and Codex or
  Copilot without writing are refused on native git repositories ("use --minigw-work=native-direct"). Older minigw
  clients also get branches on a native git repository; update minigw to use `native-direct`. If a branch cannot be
  delivered (for example you committed to it, or it is checked out), minigw shows why. The delivery runs git with the
  gateway's settings only, in a sandbox, so nothing from the repository's own config runs.
- **Device history and sessions (Mac and `minigw serve`):** last seen and the client's OS and version now survive
  restarts, and each allowed device keeps its last 20 connections (Devices → History, `minigw gw devices history <id>`,
  `GET /devices/{id}/history`). A new **Sessions** pane lists the last 200 client sessions with device, folder, work mode
  and per-model requests, errors, refusals, time and tokens (`minigw gw sessions`, `GET /sessions`); **Clear History**
  clears them. It stores folder paths, branch names and client addresses, never text; the history is a 0600 file on
  the gateway's computer.
- `minigw --minigw-role NAME` (or `MINIGW_ROLE`) sends the caller's role to the gateway, which records it in the
  session history (both clients), with counts per model and role and the agent's restrictions (read-only, tools). It is a guard against mistakes, not a security boundary.
  A model asked by an agent (agent calls) counts in the caller's session, with the calling chain (`via`) and the
  caller's role; the asked agent cannot change either. Plugin 0.5.13.
- `minigw serve` no longer has a default folder for agents (`folder` in serve.json, the settings field and
  `minigw gw claude --folder`): as on the Mac, an agent request without a shared, synced or worktree folder is refused.
  A stored `folder` becomes an allowed folder; `minigw serve --folder X` (and `serve init`) still works, adds X to the
  allowed folders and says it is deprecated.
- **An agent may ask another model (off by default):** with **May ask other models** on for Claude CLI, Codex, Grok or
  Copilot (Settings, the settings page, `PATCH /agents/{name} {"askOthers": true}`, `minigw gw claude --ask-others=true`),
  an agent on the gateway can ask another model of that gateway itself, for example a remote `claude-cli-opus` asking
  an OpenRouter model or `claude-codex-…`: its instructions name the gateway's `minigw -p --model <id> "…"`, which sends
  the question straight to a loopback port and token for that run only (`MINIGW_AGENT_URL`, the token in a file of that run: `MINIGW_AGENT_AUTH_FILE`), with
  the rights of your device, also from inside Claude Code's sandbox (through its proxy). The asked agent works in the
  same folder (sync copy, worktree branch or native folder) and the caller waits. One level deep, never the same kind of
  agent, at most 2 questions at a time per agent; the run token works only for that agent's own processes. Claude CLI
  needs **Allow local tools** (Bash) for it, Codex "Allow network access for commands". Calls to another gateway come
  later.
- **This user's apps without a token (off by default):** programs of the gateway's own user on the same computer can use
  the models and agents (`/v1/messages`, `/v1/models`) without the token, for example an app pointed at `http://127.0.0.1:8765`.
  The gateway checks the process behind the connection (same user, not one of its agents); the request needs
  `anthropic-version` or `x-api-key`, a loopback `Host` and no `Origin`, so web pages cannot use it. Other users, other
  computers, the admin API and device approval are unchanged. Switch: General → **Allow this user's apps without a
  token** (`allowLocalApps`). On `minigw serve` for Windows and Linux; on the Mac in the app. Sandboxed apps (Mac App
  Sandbox and Seatbelt, also commands in Claude Code's sandboxed Bash; Linux Flatpak and snap; Windows AppContainer)
  and apps in a web view still need the token. Docker and other containers, VMs and tunnels (`ssh -R`, ngrok…) running as you also count as this user's apps: with the switch on they can use agents in your allowed folders without a token.
- Updates never go back to an older signed release: SHA256SUMS carries a signed `VERSION` line, and the plugin's
  minigw update, `install.sh` and `minigw serve install` refuse a release older than the installed minigw
  (`--allow-downgrade`, or `MINIGW_ALLOW_DOWNGRADE=1` for the plugin, to go back on purpose).
- Allowed folders that contain Copilot's package cache are refused like the Scratch folder, and the package is
  unpacked (`copilot --version`) in a sandbox without network (Mac `sandbox-exec`, Linux bwrap).
- Foundry Local is used only when the process on its port runs from the Foundry installation.
- **Several gateways at once (profiles, #17):** `minigw connect <host> --name <profile>` keeps every gateway, for
  example the Mac mini and a Windows gateway side by side. Models of other profiles are `<model>@<profile>`; the
  default profile's also appear without a suffix. An unreachable gateway is left out of the list, and `minigw status`
  shows each profile. The existing connection becomes the profile `default` with nothing to do.
- **Worktree mode on `minigw serve` (Windows, Linux):** `--minigw-work=worktree` (and `auto` for a git repository) now
  also works with a Windows or Linux gateway, as on the Mac: the agent works in a clone on its own branch, and the branch
  comes back to your repository (fetched fast-forward only, no hooks or filters). Native mode and `minigw run` stay
  Mac-only.
  **Changed behaviour:** `--minigw-work=auto` on a git repository with a Windows or Linux
  gateway now gives you a branch `minigw/…` instead of changing your files through a synced copy. To keep the old
  behaviour, run `minigw share add --work=sync` in that folder (or `MINIGW_WORK=sync`).
- **Windows installer:** `minigw-windows-amd64.msi` and `minigw-windows-arm64.msi` install the gateway with the tray for
  this user, without admin rights (the same as `minigw serve install`: PATH, Start menu, start at sign-in). Listed in
  the signed `SHA256SUMS`; the MSI itself is not code-signed yet, so SmartScreen warns. On an MSI install,
  `minigw serve install` updates with the release's MSI (nothing happens when it is already up to date or newer), and
  `minigw serve uninstall` points to Settings → Apps.
- **File and folder pickers on Windows:** with the tray, the settings page has **Choose…** next to Allowed folders and
  **Browse…** next to a program's path, which open the Windows file dialog. On Linux and without the tray you type the
  path.
- **Devices are the same on the Mac and on `minigw serve`:** a new device waits for approval (Settings → Devices, the menu
  bar or the tray, `minigw gw devices allow <id> [--name N]`) unless the list is empty and "Allow any device with the
  token" is on. Waiting devices show `<user>@<computer>`, the client's OS and minigw version; you can edit the name
  before you allow a device and rename it later (`rename`). Both panes show the allowed IP addresses, unencrypted
  connections, and waiting and allowed devices with their ID and when they were last seen; the Mac notifies you when a
  device starts waiting. Devices are identified by their ID only: `deny` and the admin API no longer take a name.
  Devices you already use stay allowed. Clients without a device key (minigw before 0.4.4, unencrypted clients from
  other computers) are accepted only while no device is allowed and "Allow any device with the token" is on: update
  minigw on such clients.
- **Providers:** the Mac has "Allow http (localhost, a private IP or a .local name only)" like `minigw serve`, with the
  same rule on both. Existing Mac providers at `http://localhost`, a private or loopback IP, or a `.local` name keep
  working (the switch is turned on for them). Any other `http` provider, for example at a public host or a name without
  a dot, no longer gets the API key: change its URL to `https`. LiteLLM and OpenRouter are always listed on
  `minigw serve` too (off until a key is set; they can be turned off, not removed or renamed), and on the Mac they can
  no longer be removed through `minigw gw provider remove`.
- `minigw gw claude --folders … --allow-local-tools=true|false` and `minigw gw autostart on|off` now work on Windows and
  Linux too (the same flags as on the Mac); `--allow-unsandboxed-bash` stays a `minigw serve` extra.
- The Mac's admin API has what `minigw serve` has: `PATCH /providers/{name}` (rename, on/off, URL, aliases),
  `GET /agents/claude/tools`, `GET /agents/{codex,grok}/models`, permission mode and setting sources for Claude CLI, and
  the scratch folder (moved like in Settings).
- **Claude CLI on the Mac no longer loads a folder's `.claude/settings*.json` by default** (setting sources empty, as on
  `minigw serve`): an agent with local tools could otherwise write hooks there that the next session runs outside the
  sandbox. If you never changed "Setting sources", it changes on its own; a value you set yourself stays.
- **Scratch folder rules** (Settings, `minigw gw`, admin API, Mac and `minigw serve`): not the disk root, your home folder
  or a folder above it, a temporary folder, or a folder inside or around an allowed folder of the agents; and an allowed
  folder cannot contain the scratch folder or sit inside it. The Claude agent sandbox never reads Scratch, also on an
  external disk. On the Mac a move waits until no minigw session runs.
- Plugin 0.5.9 (needs minigw 0.5.18): `/minigw:connect --name` and profiles in every skill, `/minigw:connect` explains how
  to approve a refused device, and `/minigw:gw` covers the Copilot token, autostart and the work modes.

## 0.5.17

- GitHub Copilot CLI as an agent model, compatible with the Claude Code CLI: `claude-copilot` and `claude-copilot-<model>`
  (Mac app and `minigw serve`; off by default). Read-only by default; writing, web access and commands are separate
  switches. Copilot always runs in a sandbox (Mac `sandbox-exec`, Linux bwrap) with a fresh Copilot home per request.
  It signs in with its own fine-grained GitHub token with only the Copilot Requests permission
  (`minigw gw copilot token` or Settings; required on the Mac and for commands, which also need a confirmation). Plugin 0.5.5: `/minigw:copilot`.
- `minigw run` can no longer write `~/Library/Caches`, where apps keep code they load unchecked at start (for example the
  Copilot CLI's package). `mvn`, `swift build` and `xcodebuild` builds work without it.
- If you used Codex with writing or `minigw run` in 0.5.15, run `xcrun -k` once to clear the xcrun cache.
- Agents and `minigw run` can no longer write this Mac's shared temporary and cache folders (`/var/folders/…`), where
  the xcrun cache tells `/usr/bin/git` and the other developer tools what to run; they get their own temporary folder.
  The gateway runs git directly, never through the xcrun cache. `minigw run` builds Maven, Swift and Xcode projects as
  before; a `swift build` or `xcodebuild` by an agent in worktree mode may fail (use `minigw run` or sync mode).
- `/minigw:connect` and `install.sh` pass the token on stdin, so it never appears in the process list (plugin 0.5.4).
- One command to install the gateway, compatible with the Claude Code CLI: `/minigw:gw install` (plugin 0.5.3).
  Mac: the app with Homebrew and Open at Login. Windows: the tray at sign-in. Linux: systemd, at boot when linger
  works. `/minigw:gw update` when a session start says a newer gateway is out.
- The settings page of `minigw serve` (Windows, Linux) has the panes of the Mac's Settings, with Codex CLI and
  Grok CLI.
- Codex and Grok on `minigw serve` are off by default and run only in a synced folder. On Linux they need `bwrap`
  (home and config folders hidden), on Windows "Allow commands without a sandbox". Each device gets its own Codex
  and Grok sessions there. Devices that use Codex or Grok can read every file the gateway's user can read.
- Codex network access for commands is off by default, also on the Mac (saved settings keep their value).
- The gateway refuses admin requests from its own agent processes (Linux, Windows; the Mac app already did).
- New names: Mini Gateway is the **gateway**; OpenRouter, LiteLLM and other upstreams are **providers**
  (Settings → Providers). `minigw gw …` and `minigw gw provider …` replace `minigw router …` and `router gateway …`,
  which still work in this release; the admin API has `/providers` (`/gateways` still works).
- Foundry Local (Microsoft) as an on-device provider on Windows, Linux and the Mac: `claude-foundry-<model>`, off by
  default (Settings → Foundry Local).
- Release files are signed: `SHA256SUMS.sig` (ssh-keygen, key not on GitHub). The plugin, `install.sh` and
  `minigw serve install` refuse a release whose SHA256SUMS is not signed by Mini Gateway; a first install without
  `ssh-keygen` stops with a message.
