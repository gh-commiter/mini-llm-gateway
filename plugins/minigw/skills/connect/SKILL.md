---
name: connect
description: Connect this computer to Mini Gateway (one time). Use when minigw reports it is not connected, or the user gives a gateway address and token.
argument-hint: <gateway[:port]> <token from Mini Gateway → Copy Token>
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Run, with the address and the full token string (it contains `#`, so keep the single quotes):

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" connect <gateway[:port]> --token '<token>'
```

Then run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" status` and report the result. minigw stores the token in the system credential store: the macOS Keychain, Windows Credential Manager, or on Linux `~/.config/minigw/connection.json` (mode 0600). Do not repeat the token back to the user.
