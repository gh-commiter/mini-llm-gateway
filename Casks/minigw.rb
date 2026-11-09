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
  binary "minigw-tui"

  # Vtičnik za Claude Code: uradna ključa uporabniških nastavitev (extraKnownMarketplaces, enabledPlugins); Claude Code
  # ob naslednjem zagonu sam doda marketplace in namesti vtičnik, brez ukaza claude. Uporabnikov »false« ostane.
  # Pisanje skozi povezavo (dotfiles) in z isto pravico datoteke (lahko ima ključe v env; nova 0600).
  settings = File.expand_path("~/.claude/settings.json")
  plugin_bin = File.expand_path("~/.claude/plugins/data/minigw-mini-gateway/bin/minigw")
  marketplace = { "source" => { "source" => "github", "repo" => "gh-commiter/mini-llm-gateway" } }
  save = lambda do |data|
    real = File.exist?(settings) ? File.realpath(settings) : settings
    mode = File.exist?(real) ? File.stat(real).mode & 0o7777 : 0o600
    FileUtils.mkdir_p(File.dirname(real))
    File.open("#{real}.minigw-tmp", File::WRONLY | File::CREAT | File::TRUNC, mode) { |f| f.write(JSON.pretty_generate(data) + "\n") }
    File.chmod(mode, "#{real}.minigw-tmp")
    File.rename("#{real}.minigw-tmp", real)
  end

  postflight do
    require "json"
    begin
      s = File.exist?(settings) ? JSON.parse(File.read(settings)) : {}
      if s.is_a?(Hash)
        (s["extraKnownMarketplaces"] ||= {})["mini-gateway"] ||= marketplace
        enabled = (s["enabledPlugins"] ||= {})
        enabled["minigw@mini-gateway"] = true unless enabled.key?("minigw@mini-gateway")
        save.call(s)
      end
    rescue JSON::ParserError, SystemCallError => e
      opoo "Claude Code plugin not added (#{settings}: #{e.message}); add it in Claude Code: /plugin install minigw@mini-gateway"
    end
    # Skilli vtičnika kličejo minigw v njegovi mapi: povezava na ta minigw takoj, ne šele ob naslednji seji (hook
    # vtičnika naredi isto: nameščen in podpisan minigw ima prednost pred preneseno kopijo).
    begin
      FileUtils.mkdir_p(File.dirname(plugin_bin))
      FileUtils.rm_f(plugin_bin)
      File.symlink("#{HOMEBREW_PREFIX}/bin/minigw", plugin_bin)
    rescue SystemCallError => e
      opoo "minigw not linked for the Claude Code plugin (#{e.message}); a new Claude Code session sets it up"
    end
  end

  # Teče tudi ob brew upgrade/reinstall (nato postflight): odstrani le, kar je dodal cask (true, isti vir), uporabnikov
  # »false« ostane. Le ob pravi odstranitvi še »claude plugin marketplace remove« (datoteke vtičnika), ne ob nadgradnji,
  # da vtičnik ne izgine med tekočo sejo.
  uninstall_postflight do
    require "json"
    upgrading = defined?(Homebrew.running_command) && %w[install upgrade reinstall].include?(Homebrew.running_command)
    claude = ["~/.local/bin", "/opt/homebrew/bin", "/usr/local/bin"].map { |d| File.join(File.expand_path(d), "claude") }
                                                                    .find { |f| File.executable?(f) }
    if claude && !upgrading
      system_command claude, args: ["plugin", "marketplace", "remove", "mini-gateway", "--scope", "user"], must_succeed: false
    end
    if !upgrading && File.symlink?(plugin_bin) && File.readlink(plugin_bin) == "#{HOMEBREW_PREFIX}/bin/minigw"
      FileUtils.rm_f(plugin_bin)
    end
    begin
      s = JSON.parse(File.read(settings))
      if s.is_a?(Hash)
        changed = false
        if s.dig("enabledPlugins", "minigw@mini-gateway") == true
          s["enabledPlugins"].delete("minigw@mini-gateway")
          changed = true
        end
        if s.dig("extraKnownMarketplaces", "mini-gateway") == marketplace
          s["extraKnownMarketplaces"].delete("mini-gateway")
          changed = true
        end
        save.call(s) if changed
      end
    rescue JSON::ParserError, SystemCallError
      nil
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
