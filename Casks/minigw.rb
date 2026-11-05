cask "minigw" do
  version "0.2"
  sha256 "94b49c07c1bb9920a4250657e44cd0bb5311c496a8aa695348c03d6af1e556b6"

  url "https://github.com/gh-commiter/mini-llm-gateway/releases/download/v#{version}/minigw.zip"
  name "minigw"
  desc "Client for a Mini Gateway router: runs the Claude Code CLI through it"
  homepage "https://github.com/gh-commiter/mini-llm-gateway"

  depends_on arch: :arm64
  depends_on macos: :golden_gate

  binary "minigw"

  caveats <<~EOS
    minigw needs the Claude Code CLI (claude):
      brew install --cask claude-code
    Connect to the router Mac (token: Mini Gateway → Settings… → General → Copy Token):
      minigw connect <router-mac>[:port] --token <token>
  EOS
end
