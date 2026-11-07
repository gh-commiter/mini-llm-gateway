# Release notes

## Unreleased

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
