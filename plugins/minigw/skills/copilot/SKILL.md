---
name: copilot
description: Ask GitHub Copilot (through Mini Gateway) for a second opinion or a self-contained task in this folder. Copilot runs on the gateway; it reads the shared folder, and edits files or runs commands only when the gateway allows it (off by default).
argument-hint: "[--ro] <task>"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

If the arguments start with `--ro` (or the user asks Copilot only to look, analyse or answer), share the folder read-only:
put `--minigw-ro` right after `minigw` and drop `--ro` from the task.

Run the task with Bash (timeout 600000 ms, or in the background for long work):

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" -p --model claude-copilot "$(cat <<'PROMPT'
<the task, self-contained: goal, files, constraints, how to verify>
When you finish, list the files you changed (if any) and how you verified the result.
PROMPT
)"
```

Copilot on the gateway is read-only unless its settings allow writing, and runs commands only with "Allow commands" and
its own GitHub token (`minigw gw copilot token` on the gateway): if it says it cannot edit or run something, tell the user
which switch in Mini Gateway → Settings → Copilot CLI it needs. If
minigw prints that the folder "is not shared with the gateway", run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" share add` and run
the task again. Check any changed files yourself (for example `git diff`) before telling the user it is done.
