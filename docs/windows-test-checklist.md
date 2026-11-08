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

## Devices (gateway on Windows)
- Connect a new client: it waits in the tray menu (Waiting for approval) and on the settings page (Devices), with a tray
  notice. The row shows `<user>@<computer>`, ID, address, time, OS and minigw version.
- Edit the name in the waiting row, then Allow: the device is allowed under that name. Reject: it disappears; its next
  connect waits again.
- `minigw gw devices allow <id> --name Laptop` and `minigw gw devices rename <id> --name Desk`: the name changes on the
  page at once. `minigw gw devices deny <id>` removes it (a name is not accepted).
- A waiting device with the same name as an allowed one shows ⚠ (same name).
- Allowed devices show last seen and OS/version; Copy ID copies the ID.
- "Allow any device with the token" can be turned on only while the allowed list is empty; with a device allowed it is off.
- A client without a device key (an old minigw, or plain HTTP from another computer) is refused unless the list is empty
  and "Allow any device" is on.
- Devices pane: Allowed clients (IP) and "Allow unencrypted connections" are there, not under General.

## Providers
- LiteLLM and OpenRouter are always listed (off until a key is set). They can be turned off, but not removed or renamed
  (page, `minigw gw provider remove`, admin API: 400).
- Add a provider at `http://localhost:4000`, `http://192.168.x.y` or `http://name.local` with "Allow http": accepted.
  Without the switch: refused.
- `http://example.com` or `http://gateway` (a name without a dot): refused even with the switch.

## This user's apps without a token (gateway on Windows)
- The switch "Allow this user's apps without a token" is under General on the settings page, off by default: the
  first curl below gives 401. Turn it on, then:
- `curl.exe -s -o NUL -w "%{http_code}" -H "anthropic-version: 2023-06-01" http://127.0.0.1:8765/v1/models`: 200.
- The same with `-H "Origin: http://example.com"` added: 401.
- The same without the `anthropic-version` header: 401.
- The same from another Windows user on this computer (`runas /user:<other> cmd`, then the curl): 401.
- Turn the switch off, then the first curl again: 401.

## Foundry Local
- `winget install Microsoft.FoundryLocal`, then Settings page → Foundry Local → Enabled, and send a message to a
  `claude-foundry-<model>`: is Foundry accepted? The gateway uses the port only when the listening program is this user's
  and inside Foundry's install folder (the folder of `foundry.exe`, or above its `bin` for a versioned package).
  winget installs an MSIX: `foundry.exe` is an alias in `%LOCALAPPDATA%\Microsoft\WindowsApps`, the server may run from
  `C:\Program Files\WindowsApps\…`. If the error is "Foundry Local's port belongs to another user's process: not used",
  note both paths (`where foundry`, Task Manager → Details → the listener's "Open file location").

## minigw gw CLI on Windows
- `minigw gw claude --folders C:\Projects --allow-local-tools=true`: folders and local tools show on the page.
  (Local tools also need "Allow commands without a sandbox" on Windows: refused otherwise.)
- `minigw gw autostart off` / `on`: the HKCU Run value for the tray goes away / comes back (tray menu "Start at sign-in").

## Scratch rules
- Settings page → General → Scratch folder: `C:\`, your home folder, `%TEMP%`, or a folder inside an allowed folder:
  refused with a reason. Choose… (tray) opens the folder dialog.
- Claude CLI → Allowed folders: add `%LOCALAPPDATA%` (it contains the default scratch folder) or a folder inside Scratch:
  refused ("contains or is inside the scratch folder"). The same with `minigw gw claude --folders`.
- Put such a folder into `serve.json` by hand and restart: the gateway starts, the log says Claude CLI and Codex are
  off until you remove it.
