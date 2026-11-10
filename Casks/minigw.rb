cask "minigw" do
  version "0.5.22"
  sha256 "9dfd416c4c2e18d0bcabe0625c46f6c33fcf15f89aee4999db5fda7640eb97c0"

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
  # Izpiše, kar je res naredil (added / already in). #64: postflight_steps (nadomestek) v peskovniku Homebrew ne doseže
  # ~/.claude: HOME je začasna mapa (sandbox.rb, run: "HOME" => tmpdir), ~/.claude in ~/.claude.json pa sta na seznamu
  # zaprtih poti (deny_read_home); omrežje bi lahko zahtevali (network_access). Zato ostane postflight.
  postflight do
    system_command "#{HOMEBREW_PREFIX}/bin/minigw", args: ["plugin", "install"], must_succeed: false, print_stdout: true
  end

  # Pred odstranitvijo (minigw je še tu): minigw plugin uninstall odstrani le, kar je dodal plugin install. Ob brew
  # upgrade/reinstall (nato postflight) nič: vnosi, marketplace in povezava ostanejo (vtičnik ne izgine med tekočo
  # sejo), postflight pa pravilno izpiše »already in« (#64; prej je --upgrading vnose odstranil in postflight je
  # izpisal »added«). uninstall_preflight_steps ne gre: sprejme le dobesedne argumente, nadgradnjo pa loči le
  # Homebrew.running_command.
  uninstall_preflight do
    upgrading = defined?(Homebrew.running_command) && %w[install upgrade reinstall].include?(Homebrew.running_command)
    minigw = "#{HOMEBREW_PREFIX}/bin/minigw"
    if !upgrading && File.executable?(minigw)
      system_command minigw, args: ["plugin", "uninstall"], must_succeed: false, print_stdout: true
    end
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
