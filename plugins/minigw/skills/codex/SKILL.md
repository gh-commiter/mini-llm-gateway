---
name: codex
description: Hand a self-contained task to OpenAI Codex (GPT-6.1-Sol) through Mini Gateway. Codex runs on the gateway Mac, but minigw shares the current folder with it, so Codex reads, edits and runs commands in your files here, and reports what it did.
argument-hint: "[--ro] <task>"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

If the arguments start with `--ro` (or the user asks Codex only to look, analyse or answer without changing files), share the folder read-only: put `--minigw-ro` right after `minigw` and drop `--ro` from the task. Otherwise Codex may edit files.

Run the task with Bash (timeout 600000 ms, or in the background for long work):

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" -p --model claude-codex-gpt-6.1-sol "$(cat <<'PROMPT'
<the task, self-contained: goal, files, constraints, how to verify>
When you finish, list the files you changed and how you verified the result.
PROMPT
)"
```

Always run it; do not assume Codex cannot see the files. If minigw prints that the folder "is not shared with the gateway", run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" share add` (the user asked for Codex in this folder) and run the task again.

Then check the changed files yourself (for example `git diff`) before telling the user it is done.
