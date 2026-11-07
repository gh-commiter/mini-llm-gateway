# Release notes

## Unreleased

- One command to install the router, compatible with the Claude Code CLI: `/minigw:gw install` (plugin 0.5.2).
  Mac: the app with Homebrew and Open at Login. Windows: the tray at sign-in. Linux: systemd, at boot when linger
  works. `/minigw:gw update` when a session start says a newer router is out.
- The settings page of `minigw serve` (Windows, Linux) has the panes of the Mac's Settings, with Codex CLI and
  Grok CLI.
- Codex and Grok on `minigw serve` are off by default and run only in a synced folder. On Linux they need `bwrap`
  (home and config folders hidden), on Windows "Allow commands without a sandbox". Each device gets its own Codex
  and Grok sessions there. Devices that use Codex or Grok can read every file the router's user can read.
- Codex network access for commands is off by default, also on the Mac (saved settings keep their value).
- The router refuses admin requests from its own agent processes (Linux, Windows; the Mac app already did).
