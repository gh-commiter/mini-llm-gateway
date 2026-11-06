#!/bin/sh
# SessionStart: zagotovi minigw v podatkovni mapi vtičnika (brez brew).
# 1) že tam in veljaven → nič; 2) nameščen drugje (brew) → povezava nanj;
# 3) sicer prenos iz izdaje na GitHubu: SHA256SUMS; na macOS še podpis Developer ID (ekipa, identifikator).
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
  elif ! valid "$EXE"; then
    rm -f "$EXE"
    tmp=$(mktemp -d) || exit 0
    if curl -fsSL -o "$tmp/minigw.zip" "$base/minigw.zip" && curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" \
       && [ "$(sha "$tmp/minigw.zip")" = "$(grep ' minigw.zip$' "$tmp/SHA256SUMS" | cut -d' ' -f1)" ] \
       && ditto -x -k "$tmp/minigw.zip" "$tmp/x" && valid "$tmp/x/minigw"; then
      mv "$tmp/x/minigw" "$EXE"
    else
      echo "minigw plugin: could not download or verify minigw from github.com/$REPO; gateway skills are unavailable."
      rm -rf "$tmp"; exit 0
    fi
    rm -rf "$tmp"
  fi
elif [ ! -x "$EXE" ]; then
  # Windows/Linux: ena izvršljiva datoteka (Go, brez odvisnosti); kontrolna vsota iz SHA256SUMS iste izdaje.
  tmp=$(mktemp -d) || exit 0
  if curl -fsSL -o "$tmp/$asset" "$base/$asset" && curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" \
     && [ "$(sha "$tmp/$asset")" = "$(grep " $asset\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)" ]; then
    chmod +x "$tmp/$asset" && mv "$tmp/$asset" "$EXE"
  else
    echo "minigw plugin: could not download or verify $asset from github.com/$REPO; gateway skills are unavailable."
    rm -rf "$tmp"; exit 0
  fi
  rm -rf "$tmp"
fi

# Claudov Bash najde »minigw« tudi brez poti.
[ -n "${CLAUDE_ENV_FILE:-}" ] && echo "export PATH=\"\$PATH:$BIN\"" >> "$CLAUDE_ENV_FILE"
if "$EXE" status >/dev/null 2>&1; then
  echo "minigw plugin: $("$EXE" --version 2>/dev/null | head -1), connected to Mini Gateway."
else
  echo "minigw plugin: $("$EXE" --version 2>/dev/null | head -1) installed but not connected; use /minigw:connect <router-mac> <token>."
fi
