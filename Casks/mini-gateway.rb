cask "mini-gateway" do
  version "0.5.1"
  sha256 "0daeb07e1a9a0f554c661c0fb66d0e907b5b6b87298be73fb03afe25fe9b2f7e"

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
    The minigw command: Mini Gateway → Settings… → Claude CLI → Install.
  EOS
end
