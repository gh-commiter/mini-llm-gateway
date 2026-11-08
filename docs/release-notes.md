# Release notes

## Unreleased

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
  Linux too (the same flags as on the Mac); `--folder` and `--allow-unsandboxed-bash` stay as `minigw serve` extras.
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
