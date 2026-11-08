---
name: gw
description: Install, update, check or remove Mini Gateway on this computer (Windows and Linux run minigw serve, macOS runs the Mini Gateway app), and manage it (devices, providers, Claude CLI, token, settings page). Use for /minigw:gw install|update|uninstall|status and when the user wants to change the gateway on this computer.
argument-hint: install | update | uninstall | status | devices [allow <id> [--name N] | rename <id> --name N | deny <id>] | provider add|remove|list|models | claude … | copilot token [remove] | autostart on|off | token [rotate] | settings | modes
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Use the plugin's own minigw and find the system:

```sh
M="${CLAUDE_PLUGIN_DATA}/bin/minigw"; uname -s
```

`Darwin` is macOS. `Linux` is Linux. `MINGW*`, `MSYS*` or `CYGWIN*` is Windows (Git Bash runs `minigw.exe`). If `$M` is missing, the plugin could not download it at session start: a new session retries (it needs GitHub once).

Profiles: `"$M" status` shows each gateway this client uses; the one at `127.0.0.1` is the gateway on this computer. `install`, `update`, `uninstall` and `gw …` change only that one; for another profile run `/minigw:gw` on its computer.

Rules:
- Profiles (`--name`, `profiles`, `--minigw-profile`, `<model>@<profile>`) need minigw 0.5.18 or newer: if the first line of `"$M" --version` is older, tell the user to run `/minigw:gw update` and do not use them (an older minigw passes them to claude).
- Never put an API key in a command line. Never print the gateway token. `serve init` and `gw token` print it in the connect line: send their output to `/dev/null`, unless the user asked for the connect line.
- Never update on your own. An update restarts the gateway and stops the agent requests that run on it.
- Report what happened in a few lines: running or not, version, starts at boot / at sign-in / not on its own.

## install

**Windows and Linux** (`minigw serve`, per user, no admin rights):

```sh
"$M" serve init >/dev/null   # creates the config once; keeps an existing one
"$M" serve install
```

- Windows: the gateway runs in the tray (notification area) and starts at sign-in.
- Linux: a systemd user unit. `serve install` also turns on start at boot (linger). If its output ends with `To start Mini Gateway at boot without logging in, run: sudo loginctl enable-linger <user>`, the gateway starts only at sign-in. Tell the user, and offer that they run that line themselves with `!` (sudo asks for their password). Then check with `"$M" gw status`.

**macOS**: the gateway is the Mini Gateway app (not `minigw serve`).

1. If `/Applications/Mini Gateway.app` is missing, ask the user before installing. With Homebrew:
   ```sh
   brew tap gh-commiter/mini-gateway https://github.com/gh-commiter/mini-llm-gateway
   brew trust --tap gh-commiter/mini-gateway
   brew install --cask gh-commiter/mini-gateway/mini-gateway
   ```
   Skip the tap and trust lines if `brew tap` already lists `gh-commiter/mini-gateway`. Without Homebrew, point the user to the DMG on https://github.com/gh-commiter/mini-llm-gateway/releases/latest and stop.
2. `open -a "Mini Gateway"`. Then retry `"$M" gw status` for up to 20 seconds, until it answers.
3. `"$M" gw autostart on` (Open at Login).

**Then, on every system**, connect this computer's minigw to its own gateway as profile `local`; other profiles stay. Skip it if `"$M" profiles` already lists `127.0.0.1`. The token goes on stdin from one command to the other: it is never printed and never in a command line:

```sh
l=$("$M" gw token | sed -n 's/^  minigw connect //p'); p=${l%% *}; p=${p##*:}; t=${l#*--token \'}; t=${t%\'}
printf '%s' "$t" | "$M" connect "127.0.0.1:$p" --name local --token - && "$M" gw status
```

For other computers, `/minigw:gw token` shows the connect line to run there (or `/minigw:connect` on them).

## update

1. Run `"$M" gw status`. If `agent requests running` is more than 0, tell the user that the update stops them, and ask whether to go on or wait.
2. Windows and Linux: `"$M" serve install`. The plugin's minigw is the latest release: it replaces the installed copy and restarts the gateway (Windows also the tray). If `gw status` says "installed with the MSI", the same command downloads the release's MSI (checked against the signed `SHA256SUMS`) and runs it.
3. macOS: if `brew list --cask gh-commiter/mini-gateway/mini-gateway` succeeds, run `brew upgrade --cask gh-commiter/mini-gateway/mini-gateway`. Otherwise point to the DMG. The running app keeps the old version until it restarts. Ask the user, then run `osascript -e 'quit app "Mini Gateway"'` and `open -a "Mini Gateway"`.
4. `"$M" gw status` and report the new version.

