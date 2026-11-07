---
name: ask
description: Ask any Mini Gateway model through the minigw client (Codex, Siri AI, Apple Foundation Models, Claude CLI agents, LiteLLM or OpenRouter models) and use its answer. Use when the user wants a specific gateway model, a second model's view, or work done by a gateway agent. Arguments - the model ID, then the question or task.
argument-hint: "[--ro] <model> <question or task>"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Run the question through Mini Gateway with the `minigw` client, in the current folder:

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" -p --model <model> "$(cat <<'PROMPT'
<question or task, with the context the model needs>
PROMPT
)"
```

- `--ro` in the arguments (or a task that only reads or answers): put `--minigw-ro` right after `minigw`, so agent models get the folder read-only and cannot change files.
- If the user did not name a model, run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" models` and pick a fitting one, or ask.
- Agent models (`claude-codex*`, `claude-cli-*`) run on the gateway Mac but work in the current folder, which minigw shares with it: they read, edit and run commands in your files. Give them a self-contained task. If minigw prints that the folder "is not shared with the gateway", suggest `/minigw:share`.
- Siri AI and Apple Foundation Models have no tools. Name the files they need in the question; the gateway adds them.
- Codex and Claude CLI can take minutes. Use a Bash timeout of 600000 ms, or run it in the background for longer work.
- Show the user the model's answer (summarize long output), then continue with the task.
- The plugin installs `minigw` at session start. If it is not connected, use `/minigw:connect`; if it is missing, start a new session (it needs network access to GitHub once).
