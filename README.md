# Mini Gateway

A macOS menu bar LLM gateway compatible with the [Claude Code](https://claude.com/claude-code) CLI. One endpoint for several models:
the **Siri AI** app, **Apple Foundation Models** (on-device), the local **Claude Code CLI**, and any
Anthropic-compatible upstream such as **LiteLLM** or **OpenRouter**. Pick a model with `/model`, or give each
Claude Code subagent its own.

<img src="Resources/AppIcon-preview.png" width="128" alt="Mini Gateway icon">

## What it does

| Model in `/model` | Provider | Notes |
|---|---|---|
| `claude-siri-ai` | Siri AI app (macOS 27) | Driven through Accessibility, in the background. Conversation only, no tools. Keeps a real Siri thread per Claude Code conversation. |
| `claude-apple-fm` | Apple Foundation Models | On-device, streaming, ~8K context (history trimmed automatically). No tools. |
| `opus`, `sonnet`, `haiku`, `fable` | `claude -p` | Runs your local Claude Code as an agent in the session's folder; sessions resume with `--resume`. `claude-chat-*` = no tools. |
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

## Use

```sh
minigw                                  # Claude Code through the router; choose models with /model
minigw --model claude-apple-fm -p "Hi"  # any claude option works
minigw status                           # router, providers, number of models
minigw models                           # list available models
minigw connect 192.168.1.20 --token …   # use a router on another Mac
```

Subagents can use router models too, e.g. `model: claude-siri-ai` in `.claude/agents/<name>.md`.

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
