---
name: share
description: Share the current folder (or a given one) with the Mini Gateway router, so agent models (Codex, Claude CLI, Grok) can read and edit it. Use when minigw says a folder "is not shared with the router", for example a project outside the home folder such as D:\Projects on Windows.
argument-hint: "[folder | list | remove [folder]]"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Folders under the home folder are shared automatically. Other folders are shared once the user adds them; the setting is permanent.

- No argument or a folder: `"${CLAUDE_PLUGIN_DATA}/bin/minigw" share add [folder]`
- `list`: `"${CLAUDE_PLUGIN_DATA}/bin/minigw" share list`
- `remove [folder]`: `"${CLAUDE_PLUGIN_DATA}/bin/minigw" share remove [folder]`

minigw refuses a drive root or the home folder itself. Report the result in one line. Sharing applies to the next minigw command.
