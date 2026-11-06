---
name: codex
description: Hand a self-contained task to OpenAI Codex (GPT-6.1-Sol) through Mini Gateway. Codex works as an agent in the current folder on the router Mac (reads, edits, runs commands) and reports what it did.
argument-hint: <task>
allowed-tools: Bash(minigw *)
---

Run the task with Bash (timeout 600000 ms, or in the background for long work):

```sh
minigw -p --model claude-codex-gpt-6.1-sol "$(cat <<'PROMPT'
<the task, self-contained: goal, files, constraints, how to verify>
When you finish, list the files you changed and how you verified the result.
PROMPT
)"
```

Then check the changed files yourself (for example `git diff`) before telling the user it is done.
