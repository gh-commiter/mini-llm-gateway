# Mini Gateway <img src="Resources/AppIcon-preview.png" width="48" align="top" alt="Mini Gateway icon">

A macOS menu bar LLM gateway compatible with the [Claude Code](https://claude.com/claude-code) CLI. One endpoint for several models:
the **Siri AI** app, **Apple Foundation Models** (on-device), the **Claude Code CLI**, **Codex** and **Grok** as agents,
and any Anthropic-compatible upstream such as **LiteLLM** or **OpenRouter**. The router runs on a Mac; the `minigw`
client runs on macOS, Windows and Linux. Pick a model with `/model`, or give each Claude Code subagent its own.

**Why.** Sometimes you want a second opinion: a review by Codex, a quick check by Grok, an answer from a local model
or from Siri, without leaving Claude Code. And sometimes Claude has to share the work: Mini Gateway was inspired by
a Claude Ultra plan that used up 100% of its weekly quota on day 2. Routine tasks can go to other or local models,
so Claude stays free for the work that needs it.

## Contents

- [1. What it does](#1-what-it-does)
- [2. Requirements](#2-requirements)
- [3. Install](#3-install)
  - [3.1 Router (Mac)](#31-router-mac)
  - [3.2 Client (minigw)](#32-client-minigw)
    - [3.2.1 Claude Code plugin](#321-claude-code-plugin)
    - [3.2.2 Another Mac](#322-another-mac)
    - [3.2.3 Windows and Linux](#323-windows-and-linux)
    - [3.2.4 Router on Windows or Linux (minigw serve)](#324-router-on-windows-or-linux-minigw-serve)
- [4. Use](#4-use)
- [5. Router settings](#5-router-settings)
- [6. Security notes](#6-security-notes)
- [7. Build from source](#7-build-from-source)
- [8. Disclaimer](#8-disclaimer)
- [9. Credits](#9-credits)
- [10. License](#10-license)

## 1. What it does

```mermaid
flowchart LR
  subgraph client["Client: Mac, Windows or Linux"]
    cc["Claude Code<br/>(or a plugin skill)"] -->|"Anthropic API"| mg["minigw"]
    folder[("project folder")]
  end
  mg <-->|"encrypted (Noise NK), port 8765"| router
  mg -.->|"serves the folder (WebDAV over the same connection)"| folder
  subgraph mac["Router Mac"]
    router["Mini Gateway<br/>(menu bar app)"]
    router --> siri["Siri AI app"]
    router --> fm["Apple Foundation Models"]
    router --> agents["agents: claude -p, codex, grok"]
    router --> up["LiteLLM / OpenRouter"]
    agents --> mount[("mounted folder")]
  end
  mount -.- folder
```

Claude Code talks to `minigw`, which forwards every request to the router over one encrypted connection. The router
picks the provider by model name. For agent models `minigw` also shares the current folder: the router mounts it
and the agent works on your files, while they stay on the client.

| Model in `/model` | Provider | Notes |
|---|---|---|
| `claude-siri-ai` | Siri AI app (macOS 27) | Driven through Accessibility, in the background. Conversation only, no tools. Keeps a real Siri thread per Claude Code conversation. **Privacy:** Siri answers with the personal context of the router Mac's user (notes, documents), so other Macs get it only when you allow the device (Settings → Siri AI → Devices; minigw 0.4.4 or newer proves its device key over the encrypted connection). This Mac always has it. It cannot read folders itself: files you mention (`@CLAUDE.md`, or names like “CLAUDE.md”, “kb” that exist in the session folder) are found by the router and added to the question as text (up to 16 KB; names extracted on-device with Apple Foundation Models). Images and PDFs (up to 4) are attached in the Siri app itself, so Siri reads them. |
| `claude-apple-fm` | Apple Foundation Models | On-device, streaming, ~8K context (history trimmed automatically). No tools. |
| `opus`, `sonnet`, `haiku`, `fable` | `claude -p` | Runs your local Claude Code as an agent in the session's folder; sessions resume with `--resume`. Listed in `/model` as `claude-cli-*`; `claude-chat-*` = no tools. Tools: read-only by default; **Allow local tools** (Settings → Claude CLI) gives every Claude Code tool on the router Mac (commands, builds), with a table to exclude single tools. Files stay in the session folder and the allowed folders: file tools are refused outside them, and commands run in Claude Code's sandbox (no reads of the home folder, writes only in these folders, network allowed). |
| `claude-codex`, `claude-codex-<model>` | `codex exec` (OpenAI Codex CLI) | Off by default (Settings → Codex CLI). One entry per enabled Codex model of your account (model table, allowed folders and sandbox switches in Settings → Codex CLI). Runs Codex as an agent in the session's folder, like `claude -p`. Its sandbox writes only to that folder and the allowed folders. Within a conversation Codex continues its own thread (`codex exec resume`), also after `minigw -c` or `--resume`. |
| `claude-grok`, `claude-grok-<model>` | `grok -p` (xAI Grok Build CLI) | Off by default (Settings → Grok CLI). Agent in the session's folder; Grok's kernel sandbox (`workspace`) limits writes to that folder, or `read-only`. Within a conversation Grok continues its own session (`-r`). |
| discovered models | LiteLLM, OpenRouter or any Anthropic-compatible gateway | Pure passthrough (tools, streaming). Add as many gateways as you like, each with its own key. Models and context windows are discovered from the upstream; enable the ones you want. |

The router speaks the Anthropic Messages API (`/v1/messages`, `/v1/models`, `count_tokens`), so Claude Code
sees it as a regular LLM gateway. Only models that were verified to work are listed.

## 2. Requirements

- Router: macOS 27 on Apple Silicon
- Client (`minigw`): macOS, Windows or Linux, with Claude Code (`claude`) in your `PATH`
- Optional on the router Mac, for their providers: Claude Code, the **Siri AI** app, [Codex CLI](https://github.com/openai/codex),
  Grok CLI, a LiteLLM or OpenRouter account

## 3. Install

Mini Gateway has two parts: the **router** (the menu bar app, on one Mac) and the **client** `minigw` (on every
computer that uses it: the same Mac, other Macs, Windows, Linux).

### 3.1 Router (Mac)

1. Install Claude Code if you don't have it yet, then sign in once by running `claude`.
   Check whether it is already installed:

   ```sh
   claude --version
   ```

   Install with Homebrew:

   ```sh
   brew install --cask claude-code
   ```

   Or with the native installer:

   ```sh
   curl -fsSL https://claude.ai/install.sh | bash
   ```

2. Download `Mini-Gateway-<version>.dmg` from [Releases](../../releases) (signed with Developer ID and notarized;
   a `.zip` is there too, checksums in `SHA256SUMS`).
3. Open the DMG, drag **Mini Gateway** to **Applications** and open it. It lives in the menu bar.
4. **Settings… → Claude CLI → Install** puts the `minigw` command in your `PATH` and connects it.

With Homebrew instead of the DMG (add the tap once):

```sh
brew tap gh-commiter/mini-gateway https://github.com/gh-commiter/mini-llm-gateway
brew trust --tap gh-commiter/mini-gateway
brew install --cask gh-commiter/mini-gateway/mini-gateway
```

Then **Settings… → Claude CLI → Install** for `minigw`. The cask also installs Claude Code (`claude-code`) if it is
not installed yet.

### 3.2 Client (minigw)

`minigw` runs wherever Claude Code runs. Pick one way to install it.

#### 3.2.1 Claude Code plugin

This repository is also a Claude Code plugin marketplace. The `minigw` plugin lets any Claude Code session use
Mini Gateway models through the `minigw` client: Claude runs `minigw -p --model <model> "…"` in the current folder
and uses the answer. The session itself does not have to run through the router, so it works with your normal Claude
login, Remote Control and so on. The plugin brings its own `minigw` on macOS, Windows (Git Bash)
and Linux: at session start it uses an installed one or downloads it from the latest release into its data folder and
checks the SHA-256 (on macOS also the Developer ID signature). A downloaded copy is updated at session start when a
new release is out. No brew needed. Connect once with
`/minigw:connect <router-mac> <token>` (the token goes to the Keychain or Windows Credential Manager).

| Skill | Model | Use |
|---|---|---|
| `/minigw:ask [--ro] <model> <question>` | any gateway model | Ask a specific model (`/minigw:models` lists them) |
| `/minigw:review [--rw] [what]` | `claude-codex-gpt-6-astra` | Independent review of the current changes, a plan or a bug analysis (folder read-only unless `--rw`) |
| `/minigw:codex [--ro] <task>` | `claude-codex-gpt-6.1-sol` | Hand a self-contained task to Codex in the current folder (`--ro`: read-only) |
| `/minigw:siri <question>` | `claude-siri-ai` | Questions about the router Mac's personal context (Siri must be allowed for the device) |
| `/minigw:on-device <text>` | `claude-apple-fm` | Short private answers on-device |
| `/minigw:status` | – | Connection and available models |
| `/minigw:models` | – | Gateway models, grouped by provider |
| `/minigw:version` | – | Versions of minigw, Claude Code and the plugin |
| `/minigw:connect <router> <token>` | – | One-time connection to the router Mac |
| `/minigw:share [folder]` | – | Share a folder that minigw does not share by itself, such as a system folder |

Claude can also pick these skills by itself when they fit the request. Install (Claude Code 2.1.275 or later):

```
/plugin install minigw --marketplace gh-commiter/mini-llm-gateway
```

#### 3.2.2 Another Mac

On a Mac that should use the router running on your Mac, one command installs Claude Code (with Homebrew if
present, otherwise the native installer) and the signed `minigw` command into your `PATH`:

```sh
curl -fsSL https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/install.sh | sh
```

To connect in the same step, add the router Mac's name or address. The script then asks for the token
(**Settings… → General → Copy Token** on the router Mac):

```sh
curl -fsSL https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/install.sh | sh -s -- mini.local
```

The script checks the download against `SHA256SUMS` and the Developer ID signature before installing.

With Homebrew (tap as above; also installs Claude Code if missing):

```sh
brew install --cask gh-commiter/mini-gateway/minigw
```

#### 3.2.3 Windows and Linux

The router runs on a Mac; `minigw` runs wherever Claude Code runs. For Windows and Linux it is a single executable
without dependencies (`client/`, Go standard library only), in [Releases](../../releases):

| System | File |
|---|---|
| Windows x64 / ARM64 | `minigw-windows-amd64.exe` / `minigw-windows-arm64.exe` |
| Linux x64 / ARM64 | `minigw-linux-amd64` / `minigw-linux-arm64` (static, any distribution) |

Check it against `SHA256SUMS`, rename it to `minigw` (`minigw.exe` on Windows) and put it in your `PATH`. Linux:

```sh
curl -fsSLo minigw https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/minigw-linux-amd64
curl -fsSL https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/SHA256SUMS | grep ' minigw-linux-amd64$' | sed 's/minigw-linux-amd64$/minigw/' | sha256sum -c -
chmod +x minigw && sudo mv minigw /usr/local/bin/
```

Windows (PowerShell; compare the hash with `SHA256SUMS`):

```powershell
Invoke-WebRequest https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/minigw-windows-amd64.exe -OutFile $HOME\minigw.exe
Get-FileHash $HOME\minigw.exe -Algorithm SHA256
```

Then connect once with the token from **Settings… → General → Copy Token** on the router Mac (quote it, it
contains `#`):

```sh
minigw connect mini.local --token '<token>'
```

The connection is stored in Windows Credential Manager, on Linux in `~/.config/minigw/connection.json` (mode 0600).
All clients encrypt with AES-256-GCM; the Mac router also accepts ChaCha20-Poly1305 from older macOS clients.
With the [Claude Code plugin](#321-claude-code-plugin) you can skip all of this: it downloads and checks `minigw` itself.

#### 3.2.4 Router on Windows or Linux (minigw serve)

`minigw serve` turns a Windows or Linux computer into a small router: `claude -p` on that computer as
`claude-cli-opus`, `claude-cli-sonnet` and `claude-cli-haiku`, plus gateways; no Siri or Apple models. Agents work in a
synced copy of the client's folder, so clients run with `MINIGW_WORK=sync` (folder mounts need the Mac router).
Install Claude Code there (on Windows the native `claude.exe`; `minigw serve` does not run `claude.cmd`).

The setup is one command after `minigw serve init`: `minigw serve install` copies minigw to a fixed place, adds it to
PATH, starts the router now and at sign-in, all without admin rights. On Windows it installs to
`%LocalAppData%\Programs\Mini Gateway` (with the tray icon; downloaded and checked against the release's
`SHA256SUMS` if `minigw-tray*.exe` is not next to `minigw.exe`); on Linux to `~/.local/bin` with a systemd user unit.
Run it again to update; `minigw serve uninstall` removes it and keeps the config and keys.

```sh
minigw serve init            # creates the config with a new token and router key, prints the minigw connect line
minigw serve install         # install, start now and at sign-in
minigw serve                 # or run it in this terminal: port 8765; --port, --folder, --config
```

On each client run the printed `minigw connect` line. The first connect from a new device is refused and the router
log shows `minigw serve allow <id> "<name>"`. Anyone with the token can try to connect, so compare the ID with the
device you just connected before you allow it. `minigw router devices` lists the allowed devices and the ones
waiting, `minigw router devices deny <id or name>` removes one.

**Windows tray.** `minigw-tray-windows-amd64.exe` (or `-arm64.exe`) is the same router with an icon in the
notification area instead of a console window. On first start it creates the config. Its menu shows the status,
devices waiting for approval (Allow / Deny, with a notification when one appears), **Settings…**, **Copy connect
line**, **Start at sign-in**, the log (`minigw-serve.log` in the config folder) and Quit. `minigw serve install` sets
it up.

**Settings page.** **Settings…** opens `http://127.0.0.1:<port>/minigw/settings` in the browser with a one-time
link (60 s): General (port, address, connect line, new token), Devices, Gateways (key, models) and Claude CLI.
Changes apply at once. On Linux, `minigw router settings` prints such a link; open it on the router computer or
through `ssh -L <port>:127.0.0.1:<port>` with the same port on both ends (the page checks the port).

**Changing the router.** `minigw router status|devices|gateway|claude|token` (also as `minigw serve allow`, `deny`,
`devices`, `gateway`) changes the running router at once, through its admin API; when it does not run, it changes
`serve.json`. Port and address need a restart. `minigw router token rotate` makes a new token and prints the new
connect line for the clients. The admin API ([docs/admin-api.md](docs/admin-api.md)) answers only on this computer,
with the secret in `admin.secret` in the config folder.

The config (`%AppData%\minigw\serve.json`, `~/.config/minigw/serve.json`, mode 0600) holds the token, the router key,
the allowed devices and the agent's `tools` (default `Read,Grep,Glob,Edit,Write,MultiEdit`, limited to the copy).
`Bash` runs in Claude Code's sandbox on Linux (needs `bubblewrap` and `socat`); on Windows there is no sandbox, so it
also needs `"allowUnsandboxedBash": true`; agents can then run any command as you and read the gateway API keys in
Credential Manager and the admin secret. Connections from the router computer itself (loopback) skip the device
check and encryption: do not forward the port to loopback (`ssh -L`, `netsh interface portproxy`, a reverse proxy).

**Gateways.** Any Anthropic-compatible upstream (OpenRouter, LiteLLM) works as on the Mac, with the same model names.

```sh
minigw router gateway add openrouter https://openrouter.ai/api  # asks for the API key (or reads it from stdin)
minigw router gateway models openrouter --enable anthropic/claude-sonnet-4.5,openai/gpt-5
minigw router gateway models openrouter haiku                   # enabled models; a text or --all also shows the others
minigw router gateway list                                      # also: gateway remove <name>
```

The router lists the enabled models with the claude-cli ones and passes requests through unchanged, streaming
included. A gateway with more than 20 models starts with none enabled. The URL must be `https`; `http` works only
to localhost or a private IP, with `--insecure`. The API key is never in `serve.json`: it is in Credential Manager
on Windows, in the Secret Service (`secret-tool`) on Linux, or in `gateway-keys.json` (mode 0600) next to
`serve.json` if there is no Secret Service.

## 4. Use

```sh
minigw                                  # Claude Code through the router; choose models with /model
minigw --model claude-apple-fm -p "Hi"  # any claude option works
minigw status                           # router, providers, number of models
minigw models                           # list available models
minigw connect 192.168.1.20 --token …   # use a router on another Mac
```

Subagents can use router models too, e.g. `model: claude-siri-ai` in `.claude/agents/<name>.md`.

**Another Mac and Claude CLI models with tools.** `opus`, `sonnet`, `haiku` and `fable` run `claude -p` as an agent on
the router Mac. When `minigw` talks to a router on another Mac, it shares the current folder with it for the length
of the session: the router mounts it (built-in WebDAV, through the connection `minigw` opens, so the router Mac does
not need to reach the client) and the agent works on your files. Nothing to install on either side. Any project
folder is shared, also outside your home folder (for example `D:\Projects\app` on Windows or `/Volumes/Disk/app`),
but never a drive or volume root, the home folder itself, system folders (`C:\Windows`, `Program Files`, `/usr`,
`/System` …), other users' home folders, `~/Library`, `AppData` or hidden folders such as `~/.ssh`. To share one of
those anyway, run `minigw share add` in it (`minigw share list`, `minigw share remove`); a root is always refused.
**Read-only:** `minigw --minigw-ro …` (or `MINIGW_SHARE=ro`, for example in the `env` of Claude Code's settings for the
plugin) shares the folder read-only: minigw refuses every write, the router mounts it read-only and runs Codex and
Grok in their read-only sandbox. Agents read files up to 32 MB in one piece (larger ones only in ranges).
The router caches folder listings (PROPFIND) for 10 seconds, like davfs2's `dir_refresh`; writes through the mount clear
the cache at once, changes made on the client outside the mount show up within 10 seconds.
The folder's own Claude Code settings and hooks are not loaded on the router Mac. Set `MINIGW_SHARE=0` to turn it off.

**Sync instead of a mount (for builds).** `minigw --minigw-work=sync …` (or `MINIGW_WORK=sync`) copies the folder to the
router's Scratch folder before each agent request and copies the files the agent changed back after it. Agents and
builds then work on the router's local disk.
- **Sent to the router:** only changed files, compared by size, time and SHA-256.
- **Never sent, never copied back:**
  - files matched by `.gitignore` or the built-in list (`.git`, `target/`, `.build/`, `node_modules/`, `build/`,
    `dist/`, `DerivedData/` …);
  - files the agent puts into `.git`, `.claude/settings*.json`, `.vscode/tasks.json` or `.envrc`.
- **Conflicts:** if you changed a file the agent also changed, yours stays and the agent's version is saved next to it
  as `<name>.minigw-conflict`.
- **Read-only:** with `--minigw-ro` nothing comes back.
- **The router copy is kept between requests,** so the next sync and the next build are incremental.
- **Agents in the same folder run one after another.**

**Native: the folder is the router's own SMB share.** When the client works in an SMB mount of a router share (for
example `/Volumes/Java/eureko-modules` mounted from the router's `/Volumes/Java`), `minigw --minigw-work=native …`
(or `MINIGW_WORK=native`) lets agents work directly in the router's folder: no mount back, no copy.
`minigw run -- mvn -q verify` runs a build there and streams its output and exit code back.
- **Mode per folder:** `minigw share add --work=native` (or `sync`, `direct`, with `--ro` for read-only) in a folder
  saves its default mode for it and its subfolders in minigw's share list (never in the project); `minigw share list`
  shows it. `--minigw-work=` or `MINIGW_WORK` still wins for one run.
- **Proof, not a path:** minigw sends the share name and the path under it; the router writes a check file with random
  content into its folder, and minigw must read it back through its mount. A folder with the same path elsewhere fails.
  The check is for correctness (the same folder), not a permission: permission comes from the token, the device, the
  share and the allowed folders.
- **Only allowed folders:** the folder must be in a writable SMB share of the router (System Settings → Sharing) and
  inside its **Allowed folders**; the router checks it again on every request. Agents keep the same sandbox and folder
  rules.
- **Like `direct`:** the agent can change every file in the folder, also `.git`, `.claude/settings*.json` and
  `.vscode/tasks.json`. Use `sync` for a folder where those must stay protected. Native cannot be read-only:
  `--minigw-ro` (or a saved `--ro`) uses the read-only mount instead.
- **`minigw run` is off by default:** turn it on per device in **Settings → General → minigw run**, or with the admin
  API (`PATCH /run`). Only the listed commands run (default `mvn, swift, xcodebuild`), without a shell, at most two at
  a time per device. It is outside the agent sandbox: the build runs the project's code with your rights on the router,
  so allow only devices you trust. A sandbox (`sandbox-exec`) limits only where it writes: the folder, the allowed
  folders, `~/.m2`, `~/Library/Developer`, `~/Library/Caches`, SwiftPM's folders and temporary folders; reading and the
  network stay open. A sandbox inside it is not possible, so the router runs `swift build|test|run|package` with
  `--disable-sandbox` and `xcodebuild` without its package and script sandboxes. Stopping minigw stops the whole
  process group. In a terminal minigw shows only colours from the output, no other escape sequences.
- If the folder is not a router share, minigw falls back to the usual mount. On Linux (cifs) and Windows (UNC path or
  mapped drive) detection is automatic; elsewhere set `MINIGW_NATIVE_MOUNT=<share>=<mount folder>`. The Go router
  (`minigw serve`) does not support native yet.

## 5. Router settings

Settings of the menu bar app on the router Mac (**Settings…**, laid out like System Settings). Client options (`minigw share`, `--minigw-ro`,
`MINIGW_SHARE`) are in [4. Use](#4-use).

- **General** – listen address and port (default `127.0.0.1:8765`; use `0.0.0.0` or a LAN IP for other computers),
  allowed client IPs, unencrypted connections, access token, and About (version, build commit, license).
- **Gateways** (LiteLLM, OpenRouter or any other Anthropic-compatible one) – name, base URL, API key, and a switch
  per discovered model. **+ / −** under the sidebar adds or removes gateways, so you can use several, each with its own
  key and models.
- **Siri AI** – off by default; turning it on asks for Accessibility permission. Privacy warning and **Devices**:
  which other computers may use Siri (this Mac always may).
- **Apple Foundation Models** – on-device model and its instructions.
- **Claude CLI** – path to `claude`, allowed folders, **Allow local tools** with a table to exclude tools; install or
  download `minigw`.
- **Codex CLI** / **Grok CLI** – off by default; model table, write and network switches for their sandbox.

<table>
<tr><td><img src="docs/images/general.png" width="400" alt="Settings: General"></td>
<td><img src="docs/images/siri-ai.png" width="400" alt="Settings: Siri AI with devices"></td></tr>
<tr><td><img src="docs/images/claude-cli.png" width="400" alt="Settings: Claude CLI"></td>
<td><img src="docs/images/codex-cli.png" width="400" alt="Settings: Codex CLI"></td></tr>
</table>

Changes apply immediately. Keys and the access token are stored in the Keychain, settings in
`~/Library/Preferences/dev.trojar.minigateway.plist`.

**From the command line.** `minigw router status|devices|gateway|claude|token` changes the same settings on this
Mac, with the same commands as on Windows and Linux (`minigw router help`), for example
`minigw router gateway add Lab https://gw.example` (the key comes from stdin or a hidden prompt) or
`minigw router gateway models OpenRouter --enable anthropic/claude-sonnet-4.5`. The app must be running; the
Settings window shows the change at once. The first time, macOS asks whether `minigw` may use the app's Keychain
item (the admin secret); `minigw` talks to the app only after the app proves its router key. The API is in
[docs/admin-api.md](docs/admin-api.md).

## 6. Security notes

- Every request needs the router's token; the server listens only on `127.0.0.1` unless you change it.
- The Claude CLI provider runs `claude -p` with tools in the client's folder: it only runs inside the allowed
  folders and uses the tools and permission mode you configure (default: read-only, `dontAsk`).
- Between `minigw` and a router on another Mac everything is encrypted (WireGuard-style: Noise NK with Curve25519
  and ChaCha20-Poly1305). **Copy Token** includes the router's public key, so `minigw` only talks to that router.
  Unencrypted connections are accepted only from the router Mac itself, unless you turn on
  **Allow unencrypted connections from other Macs** (for other clients, on a trusted network).
- With a remote router, `minigw` gives the router Mac read and write access to the current folder while it runs
  (nothing outside it). Use it only with a router Mac you trust.
- The Siri provider uses Accessibility UI automation of the Siri AI app. It only touches that app, but
  Accessibility is a powerful permission.

## 7. Build from source

```sh
./build.sh            # build, sign (Developer ID if available, else Apple Development)
./build.sh --test     # run tests first
./build.sh --release  # Developer ID + notarization + staple → build/release/Mini-Gateway-<version>.{dmg,zip}
```

`--release` needs a *Developer ID Application* certificate and a notarytool profile:

```sh
xcrun notarytool store-credentials minigateway-notary --key AuthKey_XXXX.p8 --key-id XXXX --issuer <issuer-id>
```

Set `SIGN_IDENTITY` or `NOTARY_PROFILE` to override the defaults. The app icon is drawn by
`swift scripts/make-icon.swift`.

## 8. Disclaimer

Mini Gateway is provided **"AS IS", without warranty of any kind**, express or implied. Use it at your own risk.
The authors are not liable for any damage, data loss, costs (for example API usage billed by a provider) or other
claims arising from its use. Mini Gateway is an independent project and is not affiliated with or endorsed by
Apple, Anthropic, OpenAI, xAI, BerriAI or OpenRouter. The Siri provider automates the Siri AI user interface, which
has no public API and may change with any macOS update. Siri and Apple are trademarks of Apple Inc.; Claude is a
trademark of Anthropic, PBC; Codex and OpenAI are trademarks of OpenAI; Grok is a trademark of xAI.

## 9. Credits

Mini Gateway only connects tools that others built:

- [Anthropic](https://www.anthropic.com): [Claude Code](https://claude.com/claude-code) and the Claude models. Claude Code
  is the client the whole project is made for, and Claude wrote all of the code.
- [OpenAI](https://openai.com): [Codex CLI](https://github.com/openai/codex).
- [xAI](https://x.ai): Grok CLI.
- [Apple](https://www.apple.com): the Siri AI app and the [Foundation Models](https://developer.apple.com/documentation/foundationmodels) framework.
- [LiteLLM](https://github.com/BerriAI/litellm) by BerriAI and [OpenRouter](https://openrouter.ai): upstream model gateways.
- [The Noise Protocol Framework](https://noiseprotocol.org) by Trevor Perrin: the encrypted connection between `minigw` and the router.

Thank you to everyone behind these projects for making them available. Mini Gateway would not exist without them.

## 10. License

[MIT](LICENSE)
