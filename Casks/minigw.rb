cask "minigw" do
  version "0.5.29"
  sha256 "e3fa1a20cab9964ed221e3f44f48bc2e3dd8c631360789471d2ff0efd973d07e"

  url "https://github.com/gh-commiter/mini-llm-gateway/releases/download/v#{version}/minigw.zip"
  name "minigw"
  desc "Client for Mini Gateway: runs the Claude Code CLI through it"
  homepage "https://github.com/gh-commiter/mini-llm-gateway"

  # Claude Code: le če ga še ni (native installer ali brew), da ne nastaneta dve kopiji
  claude_installed = ["~/.local/bin", "/opt/homebrew/bin", "/usr/local/bin", *ENV.fetch("PATH", "").split(":")]
                     .any? { |dir| File.executable?(File.join(File.expand_path(dir), "claude")) }

  depends_on arch: :arm64
  depends_on macos: :golden_gate

  depends_on cask: "claude-code" unless claude_installed

  binary "minigw"
  binary "minigw-tui"

  # Vtičnik za Claude Code: minigw plugin install (client/plugin.go, Sources/MiniGW/PluginCommand.swift) doda
  # marketplace (veja release, autoUpdate, #63) in enabledPlugins v ~/.claude/settings.json (uporabnikov »false«
  # ostane, neveljaven JSON ostane) in poveže bin/minigw vtičnika na ta minigw takoj, ne šele ob naslednji seji.
  # Izpiše, kar je res naredil (added / already in). Homebrew 7: postflight/uninstall_preflight (Ruby) sta opuščena
  # in opozarjata ob vsakem ukazu brew, *_steps pa tečejo v peskovniku (sandbox.rb add_install_hook_rules:
  # deny_read_home zapre ~/.claude in ~/.claude.json, HOME je začasna mapa). writable_paths (writable_base: :home)
  # ju odpre za ta korak (install_steps.rb sandbox_write_paths → allow_write_path + allow_read), HOME "" pa da minigw
  # pravo domačo mapo (prazen HOME: NSHomeDirectory(), ki na macOS bere uporabnikov zapis, ne HOME). Omrežja ni.
  postflight_steps do
    run "{{HOMEBREW_PREFIX}}/bin/minigw",
        args:           ["plugin", "install"],
        env:            { "HOME" => "" },
        writable_paths: [".claude"],
        writable_base:  :home,
        print_stdout:   true,
        must_succeed:   false
  end

  # Pred odstranitvijo (minigw je še tu): minigw plugin uninstall odstrani le, kar je dodal plugin install, in
  # marketplace (claude plugin marketplace remove piše tudi ~/.claude.json). Ob brew upgrade/reinstall/install nič:
  # vnosi, marketplace in povezava ostanejo (vtičnik ne izgine med tekočo sejo, postflight izpiše »already in«, #64).
  # Koraki dobijo le dobesedne argumente in ne vedo, kateri ukaz teče (UninstallPreflightSteps ne preda upgrade:,
  # Homebrew.running_command v peskovniku ni), zato ukaz brew prebere iz prednika brew.rb (ps; brew rm/remove →
  # uninstall). Ne najde ga: nič ne odstrani (kot ob nadgradnji); minigw plugin uninstall to naredi ročno.
  uninstall_preflight_steps do
    run "/bin/sh",
        args:           ["-c",
                         '[ -x "$0" ] || exit 0; p=$PPID c=; while [ -z "$c" ] && [ "${p:-1}" -gt 1 ]; do ' \
                         'c=$(/bin/ps -o args= -p "$p" | ' \
                         '/usr/bin/sed -n "s|.*/Library/Homebrew/brew\.rb \(-[^ ]* \)*\([a-z]*\).*|\2|p"); ' \
                         'p=$(/bin/ps -o ppid= -p "$p" | /usr/bin/tr -d " "); done; ' \
                         'case $c in uninstall) exec "$0" plugin uninstall ;; esac',
                         "{{HOMEBREW_PREFIX}}/bin/minigw"],
        env:            { "HOME" => "" },
        writable_paths: [".claude", ".claude.json"],
        writable_base:  :home,
        print_stdout:   true,
        must_succeed:   false
  end

  # brew uninstall --zap: minigw uninstall --purge --yes (minigw je še tu) odstrani še nastavitve, profile (Keychain,
  # z minigw-ovimi klici) in mapo vtičnika; brew uninstall brez --zap jih pusti kot doslej. Brez vprašanja: --zap je
  # izrecna zahteva, brew pa vprašanja ne more zanesljivo prikazati (#64); kaj gre, piše v caveats.
  zap script: {
    executable:   "/bin/sh",
    args:         ["-c", '[ ! -x "$0" ] || "$0" uninstall --purge --yes', "#{HOMEBREW_PREFIX}/bin/minigw"],
    must_succeed: false,
  }

  caveats <<~EOS
    minigw plugin install (run above) adds the Claude Code plugin (minigw@mini-gateway) to ~/.claude/settings.json
    unless it is already there; Claude Code installs it the next time you start claude (or run /reload-plugins) and
    updates it automatically from the release branch. To skip the plugin, set "minigw@mini-gateway": false under
    enabledPlugins there; to turn off auto-update, set "autoUpdate": false under extraKnownMarketplaces → mini-gateway
    (both stay, also on upgrade). brew uninstall removes the plugin again (your own mini-gateway entry stays); brew upgrade keeps it.
    brew uninstall --zap --cask minigw runs minigw uninstall --purge --yes without asking and removes: the plugin
    entries and the mini-gateway marketplace, ~/.claude/plugins/data/minigw-mini-gateway, the scratch folder
    ~/Library/Caches/minigw, the minigw profiles and device key in the Keychain, ~/Library/Application Support/minigw
    and an old minigw serve LaunchAgent, if there.
    Connect to the gateway Mac (token: Mini Gateway → Settings… → General → Copy Token):
      minigw connect <gateway-mac>[:port] --token <token>
  EOS
end
