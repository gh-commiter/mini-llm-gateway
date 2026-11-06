---
name: status
description: Show the Mini Gateway connection and the models available through it. Use when the user asks whether the router works or which gateway models exist.
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Run these commands and summarize the result for the user in a few lines (router reachable or not, encrypted or not, which providers work, how many models):

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" status
"${CLAUDE_PLUGIN_DATA}/bin/minigw" models
```

If it is not connected, suggest `/minigw:connect <router-mac> <token>`. If the file is missing, the plugin could not install it at session start (it needs network access to GitHub once); a new session retries.
