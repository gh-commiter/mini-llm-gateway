#!/bin/sh
# Mini Gateway — client install: Claude Code + the minigw command, for a Mac that uses a gateway on another Mac.
#
#   curl -fsSL https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/install.sh | sh
#   curl -fsSL …/install.sh | sh -s -- mini.local        # also connect; asks for the token
#   … | sh -s -- --allow-downgrade [gateway]             # install a release older than the installed minigw
#
# Token: Mini Gateway → Settings → General → Copy Token (on the gateway Mac).
set -eu

REPO="gh-commiter/mini-llm-gateway"
TEAM_ID="Y2M489RDV8"                      # Developer ID that signs minigw
BASE="${MINIGW_BASE:-https://github.com/$REPO/releases/latest/download}"   # MINIGW_BASE: test/mirror

ALLOW_DOWNGRADE=0
[ "${1:-}" = --allow-downgrade ] && { ALLOW_DOWNGRADE=1; shift; }

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = Darwin ] || die "macOS only"
[ "$(uname -m)" = arm64 ] || die "Apple Silicon only"

# 1. Claude Code
if command -v claude >/dev/null 2>&1; then
  say "Claude Code: $(claude --version 2>/dev/null || echo installed)"
else
  BREW=$(command -v brew || { [ -x /opt/homebrew/bin/brew ] && echo /opt/homebrew/bin/brew; } || true)
  if [ -n "$BREW" ]; then
    say "Installing Claude Code with Homebrew…"
    "$BREW" install --cask claude-code
  else
    say "Installing Claude Code with the native installer…"
    curl -fsSL https://claude.ai/install.sh | bash
  fi
fi

# 2. minigw: download, verify checksum and signature
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
say "Downloading minigw…"
curl -fsSL -o "$TMP/minigw.zip" "$BASE/minigw.zip"
curl -fsSL -o "$TMP/SHA256SUMS" "$BASE/SHA256SUMS"
curl -fsSL -o "$TMP/SHA256SUMS.sig" "$BASE/SHA256SUMS.sig" || die "no SHA256SUMS.sig in the release"
# SHA256SUMS je podpisan s ključem Mini Gateway (ključ pripet tu); ssh-keygen je del macOS.
printf '%s\n' 'minigw-release namespaces="minigw-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINt++OHn+HHmR9mmA2UI+B0osuilO2GP0ddEzNp0GTrH' > "$TMP/signers"
ssh-keygen -Y verify -f "$TMP/signers" -I minigw-release -n minigw-release -s "$TMP/SHA256SUMS.sig" < "$TMP/SHA256SUMS" >/dev/null 2>&1 \
  || die "SHA256SUMS is not signed by Mini Gateway"
(cd "$TMP" && grep ' minigw.zip$' SHA256SUMS | shasum -a 256 -c - >/dev/null) || die "checksum mismatch"
# #25: not back to an older signed release. Its version is the signed "VERSION <v>" line; a signed release without it
# is 0.5.17 or older (the line exists since 0.5.18; the URL is no help: GitHub redirects to its CDN).
REL=$(sed -n 's/^VERSION \([0-9][0-9.]*\)$/\1/p' "$TMP/SHA256SUMS" | head -1)
[ -n "$REL" ] || REL=0.5.17
HAVE=$(minigw --version 2>/dev/null | head -1 | sed -n 's/^minigw \([0-9][0-9.]*\)$/\1/p' || true)
if [ -n "$REL" ] && [ -n "$HAVE" ] && [ "$REL" != "$HAVE" ] && [ "$(printf '%s\n%s\n' "$REL" "$HAVE" | sort -V | head -1)" = "$REL" ] \
   && [ $ALLOW_DOWNGRADE = 0 ]; then
  die "this release ($REL) is older than the installed minigw $HAVE; to go back anyway: … | sh -s -- --allow-downgrade"
fi
ditto -x -k "$TMP/minigw.zip" "$TMP/out"
codesign --verify --strict "$TMP/out/minigw" || die "invalid signature"
codesign -dv "$TMP/out/minigw" 2>&1 | grep -q "TeamIdentifier=$TEAM_ID" || die "unexpected signer"

# 3. Into the user's PATH: the usual place for own commands, writable without a password
DIR=""
for d in "$HOME/.local/bin" "$HOME/bin" /opt/homebrew/bin /usr/local/bin; do
  case ":$PATH:" in *":$d:"*) [ -w "$d" ] && { DIR=$d; break; } ;; esac
done
IN_PATH=1
[ -n "$DIR" ] || { DIR="$HOME/.local/bin"; IN_PATH=0; mkdir -p "$DIR"; }
install -m 755 "$TMP/out/minigw" "$DIR/minigw"
say "Installed: $DIR/minigw"
[ $IN_PATH = 1 ] || say "Add it to your PATH:  echo 'export PATH=\"$DIR:\$PATH\"' >> ~/.zshrc"

# 4. Optional: connect to the gateway (token read from the terminal, not the command line)
if [ $# -ge 1 ]; then
  printf 'Gateway token for %s: ' "$1" >/dev/tty
  stty -echo </dev/tty; read -r TOKEN </dev/tty; stty echo </dev/tty; printf '\n' >/dev/tty
  printf '%s\n' "$TOKEN" | "$DIR/minigw" connect "$1" --token -  # stdin: the token never appears in the process arguments
else
  say "Next: minigw connect <gateway>[:port] --token <token>"
fi
