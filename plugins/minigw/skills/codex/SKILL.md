---
name: codex
description: Hand a self-contained task to OpenAI Codex (GPT-6.1-Sol) through Mini Gateway. Codex runs on the gateway Mac, but minigw shares the current folder with it, so Codex reads, edits and runs commands in your files here, and reports what it did.
argument-hint: "[--ro] [--profile <p>] <task>"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Profiles (`--name`, `profiles`, `--minigw-profile`, `<model>@<profile>`) need minigw 0.5.18 or newer: if the first line of `"${CLAUDE_PLUGIN_DATA}/bin/minigw" --version` is older, tell the user to run `/minigw:gw update` and do not use them (an older minigw passes them to claude).

If the arguments start with `--ro` (or the user asks Codex only to look, analyse or answer without changing files), share the folder read-only: put `--minigw-ro` right after `minigw` and drop `--ro` from the task. Otherwise Codex may edit files.

`--profile <p>` (or the user names a gateway): use `--model claude-codex-gpt-6.1-sol@<p>`, or put `--minigw-profile <p>` right after `minigw`. `minigw profiles` lists the gateways.

Run the task with Bash (timeout 600000 ms, or in the background for long work):

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" -p --model claude-codex-gpt-6.1-sol "$(cat <<'PROMPT'
<the task, self-contained: goal, files, constraints, how to verify>
When you finish, list the files you changed and how you verified the result.
PROMPT
)"
```

Always run it; do not assume Codex cannot see the files. If minigw prints that the folder "is not shared with the gateway", run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" share add` (the user asked for Codex in this folder) and run the task again.

If minigw printed `agent work is on branch minigw/…` (worktree mode, the default in a git repository), the changes are on that branch, not in the files yet, and you integrate them: review `git log HEAD..<branch>` and `git diff HEAD...<branch>` (look at changes to `.gitattributes` and `.gitmodules` first: filter drivers run on checkout, submodule URLs), then bring the changes into the working tree with your own tools under the user's normal permissions (`git merge --ff-only <branch>`, `git cherry-pick`, `git diff HEAD...<branch> | git apply`, or Edit). Never check the branch out blindly. In sync and direct mode the changes are already in the folder.

Then check the changed files yourself (for example `git diff`) before telling the user it is done.
