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
- Gateway requirements for nested repositories. Mac app: none. Linux `minigw serve`: bwrap must work, and for Claude
  CLI also bwrap inside bwrap, so every agent runs in its own process namespace. On Ubuntu 24.04 AppArmor's
  `bwrap-userns-restrict` profile blocks the inner one; allow it (for example with an AppArmor profile for
  `/usr/bin/bwrap` that grants `userns`, or `sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0`) or use sync.
  Without that, a request with nested repositories is refused before the agent starts (a final error: Claude Code does not retry it). macOS `minigw serve` (for
  development) refuses nested repositories; Windows `minigw serve` handles them. `minigw serve` also needs git 2.40 or
  later on the gateway; with older git the request is refused ("nested git repositories need git 2.40 or later on
  the gateway; update git or use sync").
- The agent may commit inside a nested clone: its note names the clones and the form
  `GIT_DIR=<path>/.git GIT_COMMON_DIR=<path>/.git GIT_WORK_TREE=<path> git …` (plain git in the clone works on the top
  repository). Its commits are kept; changes it leaves uncommitted are committed by the gateway. Codex can commit there;
  Claude CLI agents on the Mac app gateway cannot run git yet (its sandbox), so the gateway commits their changes.
- A nested repository in a folder git ignores (for example `build/_deps` from CMake FetchContent) stays out, as before.
  If the agent itself creates a repository in the clone, minigw prints "changes inside the nested repository <path>
  do not come back in worktree mode; use sync" and no branch is delivered for that run.

**Agents in worktree mode:**

- Claude CLI agents get Bash, always in Claude Code's sandbox. Codex uses its own sandbox with the clone as the
  writable folder.
- Agents that may not write (Codex, Copilot or Grok with writing off) only read in the clone and bring back no branch.
- Grok with writing on is refused in worktree mode: "rerun with minigw --minigw-work=sync …".
- On the Mac, agents write only to the clone, their own Maven repository, DerivedData and temporary folders, never to
  your `~/.m2` settings, plugins or SwiftPM configuration. Claude cannot read your home folder (from `~/.m2` only
  `settings.xml` and `toolchains.xml`). Maven
  builds work; `swift build` and `xcodebuild` may fail there. Build Swift and Xcode projects in sync mode or with
  [`minigw run`](#native-mode-and-builds-on-the-gateway-mac).
- Network for builds (Mac): on for Claude, off for Codex. Change it in Settings → General → Worktree mode.
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
  minigw must read it back through its mount.
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
gateway commands need **Allow commands without a sandbox**.

**Codex** runs one entry per enabled Codex model of your account. Its sandbox writes only to the session folder and
the allowed folders. Within a conversation Codex continues its own thread (`codex exec resume`), also after
`minigw -c` or `--resume`.

**Grok** runs in its kernel sandbox: `workspace` (writes only to the session folder) or `read-only`. Within a
conversation it continues its own session (`-r`).

**Copilot** runs in a sandbox (on the Mac a `sandbox-exec` profile, on Linux bwrap) with a fresh Copilot home for
each request, deleted afterwards. Windows has no sandbox: there Copilot, Codex and Grok run only with **Allow commands
without a sandbox**. `claude-copilot` uses Copilot's default model (`auto`); list other models by
name in its settings.
- It only reads the folder unless **Allow writing** is on.
- Commands need **Allow commands** together with **Allow writing**, and a dedicated GitHub token: a fine-grained
  personal access token with only the Copilot Requests permission (`minigw gw copilot token`). Commands can use that
  token to make Copilot requests on your account. `gh` and `security` stay denied.
- The Mac app needs that token for every request. `minigw serve` on Linux may also use the GitHub CLI's sign-in
  (`gh auth login`), and on Windows the user's own `copilot login`.
- Within a conversation Copilot continues its own session (`--session-id`).

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
