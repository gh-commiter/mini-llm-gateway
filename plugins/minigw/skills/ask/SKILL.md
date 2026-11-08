---
name: ask
description: Ask any Mini Gateway model through the minigw client (Codex, Siri AI, Apple Foundation Models, Claude CLI agents, LiteLLM or OpenRouter models) and use its answer. Use when the user wants a specific gateway model, a second model's view, or work done by a gateway agent. Arguments - the model ID, then the question or task.
argument-hint: "[--ro] [--profile <p>] <model>[@<profile>] <question or task>"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *)
---

Profiles (`--name`, `profiles`, `--minigw-profile`, `<model>@<profile>`) need minigw 0.5.18 or newer: if the first line of `"${CLAUDE_PLUGIN_DATA}/bin/minigw" --version` is older, tell the user to run `/minigw:gw update` and do not use them (an older minigw passes them to claude).

Run the question through Mini Gateway with the `minigw` client, in the current folder:

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" -p --model <model> "$(cat <<'PROMPT'
<question or task, with the context the model needs>
PROMPT
)"
```

- `--ro` in the arguments (or a task that only reads or answers): put `--minigw-ro` right after `minigw`, so agent models get the folder read-only and cannot change files.
- `--role <name>` in the arguments: put `--minigw-role <name>` right after `minigw` (or keep `MINIGW_ROLE` from the environment). The gateway records the role in its session history; it is a label, not a security boundary.
- A model ending in `@<profile>` (for example `claude-cli-opus@win`) runs on that profile's gateway; keep the suffix. `--profile <p>`: put `--minigw-profile <p>` right after `minigw` (that gateway only).
- If the user did not name a model, run `"${CLAUDE_PLUGIN_DATA}/bin/minigw" models` and pick a fitting one, or ask.
- Agent models (`claude-codex*`, `claude-cli-*`) run on the gateway Mac but work in the current folder, which minigw shares with it: they read, edit and run commands in your files. Give them a self-contained task. If minigw prints that the folder "is not shared with the gateway", suggest `/minigw:share`.
- If minigw printed `agent work is on branch <branch>: review with git diff <base>..<branch> …` (worktree mode, the default in a git repository, and native mode in a git repository), the changes are on that branch, not in the files yet, and you integrate them. Use exactly the range minigw printed (`<base>` is the user's state when the agent started, not `HEAD`), and only a branch name matching `^minigw/[a-z]+-[0-9a-f]{8}-[0-9]+$`. Review `git log <base>..<branch>` and `git diff <base>..<branch>` (look at changes to `.gitattributes` and `.gitmodules` first: filter drivers run on checkout, submodule URLs), then apply the changes into the working tree with your own tools under the user's normal permissions: `git diff <base>..<branch> | git apply` in the repository root, `git cherry-pick -n <base>..<branch>`, or Edit, so the user's own uncommitted changes stay uncommitted. Never merge, fast-forward or check out the branch itself: it contains a snapshot commit with the user's uncommitted changes and untracked files (possibly keys), which would then be committed. In sync and direct mode the changes are already in the folder. If the gateway refuses with "… use sync" or "rerun with minigw --minigw-work=sync …", run the same command again with `--minigw-work=sync` right after `minigw`.
- Siri AI and Apple Foundation Models have no tools. Name the files they need in the question; the gateway adds them.
- Codex and Claude CLI can take minutes. Use a Bash timeout of 600000 ms, or run it in the background for longer work.
- Show the user the model's answer (summarize long output), then continue with the task.
- The plugin installs `minigw` at session start. If it is not connected, use `/minigw:connect`; if it is missing, start a new session (it needs network access to GitHub once).
