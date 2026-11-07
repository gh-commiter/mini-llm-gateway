cask "minigw" do
  version "0.5.15"
  sha256 "90838a99c277a502f5b0c267efa509de3172c9074ec7002a794210353beca0ac"

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

  caveats <<~EOS
    Connect to the router Mac (token: Mini Gateway → Settings… → General → Copy Token):
      minigw connect <router-mac>[:port] --token <token>
  EOS
end
