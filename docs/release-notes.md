# Release notes

## Unreleased

- **Worktree mode on `minigw serve` (Windows, Linux):** `--minigw-work=worktree` (and `auto` for a git repository) now
  also works with a Windows or Linux gateway, as on the Mac: the agent works in a clone on its own branch, and the branch
  comes back to your repository (fetched fast-forward only, no hooks or filters). Native mode and `minigw run` stay
  Mac-only. Plugin 0.5.9.
- **Several gateways at once (profiles, #17):** `minigw connect <host> --name <profile>` keeps every gateway, for
  example the Mac mini and a Windows gateway side by side. Models of other profiles are `<model>@<profile>`; the
  default profile's also appear without a suffix. An unreachable gateway is left out of the list, and `minigw status`
  shows each profile. The existing connection becomes the profile `default` with nothing to do. Plugin 0.5.7:
  `/minigw:connect --name`, and status and models show every profile.
- Windows installer: `minigw-windows-amd64.msi` and `minigw-windows-arm64.msi` install the gateway with the tray for this
  user, without admin rights (the same as `minigw serve install`: PATH, Start menu, start at sign-in). Listed in the
  signed `SHA256SUMS`; the MSI itself is not code-signed yet, so SmartScreen warns.
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
