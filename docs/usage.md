# Using Mini Gateway

## Quota history

`minigw gw quota --history [--days N] [--provider P] [--json]` shows existing probes without running one, on both
clients. Text prints one line per attempt with every window, value, unit and reset, or a bounded error kind. The
default is seven days; the range is 1–400 days. Provider matching is exact: Go uses the configured provider name,
Swift its provider ID; CLI providers use `claude` and `codex`. Results contain the newest 1000 probes and at most
1000 loss gaps; truncated responses use HTTP 206 and clients warn on stderr, including with `--json`.

The shared core retains quota detail for 400 days, pruned at startup and in the usage recorder's daily retention
pass. Clear History clears legacy sessions, not quota history. Backup and restore include it. Cached reads,
disabled probing and retry backoff create no samples. Claude probing stays opt-in. No background sampling is added.
Account quota includes work outside Mini Gateway; missing samples are not zero usage.

Account scopes are random opaque IDs, private in storage and never shown; neither credentials nor their hashes are stored.
The core keeps one random scope per provider (in its meta table), so the history of one account stays one series across
restarts. When the gateway sees the key or address of a provider change while it runs, the next sample starts a new
random scope (a change made while it was stopped cannot be seen). CLI logins (Claude, Codex) have no safe account
identifier: they keep one scope per provider. History exposes no scopes, source metadata or raw errors. Loss gaps describe recorder losses across all
providers, including when the probe list is filtered. See [the admin API](admin-api.md) for the response contract.

Scenarios from first use to parallel agents. Unless it starts with `minigw gw` or `minigw serve` (owner commands, on the
gateway computer), every command here runs on the client, the computer where you work. For the full option list see
[commands.md](commands.md); for what happens on the wire see
[how-it-works.md](how-it-works.md).

