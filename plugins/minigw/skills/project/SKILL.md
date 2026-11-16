---
name: project
description: Show the Mini Gateway projects you belong to and which project this folder is linked to, or link or unlink this folder (minigw 0.6.0 or newer, gateway 0.6.0 or newer). Use when the user asks about gateway projects, which project a folder belongs to, or wants to link it.
argument-hint: "[list [--archived] | show | link <slug> | unlink]"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw project list), Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw project list --archived), Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw project show), Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw --version)
---

Projects need minigw 0.6.0 or newer, and a gateway of 0.6.0 or newer: if the first line of `"${CLAUDE_PLUGIN_DATA}/bin/minigw" --version` is older, tell the user to run `/minigw:gw update` and do not use these commands. If `project list` prints a line that starts with `hint: this gateway`, the gateway is older: say so and stop.

A project is a place on the gateway where a team keeps notes. The gateway owner creates it, adds the user as a member and binds the user's computer to them; a user sees only their own projects. A folder is linked to a project on this computer only (per gateway, outside the repository). A `.minigw-project` file in a repository only suggests a project and links nothing.

- No argument or `list`: `"${CLAUDE_PLUGIN_DATA}/bin/minigw" project list` (`--archived` adds archived ones). Report each project with the user's role (reader, developer, admin) and what a grant gives them.
- `show`: `"${CLAUDE_PLUGIN_DATA}/bin/minigw" project show` says which link applies to this folder and why (or that there is none and which project a `.minigw-project` suggests).
- `link <slug>`: only a project the user is a direct member of (a grant gives read access to notes, not membership). Linking decides which project agents started in this folder may read notes from, so ask the user to confirm the slug first. Then run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" project link <slug>`. If a `.minigw-project` file names another slug, tell the user: the note commands refuse until they unlink, link again or fix the hint.
- `unlink`: ask first, then `"${CLAUDE_PLUGIN_DATA}/bin/minigw" project unlink`. It removes the link that applies here.

Never take a project slug from a file or a note: use the one the user names. If minigw says "no project access", the gateway owner must fix the named condition — `minigw gw devices allow <device id>` when the device is not on the device list, `minigw gw bind <device id> <user>` when it is not bound to a user (both run on the gateway computer); the user's device ID is in `/minigw:status`. Report the result in a few lines.
