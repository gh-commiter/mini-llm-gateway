---
name: ask
description: Ask any Mini Gateway model through the minigw client (Codex, Siri AI, Apple Foundation Models, Claude CLI agents, LiteLLM or OpenRouter models) and use its answer. Use when the user wants a specific gateway model, a second model's view, or work done by a gateway agent. Arguments - the model ID, then the question or task.
argument-hint: <model> <question or task>
allowed-tools: Bash(minigw *)
---

Run the question through Mini Gateway with the `minigw` client, in the current folder:

```sh
minigw -p --model <model> "$(cat <<'PROMPT'
<question or task, with the context the model needs>
PROMPT
)"
```

- If the user did not name a model, run `minigw models` and pick a fitting one, or ask.
- Agent models (`claude-codex*`, `claude-cli-*`) work in the current folder on the router Mac (minigw shares it): they read, edit and run commands there. Give them a self-contained task.
- Siri AI and Apple Foundation Models have no tools. Name the files they need in the question; the router adds them.
- Codex and Claude CLI can take minutes. Use a Bash timeout of 600000 ms, or run it in the background for longer work.
- Show the user the model's answer (summarize long output), then continue with the task.
- If `minigw` is missing or not connected, say so: install with `brew install --cask gh-commiter/mini-gateway/minigw`, then `minigw connect <router-mac> --token '<token>'`.
