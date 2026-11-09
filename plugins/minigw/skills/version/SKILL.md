---
name: version
description: Show the versions of the minigw client, Claude Code, the minigw plugin and the gateway. Use when the user asks which minigw or plugin version is installed.
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw --version), Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw plugin status), Bash(cat ${CLAUDE_PLUGIN_ROOT}/.claude-plugin/plugin.json)
---

1. Run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" --version` (one line: `minigw <version>`).
2. If that version is 0.5.20 or newer, run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" plugin status`. Its lines: `minigw:`
   (client version and path), `Claude Code:`, `Plugin:` (the installed plugin), `Settings:`, `Plugin bin:` (which
   minigw the plugin uses) and `Gateway:` (address and version of the default gateway, or why it is not connected).
3. If it is older (it has no `plugin status`), run `cat ${CLAUDE_PLUGIN_ROOT}/.claude-plugin/plugin.json` for the
   plugin version and tell the user that `/minigw:gw update` brings the newer minigw.

Report client, Claude Code, plugin and gateway versions in short lines (those you have); mention it when the plugin bin
points at another minigw. If the minigw file is missing, say that the plugin sets it up at session start and a new
session retries.
