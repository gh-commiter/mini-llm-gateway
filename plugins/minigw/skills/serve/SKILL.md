---
name: serve
description: Install, update, check or remove the Mini Gateway router on this computer (Windows and Linux run minigw serve, macOS runs the Mini Gateway app), and manage it (devices, gateways, Claude CLI, token, settings page). Use for /minigw:serve install|update|uninstall|status and when the user wants to change the router on this computer.
argument-hint: install | update | uninstall | status | devices [allow <id> | deny <id>] | gateway add|remove|list|models | claude … | token [rotate] | settings
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Use the plugin's own minigw and find the system:

```sh
M="${CLAUDE_PLUGIN_DATA}/bin/minigw"; uname -s
```

`Darwin` is macOS. `Linux` is Linux. `MINGW*`, `MSYS*` or `CYGWIN*` is Windows (Git Bash runs `minigw.exe`). If `$M` is missing, the plugin could not download it at session start: a new session retries (it needs GitHub once).

Rules:
- Never put an API key in a command line. Never print the router token. `serve init` and `router token` print it in the connect line: send their output to `/dev/null`, unless the user asked for the connect line.
- Never update on your own. An update restarts the router and stops the agent requests that run on it.
- Report what happened in a few lines: running or not, version, starts at boot / at sign-in / not on its own.

## install

**Windows and Linux** (`minigw serve`, per user, no admin rights):

```sh
"$M" serve init >/dev/null   # creates the config once; keeps an existing one
"$M" serve install
```

- Windows: the router runs in the tray (notification area) and starts at sign-in.
- Linux: a systemd user unit. `serve install` also turns on start at boot (linger). If its output ends with `To start Mini Gateway at boot without logging in, run: sudo loginctl enable-linger <user>`, the router starts only at sign-in. Tell the user, and offer that they run that line themselves with `!` (sudo asks for their password). Then check with `"$M" router status`.

**macOS**: the router is the Mini Gateway app (not `minigw serve`).

1. If `/Applications/Mini Gateway.app` is missing, ask the user before installing. With Homebrew:
   ```sh
   brew tap gh-commiter/mini-gateway https://github.com/gh-commiter/mini-llm-gateway
   brew trust --tap gh-commiter/mini-gateway
   brew install --cask gh-commiter/mini-gateway/mini-gateway
   ```
   Skip the tap and trust lines if `brew tap` already lists `gh-commiter/mini-gateway`. Without Homebrew, point the user to the DMG on https://github.com/gh-commiter/mini-llm-gateway/releases/latest and stop.
2. `open -a "Mini Gateway"`. Then retry `"$M" router status` for up to 20 seconds, until it answers.
3. `"$M" router autostart on` (Open at Login).

**Then, on every system**, connect this computer's minigw to its own router. If `"$M" status` shows a connection to another router, ask first: minigw keeps one connection. The token goes on stdin from one command to the other: it is never printed and never in a command line:

```sh
l=$("$M" router token | sed -n 's/^  minigw connect //p'); p=${l%% *}; p=${p##*:}; t=${l#*--token \'}; t=${t%\'}
printf '%s' "$t" | "$M" connect "127.0.0.1:$p" --token - && "$M" router status
```

For other computers, `/minigw:serve token` shows the connect line to run there (or `/minigw:connect` on them).

## update

1. Run `"$M" router status`. If `agent requests running` is more than 0, tell the user that the update stops them, and ask whether to go on or wait.
2. Windows and Linux: `"$M" serve install`. The plugin's minigw is the latest release: it replaces the installed copy and restarts the router (Windows also the tray).
3. macOS: if `brew list --cask gh-commiter/mini-gateway/mini-gateway` succeeds, run `brew upgrade --cask gh-commiter/mini-gateway/mini-gateway`. Otherwise point to the DMG. The running app keeps the old version until it restarts. Ask the user, then run `osascript -e 'quit app "Mini Gateway"'` and `open -a "Mini Gateway"`.
4. `"$M" router status` and report the new version.

## uninstall

- Windows and Linux: `"$M" serve uninstall`. It stops the router and removes autostart. The config (with devices and gateways) stays. On Linux, linger stays on. `loginctl disable-linger` turns it off if the user wants.
- macOS: `"$M" router autostart off`. Remove the app only if the user asks: `brew uninstall --cask gh-commiter/mini-gateway/mini-gateway`, or move it to the Trash.

## status

`"$M" router status`. Summarize: running, version, autostart (at boot / at sign-in / no), running agent requests, devices (allowed, waiting), gateways (models, ready). If it is not installed, suggest `/minigw:serve install`.

## Managing the router

Everything else goes to `"$M" router …`. It changes the running router at once (on Windows and Linux also when it is stopped: then serve.json).

- `devices`: lists allowed and waiting devices.
- `devices allow <id> [name]`: first run `devices`. Show the user the waiting device's name and full ID. Ask them to compare it with `minigw status` on that computer. Run `allow` only after they confirm. Anyone with the token can try to connect.
- `devices deny <id or name>`.
- `gateway add <name> <baseURL> [--insecure]` needs an API key. Never pass it yourself. Print the full path (`echo "$M"`). Ask the user to run, in this session, `! "<that path>" router gateway add <name> <baseURL>`. minigw then asks for the key without showing it. `--insecure` allows `http` (localhost or a private IP only).
- `gateway list`, `gateway remove <name>`, `gateway models <name> [text] [--all] [--enable m1,m2|all] [--disable m1,m2|all]`.
- `claude`: on Windows and Linux `[--path FILE] [--tools T1,T2] [--folder DIR] [--allow-unsandboxed-bash=true|false]`. On the Mac `[--path FILE] [--tools T1,T2] [--folders DIR1,DIR2] [--allow-local-tools=true|false]`. Windows has no sandbox for commands: before `--allow-unsandboxed-bash=true`, tell the user that agents can then run any command as them.
- `token`: prints the connect line for other computers (the user asked for it, so show it). `token rotate`: ask first. Every client must connect again with the new line.
- `settings` (Windows and Linux): prints a one-time link to the settings page, valid for 60 seconds. Tell the user to open it now. On the Mac, the settings are the app's Settings window (menu bar icon → Settings…).
