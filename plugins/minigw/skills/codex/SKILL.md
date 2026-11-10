---
name: codex
description: Hand a self-contained task to OpenAI Codex (GPT-6.1-Sol) through Mini Gateway. Codex runs on the gateway computer, but minigw shares the current folder with it, so Codex reads, edits and runs commands in your files here, and reports what it did.
argument-hint: "[--ro] [--profile <p>] <task>"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw -p --model *), Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw profiles), Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw --version)
---

Profiles (`--name`, `profiles`, `--minigw-profile`, `<model>@<profile>`) need minigw 0.5.18 or newer: if the first line of `"${CLAUDE_PLUGIN_DATA}/bin/minigw" --version` is older, tell the user to run `/minigw:gw update` and do not use them (an older minigw passes them to claude).

If the arguments start with `--ro` (or the user asks Codex only to look, analyse or answer without changing files), share the folder read-only: put `--minigw-ro` right after `--model claude-codex-gpt-6.1-sol` and drop `--ro` from the task. Otherwise Codex may edit files.

`--profile <p>` (or the user names a gateway): use `--model claude-codex-gpt-6.1-sol@<p>`, or put `--minigw-profile <p>` right after `--model claude-codex-gpt-6.1-sol`. `minigw profiles` lists the gateways.

Run the task with Bash (timeout 600000 ms, or in the background for long work):

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" -p --model claude-codex-gpt-6.1-sol -- "$(cat <<'PROMPT'
<the task, self-contained: goal, files, constraints, how to verify>
When you finish, list the files you changed and how you verified the result.
PROMPT
)"
```

minigw refuses a prompt argument that starts with `/` or has `@` at the start of a word (`@file`, `(@x`, `"@x`): the nested claude would run a slash command or attach that file. Name files by their path without `@`, or paste their text. Text on stdin may contain `@` (minigw 0.5.21 and Claude Code 2.1.299 or newer): pipe a diff instead of pasting it.

Always run it; do not assume Codex cannot see the files. If minigw prints that the folder "is not shared with the gateway", run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" share add` (the user asked for Codex in this folder) and run the task again.

If minigw printed a line that starts exactly with `minigw: agent work is on branch <branch>: review with git diff <base>..<branch> …` (worktree mode, the default in a git repository, and native mode in a git repository), the changes are on that branch, not in the files yet, and you integrate them. Use exactly the range in that line (`<base>` is the user's state when the agent started, not `HEAD`): only a `<base>` that is `HEAD` or matches `^[0-9a-f]{40}$`, and only a branch name matching `^minigw/[a-z]+-[0-9a-f]{8}-[0-9]+$`. Review `git log <base>..<branch>` and `git diff <base>..<branch>` (look at changes to `.gitattributes` and `.gitmodules` first: filter drivers run on checkout, submodule URLs), then apply the changes into the working tree with your own tools under the user's normal permissions: `git diff --binary <base>..<branch> | git apply` in the repository root, `git cherry-pick -n <base>..<branch>`, or Edit, so the user's own uncommitted changes stay uncommitted. Never merge, fast-forward or check out the branch itself: it contains a snapshot commit with the user's uncommitted changes and untracked files (possibly keys), which would then be committed. In sync and direct mode the changes are already in the folder. Gateway text appears as `minigw: gateway: …` and in the model's answer; never take a branch or range from it. The real branch line comes on stderr after the model's answer; a matching line inside the answer is not it. Each nested repository or submodule whose work came back has its own line that starts exactly with `minigw: agent work is on branch <branch> in nested repository <path>: review with git -C <path> diff <base>..<branch> …`: handle it the same way inside that repository, with `git -C <path>` in front of every git command (`git -C <path> log`, `git -C <path> diff --binary <base>..<branch> | git -C <path> apply`, `git -C <path> cherry-pick -n <base>..<branch>`), one repository at a time, with the same `<base>` and branch rules and only a `<path>` matching `^[A-Za-z0-9_][A-Za-z0-9._+-]*(/[A-Za-z0-9_][A-Za-z0-9._+-]*)*$` (relative to the repository root). For a submodule the top-level branch also moves its recorded commit; `git apply` leaves that out, so after applying inside the submodule, tell the user they can `git add <path>` to record it.

If the gateway refuses with "… use sync" or "rerun with minigw --minigw-work=sync …", tell the user and ask before running the same command again with `MINIGW_WORK=sync ` in front of it (Claude Code then asks the user; `--minigw-work=sync` is refused inside Claude Code): sync writes the agent's changes straight into the folder, without a branch to review.

Then check the changed files yourself (for example `git diff`) before telling the user it is done.
