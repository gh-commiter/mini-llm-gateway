---
name: version
description: Show the versions of the minigw client, Claude Code and the minigw plugin. Use when the user asks which minigw or plugin version is installed.
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *), Bash(cat ${CLAUDE_PLUGIN_ROOT}/.claude-plugin/plugin.json)
---

1. Run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" --version` (first line: minigw, second: Claude Code).
2. Run `cat ${CLAUDE_PLUGIN_ROOT}/.claude-plugin/plugin.json` for the plugin version.

Report the three versions in three short lines. If the minigw file is missing, say that the plugin installs it at session start and a new session retries.
