cask "minigw" do
  version "0.5.18"
  sha256 "283fc795bfe1cc33a8860462f853d7dbfeb99ce12ca09448a59b147b35ae3a60"

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

  # Vtičnik za Claude Code: uradna ključa uporabniških nastavitev (extraKnownMarketplaces, enabledPlugins); Claude Code
  # ob naslednjem zagonu sam doda marketplace in namesti vtičnik, brez ukaza claude. Uporabnikov »false« ostane.
  settings = File.expand_path("~/.claude/settings.json")
  postflight do
    require "json"
    begin
      s = File.exist?(settings) ? JSON.parse(File.read(settings)) : {}
      if s.is_a?(Hash)
        (s["extraKnownMarketplaces"] ||= {})["mini-gateway"] ||= { "source" => { "source" => "github", "repo" => "gh-commiter/mini-llm-gateway" } }
        enabled = (s["enabledPlugins"] ||= {})
        enabled["minigw@mini-gateway"] = true unless enabled.key?("minigw@mini-gateway")
        FileUtils.mkdir_p(File.dirname(settings))
        File.write("#{settings}.minigw-tmp", JSON.pretty_generate(s) + "\n")
        File.rename("#{settings}.minigw-tmp", settings)
      end
    rescue JSON::ParserError, SystemCallError => e
      opoo "Claude Code plugin not added (#{settings}: #{e.message}); add it in Claude Code: /plugin install minigw@mini-gateway"
    end
  end

  uninstall_postflight do
    require "json"
    claude = ["~/.local/bin", "/opt/homebrew/bin", "/usr/local/bin"].map { |d| File.join(File.expand_path(d), "claude") }
                                                                    .find { |f| File.executable?(f) }
    system_command claude, args: ["plugin", "marketplace", "remove", "mini-gateway"], must_succeed: false if claude
    begin
      s = JSON.parse(File.read(settings))
      if s.is_a?(Hash) && (s.dig("extraKnownMarketplaces", "mini-gateway") || s.dig("enabledPlugins", "minigw@mini-gateway") != nil)
        s["extraKnownMarketplaces"]&.delete("mini-gateway")
        s["enabledPlugins"]&.delete("minigw@mini-gateway")
        File.write("#{settings}.minigw-tmp", JSON.pretty_generate(s) + "\n")
        File.rename("#{settings}.minigw-tmp", settings)
      end
    rescue JSON::ParserError, SystemCallError
      nil
    end
  end

  caveats <<~EOS
    The Claude Code plugin (minigw@mini-gateway) is added to ~/.claude/settings.json; Claude Code installs it the next
    time you start claude. To skip it, set "minigw@mini-gateway": false under enabledPlugins there.
    Connect to the router Mac (token: Mini Gateway → Settings… → General → Copy Token):
      minigw connect <router-mac>[:port] --token <token>
  EOS
end
