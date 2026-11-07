---
name: share
description: Share the current folder (or a given one) with Mini Gateway, so agent models (Codex, Claude CLI, Grok) can read and edit it. Only needed when minigw says a folder "is not shared with the gateway" (system folders, hidden folders); project folders are shared automatically.
argument-hint: "[folder | list | remove [folder]]"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Profiles (`--name`, `profiles`, `--minigw-profile`, `<model>@<profile>`) need minigw 0.5.18 or newer: if the first line of `"${CLAUDE_PLUGIN_DATA}/bin/minigw" --version` is older, tell the user to run `/minigw:gw update` and do not use them (an older minigw passes them to claude).

Project folders are shared automatically, on any drive. System and hidden folders are shared once the user adds them; the setting is permanent.

- No argument or a folder: `"${CLAUDE_PLUGIN_DATA}/bin/minigw" share add [folder]`
- `list`: `"${CLAUDE_PLUGIN_DATA}/bin/minigw" share list`
- `remove [folder]`: `"${CLAUDE_PLUGIN_DATA}/bin/minigw" share remove [folder]`

The setting is per computer, not per gateway: with several profiles another gateway gets the folder only on its first request for an agent model (`claude-cli-*`, `claude-codex*`, `claude-grok*`, `claude-copilot*`) in that session. A Windows or Linux gateway needs sync or worktree: `share add --work=sync|worktree [folder]`, or `--work=auto` (worktree for a git repository, otherwise sync).

minigw refuses a drive root or the home folder itself. Report the result in one line. Sharing applies to the next minigw command.
