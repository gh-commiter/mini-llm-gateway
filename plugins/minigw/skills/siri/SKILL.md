---
name: siri
description: Ask Siri AI on the gateway Mac through Mini Gateway. Use for questions about the gateway Mac user's personal context (calendar, notes, documents that Apple Intelligence can use), or a short question about files that are named in it. Siri must be allowed for this Mac in Mini Gateway.
argument-hint: "[--profile <p>] <question>"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Profiles (`--name`, `profiles`, `--minigw-profile`, `<model>@<profile>`) need minigw 0.5.18 or newer: if the first line of `"${CLAUDE_PLUGIN_DATA}/bin/minigw" --version` is older, tell the user to run `/minigw:gw update` and do not use them (an older minigw passes them to claude).

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" -p --model claude-siri-ai "<question; name any project files it needs, the gateway attaches them>"
```

Show Siri's answer. If it fails with "Siri AI is not allowed for device", tell the user to allow this Mac in Mini Gateway → Settings → Siri AI → Devices.

`--profile <p>`: `--model claude-siri-ai@<p>` (a Mac gateway only).
