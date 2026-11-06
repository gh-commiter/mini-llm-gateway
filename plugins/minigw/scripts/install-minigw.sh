#!/bin/sh
# SessionStart: zagotovi minigw v podatkovni mapi vtičnika (brez brew).
# 1) že tam in veljaven → nič; 2) nameščen drugje (brew) → povezava nanj;
# 3) sicer prenos iz izdaje na GitHubu: SHA256SUMS + podpis Developer ID (ekipa, identifikator).
# Izpis gre Claudu kot kontekst: ena vrstica.
set -u
REPO="gh-commiter/mini-llm-gateway"
REQ='anchor apple generic and identifier "dev.trojar.minigateway.cli" and certificate leaf[subject.OU] = "Y2M489RDV8"'
DATA="${CLAUDE_PLUGIN_DATA:-$HOME/.claude/plugins/data/minigw}"
BIN="$DATA/bin"
EXE="$BIN/minigw"

if [ "$(uname -s)" != "Darwin" ]; then
  echo "minigw plugin: the minigw client is macOS-only for now; gateway skills are unavailable on this system."
  exit 0
fi
mkdir -p "$BIN"
valid() { [ -x "$1" ] && codesign --verify --strict -R="$REQ" "$1" 2>/dev/null; }

if ! valid "$EXE"; then
  rm -f "$EXE"
  other=$(command -v minigw 2>/dev/null || true)
  if [ -n "$other" ] && valid "$other"; then
    ln -sf "$other" "$EXE"
  else
    tmp=$(mktemp -d) || exit 0
    base="https://github.com/$REPO/releases/latest/download"
    if curl -fsSL -o "$tmp/minigw.zip" "$base/minigw.zip" && curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" \
       && [ "$(shasum -a 256 "$tmp/minigw.zip" | cut -d' ' -f1)" = "$(grep ' minigw.zip$' "$tmp/SHA256SUMS" | cut -d' ' -f1)" ] \
       && ditto -x -k "$tmp/minigw.zip" "$tmp/x" && valid "$tmp/x/minigw"; then
      mv "$tmp/x/minigw" "$EXE"
    else
      echo "minigw plugin: could not download or verify minigw from github.com/$REPO; gateway skills are unavailable."
      rm -rf "$tmp"
      exit 0
    fi
    rm -rf "$tmp"
  fi
fi

# Claudov Bash najde »minigw« tudi brez poti.
[ -n "${CLAUDE_ENV_FILE:-}" ] && echo "export PATH=\"\$PATH:$BIN\"" >> "$CLAUDE_ENV_FILE"
if "$EXE" status >/dev/null 2>&1; then
  echo "minigw plugin: $("$EXE" --version 2>/dev/null | head -1), connected to Mini Gateway."
else
  echo "minigw plugin: $("$EXE" --version 2>/dev/null | head -1) installed but not connected; use /minigw:connect <router-mac> <token>."
fi