- [Run Claude Code through the gateway](#run-claude-code-through-the-gateway)
- [Ask another model from Claude Code](#ask-another-model-from-claude-code)
- [Hand a task to an agent](#hand-a-task-to-an-agent)
- [Get a review](#get-a-review)
- [Work modes: what happens to your files](#work-modes-what-happens-to-your-files)
- [Worktree mode: results as branches](#worktree-mode-results-as-branches)
- [Read-only and roles](#read-only-and-roles)
- [Which folders are shared](#which-folders-are-shared)
- [Projects and notes](#projects-and-notes)
- [Several gateways (profiles)](#several-gateways-profiles)
- [Native mode and builds on the gateway Mac](#native-mode-and-builds-on-the-gateway-mac)
- [Models](#models)

## Run Claude Code through the gateway

```sh
minigw                                   # Claude Code with the gateway's models; pick one with /model
minigw --model claude-apple-fm -p "Hi"   # any claude option works in a terminal
minigw models                            # list the models
```

`minigw` starts `claude` with the gateway as its LLM endpoint. A subagent can use a gateway model too:
`model: claude-siri-ai` in `.claude/agents/<name>.md`.

You do not have to run your whole session through the gateway. With the Claude Code plugin your session keeps
your normal Claude login, and the plugin skills call single gateway models (next section).

## Ask another model from Claude Code

In any Claude Code session with the plugin:

```
/minigw:models
/minigw:ask claude-codex What is the worst-case complexity of this algorithm in src/sort.go?
/minigw:on-device Summarize these meeting notes in three lines: …
/minigw:siri What is on my calendar tomorrow?
```

- `/minigw:ask <model> <question>` takes any model ID that `/minigw:models` lists.
- `/minigw:on-device` uses Apple Foundation Models on the gateway Mac (about 8K context). Use it for short texts
  that should not go to a cloud model.
- `/minigw:siri` answers with the personal context of the gateway Mac's user. The gateway must allow Siri for
  your device ([gateway.md → Siri AI](gateway.md#settings)).

Claude can pick these skills by itself when the request fits. The answer comes back into your session.

## Hand a task to an agent

Agent models (`claude-codex*`, `claude-grok*`, `claude-copilot*`, `claude-cli-*`) run on the gateway computer but
work in your current folder. `minigw` brings the folder to them; how depends on the [work mode](#work-modes-what-happens-to-your-files).

```
/minigw:codex Add input validation to cmd/serve.go and run go test ./...
/minigw:copilot Explain how the retry logic in client/http.go works
/minigw:ask claude-cli-opus Refactor the parser in src/parse.ts; keep the public API
```

From a terminal:

```sh
minigw -p --model claude-codex "Fix the failing test in pkg/store and run go test ./pkg/store"
```

What you get back:

| Folder | Default mode | Where the agent's changes are |
|---|---|---|
| Root of a git repository with a commit | worktree | On a branch `minigw/<agent>-<session>-<n>` in your repository. Your files do not change. See [Worktree mode](#worktree-mode-results-as-branches). |
| Any other project folder | sync | In your folder, copied back after each request |
| Gateway on the same Mac | – | In the folder itself (nothing is shared) |

The agent's own permissions are set on the gateway (writing, network, commands). Codex, Grok and Copilot are off
until the gateway owner turns them on. Grok is not supported on the Mac gateway (Windows and Linux gateways only); a Mac
gateway answers 400 "Grok is not supported on the Mac gateway. Use another agent, or Grok through minigw serve on Windows or
Linux." See [gateway.md → Settings](gateway.md#settings).

### Watching a long run

While an agent works, `minigw -p` prints each of its steps on stderr as it happens (`minigw: · read_file src/a.go`;
paths are relative to your shared folder, never the gateway's own folders). This holds for Codex, Grok and Copilot on a
Windows or Linux gateway, and for Codex and Copilot on a Mac gateway; the answer on stdout never contains the steps. A run
can take a long time, so the gateway also keeps the connection alive with a ping and an empty text delta every 10 seconds
(Claude Code drops a stream that shows no real event for a long time).

To ask what your requests are doing right now, from any terminal on the device that started them:

```sh
minigw running            # model, folder, time running, number of steps, last step and how long ago
minigw running --json
```

You see only your own device's running requests, never a prompt and never another device's. It needs no admin rights
(`minigw gw sessions` is the admin view on the gateway computer). Both gateways answer it; a gateway older than this
version answers 404, which `minigw` reports as "too old".

Agents can also ask each other: with **May ask other models** on for an agent, it can call another gateway model
with `minigw -p --model <id> "…"`, one level deep and never its own kind.

## Get a review

```
/minigw:review
/minigw:review the plan in docs/plan.md
/minigw:review --rw run the tests and review the failures
```

`/minigw:review` asks Codex (`claude-codex-gpt-6-astra`) for an independent review of your uncommitted changes, a
plan or a bug analysis. The folder is shared read-only, so the reviewer cannot change files; `--rw` allows writing.
Claude then shows the findings and says which ones it agrees with.

In a git repository the skill pipes the diff to the reviewer on stdin (`git diff HEAD | minigw --minigw-ro -p …`). A
Windows or Linux gateway gets a read-only copy of the folder without `.git`, so this is how the reviewer there sees
the diff. Inside Claude Code, minigw accepts `@` in text on stdin (the `@@` lines of a diff) only with Claude Code
2.1.299 or newer: minigw then turns off file mentions for the nested claude in a way your `settings.json` cannot undo.
With an older Claude Code it refuses such stdin, and the skill describes the changes instead. A prompt argument with
`@` at the start of a word, and a prompt or stdin that starts with `/`, are still refused. If stdin brings nothing
within 3 s of a run with a prompt argument, minigw goes on without it and says so on stderr.

## Work modes: what happens to your files

The work mode decides how an agent on another computer gets your folder. Choose it per run with
`--minigw-work=<mode>` (or `MINIGW_WORK=<mode>`), or save a default per folder with `minigw share add --work=<mode>`.
Priority: flag or environment, then the saved folder mode, then `auto`.

| Mode | What the agent works on | Your files | Gateways |
|---|---|---|---|
| `auto` (default) | Picks one of the modes below | – | all |
| `worktree` | A clone of your git repository on the gateway | Unchanged; results come back as a branch | Mac, `minigw serve` |
| `sync` | A copy on the gateway's disk | Changed files are copied back after each request | Mac, `minigw serve` |
| `direct` | Your folder, mounted on the gateway Mac over the connection (WebDAV) | Changed in place, git data writable (commits work), other protected paths read-only | Mac only |
| `native` | The gateway's own folder, when your folder is an SMB mount of it | Git repository: a branch per agent; otherwise in place | Mac only |
| `native-direct` | As `native`, but always in the folder itself | Changed in place, one writing agent at a time | Mac only |

**`auto`** picks, in this order:

1. Read-only (`--minigw-ro`) → `direct`. A Windows or Linux gateway has no `direct` and uses `sync` instead.
2. The folder is an SMB mount of a gateway share → `native`.
3. The folder is the root of a git repository with a commit → `worktree`, else `sync`. Never `direct` read-write:
   agents could not commit there.
4. Otherwise → `sync`, then `direct`.

A subfolder of a git repository is synced, so files outside it are never sent. Only an explicit
`--minigw-work=worktree` uses the parent repository. An explicit `--minigw-work=direct` is refused by a Windows or
Linux gateway. `minigw share add --work=direct` keeps direct mode as the default for one folder.

**Inside Claude Code** (the plugin's skills; see [What minigw passes to Claude Code](how-it-works.md#what-minigw-passes-to-claude-code)
for how minigw tells), `--minigw-work=direct`, `sync` and `native-direct` are
refused, because the skills' model calls run without asking you and these modes change your files without a branch to
review. They work when the run is read-only, when the folder is saved with that mode (`minigw share add --work=`), or
with `MINIGW_WORK=<mode>` in front of the command, which Claude Code asks you about. An explicit `native` or `worktree`
that is not possible in the folder fails instead of falling back to `direct`.

A gateway on the same Mac needs no mode: agents work in the folder itself. A `minigw serve` gateway on the same
Windows or Linux computer still uses sync or worktree.

### Sync mode

`minigw --minigw-work=sync …` copies the folder to the gateway's Scratch folder before each agent request and copies
the agent's changes back after it. Agents and builds work on the gateway's local disk.

- **Sent:** every file, hidden ones included, except files matched by `.gitignore` and a built-in list (`.git`,
  `target/`, `.build/`, `node_modules/`, `build/`, `dist/`, `DerivedData/` …). After the first request only changed
  files go (compared by size, time and SHA-256). When the root `.gitignore` (not the built-in list) keeps top-level
  entries out, `minigw` prints one line on stderr the first time in a run, naming up to five of them: the agent
  cannot see those (use direct mode, or put what it needs in the prompt).
  Models of a provider (LiteLLM, OpenRouter, xAI) with `--minigw-tools` do not use sync at all: their tools read the folder here.
- **Never copied back:** files the agent puts into a [protected path](how-it-works.md#protected-paths).
- **Conflicts:** if you changed a file the agent also changed, yours stays and the agent's version is saved next to
  it as `<name>.minigw-conflict` (a later conflict on the same file: `-2`, `-3`, ...; copies are never overwritten).
- **Read-only:** with `--minigw-ro` nothing comes back.
- **Kept between requests:** the next sync and build are incremental. A copy that no session used for 14 days is
  deleted. At the synced-folder limit, the gateway also points the owner to `minigw gw sync remove`.
- **Names:** files whose names differ only in case (`A.txt`, `a.txt`) are not synced, with a note. Accented names
  travel in NFC. A Linux folder that holds the NFD form of a name may get a second, NFC copy back.
- Agents in the same folder run one after another.

Limits are in [how-it-works.md → Limits](how-it-works.md#limits).

### Direct mode (Mac gateway)

`minigw --minigw-work=direct …` lets the gateway Mac mount your folder through the encrypted connection (built-in
WebDAV). The agent changes your files in place and, in a git repository, can commit there (`git add`, `git commit`,
`git branch`, `git checkout`, `git stash`, `git tag`, `git merge` of a local branch). Writes to other
[protected paths](how-it-works.md#protected-paths) are refused: the agent cannot change `.git/config`, hooks,
attributes, alternates, a rebase in progress, or the folder's Claude Code settings. Use worktree mode when you want
the agent to work on a separate copy and bring its work back as a branch. A git repository still gets worktree mode by
default; direct is only what you ask for. See [Git in direct mode](how-it-works.md#protected-paths).

## Worktree mode: results as branches

`minigw --minigw-work=worktree …` is the default for the root of a git repository. The agent works in its own clone
on the gateway; you get its work as a branch and decide what to keep.

**Dependencies folder.** Each conversation's clone has a dependencies folder next to it under the gateway's Scratch
folder; agents see it as `$MINIGW_DEPS`. Repositories an agent creates (`git init`, `git clone`, CMake FetchContent
with git sources, SwiftPM checkouts) go only there, and the same rule holds for every agent: anywhere else, also inside
the clone, git refuses with a hint to use `$MINIGW_DEPS` (for example
`cmake -DFETCHCONTENT_BASE_DIR="$MINIGW_DEPS/cmake"`, `swift build --scratch-path "$MINIGW_DEPS/swiftpm"`, Xcode
`-clonedSourcePackagesDirPath "$MINIGW_DEPS/spm" -derivedDataPath "$MINIGW_DEPS/DerivedData"`). The folder is kept
for the whole conversation and removed with the clone; it never comes back to you and is not kept with a kept clone.
Only an agent that may write gets it; `git clone` of a remote URL needs the agent's network for builds (the refusal
names the setting). On a Windows gateway this works in the Windows sandbox (Claude CLI); Codex there, and
every agent on a Windows gateway without the Windows sandbox, cannot create repositories; the folder still holds
downloads and caches.

Everything agent builds write is under the Scratch folder, never in the system's temporary folder or your home folder:
the clone, `$MINIGW_DEPS` (also Claude Code's temporary folder, `CLAUDE_CODE_TMPDIR`), one Maven repository per project
and shared download caches per project for Go modules (`GOMODCACHE`), npm and pip. Like the Maven repository, those
are shared by the project's conversations, so one conversation's agent could change what another one downloads; this
is accepted for reuse. The Go build cache (`GOCACHE`), Cargo (`CARGO_HOME`) and Gradle (`GRADLE_USER_HOME`) get their
own folder per conversation in `$MINIGW_DEPS`, because builds use their content without checking it (compiled
output, settings and scripts). Claude Code needs a short temporary path (its sockets), so on the Mac and Linux it
gets a link `/tmp/mgw-…/<name>` that points into `$MINIGW_DEPS/tmp`: the files stay under Scratch, and only the link
is in `/tmp`, in a private folder of the gateway user that agents cannot write. The link goes with the clone (Mac) or
the run (`minigw serve`); if the gateway cannot make it, the run is refused. On Windows the temporary folder is
`$MINIGW_DEPS/tmp` itself (in sync mode a folder under `Scratch\tmp` for the run, removed after it). Before every run the gateway checks that these folders are still real folders of its user
(an agent could replace one with a link to another place) and makes them again if not.

**1. Start.** minigw sends the repository to a mirror on the gateway (git bundles over the encrypted connection,
only missing objects). The agent starts from your exact state: `HEAD` plus your uncommitted changes and your
untracked files that are not ignored. **Those files are sent to the gateway too.** minigw saves them as a commit
"minigw: your uncommitted changes" with a temporary index. Your index, branches and files do not change.

**2. The agent works** in a clone on branch `minigw/<agent>-<session>-<n>` (for example `minigw/codex-1a2b3c4d-0`).
Parallel agents get separate branches. On `minigw serve` there is one clone per session and agent, so the branch
ends in `-0` and a second request in the same session moves it forward.

**3. Results come back.** After each request the gateway commits what the agent left uncommitted, and minigw
fetches the branch into your repository. It is never checked out. minigw prints one line per branch on stderr (with
`minigw -p` without `--output-format`, after the model's answer):

```
minigw: agent work is on branch minigw/codex-1a2b3c4d-0: review with git diff <base>..minigw/codex-1a2b3c4d-0 (<base> = your state when the agent started), then bring it in with git cherry-pick <base>..minigw/codex-1a2b3c4d-0 or git diff --binary <base>..minigw/codex-1a2b3c4d-0 | git apply
```

**4. Review and bring it in.** `<base>` is the snapshot commit with your uncommitted changes. Use the range from
that line:

```sh
git log  <base>..minigw/codex-1a2b3c4d-0
git diff <base>..minigw/codex-1a2b3c4d-0
git diff --binary <base>..minigw/codex-1a2b3c4d-0 | git apply     # keeps your own changes uncommitted
# or: git cherry-pick <base>..minigw/codex-1a2b3c4d-0
```

- Do **not** merge, fast-forward or check out the branch itself. It contains the snapshot commit, so your
  uncommitted changes and untracked files (keys too) would end up committed.
- Look at changes to `.gitattributes` (filter and diff drivers run on checkout) and `.gitmodules` (submodule URLs)
  first.
- The plugin skills do this for you: they review the range and apply it with `git apply` or `git cherry-pick -n`.

**5. Clean up.** The branch stays in your repository until you delete it:

```sh
git branch --list 'minigw/*'
git branch -D minigw/codex-1a2b3c4d-0
```

On the gateway, mirrors and clones that no agent used for 30 days are removed; `minigw serve` removes a session's
clone when the session ends.

**Continuing a conversation:** `minigw -p --continue …` (or `--resume <id>`) starts the agent from the
conversation's last branch, not from your folder, so it sees the files it made in the earlier turns. It does so even
when your folder changed since the last turn (your own edits, new commits, a branch you cherry-picked or applied); those changes
are not in the agent's copy, and minigw says so:

```
minigw: worktree: continuing from minigw/codex-1a2b3c4d-0; changes in your folder since the last turn are not in the agent's copy
```

The new branch contains the earlier turns too, and its line says
`(<base> = your state when this conversation started; this branch includes the earlier turns)`: one range covers the
whole conversation, so bring in only the newest branch (after applying an earlier one, review the range for what is
new). `--resume <id>` and `--continue` reuse that session's branch name, which moves forward.

- `--continue` (`-c`) takes the last conversation minigw ran in this folder: minigw passes it to `claude` as
  `--resume <id>` of that conversation. It never continues a Claude Code conversation you had without minigw (plain
  `claude -c` would take the newest one in the folder and send its transcript to the gateway). When minigw has no
  conversation in this folder yet, it starts a new one and says `minigw: no earlier minigw conversation in …; starting
  a new one`. minigw keeps the last conversation per folder in `minigw/last-session.json` (macOS
  `~/Library/Application Support`, Linux `~/.config`, Windows `%AppData%`), and writes it only after `claude` ended
  without an error, so a failed run keeps the earlier conversation. Only runs outside Claude Code are recorded: model
  calls from the plugin's skills never replace it, so `minigw -c` in your terminal always continues your own
  conversation. Write `-c` and `-r` as separate options: minigw refuses combined short options such as `-pc`.
  `--resume <id>` takes that session; `--resume <id> --fork-session` continues from that session's branch, and the fork then
  goes on as its own conversation. `--resume` without an id (the session picker) starts from your folder.
- All agents of one minigw run start from the same point (decided at the run's first agent), so agents in one turn
  stay independent of each other. The next turn continues from the branch that came back last in the previous run.
- It starts from your folder, with one line `minigw: worktree: starting from your folder, not from minigw/…: <reason>`,
  when you deleted the branch, when the conversation ran on another gateway (the branch of gateway A is never sent to
  gateway B), or in a repository with nested repositories; also for a conversation that has no branch yet. A branch
  you already merged into `HEAD` starts from your folder without a line: your folder has its work and your newer work.
- To start a conversation fresh from your folder, delete its branch (`git branch -D minigw/…`).
- minigw keeps the last branch per conversation in `.git/minigw-continue.json`.

**What the fetch does not do:** it takes only a git bundle, checks objects (fsck), runs no hooks, submodules or
filters, and does not change `.git/config`. If you committed on that branch yourself, your commits stay and the
agent's version is not fetched.

**Nested repositories and submodules:** in worktree mode (and in native mode on the Mac) every git repository inside
the repository at a path git does not ignore gets the same treatment as the top level: submodules and untracked nested
repositories, with or without a commit, at any depth. minigw sends a snapshot of each (one without a commit gets a
commit without a parent from its index and files), the gateway clones each at its path inside the agent's clone, and
the agent's changes in it come back as branch `minigw/<agent>-…` in that repository, with one line per repository:

```
minigw: agent work is on branch minigw/codex-1a2b3c4d-0 in nested repository libs/core: review with git -C libs/core diff <base>..minigw/codex-1a2b3c4d-0 (…), then bring it in with git -C libs/core cherry-pick … or git -C libs/core diff --binary … | git -C libs/core apply
```

Review and apply each repository on its own, with `git -C <path>`. For a submodule the top-level branch also records
the submodule's new commit; `git apply` leaves that out, so `git add <path>` after applying inside the submodule. An
untracked nested repository is never a gitlink in the top-level snapshot or branch, so `git cherry-pick -n` of the
top-level range stays clean. A path with characters other than letters, digits and `. _ + -` gets a line without
commands; review it by hand. A folder or gitlink that is a symbolic link is not a nested repository of the folder.
Notes:

- The mode never changes because of a folder. These stop the run before the agent starts, with a message that ends in
  "use sync": more than 32 nested repositories, a name with `:`, `\`, control characters or inside `.build`, or a
  nested repository whose `.git/config` defines git filters (`filter.*`; minigw does not run them for nested
  repositories). A gateway older than 0.5.20 stops it with "update the gateway".
- Client requirement for nested repositories: git 2.40 or later (Debian 12, Ubuntu 22.04 and older Xcode Command Line
  Tools have older git); with older git the request is refused ("update git or use sync").
- Gateway requirements for nested repositories. Mac app: git 2.40 or later on the Mac (older Xcode Command Line Tools
  git is older), also in native mode; otherwise the request is refused before the agent starts with "nested git
  repositories need git 2.40 or later on the gateway; update git or use sync" (native mode suggests
  `--minigw-work=native-direct` instead of sync). Linux `minigw serve`: bwrap must work, and for Claude
  CLI also bwrap inside bwrap, so every agent runs in its own process namespace. On Ubuntu 24.04 AppArmor's
  `bwrap-userns-restrict` profile blocks the inner one; allow it (for example with an AppArmor profile for
  `/usr/bin/bwrap` that grants `userns`, or `sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0`) or use sync.
  Without that, a request with nested repositories is refused before the agent starts (a final error: Claude Code does not retry it). macOS `minigw serve` (for
  development) refuses nested repositories; Windows `minigw serve` handles them. `minigw serve` also needs git 2.40 or
  later on the gateway; with older git the request is refused ("nested git repositories need git 2.40 or later on
  the gateway; update git or use sync"). If an agent would run there without its own process namespace in a clone
  with nested repositories (for example Codex without bwrap, called by Claude CLI), only that request is refused: "nested repositories need bwrap for <agent> on this Linux gateway (its own PID
  namespace); this request did not run; use sync". The next request in the session works as before.
- If a nested repository cannot come back (the agent moved or replaced it, its branch is larger than 64 MB, or the
  gateway could not save it), the top-level branch and the other nested repositories still come back, without that
  repository's files. minigw prints one line per such repository with the reason:
  `minigw: gateway: worktree: <reason>; the agent's files are kept on the gateway for 30 days as
  worktrees/<device>/<id>/<clone>-kept-<time> after this session (files only; its .git folders renamed .git.agent, or
  nothing is kept if that fails — do not run git there)`. The path is inside the gateway's Scratch folder. A later request in the same session that
  delivers the work ends that. When the session ends (or the next request needs a new clone), the gateway keeps the
  agent's files there with every `.git` renamed `.git.agent` (and the `HEAD` of any folder shaped like a bare
  repository renamed `HEAD.agent`), so no git command picks up the agent's git settings or
  hooks; copy the files you need. At most 3 such folders are kept per repository (the oldest goes first), each for
  30 days. The same applies when no branch could be delivered at all, except for a clone refused as too large.
- The agent may commit inside a nested clone: its note names the clones. On the Mac app and on Linux
  `minigw serve`, git run in a nested clone's folder (`cd <path> && git …` or `git -C <path> …`) works in that clone,
  and plain git elsewhere in the clone works on the top repository. This holds for every agent (Claude CLI, Codex, Grok,
  Copilot). On a Windows `minigw serve` gateway with the Windows sandbox it is the same, and the agent commits its own
  work. Without the Windows sandbox the note there tells the agent not to run `git add` or `git commit`, and the
  gateway commits its changes when it finishes. The agent's commits are kept; changes it leaves uncommitted are
  committed by the gateway.
- Agent git in the clone (all gateways, Windows too) does not use the gateway owner's git config (identity, signing, credential helpers,
  `url.*.insteadOf`, global ignore file). Agent commits are authored as the agent, for example
  `Claude (Mini Gateway) <claude@minigw.invalid>`, `Codex (Mini Gateway) <codex@minigw.invalid>`, Grok or Copilot
  likewise; you see that author in `git log <base>..<branch>` of the delivered branch.
- A nested repository in a folder git ignores (for example `build/_deps` from a tarball download) stays out, as before.
  `git init` and `git clone` in the clone are refused (use `$MINIGW_DEPS`); if a repository appears in the clone
  anyway, minigw prints "changes inside the nested repository <path> do not come back in worktree mode; use sync" and
  no branch is delivered for that run.

**Agents in worktree mode:**

- Claude CLI agents get Bash, always in Claude Code's sandbox. Codex uses its own sandbox with the clone as the
  writable folder.
- Agents that may not write (Codex, Copilot or Grok with writing off) only read in the clone and bring back no branch.
- Grok with writing on is refused in worktree mode (Windows and Linux gateways): "rerun with minigw --minigw-work=sync …".
- Agents write only under the Scratch folder: the clone, `$MINIGW_DEPS` (repositories they create, CMake, SwiftPM,
  DerivedData, Cargo, Gradle, the Go build cache, temporary files), the project's Maven repository and the shared
  download caches (Go modules, npm, pip). Not `/tmp`, never your `~/.m2` settings, plugins or SwiftPM configuration.
  Claude cannot read your home folder (from `~/.m2` only `settings.xml` and `toolchains.xml`). Maven builds work;
  on the Mac `swift build` and `xcodebuild` fail in the agents' sandbox (they need macOS's per-user temporary folder
  and a nested sandbox). Build Swift and Xcode projects in sync mode or with
  [`minigw run`](#native-mode-and-builds-on-the-gateway-mac).
- Network for builds: on for Claude, off for Codex. Mac: Settings → General → Worktree mode (also "Network for Codex
  builds"). `minigw serve`: settings page → General → Worktree mode → **Network for builds** (Claude CLI; Codex follows
  its own "Allow network access for commands"), `minigw gw settings` → General, or `PATCH general {"worktreeNetwork":
  false}`. With it off Claude's commands have no network (Linux and macOS `minigw serve`: Claude Code's sandbox; Windows:
  the Windows sandbox, which allows only TCP 443 to the addresses of `api.anthropic.com` resolved when the run starts,
  so Claude's web fetch and web search fail; if the name cannot be resolved the run is refused) and a `git clone` of a
  remote URL is refused and names the setting; on a Windows gateway without the Windows sandbox the switch has no
  effect. On the Mac Copilot follows "Network for Codex builds" (and its "Allow web access") for that.
- On `minigw serve`, agent requests run one at a time per device and repository, also across that device's
  sessions. The gateway uses its own `git` from PATH, without system or global config, hooks, fsmonitor or
  submodules.

## Read-only and roles

```sh
minigw --minigw-ro -p --model claude-codex "Find the bug in src/cache.go; do not change files"
MINIGW_SHARE=ro minigw …                      # the same for every run, e.g. in the env of Claude Code's settings
minigw --minigw-role security -p --model claude-codex "…"
```

**Read-only** (`--minigw-ro` or `MINIGW_SHARE=ro`): minigw refuses every write, the gateway mounts the folder
read-only, Codex and Grok run in their read-only sandbox, Copilot runs without its write tool, and Claude CLI gets only
its reading tools (Read, Grep, Glob, web search and fetch; commands only in a sandbox that cannot write to the
folder). This also holds when the gateway runs on the same computer. `/minigw:ask --ro` and `/minigw:review` use it.
**Tools of provider models** (`--minigw-tools=read`, `edit` or `none`): a LiteLLM or OpenRouter model runs in the local Claude Code with the
session's permissions (Bash, Edit, MCP tools, sub-agents), and `--tools` is refused inside Claude Code. `--minigw-tools=read` gives it only Read,
Grep and Glob; `none` gives it no tools; `edit` adds Edit and Write, still no Bash, MCP or sub-agents. With `edit` the model changes files only
inside the current folder (`Edit(./**)`, `--permission-mode=dontAsk`: anything else is refused without a prompt), never through a symlink that leads out of it
or through a symlink inside the folder to a protected place (`hooks` → `.git/hooks`, a link to `.git/config` or to `.claude`: the CLI resolves the link and
refuses; measured with Claude Code 2.1.322 on macOS, for existing and new files), and never in `.git` (also a gitdir file), `.claude/settings*.json`,
`.claude/{hooks,commands,agents,skills}`, `.husky`, `.githooks`, `.vscode/tasks.json` or `.envrc`, in any subfolder and in any letter case (a deny rule per
name, checked in the tests against the paths that direct mode protects on macOS and Linux). Claude Code itself also refuses other files inside `.claude`
and `.vscode`. **Windows:** `edit` is refused on a Windows client for now, because its path rules cannot cover Windows name forms (a trailing dot or space,
8.3 short names such as `CLAUDE~1`); Unicode look-alikes such as `ſ` are not covered either (they are not the same name on APFS or NTFS). **Not protected**,
and later run or obeyed without a person reading them: `Makefile`, `package.json` scripts, `.github/workflows`, `.pre-commit-config.yaml`, `CLAUDE.md`,
`AGENTS.md`, `.cursor/rules`, `.mcp.json`, `.vscode/settings.json`, `.gitmodules`. A model that may write can change any of them, so give `edit` only to
models and folders where a review of the diff follows. `edit` with `--minigw-ro` is an error, and so is `edit` above what the gateway's owner allows for the model (`minigw gw provider tools <provider> edit [--model m]`, default `read`); the error names that command. minigw passes `--tools=<list>`, `--strict-mcp-config` (no MCP tools, not even the claude.ai connectors),
`--disable-slash-commands` and `--setting-sources=` to claude: with the default setting sources a hook in your user, project or local settings
still runs (measured with a real run), so for this run **your own settings and hooks are not loaded**. It is accepted inside Claude Code because the set
is fixed by minigw (read-only, or edit limited to the folder), not chosen by the caller. While a restriction applies, minigw refuses options that could widen it, also outside Claude Code and with
`MINIGW_ALLOW_UNSAFE_FLAGS=1` (`--tools`, `--allowedTools`, `--mcp-config`, `--permission-mode`, `--dangerously-skip-permissions`, `--settings`,
`--setting-sources`, `--add-dir`, `--agents`, `--plugin-dir` …); `-c`, `-r` and `--session-id` stay allowed. `--minigw-ro` (also a folder
saved read-only) means `read` for every model that is not certainly a gateway agent, also when no `--model` is given: the default model then
comes from the `model` key of your Claude settings (measured: `{"model":"haiku"}` in a project's `.claude/settings.json` ran claude-haiku-5-5 without `--model`),
and that key can name a provider model, so minigw cannot know it. The restricted run loads no settings, so an interactive `minigw --minigw-ro`
without `--model` uses Claude Code's built-in default model, with read tools only and without your own settings and hooks; name `--model` to choose. Agent models
(`claude-cli-…`, `claude-codex…`, `claude-grok…`, `claude-copilot…`, exact names as the gateway uses them) are limited on the gateway:
`--minigw-tools` with one is an error (use `--minigw-ro`). `ANTHROPIC_MODEL` does not name the model (minigw does not pass it to claude).
Inside a gateway agent (`MINIGW_AGENT_URL`) a provider model is asked directly and gets no tools at all, so the flag is accepted and has nothing
to restrict. A prompt with `@` before a word
(`@Test` in a diff) is refused inside Claude Code; pipe it on stdin instead (`git diff | minigw -p --model <model>`, Claude Code 2.1.299 or newer).

Leaving `--minigw-ro` out is not prompted: a model call without it inside Claude Code is pre-approved like one with it,
so the agent may write (in worktree mode only on its branch).

**The gateway's cap per agent:** the gateway's owner can set, per agent, the highest mode in which it may change your
folder (Settings → the agent → **Changes in the client's folder**; admin API `work`). In a mode above it the agent
still runs, but only reads the folder, and minigw prints for example `minigw: gateway: Mini Gateway lets Codex change
files only in worktree mode, so it only read the folder (in a git repository, run minigw --minigw-work=worktree)`. Run again with that
mode to let it change files.

**Role** (`--minigw-role NAME` or `MINIGW_ROLE`, 1–32 letters, digits, `. _ -`): a label the gateway records in its
session history, for example `security` in a subagent's environment. It is a guard against mistakes, not a security
boundary.

**Usage labels** (`--minigw-phase`, `--minigw-activity`, `--minigw-task`, or `MINIGW_PHASE`, `MINIGW_ACTIVITY`, `MINIGW_TASK`): say what a run was for, so
the quota can be read by type of work. The phase is plan, do, check or act (act: correcting the plan because of a new state found while
doing or checking); the activity is the kind of work: feature, bugfix, security, refactor, docs, test, release, research, ops; the task is
a short reference such as an issue number or a branch. All three are optional and none ever refuses or delays a run: a value outside the
suggested words is kept (trimmed; phase and activity are also lower-cased) when it is a short token (phase and activity: letters, digits and `. _ -`, up to 32; task: also
`#` and `/`, up to 64); anything else is dropped with one warning line and the run goes on. In a folder linked to a project and inside a git
repository, an empty task becomes the current branch name (marked `branch`; the branch is read from the repository's files, nothing runs).
A role such as `plan.feature` is not parsed into labels. The labels are data, not permissions. Read them with `minigw gw usage --by phase`
(also `activity`, `task`, `project`, `model`, or two of them: `--by project,phase`), filter with `--phase`, `--activity`, `--task` on
`gw usage`, `gw usage-events`, `gw project-sessions` and `minigw project sessions`. Usage without a label is its own row, `(unlabelled)`.
A phase or activity that a model chose (off by default; see below) is a row of its own, `do (model)`
next to `do`; `--label-source given|branch|model` on `gw usage` and `gw usage-events` keeps the events that have a label from that source. A label a
caller gave is never replaced by a model's, and a label never gives access to anything.
Totals older than 400 days are folded without labels. A topic project is the place for a task of its own: link its subfolder and the branch
names the task.

**Model labels (beta: Windows gateway with Foundry Local, Mac with Apple Foundation Models).** The owner can enable Foundry Local, choose an installed model,
then use the owner admin API `PUT /usage/labels/settings` with `{"backend":"foundry:<model>","enabled":true}`.
The Model labels pane edits the same settings; the Windows/Linux gateway has no native panel renderer yet.
Nothing runs by default. The settings route of this gateway accepts only `foundry:<model>` (the panel's "Supported backends" line says so) and refuses `apple-fm`,
which is the Mac app's. The Linux worker reports an unavailable backend in `lastError`; the Mac is described below.
Only root `/v1/messages` events are considered. The first user message, with system reminders stripped, plus the task/branch
label if present, is capped to 2000 Unicode code points and sent directly to the selected Foundry Local model on this computer.
No remote provider is used. The classifier never chooses a task. Its input is kept only in memory, never in the database,
audit trail, logs or error text. The local model runtime’s own logging and operating-system crash dumps are outside
the gateway’s control. Attached Read content is retained as in the on-device chat adapter.

Phase and activity each use three samples at temperature 0.7. The default threshold `1.0` needs all three to agree;
`0.67` accepts two of three (stored as 667 per mille). These scores mean **agreement**, not calibrated confidence or probability.
An invalid answer casts no vote; insufficient agreement leaves that label empty. Explicit labels always win.
The background worker starts after dispatch, runs one decision at a time, queues at most eight inputs, and allows 20 seconds per attempt
for both labels together, including discovery and waiting for Foundry to become idle. HTTP requests are enqueued only
after the upstream HTTP call has been sent and returned response headers (or an error). Enqueue extracts and copies only
the capped text plus its hash; it never retains the request-body buffer. The eight queued inputs retain at most 2000
Unicode code points each; admission also keeps the existing 8 MiB budget on their original request sizes.
An oversized body or a full queue skips classification.
Foreground Foundry requests have priority from discovery through the end of their response. Classification starts each
sample, including any model loading, only while this gateway’s Foundry is idle. A foreground arrival cancels the sample
without waiting for the runtime to stop; the decision is retried at most once, then left unknown. Each attempt counts
toward `maxPerHour` (120 by default) in a rolling hour; reaching the rate limit skips work. Cancellation is cooperative:
the gateway cannot force the local runtime to stop inference or model loading immediately. Other applications using that
runtime are outside this coordination. Settings changes invalidate pending results inside the storage transaction,
including shadow counters.
Up to 500 decisions are reused for six hours for the same device, first message, task and settings. Classification failures
leave the user's call and its labels alone; `lastError` contains only an error class.

With `shadow` enabled (the default), every fourth root call carrying explicit labels is compared with the model;
`shadowRate` changes that interval. That sampled call only updates agreement counters for its explicit labels, never labels.
The owner can read the counters at `GET /usage/labels/agreement` and in the Model Labels panel (kind, the agreement of the model,
agree %, number of checks and the day range of the last 30 days); unknown decisions count as disagreements. Agreement is not calibrated
accuracy: the explicit labels are a biased sample (scripted runs) and a model that does not decide counts as wrong. The panel says so, and
it is there to choose the threshold, not to prove the labels right.
A decision using the model records one internal usage event (`gateway`, `ops`, `usage-labels`), with estimated tokens and
no user, so it consumes no user's quota and never classifies itself. Cache hits create no model usage event.

**Model labels on the Mac (on-device model).** The Mac app's worker uses Apple Foundation Models, the on-device model of the gateway Mac:
choose `apple-fm` as the backend (Model labels pane, or `PUT /usage/labels/settings` with `{"backend":"apple-fm","enabled":true}`). The Mac
settings route refuses any other backend with "choose apple-fm". Nothing leaves the Mac: the first prompt is classified in the app by the
on-device model, directly (never through the gateway's own `/v1/messages` route), with no network call and no other provider. If the model
cannot be used (macOS or Apple Intelligence off, model still downloading) the worker does nothing and `lastError` says "on-device model
unavailable"; it never falls back to another model. Everything else is as above: same input and cap, same three samples (the answers are
limited to the list of phases or activities by guided generation, and still parsed strictly), same thresholds and stored scores (667 or
1000 per mille), same shadow counters and cache, one decision at a time, a queue of 8, 20 seconds per decision, `maxPerHour`. The worker
starts only after the request was dispatched and never delays it. It also yields to the user: while a request for an on-device model
(`claude-apple-fm`) runs through this gateway no sample starts, a sample that is running is cancelled, and the decision is retried at most
once, then left unknown. The worker stops when the app quits: no helper call starts after that. An attempt that a user's request interrupted after inference began is recorded as a cancelled internal usage event. A stored backend this Mac cannot run (for example `foundry:…` from an older version) cannot be turned on; the row shows it under "Backend problem" and `lastError` says so. **What *agreement* means:** the share of three independent samples that gave
the same answer; it says how steady the small model is, not how often it is right (that is what the shadow counters, compared with explicit
labels, are for). **Privacy limit:** the gateway never stores, logs or reports the classified text, but the on-device model runtime and the
operating system (their own logs, crash dumps, Apple's diagnostics) are outside the gateway's control.

**Check the model against your own labels: `minigw gw labels eval --file <examples.jsonl> [--show-errors]`.** A local owner command for the
gateway computer (the model is there). The file holds hand-labelled examples, one JSON object per line: `{"text": "...", "phase": "do",
"activity": "bugfix"}` (`phase` is plan, do, check or act, `activity` one of the configured list; a line needs at least one of them; an
unknown key is refused; blank lines are skipped). Limits: the file at most 1 MiB, at most 500 lines, each text capped to 2000 Unicode code
points as the worker caps it. Each example goes through the same decision the worker makes (the configured backend, three samples for the
phase and for the activity, a user's own requests keep priority exactly as for the worker: no sample starts while one runs, a sample that
one ends is tried again, up to three times). The command prints, for the phase and for the activity, how many examples the model decided
(coverage) and how many of those it got right (accuracy) when all three samples agree and when two of three do (the threshold `0.67`),
and the most frequent mistakes (hand label -> model). It needs a backend chosen, not the feature turned on, so you can measure before you
trust. **It stores nothing:** no label, no agreement counter and no text. The only record is the worker's own accounting rule, ONE
internal usage event for the whole run (no user, device `gateway`, activity `ops`, task `usage-labels`, source `internal`, estimated
tokens), so the model time shows in the reports. The example text is held in memory for the run, never logged and never in an answer: the
gateway returns numbers and the position of each miss, and the command shows a miss's text, from its own copy of the file, only with
`--show-errors` and only on this terminal. One evaluation runs at a time (a second is refused with a clear text); a run stops after 30
minutes, the examples not reached are counted as not run. The route is `POST /usage/labels/eval` of the owner's admin API
(see [admin-api.md](admin-api.md)); a member, an agent run or the remote settings page cannot reach it. Like the agreement, the numbers are
agreement with your labels on those examples, not calibrated accuracy.

`MINIGW_SHARE=0` turns folder sharing off; `MINIGW_SHARE=1` shares a folder that is not shared automatically, for
one run.

## Which folders are shared

Links inside a shared folder work when they stay inside it: relative and absolute symbolic links and, on Windows,
directory junctions (also with an absolute target, as pnpm and `npm link` create them). A link or junction that
leads outside the shared folder is refused ("links that lead outside the shared folder are not followed"). Links
into protected paths such as `.claude/hooks` or `.git` stay read-only for the agent. A link whose target is on another
drive or a network path (`\\host\share`) is outside and refused without the target being opened (no connection to that
host). The user’s linked
`.gitignore` is still read.

minigw shares the current folder with a gateway on another computer when it gets its first request for an agent
model. Any project folder is shared, on any drive (for example `D:\Projects\app` or `/Volumes/Disk/app`).

**Not shared automatically:** a drive or volume root, the home folder itself, system folders (`C:\Windows`,
`Program Files`, `/usr`, `/System` …), other users' home folders, `~/Library`, `AppData`, and hidden folders such as
`~/.ssh`. In a temporary folder (`/tmp/project`, `mktemp -d`, `%TEMP%\project`) a project folder is shared, but not
the temporary folder itself, hidden folders in it, other users' folders, or Claude Code's top temporary levels (the
session's `scratchpad` is shared).

To share such a folder anyway:

```sh
minigw share add            # this folder (or: minigw share add <folder>); /minigw:share does the same
minigw share list
minigw share remove
```

Adding an alias of an existing shared folder keeps one entry and updates its saved options.
To remove it, use the saved name shown by `minigw share list`. An unstored alias or other name that is not
shared reports `not shared`, exits non-zero, and leaves the list unchanged.

`minigw share add` always refuses a drive root, the root of the volume that holds your home folder or the system
(`/System/Volumes/Data` on macOS; on Linux a mount of the same file system as home or `/` that shows it from its top,
such as a btrfs pool next to `@home`; on Windows a folder mount of the system or home volume), your home folder and any
folder that contains it (`/Users`, `/home`, `C:\Users`). Another disk (`/Volumes/<Disk>`, a Windows folder mount of
another volume), a devcontainer `/workspaces/<project>` or a Docker volume can be shared. minigw checks the folder
itself, not its name, so a link, junction, 8.3 short name or macOS alias such as `/System/Volumes/Data/Users/<you>` is
refused too; a folder it cannot check is refused (on Linux this includes every folder when `/proc/self/mountinfo`
cannot be read). minigw checks again each time it shares: a current folder that is a link to your home folder prints
"minigw: not sharing … (…)" and is not shared, even with `MINIGW_SHARE=1`; a folder saved by an older version that is
now refused prints `minigw: not sharing … (…); remove it with: minigw share remove "…"` and is not shared, and
`minigw share list` marks it "not shared". The share list is per computer: with several profiles every gateway gets
the folder on its first agent request.

WebDAV refuses links that leave the shared folder with HTTP 403 and a short reason, including entries in directory listings.

**Everything inside a shared folder is shared**, hidden files included (`.claude/`, `.env`, `.secret/`). Keep
credentials out of project folders you use with a gateway, or list them in `.gitignore` for sync and worktree mode.

If minigw prints that the folder "is not shared with the gateway", run `minigw share add` or `/minigw:share`.
If it prints "not sharing …: this drive does not report real paths (network or virtual drive)" (Windows), the folder is on a
network or virtual drive that minigw cannot check safely: share a folder on a local disk instead.

## Projects and notes

A **project** is a named place on the gateway where a team keeps short notes (design decisions, structure, data
facts) that people and agents can read. The gateway owner creates projects, adds you as a member and binds your
computer's device key to you; you see projects you belong to or can read through explicit grants. Works on every gateway: `minigw serve` and the Mac app (project runs of agents on the Mac app included).

A client on the gateway computer itself (next to `minigw serve`, or the Mac app) works over loopback and is accepted for
models without being on the gateway's device list, but projects need both: the owner allows the device with
`minigw gw devices allow DEVICE-ID` and binds it to a user with `minigw gw bind DEVICE-ID USER`. A refusal says
which of the two is missing, and `minigw status` shows it on the `Device:` line.

### Markdown copies

With the gateway running, members can copy published notes to their own computer:

```sh
minigw kb export fakture ./knowledge
minigw kb import fakture ./knowledge
minigw kb import fakture ./proposal.md --class structure --submit
```

Export waits and retries if reads hit the gateway rate limit. It writes one Markdown file per readable note
and an `INDEX.md`. It includes readable private notes,
and excludes pending submissions and notes hidden from the caller. Use a new or empty destination directory: existing files
are never overwritten, and the destination must not be a symbolic link. Each file has `---` front matter with JSON string values for `id`, `title`, `class`,
`kind` (the provenance source kind), `revision`, `author`, `created`, `updated`, `hash` and `origin` (the gateway and project the file came from), followed by
the exact body. No YAML features are interpreted. Importing a file back into the gateway and project it was exported from updates that same note
(an unchanged file is skipped; a note changed here since the export is kept and reported); a file from anywhere else, or one naming a note you may
not write, is imported as a new note. Files exported before `origin` existed import as new notes.

Import accepts a directory tree or one `.md` file; front matter is optional and directory imports ignore
`INDEX.md`. Without a title, the file name supplies it. `--class` overrides front matter; the default is
`data` when neither supplies a class. The caller needs the same create permission as `note push`;
readers cannot import structure or data. `--submit` uses submission permissions and pending limits.
Imported revisions use source kind `import` and the file name as source reference. Neither command is
available inside an agent run.

The stable key is the front matter ID, or the relative path when no ID exists. Keep that ID or path when
repeating an import. The hash covers title and body, and is recomputed on import. An unchanged key and hash
skips; changed content updates the existing note or pending submission, even after 24 hours.
Acceptance moves the receipt to the published note. Re-import never creates a second note for that key.
A target edited locally is skipped and reported; resolve it yourself before trying again.
Large member imports still use the normal 60 writes per minute limit: the client waits for the retry interval
and continues when limited. Durable receipts also make repeating after an interrupted command safe. These are
one-way copies, with no continuous sync.

### Team directory

The owner groups users and projects into teams. Users can join several teams; a project belongs to at most
one team. **Team membership alone grants no project role or access to notes.** Existing users, projects,
memberships and grants stay unchanged. Keep assigning project roles with `gw member` and sharing classes
with `gw grant`.

```sh
minigw gw team add Engineering
minigw gw team member add Engineering ana
minigw gw team project add Engineering fakture
minigw gw team access set Engineering fakture ana developer
minigw gw team access list Engineering
minigw gw member fakture list --explain
minigw project list
minigw team list
minigw team show Engineering
```

`minigw gw team archive <team>` and `unarchive <team>` switch a team off and on. To move a project to another team, remove it from
its team first (`minigw gw team project remove <team> <project>`, also for an archived team); adding a project that another team
still holds is refused and names that team.

To copy one team and its knowledge to another gateway, the owner runs these commands on the gateway
computer, with the gateway running (the same commands work on Windows, Linux, and the Mac app):

```sh
minigw gw export --team Engineering --project fakture --with-private ./engineering.zip
minigw gw import --source engineering ./engineering.zip --dry-run --map ana=Ana
minigw gw import --source engineering ./engineering.zip --map ana=Ana
minigw gw import --source engineering ./engineering.zip --adopt project:shop,user:alice   # only after reading the conflict list
```

Omit `--project` to include every project in that team; repeat it for a subset. Private notes require
`--with-private`. The zip contains a versioned manifest, Markdown files and `INDEX.md`. It copies member
names, direct memberships, team assignments and grants between included projects, together with published
notes and provenance. It excludes devices, bindings, tokens, keys, usage, audit history, sessions and pending
submissions. Note bodies cannot define access or accounts.
It also carries the team's path label, the registered git identities of each project and the kind of each note.
On import the identities are added to those already registered (at most 8 per project); the path label is set only
when the team is new or has no label (otherwise the dry run says `skip team path`). A bundle made by 0.7.11 imports
unchanged; an older gateway refuses a bundle that carries a team path or identities.

`--source <name>` is required on import: it names where this bundle comes from, on this gateway, and the
owner chooses it (1–64 characters, using team-name validation). The first import under a source records the
bundle's origin gateway; a later import under the same source from a different gateway is refused, as is a
bundle that was exported by this gateway itself. Receipts are kept per source name, project slug and source
note id; the `origin_gateway` string inside the bundle never establishes a namespace. A dry run reports the
source name and the origin it records.
The owner must choose a source only for an archive they trust: the recorded origin is a consistency check,
not cryptographic authentication of the exporting gateway.

Import validates the entire zip before writing, within fixed limits on its size, entry count, central
directory, manifest size and nesting, note file size, and collection sizes. A note file that is too large, and any entry the
manifest does not list, is refused from the archive's metadata before its body is decompressed. Dry-run
reports create, merge, skip and refuse decisions and writes nothing. Real import is one transaction,
including one `kb.import` audit event when anything changes. Names match case-insensitively after `--map
old=new` and after Unicode normalization, so a name written with a combining accent finds the same
account as its composed form. If two target accounts normalize to one name, or two bundle users would land
on one target account, the import is refused and both are named. Slugs match projects and names match
teams. Unknown users are inactive non-owners with no device
binding. The owner must activate and bind them explicitly. A project already on another team refuses the whole import.

An import merges only into objects that the same source created before (the gateway records them per source).
A project, team or user that the bundle names and that already exists here but was not created by this source is a
**conflict**: the import stops before it writes anything and lists each conflict, for example `project:shop` and
`user:alice`. Import only bundles you trust: to merge into such objects anyway, repeat the import with
`--adopt project:<slug>,team:<name>,user:<name>`, listing only the objects you mean (also `--adopt` twice). Nothing is
adopted implicitly, and a user is never adopted just because its project or team is: each user must be listed.
`--dry-run` shows the same list, and with `--adopt` the lines the adoption would produce (`merge adopt project:shop`, then
the team, member and role lines). An adopted object keeps its local roles: the bundle may add a role where there is
none, never raise one, and grants from an adopted project are added only when that project is listed in `--adopt`
again. The adoption is remembered for the source, so a later re-import from the same source needs no flag for the
objects; the `kb.import` audit row names every adopted object (`adopted`). An `--adopt` entry that matches no object
the bundle names is refused. If the owner renames an object that an import created (or adopted) locally, the source no
longer recognizes it: the bundle's old name then creates a new object, or is a conflict if that name exists here. Bundles
imported by an earlier version left no record: their objects show up as conflicts once and need one `--adopt`. Target project and note IDs
are allocated locally.

Re-importing the same bundle is a no-op. The receipt stores the note revision and class written by the last
import: any local change, including a class change, bumps the revision, so a locally changed note is skipped
and reported and is never overwritten, even when the incoming content differs. An incoming class change is
applied only to a note that was not changed locally. The imported revision records the owner's import as its
writer — actor kind `import`, no user id, this project, source kind `import`, source reference
`<source>/<project slug>/<source note id>`, the receive time, and no reviewer. Everything the bundle says
about the note's history (author, source kind, times, reviewer, its own provenance) is stored separately as
unverified source metadata and is shown by `note read --json` under `unverifiedSource`; it never drives
access, permissions or review. A pending local submission for the same source entry is skipped and
reported, never superseded by the import.
The mapped author name is retained for display, without own-note permissions. Imported private notes are
therefore visible to project administrators, and list and search apply the same restriction. Member folder
imports open files beneath a held directory, reject links, and stop at 10,000 total entries or 32 path
components, including empty directories and non-Markdown entries. Each file must be smaller than 96 KiB.
This copies knowledge to a different gateway; it does not restore a database or delete omitted target rows.

Members see only their own teams, their teammates' names, and attached projects they can already read through
project membership or explicit grants. Missing and hidden teams give the same answer. Devices must be approved
and bound to active users. `team list --archived` includes archived teams; member list and show accept `--json`.
Names or numeric IDs identify teams; use the ID for an all-digit name.

`gw team rename <team> <name>` renames a team. Archiving a team hides it from the default member list,
blocks new attachments and deletes its assignments; unarchiving does not bring them back (deliberate: set them again with `gw team access set`). Removing a team
member or project deletes the matching assignments in the same transaction. Archiving a team does not
archive its projects. Explicit grants remain unchanged.

A team assignment creates the same project membership as a direct one; the strongest wins
(admin > developer > reader); removing a team assignment does not remove a direct one.
`gw member` changes only the direct origin. Its output reports a stronger surviving team role.
`gw member <project> list --explain` shows every origin and role; `project list` shows the effective role
and origin names. Removing or lowering an origin invalidates existing runs when effective access narrows.
Team assignments require both the user and the project to belong to that active team.

For three people in two projects, attach both projects to Engineering, add ana, bor and eva as team
members, then assign ana and bor developer and eva reader in each project with `gw team access set`.
Team membership alone grants nothing. To remove only eva's team role in fakture, run
`minigw gw team access remove Engineering fakture eva`, then inspect `gw member fakture list --explain`
for any surviving direct access. Note classes and explicit one-hop grants behave exactly as for direct roles.


### Projects in 10 minutes

The commands are the same on a Mac gateway (the app) and on `minigw serve` (Windows, Linux). Steps 1 to 4 are the
owner's, on the gateway computer; steps 5 to 8 are on the client of a member. A one-page guide for a team that starts on the shared knowledge base: [team-onboarding.md](team-onboarding.md).

```sh
# 1. on the gateway computer: create a user (the owner's own user exists already, named after the OS account)
minigw gw user add ana
# 2. bind a device that is on the approved list to the user (the ID is in `minigw gw devices`)
minigw gw devices
minigw gw bind <device id> ana
# 3. create the project and 4. add the member (roles: reader, developer, admin)
minigw gw project add fakture "FAKTURE importer"
minigw gw member fakture set ana developer

# 5. on Ana's computer: link the folder to the project (you must be a direct member)
cd ~/src/fakture && minigw project link fakture
# 6. push a note, read it back
minigw note push --class structure notes.md
minigw note list
# 7. start the agent in this folder: it can read the project's notes (never `private`) with `minigw kb search` or `minigw note list|read`
minigw -p --model claude-codex "Read the project notes and explain how the importer is laid out"
# 8. usage: Ana sees her own events, the owner sees everything
minigw usage --days 7
minigw gw usage-events --project fakture            # on the gateway computer
minigw gw usage --days 30                           # on the gateway computer: requests, tokens and cost per provider and model (--csv for a spreadsheet)
```

The provider/model usage report also shows failed agent time: the minutes between dispatch and the end of requests that
ended in an error (a hung provider, a stream that broke after HTTP 200, an agent that failed or hit the 30 minute limit of `minigw serve`).
It appears as a "failed min" column when any model has some and as "7 errors, 58.0 failed agent minutes" on the total line;
`--sort failed` lists the models with the most failed time first. **Summed agent minutes are concurrent work time, not
clock time**: 31 requests that each hang for 8 minutes at the same time are 250 agent minutes inside about 8 minutes of
clock. Requests recorded before 0.6.7 and runs cut off by a crash have an unknown duration; they are counted separately,
never as zero. The Usage views of the Mac app and the browser Settings have a "Failed time" measure.

The provider/model usage report shows reported USD charges plain and estimated charges with `~` (for example
`~0.42`). Mixed totals show `0.96 + ~0.42`; unknown costs retain their count. CSV exports have separate
`reported_cost_usd` and `estimated_cost_usd` columns. Claude, Codex, Grok and Copilot subscription calls have no money.

Prompt-cache reads are part of the report. When any model read from the provider's prompt cache, the model table gains
a "cached in" column after "tokens in" (the sum of cache-read input tokens; a row without cache data shows 0), the
per-day table gains the same column, and the total line says for example
`..., 1K tokens in, 900K cached in, 200 tokens out, ...`. This matters for providers with automatic prompt caching,
where about 95% of the input is a cache read, so "tokens in" alone hides most of the traffic. CSV rows end with
`cache_read_tokens` and `cache_write_tokens` (existing columns do not move); `--json` carries `cacheReadTokens` and
`cacheWriteTokens`. Requests without cache data print as before. Cache counts of events folded into the daily totals
after the 400-day retention are not kept (the daily table has no cache columns), so such rows report 0.

Prices refresh in the background with a timeout and one refresh per provider at a time; failures keep the last good
catalog. OpenRouter prices come from its `/v1/models` list, LiteLLM prices from the owner's own proxy (`/model/info`; complete input and
output rates only), both USD per token. The gateway calls no other host for prices; a LiteLLM model without rates there has unknown cost.
Aliases use the routed upstream model's price. A snapshot
is frozen at dispatch and retained in the shared core database (migration 6); replay and later refreshes never
reprice a completed event. Snapshots include source and fetch time; last-good prices can be stale.
A request never waits for a price: a stale catalog is used as it is and refreshed in the background. Only the first
request for a model that is not an alias and not yet in any list waits for the model list, at most 2 s, as before. A model
without a price at dispatch gets unknown cost (never zero), and such an event is not repriced later.

Estimates currently cover ordinary input/output tokens only. Cache read/write counts are retained when reported. Cache token usage, images, audio, tools and complex
tiers are not priced; unsupported price dimensions or incomplete token counts leave cost unknown rather than zero.
Reported costs still win, including reported zero. Legacy `/usage` counters and Clear History are unchanged.

Back up the project database before you change things, and restore it with the gateway stopped:

```sh
minigw gw core backup before-upgrade                # gateway running; writes <core>/backups/before-upgrade
# gateway stopped (Mac: the app quit):
minigw gw core restore before-upgrade
```

A database backup is the wrong tool for copying a team to another gateway. It belongs to its original
gateway and includes bindings, credentials-related state, usage, audit and submissions.
Use `gw export` / `gw import` for a portable knowledge bundle while the gateway is running.
A zip bundle is not a database and must not be passed to `core restore`. Backup and restore behavior is unchanged.

What differs on the Mac app:

- **Restore needs the app quit.** The Mac `minigw` then starts the helper `minigw-core restore` itself, which runs the
  same restore as `minigw serve`. With the app running it refuses. On Windows and Linux stop the gateway first.
- **The helper.** The Mac app keeps the database in the helper `minigw-core`. While it is not running, project routes
  and every request that carries a project answer `503 core_unavailable`; plain model requests work as before.
  `minigw gw status` and Settings → General say whether the project database works.
- **Admin panes.** The Mac app's Settings window has the four admin panes (Users and devices, Teams and projects,
  Knowledge base, Upstream gateways); the `minigw serve` settings page draws the same four. `minigw gw …` still does everything on both gateways.
- **Agents.** On the Mac the Claude CLI agent reads notes only with **Allow local tools**, Codex only with its network
  switch on ([gateway.md](gateway.md#the-mac-app-agents-with-a-project)). A Windows or Linux gateway without a sandbox
  keeps projects off until the owner accepts that agents can read all notes
  ([gateway.md](gateway.md#minigw-serve-on-windows-and-linux)).

```
minigw project list                     your projects: your role, and what a grant gives you
minigw project link fakture             link this folder (and everything under it) to the project
minigw project link                     no slug: find the project by this folder's git remote
minigw project show                     which link applies here and why
minigw project unlink
minigw project remote list|add|remove   the git remotes registered for a project
minigw project list --path acme         only the projects of your teams whose path label is "acme" or starts with "acme/"
```

- **Link by git remote.** A project admin (or the owner) registers the git remotes of a project:
  `minigw project remote add --here` in a checkout, or `minigw project remote add <url> [<slug>]`
  (`list` shows them, `remove <url or identity as listed>` removes one; at most 8 per project). The gateway keeps only
  the identity `host/path` (lower case, without credentials, port and `.git`): `git@GitLab.example.com:Acme/Shared.git`
  and `https://tok:x@gitlab.example.com:8443/acme/shared` are the same. Then `minigw project link` without a slug reads
  this folder's `origin` (the value in the repository's own git config, without `insteadOf` rewriting), removes any
  user name and password from it on your computer, and links the project if exactly one of your projects
  (direct member, not archived) is registered for it. Zero or several matches are refused with the projects named;
  then link by slug. The gateway never sees folder paths and never stores the URL you typed. The same identity may be
  registered for several projects (a monorepo); then only people in one of them link without a slug.
- **Team path label.** The owner may give a team a free-text label such as `acme/backend`:
  `minigw gw team path <team> <label>` (`-` clears it). `minigw project list --path acme` lists your visible projects
  that belong to a team of yours whose label is `acme` or starts with `acme/`. A label groups and filters only;
  it never gives anyone access.

- **Link.** The link is stored on your computer, not in the repository: per gateway, with the real path of the
  folder. A clone, a git worktree, a copied or a moved folder has no link until you link it. Inside a linked folder
  you may link a deeper folder to another project; the nearest linked folder wins. You can link only a project
  you are a **direct member** of: a grant lets you read another project's notes, not work in it.
  The commands and the link file are the same on every system: the Mac `minigw` keeps it in
  `~/Library/Application Support/minigw/projects.json` (Windows and Linux: `minigw/projects.json` in the user's config folder),
  in the same format.
- **Hint file.** A file `.minigw-project` with one line, the project's slug, may be committed to the repository. It
  never links anything: without a link `minigw project show` and the note commands only suggest
  `minigw project link <slug>`. If a linked folder has a hint with another slug, the note commands refuse until you
  unlink, link again or fix the hint.
- **Notes.** They use the project of the current folder (`--project <slug>` only confirms that it is the linked one):

```
minigw note list [--class data] [--limit 20]      newest first; --json for scripts
minigw note read 12                              by A, last changed by B  when someone other than the author edited it (list: "(edited: B)")
minigw note push --class structure notes.md       "-" reads standard input; the title is the first line
minigw note edit 12 --title "New title" notes.md  reads the current revision first (or --revision n)
minigw note rm 12
```

`note push` is safe to repeat after a lost answer (it uses an idempotency key and creates the note once); if no
answer ever arrives it prints the key for `--key`. A note is text up to 64 KiB, a title up to 256 bytes; limits per
project and per minute apply.

| Class | Who reads it | Who changes it |
|---|---|---|
| `structure` | every member, and members of projects with a `structure` grant | developers (own notes), admins |
| `data` | developers and admins, and members of projects with a `data` grant | developers (own notes), admins |
| `private` | the author and the project admins | the author (developers), admins |

`private` = visible to you and the project admins; no grant ever includes it. Only the gateway owner, using owner authentication, manages members and
grants with `minigw gw member` and `minigw gw grant` (see [gateway.md](gateway.md)). A project admin role or a user’s owner flag does not authorize these owner API commands. They are not
available inside an agent run, except `kb search`, `note list` and `note read`: when you start `minigw` in a linked folder, the agent run can read that project's notes (never `private` ones) and nothing of your other projects. If the gateway changes who may see what, the next read of a running agent fails; see [how-it-works.md](how-it-works.md#projects-notes-and-agents-minigw-serve-and-the-mac-app-060).

### Three people, two projects

Use this recipe on either gateway. The gateway must already approve the three devices; get their exact IDs
from `minigw gw devices`. Replace `ANA_DEVICE`, `BOR_DEVICE`, and `EVA_DEVICE` below with those IDs.
Owner commands run on the gateway computer with its owner authentication, not with a project admin role.

A: two developers and one reader share FAKTURE. B: the same people also work in modules, which receives only
FAKTURE's `structure` notes. The owner runs:

```sh
minigw gw user add ana
minigw gw user add bor
minigw gw user add eva
minigw gw bind ANA_DEVICE ana
minigw gw bind BOR_DEVICE bor
minigw gw bind EVA_DEVICE eva
minigw gw project add fakture FAKTURE
minigw gw project add modules Modules
minigw gw member fakture set ana developer
minigw gw member fakture set bor developer
minigw gw member fakture set eva reader
minigw gw member modules set ana developer
minigw gw member modules set bor developer
minigw gw member modules set eva reader
minigw gw grant fakture add modules structure
```

Each member uses their own approved, bound client. On Ana's client:

```sh
cd ~/src/fakture
minigw project link fakture
printf '# Schema\nSanitized table and relationship descriptions.\n' > schema.md
minigw note push --class structure schema.md
printf '# Findings\nSanitized developer findings, without customer records.\n' > findings.md
minigw note push --class data findings.md
printf '# Plans\nRestricted author plans.\n' > plans.md
minigw note push --class private plans.md
minigw note list
```

On Bor's client:

```sh
cd ~/src/fakture
minigw project link fakture
minigw note list
minigw note read STRUCTURE_ID
minigw note read DATA_ID
```

On Eva's client:

```sh
cd ~/src/fakture
minigw project link fakture
minigw note list
minigw note read STRUCTURE_ID
```

Replace `STRUCTURE_ID` and `DATA_ID` with IDs printed by push/list. Bor can read shared structure and data;
Eva sees structure only. Neither sees Ana's private note. Project admins can read private notes.

For B, each member links their modules folder on their own client:

```sh
cd ~/src/modules
minigw project link modules
minigw note list
minigw note read STRUCTURE_ID
```

Member note commands list/read the linked project's notes, so this last list shows modules notes, not the
granted FAKTURE notes; that read of a FAKTURE ID returns the same 404 as a missing note. To read FAKTURE as
a member, use the linked FAKTURE folder above. A called agent started in the linked modules folder can use
`minigw note list` and `minigw note read STRUCTURE_ID` to read both modules notes and the explicitly granted
FAKTURE structure notes. Its scope never includes private notes or unrelated memberships.

A grant alone does not allow linking a folder. Grants are one hop, never transitive. A `data` grant would let
**every** modules member, including Eva, read FAKTURE data through that scope; this recipe grants only structure.
The `data` label never permits storing customer records, secrets or personal data.

### Your model usage

```
minigw usage                      this folder's project (or, with no link, your own events in all projects), last 7 days
minigw usage --project fakture --days 30
minigw usage --json
```

One line per day, model and user, with the totals. A developer sees the events of the project that are their own, a
project admin sees all of them, readers and people who only have a grant see nothing (the project's usage is not
visible to them). Without a project you see your own events over all your projects. Lost-event records are not shown
here (they are gateway-wide, not the project's): only the owner's `gw usage-events` shows `N events lost between A and B`.

### Project session history

```sh
minigw project sessions                       # linked project; a link is required
minigw project sessions fakture --days 30 --model claude-cli-haiku
minigw project sessions 7 --device DEVICE_ID --json
minigw gw project-sessions --project fakture --days 30 --json
```

A session is one recorded top-level request and its transitive nested requests. Its ID is the root event ID.
Repeated requests in the same conversation remain separate sessions: usage events do not store the client session
string. History contains metadata only, with no prompts, responses, transcripts, folder paths or previews.
It is separate from the older `minigw gw sessions` listing, detail and clearing commands.

Readers and developers see their own recorded user IDs in the selected project; project admins see the whole project.
The owner command requires `--project` and authenticated owner access and sees all project sessions. An owner marker
on a user does not expand the member command's role. Grants and agent run capabilities do not authorize this history.
Device rebinding does not change historical users. User and device names are current labels; a removed device displays
its recorded ID. The member route does not expose the device directory.

Days default to 7, accept 1–400 and use UTC calendar days. Selection uses the root's start. Descendants outside that
period still contribute. Model IDs match exactly: a matching event selects the entire chain, including other models.
The device filter matches the root's recorded device ID. Both clients follow every API page, ordered by descending
root start then descending root ID. APIs accept `limit` (default 100, maximum 500) and an opaque `after` cursor.

Each event contributes once. Input, output, cache-read and cache-write counts remain separate. JSON metrics contain
`value` (null if entirely unknown) and `unknownEvents`; text uses `unknown` or `+?` for missing or partial counts.
Costs are exact decimal USD from recorded integer nanodollars, with reported and estimated subtotals and an
`eventsWithoutCost` count. Partial costs are known subtotals. Subscription and local costs may be unknown; they are
not reconstructed from current prices. Zero always means recorded zero.

Duration is chain wall time, latest end minus root start, rather than the sum of overlapping dispatch durations.
Interrupted or incomplete chains have unknown duration. Chains with any open event are excluded. Otherwise errors
and interruptions take precedence (`failed`), then cancellation (`cancelled`), then `ok`. A missing or invalid parent
produces an explicitly `incomplete` retained component, without revealing the parent ID or claiming complete totals.
Only 400 days of detail are retained; folded daily totals cannot recreate sessions. Only the window (one day more, so
a chain that began just before it is joined) and the ancestors of its events are read; other older events cost nothing,
however many a project holds. Corrupt cycles, arithmetic
overflow and traversal bounds (100,000 authorized events or 10,000 ancestry steps) return an error, never partial pages.
The Mac Settings Usage chart remains an aggregate report.

## Several gateways (profiles)

One client can use several gateways in the same session, for example a Mac and a Windows gateway:

```sh
minigw connect <mac-gateway> --name mini --token -  # each gateway is a profile; - reads the token from stdin
minigw connect <windows-gateway> --name win --token -
minigw profiles                                     # list; * marks the default
minigw profiles default mini                        # its models also appear without a suffix
minigw --model claude-cli-opus@win -p "…"           # <model>@<profile> runs on that gateway
minigw --minigw-profile win …                       # this command uses only that gateway
minigw disconnect --name win
```

- The connection you had before profiles is the profile `default`. Nothing changes while it is the only one.
- With several profiles, minigw runs a local proxy on `127.0.0.1` (with its own token) in front of Claude Code. It
  sends `<model>@<profile>` to that gateway without the suffix.
- `/model` and `minigw models` list every reachable gateway's models. One that cannot be reached is left out;
  `minigw status` shows why.
- Another gateway gets the folder on its first agent request, in the same work mode.
- Each profile has its own address, token and gateway key. The device key is one per client, so every gateway
  recognises the same device.
- A gateway got a new token (New Token, `minigw gw token rotate`): run `minigw reconnect [--name <profile>]` and paste
  it. The address and the device stay, so the device's approval and its session history on the gateway stay too.
  `minigw disconnect` also keeps the device key: connecting again later is the same device. Only
  `minigw uninstall --purge` removes it.

The plugin skills take `--profile <p>` or a model with `@<p>`, for example `/minigw:codex --profile win …`.

## Native mode and builds on the gateway Mac

Use native mode when your client works in an SMB mount of a gateway Mac's share (for example
`/Volumes/Java/projects` mounted from the gateway's `/Volumes/Java`). Agents then work in the gateway's own folder:
no mount back, no copy. `auto` picks it by itself.

```sh
minigw --minigw-work=native …                 # git repository: a branch per agent; otherwise in the folder
minigw --minigw-work=native-direct …          # always in the folder, one writing agent at a time
minigw share add --work=native                # save it as the default for this folder and its subfolders
minigw run -- mvn -q verify                   # run a build there; output and exit code stream back
```

- **Git repository:** each agent works in its own clone on branch `minigw/<agent>-<session>-<n>`, which starts from
  your state (uncommitted changes too). The branch appears in your repository; review and bring it in as in
  [worktree mode](#worktree-mode-results-as-branches). Grok, and Codex or Copilot without writing, are refused.
  Submodules and nested repositories get their own clone and branch as in
  [worktree mode](#worktree-mode-results-as-branches) (a nested repository without a commit too); each branch appears
  in its own repository. More than 32 nested repositories, an unusable name, a nested linked worktree or a git folder
  outside the share stop the run with the reason, ending in "use --minigw-work=native-direct".
- **In the folder itself** (`native-direct`, or a folder that is not a git repository): one writing agent at a time
  per folder. Others wait up to 5 minutes, then get an error. Agents that only read do not wait.
- **Unlike direct mode,** the agent can change every file, also `.git`, `.claude/settings*.json` and
  `.vscode/tasks.json`. Use sync where those must stay protected. Native cannot be read-only: `--minigw-ro` uses the
  read-only mount instead.
- **Requirements:** the folder is in a writable SMB share of the gateway Mac (System Settings → Sharing) and inside
  its allowed folders. minigw proves it is the same folder: the gateway writes a check file with random content and
  minigw must read it back through its mount. Shares listed in File Sharing work, and the automatic home-folder
  share of the gateway Mac's user (named after that user, for example `uros` = `/Users/uros`) unless it is turned off
  (`VirtualHomeShares` false in `com.apple.smb.server`, or SMB sharing off). In any share only allowed folders work,
  never the home folder itself, a hidden folder in it (`~/.ssh` …) or `~/Library`, also through a link and when the
  whole home is allowed (the same for `minigw run`). The gateway shares nothing itself.
- **Detection:** automatic on macOS, Linux (cifs) and Windows (UNC path or mapped drive). Elsewhere set
  `MINIGW_NATIVE_MOUNT=<share>=<mount folder>`. If the folder is not a gateway share, minigw falls back to the usual
  mount.
- **`minigw run`** is off by default. The gateway owner turns it on per device (Settings → General → minigw run).
  Only the listed commands run (default `mvn, swift, xcodebuild`), without a shell, in a sandbox. See
  [how-it-works.md → minigw run](how-it-works.md#minigw-run-sandbox). It uses the default profile, or
  `--minigw-profile P`.

Native mode and `minigw run` need a Mac gateway.

## Models

The gateway picks the provider by the model name.

| Model | Provider | Tools | Gateways |
|---|---|---|---|
| `claude-cli-opus`, `-sonnet`, `-haiku`, `-fable` (also `opus` …) | Claude Code on the gateway (`claude -p`) | yes | all (`minigw serve` does not list `-fable`) |
| `claude-chat-*` | the same, without tools | no | all |
| `claude-codex`, `claude-codex-<model>` | OpenAI Codex CLI (`codex exec`) | yes | all, off by default |
| `claude-grok`, `claude-grok-<model>` | xAI Grok Build CLI (`grok -p`); Windows and Linux gateways only, a Mac gateway answers "Grok is not supported on the Mac gateway" | yes | all, off by default |
| `claude-copilot`, `claude-copilot-<model>` | GitHub Copilot CLI (`copilot -p`) | yes | all, off by default |
| `claude-siri-ai` | Siri AI app | no | Mac (macOS 27) |
| `claude-apple-fm` | Apple Foundation Models, on-device | no | Mac |
| `claude-foundry-<model>` | Microsoft Foundry Local, on-device | no | all, off by default |
| `claude-<id>` | LiteLLM, OpenRouter or any Anthropic-compatible provider | passthrough | all |

**Claude CLI** runs your gateway's Claude Code as an agent in the session's folder. Sessions resume with `--resume`.
Tools are read-only by default; **Allow local tools** gives every Claude Code tool (commands, builds), with a table
to exclude single tools. File tools are refused outside the session folder and the allowed folders, and commands run
in Claude Code's sandbox (no reads of the home folder, writes only in these folders, network allowed). On a Windows
gateway the whole Claude Code runs in the Windows sandbox (see [Windows gateway](#windows-gateway-sandbox)) and runs
commands with its PowerShell tool; without the Windows sandbox commands need **Allow commands without a sandbox**.

The browser Settings page of `minigw serve` includes **Usage**: 7, 30 or 90 UTC days of tokens, spend or requests,
with daily model stacks and a Top models table. Spend separates reported and ~estimated USD and notes requests
without a known cost and recording gaps. Empty periods and API errors appear in the view.

The browser Settings page marks refused allowed folders with a warning and an explanation next to the path.
Remove and add the folder again after checking that it is the folder you intended to allow.

**Codex** runs one entry per enabled Codex model of your account. Its sandbox writes only to the session folder and
the allowed folders. Within a conversation Codex continues its own thread (`codex exec resume`), also after
`minigw -c` or `--resume`.

**Grok** (`minigw serve` on Windows and Linux; not supported on the Mac gateway) runs in its kernel sandbox: `workspace` (writes only to the session folder) or `read-only`. Within a
conversation it continues its own session (`-r`).

**Copilot** runs in a sandbox (on the Mac a `sandbox-exec` profile, on Linux bwrap) with
a fresh Copilot home for each request, deleted afterwards. Copilot is not supported on a Windows gateway in this
version (it cannot start in the Windows sandbox): its models are not listed and it cannot be turned on. On a Windows
gateway without the Windows sandbox, Grok runs only with **Allow commands without a sandbox**; Codex on Windows always
needs that switch.
`claude-copilot` uses Copilot's default model (`auto`); list other models by name in its settings.
- It only reads the folder unless **Allow writing** is on.
- It signs in only with your own `copilot login`: run it once on the gateway, as the user the gateway runs as. The
  token stays in the system keyring (Keychain on the Mac, a Secret Service keyring such as GNOME Keyring or KWallet on
  Linux) and only Copilot reads it; the gateway copies just the account pointer into Copilot's fresh home. There is
  no token, `gh` or PAT sign-in. Without a usable login Copilot is off and the settings, `minigw gw status` and the
  request say why. A headless Linux gateway (no Secret Service; `copilot login` then stores the token in plain text)
  has no Copilot. Linux also needs `xdg-dbus-proxy`, so Copilot reaches only the keyring from its sandbox.
- Copilot has no commands (its shell tool is always denied; **Allow commands** is greyed out with the reason): a command
  could read Copilot's sign-in from the Keychain or keyring.
- Within a conversation Copilot continues its own session (`--session-id`).

**Logins (`minigw serve`).** Agents use your subscription logins, never API keys, and nothing is copied back from an
agent run. Each agent has a **Login** row (settings page, also the Gateway tray's Settings window; `minigw gw
settings`; `minigw gw <agent> login`) that shows only the state (set, not set, logged in) and a one-line hint, never
the value: Claude CLI its `claude setup-token` token (`minigw gw claude token [remove]`, needed in the Windows
sandbox), Codex its ChatGPT login (`codex login`), Grok its `grok login`, Copilot its `copilot login`. **Use an API
key instead** (Claude CLI, Codex, Grok; `minigw gw claude|codex|grok apikey [remove]`) stores a key in the key store
and passes it only then; it is billed through the provider's API, not your subscription. API keys in the gateway's own
environment (`ANTHROPIC_*`, `OPENAI_API_KEY`, `CODEX_API_KEY`, `XAI_API_KEY`, `GROK_CODE_XAI_API_KEY`,
`GEMINI_API_KEY`, `GOOGLE_API_KEY`) never reach an agent. The Mac app has a Login row only for Copilot; Claude uses your Keychain login.

### Windows gateway sandbox

`minigw serve` on Windows (MSI, part Gateway) runs Claude CLI and Grok in the Windows sandbox, Microsoft MXC
(`mxc\wxc-exec.exe` in the install folder, no admin rights). It needs Windows 11 build 26100.9278 / 26200.9278 or
later (the Windows process container). Settings page → General → About shows "Agent sandbox: Windows sandbox (MXC
1.0.1)", or "not available (reason)"; then agents run only with **Allow commands without a sandbox**, as in 0.5.22.
With the sandbox that switch is not needed for Claude CLI and Grok (Codex still needs it: it cannot start in it; Copilot is not supported on a Windows gateway in this version).
New installs (`minigw serve init`, the tray's first start) and the settings page's Scratch **Default** button put the
Scratch folder at `minigw-scratch\<instance>` on the first fixed drive that is not the system drive, else on
the system drive (for example `D:\minigw-scratch\gw-3f9a1c2b7d4e`; 0.5.24), with an access list only your user can open.
Each gateway has its own folder `<base>\<instance>` with an owner file; a folder of another gateway is not used, and
after a reinstall **Take over** in the Scratch row (or `minigw gw scratch take [NAME]`) renames the old folder to the
new gateway's. Existing installs (for example `D:\minigw-scratch\ana`) keep their Scratch folder; when it is inside your home folder (for example the old default
`%LOCALAPPDATA%\minigw\scratch`) the gateway warns and proposes the new location. Move it with
`minigw gw scratch move [DIR]` (no DIR: the proposed folder), the settings page's Scratch **Move…** button or
`minigw gw settings` → General → Move everything to. The move takes everything along (mirrors, clones, `$MINIGW_DEPS`,
caches, agent state) and works only while no minigw session or agent runs; new requests wait (503) until it is done. On
the same drive the folder is renamed; on another drive the target first gets the owner-only access list, every file is
copied (links are copied as links, not followed) and checked (size and SHA-256), then `serve.json` is switched and only
then the old folder is removed. On any error the old Scratch stays in use and the error is shown. Keep Scratch on a
fixed NTFS drive. The same move works on Linux.
- The agents write only to the session's folder (when allowed), `$MINIGW_DEPS`, caches and temporary files under
  Scratch; they cannot read your home folder, other devices' copies or the gateway's settings
  ([How it works](how-it-works.md#how-the-folder-reaches-the-agent), Windows sandbox).
- Folders on `PATH` inside your home folder (for example `~\.local\bin`) are readable by agents; the settings page
  warns about them. Keep no secrets there.
- Network: agents have outbound network, except Claude in worktree mode with **Network for builds** off: then only
  TCP 443 to the addresses of `api.anthropic.com` resolved when the run starts (logged in the gateway log); Claude's
  web fetch and web search and every other host fail. If the name cannot be resolved the run is refused: "Cannot
  resolve api.anthropic.com for Claude CLI without network for builds; try again, or turn on Settings → General →
  Worktree mode → Network for builds" (503). There is no proxy in the sandbox.
- **May ask other models** opens the host's `127.0.0.1` to the sandbox (both directions), because the run port is
  there; without it loopback is denied. The gateway still refuses the agent (the admin API refuses it, the main port
  needs the token, which the agent cannot read, and the run port accepts only that run's processes), but other local
  services such as LiteLLM or Foundry Local are reachable: keep the switch off if you run local services agents should
  not reach. If the sandbox cannot reach `127.0.0.1` on this computer (it may need a newer cumulative update), the
  gateway log says so and "May ask other models" does not work there.
- **Claude CLI** needs the long-lived token from `claude setup-token` (your Claude subscription, valid one year):
  run `claude setup-token`, then `minigw gw claude token` (or settings page → Claude CLI → Login, or
  `minigw gw settings`). It is kept in Credential Manager; `~\.claude` is never copied into the sandbox. Without it a
  Claude request is refused with "Claude CLI in the Windows sandbox needs a long-lived sign-in token: run claude
  setup-token, then minigw gw claude token". Each run starts with a fresh Claude config and home; only the
  conversation's transcript is kept between turns, so `--resume` works.
- The sign-in the agent runs with (Claude's `claude setup-token` token or the API key you chose
  with `minigw gw <agent> apikey`) is in the sandbox's environment, so the agent's own commands in the box can read it
  (and, with network, send it elsewhere). For untrusted repositories keep **Network for builds** off.
- **Grok** has no long-lived token: its `~\.grok\auth.json` and `config.toml` are copied into its own home for the
  device before each run, never back. After Grok refreshes its login you may need `grok login` again.
- Claude runs commands with PowerShell (Git Bash cannot run in the sandbox); `Bash` in the tools list means PowerShell.
- **Codex** does not run in the Windows sandbox and runs only with **Allow commands without a sandbox**, also when
  its own elevated sandbox is set up: a Codex command in that sandbox can read the gateway's settings (`admin.secret`,
  `serve.json`). When it is set up (`codex sandbox setup --elevated` once, as administrator) each run still uses it
  as extra protection (`~\.ssh`, writes in your profile). If `~\.codex\config.toml` asks for `elevated` but it is
  not set up, the run is refused: "Codex's elevated Windows sandbox is not set up on the gateway: run codex sandbox setup --elevated once at the gateway PC (as administrator)". Codex uses your real `~\.codex` and its ChatGPT login, which refreshes
  itself.

**Siri AI** is driven through Accessibility, in the background. It keeps one Siri thread per Claude Code
conversation and has no tools.
- **Privacy:** Siri answers with the personal context of the gateway Mac's user (notes, documents). Other devices
  get it only when the gateway allows them (Settings → Siri AI → Devices). The gateway Mac itself always may.
- Files you mention (`@CLAUDE.md`, or names like "CLAUDE.md" or "kb" that exist in the session folder) are found by
  the gateway and added to the question as text (up to about 16 000 characters). Images and PDFs (up to 4) are
  attached in the Siri app, so Siri reads them.

**Apple Foundation Models** is on-device and streams, with about 8K context; the history is trimmed automatically.

**Foundry Local** gives Microsoft's on-device models. The gateway finds the local server (`foundry server status`),
starts it when needed and loads the model. Images work for models that take them.

**Providers** (LiteLLM, OpenRouter, any Anthropic-compatible upstream) are a pure passthrough, tools and streaming
included. Models and context windows are discovered from the upstream; you enable the ones you want
([gateway.md → Providers](gateway.md#providers)).

**The `claude-` rule:** Claude Code's `/model` shows only IDs that start with `claude-`, so the gateway lists every
provider model as `claude-<id>`, unless the ID already starts with `claude-`. For example OpenRouter's
`openai/gpt-5` is `claude-openai/gpt-5`, while a LiteLLM model named `claude-sonnet-4.5` stays as it is. Use the ID
that `minigw models` prints.


**Troubleshooting the Windows sandbox.** When the startup probe fails, the gateway log has the reason and a second line
"Windows sandbox probe: wxc-exec exit …; stdout: …; stderr: …". Start the gateway with `MINIGW_MXC_DEBUG=1` to keep
each sandbox request, without secrets (variables with TOKEN, KEY, SECRET, PASSWORD or AUTH in the name read
`REDACTED`), as `Scratch\tmp\mxc\mxc-<n>.debug.json`, a folder only your user can open; replay it with
`"%LOCALAPPDATA%\Programs\Mini Gateway\mxc\wxc-exec.exe" --debug <file>`. The probe itself is
`minigw.exe __mxc-probe <writable folder> <file that must be unreadable> 127.0.0.1:<port>` with one line on stdin,
run with outbound network denied and loopback allowed; it prints `nonce:<line>`, `write:ok`, `deny:ok`, `loopback:ok`
and `final:<path>` and exits 7 (wrong arguments: a message on stderr and exit 2). A failed `loopback` check alone does
not make the sandbox unavailable; only "May ask other models" does not work there then.
## KB submissions and provenance

`minigw note push` still publishes directly. `note read <id> --json` now includes
authenticated writer user/device, project, actor/source kind, source reference,
content hash, authored/received times, reviewer and review time. Legacy notes keep
known authors/projects/times, with `UNKNOWN` device and source. Older revisions
cannot be reconstructed. Import, session summary and gateway synthesis are reserved
source names; ingestion and service writes are deferred.

Use `minigw kb submit <project>|--team <team> [--class structure|data|private] [--kind finding|faq] [--title t]
[--key k] <file|->` to propose a note (`private` is for projects only). Input and title defaults match note push.
For a replacement add `--replaces <note-id> --revision <n>`. Existing project write
permissions apply; readers cannot submit. `minigw kb submissions [<project>|--team <team>]
[--json]` lists your submissions, across readable projects and the teams you are on (labelled `team <name>`) when omitted.

**Entry kinds.** An entry is a `finding` (the default) or a `faq`: the title is the question (at most 256 bytes)
and the text a short answer (at most 2 KiB). The kind is chosen with `kb submit --kind`, travels with the
review (the owner accepts a faq as a faq) and cannot be changed after publication (a Markdown or bundle import sets the kind only when it creates the note; a replacement always keeps the kind of the note it replaces); `note push` and `note edit` always
make or keep a finding. A replacement without `--kind` keeps the kind of the note it replaces. A faq shows
`  (faq)` at the end of its first line in `kb search`, `kb submissions` and `gw kb review`; findings print as before.
`minigw kb faq [<project>|--team <team>] [--json]` lists the published questions of a project (or a team's own notes), newest first, each with its answer;
it follows the same visibility as `note list` (private entries only for their author and the project admins) and is not
available inside an agent run. Exported Markdown and team bundles carry `entry_kind`.

**Team notes.** A team has notes of its own, next to those of its projects. Every member of the team (the roster
the owner keeps with `gw team member add`) reads them; whoever is admin of at least one of the team's projects (one that is not archived) can
also write them. A team member who is only in a project of the team, and not on the team's roster, reads nothing from
them. They are `structure` or `data` notes; there is no `private` class at team level, and an archived team's notes
are hidden until it is unarchived. Give `--team <team>` in place of a project: `minigw note list|read|push|edit|rm --team acme`,
`minigw kb submit --team acme <file>` (the owner reviews it with `minigw gw kb review --team acme`, another writer of the team's notes with `minigw kb review --team acme`),
`minigw kb faq --team acme`, `minigw kb search --team acme <query>` (only the team's notes),
`minigw kb export --team acme <dir>` and `kb import --team acme <dir>`. `--team` is not accepted together with a project,
nor inside an agent run (the run's project is its own). A search of a project also covers the notes of its team, as far as you
may read them. A line from a team's own notes names the team (`team <name>` where a project would show `project <id>`), and a hit
from there in a project search also ends with `from team:<name>`; `minigw team show <team>` tells you how many team notes exist; `minigw note read <id>` opens it. Inside an agent
run the same holds when the person who started the run is on the team's roster.

**Dependencies.** The owner can record that a project depends on another one, for example a topic project linked to a
subfolder of a larger project's folder: `minigw gw project depends add <project> <other>` (`remove`, `list` likewise; at most
16 per project, across teams, one level only). The people of `<project>` then also find the notes of `<other>` when they search
`<project>`, and can open a hit with `minigw note read <id>`, but only the notes each person could read in `<other>` anyway
(as a member, or through a grant): a dependency is a search hint and never a permission, and `private` notes never appear.
Such a hit ends with `from project:<slug>`. `minigw project depends [<project>]` lists the dependencies you can read (a team on its own line, and only if you are on its roster). An agent
run is not widened by a dependency: it reads another project only through an owner grant, as before; the dependency only
labels and orders those hits. A project linked to a subfolder of another project's folder gets **no automatic** dependency:
the owner sets it.

A project can also depend on a **team**: `minigw gw project depends add <project> --team <team>` (`remove` likewise; `list`
shows both kinds, a team as `depends on team <name>`). The people of `<project>` who are **on that team's roster** then also find
the team's notes when they search `<project>` (hit ends with `from team:<name>`) and can open them with `minigw note read <id>`;
everyone else sees no hit, no label and no count. It is a search hint like the one above: the roster decides, as for a direct
read, and an agent run gets nothing from it (a run reads the notes of the project's own team when its user is on that roster, as
before). The cap of 16 counts both kinds; an archived team gives nothing; the project's own team is refused (its notes are
searched anyway). A team dependency is not carried by `minigw gw export` / `import`: a bundle holds one team and its own projects,
and a project never depends on its own team.

Only the author and owner reviewers see pending content. It never appears in note
lists, reads, agent runs or counts. The owner uses `minigw gw kb review [<project>]`,
`minigw gw kb accept <id>` and `minigw gw kb reject <id> [--reason text]`.
Acceptance publishes a note or replacement revision; stale revisions conflict.
The writer's permissions and original device binding are rechecked at acceptance.
Project admins do not gain owner review authority. Called agents remain read-only.

Pending limits are 100 entries / 5 MiB per user and 500 / 20 MiB per project.
Reasons are bounded to 1024 bytes; reasons and submission titles reject control
characters. Existing note input and 24-hour idempotency limits apply. Save the
printed key to retry after a lost answer. Lists page in batches of 100.

Customer names, policy numbers, amounts and raw records must never enter **any KB
class**, including private. `data` is a permission label, not permission to store
customer data. Preview and sanitize before publishing or submitting; this beta
does not detect customer data. Rejection is not deletion. Backups and snapshots
retain content and provenance; restore can restore old content and permissions.

### Review by project admins

The gateway owner is not the only reviewer: an admin of a project reviews its submissions with `minigw kb review [<project>|--team <team>]`
(pending submissions with their text, writer and device, and what they replace), `minigw kb accept <id>` and
`minigw kb reject <id> [--reason text]`; the output is that of `minigw gw kb review|accept|reject`. Admin means the role
`admin` in that project, given directly or through a team assignment; a grant never makes a reviewer. A team's own notes
are reviewed by whoever may write them (an admin of one of the team's projects who is on the team's roster). Anyone else
sees nothing of the review: the answer is the "not found" of a project they cannot see.

**Four eyes.** Nobody reviews their own submission: another admin of the project or the gateway owner does. A project
with a single admin therefore leaves that admin's submissions for the owner (or a second admin). Accepting also needs
what the reviewer could write themselves (the class, and the note a replacement changes). The published note keeps the
writer as its author; its provenance names the reviewing user, and the audit records accept and reject with that user.
`kb review` prints a line `version <hash>` for each submission: the whole SHA-256 (64 hex characters) of what an accept
publishes and attributes (text, class, kind, replacement, and the stored provenance such as device and source). Accept
what you read with `minigw kb accept <id> --version <hash>` (the owner: `gw kb accept`), copying the whole line: if the
writer changed the pending submission since (also the same text sent again from another device), it is refused with "the
submission changed since you read it: review it again" and nothing is published; a shortened hash is refused outright.
Without `--version`, `kb accept` shows the text and asks "Accept this text? [y/N]", and only in a real terminal.

What this protects and what it does not: it guarantees that what is published is exactly the version that was read, so a
text CHANGED after the review is never published by mistake. It does not prove that a person read it: `--version` is how
scripts and agents accept, and nothing technical makes an agent show you the text first (the minigw skill tells it to);
four eyes means a second admin's credentials, and the care of the person who holds them.

Text a reviewer cannot see is never published: the review reads (`kb review`, `gw kb review` and the owner's Knowledge base panel) show
the title and text without invisible and direction-changing characters (bidi overrides, zero-width characters, the tag block, line and
paragraph separators, a carriage return inside a line and the other controls except newline and tab: the rule the mirror uses; Windows line
endings are stored as plain line ends in every note and submission, and a CRLF in a submission stored before that is a line end too) and say how many were left out
("contains hidden characters: N removed"). Such a submission cannot be accepted; reject it and ask the writer to submit the text without
them. The same rule applies at submit: a submission whose title or text has such characters is refused at once, with their number and the line, column and code point of the first three ("the text has 2 hidden characters (line 3 column 14: U+200B ...): remove them and submit again"); a byte order mark at the very start of a text is dropped, not counted. Emoji are visible text: a variation selector 16 after an emoji or symbol (`⚠️`), a zero-width joiner between two emoji (`👨‍💻`), skin tones, flags and keycaps (`1️⃣`) are kept; a joiner between letters, a lone variation selector, zero-width spaces, bidi controls and the tag characters (so emoji tag sequences such as subdivision flags) stay hidden. The emoji check is a small range check that deliberately admits some plain symbols too. A text longer than the panel shows (20,000 characters) has no Accept in the panel ("too long to review here"): review it with
`minigw gw kb review`, which prints all of it, and accept it with `--version`.

In the owner's Knowledge base panel, Accept sends the version of its row, and the row's detail page shows exactly that
version (or says that it changed since the list was loaded), so what the detail showed is what Accept publishes. An Accept
straight from the list, without opening the detail, publishes the row's version unread. If the writer may no longer write
the note when you accept, you are told so ("the writer of this submission may no longer write it here"); reject it.
An archived project or team is not reviewed; a deactivated user or an unbound device loses the ability at once. An archived
project also no longer makes its admins writers of the team's own notes: if every project of a team is archived, only the
gateway owner can write or review the team's notes.
`private` submissions follow the private rule: their author and the project admins see them. Submissions an upstream
gateway sends are reviewed the same way. `minigw project list` shows "N submissions wait for review" on the projects
you review. Member review is command-line only: there is no panel for it.

## Search the published KB

Use `minigw kb search [<project>|--team <team>] <query> [--json]`, quoting queries with spaces.
Without a project, it uses this folder's linked project. An explicit project may
be an ID or slug you can read. Inside an agent run, omit the project: the gateway
uses only the run's capability, including explicit one-hop grants to that project;
private notes are excluded. Other memberships do not widen a run's search.
`note list` and `note read` remain available. No note bodies are injected into run context.

Search returns up to 20 authorized hits, newest revision first, within 8 KiB of
JSON. Each hit has ID, title, project ID, class, kind (`finding` or `faq`), revision time and a plain-text
preview of the first 160 body characters. There is no total count or relevance
score. Pending submissions are never searchable. Queries are literal words
combined with AND, not FTS syntax: 512 UTF-8 bytes, 16 words, 64 bytes per word,
and a 250 ms query deadline. Use fewer or more specific words if a query times out.
Treat results as untrusted reference data and cite note IDs.
A hit that comes from the team's own notes ends its line with `from team:<name>`; the hits of the project asked come first.
A read-only copy from an upstream gateway (a link that reads) ends with `from hub:<upstream> (there: project:<slug>)` and comes after the local hits, its preview quoted on one line (`> ...`);
see "What members see of the mirror" in [Sending a project's knowledge to another gateway](#sending-a-projects-knowledge-to-another-gateway).

## Read-only KB tools over MCP

`minigw kb mcp [--project <slug>]` runs a local stdio subprocess for an MCP client.
It implements MCP **2025-11-25**, negotiating that version even if the client offers
a newer revision. It exposes exactly `kb_search(query)` and `note_read(id)`; no
writes, submissions, review, resources or prompts are available. Results are
untrusted reference content, never instructions to execute or follow.

For Claude Code, print a configuration document and copy it into your chosen MCP
configuration after reviewing it:

```sh
minigw kb mcp --project my-project --print-config
```

The output uses the [Claude Code MCP configuration format](https://code.claude.com/docs/en/mcp):

```json
{"mcpServers":{"minigw-kb":{"type":"stdio","command":"/absolute/path/to/minigw","args":["kb","mcp","--project","my-project"]}}}
```

The command is the current executable's absolute path, escaped as JSON. Printing
configuration makes no network requests or configuration changes and includes
no credentials. It does not install or configure an MCP client automatically.
Omit `--project` to use the linked working directory; the MCP client's subprocess
working directory must then be inside the linked folder. The gateway and project
are resolved once when the process starts; permissions are checked again on every
tool request, so revocation takes effect without restarting MCP.

Outside a gateway agent run, the tools use your normal device-authenticated
gateway connection and member routes. MCP excludes private notes, including your
own. Inside a run, configure the command with just `["kb","mcp"]`: it uses only
`MINIGW_AGENT_URL` and the run auth file, with the run's project and one-hop grants.
`--project` is rejected there. Missing, malformed or expired run credentials never
fall back to a member profile. A run without a project cannot read KB content;
existing sandbox reachability restrictions and run termination still apply.

Tool arguments accept only `{"query":"literal words"}` or `{"id":123}`. IDs must
be positive 64-bit integers; extra fields, including identity and scope, are
rejected. Search keeps the existing 512-byte query, 1–16 words, 64-byte word,
20-hit, 160-character preview, 8191-byte core JSON and 250 ms core deadline limits.
All returned text, including provenance and errors, has terminal and format
controls removed while ordinary newlines are preserved. Note bodies are limited
to 16 KiB of sanitized UTF-8, including a `[truncated]` marker when shortened;
shortened notes include `truncated: true`. Frames are limited to 64 KiB and complete
encoded replies to 256 KiB; oversized metadata produces a bounded tool error.
The [stdio transport](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)
uses one UTF-8 JSON message per line, protocol-only stdout, stderr diagnostics,
and exits cleanly when stdin closes.

## Sending a project's knowledge to another gateway

A gateway can send the **published notes** of a project (or of a team's own notes) to another gateway, the *upstream*, for example
from the gateway on your own computer to the team's gateway. This is step A of the upstream feature (0.7.21): **one way, by hand,
and as submissions**: nothing is sent unless the owner of the local gateway runs `push`, and what arrives is a *proposal* that the
upstream's reviewers accept or reject with the usual `minigw gw kb review`. Nothing is pulled back, nothing is sent automatically,
and nothing is written there directly.

The local gateway is an ordinary **client** of the upstream, with a device key of its own. No new trust is created: the upstream's
owner decides what it may do with the existing means.

1. On the upstream, copy its token (Mini Gateway, Settings, General, Copy Token). Locally, as the owner:

   ```text
   minigw gw upstream add hub mini.example:8765 [--device-name <text>]   # the token line on stdin (or typed, not echoed): never an argument
   ```

   The command makes the first contact of an ordinary client, so the device is listed as **waiting for approval** on the upstream under its name, and prints the **device ID** of this gateway, where the credentials are kept (the credential store, like a provider key;
   never the project database, so never in a backup) and what the upstream says about it. The address is `host` or `host:port` of a
   gateway (the same encrypted protocol as `minigw connect`; no scheme, user or path). The gateway contacts the upstream only for
   the commands on this page, never on its own, and never follows a redirect.
2. On the upstream, its owner **allows that device** (`minigw gw devices allow <device id>`), **binds it to a user** (best a
   dedicated one, for example `gw-mini`: `minigw gw bind <device id> gw-mini`) and **gives that user a role** (developer or admin)
   in the project that should receive the notes, or in a team's notes (an admin of a project of the team).
3. Locally, link a project to a project there. A project without a link sends nothing:

   ```text
   minigw gw upstream link <project> hub [--remote-project <slug>] [--class structure,data] [--with-private]
   minigw gw upstream link --team <team> hub [--remote-team <name>]     # a team's own notes
   minigw gw upstream status [<project>|--team <team>]                  # what would go, when it last went, the last refusal
   minigw gw upstream push [<project>|--team <team>] [--dry-run]
   minigw gw upstream unlink <project> hub        # also: list, remove <name>
   ```

   `private` notes go only when the link says `--with-private` (and names the class); it then also sends the private notes of
   ALL members of the project, not only your own. `push --dry-run` only asks the upstream what exists and writes nothing,
   there or here. `minigw gw upstream pull [<project>|--team <team>] --dry-run` shows the other direction:
   what the upstream's device user could read of the linked project (per source: the project, its team's notes, what it depends on;
   counts per class and kind, never private, at most 5000 notes). It stores nothing and nothing runs it by itself.
   Reading the upstream's notes is described below ("Reading the upstream's notes"). Each note carries the name of its author in its text (a line at its end, with an opaque reference and, for
   a replacement, the number of the earlier submission); the submission itself belongs to the bound user.

**Reading the upstream's notes (the mirror, the manual pull and what members see).** A link can also *read*: the gateway keeps a bounded **mirror**
of the notes the upstream's device user may read in the linked project, in a table of its own (`upstream_mirror`), never among the local
notes. The members of the local project find it in `kb search`, `kb faq`, `note read` and `kb mcp`, marked as coming from the upstream
(see "What members see of the mirror" below). A link with reading on is also **pulled by itself** (see "Automatic pull" below), so the
mirror stays fresh with no command typed; the Upstream gateways panel has a Reading section for all of it.

```text
minigw gw upstream pull <project> --dry-run      # first look: what the upstream's user could read (sources, classes, counts)
minigw gw upstream link <project> hub --read [--read-class structure,data] [--read-sources team:acme,project:eureko|none] --accept-reach
minigw gw upstream pull [<project>|--team <team>]   # fills the mirror; asks again until it is through
minigw gw upstream status                           # read on/off, accepted reach, mirror notes and size, last pull and its age, last error
minigw gw upstream link <project> hub --pull-every 30   # how often a link with reading on is pulled by itself (minutes, 5 to 1440; default 15)
minigw gw upstream link <project> hub --no-read      # reading off: the mirror is deleted
```

`--read` is refused without `--accept-reach`: reading means that the local project's members find everything the
upstream's **device user** may read in the project, by their local class rights, so bind each gateway's device on the upstream to a user
that has the reader role only in the projects meant for that gateway. The default is the `structure` class of the linked project itself;
`data` only with `--read-class structure,data`; the project's team notes and what it depends on only when named with `--read-sources`
(the labels `pull --dry-run` prints). `private` notes are never read. An existing link keeps its push settings when only the reading is
changed; changing what is read deletes the mirror (the next pull fills it again). Without `--read-sources` the sources the link reads now are kept, `--read-sources none` clears them, and turning reading **off forgets** the classes and sources of the link, so turning it on again starts from the default (structure, no extra sources): an acceptance never brings back a reach you did not see. A link update (`link ...`) is refused while a push or pull of that link is running. The acceptance covers the exact **reach**: the upstream, the remote project or team, the classes and the sources (shown by `status` as "accepted ... for <remote>"; the audit row of the link names them). A link that is changed to reach another remote project or team (for example `link <project> <name> --auto`, which defaults the remote to the local name) turns reading off and deletes its mirror until you repeat `--read --accept-reach`. The mirror is deleted by `--no-read`, `unlink` and
`upstream remove`, is not part of an export bundle and is never pushed.

A pull asks the upstream for a **manifest** (ids and revisions only), fetches at most 200 new or changed notes per run (the command repeats
until it is through, and a helper-backed gateway such as the Mac works within the time of one helper call, about 10 seconds, and
continues where it stopped), and deletes what a **complete** manifest no longer lists (never from a manifest that was cut at 5000
notes). When the upstream refuses the device (401/403), no longer knows the project or team, the mirror of that link is **deleted at
once**. An upstream that is too old for the manifest (404/501, or another version) is left alone: nothing is stored. What the upstream
sends is treated as data: a note that is not the one the manifest listed, is over 256 bytes of title or 64 KiB of body, or answers
badly is skipped (and shown), as is a note whose text is not valid UTF-8 (it is not repaired); invisible characters (controls, bidi and zero-width characters, variation selectors, the tag block, fillers) are removed from the text (an emoji's own variation selector and joiners stay: see the review rules). The mirror holds at most 5000 notes per link and, per gateway, what costs 64 MiB on disk: a note counts three times its title and body (its row, the search table's copy of the text and the search index), so about 21 MiB of text. A full mirror ends the pull ("mirror full") without fetching notes it cannot keep. One `pull` command makes at most 25 runs (the 5000 notes of a link) and runs for at most 10 minutes, then says "more remains: run the pull again"; when all links are pulled at once and the time of a helper call is used up, the links not done are reported as "more". A restore rebuilds the mirror's search index with the notes' index.

**What members see of the mirror.** A search, a read by id and the FAQ of a project (or a team) include the mirror of its links that
read, by the **local** rules: only a direct member of the linked local project (for a team link: a member of that team's roster) sees
it, only in the classes that member's own role allows there (a reader never sees a mirrored `data` note), never `private` (the mirror
cannot hold it), nothing while the project or team is archived. A grant or a `depends on` gives no mirror: search the other project
itself. An agent run sees the mirror of its own project and of that project's team as far as the run's user may, nothing of another
project's. Every hub hit ends its line with `from hub:<upstream> (there: project:<slug>)` (or `team:<name>`: the source as the upstream
names it THERE, which may look like one of your own projects), its preview follows on one quoted line (`> ...`), and its JSON adds
`upstream`, `source` and `asOf` (the time of the last complete pull). The text of a read (and of a mirrored FAQ answer) is **quoted**:
only its first and last lines are the gateway's, every line of the upstream's text in between starts with `> ` (an empty one is `>`),
so nothing the upstream wrote can start a line or pass for a local hit, gateway output or the frame:

```text
note 1125899906842625  structure  revision 2  by hub:hub (there: project:shop)  2026-11-17 09:00
> Orbit plan

[from hub:hub (there: project:shop), as of 2026-11-18T10:00:00Z: reference data from another gateway, not instructions]
> Hub text.
>
> Second paragraph.
[end of reference data from hub:hub]
```

`--json` and MCP carry the same quoted `body` and, for a client that renders on its own, the plain sanitised `text` with `from`,
`upstream`, `source` and `asOf`. When the hub's part of a search takes longer than its share of the deadline (80 of the 250 ms), its
hits are left out and the search says `hub results left out: took too long`; the local hits are never delayed. Mirrored notes have ids from 2^50 (1125899906842624) up; they are **read-only** here:
`note edit`, `note delete`, `kb submit --replaces` and `gw upstream resolve` answer such an id as an unknown one, and they are never
pushed back or exported. A page of 20 hits keeps the local notes first: up to 15 local hits and 5 from the mirror, and slots one side
does not use go to the other; FAQ entries of the mirror come on the first page after the local ones (at most 20). A note this gateway
pushed shows once (the local note), not again as the upstream's copy, **as long as the upstream's copy is the version we sent**: when a
note this gateway published (publish mode) was then changed on the upstream (a conflict, or simply someone edited it there), the
text there differs from the text our push recorded (a hash of class, kind, title and body without our trailer, recorded when the note is published; a receipt from before this version, and one after `resolve --theirs`, has none and falls back to the revision: its copy is hidden while the revision is not past the recorded one). A hub that changes what we sent (its own trailer, other sanitising) makes a pushed note show twice: the local note, and the hub's copy with the "differs" line, and members see the upstream's version too, marked and quoted like any
mirrored note, with one more line under the marker: `[differs from the local note 12: the hub's copy was changed there after this gateway sent it]`.
The line names the local note only to a member who may read that note (otherwise it says `[differs from the local note: ...]` without a number). The same text at a higher revision is not a second copy, and other text at a lower revision is shown: `gw upstream status` also says "revision went backwards on the upstream: note N" when the upstream's revision is below the recorded one. The owner's status keeps listing the conflict, and `resolve --theirs` (the recorded revision becomes the upstream's: the
copy is hidden again) and `resolve --ours` (our version goes as a submission; the copy stays visible meanwhile) work as before. A copy of
a note sent as a submission (no note id there is known here) is still recognised by the trailer at the end of its text and hidden. A mirror without a complete pull for **7 days** is hidden from
members (not deleted) and `gw upstream status` says "stale" until a pull goes through. Reading never contacts the upstream: an
offline laptop or a stopped hub answers from the mirror within the usual search deadline.

**Automatic pull (links with reading on).** Every link with reading on, whose reach you accepted, is pulled by the gateway itself every
**15 minutes** (the link's own interval: `link <project> <name> --pull-every <minutes>`, 5 to 1440, or **Set Pull Interval** in the panel). On
Windows and Linux the gateway's timer does it; on the Mac the app's timer asks the helper for one step at a time (the helper's answer says
when the next one is due) and never while the app is shutting down. A step is one link, pushed or pulled: the two are tried in turn, so
neither starves the other. A pull is the same run as `pull` by hand (the same bounds per run: 200 notes, a few seconds on the Mac); a run
that is not through ends with "more" and continues at the next tick from its cursor. A successful pull schedules the next one an interval
ahead. A traversal (from the first note to the last, over as many runs as it takes) in which the upstream could not be asked or did not answer properly (no answer, a timeout, a 5xx, a read limit that could not be waited out, a failed manifest) in any run never ends fresh: it records progress, goes on, then ends as a failure (back-off, no new "last complete pull") and the next traversal starts from the beginning and fetches what is still missing. When the upstream ANSWERS about one note and the answer is definitive (not served: 404 or another 4xx; or it fails the checks: wrong id, revision, class or kind, invalid UTF-8, too large, malformed), only that note is skipped: it is counted and remembered (up to 50 as `id:reason`, never upstream text), shown by `status` ("N notes could not be read") and in the panel, retried at the next traversal, and the traversal still completes and is fresh. A copy of such a note that the mirror holds is kept while the manifest still lists its revision and dropped when the revision moved, so stale text is never served as current. A failure backs off from a minute, doubling up to an hour, with jitter, and a run that made no progress counts as a failure. If the
upstream **refuses** the device (401/403) or no longer knows the project, the mirror is deleted at once, as in a pull by hand, and the
**timer for that link is switched off** with the error left visible in `status` and the panel; it is switched on again only by accepting the
reach again (`--read --accept-reach`); a pull by hand never switches the timer ON, and a refusal switches it off whoever started the pull (access was withdrawn); a complete pull by hand also ends the audit episode. A pull (by hand or by the timer) reads the link, its remote and its accepted reach together and refuses ("the link's reach is not the one you accepted") when they do not match, so it never contacts one remote with another's acceptance. A pull never runs at the same time as a push of the same
link: whichever comes second is refused and tries again a second later, and one automatic pull holds the link for at most 45 seconds, so a
push is never kept waiting for the 5 minutes of a long pull. `gw upstream status` shows the interval, the next attempt, the failures in a
row and, when the timer is off, why. The audit log gets one row (`upstream.auto-pull`: counts only, never a title) for every run that added,
changed or removed something, and otherwise one per change of outcome, at most five in all per failure episode (from the first run that was not a complete success to the next one that was). With the timer on the mirror stays fresh, so the 7-day
"stale" state only happens when the upstream cannot be reached for a week (or the timer was switched off). The timer contacts only an
upstream you added and a link whose reach you accepted (never one of an archived project or team); no member can start or change it. It has been tried only in tests, not between
two real gateways.

**The panel.** The Upstream gateways pane (the Mac app, and the `minigw serve` settings page) has a section **Reading from upstream
gateways** under the links. Per link it shows whether reading is on, the classes and extra sources, the interval, the notes in the mirror,
the last complete pull, the next pull and, when it is not hidden, nothing more; **Open** adds the accepted time, the size and age of the
mirror, the last try, the failures in a row, whether the automatic pull is off, the last error and "hidden from members" (stale).
Actions: **Dry Run** (asks the upstream what its user for this gateway may read and shows the sources and counts; writes nothing),
**Turn Reading On…** and **Turn Reading On With Data…** (a confirmation names the upstream, the project or team there, the classes and
the extra sources you typed, says that members of the local project will find it, and that confirming records that you accept this
reach: the same acceptance as `--read --accept-reach`), **Change What Is Read…** (new classes and sources, an empty list meaning none; a new confirmation, the mirror
is deleted and refilled), **Turn Reading Off** (deletes the mirror at once), **Set Pull Interval…** and **Pull Now** (one bounded run; the
report says when it is not finished: press it again). Every action that contacts the upstream or changes what is read asks first.

**What leaves the gateway with a push**, exactly: for each note its title, class, kind (finding or faq) and body; one line at the end of the
body with an opaque reference (a hash of this gateway, the link and the note: stable for the note, it says nothing about this gateway) and
the local author's display name, and, for a replacement, the number of the earlier submission; and, in the encrypted handshake, the device
name of this gateway and the device ID (a public key), the upstream's Copy Token as the credential. The device name is `gateway <8 characters
of an id>` unless the owner chooses one with `--device-name` (printable ASCII, up to 64 characters); it is never the computer's name. No
note id, revision, gateway id, user name of the owner, path, session or usage data goes. An upstream on the same computer (a loopback address
with another port than this gateway's own) is allowed: it is another gateway. Names that are numbers in disguise (`2852039166`,
`0x7f000001`) are refused as addresses. An archived project or team cannot be linked, and nothing is sent for one that was archived later.

**Automatic push (opt-in per link).** `minigw gw upstream link <project> <name> --auto` makes the gateway send a link's notes by itself; `--manual`
turns it off (manual is the default). When a note of the project is published, edited, imported or accepted, the link is marked as having unsent
changes (two timestamps in the database, never a queue of notes: a restart loses nothing). A background pusher sends the link once it has been quiet
for a minute, and no later than ten minutes after the first change, so a burst of edits (or a member editing again and again) is one push, and a
member can never cause more than one push per window. One link at a time. If the push fails, the next attempt is after a minute, then two, four and
so on up to an hour (with up to 20 % jitter either way), a limit of the upstream's writes is waited out and is not a failure, and a push that has
unsent changes arriving while it runs leaves the link marked. A traversal may span several batches: changes below its cursor are sent by
a new traversal from the beginning afterwards, and a refusal in any batch keeps the link dirty with back-off. Its generation and failure
state survive a restart. `minigw gw upstream status` shows per link whether it is auto or manual, since when it
has unsent changes, the last success, the last failure and how many in a row, the reason and the next attempt, and lists sent notes that were deleted
here ("deleted here") or made private here ("now private here"): neither is propagated, the copy stays on the upstream until you remove it there.
Unlink and upstream removal are refused while a push of that link is running; try again after the push finishes.
Each automatic run leaves one audit row (the link and the counts, never a title or a text). **The gateway never calls an outside host on its own
account:** the automatic push contacts only an upstream you added with `upstream add` and only for a link you set to `--auto`. On the Mac the app's
timer asks the core's upstream lane every few seconds (`POST /upstream-auto`, at most one link per request), because the helper cannot call out by itself.

**Publish mode.** By default a push sends proposals: the upstream's reviewers accept them (`--mode submit`). A link made with `--mode publish`
writes the notes on the upstream directly (the device's user there needs the developer role in the project): the first push creates the note
there, later pushes update that same note. Before updating, the gateway reads the note there: if its revision is not the one it left (somebody
changed it on the upstream, or deleted it) the change is **not** written. The note is a conflict: it is listed in `upstream status` (and in the
push report) as `CONFLICT: note N (changed there)` and is skipped until you decide with `minigw gw upstream resolve <project> <note> --theirs`
(keep the upstream's version; our change is forgotten for it) or `--ours` (our version is sent as a submission for the upstream's reviewers,
never as an overwrite). Add `--upstream <name>` to resolve only that link; it is required when the note conflicts on several upstreams.
Without it, a single conflict is still resolved as before; an ambiguous request lists the upstreams and changes nothing. The mode cannot
be changed once the link has sent notes: unlink and link again (what was sent stays there). Deleting a
note here or making it private is not propagated in either mode; `status` lists it.

**Notes that came from another gateway are never pushed onward.** A note whose text ends with the trailer line a push writes (`<!-- minigw
upstream ref …, author … -->`) arrived from another gateway: that gateway's push created or updated it here, or a submission it sent was
accepted. Such a note is never sent by any push, automatic or manual, in either mode: not back to the gateway it came from (two gateways
linked to each other would otherwise copy each other's notes back and forth until both projects were full) and not to any other upstream
(that would hand a note, and its author's name, to a gateway its origin never chose). `upstream status` counts them ("N notes not sent:
arrived from another gateway, never pushed onward"), and the push report lists each as `skip … arrived from another gateway`. A member who
takes such a note over and removes that last line makes it local work: it is sent like any other note (to the gateway it came from too,
where it becomes a new note that ends with this gateway's trailer, so it never comes back). A member who ends a note with such a line by
hand keeps that note from being pushed; they could change or delete it anyway. Relaying notes on purpose is not a setting (yet).

A repeated push sends only what changed: every note is remembered by its content (class, kind, title, text). A changed note is sent
as a **replacement** of the note the upstream accepted before; if that note is still pending there, or cannot be read by the bound
user, it goes as a new submission that names the earlier one. Notes deleted here, and changes of class to private, are not
propagated; the copy there stays until it is removed there. If the upstream limits writes the push waits and goes on. Every note
of a run is reported as `create`, `skip`, `replace` or `refused` with the upstream's own text; a refusal that would apply to all
notes (not allowed, no such project, too many pending submissions) ends the run for that link and is shown by `status`.

What an upstream answers is treated as data: it can make this gateway neither write a note, nor change a setting, nor call another
host. The gateway sends nothing to itself (an upstream that is this same gateway is refused). The commands are the same on the Mac app
and on `minigw serve`; on the Mac the credentials are Keychain items of the app.

## Hand findings in from a session

The `minigw` plugin for Claude Code can ask a session to hand in what it learned, so the project knowledge base grows
without someone having to remember it. It is **off by default**: installing or updating the plugin enables nothing, and the
hook entries print nothing until you switch them on for a project.

```text
minigw kb hooks on [<project>]
minigw kb hooks off [<project>]
minigw kb hooks status [<project>]
```

Without a project, the folder you run it in must be linked (`minigw project link`); with a slug, it is a project that one of
your folders on this computer is linked to. The switch is per project, per gateway and per user, and it is stored only on this
computer in `kb-hooks.json` next to `projects.json` (mode 0600, never inside a repository). These commands make no network call
and need no project role. `off` stops the hooks without uninstalling the plugin; uninstalling the plugin removes the entries.

What the hooks do, in a folder linked to a project with hooks on (`minigw kb hook start|compact|stop|end`, called by the plugin
and not meant to be typed):

- At the start of a session (and after `resume`, `clear` and `compact`) they tell the model that the folder is linked to a
  project with a shared knowledge base and how to search it (`minigw kb search`).
- Before a compaction they ask the summary to include up to three one-line candidates (decisions, pitfalls, recurring
  questions) or the line `KB: nothing new`; after the compaction they ask the model once to hand them in.
- When the model stops after a **commit** was made in the folder during the session, and nothing was handed in yet, they ask
  once per session for a hand-in of at most three short entries (`minigw kb submit <project> --kind finding|faq`). "Nothing to
  hand in" is the normal answer and the model is told to give it when nothing is new.

What they do not do: they do not ask at the end of a session (Claude Code gives a hook no way to talk to the model then), they
never read or send the transcript, they never block a compaction, and they make no network call themselves. They add no
permission of their own, but be exact about what you approve: each `minigw kb submit` the model runs goes through Claude Code's
normal Bash prompt unless you allow-listed `minigw` commands, and then there is no prompt. While the plugin's `notes` skill is
active, `minigw kb search *` is pre-allowed, so the words of a search (the model's candidate text) reach the gateway without a
prompt. Members review every submitted entry before it is published. A failed hand-in is reported once and not retried.

Privacy: the hook texts are fixed and contain only the project's slug; no file names, no knowledge entries and no transcript
text. What can reach the gateway is the model's own short text, in a search or a hand-in. The hook runs `git` read-only in the
linked folder: `git rev-parse HEAD`, `git ls-files -s` and `git ls-files -o --exclude-standard`, and `lstat` (size, time) of the
tracked files; `git status` is not used because it runs a checkout's content filters. The guarded environment of the other client
git calls (no global or system git config, hooks, attributes file or fsmonitor) and `core.fsmonitor=false` apply; the checkout's
own `.git/config` and `.git/info/attributes` still apply to those commands, which read no file content. Only HEAD and a hash are
kept. The plugin calls the client only when it is version 0.7.13 or newer (an older `minigw` would pass the hook's input to a model).

Files, under the same `minigw` folder as `projects.json` (macOS: `~/Library/Application Support/minigw`; Windows and Linux: the
user's config folder), mode 0600:

- `kb-hooks.json`: the projects the hooks are on for.
- `kb-hook/<session id>.json`: one marker per session (where it started and whether it was already asked); removed at the end of
  the session, markers older than 14 days are removed at the next session start and at most 200 are kept. A session id is 1 to 64
  letters, digits, `-` or `_`; Windows device names (`CON`, `NUL`, `COM1` ...) are refused.
- `kb-submitted.json`: when `minigw kb submit` last succeeded for a project (a manual submit counts as a hand-in too).

Hooks inside an agent run of the gateway do nothing. Hand-in requires the gateway to be reachable when the model submits.
