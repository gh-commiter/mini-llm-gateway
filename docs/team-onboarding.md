# Team onboarding: the shared knowledge base

One page for a small team that starts using a Mini Gateway knowledge base. Names below are examples: team `acme`, project `shop`, colleague `ana`. Part 1 is for the gateway owner, part 2 for each colleague. The commands are the same on a Mac gateway (the app) and on `minigw serve` (Windows, Linux, Docker); in Docker prefix them with `docker compose exec minigw`.

**How the knowledge is laid out.** A team's own notes are the general segment: what everyone on `acme` should know. Each project is a topic part: what is specific to `shop`. A project can depend on another project or on a team, and then its members' searches cover that too (`project depends`). A dependency is only the address, never a permission: a project reads another project's notes through it only where `gw grant` (or a role) already allows that, and a grant alone is not found by search or `note read <id>` from the receiving project's folder, so give both (`gw grant <source> add <project> structure`, then `gw project depends add <project> <source>`). Team membership alone grants no access to any project.

## 1. Gateway owner

On the gateway computer. `minigw gw status` must say `projects: on`.

```sh
minigw gw user add ana
minigw gw team add acme
minigw gw project add shop "Shop"
minigw gw team member add acme ana
minigw gw team project add acme shop
minigw gw team access set acme shop ana developer     # roles: reader, developer, admin (admin = reviewer, see below)
minigw gw project depends add shop --team acme         # shop's searches also cover the team notes of acme
minigw gw token                                        # the connect line for colleagues (contains the token)
```

Give each colleague the connect line. When they connect, their device is waiting: run `minigw gw devices`, ask them to read out the device ID that `minigw status` shows on their computer, and compare it with the one you see. Only then:

```sh
minigw gw devices allow <device id> --name ana-laptop
minigw gw bind <device id> ana                          # a device belongs to one user
```

Submissions from colleagues wait for you: `minigw gw kb review shop` (or `--team acme`), then `minigw gw kb accept <id> --version <hash>` (`--version`, the whole version line that review printed, is required without a terminal; in a terminal, `accept` without it shows the text and asks for a "y") or `minigw gw kb reject <id> --reason "..."`. You can review everything. A project admin reviews the submissions of their project too, with `minigw kb review|accept|reject` (never their own submission; `accept` names the version that was read), so giving someone the role `admin` makes them a reviewer of that project's knowledge base: give `developer` when that is not wanted. A team's notes are written, and reviewed, by a project admin of the team. A reviewer is told what waits without looking: `minigw project show` and `minigw team show <team>` print a line such as `2 submissions wait for your review in team acme: minigw kb review --team acme`, and with `minigw kb hooks on` a session start prints the same line (it asks the gateway once, for at most 2 seconds).

The same things are in the Settings window of the Mac app and on the settings page of `minigw serve` (`minigw gw settings link` prints a one-time link): four panes, Users and devices, Teams and projects, Knowledge base and Upstream gateways. Remote settings (`minigw gw settings password`) manage providers and devices, show users, bindings, teams and projects read-only (menu item "Team (read-only)"), and review pending knowledge-base submissions (menu item "Review": accept or reject the version you read); everything else stays with the CLI.

## 2. Colleague

1. Install Claude Code, then the plugin: `/plugin marketplace add gh-commiter/mini-llm-gateway#release` and `/plugin install minigw@mini-gateway`. You do not need `/minigw:gw install`: that sets up a gateway.
   **Where `minigw` runs.** The plugin keeps its own `minigw` and puts it on the PATH of Claude Code's shell only, so in a normal terminal `minigw` is "command not found". Either run every `minigw …` below inside a Claude Code session (the plugin puts its own `minigw` first on the PATH of the session's shell; ask Claude to run it, or type `! minigw …` and check `! minigw --version` first: it must be the plugin's version, which the session start line names), or install the client for your terminal: macOS or Linux `curl -fsSL https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/install.sh | sh`, macOS also `brew install --cask gh-commiter/mini-gateway/minigw`, Windows the MSI ([install.md](install.md)). Only `minigw connect … --token -` needs a real terminal (the token is typed hidden); inside Claude Code use `/minigw:connect` instead. Everything else works as `! minigw …` too.
   A `minigw` that is already installed on the computer and is older than the release (for example an old Homebrew one) is not the plugin's: the session start names it and how to update it (Mac: `brew upgrade --cask gh-commiter/mini-gateway/minigw`; Linux: the `install.sh` line above; Windows: the newer MSI). Update it, so that a `minigw` typed in a terminal does the same as the plugin's.
2. Connect with the line the owner gave you: `/minigw:connect <gateway> <token>` in Claude Code, or `minigw connect <gateway> --token -` in a terminal with the client installed. Send the owner your device ID (`! minigw status`), wait for the approval, then connect again.
   Until the owner has allowed and bound your device, a project command answers `no project access: this device is not on the gateway's device list` or `... is not bound to a user`; send the owner the device ID it names. A wrong or old token answers `invalid gateway key ...: copy the token again`: ask for the connect line again.
3. In your repository: `! minigw project link shop` (without a slug the project is found by the repository's git remote, once an admin registered it with `minigw project remote add --here`). `minigw project show` says what applies. Use sync mode (`--minigw-work=sync`), not worktree, for a repository you do not trust: in worktree mode a repository's own git filters run on your machine.
4. Read:
   ```sh
   minigw kb search "invoice import"            # also covers the team notes and what shop depends on
   minigw note read <id>
   minigw kb faq
   minigw team list ; minigw team show acme
   ```
5. Hand in:
   ```sh
   minigw note push --class structure --title "Order states" notes.md     # you may write
   minigw kb submit shop --kind finding --title "..." finding.md          # proposal, a reviewer of the project accepts it
   minigw kb submit shop --kind faq --title "How do I ...?" answer.md
   ```
   `private` is visible to you and the project admins only. Never put secrets, customer data or personal data in a note.
6. Claude Code: `minigw kb mcp --project shop --print-config` prints a configuration for the read-only tools `kb_search` and `note_read`; review it and add it to your MCP configuration yourself. Optional: `minigw kb hooks on shop` makes a session ask, after a commit or a compaction, to hand in up to three short entries. It is off by default and `minigw kb hooks off` stops it.
7. Usage labels: add `--minigw-phase`, `--minigw-activity` or `--minigw-task` to a run (or `MINIGW_PHASE`, `MINIGW_ACTIVITY`, `MINIGW_TASK`) so that `minigw usage` and the owner's `minigw gw usage --by phase|activity|task` say what the run was for.

## What is new and little tried

Teams, the knowledge base, `kb mcp` and the hooks are recent and not yet in daily use. The panes have been tried through tests, and the settings page on one Windows gateway in a headless Chromium browser; the hooks not in a real Claude Code session. Review is by the owner and the project's admins (four eyes: not their own). The current list is under Known problems in the [release notes](release-notes.md). More: [usage.md](usage.md#projects-in-10-minutes), [parity.md](parity.md).
