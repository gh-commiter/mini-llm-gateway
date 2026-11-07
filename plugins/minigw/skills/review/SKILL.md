---
name: review
description: Get an independent review of the current changes, a plan or a bug analysis from OpenAI Codex (GPT-6-Astra) through Mini Gateway. Use when the user asks for a second opinion or a review by another model.
argument-hint: "[--rw] [--profile <p>] [what to review]"
allowed-tools: Bash(${CLAUDE_PLUGIN_DATA}/bin/minigw *), Bash(git diff*), Bash(git status*)
---

Profiles (`--name`, `profiles`, `--minigw-profile`, `<model>@<profile>`) need minigw 0.5.18 or newer: if the first line of `"${CLAUDE_PLUGIN_DATA}/bin/minigw" --version` is older, tell the user to run `/minigw:gw update` and do not use them (an older minigw passes them to claude).

1. Collect what to review: the user's argument, or `git diff` and `git diff --staged` in a git repository.
2. Run the review with Bash (timeout 600000 ms). The folder is shared read-only (`--minigw-ro`): the reviewer cannot change files. Leave out `--minigw-ro` only if the user passed `--rw` (for example so the reviewer may run a build that writes output):

```sh
"${CLAUDE_PLUGIN_DATA}/bin/minigw" --minigw-ro -p --model claude-codex-gpt-6-astra "$(cat <<'PROMPT'
You are an independent reviewer. Do not modify files.
Goal of the change: <one or two sentences>
Review the following in the current folder and report concrete problems first (bug, wrong assumption, missing case, security), with file and line, then smaller improvements briefly:
<diff, file names or plan>
PROMPT
)"
```

`--profile <p>`: use `--model claude-codex-gpt-6-astra@<p>` (that gateway; `minigw profiles` lists them).

3. Show the findings, most important first. Say which ones you agree with and why, then ask whether to apply fixes.
