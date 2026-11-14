---
name: usage
description: Show the user's Mini Gateway model usage (requests and tokens per day and model, for this folder's project or for all their projects), with the gaps where events were lost (minigw 0.6.0 or newer, gateway 0.6.0 or newer). Use when the user asks how much they or their project used the gateway models.
argument-hint: "[--project <slug>] [--days <n>]"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw usage), Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw usage --project *), Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw usage --days *), Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw --version)
---

Usage events need minigw 0.6.0 or newer, and a gateway of 0.6.0 or newer: if the first line of `"${CLAUDE_PLUGIN_DATA}/bin/minigw" --version` is older, tell the user to run `/minigw:gw update` and do not use this command.

Run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" usage [--project <slug>] [--days <n>]` (the last 7 days by default). Without `--project` it shows the linked project of this folder, or, with no link, the user's own events in all their projects. It prints one line per day, model and user (events, tokens in and out; nested calls that an agent made to another model are counted once, shown as "nested"), then the totals.

Say what the numbers mean: a developer sees only their own events of a project, a project admin sees the project's, a reader or a user who only has a grant sees nothing of the project's usage. Tokens of Codex, Grok and Copilot are estimated from the answer's length, not reported by the provider. If a line says `N events lost between A and B`, the totals are incomplete for that time: tell the user. The gateway owner's view (all users, all projects) and the subscription quota are `minigw gw usage-events` and `minigw gw quota` on the gateway computer (`/minigw:gw`).
