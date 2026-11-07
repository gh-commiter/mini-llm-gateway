# Windows test checklist (combined build, HPX360)

One pass with the combined build, before the release. Each item: what to do, what you should see.

## Installer (MSI)
- Install `minigw-windows-amd64.msi` without admin: tray starts, `minigw` on PATH in a new terminal, Start menu entry.
- Install the same MSI again, then `minigw serve install`: "already up to date", nothing replaced.
- Uninstall in Settings → Apps: tray closes, PATH entry gone, `%AppData%\minigw` (config, keys) stays.
- `minigw serve uninstall` on an MSI install: refuses and points to Settings → Apps.

## Settings page pickers
- Choose… next to Allowed folders and Browse… next to a program path: the dialog opens in front of the browser.
- Cancel adds nothing; a path with spaces or č/š/ž comes back intact; a second Choose… while one is open shows 409.

## Profiles (client on Windows)
- `minigw connect <mac> --name mini` and `minigw connect 127.0.0.1 --name local`: both in `minigw profiles`.
- `minigw status` shows both; `minigw models` lists `…@mini` and `…@local`.
- In a session, `claude-cli-haiku@local` answers; with one gateway off, it is left out and `status` shows it.

## Worktree mode on the Windows gateway (minigw serve)
- From a client in a git repository: `minigw --minigw-work=worktree -p --model claude-cli-haiku "create hello.txt and commit it"`.
  Expected: minigw prints "(worktree)"; the client repository gets branch `minigw/claude-…-0` with `hello.txt`;
  the client's files, index and checked-out branch do not change.
- Uncommitted changes on the client are in the agent's start state (`git show minigw/claude-…-0:<file>`).
- A second request in the same session moves the same branch forward.
- `--minigw-work=auto` in a git repository picks worktree; outside a repository, sync.
- Codex with writing off, and Grok: refused with "use sync".
- Gateway without git on PATH: register refused with "git is not installed on the gateway".
- After the session: the clone in `<scratch>\worktrees\<device>\…` is removed; the mirror in `<scratch>\repos` stays.