## uninstall

- Windows installed with the MSI (`gw status` says so): `serve uninstall` refuses. Tell the user: Settings → Apps → Installed apps → Mini Gateway → Uninstall. The config stays.
- Windows and Linux: `"$M" serve uninstall`. It stops the gateway and removes autostart. The config (with devices and providers) stays. On Linux, linger stays on. `loginctl disable-linger` turns it off if the user wants.
- macOS: `"$M" gw autostart off`. Remove the app only if the user asks: `brew uninstall --cask gh-commiter/mini-gateway/mini-gateway`, or move it to the Trash.

## status

`"$M" gw status`. Summarize: running, version, autostart (at boot / at sign-in / no), running agent requests, devices (allowed, waiting), providers (models, ready). Then `"$M" status`: list each profile (reachable or not) and say which one is this computer (`127.0.0.1`). If it is not installed, suggest `/minigw:gw install`.

## Managing the gateway

Everything else goes to `"$M" gw …`. It changes the running gateway at once (on Windows and Linux also when it is stopped: then serve.json).

- `devices`: lists allowed and waiting devices.
- `devices allow <id> [--name N]`: first run `devices`. Show the user the waiting device's name, full ID, OS and minigw version. Ask them to compare the ID with `minigw status` on that computer. Run `allow` only after they confirm. Anyone with the token can try to connect, and a name proves nothing (a waiting device may copy an allowed device's name).
- `devices rename <id> --name N`: the name is only a label.
- `devices deny <id>`: removes an allowed device or rejects a waiting one (by ID only). Reject is not a block list: to lock someone out, also rotate the token.
- `provider add <name> <baseURL> [--insecure]` needs an API key. Never pass it yourself. Print the full path (`echo "$M"`). Ask the user to run, in this session, `! "<that path>" gw provider add <name> <baseURL>`. minigw then asks for the key without showing it. `--insecure` allows `http` (localhost or a private IP only).
- `provider list`, `provider remove <name>`, `provider models <name> [text] [--all] [--enable m1,m2|all] [--disable m1,m2|all]`.
- `claude [--path FILE] [--tools T1,T2] [--folders DIR1,DIR2] [--allow-local-tools=true|false] [--ask-others=true|false]` on every system (`--ask-others`: Claude may ask other models of this gateway; needs local tools); Windows and Linux also `[--allow-unsandboxed-bash=true|false]`. Windows has no sandbox for commands: before `--allow-unsandboxed-bash=true`, tell the user that agents can then run any command as them.
- `copilot token [remove]`: Copilot's own GitHub token (fine-grained, only "Copilot Requests"). Never pass it yourself: ask the user to run `! "<path of $M>" gw copilot token`; minigw asks for it without showing it.
- `autostart on|off`: Mac Open at Login; Windows the tray at sign-in. Linux: use `serve install` / `serve uninstall`.
- Model requests from this user's own apps on the gateway's computer need no token while General → "Allow this user's apps without a token" is on (off by default: containers and tunnels running as the user would also count); it is a settings switch (`allowLocalApps`), not a `gw` command. minigw itself still sends the token from its local profile.
- `token`: prints the connect line for other computers (the user asked for it, so show it). `token rotate`: ask first. Every client must connect again with the new line.
- `settings` (Windows and Linux): prints a one-time link to the settings page, valid for 60 seconds. Tell the user to open it now. On the Mac, the settings are the app's Settings window (menu bar icon → Settings…).

## modes

How agents get the client's folder (`minigw --minigw-work=<mode> …`, or `MINIGW_WORK`; per folder `minigw share add --work=<mode>`):
- `direct`: the gateway Mac mounts the folder (default; Mac gateway only).
- `sync`: a copy on the gateway, changes come back after each request (needed for Windows and Linux gateways).
- `worktree`: a git repository; the agent works in a clone, its work comes back as branch `minigw/…` (Mac and minigw serve; Grok not).
- `native`: the folder is the gateway Mac's own SMB share; agents work in it directly. `minigw run -- <command>` runs a build there (allowed per device in the gateway's Settings; Mac only).
- `auto`: native for a gateway share, worktree for a git repository, otherwise sync.
