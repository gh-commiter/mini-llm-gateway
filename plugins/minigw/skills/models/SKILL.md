---
name: models
description: List the models available through Mini Gateway (Codex, Grok, Claude CLI agents, Siri AI, Apple Foundation Models, LiteLLM and OpenRouter models). Use when the user asks which gateway models exist or which one to use.
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Profiles (`--name`, `profiles`, `--minigw-profile`, `<model>@<profile>`) need minigw 0.5.18 or newer: if the first line of `"${CLAUDE_PLUGIN_DATA}/bin/minigw" --version` is older, tell the user to run `/minigw:gw update` and do not use them (an older minigw passes them to claude).

Run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" models` and show the result grouped by provider (agents with tools: `claude-codex*`, `claude-grok*`, `claude-cli-*`; without tools: Siri AI, Apple FM, LiteLLM, OpenRouter, `claude-chat-*`). Keep the model IDs exactly as printed: they are what `/minigw:ask <model>` takes. With several gateways (profiles), `<model>@<profile>` runs on that gateway and the default profile's models also appear without a suffix; group by profile too. minigw prints a note for a gateway that is not reachable: mention it. If minigw is not connected, suggest `/minigw:connect`.

One gateway only: `"${CLAUDE_PLUGIN_DATA}/bin/minigw" --minigw-profile <p> models`.
