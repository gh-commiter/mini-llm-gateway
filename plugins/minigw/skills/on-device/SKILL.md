---
name: on-device
description: Short private answers or summaries with Apple Foundation Models on the gateway Mac through Mini Gateway (on-device, about 8K context). Use for short texts that should not go to a cloud model.
argument-hint: <short question or text>
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" -p --model claude-apple-fm "$(cat <<'PROMPT'
<short question or text, under about 20 000 characters>
PROMPT
)"
```
