#!/bin/sh
# SessionStart: zagotovi minigw v podatkovni mapi vtičnika (brez brew).
# 1) nameščen drugje (macOS: brew, aplikacija) → povezava nanj; 2) sicer prenos iz zadnje izdaje na GitHubu,
# ponovno, ko se SHA256SUMS spremeni (posodobitev); na macOS še podpis Developer ID (ekipa, identifikator).
# Windows (Git Bash) in Linux: minigw-<os>-<arch> iz iste izdaje.
# Izpis gre Claudu kot kontekst: ena vrstica.
set -u
REPO="gh-commiter/mini-llm-gateway"
REQ='anchor apple generic and identifier "dev.trojar.minigateway.cli" and certificate leaf[subject.OU] = "Y2M489RDV8"'
DATA="${CLAUDE_PLUGIN_DATA:-$HOME/.claude/plugins/data/minigw}"
BIN="$DATA/bin"
EXE="$BIN/minigw"

base="https://github.com/$REPO/releases/latest/download"
sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux; asset="minigw-linux-$arch" ;;
  MINGW*|MSYS*|CYGWIN*) os=windows; asset="minigw-windows-$arch.exe"; EXE="$BIN/minigw.exe" ;;  # Git Bash: »minigw« najde minigw.exe
  *) echo "minigw plugin: no minigw client for $(uname -s)."; exit 0 ;;
esac
mkdir -p "$BIN"

# Kontrolne vsote zadnje izdaje: po njih se prenesena kopija posodobi (brez omrežja ostane obstoječa).
sums=$(curl -fsSL --max-time 20 "$base/SHA256SUMS" 2>/dev/null || true)
want() { printf '%s\n' "$sums" | grep " $1\$" | cut -d' ' -f1; }
stamp="$BIN/.asset.sha256"  # vsota prenesenega paketa (na macOS je to minigw.zip, ne izvršljiva datoteka)

# fetch <asset>: prenese, preveri vsoto (in na macOS podpis) ter namesti v $EXE.
fetch() {
  w=$(want "$1"); [ -n "$w" ] || return 1
  tmp=$(mktemp -d) || return 1
  if curl -fsSL -o "$tmp/$1" "$base/$1" && [ "$(sha "$tmp/$1")" = "$w" ]; then
    if [ "$os" = darwin ]; then
      ditto -x -k "$tmp/$1" "$tmp/x" && valid "$tmp/x/minigw" && rm -f "$EXE" && mv "$tmp/x/minigw" "$EXE"
    else
      chmod +x "$tmp/$1" && mv -f "$tmp/$1" "$EXE"
    fi && echo "$w" > "$stamp" && { rm -rf "$tmp"; return 0; }
  fi
  rm -rf "$tmp"; return 1
}

if [ "$os" = darwin ]; then
  # macOS: podpisan minigw (Developer ID, ekipa in identifikator) iz minigw.zip ali že nameščen (brew).
  valid() { [ -x "$1" ] && codesign --verify --strict -R="$REQ" "$1" 2>/dev/null; }
  # Nameščen minigw (brew, aplikacija) ima vedno prednost pred preneseno kopijo: posodablja ga brew.
  other=""
  for c in "$(command -v minigw 2>/dev/null || true)" /opt/homebrew/bin/minigw /usr/local/bin/minigw \
           "/Applications/Mini Gateway.app/Contents/Helpers/minigw"; do
    [ -n "$c" ] && [ "$c" != "$EXE" ] && valid "$c" && { other=$c; break; }
  done
  if [ -n "$other" ]; then
    [ "$(readlink "$EXE" 2>/dev/null)" = "$other" ] || { rm -f "$EXE"; ln -s "$other" "$EXE"; }
  elif [ -L "$EXE" ] || ! valid "$EXE" || { [ -n "$(want minigw.zip)" ] && [ "$(cat "$stamp" 2>/dev/null)" != "$(want minigw.zip)" ]; }; then
    fetch minigw.zip || valid "$EXE" || {
      echo "minigw plugin: could not download or verify minigw from github.com/$REPO; gateway skills are unavailable."; exit 0; }
  fi
else
  # Windows/Linux: ena izvršljiva datoteka (Go, brez odvisnosti); kontrolna vsota iz SHA256SUMS iste izdaje.
  if [ ! -x "$EXE" ] || { [ -n "$(want "$asset")" ] && [ "$(sha "$EXE")" != "$(want "$asset")" ]; }; then
    fetch "$asset" || [ -x "$EXE" ] || {
      echo "minigw plugin: could not download or verify $asset from github.com/$REPO; gateway skills are unavailable."; exit 0; }
  fi
fi

# Nameščen router starejši od izdaje: le namig (posodobitev bi prekinila tekoče zahteve agentov).
ver() { "$1" --version 2>/dev/null | head -1 | sed 's/^minigw //'; }
older() { [ -n "$1" ] && [ -n "$2" ] && [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -1)" = "$1" ]; }
old=""; new=$(ver "$EXE")
case "$os" in
  darwin) app="/Applications/Mini Gateway.app"
    [ -d "$app" ] && old=$(plutil -extract CFBundleShortVersionString raw "$app/Contents/Info.plist" 2>/dev/null)
    # minigw vtičnika je tu lahko kar minigw aplikacije: zadnja izdaja iz preusmeritve releases/latest
    new=$(curl -fsS -o /dev/null --max-time 10 -w '%{redirect_url}' "https://github.com/$REPO/releases/latest" 2>/dev/null | sed -n 's|.*/tag/v||p') ;;
  linux) [ -f "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/minigw-serve.service" ] && old=$(ver "$HOME/.local/bin/minigw") ;;
  windows) [ -n "${LOCALAPPDATA:-}" ] && command -v cygpath >/dev/null 2>&1 &&
    old=$(ver "$(cygpath -u "$LOCALAPPDATA")/Programs/Mini Gateway/minigw.exe") ;;
esac
# Le številke in pike: izpis gre Claudu kot kontekst (F6).
case $new in *[!0-9.]*) new="" ;; esac
case $old in *[!0-9.]*) old="" ;; esac
older "$old" "$new" && echo "Mini Gateway router $old is installed, $new is available: /minigw:gw update"

# Claudov Bash najde »minigw« tudi brez poti.
[ -n "${CLAUDE_ENV_FILE:-}" ] && echo "export PATH=\"\$PATH:$BIN\"" >> "$CLAUDE_ENV_FILE"
if "$EXE" status >/dev/null 2>&1; then
  echo "minigw plugin: $("$EXE" --version 2>/dev/null | head -1), connected to Mini Gateway."
else
  echo "minigw plugin: $("$EXE" --version 2>/dev/null | head -1) installed but not connected; use /minigw:connect <router-mac> <token>."
fi
