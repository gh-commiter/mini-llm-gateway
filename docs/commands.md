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
| `minigw status` | Each profile: gateway and encryption, providers, number of models, device ID and approval, status. Exits 1 when the default profile is not connected |
| `minigw models` | Models of all profiles; other profiles' models end in `@P` |
| `minigw profiles [list]` | List profiles; `*` marks the default |
| `minigw profiles default P` | Make `P` the default (its models also appear without `@P`) |
| `minigw share add [--work=MODE] [--ro] [folder]` | Share a folder that is not shared automatically, and/or save its default work mode |
| `minigw share remove [folder]` | Remove a folder from the share list |
| `minigw share list` | Show shared folders and their saved modes |
| `minigw run [--] <command> [args …]` | Run a build in this folder on the gateway Mac (native mode, allowed per device) |
| `minigw --version` | minigw's version (`claude --version` gives Claude Code's) |
| `minigw plugin install` | Add the Claude Code plugin `minigw@mini-gateway` to `~/.claude/settings.json` with its marketplace on the `release` branch and `"autoUpdate": true` (an entry from minigw 0.5.21 or older is updated; a `false` you set stays; invalid JSON is not changed) and point the plugin's `bin/minigw` at this minigw |
| `minigw plugin uninstall [--upgrading]` | Remove only what `plugin install` added, and `claude plugin marketplace remove mini-gateway` (not with `--upgrading`) |
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
| `--minigw-role NAME` | `MINIGW_ROLE=NAME` | The caller's role, recorded in the gateway's session history |
| `--minigw-profile P` | – | Use only profile `P` for this command |
| `--model <model>@P` | – | Run on profile `P`'s gateway |
| – | `MINIGW_NATIVE_MOUNT=<share>=<folder>` | Tell minigw where a gateway SMB share is mounted, where it cannot detect it |
| – | `MINIGW_ALLOW_UNSAFE_FLAGS=1` | Turn off the option and prompt checks inside Claude Code ([how-it-works.md](how-it-works.md#what-minigw-passes-to-claude-code)) |

## Gateway: minigw gw

`minigw gw …` shows or changes the gateway **on this computer**: the Mini Gateway app on a Mac, `minigw serve` on
Windows and Linux. Changes apply at once. On a Mac the app must be running; on Windows and Linux a stopped gateway's
`serve.json` is changed instead. `minigw gw` talks to the admin API ([admin-api.md](admin-api.md)), which answers
only on the gateway computer.

| Command | What it does |
|---|---|
| `minigw gw status` | Running, version, autostart, agent requests, devices, providers |
| `minigw gw devices` | Allowed devices and devices waiting for approval |
| `minigw gw devices allow <id> [--name N]` | Allow a device (compare the ID with `minigw status` on that client first) |
| `minigw gw devices rename <id> --name N` | Rename a device (the name is only a label) |
| `minigw gw devices deny <id>` | Remove an allowed device or reject a waiting one |
| `minigw gw devices history <id>` | The device's last 20 connections (time, address, OS, minigw version) |
| `minigw gw sessions [--device ID] [--folder DIR]` | Recent sessions |
| `minigw gw sessions <session id>` \| `clear` | One session in detail / clear the history (and the project counters) |
| `minigw gw usage [--days N] [--device ID] [--folder DIR]` | Counters per folder, UTC day and agent (the last 7 days; 1-31) |
| `minigw gw provider add <name> <baseURL> [--insecure]` | Add a provider; the API key is read from stdin or asked for |
| `minigw gw provider list` \| `remove <name>` | List or remove providers |
| `minigw gw provider models <name> [text] [--all] [--enable m1,m2\|all] [--disable m1,m2\|all]` | Show, enable or disable a provider's models |
| `minigw gw claude [--path FILE] [--tools T1,T2] [--folders DIR1,DIR2] [--allow-local-tools=true\|false] [--ask-others=true\|false]` | Claude CLI settings |
| `minigw gw claude --allow-unsandboxed-bash=true\|false` | Commands without a sandbox (`minigw serve` only) |
| `minigw gw copilot token [remove]` | Copilot's GitHub token for commands, read from stdin or asked for |
| `minigw gw autostart [on\|off]` | Mac: Open at Login. Windows: the tray at sign-in. No argument: show it |
| `minigw gw token [rotate]` | Print the connect line for clients; `rotate` makes a new token |
| `minigw gw settings` | The settings in this terminal, also over SSH: the pages General, Devices, Providers, Agents, Folders, Sessions, Activity (`minigw-tui`) |
| `minigw gw settings link` | Print a one-time link (60 s) to the settings page (`minigw serve` only) |
| `minigw gw api GET\|POST\|PATCH\|DELETE <path>` | One [admin API](admin-api.md) request: JSON body on stdin, JSON answer on stdout (`minigw-tui` uses it) |

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
| `/minigw:gw install \| update \| uninstall \| status` | – | Install, update, remove or check the gateway on this computer |
| `/minigw:gw devices \| provider \| claude \| copilot token \| autostart \| token \| settings` | – | Manage it, as `minigw gw` |
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
