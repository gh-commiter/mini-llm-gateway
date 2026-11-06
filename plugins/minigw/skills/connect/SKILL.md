---
name: connect
description: Connect this Mac to a Mini Gateway router (one time). Use when minigw reports it is not connected, or the user gives a router address and token.
argument-hint: <router-mac[:port]> <token from Mini Gateway → Copy Token>
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Run, with the address and the full token string (it contains `#`, so keep the single quotes):

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" connect <router-mac[:port]> --token '<token>'
```

Then run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" status` and report the result. The token is stored in the macOS Keychain, not in any file. Do not repeat the token back to the user.
