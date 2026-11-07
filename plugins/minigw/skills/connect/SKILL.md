---
name: connect
description: Connect this computer to Mini Gateway (one time). Use when minigw reports it is not connected, or the user gives a gateway address and token.
argument-hint: "[--name <profile>] <gateway[:port]> <token from Mini Gateway → Copy Token>"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Run, with the address and the full token string on the line between the markers (`--token -` reads it from stdin, so it
never appears in the process list):

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" connect <gateway[:port]> [--name <profile>] --token - <<'MINIGW_TOKEN'
<token>
MINIGW_TOKEN
```

- `--name <profile>` (lowercase letters, digits and `-`) adds or replaces that gateway and keeps the others, for
  example `--name mini` for the Mac mini and `--name win` for a Windows gateway. Use it whenever the user connects a
  second gateway; without it the default profile is replaced. Models of other profiles are then `<model>@<profile>`.
- `minigw profiles` lists the profiles, `minigw profiles default <name>` picks the one whose models also appear without
  a suffix, and `minigw disconnect --name <profile>` removes one.

Then run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" status` and report the result. minigw stores the token in the system credential store: the macOS Keychain, Windows Credential Manager, or on Linux `~/.config/minigw/connection.json` and `connections/<profile>.json` (mode 0600). Do not repeat the token back to the user.
