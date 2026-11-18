# Team onboarding: the shared knowledge base

One page for a small team that starts using a Mini Gateway knowledge base. Names below are examples: team `acme`, project `shop`, colleague `ana`. Part 1 is for the gateway owner, part 2 for each colleague. The commands are the same on a Mac gateway (the app) and on `minigw serve` (Windows, Linux, Docker); in Docker prefix them with `docker compose exec minigw`.

**How the knowledge is laid out.** A team's own notes are the general segment: what everyone on `acme` should know. Each project is a topic part: what is specific to `shop`. A project can depend on another project or on a team, and then its members' searches cover that too (`project depends`). Team membership alone grants no access to any project.

## 1. Gateway owner

On the gateway computer. `minigw gw status` must say `projects: on`.

```sh
minigw gw user add ana
minigw gw team add acme
minigw gw project add shop "Shop"
minigw gw team member add acme ana
minigw gw team project add acme shop
minigw gw team access set acme shop ana developer     # roles: reader, developer, admin
minigw gw project depends add shop --team acme         # shop's searches also cover the team notes of acme
minigw gw token                                        # the connect line for colleagues (contains the token)
```

Give each colleague the connect line. When they connect, their device is waiting: run `minigw gw devices`, ask them to read out the device ID that `minigw status` shows on their computer, and compare it with the one you see. Only then:

```sh
minigw gw devices allow <device id> --name ana-laptop
minigw gw bind <device id> ana                          # a device belongs to one user
```

Submissions from colleagues wait for you: `minigw gw kb review shop` (or `--team acme`), then `minigw gw kb accept <id>` or `minigw gw kb reject <id> --reason "..."`. Only the owner reviews; a project admin cannot. A team's notes are written by a project admin of the team.

The same things are in the Settings window of the Mac app and on the settings page of `minigw serve` (`minigw gw settings link` prints a one-time link): four panes, Users and devices, Teams and projects, Knowledge base and Upstream gateways. Remote settings (`minigw gw settings password`) show providers and devices only, not teams or review.

## 2. Colleague

1. Install Claude Code, then the plugin: `/plugin marketplace add gh-commiter/mini-llm-gateway#release` and `/plugin install minigw@mini-gateway` (the plugin fetches `minigw`; other installers: [install.md](install.md)). You do not need `/minigw:gw install`: that sets up a gateway.
2. Connect with the line the owner gave you: `/minigw:connect <gateway> <token>`, or `minigw connect <gateway> --token -`. Send the owner your device ID (`minigw status`), wait for the approval, then connect again.
3. In your repository: `minigw project link shop` (without a slug the project is found by the repository's git remote, once an admin registered it with `minigw project remote add --here`). `minigw project show` says what applies.
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
   minigw kb submit shop --kind finding --title "..." finding.md          # proposal, the owner reviews
   minigw kb submit shop --kind faq --title "How do I ...?" answer.md
   ```
   `private` is visible to you and the project admins only. Never put secrets, customer data or personal data in a note.
6. Claude Code: `minigw kb mcp --project shop --print-config` prints a configuration for the read-only tools `kb_search` and `note_read`; review it and add it to your MCP configuration yourself. Optional: `minigw kb hooks on shop` makes a session ask, after a commit or a compaction, to hand in up to three short entries. It is off by default and `minigw kb hooks off` stops it.
7. Usage labels: add `--minigw-phase`, `--minigw-activity` or `--minigw-task` to a run (or `MINIGW_PHASE`, `MINIGW_ACTIVITY`, `MINIGW_TASK`) so that `minigw usage` and the owner's `minigw gw usage --by phase|activity|task` say what the run was for.

## What is new and little tried

Teams, the knowledge base, `kb mcp` and the hooks are recent and not yet in daily use. The panes have been tried through tests, and the settings page on one Windows gateway in a headless Chromium browser; the hooks not in a real Claude Code session. Review is a one-person step. The current list is under Known problems in the [release notes](release-notes.md). More: [usage.md](usage.md#projects-in-10-minutes), [parity.md](parity.md).
