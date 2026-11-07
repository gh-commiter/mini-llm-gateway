---
name: connect
description: Connect this computer to Mini Gateway (one time). Use when minigw reports it is not connected, or the user gives a gateway address and token.
argument-hint: <gateway[:port]> <token from Mini Gateway → Copy Token>
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Run, with the address and the full token string on the line between the markers (`--token -` reads it from stdin, so it
never appears in the process list):

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" connect <gateway[:port]> --token - <<'MINIGW_TOKEN'
<token>
MINIGW_TOKEN
```

Then run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" status` and report the result. minigw stores the token in the system credential store: the macOS Keychain, Windows Credential Manager, or on Linux `~/.config/minigw/connection.json` (mode 0600). Do not repeat the token back to the user.

If minigw says the gateway **refused this device** (403), the token is right but this device is not allowed yet. Run
`"${CLAUDE_PLUGIN_DATA}/bin/minigw" status` and tell the user the device ID it prints. They approve the device on the
gateway: on the Mac in Settings… → Devices or in the menu bar (Waiting for approval), on Windows in the tray or the
settings page, or with `/minigw:gw devices allow <id>` there. Ask them to compare the ID before they allow it, then run
the connect line above again.
