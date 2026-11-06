# Mini Gateway

A macOS menu bar LLM gateway compatible with the [Claude Code](https://claude.com/claude-code) CLI. One endpoint for several models:
the **Siri AI** app, **Apple Foundation Models** (on-device), the local **Claude Code CLI**, and any
Anthropic-compatible upstream such as **LiteLLM** or **OpenRouter**. Pick a model with `/model`, or give each
Claude Code subagent its own.

<img src="Resources/AppIcon-preview.png" width="128" alt="Mini Gateway icon">

## What it does

| Model in `/model` | Provider | Notes |
|---|---|---|
| `claude-siri-ai` | Siri AI app (macOS 27) | Driven through Accessibility, in the background. Conversation only, no tools. Keeps a real Siri thread per Claude Code conversation. **Privacy:** Siri answers with the personal context of the router Mac's user (notes, documents), so other Macs get it only when you allow the device (Settings → Siri AI → Devices; minigw 0.4.4 or newer proves its device key over the encrypted connection). This Mac always has it. It cannot read folders itself: files you mention (`@CLAUDE.md`, or names like “CLAUDE.md”, “kb” that exist in the session folder) are found by the router and added to the question as text (up to 16 KB; names extracted on-device with Apple Foundation Models). |
| `claude-apple-fm` | Apple Foundation Models | On-device, streaming, ~8K context (history trimmed automatically). No tools. |
| `opus`, `sonnet`, `haiku`, `fable` | `claude -p` | Runs your local Claude Code as an agent in the session's folder; sessions resume with `--resume`. Listed in `/model` as `claude-cli-*`; `claude-chat-*` = no tools. Tools: read-only by default; **Allow local tools** (Settings → Claude CLI) gives every Claude Code tool on the router Mac (commands, builds), with a table to exclude single tools. Files stay in the session folder and the allowed folders: file tools are refused outside them, and commands run in Claude Code's sandbox (no reads of the home folder, writes only in these folders, network allowed). |
| `claude-codex`, `claude-codex-<model>` | `codex exec` (OpenAI Codex CLI) | Off by default (Settings → Codex CLI). One entry per enabled Codex model of your account (model table, allowed folders and sandbox switches in Settings → Codex CLI). Runs Codex as an agent in the session's folder, like `claude -p`. Its sandbox writes only to that folder and the allowed folders. |
| discovered models | LiteLLM, OpenRouter | Pure passthrough (tools, streaming). Models and context windows are discovered from the upstream; enable the ones you want. |

The router speaks the Anthropic Messages API (`/v1/messages`, `/v1/models`, `count_tokens`), so Claude Code
sees it as a regular LLM gateway. Only models that were verified to work are listed.

## Requirements

- macOS 27 on Apple Silicon
- Claude Code (`claude`) in your `PATH` for the CLI provider and the `minigw` command
- The **Siri AI** app, for the Siri provider

## Install

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

### Another Mac (client only)

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

### Windows and Linux (client only)

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
Windows and Linux clients encrypt with AES-256-GCM, the macOS client with ChaCha20-Poly1305; the router accepts both.
With the Claude Code plugin (below) you can skip all of this: it downloads and checks `minigw` itself.

### Homebrew

The app and the client command are separate casks in this repository. Add the tap once:

```sh
brew tap gh-commiter/mini-gateway https://github.com/gh-commiter/mini-llm-gateway
```

```sh
brew trust --tap gh-commiter/mini-gateway
```

Router Mac (the app; then **Settings… → Claude CLI → Install** for `minigw`):

```sh
brew install --cask gh-commiter/mini-gateway/mini-gateway
```

Another Mac (client only). Both casks also install Claude Code (`claude-code`) if it is not installed yet:

```sh
brew install --cask gh-commiter/mini-gateway/minigw
```

## Use

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
not need to reach the client) and the agent works on your files. Nothing to install on either side. Only folders
inside your home folder are shared: never the home folder itself, `~/Library` or hidden folders such as `~/.ssh`.
The folder's own Claude Code settings and hooks are not loaded on the router Mac. Set `MINIGW_SHARE=0` to turn it off.

## Claude Code plugin

This repository is also a Claude Code plugin marketplace. The `minigw` plugin lets any Claude Code session use
Mini Gateway models through the `minigw` client: Claude runs `minigw -p --model <model> "…"` in the current folder
and uses the answer. The session itself does not have to run through the router, so it works with your normal Claude
login, Remote Control and so on. The plugin brings its own `minigw` on macOS, Windows (Git Bash)
and Linux: at session start it uses an installed one or downloads it from the latest release into its data folder and
checks the SHA-256 (on macOS also the Developer ID signature). No brew needed. Connect once with
`/minigw:connect <router-mac> <token>` (the token goes to the Keychain or Windows Credential Manager).

| Skill | Model | Use |
|---|---|---|
| `/minigw:ask <model> <question>` | any gateway model | Ask a specific model (`minigw models` lists them) |
| `/minigw:review [what]` | `claude-codex-gpt-6-astra` | Independent review of the current changes, a plan or a bug analysis |
| `/minigw:codex <task>` | `claude-codex-gpt-6.1-sol` | Hand a self-contained task to Codex in the current folder |
| `/minigw:siri <question>` | `claude-siri-ai` | Questions about the router Mac's personal context (Siri must be allowed for the device) |
| `/minigw:on-device <text>` | `claude-apple-fm` | Short private answers on-device |
| `/minigw:status` | – | Connection and available models |
| `/minigw:connect <router> <token>` | – | One-time connection to the router Mac |

Claude can also pick these skills by itself when they fit the request. Install (Claude Code 2.1.275 or later):

```
/plugin install minigw --marketplace gh-commiter/mini-llm-gateway
```

## Settings

- **General** – listen address and port (default `127.0.0.1:8765`; use `0.0.0.0` or a LAN IP for other Macs),
  allowed client IPs, access token.
- **LiteLLM / OpenRouter** – base URL, API key, and a switch per discovered model.
- **Siri AI** – off by default. Turning it on asks for Accessibility permission.
- **Apple Foundation Models** – on-device model and its instructions.
- **Claude CLI** – path to `claude`, allowed folders, tools, permission mode; install or download `minigw`.

Changes apply immediately. Keys and the access token are stored in the Keychain, settings in
`~/Library/Preferences/dev.trojar.minigateway.plist`.

## Security notes

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

## Build from source

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

## Disclaimer

Mini Gateway is provided **"AS IS", without warranty of any kind**, express or implied. Use it at your own risk.
The authors are not liable for any damage, data loss, costs (for example API usage billed by a provider) or other
claims arising from its use. Mini Gateway is an independent project and is not affiliated with or endorsed by
Apple or Anthropic. The Siri provider automates the Siri AI user interface, which has no public API and may change
with any macOS update. Siri and Apple are trademarks of Apple Inc.; Claude is a trademark of Anthropic, PBC.

## License

[MIT](LICENSE)
