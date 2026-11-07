---
name: share
description: Share the current folder (or a given one) with Mini Gateway, so agent models (Codex, Claude CLI, Grok) can read and edit it. Only needed when minigw says a folder "is not shared with the gateway" (system folders, hidden folders); project folders are shared automatically.
argument-hint: "[folder | list | remove [folder]]"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Project folders are shared automatically, on any drive. System and hidden folders are shared once the user adds them; the setting is permanent.

- No argument or a folder: `"${CLAUDE_PLUGIN_DATA}/bin/minigw" share add [folder]`
- `list`: `"${CLAUDE_PLUGIN_DATA}/bin/minigw" share list`
- `remove [folder]`: `"${CLAUDE_PLUGIN_DATA}/bin/minigw" share remove [folder]`

minigw refuses a drive root or the home folder itself. Report the result in one line. Sharing applies to the next minigw command.
