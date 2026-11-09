# Release notes

## Unreleased

- One command on every OS for the Claude Code plugin, compatible with the Claude Code CLI: `minigw plugin install`, `minigw plugin uninstall` and `minigw plugin status`. `install` adds `minigw@mini-gateway` to `~/.claude/settings.json` with the rules the Homebrew cask used (a `false` you set stays, a file that is not valid JSON is never overwritten, the file mode stays) and points the plugin's `bin/minigw` at this minigw (on Windows `minigw` and `minigw.cmd` shims, since Git Bash does not find a `.cmd` by itself). `status` shows the minigw, Claude Code, plugin and gateway versions and which minigw the plugin uses; `/minigw:version` runs it. The gateway returns its version to clients with a token. Both clients.
- `minigw uninstall [--purge] [--yes]` lists and then removes what minigw added on this computer: the plugin entries and data folder, the sign-in start of `minigw serve` (systemd user unit, Run value without the MSI, an old LaunchAgent) and scratch folders. Settings and profiles stay unless you add `--purge` (macOS Keychain items only through minigw's own Keychain calls). It prints how to remove minigw itself (Homebrew, MSI, `rm`). Both clients.
- The plugin uses the installed minigw (Homebrew/app, MSI, `~/.local/bin`) whenever it is not older than the plugin needs, as a link (Windows: shim) instead of a copy, and downloads only when no minigw is installed: one minigw, updated by its installer.
- `minigw plugin install` does not link a minigw that is, or lies in a folder that is, writable by other users or not owned by you or root. `minigw plugin` and `minigw uninstall` refuse to run as root. Both clients.
- When a signed release is newer than the installed minigw, the session start says so also for a minigw linked by the plugin; a Linux client without `minigw serve` gets the replace-the-file hint instead of `/minigw:gw update`.
- The Homebrew `minigw` cask calls `minigw plugin install` / `minigw plugin uninstall` instead of its own Ruby.
- New: `minigw gw settings` shows the gateway settings in the terminal, also over SSH and inside tmux, on the Mac app and on `minigw serve`: General, Devices, Providers, Agents, Folders, Sessions, and on the Mac a page for Siri AI, Apple Foundation Models and `minigw run`, so the Mac is fully manageable over SSH. Keyboard first, mouse optional, no colors. It is a new helper, `minigw-tui` (Go, Bubble Tea), shipped in the app (`Contents/Helpers`, signed), `minigw.zip` and the `minigw` cask, the MSI, the release (`minigw-tui-<system>-<arch>`), and installed by `install.sh` and `minigw serve install` (Windows and Linux; uninstall removes it). It never reads the config, the key store or the admin secret: every read and change runs `minigw gw api` of the same minigw, so the admin API's rules apply; typed keys and tokens go on stdin and are never shown. The Mac's `minigw` starts it only with a valid Mini Gateway signature. `minigw` itself still uses only the Go standard library.
- New: `minigw gw api GET|POST|PATCH|DELETE <path>` (both clients): one admin API request, JSON body on stdin, JSON answer on stdout. It is for `minigw-tui`; the plugin's skills use the named `gw` commands.
- New: Mac admin API `GET/PATCH /appleFM` (`enabled`, `instructions`), as in Settings → Apple Foundation Models.
- Windows MSI: choose the parts in its window or with `ADDLOCAL=` for `/passive`: Client (always), Gateway (tray at sign-in, Start menu, `minigw-tui`) and Claude Code plugin (runs `minigw plugin install` as you, without a console window), both on by default. Open it again (Settings → Apps → Modify) to change the parts or to remove it with *Also remove settings and profiles*; `PURGE=1` does the same without the window (`minigw uninstall --purge --yes` before the files go). Uninstall in Settings → Apps keeps settings and profiles. An upgrade asks nothing, keeps the parts and starts the tray again only if it was running; upgrading from 0.5.19 starts the tray once even if it was not running (0.5.19 does not record it). Before closing the tray the installer waits up to 2 minutes for running agent requests. Without Claude Code (no `claude`, no `~/.claude`) the plugin part adds nothing; the installer's minigw output goes to `%LOCALAPPDATA%\minigw\minigw-msi.log`. On a client-only install, `minigw serve install` (`/minigw:gw install`) adds the Gateway part with the release's MSI.
- On Windows without a gateway (client-only MSI), the session start's update line points at the MSI of the latest release instead of `/minigw:gw update`.
- Homebrew: `brew uninstall --zap --cask minigw` also runs `minigw uninstall --purge --yes`; `brew uninstall --zap --cask mini-gateway` lets the app delete its own Keychain items and login item (`MiniGateway --zap`: it asks first, Cancel is the default; no Keychain prompt, never the `security` command; it refuses while another copy of the app runs) and trashes its Application Support, cache (scratch), preferences, HTTP storage and saved window state. `brew uninstall` without `--zap` is unchanged.
- `install.sh` now also runs on Linux (x64, ARM64): it downloads `minigw` and `minigw-tui`, checks them against the signed `SHA256SUMS` and installs them in `~/.local/bin`. Parts: `--client`, `--plugin`, `--gateway` (Linux: `minigw serve init` and `minigw serve install`); default client and plugin, also on macOS (`minigw plugin install`). `--uninstall [--purge]` runs `minigw uninstall` and removes the files it installed. It refuses to run as root.
- Changed: on Windows and Linux, `minigw gw settings` used to print a one-time link to the settings page; that is now `minigw gw settings link` (the tray's **Settings…** is unchanged).

### Security

- Windows `minigw` folder share: names ending in a dot or space and reserved device names (`CON`, `NUL`, `COM1` …) are refused.
- Release builds also require `install.sh`, `scripts` and `plugins` to be committed (`install.sh` is in the signed `SHA256SUMS`).
- macOS `minigw`, direct mode: copying a folder in the shared folder (WebDAV COPY) copies only files and folders, not symbolic links, as the Windows and Linux `minigw` do.
- Mac gateway: a request body over 1 MB from another computer is read only after the token in its header is valid, and at most 256 connections wait for a full request at a time (as `minigw serve`).
- Inside Claude Code, `--minigw-work=direct`, `sync` and `native-direct` are refused unless the run is read-only or the folder is saved with that mode (`minigw share add --work=`): these modes change your files without a branch to review, and the skills' model calls do not ask. `MINIGW_WORK=<mode>` in front of the command still works, and Claude Code asks for it. Dropping `--minigw-ro` is still not asked (documented limit). Both clients.
- The note about a file that changed during sync starts with `sync:`, so a file name cannot make it look like the branch line; the skills say the branch line comes on stderr after the answer. Both clients.
- Worktree mode with Claude CLI on Linux `minigw serve` without the `.git` pin (nested bwrap restricted): the gateway now watches the clone's `.git` with inotify instead of only checking it every second, so a replaced `.git` stops the run and the processes in the clone even if it is put back before the next check. If the watch cannot be set up, the run is refused (use sync).
- Worktree mode with read-only (`--minigw-ro`) is refused before the folder is cloned on the gateway, not after. `minigw serve`.

## 0.5.19

- Security: minigw inside Claude Code (when `CLAUDECODE` is set) passes on only claude options that do not widen permissions: `-p`/`--print`, `--model`, `--output-format`, `--verbose`, `--max-turns`, `--append-system-prompt`, `--fallback-model` and `--help`. Every other option is refused, for example `--dangerously-skip-permissions`, `--permission-mode`, `--allowedTools`, `--settings`, `--mcp-config`, `--add-dir`, `--plugin-dir`, `--input-format` (`stream-json` control requests can add an MCP server or change the permission mode) and `-c`/`--continue`, `-r`/`--resume`, `--session-id` (they would send another session's transcript to the gateway). Before, a model could run `minigw --dangerously-skip-permissions -p …` through a plugin skill and the nested claude ran every tool call without asking. Everything after `--` is the prompt, so a prompt may start with `-` (the skills pass `-- "<prompt>"`). Both clients.
- Security: minigw inside Claude Code also checks the prompt (words that are not options, everything after `--`, and stdin, which it passes on unchanged) and refuses with exit 1 a prompt that starts with `/` or has `@` at the start of a word: after a space, Unicode spaces (U+00A0, U+3000, U+2028 …), U+FEFF, `\v`, `(`, `"`, `'` or `。、？！`. The nested `claude -p` reads its prompt as your input: `@~/.ssh/id_ed25519` attached that file to the gateway request, and `/config permissionMode=acceptEdits` changed the permission mode of all your Claude Code sessions. `a@b.com`, `2 / 3` and `<model>@<profile>` after `--model` still work. The check is a backstop: whenever claude reads its prompt as input (`-p`/`--print`, stdin that is not a terminal, or inside Claude Code), minigw starts it with `CLAUDE_CODE_DISABLE_FILE_MENTIONS=1` and `--disable-slash-commands`, so no `@` mention attaches a file and skills and slash commands are off in that run (measured with Claude Code 2.1.318). An interactive `minigw` session in a terminal keeps both. The skills name files without `@`, and `/minigw:review` gives the reviewer the git range and a description of the change instead of a pasted diff. `MINIGW_ALLOW_UNSAFE_FLAGS=1` turns the option and prompt checks off. Both clients.
- Security: the gateway's own Claude CLI runs (agent, read-only, chat, worktree, and Claude asked by another agent) start with `CLAUDE_CODE_DISABLE_FILE_MENTIONS=1` and `--disable-slash-commands`. Before, a device or a repository file could make the gateway's Claude attach a file of the gateway user (`@~/.ssh/id_ed25519`), outside the agent's sandbox, or change the gateway owner's permission mode with `/config`. Mac and `minigw serve`.
- Security: the plugin's skills no longer pre-approve every minigw command (`minigw *`), only read-only commands and model calls (for example `minigw -p --model …`, `minigw --version`, `status`, `models`, `profiles`, `gw status`, `gw devices`, `gw devices history <id>`, `gw sessions`, `gw provider list`, `gw autostart` without an argument, `share list`). Commands that change the gateway or this client ask you first, also when the model starts the skill (`gw claude …` and other `gw` settings, `gw devices allow|rename|deny`, `gw token`, `gw provider add|remove|models`, `gw sessions clear`, `gw settings`, `serve …`, `connect`, `disconnect`, `profiles default`, `share add|remove`). No pattern starts with a wildcard that could hide a subcommand: the skills put `--minigw-ro`, `--minigw-role`, `--minigw-profile` and `--minigw-work` after `--model <model>`, or after `status`/`models`.
- Security: read-only mode (`--minigw-ro`, `MINIGW_SHARE=ro`; used by `/minigw:review` and `--ro` of `/minigw:ask`, `/minigw:codex`, `/minigw:copilot`) is enforced also when the gateway runs on the same computer: minigw sends the header `x-claude-mini-ro: 1`, and the gateway runs every agent read-only when the share or the header says so (before, Codex, Grok and Copilot could change your files there). Update both minigw and the gateway: an older gateway ignores the header. An agent asked by another agent keeps its caller's read-only setting. Claude CLI is really read-only: only Read, Grep, Glob, LS, WebSearch and WebFetch (Edit, Write and the other writing tools are refused, tools added to Claude Code later are off), plus Bash only in a sandbox that cannot write to the session folder or the allowed folders. Worktree mode with read-only is refused by the gateway; a resumed Codex thread on the Mac stays read-only. Session history records these runs as read-only with the tools they had, and Codex `network` only when Codex can write. Mac and `minigw serve`.
- Security: protected paths, one list for direct mode (the mount refuses writes with 403) and sync (never copied back): `.git` (any letter case, also `git~1`), `.claude/settings*.json`, `.claude/hooks`, `.claude/commands`, `.claude/agents` and `.claude/skills` (also in subfolders), `.husky`, `.githooks`, `.vscode/tasks.json` and `.envrc`, also through a link in the folder. Moving, copying or deleting a folder that would create or remove one of them is refused; reading stays allowed. Agents in direct mode can therefore no longer `git commit` or change the folder's Claude Code settings: use worktree (the default for a git repository) for commits. A refused write prints `minigw: refused agent write to <path> (protected path)` once per path per run (at most 50 paths). A gateway on the same Mac shares no folder, so this does not apply there. Both clients.
- Security: worktree mode no longer follows a symbolic link in the folder path (a session in `a/.ssh` with `a -> /home/user` worked in the gateway user's real `~/.ssh`): every part of the path must be a real folder in the clone, otherwise the agent starts at the clone's root. Mac and `minigw serve`.
- Security: worktree mode with Claude CLI: an agent that replaced the clone's `.git` (a new config with a filter, or `info/attributes`) could make Claude Code's own `git status`, which runs outside the sandbox, run its command. The gateway records the clone's `.git` and checks it before reusing the clone and before every Claude CLI start: a changed `.git`, config, `info/attributes` or `commondir` means a fresh clone, or a 400 error in a running request. During a run, Linux `minigw serve` pins `.git` with bwrap when nested bwrap works; where nested user namespaces are restricted (Ubuntu 24.04 AppArmor `bwrap-userns-restrict`) it logs that the pin is off, so Bash in Claude keeps working, and the failed check is repeated after a minute. Without the pin, and on Windows and macOS `minigw serve`, the gateway checks `.git` on every Claude event and every second and stops the run with "the agent replaced its clone's .git during the run". Mac and `minigw serve`.
- Security: Mac, Claude CLI in your own folder (native, allowed folders, sync): the sandbox also protects `.git/commondir`, `.git/config.worktree` and `.git/worktrees`, so the agent cannot point git at another config or change a linked worktree's settings; the `minigw run` sandbox protects them in nested repositories too.
- Security: gateway text can no longer pose as minigw's `minigw: agent work is on branch …` line. Gateway messages start with `minigw: gateway:`, are one line without control characters, at most 500 characters, and one that claims a branch is dropped; sync messages and refusals are one line too. The skills accept only minigw's own line, with a base of `HEAD` or a full commit ID. Both clients.
- Security: `minigw models` cleans model IDs and descriptions (one line, no control characters, at most 100 and 200 characters), since `/minigw:models` and `/minigw:status` put them into Claude's context. Both clients.
- Security: in direct mode a gateway read of a pipe (FIFO) or device file is refused, so it cannot block minigw's share workers. Both clients.
- Security: Go client: a folder whose name starts with `..` (for example `/srv/..minigw`) counts as inside its parent, so the checks that keep the Scratch folder apart from allowed folders no longer miss it.
- Work mode auto is the default with a Mac gateway on another computer too, like Windows and Linux: the root of a git repository with a commit goes to a worktree branch, a subfolder or other folder is synced, read-only stays direct (an explicit worktree uses the parent repository). For the old behaviour use `minigw share add --work=direct`, or `MINIGW_WORK=direct` for every folder. Auto keeps a copy in the gateway's Scratch folder (worktree mirror 30 days, sync copy 14 days after the last use), readable by agents that can read all of the gateway user's files (Codex and Grok on the Mac, any agent on Windows), also other devices' agents. The worktree start line says that your uncommitted and untracked, not ignored files are sent too. Each delivered branch is printed once with only the agent's part: `agent work is on branch <branch>: review with git diff <base>..<branch> (<base> = your state when the agent started), then bring it in with git cherry-pick <base>..<branch> or git diff --binary <base>..<branch> | git apply`; `<base>` is minigw's snapshot of your changes, so `git diff HEAD...<branch>` or a merge would include them. Native mode in a git repository prints the same line (minigw checks the branch name, SHA and that the base is an ancestor). The skills use exactly that range, accept only `minigw/<agent>-<8 hex>-<n>` branches and apply the changes into the working tree (`git apply`, `git cherry-pick -n` or Edit), never merging or checking out the branch. Both clients.
- Worktree mode: Codex, Copilot and Grok without writing only read in the clone and bring back no branch (minigw says so) instead of 400 "use sync"; Grok with writing is refused with "rerun with minigw --minigw-work=sync …", and the skills ask before rerunning with sync. `--minigw-ro` with worktree or native against a Windows or Linux gateway uses a read-only sync instead of failing; an explicit `direct` is still an error there. The Mac gateway accepts worktree from a Windows client (it now treats `\` as `/`) instead of falling back to sync.
- A folder inside a temporary folder (`/tmp/project`, `mktemp -d`, `%TEMP%\project`) is shared like any project folder, so auto mode works there without `minigw share add`. Not shared: the temporary folder itself, hidden folders in it, other users' folders (not checked on Windows, where `%TEMP%` is per user), and Claude Code's top levels (`claude-<uid>`, its project and session folders; the session's `scratchpad` is shared). A `TMPDIR` of `/`, the home folder or above is ignored. Both clients.
- Direct mode: a case-only rename (`a.txt` → `A.txt` on APFS or NTFS) no longer deletes the file; a move onto the same file renames it; a copy onto itself and a move or copy of a folder into its own subfolder or onto a folder that contains it are refused (403). On Windows and Linux a file the gateway overwrites through the mount keeps its permissions (`build.sh` stays executable). Both clients.
- `minigw status` succeeds only when the gateway is reachable, the token is accepted and this device is approved; otherwise it exits 1 with a `Status:` line (`token rejected: …`, `device not approved: <the gateway's instructions>` or `gateway not reachable`). It prints `Device: <ID> (approved / waiting for approval)` and the `Models:` line only when connected. With several profiles each shows its own reason; the exit code follows the default profile. `minigw models` keeps the gateway's error text (401 token, 403 device approval), and a profile with a rejected token is no longer called "not reachable". `minigw connect … --token '#'` stops with "the token is empty or incomplete". Both clients.
- Agent limits (Codex, Grok, Copilot, Claude CLI): a usage limit, rate limit, quota or credits error is a 429 `rate_limit_error` with `x-should-retry: false`, so Claude Code shows it once instead of retrying for minutes (before: 502 "codex exited with 1"). The client shows only the CLI's cleaned text (for example `You have run out of credits or need a Grok subscription. …`); other agent failures stay 502 with the CLI's message (300 characters, no paths). Codex, Grok and Copilot send the stream head only after 30 s, so an early limit is a single real 429; a later one is an SSE event, and Claude Code's repeat without streaming gets the same 429 at once instead of a second run. Grok stops at its first error event. Copilot's message in `minigw serve` is fixed (details in the gateway log). Busy 429s (queue full) stay retryable. Mac and `minigw serve`.
- Plugin 0.5.19 needs minigw 0.5.19; with an older one the session hook says so and points to `/minigw:gw update`, which skips `serve install` when the gateway already runs the newest version. The session hook reports "connected", "token rejected", "device not approved (ID …)" or "not connected", stays within its 60 s, says when the standalone `claude` command is not on PATH (the VS Code extension's bundled CLI is not enough for the agent skills), and also runs after compaction. On Windows and Linux the plugin uses your installed minigw (MSI or `install.sh`) when it is not older than the latest release (copied into the plugin's folder), so it no longer stays on an older release; downloads keep the signature and downgrade checks. The Homebrew `minigw` cask links its minigw into the plugin's folder and adds the Claude Code plugin (`brew uninstall` removes it; `brew upgrade` keeps it and a `false` you set).
- Provider models (OpenRouter, LiteLLM, other gateways) get an ID that starts with `claude-` also when the name only contains "claude" or "anthropic" (`anthropic/claude-haiku-4.5` → `claude-anthropic/claude-haiku-4.5`), so Claude Code lists them in `/model`; the old ID still works. Mac and `minigw serve`.
- Mac Settings could show a value the gateway was not using (a change just before closing, or while the Claude CLI check failed, was dropped; an open window missed changes made elsewhere). The window applies pending changes on close or when the check passes and reloads on open. The `minigw serve` settings page also saves a field typed just before the window closes.
- macOS: `minigw gw` no longer asks for access to the "dev.trojar.minigateway" Keychain item: the app's `admin` item trusts minigw by its signature (identifier `dev.trojar.minigateway.cli`, team Y2M489RDV8), so updated minigw copies read it without asking and other programs cannot. The app rewrites the item on its first start. Until then minigw stops with "Mini Gateway has not given minigw access to its admin key yet: quit and open the Mini Gateway app once …"; a profile it cannot read without a dialog is left out of `status`, `models` and `profiles` and is an error only when in use.
- Session history: an invalid or too long role header is ignored instead of shortened, a trailing `--minigw-role` without a value is an error, and a session's role is that of its latest request. Mac and `minigw serve`.

## 0.5.18

- Sharing and sync send the whole folder, hidden files included (README and `minigw share add` help now say so): keep
  credentials out or list them in `.gitignore`.
- Windows: after installing or updating, the tray is started through Explorer, as from the Start menu, so it finds CLIs installed since sign-in (Codex) without a restart. The tray log's startup line lists the agents found (`codex=`, `grok=`, `copilot=`).
- On Windows, Codex can ask other models only with its unelevated sandbox: `[windows]` `sandbox = "unelevated"` in
  `~/.codex/config.toml` (the elevated sandbox cannot read the run's token file).
- Agents no longer get client variables in other letter case (`Anthropic_Base_Url` on Windows), and programs found through the current folder are ignored.
- Codex, Grok and Copilot agents are told the full path of `minigw` for asking other models (Codex's Windows sandbox runs commands without the gateway's PATH, so `minigw` was not found).
- Windows tray: Settings opens in its own app window (Edge, Chrome, Brave or Vivaldi; otherwise the default browser), and clicking "Settings…" again brings it to the front instead of opening a new tab. A window whose session expired is replaced by a signed-in one.
- Windows and Linux gateway (`minigw serve`): `minigw` without `--minigw-work` (and no `MINIGW_WORK` or saved mode) no
  longer fails with 400 "this gateway supports --minigw-work=sync and worktree": when the gateway refuses direct, the
  client picks worktree (the folder is the root of a git repository with a commit) or sync, as `auto` does. An explicit `direct` is still an error.
  Both clients.
- Settings page, Claude CLI pane: an allowed folder that the gateway left out at start (it contains or is inside the
  scratch folder) is listed, marked, and can be removed; after saving the folders the switch turns Claude CLI on without
  restarting the gateway. Turning it on before that gives a clear error and the switch goes back off. The reason names
  each folder once.
- Windows tray and the settings page: **Copy Token** copies only the token (`token#key`), as on the Mac; the tray item was
  "Copy connect line". `gw token` still prints the whole `minigw connect …` line;
  `minigw gw token rotate` prints the whole connect line on the Mac too; Show Token shows the token without `#key`.
- **No console windows on Windows:** the tray and every program it starts (claude, git, agents …) run without a console
  window; GUI windows such as the installer's progress stay visible. The MSI closes the tray with
  `minigw-tray.exe quit` instead of `taskkill` in a cmd window. `minigw serve install` on Windows now fails if `minigw-tray.exe` is not next
  to `minigw.exe` and cannot be downloaded, and Start at sign-in (`gw autostart on`, the tray menu, Settings) needs
  `minigw-tray.exe`: there is no fallback to `minigw.exe` at sign-in, which opened a console window. `minigw tray quit`
  stops only `minigw-tray.exe` of this user's Windows session.
- **Windows gateway fixes:** the tray started by the MSI now finds `claude`, `codex`, `git` … (PATH also
  from the registry and `%USERPROFILE%\.local\bin`). Agent calls on Windows use `minigw.exe` (not the tray) as
  `minigw -p …`, pre-approved for Bash and PowerShell (`Bash(minigw -p:*)`, `PowerShell(minigw -p *)`), unless `minigw.exe`'s folder is inside an allowed folder, the session folder or Scratch:
  then nothing is pre-approved and the full path is used; other Bash commands of an agent still need approval there (no
  sandbox). An invalid `serve.json` is written to the log and shown in a message box in front; `gw status` says the
  settings must be fixed. When an allowed folder contains the scratch folder, the reason why Claude CLI and Codex are off
  is in the log, `gw status`, `gw claude` and the Claude CLI pane; the tray log now also has the startup warnings.
- `minigw gw devices deny <id>` refuses options such as `--name` (it used to remove the device). **Allow any device with
  the token** can be turned on only while no device is allowed (page, Mac Settings, admin API 400).
- `gw status` on Windows says `minigw gw autostart off`; `gw autostart` without an argument shows the state (both CLIs).
  Scratch folder errors say that it must not contain the config folder (named).
- Sessions: a device on 127.0.0.1 that was never approved shows as this computer, not "(device removed)"; an allowed
  device has connection history also over 127.0.0.1; a request with an invalid session ID counts in a `t-…` session;
  streamed Claude CLI answers count input tokens. `minigw status` lists the providers of a `minigw serve` gateway; `/health/ready` names the providers only to this computer or a caller with the token (Mac too). No
  empty `.maven` folder stays after the last worktree clone. The Claude CLI pane shows changes from `gw claude`.
- `minigw` started inside Claude Code or Claude Desktop no longer gets 401 from the gateway: the inner `claude` no
  longer inherits the parent's `ANTHROPIC_*`, `CLAUDECODE` and `CLAUDE_CODE_*` variables (session, OAuth, base URL;
  `CLAUDE_CODE_GIT_BASH_PATH` stays). Both clients.
- `minigw --model <model>@<profile>` uses that profile for the whole run (folder share, sync, worktree, native), as
  `--minigw-profile <profile>` does; before, the folder went to the default gateway. Both clients.
- Windows and Linux: a gateway on the same computer (`minigw serve`, e.g. profile `local` on 127.0.0.1) gets the project
  folder shared automatically, as a remote one does (before only with `MINIGW_SHARE=1`). On a Mac the app on the same
  Mac still works in the folder itself.
- `--minigw-work=auto` or `worktree` in a git repository without a commit (also a folder inside one) uses sync at once,
  with one line why, instead of failing on every agent request with "cannot take a snapshot". Both clients.
- `minigw --version` prints only minigw's version (Claude Code's: `claude --version`); `minigw gw status` shows it on one
  line again.
- **Codex on a Mac gateway shared by several devices:** Codex's own sandbox can read every file of the gateway's user,
  so Codex agents are not isolated between devices for reads (other devices' synced copies and worktree clones in
  Scratch, and an agent run's token file there). Allow Codex only for devices you trust with that.
- **Native mode (Mac gateway) on a git repository works on branches (new default):** each agent run gets its own clone on the gateway
  and a branch `minigw/<agent>-<session>-<n>` from your current state (also uncommitted changes); the branch then appears
  in your repository (fast-forward only, no hooks, fsck), while your files, index and checked-out branch stay as they
  are. So parallel agents never edit the same files. **Changed behaviour:** to let agents change the folder itself as
  before, use `--minigw-work=native-direct` (or `minigw share add --work=native-direct`). In folders that are not a git
  repository, with `native-direct` and in direct mode, only one writing agent at a time works in a folder (also a folder
  inside it); the others wait up to 5 minutes, and `minigw run` takes the same lock for its build. Grok and Codex or
  Copilot without writing are refused on native git repositories ("use --minigw-work=native-direct"). Older minigw
  clients also get branches on a native git repository; update minigw to use `native-direct`. If a branch cannot be
  delivered (for example you committed to it, or it is checked out), minigw shows why. The delivery runs git with the
  gateway's settings only, in a sandbox, so nothing from the repository's own config runs.
- **Device history and sessions (Mac and `minigw serve`):** last seen and the client's OS and version now survive
  restarts, and each allowed device keeps its last 20 connections (Devices → History, `minigw gw devices history <id>`,
  `GET /devices/{id}/history`). A new **Sessions** pane lists the last 200 client sessions with device, folder, work mode
  and per-model requests, errors, refusals, time and tokens (`minigw gw sessions`, `GET /sessions`); **Clear History**
  clears them. It stores folder paths, branch names and client addresses, never text; the history is a 0600 file on
  the gateway's computer.
- `minigw --minigw-role NAME` (or `MINIGW_ROLE`) sends the caller's role to the gateway, which records it in the
  session history (both clients), with counts per model and role and the agent's restrictions (read-only, tools). It is a guard against mistakes, not a security boundary.
  A model asked by an agent (agent calls) counts in the caller's session, with the calling chain (`via`) and the
  caller's role; the asked agent cannot change either.
- `minigw serve` no longer has a default folder for agents (`folder` in serve.json, the settings field and
  `minigw gw claude --folder`): as on the Mac, an agent request without a shared, synced or worktree folder is refused.
  A stored `folder` becomes an allowed folder; `minigw serve --folder X` (and `serve init`) still works, adds X to the
  allowed folders and says it is deprecated.
- **An agent may ask another model (off by default):** with **May ask other models** on for Claude CLI, Codex, Grok or
  Copilot (Settings, the settings page, `PATCH /agents/{name} {"askOthers": true}`, `minigw gw claude --ask-others=true`),
  an agent on the gateway can ask another model of that gateway itself, for example a remote `claude-cli-opus` asking
  an OpenRouter model or `claude-codex-…`: its instructions name the gateway's `minigw -p --model <id> "…"`, which sends
  the question straight to a loopback port and token for that run only (`MINIGW_AGENT_URL`, the token in a file of that run: `MINIGW_AGENT_AUTH_FILE`), with
  the rights of your device, also from inside Claude Code's sandbox (through its proxy). The asked agent works in the
  same folder (sync copy, worktree branch or native folder) and the caller waits. One level deep, never the same kind of
  agent, at most 2 questions at a time per agent; the run token works only for that agent's own processes. Claude CLI
  needs **Allow local tools** (Bash) for it, Codex "Allow network access for commands". Calls to another gateway come
  later.
- **This user's apps without a token (off by default):** programs of the gateway's own user on the same computer can use
  the models and agents (`/v1/messages`, `/v1/models`) without the token, for example an app pointed at `http://127.0.0.1:8765`.
  The gateway checks the process behind the connection (same user, not one of its agents); the request needs
  `anthropic-version` or `x-api-key`, a loopback `Host` and no `Origin`, so web pages cannot use it. Other users, other
  computers, the admin API and device approval are unchanged. Switch: General → **Allow this user's apps without a
  token** (`allowLocalApps`). On `minigw serve` for Windows and Linux; on the Mac in the app. Sandboxed apps (Mac App
  Sandbox and Seatbelt, also commands in Claude Code's sandboxed Bash; Linux Flatpak and snap; Windows AppContainer)
  and apps in a web view still need the token. Docker and other containers, VMs and tunnels (`ssh -R`, ngrok…) running as you also count as this user's apps: with the switch on they can use agents in your allowed folders without a token.
- Updates never go back to an older signed release: SHA256SUMS carries a signed `VERSION` line, and the plugin's
  minigw update, `install.sh` and `minigw serve install` refuse a release older than the installed minigw
  (`--allow-downgrade`, or `MINIGW_ALLOW_DOWNGRADE=1` for the plugin, to go back on purpose).
- Allowed folders that contain Copilot's package cache are refused like the Scratch folder, and the package is
  unpacked (`copilot --version`) in a sandbox without network (Mac `sandbox-exec`, Linux bwrap).
- Foundry Local is used only when the process on its port runs from the Foundry installation.
- **Several gateways at once (profiles, #17):** `minigw connect <host> --name <profile>` keeps every gateway, for
  example the Mac mini and a Windows gateway side by side. Models of other profiles are `<model>@<profile>`; the
  default profile's also appear without a suffix. An unreachable gateway is left out of the list, and `minigw status`
  shows each profile. The existing connection becomes the profile `default` with nothing to do.
- **Worktree mode on `minigw serve` (Windows, Linux):** `--minigw-work=worktree` (and `auto` for a git repository) now
  also works with a Windows or Linux gateway, as on the Mac: the agent works in a clone on its own branch, and the branch
  comes back to your repository (fetched fast-forward only, no hooks or filters). Native mode and `minigw run` stay
  Mac-only.
  **Changed behaviour:** `--minigw-work=auto` on a git repository with a Windows or Linux
  gateway now gives you a branch `minigw/…` instead of changing your files through a synced copy. To keep the old
  behaviour, run `minigw share add --work=sync` in that folder (or `MINIGW_WORK=sync`).
- **Windows installer:** `minigw-windows-amd64.msi` and `minigw-windows-arm64.msi` install the gateway with the tray for
  this user, without admin rights (the same as `minigw serve install`: PATH, Start menu, start at sign-in). Listed in
  the signed `SHA256SUMS`; the MSI itself is not code-signed yet, so SmartScreen warns. On an MSI install,
  `minigw serve install` updates with the release's MSI (nothing happens when it is already up to date or newer), and
  `minigw serve uninstall` points to Settings → Apps. Reinstalling the same version replaces it quietly; the user PATH
  gets Mini Gateway once.
- **File and folder pickers on Windows:** with the tray, the settings page has **Choose…** next to Allowed folders and
  **Browse…** next to a program's path, which open the Windows file dialog. On Linux and without the tray you type the
  path.
- **Devices are the same on the Mac and on `minigw serve`:** a new device waits for approval (Settings → Devices, the menu
  bar or the tray, `minigw gw devices allow <id> [--name N]`) unless the list is empty and "Allow any device with the
  token" is on. Waiting devices show `<user>@<computer>`, the client's OS and minigw version; you can edit the name
  before you allow a device and rename it later (`rename`). Both panes show the allowed IP addresses, unencrypted
  connections, and waiting and allowed devices with their ID and when they were last seen; the Mac notifies you when a
  device starts waiting. Devices are identified by their ID only: `deny` and the admin API no longer take a name.
  Devices you already use stay allowed. Clients without a device key (minigw before 0.4.4, unencrypted clients from
  other computers) are accepted only while no device is allowed and "Allow any device with the token" is on: update
  minigw on such clients.
- **Providers:** the Mac has "Allow http (localhost, a private IP or a .local name only)" like `minigw serve`, with the
  same rule on both. Existing Mac providers at `http://localhost`, a private or loopback IP, or a `.local` name keep
  working (the switch is turned on for them). Any other `http` provider, for example at a public host or a name without
  a dot, no longer gets the API key: change its URL to `https`. LiteLLM and OpenRouter are always listed on
  `minigw serve` too (off until a key is set; they can be turned off, not removed or renamed), and on the Mac they can
  no longer be removed through `minigw gw provider remove`.
- `minigw gw claude --folders … --allow-local-tools=true|false` and `minigw gw autostart on|off` now work on Windows and
  Linux too (the same flags as on the Mac); `--allow-unsandboxed-bash` stays a `minigw serve` extra.
- The Mac's admin API has what `minigw serve` has: `PATCH /providers/{name}` (rename, on/off, URL, aliases),
  `GET /agents/claude/tools`, `GET /agents/{codex,grok}/models`, permission mode and setting sources for Claude CLI, and
  the scratch folder (moved like in Settings).
- **Claude CLI on the Mac no longer loads a folder's `.claude/settings*.json` by default** (setting sources empty, as on
  `minigw serve`): an agent with local tools could otherwise write hooks there that the next session runs outside the
  sandbox. If you never changed "Setting sources", it changes on its own; a value you set yourself stays.
- **Scratch folder rules** (Settings, `minigw gw`, admin API, Mac and `minigw serve`): not the disk root, your home folder
  or a folder above it, a temporary folder, or a folder inside or around an allowed folder of the agents; and an allowed
  folder cannot contain the scratch folder or sit inside it. The Claude agent sandbox never reads Scratch, also on an
  external disk. On the Mac a move waits until no minigw session runs.
- Plugin 0.5.16 (needs minigw 0.5.18): `/minigw:connect --name` and profiles in every skill, `/minigw:connect` explains how
  to approve a refused device, and `/minigw:gw` covers the Copilot token, autostart and the work modes.

## 0.5.17

- GitHub Copilot CLI as an agent model, compatible with the Claude Code CLI: `claude-copilot` and `claude-copilot-<model>`
  (Mac app and `minigw serve`; off by default). Read-only by default; writing, web access and commands are separate
  switches. Copilot always runs in a sandbox (Mac `sandbox-exec`, Linux bwrap) with a fresh Copilot home per request.
  It signs in with its own fine-grained GitHub token with only the Copilot Requests permission
  (`minigw gw copilot token` or Settings; required on the Mac and for commands, which also need a confirmation). Plugin 0.5.5: `/minigw:copilot`.
- `minigw run` can no longer write `~/Library/Caches`, where apps keep code they load unchecked at start (for example the
  Copilot CLI's package). `mvn`, `swift build` and `xcodebuild` builds work without it.
- If you used Codex with writing or `minigw run` in 0.5.15, run `xcrun -k` once to clear the xcrun cache.
- Agents and `minigw run` can no longer write this Mac's shared temporary and cache folders (`/var/folders/…`), where
  the xcrun cache tells `/usr/bin/git` and the other developer tools what to run; they get their own temporary folder.
  The gateway runs git directly, never through the xcrun cache. `minigw run` builds Maven, Swift and Xcode projects as
  before; a `swift build` or `xcodebuild` by an agent in worktree mode may fail (use `minigw run` or sync mode).
- `/minigw:connect` and `install.sh` pass the token on stdin, so it never appears in the process list (plugin 0.5.4).
- One command to install the gateway, compatible with the Claude Code CLI: `/minigw:gw install` (plugin 0.5.3).
  Mac: the app with Homebrew and Open at Login. Windows: the tray at sign-in. Linux: systemd, at boot when linger
  works. `/minigw:gw update` when a session start says a newer gateway is out.
- The settings page of `minigw serve` (Windows, Linux) has the panes of the Mac's Settings, with Codex CLI and
  Grok CLI.
- Codex and Grok on `minigw serve` are off by default and run only in a synced folder. On Linux they need `bwrap`
  (home and config folders hidden), on Windows "Allow commands without a sandbox". Each device gets its own Codex
  and Grok sessions there. Devices that use Codex or Grok can read every file the gateway's user can read.
- Codex network access for commands is off by default, also on the Mac (saved settings keep their value).
- The gateway refuses admin requests from its own agent processes (Linux, Windows; the Mac app already did).
- New names: Mini Gateway is the **gateway**; OpenRouter, LiteLLM and other upstreams are **providers**
  (Settings → Providers). `minigw gw …` and `minigw gw provider …` replace `minigw router …` and `router gateway …`,
  which still work in this release; the admin API has `/providers` (`/gateways` still works).
- Foundry Local (Microsoft) as an on-device provider on Windows, Linux and the Mac: `claude-foundry-<model>`, off by
  default (Settings → Foundry Local).
- Release files are signed: `SHA256SUMS.sig` (ssh-keygen, key not on GitHub). The plugin, `install.sh` and
  `minigw serve install` refuse a release whose SHA256SUMS is not signed by Mini Gateway; a first install without
  `ssh-keygen` stops with a message.
