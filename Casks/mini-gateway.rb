cask "mini-gateway" do
  version "0.5.26"
  sha256 "677675ff68921f99791d30b473397858a493bc6e86c5180f765f7343fa608b32"

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

  # brew uninstall --zap: najprej aplikacija sama (MiniGateway --zap) izbriše svoje vnose v Keychainu in zagon ob
  # prijavi (brez vprašanja, nikoli CLI security), nato datoteke. Nastavitve minigw (odjemalca) briše cask minigw.
  zap quit:   "dev.trojar.minigateway",
      script: {
        executable:   "/bin/sh",
        args:         ["-c", '[ ! -x "$0" ] || "$0" --zap', "#{appdir}/Mini Gateway.app/Contents/MacOS/MiniGateway"],
        must_succeed: false,
      },
      trash:  [
        "~/Library/Application Support/Mini Gateway",
        "~/Library/Caches/dev.trojar.minigateway",
        "~/Library/HTTPStorages/dev.trojar.minigateway",
        "~/Library/Preferences/dev.trojar.minigateway.plist",
        "~/Library/Saved Application State/dev.trojar.minigateway.savedState",
      ]

  caveats <<~EOS
    The minigw command: Mini Gateway → Settings… → Claude CLI → Install (or brew install --cask
    gh-commiter/mini-gateway/minigw, which also adds the Claude Code plugin).
    The plugin by hand, in Claude Code: /plugin marketplace add gh-commiter/mini-llm-gateway#release, then
    /plugin install minigw@mini-gateway, and turn on auto-update for mini-gateway under /plugin → Marketplaces
  EOS
end
