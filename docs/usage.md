# Using Mini Gateway

Scenarios from first use to parallel agents. Every command here runs on the client, the computer where you work.
For the full option list see [commands.md](commands.md); for what happens on the wire see
[how-it-works.md](how-it-works.md).

- [Run Claude Code through the gateway](#run-claude-code-through-the-gateway)
- [Ask another model from Claude Code](#ask-another-model-from-claude-code)
- [Hand a task to an agent](#hand-a-task-to-an-agent)
- [Get a review](#get-a-review)
- [Work modes: what happens to your files](#work-modes-what-happens-to-your-files)
- [Worktree mode: results as branches](#worktree-mode-results-as-branches)
- [Read-only and roles](#read-only-and-roles)
- [Which folders are shared](#which-folders-are-shared)
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
/minigw:ask claude-grok What is the worst-case complexity of this algorithm in src/sort.go?
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
until the gateway owner turns them on. See [gateway.md → Settings](gateway.md#settings).

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
| `direct` | Your folder, mounted on the gateway Mac over the connection (WebDAV) | Changed in place, protected paths read-only | Mac only |
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

**Inside Claude Code** (the plugin's skills, `CLAUDECODE` set), `--minigw-work=direct`, `sync` and `native-direct` are
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
  files go (compared by size, time and SHA-256).
- **Never copied back:** files the agent puts into a [protected path](how-it-works.md#protected-paths).
- **Conflicts:** if you changed a file the agent also changed, yours stays and the agent's version is saved next to
  it as `<name>.minigw-conflict`.
- **Read-only:** with `--minigw-ro` nothing comes back.
- **Kept between requests:** the next sync and build are incremental. A copy that no session used for 14 days is
  deleted.
- **Names:** files whose names differ only in case (`A.txt`, `a.txt`) are not synced, with a note. Accented names
  travel in NFC. A Linux folder that holds the NFD form of a name may get a second, NFC copy back.
- Agents in the same folder run one after another.

Limits are in [how-it-works.md → Limits](how-it-works.md#limits).

### Direct mode (Mac gateway)

`minigw --minigw-work=direct …` lets the gateway Mac mount your folder through the encrypted connection (built-in
WebDAV). The agent changes your files in place. Writes to [protected paths](how-it-works.md#protected-paths) are
refused, so agents cannot `git commit` or change the folder's Claude Code settings there. Use worktree mode for
commits.

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
names the setting). On a Windows gateway this works in the Windows sandbox (Claude CLI, Copilot); Codex there, and
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
fetches the branch into your repository. It is never checked out. minigw prints one line per branch:

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
new). `--resume <id>` reuses that session's branch name, which moves forward; `--continue` gets a new branch name each
turn.

- `--continue` takes the last conversation minigw ran in this folder (per folder, like Claude Code's `-c`);
  `--resume <id>` that session; `--resume <id> --fork-session` continues from that session's branch, and the fork then
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
- Grok with writing on is refused in worktree mode: "rerun with minigw --minigw-work=sync …".
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
  effect. On the Mac Copilot follows "Network for Codex builds" (and its "Allow web access") for that, although its
  commands always have network.
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

`MINIGW_SHARE=0` turns folder sharing off; `MINIGW_SHARE=1` shares a folder that is not shared automatically, for
one run.

## Which folders are shared

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

A root is always refused. The share list is per computer: with several profiles every gateway gets the folder on its
first agent request.

**Everything inside a shared folder is shared**, hidden files included (`.claude/`, `.env`, `.secret/`). Keep
credentials out of project folders you use with a gateway, or list them in `.gitignore` for sync and worktree mode.

If minigw prints that the folder "is not shared with the gateway", run `minigw share add` or `/minigw:share`.

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
| `claude-grok`, `claude-grok-<model>` | xAI Grok Build CLI (`grok -p`) | yes | all, off by default |
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

**Codex** runs one entry per enabled Codex model of your account. Its sandbox writes only to the session folder and
the allowed folders. Within a conversation Codex continues its own thread (`codex exec resume`), also after
`minigw -c` or `--resume`.

**Grok** runs in its kernel sandbox: `workspace` (writes only to the session folder) or `read-only`. Within a
conversation it continues its own session (`-r`).

**Copilot** runs in a sandbox (on the Mac a `sandbox-exec` profile, on Linux bwrap, on Windows the Windows sandbox) with
a fresh Copilot home for each request, deleted afterwards. On a Windows gateway without the Windows sandbox, Copilot
and Grok run only with **Allow commands without a sandbox**; Codex on Windows always needs that switch.
`claude-copilot` uses Copilot's default model (`auto`); list other models by name in its settings.
- It only reads the folder unless **Allow writing** is on.
- Commands need **Allow commands** together with **Allow writing**, and a dedicated GitHub token: a fine-grained
  personal access token with only the Copilot Requests permission (`minigw gw copilot token`). Commands can use that
  token to make Copilot requests on your account. `gh` and `security` stay denied.
- The Mac app needs that token for every request. `minigw serve` on Linux may also use the GitHub CLI's sign-in
  (`gh auth login`), and on Windows the user's own `copilot login` (only without the Windows sandbox).
- Within a conversation Copilot continues its own session (`--session-id`).

**Logins (`minigw serve`).** Agents use your subscription logins, never API keys, and nothing is copied back from an
agent run. Each agent has a **Login** row (settings page, also the Gateway tray's Settings window; `minigw gw
settings`; `minigw gw <agent> login`) that shows only the state (set, not set, logged in) and a one-line hint, never
the value: Claude CLI its `claude setup-token` token (`minigw gw claude token [remove]`, needed in the Windows
sandbox), Codex its ChatGPT login (`codex login`), Grok its `grok login`, Copilot its token for commands. **Use an API
key instead** (Claude CLI, Codex, Grok; `minigw gw claude|codex|grok apikey [remove]`) stores a key in the key store
and passes it only then; it is billed through the provider's API, not your subscription. API keys in the gateway's own
environment (`ANTHROPIC_*`, `OPENAI_API_KEY`, `CODEX_API_KEY`, `XAI_API_KEY`, `GROK_CODE_XAI_API_KEY`,
`GEMINI_API_KEY`, `GOOGLE_API_KEY`) never reach an agent. The Mac app has no Login row: Claude uses your Keychain login.

### Windows gateway sandbox

`minigw serve` on Windows (MSI, part Gateway) runs Claude CLI, Copilot and Grok in the Windows sandbox, Microsoft MXC
(`mxc\wxc-exec.exe` in the install folder, no admin rights). It needs Windows 11 build 26100.9278 / 26200.9278 or
later (the Windows process container). Settings page → General → About shows "Agent sandbox: Windows sandbox (MXC
1.0.1)", or "not available (reason)"; then agents run only with **Allow commands without a sandbox**, as in 0.5.22.
With the sandbox that switch is not needed for Claude CLI, Copilot and Grok (Codex still needs it, see below).
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
- The sign-in the agent runs with (Claude's `claude setup-token` token, Copilot's token, or the API key you chose
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