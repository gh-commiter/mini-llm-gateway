---
name: status
description: Show the Mini Gateway connection and the models available through it. Use when the user asks whether the gateway works or which gateway models exist.
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Profiles (`--name`, `profiles`, `--minigw-profile`, `<model>@<profile>`) need minigw 0.5.18 or newer: if the first line of `"${CLAUDE_PLUGIN_DATA}/bin/minigw" --version` is older, tell the user to run `/minigw:gw update` and do not use them (an older minigw passes them to claude).

Run these commands and summarize the result for the user in a few lines (gateway reachable or not, encrypted or not, which providers work, how many models). With several profiles, `status` shows a block per profile and `models` lists the default profile's models without a suffix and every profile's as `<model>@<profile>`; name each profile and say which one is not reachable:

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" status
"${CLAUDE_PLUGIN_DATA}/bin/minigw" models
```

If it is not connected, suggest `/minigw:connect <gateway> <token>`. If the file is missing, the plugin could not install it at session start (it needs network access to GitHub once); a new session retries.

One gateway only: add `--minigw-profile <p>` after `minigw`.
