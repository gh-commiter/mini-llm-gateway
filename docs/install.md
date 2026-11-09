# Install

The Claude Code plugin installs everything ([README → Install](../README.md#install)). This page lists the other
ways, and what the plugin does for you.

- [What the plugin does](#what-the-plugin-does)
- [Gateway Mac: DMG or Homebrew](#gateway-mac-dmg-or-homebrew)
- [Client on another Mac](#client-on-another-mac)
- [Client on Windows and Linux](#client-on-windows-and-linux)
- [Check a download](#check-a-download)
- [Gateway on Windows or Linux](#gateway-on-windows-or-linux)
- [Windows installer (MSI)](#windows-installer-msi)
- [Connect a client](#connect-a-client)
- [Plugin and uninstall: one command everywhere](#plugin-and-uninstall-one-command-everywhere)
- [Upgrade and uninstall per system](#upgrade-and-uninstall-per-system)
- [Build from source](#build-from-source)

## What the plugin does

- **At session start** it uses the installed `minigw` (Homebrew or the app on macOS, the MSI on Windows,
  `~/.local/bin` on Linux) when it is not older than the plugin needs: a link to it (on Windows a small `minigw` /
  `minigw.cmd` shim), never a second copy. Only when no minigw is installed does it download one from the latest
  release into its data folder; it checks the signed `SHA256SUMS` and, on macOS, the Developer ID signature. It works
  on macOS, Windows (Git Bash) and Linux.
- **Updates:** when the installed gateway is older than the release, the session start shows one line with
  `/minigw:gw update` (without a gateway on this computer: the MSI to run on Windows, the file to replace on Linux). Nothing is updated without you, because an update stops the agent requests that run on the
  gateway.
- **Your session** does not run through the gateway, so it keeps your normal Claude login, Remote Control and so on.
- The agent skills need the standalone `claude` command on PATH (the VS Code extension's bundled CLI is not enough).

## Gateway Mac: DMG or Homebrew

Needs macOS 27 on Apple Silicon.

1. Download `Mini-Gateway-<version>.dmg` from [Releases](https://github.com/gh-commiter/mini-llm-gateway/releases)
   (signed with Developer ID and notarized; a `.zip` is there too, checksums in `SHA256SUMS`).
2. Open the DMG, drag **Mini Gateway** to **Applications** and open it. It lives in the menu bar.
3. **Settings… → Claude CLI → Install** puts the `minigw` command in your `PATH` and connects it.

With Homebrew instead (add the tap once):

```sh
brew tap gh-commiter/mini-gateway https://github.com/gh-commiter/mini-llm-gateway
brew trust --tap gh-commiter/mini-gateway
brew install --cask gh-commiter/mini-gateway/mini-gateway
```

Then **Settings… → Claude CLI → Install** for `minigw`. The cask also installs Claude Code (`claude-code`) if it is
missing.

## Client on another Mac

Needs macOS 27 on Apple Silicon. One command installs Claude Code (with Homebrew if present, otherwise the native
installer) and the signed `minigw` command into your `PATH`:

```sh
curl -fsSL https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/install.sh | sh
```

To connect in the same step, add the gateway's name or address. The script then asks for the token:

```sh
curl -fsSL https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/install.sh | sh -s -- <gateway>
```

The script checks the download against the signed `SHA256SUMS` and the Developer ID signature, and refuses to go back
to an older release (`sh -s -- --allow-downgrade` does it anyway).

With Homebrew (tap as above; also installs Claude Code if missing):

```sh
brew install --cask gh-commiter/mini-gateway/minigw
```

The `minigw` cask also runs `minigw plugin install`: it adds the Claude Code plugin (`minigw@mini-gateway`) to `~/.claude/settings.json`
(`extraKnownMarketplaces`, `enabledPlugins`). Claude Code installs it the next time you start `claude` (or run
`/reload-plugins`). A `"minigw@mini-gateway": false` you set there stays, also on upgrade. `brew uninstall` removes
the plugin and its marketplace again; `brew upgrade` keeps them.

## Client on Windows and Linux

`minigw` is a single executable without dependencies, in
[Releases](https://github.com/gh-commiter/mini-llm-gateway/releases/latest):

| System | File |
|---|---|
| Windows x64 / ARM64 | `minigw-windows-amd64.exe` / `minigw-windows-arm64.exe` |
| Linux x64 / ARM64 | `minigw-linux-amd64` / `minigw-linux-arm64` (static, any distribution) |
| Settings in the terminal (optional, for a gateway) | `minigw-tui-<system>-<arch>` (`.exe` on Windows), next to `minigw` as `minigw-tui` |

On Windows the [MSI](#windows-installer-msi) does this for you. On Linux, [check the download](#check-a-download) and
put it in `~/.local/bin` as `minigw`: there the plugin finds it and `minigw serve install` updates it. Claude Code
(`claude`) must be in your `PATH` too. Then add the plugin with `minigw plugin install`. A gateway on Linux:
`minigw serve init && minigw serve install` ([below](#gateway-on-windows-or-linux)).

## Check a download

`SHA256SUMS` is signed with a key that is not on GitHub, so a changed release is refused. The plugin, `install.sh`
and `minigw serve install` check this themselves. By hand on Linux:

```sh
R=https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download
curl -fsSLO $R/minigw-linux-amd64 -O $R/SHA256SUMS -O $R/SHA256SUMS.sig
echo 'minigw-release namespaces="minigw-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINt++OHn+HHmR9mmA2UI+B0osuilO2GP0ddEzNp0GTrH' > signers
ssh-keygen -Y verify -f signers -I minigw-release -n minigw-release -s SHA256SUMS.sig < SHA256SUMS
grep ' minigw-linux-amd64$' SHA256SUMS | sha256sum -c -
mkdir -p ~/.local/bin && install -m 755 minigw-linux-amd64 ~/.local/bin/minigw
# For a gateway, also minigw gw settings (the same steps, then next to minigw):
curl -fsSLO $R/minigw-tui-linux-amd64 && grep ' minigw-tui-linux-amd64$' SHA256SUMS | sha256sum -c -
install -m 755 minigw-tui-linux-amd64 ~/.local/bin/minigw-tui
```

On Windows (PowerShell; `ssh-keygen` comes with Windows 10 and later). `cmd /c` passes `SHA256SUMS` unchanged, which a
PowerShell pipe would not. The same steps check the MSI:

```powershell
cd $HOME
$R = "https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download"
foreach ($f in "minigw-windows-amd64.exe", "SHA256SUMS", "SHA256SUMS.sig") { Invoke-WebRequest "$R/$f" -OutFile $f }
Set-Content signers 'minigw-release namespaces="minigw-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINt++OHn+HHmR9mmA2UI+B0osuilO2GP0ddEzNp0GTrH' -Encoding ascii
cmd /c "ssh-keygen -Y verify -f signers -I minigw-release -n minigw-release -s SHA256SUMS.sig < SHA256SUMS"
(Get-FileHash minigw-windows-amd64.exe -Algorithm SHA256).Hash.ToLower()   # must match its line in SHA256SUMS
Rename-Item minigw-windows-amd64.exe minigw.exe
```

## Gateway on Windows or Linux

`/minigw:gw install` runs these two commands:

```sh
minigw serve init            # creates the config with a new token and gateway key, prints the connect line
minigw serve install         # install, start now and at sign-in (Windows) or boot (Linux)
```

No admin rights needed. Details, the tray and the settings page: [gateway.md → minigw serve](gateway.md#minigw-serve-on-windows-and-linux).

## Windows installer (MSI)

Download `minigw-windows-amd64.msi` (or `-arm64.msi`) from the
[latest release](https://github.com/gh-commiter/mini-llm-gateway/releases/latest) and open it. It installs for this
user only, without admin rights, into `%LocalAppData%\Programs\Mini Gateway`. Its window lets you choose the parts:

| Part | What it adds | Default |
|---|---|---|
| Client (`Client`) | `minigw.exe` on your PATH, and `minigw-tray.exe` (the installer runs minigw through it, without a console window) | always |
| Gateway (`Gateway`) | the tray at sign-in, a Start menu entry, `minigw-tui.exe` (`minigw gw settings`); it starts the tray, which creates the config on first start: copy the token from its menu (**Copy Token**) | on |
| Claude Code plugin (`Plugin`) | runs `minigw plugin install`, which adds `minigw@mini-gateway` and points the plugin at this `minigw.exe` | on |

- Without the window: `msiexec /i minigw-windows-amd64.msi /passive` installs all parts; choose them with
  `ADDLOCAL=Client,Plugin` (any list; `Client` is always added).
- **Change parts:** Settings → Apps → Installed apps → Mini Gateway → **Modify**, or open the MSI again: tick or
  untick, then **Change**.
- **Upgrade:** open (or `/passive`) a newer MSI. No questions, the chosen parts stay, and the tray starts again only if
  it was running. `/minigw:gw update` downloads and runs it for you on a gateway.
- **Uninstall:** Settings → Apps → **Uninstall** removes the parts (tray start, PATH entry, Start menu, the plugin
  entries that `minigw plugin install` added; a `false` you set stays) and keeps the settings and profiles for a later
  install. To remove them too: **Modify** → tick *Also remove settings and profiles* → **Remove**, or
  `msiexec /x <the installed version's MSI> PURGE=1 /passive`, or `minigw uninstall --purge` before **Uninstall**. PURGE runs `minigw uninstall --purge --yes` before the files
  go (profiles and keys in Credential Manager, `%AppData%\minigw`). Not with `minigw serve uninstall`.
- The MSI is not code-signed yet, so SmartScreen warns: **More info → Run anyway**. Check it first against the signed
  `SHA256SUMS` ([Check a download](#check-a-download)).
- The ARM64 package needs Windows 11.

## Connect a client

Get the connect line on the gateway: `minigw gw token` (or `/minigw:gw token`, or **Copy Token** in the Mac app or the
Windows tray). Then on the client:

```sh
minigw connect <gateway> --token -       # paste the token, then Enter
minigw status                            # Status: connected, or why not
```

`/minigw:connect <gateway> <token>` does the same in Claude Code. The first connect from a new device is refused
until the gateway allows it ([gateway.md → Devices](gateway.md#devices)); then connect again. On the gateway computer
itself, `/minigw:gw install` connects the local `minigw` as profile `local`.

## Plugin and uninstall: one command everywhere

The same commands on macOS, Windows and Linux; the installed minigw is the only minigw.

```sh
minigw plugin install     # add minigw@mini-gateway to ~/.claude/settings.json, point the plugin at this minigw
minigw plugin status      # versions of minigw, Claude Code, the plugin and the gateway; which minigw the plugin uses
minigw plugin uninstall   # remove only what plugin install added ("minigw@mini-gateway": false stays)
minigw uninstall          # list, then remove what minigw added here: plugin, sign-in start, scratch folders
minigw uninstall --purge  # also profiles (Keychain, Credential Manager) and the settings folder
```

`minigw plugin install` refuses a `settings.json` that is not valid JSON and never overwrites it. It does not link a
minigw that is, or lies in a folder that is, writable by other users or not owned by you or root; move it to a safe
folder first. `minigw plugin` and `minigw uninstall` refuse to run as root: run them as yourself. `minigw uninstall`
asks first (`--yes` skips the question; without a terminal it needs `--yes`) and keeps settings and profiles unless
you add `--purge`. It does not remove minigw itself; it prints how: `brew uninstall --cask minigw`, Settings → Apps for
the MSI, or `rm ~/.local/bin/minigw`.

## Upgrade and uninstall per system

| | Install | Choose parts | Upgrade | Uninstall (settings stay) | Also settings and profiles |
|---|---|---|---|---|---|
| macOS client | `brew install --cask …/minigw` (client + plugin) or `install.sh` | `minigw plugin install`/`uninstall` | `brew upgrade` | `brew uninstall --cask minigw` | `brew uninstall --zap --cask minigw` |
| macOS gateway | `brew install --cask …/mini-gateway` or the DMG | | `brew upgrade`, or the app | `brew uninstall --cask mini-gateway` | `brew uninstall --zap --cask mini-gateway` |
| Windows | the [MSI](#windows-installer-msi) | MSI window, **Modify**, or `ADDLOCAL=` | a newer MSI | Settings → Apps → Uninstall | **Modify** → tick → Remove, or `PURGE=1` |
| Linux | file in `~/.local/bin` ([Check a download](#check-a-download)) | `minigw plugin install`, `minigw serve install` | replace the file, or `/minigw:gw update` with a gateway | `minigw uninstall`, then `rm ~/.local/bin/minigw ~/.local/bin/minigw-tui` | `minigw uninstall --purge` first |

- `brew uninstall --zap --cask minigw` runs `minigw uninstall --purge --yes` while minigw is still there: plugin data,
  profiles (its own Keychain items), `~/Library/Application Support/minigw`, scratch folders.
- `brew uninstall --zap --cask mini-gateway` quits the app, lets the app itself delete its Keychain items and its
  login item (`MiniGateway --zap`; macOS asks nothing, because the app created them), then moves
  `~/Library/Application Support/Mini Gateway`, `~/Library/Caches/dev.trojar.minigateway` (scratch), its preferences,
  HTTP storage and saved window state to the Trash.
- Linux uses no installer of its own: `minigw serve install` puts `minigw` and `minigw-tui` in `~/.local/bin` with a
  systemd user unit, and `minigw uninstall` removes the unit, the plugin entries and scratch folders.

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
`swift scripts/make-icon.swift`. The Windows and Linux client is in `client/`; `minigw` uses only the
Go standard library: `cd client && go build`. The terminal settings helper uses Bubble Tea:
`cd client && go build ./cmd/minigw-tui`.
