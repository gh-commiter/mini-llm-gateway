---
name: on-device
description: Short private answers or summaries with Apple Foundation Models on the gateway Mac through Mini Gateway (on-device, about 8K context). Use for short texts that should not go to a cloud model.
argument-hint: "[--profile <p>] <short question or text>"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw -p --model *), Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw --minigw-* -p --model *), Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw --version)
---

Profiles (`--name`, `profiles`, `--minigw-profile`, `<model>@<profile>`) need minigw 0.5.18 or newer: if the first line of `"${CLAUDE_PLUGIN_DATA}/bin/minigw" --version` is older, tell the user to run `/minigw:gw update` and do not use them (an older minigw passes them to claude).

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" -p --model claude-apple-fm "$(cat <<'PROMPT'
<short question or text, under about 20 000 characters>
PROMPT
)"
```

`--profile <p>`: `--model claude-apple-fm@<p>` (a Mac gateway; Windows and Linux gateways have no Apple models).
