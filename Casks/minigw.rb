cask "minigw" do
  version "0.5.19"
  sha256 "3634588fa81775367b04cdb51556fcf6bdcd434ca86755e82d121717b6a31d7a"

  url "https://github.com/gh-commiter/mini-llm-gateway/releases/download/v#{version}/minigw.zip"
  name "minigw"
  desc "Client for a Mini Gateway router: runs the Claude Code CLI through it"
  homepage "https://github.com/gh-commiter/mini-llm-gateway"

  # Claude Code: le če ga še ni (native installer ali brew), da ne nastaneta dve kopiji
  claude_installed = ["~/.local/bin", "/opt/homebrew/bin", "/usr/local/bin", *ENV.fetch("PATH", "").split(":")]
                     .any? { |dir| File.executable?(File.join(File.expand_path(dir), "claude")) }

  depends_on arch: :arm64
  depends_on macos: :golden_gate

  depends_on cask: "claude-code" unless claude_installed

  binary "minigw"

  # Vtičnik za Claude Code: minigw plugin install (client/plugin.go, Sources/MiniGW/PluginCommand.swift) doda
  # marketplace in enabledPlugins v ~/.claude/settings.json (uporabnikov »false« ostane, neveljaven JSON ostane) in
  # poveže bin/minigw vtičnika na ta minigw takoj, ne šele ob naslednji seji.
  postflight do
    system_command "#{HOMEBREW_PREFIX}/bin/minigw", args: ["plugin", "install"], must_succeed: false, print_stdout: true
  end

  # Pred odstranitvijo (minigw je še tu), tudi ob brew upgrade/reinstall (nato postflight): minigw plugin uninstall
  # odstrani le, kar je dodal plugin install. Ob nadgradnji --upgrading: brez »claude plugin marketplace remove« in
  # povezava ostane, da vtičnik ne izgine med tekočo sejo.
  uninstall_preflight do
    upgrading = defined?(Homebrew.running_command) && %w[install upgrade reinstall].include?(Homebrew.running_command)
    minigw = "#{HOMEBREW_PREFIX}/bin/minigw"
    if File.executable?(minigw)
      system_command minigw, args: ["plugin", "uninstall", *(upgrading ? ["--upgrading"] : [])], must_succeed: false,
                             print_stdout: true
    end
  end

  caveats <<~EOS
    The Claude Code plugin (minigw@mini-gateway) is added to ~/.claude/settings.json; Claude Code installs it in the
    background the next time you start claude (or run /reload-plugins). To skip it, set "minigw@mini-gateway": false
    under enabledPlugins there (it stays, also on upgrade). brew uninstall removes the plugin again.
    Connect to the router Mac (token: Mini Gateway → Settings… → General → Copy Token):
      minigw connect <router-mac>[:port] --token <token>
  EOS
end
