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
- [Build from source](#build-from-source)

## What the plugin does

- **At session start** it uses an installed `minigw` or downloads one from the latest release into its data folder.
  It checks the signed `SHA256SUMS` and, on macOS, the Developer ID signature. It works on macOS, Windows (Git Bash)
  and Linux.
- **Updates:** when the installed gateway is older than the release, the session start shows one line with
  `/minigw:gw update`. Nothing is updated without you, because an update stops the agent requests that run on the
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

The `minigw` cask also adds the Claude Code plugin (`minigw@mini-gateway`) to `~/.claude/settings.json`
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

[Check the download](#check-a-download), rename it to `minigw` (`minigw.exe` on Windows) and put it in your `PATH`.
Claude Code (`claude`) must be in your `PATH` too.

## Check a download

`SHA256SUMS` is signed with a key that is not on GitHub, so a changed release is refused. The plugin, `install.sh`
and `minigw serve install` check this themselves. By hand on Linux:

```sh
R=https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download
curl -fsSLO $R/minigw-linux-amd64 -O $R/SHA256SUMS -O $R/SHA256SUMS.sig
echo 'minigw-release namespaces="minigw-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINt++OHn+HHmR9mmA2UI+B0osuilO2GP0ddEzNp0GTrH' > signers
ssh-keygen -Y verify -f signers -I minigw-release -n minigw-release -s SHA256SUMS.sig < SHA256SUMS
grep ' minigw-linux-amd64$' SHA256SUMS | sha256sum -c -
mv minigw-linux-amd64 minigw && chmod +x minigw && sudo mv minigw /usr/local/bin/
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

Instead of the two commands, download `minigw-windows-amd64.msi` (or `-arm64.msi`) from the
[latest release](https://github.com/gh-commiter/mini-llm-gateway/releases/latest) and open it.

- It installs for this user only, without admin rights, the same as `minigw serve install`: `minigw.exe` and
  `minigw-tray.exe` in `%LocalAppData%\Programs\Mini Gateway`, minigw on your PATH, a Start menu entry, and the tray
  at sign-in.
- It then starts the tray, which creates the config on first start. Copy the token from its menu (**Copy Token**).
- Running a newer MSI updates it. Remove it in Settings → Apps (not with `minigw serve uninstall`); the config and keys
  stay.
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
`swift scripts/make-icon.swift`. The Windows and Linux client is in `client/` (Go standard library only):
`cd client && go build`.
