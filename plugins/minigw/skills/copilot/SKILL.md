---
name: copilot
description: Ask GitHub Copilot (through Mini Gateway) for a second opinion or a self-contained task in this folder. Copilot runs on the gateway; it reads the shared folder, and edits files or runs commands only when the gateway allows it (off by default).
argument-hint: "[--ro] [--profile <p>] <task>"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Profiles (`--name`, `profiles`, `--minigw-profile`, `<model>@<profile>`) need minigw 0.5.18 or newer: if the first line of `"${CLAUDE_PLUGIN_DATA}/bin/minigw" --version` is older, tell the user to run `/minigw:gw update` and do not use them (an older minigw passes them to claude).

If the arguments start with `--ro` (or the user asks Copilot only to look, analyse or answer), share the folder read-only:
put `--minigw-ro` right after `minigw` and drop `--ro` from the task.

`--profile <p>` (or the user names a gateway): use `--model claude-copilot@<p>`, or put `--minigw-profile <p>` right after `minigw`. `minigw profiles` lists the gateways.

Run the task with Bash (timeout 600000 ms, or in the background for long work):

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" -p --model claude-copilot "$(cat <<'PROMPT'
<the task, self-contained: goal, files, constraints, how to verify>
When you finish, list the files you changed (if any) and how you verified the result.
PROMPT
)"
```

Copilot on the gateway needs its own GitHub token (`minigw gw copilot token` on the gateway), is read-only unless its
settings allow writing, and runs commands only with "Allow commands": if it says it cannot edit or run something, tell the user
which switch in Mini Gateway → Settings → Copilot CLI it needs. If
minigw prints that the folder "is not shared with the gateway", run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" share add` and run
the task again.

If minigw printed `agent work is on branch minigw/…` (worktree mode, the default in a git repository), the changes are on that branch, not in the files yet, and you integrate them: review `git log HEAD..<branch>` and `git diff HEAD...<branch>` (look at changes to `.gitattributes` and `.gitmodules` first: filter drivers run on checkout, submodule URLs), then bring the changes into the working tree with your own tools under the user's normal permissions (`git merge --ff-only <branch>`, `git cherry-pick`, `git diff HEAD...<branch> | git apply`, or Edit). Never check the branch out blindly. In sync and direct mode the changes are already in the folder.

Check any changed files yourself (for example `git diff`) before telling the user it is done.
