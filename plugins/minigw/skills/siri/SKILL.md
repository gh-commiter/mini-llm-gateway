---
name: siri
description: Ask Siri AI on the gateway Mac through Mini Gateway. Use for questions about the gateway Mac user's personal context (calendar, notes, documents that Apple Intelligence can use), or a short question about files that are named in it. Siri must be allowed for this Mac in Mini Gateway.
argument-hint: <question>
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" -p --model claude-siri-ai "<question; name any project files it needs, the gateway attaches them>"
```

Show Siri's answer. If it fails with "Siri AI is not allowed for device", tell the user to allow this Mac in Mini Gateway → Settings → Siri AI → Devices.
