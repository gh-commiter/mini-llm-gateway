---
name: connect
description: Connect this computer to Mini Gateway (one time). Use when minigw reports it is not connected, or the user gives a gateway address and token.
argument-hint: "[--name <profile>] <gateway[:port]> <token from Mini Gateway → Copy Token>"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Profiles (`--name`, `profiles`, `--minigw-profile`, `<model>@<profile>`) need minigw 0.5.18 or newer: if the first line of `"${CLAUDE_PLUGIN_DATA}/bin/minigw" --version` is older, tell the user to run `/minigw:gw update` and do not use them (an older minigw passes them to claude).

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
- List: `"${CLAUDE_PLUGIN_DATA}/bin/minigw" profiles` (`*` = default).
- Default (its models also without a suffix): `"${CLAUDE_PLUGIN_DATA}/bin/minigw" profiles default <profile>`.
- Remove one: `"${CLAUDE_PLUGIN_DATA}/bin/minigw" disconnect --name <profile>`; without `--name` it removes the default.

Then run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" status` and report the result: it exits 0 only when the gateway is reachable, the token is accepted and this device is approved; its `Status:` line gives the reason otherwise (`token rejected` → ask the user to copy the token again and connect again). minigw stores the token in the system credential store: the macOS Keychain, Windows Credential Manager, or on Linux `~/.config/minigw/connection.json` and `connections/<profile>.json` (mode 0600). Do not repeat the token back to the user.

If minigw says the gateway **refused this device** (403), the token is right but this device is not allowed yet. Run
`"${CLAUDE_PLUGIN_DATA}/bin/minigw" status` and tell the user the device ID on its `Device:` line (it says `waiting for approval` until the gateway allows it; `Status: device not approved`). They approve the device on the
gateway: on the Mac in Settings… → Devices or in the menu bar (Waiting for approval), on Windows in the tray or the
settings page, or with `/minigw:gw devices allow <id>` there. Ask them to compare the ID before they allow it, then run
the connect line above again.
