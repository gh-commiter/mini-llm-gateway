---
name: second-opinion
description: Get an independent review of the current changes from OpenAI Codex through Mini Gateway (the minigw:codex-review subagent).
---

1. Collect what to review: if the folder is a git repository, use `git diff` and `git diff --staged`. Otherwise use the files or plan the user named.
2. Give it to the `minigw:codex-review` subagent with a short note about the goal of the change.
3. Show the user the reviewer's findings, most important first. Say which ones you agree with and why, then ask whether to apply fixes.
