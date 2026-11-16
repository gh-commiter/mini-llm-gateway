# Commands

Every `minigw` command and every plugin skill, with an example. `minigw --minigw-help` prints the short list,
`minigw gw help` and `minigw serve help` the gateway commands.

- [Client: minigw](#client-minigw)
- [Options and environment variables](#options-and-environment-variables)
- [Gateway: minigw gw](#gateway-minigw-gw)
- [Gateway: minigw serve (Windows, Linux)](#gateway-minigw-serve-windows-linux)
- [Plugin skills](#plugin-skills)

## Client: minigw

| Command | What it does |
|---|---|
| `minigw [claude options …]` | Run Claude Code through the gateway |
| `minigw connect <host[:port]> [--name P] [--token T \| --token -]` | Save a gateway as profile `P` (`-` reads the token from stdin) |
| `minigw reconnect [--name P] [--token T \| --token -]` | New token for an existing profile (default: the default profile); address, gateway key and device key stay. Without `--token` it asks for the token (no echo) or reads stdin. A token without `#<key>` keeps the saved gateway key. Then prints the `minigw status` lines of that profile; exits 1 when it is not connected |
| `minigw disconnect [--name P]` | Remove a profile (default: the default profile). The device key stays, so connecting again is the same device to the gateway; `minigw uninstall --purge` removes it |
| `minigw status` | Each profile: gateway and encryption, minigw and gateway version, providers, number of models, device ID and approval, status. Exits 1 when the default profile is not connected |
| `minigw models` | Models of all profiles; other profiles' models end in `@P` |
| `minigw profiles [list]` | List profiles; `*` marks the default |
| `minigw profiles default P` | Make `P` the default (its models also appear without `@P`) |
| `minigw share add [--work=MODE] [--ro] [folder]` | Share a folder that is not shared automatically, and/or save its default work mode |
| `minigw share remove [folder]` | Remove a folder from the share list |
| `minigw share list` | Show shared folders and their saved modes |
| `minigw team list [--archived] [--json]` | Your teams; directory membership gives no project access |
| `minigw team show <team> [--json]` | Your teammates and attached projects you can already read |
| `minigw project list [--archived] [--json]` | The gateway projects you can see, with your role and grants (see [usage.md](usage.md#projects-and-notes)) |
| `minigw project link <slug>` / `unlink` / `show` | Link this folder tree to a project you are a direct member of, remove the link, or show which link applies here |
| `minigw note list [--class c] [--limit n] [--after cursor] [--json]` / `read <id> [--json]` | Notes of the linked project (also inside an agent run, on the run's own project) |
| `minigw note push [--class structure\|data\|private] [--title t] [--key k] <file\|->` | Create a note (default class `data`); `--key` repeats a push whose answer was lost |
| `minigw note edit <id> [--class c] [--title t] [--revision n] [<file\|->]` / `rm <id> [--revision n]` | Change or delete a note (not inside an agent run) |
| `minigw usage [--project slug] [--days n] [--json]` | Your model usage per day and model: the linked project's, or your own events in all projects (see [usage.md](usage.md#projects-and-notes)) |
| `minigw run [--] <command> [args …]` | Run a build in this folder on the gateway Mac (native mode, allowed per device) |
| `minigw --version` | minigw's version (`claude --version` gives Claude Code's) |
| `minigw plugin install` | Add the Claude Code plugin `minigw@mini-gateway` to `~/.claude/settings.json` with its marketplace on the `release` branch and `"autoUpdate": true` (an entry from minigw 0.5.21 or older is updated; a `false` you set stays; invalid JSON is not changed) and point the plugin's `bin/minigw` at this minigw |
| `minigw plugin uninstall [--upgrading]` | Remove only what `plugin install` added, and `claude plugin marketplace remove mini-gateway` only for the entry minigw added (yours stays; not with `--upgrading`) |
| `minigw plugin status` | Versions of minigw, Claude Code, the plugin and the default gateway; which minigw the plugin uses. A plugin older than minigw: it prints `claude plugin marketplace update mini-gateway` and `claude plugin update minigw@mini-gateway` |
| `minigw uninstall [--purge] [--yes]` | List, confirm, then remove what minigw added here (plugin, sign-in start, scratch folders); `--purge` also profiles, the device key and settings. minigw itself stays: it prints the command for that ([install.md](install.md#plugin-and-uninstall-one-command-everywhere)) |

Examples:

```sh
minigw connect <gateway> --token -                        # paste the token, then Enter
minigw connect <gateway> --name win --token -            # add a second gateway as profile "win"
minigw reconnect --name win                              # the gateway "win" has a new token: paste it, then Enter
minigw -p --model claude-codex "Run the tests and fix the first failure"
minigw --model claude-cli-opus@win                       # interactive session with a model of profile "win"
minigw share add --work=sync                             # always sync this folder
minigw run -- mvn -q verify
```

The token contains `#`: quote it when you pass it in the command line (`--token '<token>'`), or use `--token -`.

Connections are stored in the macOS Keychain, in Windows Credential Manager, or on Linux in
`~/.config/minigw/connection.json` and `connections/<profile>.json` there (mode 0600). Profile names: lowercase
letters, digits and `-`, up to 32. The device key is stored on its own, once per client: a Keychain item (account
`device`), the credential `minigw:device`, or `~/.config/minigw/device-key` (mode 0600). minigw moves it there from
an existing profile on first use.

## Options and environment variables

These options go before the `claude` options. Each has an environment variable, for example for the `env` block of
Claude Code's settings.

| Option | Variable | What it does |
|---|---|---|
| `--minigw-work=MODE` | `MINIGW_WORK=MODE` | Work mode: `auto`, `worktree`, `sync`, `direct`, `native`, `native-direct` ([usage.md → Work modes](usage.md#work-modes-what-happens-to-your-files)). Inside Claude Code the flag with `direct`, `sync` or `native-direct` needs read-only or that mode saved for the folder; otherwise put `MINIGW_WORK=` in front |
| `--minigw-ro` | `MINIGW_SHARE=ro` | Share the folder read-only |
| – | `MINIGW_SHARE=0` / `1` | Do not share the folder / share it even if it is not shared automatically |
| `--minigw-tools none\|read` | | Tools of a provider model (LiteLLM, OpenRouter) that runs in the local Claude Code: `read` = Read, Grep, Glob only, `none` = no tools; no Bash, edits, MCP tools or sub-agents either way; your own Claude settings and hooks are not loaded for that run, and options that could widen the set are refused. Allowed inside Claude Code (it only takes tools away); `--minigw-ro` alone means `read` for these models; an error for agent models (use `--minigw-ro`). minigw prints `minigw: tools of this model: …` |
| `--minigw-role NAME` | `MINIGW_ROLE=NAME` | The caller's role, recorded in the gateway's session history |
| `--minigw-profile P` | – | Use only profile `P` for this command |
| `--model <model>@P` | – | Run on profile `P`'s gateway |
| – | `MINIGW_NATIVE_MOUNT=<share>=<folder>` | Tell minigw where a gateway SMB share is mounted, where it cannot detect it |
| – | `MINIGW_ALLOW_UNSAFE_FLAGS=1` | Turn off the option and prompt checks inside Claude Code ([how-it-works.md](how-it-works.md#what-minigw-passes-to-claude-code)) |

## Gateway: minigw gw

`minigw gw …` shows or changes the gateway **on this computer**: the Mini Gateway app on a Mac, `minigw serve` on
Windows and Linux. Changes apply at once. On a Mac the app must be running (except `minigw gw core restore`, which needs the
app quit); on Windows and Linux a stopped gateway's `serve.json` is changed instead. `minigw gw` talks to the admin API ([admin-api.md](admin-api.md)), which answers
only on the gateway computer.

| Command | What it does |
|---|---|
| `minigw gw status` | Running, version, autostart, agent requests, devices, providers, and a line `projects: on` or `projects: off (reason)` for the project database |
| `minigw gw sync list\|remove <copy id> --device ID` | List the synced copies of a device or remove an unused copy (stop its sessions and agents first) |
| `minigw gw devices` | Allowed devices and devices waiting for approval |
| `minigw gw devices allow <id> [--name N]` | Allow a device (compare the ID with `minigw status` on that client first) |
| `minigw gw devices rename <id> --name N` | Rename a device (the name is only a label) |
| `minigw gw devices deny <id>` | Remove an allowed device or reject a waiting one |
| `minigw gw devices history <id>` | The device's last 20 connections (time, address, OS, minigw version) |
| `minigw gw sessions [--device ID] [--folder DIR]` | Recent sessions |
| `minigw gw sessions <session id>` \| `clear` | One session in detail / clear the history (and the project counters) |
| `minigw gw usage [--days N] [--provider P] [--model M] [--sort failed] [--json\|--csv]` | Where requests, tokens, money and failed agent time go (all users, from the usage events): totals per provider and model sorted by known cost, then per UTC day, then a total line. Default 7 days. When any model has failed agent minutes (time from dispatch to the end of requests that ended in an error) the model table gets a "failed min" column and the total line says for example `7 errors, 58.0 failed agent minutes`; `--sort failed` puts the models with the most failed time first (not with `--json` or `--csv`). Cost shows reported charges plain, snapshot estimates with `~` (for example `~0.42`), mixed sums as `0.96 + ~0.42`, and `-` when unknown; `(+3 unknown)` counts requests without a usable cost. Subscription agents have no money. `--json` prints the API answer unchanged, `--csv` one row per day, provider and model with separate `reported_cost_usd` and `estimated_cost_usd` columns, then `errors`, `duration_ms`, `failed_duration_ms` and `unknown_duration_events` (text cells that start with `=`, `+`, `-` or `@` get a leading apostrophe) |
| `minigw gw usage --legacy [--days N] [--device ID] [--folder DIR]` | The older counters per folder, UTC day and agent (the last 7 days; 1-31). `--device` and `--folder` need `--legacy` |
| `minigw gw provider add <name> <baseURL> [--insecure]` | Add a provider; the API key is read from stdin or asked for |
| `minigw gw provider list` \| `remove <name>` | List or remove providers |
| `minigw gw provider models <name> [text] [--all] [--enable m1,m2\|all] [--disable m1,m2\|all]` | Show, enable or disable a provider's models |
| `minigw gw claude [--path FILE] [--tools T1,T2] [--folders DIR1,DIR2] [--allow-local-tools=true\|false] [--ask-others=true\|false]` | Claude CLI settings |
| `minigw gw claude --allow-unsandboxed-bash=true\|false` | Commands without a sandbox (`minigw serve` only) |
| `minigw gw claude --accept-agents-read-notes=true\|false` | Accept that agents without a sandbox can read all project notes; without it the core stays closed on such a Windows or Linux gateway; restart to apply (`minigw serve` only: Mac agents are sandboxed) |
| `minigw gw claude\|codex\|grok\|copilot [--work=direct\|sync\|worktree]` | Show or set the agent's cap: the highest work mode in which it may change the client's folder (in a lower mode it only reads). `grok`: `minigw serve` only; on the Mac gateway Grok is not supported and the command says so |
| `minigw gw claude\|codex\|grok\|copilot login` | The agent's login (set / not set / logged in), never the value |
| `minigw gw claude token [remove]` | The token from `claude setup-token` (needed in the Windows sandbox). Read from stdin or asked for. Copilot has no token: it uses the gateway user's `copilot login` |
| `minigw gw claude\|codex\|grok apikey [remove]` | Use an API key instead of the login (billed through the provider's API), read from stdin or asked for; `remove`: back to the login |
| `minigw gw scratch [move [DIR]]` | Show the Scratch folder (mirrors, clones, deps, caches, agent state); `move`: all of it to DIR (no DIR: the proposed folder), only while no minigw session or agent runs (`minigw serve` only) |
| `minigw gw autostart [on\|off]` | Mac: Open at Login. Windows: the tray at sign-in. No argument: show it |
| `minigw gw token [rotate]` | Print the connect line for clients; `rotate` makes a new token |
| `minigw gw settings` | The settings in this terminal, also over SSH: the pages General, Devices, Providers, Agents, Folders, Sessions, Activity (`minigw-tui`) |
| `minigw gw settings link` | Print a one-time link (60 s) to the settings page (`minigw serve` only) |
| `minigw gw api GET\|POST\|PATCH\|DELETE <path>` | One [admin API](admin-api.md) request: JSON body on stdin, JSON answer on stdout (`minigw-tui` uses it) |
| `minigw gw user list` \| `add <name>` \| `rename <user> <name>` \| `activate <user>` \| `deactivate <user>` | The users of the project database (0.6.0; also on the Mac). `<user>`: name or number. Deactivating ends the user's runs and capabilities |
| `minigw gw bind list` \| `<device id> <user>` \| `remove <device id>` | Which user a device belongs to |
| `minigw gw project list` \| `add <slug> <name>` \| `archive <project>` \| `unarchive <project>` | The projects (`<project>`: slug or number) |
| `minigw gw team add <name>` \| `list` \| `rename <team> <name>` \| `archive <team>` | Owner-managed directory; `<team>` is a name or number |
| `minigw gw team member add\|remove <team> <user>` | Change directory membership without changing project roles |
| `minigw gw team project add\|remove <team> <project>` | Group a project under at most one team without changing permissions |
| `minigw gw member <project> list` \| `set <user> <role>` \| `remove <user>` | Members of a project; role: reader, developer, admin |
| `minigw gw grant <source> list` \| `add <reader> <class>` \| `remove <reader> <class>` | Which project reads which class (structure, data) of another |
| `minigw gw core backup [name]` | A consistent copy of the core database into `<core>/backups` (the gateway or app running); a name, not a path |
| `minigw gw core restore <file\|name> [--yes]` | Only with the gateway stopped (on the Mac: with the app quit; the Mac `minigw` runs `minigw-core restore`); a name is looked up in `<core>/backups`, a path is accepted here |
| `minigw gw core audit` | The audit trail of the core |
| `minigw gw usage-events [--days n] [--user u] [--project p] [--model m]` | Model invocations per day, model and user, with the gaps (events that were lost) |
| `minigw gw quota [--claude-probe=on\|off]` | Subscription quota and provider credit with source, time and staleness; the Claude probe (`claude -p /usage`, at most every 10 minutes) is off until you turn it on (Mac: also Settings → Claude CLI) |
| `minigw gw quota --history [--days N] [--provider P] [--json]` | Existing probes only, default 7 days, range 1–400; exact provider filter. One line per probe with windows, values, units and resets, or failure kind. JSON preserves the API response. Capped results warn on stderr. Cannot combine with `--claude-probe`. |

On Windows and Linux every `minigw gw` command also takes `--config FILE`. `minigw serve allow`, `deny`, `devices`,
`provider` and `status` are older names for the same commands.

Examples:

```sh
minigw gw devices allow 3Jb…Q --name Laptop
minigw gw provider add openrouter https://openrouter.ai/api       # asks for the key without showing it
minigw gw provider models openrouter --enable anthropic/claude-sonnet-4.5,openai/gpt-5
minigw gw claude --folders ~/Projects --allow-local-tools=true
minigw gw token rotate                                            # every client must connect again
```

## Gateway: minigw serve (Windows, Linux)

| Command | What it does |
|---|---|
| `minigw serve init [--port N] [--config FILE]` | Create the config with a new token and gateway key; print the `minigw connect` line |
| `minigw serve install [--config FILE]` | Install minigw to a fixed place, add it to PATH, start the gateway now and at sign-in or boot (no admin rights). Run it again to update |
| `minigw serve uninstall [--config FILE]` | Stop the gateway and remove autostart; the config and keys stay |
| `minigw serve [--port N] [--config FILE]` | Run the gateway in this terminal |

Details: [gateway.md → minigw serve](gateway.md#minigw-serve-on-windows-and-linux).

## Plugin skills

Install the plugin `minigw@mini-gateway` in Claude Code ([README](../README.md#install)). Claude can also pick a skill by
itself when the request fits. Most skills take `--profile <p>` (or a model with `@<p>`) to use another gateway.

| Skill | Model | Use |
|---|---|---|
| `/minigw:ask [--ro] [--role R] [--profile P] <model> <question>` | any | Ask a specific gateway model or hand it a task |
| `/minigw:review [--rw] [--profile P] [what]` | `claude-codex-gpt-6-astra` | Independent review of the current changes, a plan or a bug analysis; read-only unless `--rw` |
| `/minigw:codex [--ro] [--profile P] <task>` | `claude-codex-gpt-6.1-sol` | Hand a self-contained task to Codex in this folder |
| `/minigw:copilot [--ro] [--profile P] <task>` | `claude-copilot` | Ask Copilot for a second opinion or a task in this folder |
| `/minigw:siri [--profile P] <question>` | `claude-siri-ai` | Questions about the gateway Mac user's personal context |
| `/minigw:on-device [--profile P] <text>` | `claude-apple-fm` | Short private answers on-device |
| `/minigw:models` | – | Gateway models, grouped by provider |
| `/minigw:status` | – | Connection and available models |
| `/minigw:version` | – | Versions of minigw, Claude Code, the plugin and the gateway (`minigw plugin status`) |
| `/minigw:connect [--name P] <gateway[:port]> <token>` | – | Connect this computer to a gateway; `--name` adds another one as a profile |
| `/minigw:share [folder \| list \| remove [folder]]` | – | Share a folder that minigw does not share by itself |
| `/minigw:project [list \| show \| link <slug> \| unlink]` | – | The gateway projects: list them, show or change the link of this folder (minigw 0.6.0 or newer) |
| `/minigw:notes [list \| read <id> \| push … \| edit <id> … \| rm <id>]` | – | Notes of the linked project (minigw 0.6.0 or newer) |
| `/minigw:usage [--project slug] [--days n]` | – | Your model usage (minigw 0.6.0 or newer) |
| `/minigw:gw install \| update \| uninstall \| status` | – | Install, update, remove or check the gateway on this computer |
| `/minigw:gw devices \| provider \| claude \| autostart \| token \| settings` | – | Manage it, as `minigw gw` |
| `/minigw:gw user \| bind \| project \| member \| grant \| core backup \| core restore \| usage-events \| quota` | – | The owner's commands for projects, usage and quota (0.6.0) |
| `/minigw:gw modes` | – | Explain the work modes |

Examples:

```
/minigw:ask --ro claude-cli-opus Where is the session timeout configured?
/minigw:codex --profile win Port scripts/build.sh to PowerShell and test it
/minigw:gw devices allow 3Jb…Q
```

Notes:

- The plugin needs minigw 0.5.19 or newer and says so at session start; `/minigw:gw update` updates it.
- Skills that hand work to an agent integrate worktree branches for you: they review the range minigw prints and
  apply it with `git apply` or `git cherry-pick -n`, never by merging the branch.
- `/minigw:gw` never prints the token unless you ask for the connect line, never puts an API key in a command line,
  and never updates on its own (an update stops running agent requests).

### Team project access

```text
minigw gw team access set <team> <project> <user> <role>
minigw gw team access remove <team> <project> <user>
minigw gw team access list <team>
minigw gw member <project> list --explain
minigw project list
```

A team assignment creates the same project membership as a direct one; the strongest wins;
removing a team assignment does not remove a direct one. `gw member` edits only direct origins.
The explanation lists each origin and role; the member project list displays role and origin names.
The user and project must belong to the active team. Removal from the team or team archive deletes
matching assignments; direct access survives. Narrowing effective access invalidates existing runs.
# KB review commands

Both clients support identical commands:

```text
minigw kb submit <project> [--class structure|data|private] [--title t] [--key k] [--replaces id --revision n] <file|->
minigw kb submissions [<project>] [--json]
minigw gw kb review [<project>]
minigw gw kb accept <id>
minigw gw kb reject <id> [--reason text]
```

Submission input matches note push; it requires existing project write access.
Review is owner-only. Pending entries are visible only to their author and the
owner. Acceptance publishes a new note or replacement revision. Note push still
publishes immediately. `minigw note read <id> --json` includes provenance.
