cask "mini-gateway" do
  version "0.5.19"
  sha256 "31918233615a4bf1e22e88519d8114776b48d2fadd17ed30052cf54f884ce250"

  url "https://github.com/gh-commiter/mini-llm-gateway/releases/download/v#{version}/Mini-Gateway-#{version}.zip"
  name "Mini Gateway"
  desc "Menu bar LLM gateway compatible with the Claude Code CLI"
  homepage "https://github.com/gh-commiter/mini-llm-gateway"

  # Claude Code: le če ga še ni (native installer ali brew), da ne nastaneta dve kopiji
  claude_installed = ["~/.local/bin", "/opt/homebrew/bin", "/usr/local/bin", *ENV.fetch("PATH", "").split(":")]
                     .any? { |dir| File.executable?(File.join(File.expand_path(dir), "claude")) }

  depends_on arch: :arm64
  depends_on macos: :golden_gate

  depends_on cask: "claude-code" unless claude_installed

  app "Mini Gateway.app"

  zap trash: "~/Library/Preferences/dev.trojar.minigateway.plist"

  caveats <<~EOS
    The minigw command: Mini Gateway → Settings… → Claude CLI → Install (or brew install --cask
    gh-commiter/mini-gateway/minigw, which also adds the Claude Code plugin).
    The plugin by hand, in Claude Code: /plugin marketplace add gh-commiter/mini-llm-gateway, then
    /plugin install minigw@mini-gateway
  EOS
end
