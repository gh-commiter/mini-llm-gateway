cask "mini-gateway" do
  version "0.2"
  sha256 "9f0abb9dc260b17c3e96cb79f746ccdebd5f536da28d83c6bf5ce01d29ea8898"

  url "https://github.com/gh-commiter/mini-llm-gateway/releases/download/v#{version}/Mini-Gateway-#{version}.zip"
  name "Mini Gateway"
  desc "Menu bar LLM gateway compatible with the Claude Code CLI"
  homepage "https://github.com/gh-commiter/mini-llm-gateway"

  depends_on arch: :arm64
  depends_on macos: :golden_gate

  app "Mini Gateway.app"

  zap trash: "~/Library/Preferences/dev.trojar.minigateway.plist"

  caveats <<~EOS
    The minigw command: Mini Gateway → Settings… → Claude CLI → Install.
  EOS
end
