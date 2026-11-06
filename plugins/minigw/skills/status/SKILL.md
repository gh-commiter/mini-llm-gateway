---
name: status
description: Show the Mini Gateway connection and the models available through it. Use when the user asks whether the router works or which gateway models exist.
---

Run these commands and summarize the result for the user in a few lines (router reachable or not, encrypted or not, which providers work, how many models):

```sh
minigw status
minigw models
```

If `minigw` is not found, say that this plugin needs the Mini Gateway client (`brew install --cask gh-commiter/mini-gateway/minigw`) and that Claude Code must be started with `minigw`.
