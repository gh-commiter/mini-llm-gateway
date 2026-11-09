#!/bin/sh
# Mini Gateway — install for macOS (client) and Linux (client, gateway): Claude Code, the minigw command and the
# Claude Code plugin.
#
#   curl -fsSL https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/install.sh | sh
#   curl -fsSL …/install.sh | sh -s -- mini.local        # also connect; asks for the token
#   … | sh -s -- --client | --plugin | --gateway [gateway] # parts (default: client + plugin; --gateway: Linux only)
#   … | sh -s -- --allow-downgrade [gateway]             # install a release older than the installed minigw
#   … | sh -s -- --uninstall [--purge]                   # remove it again (--purge: also settings and profiles)
#
# Token: Mini Gateway → Settings → General → Copy Token (on the gateway Mac), or minigw gw token on the gateway.
set -eu

REPO="gh-commiter/mini-llm-gateway"
TEAM_ID="Y2M489RDV8"                      # Developer ID that signs minigw
BASE="${MINIGW_BASE:-https://github.com/$REPO/releases/latest/download}"   # MINIGW_BASE: test/mirror

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

# Vse v main: skrajšan prenos (curl … | sh) se ne izvede napol.
main() {
ALLOW_DOWNGRADE=0 UNINSTALL=0 PURGE=0 PARTS=""
while [ $# -gt 0 ]; do
  case $1 in
    --allow-downgrade) ALLOW_DOWNGRADE=1 ;;
    --client|--plugin|--gateway) PARTS="$PARTS ${1#--}" ;;
    --uninstall) UNINSTALL=1 ;;
    --purge) PURGE=1 ;;
    -*) die "unknown option $1 (--client, --plugin, --gateway, --uninstall [--purge], --allow-downgrade)" ;;
    *) break ;;
  esac
  shift
done
[ $PURGE = 0 ] || [ $UNINSTALL = 1 ] || die "--purge goes with --uninstall"
# Deli: brez zastavic odjemalec in vtičnik; z zastavicami le navedeni (odjemalec vedno, drugi ga potrebujejo).
PLUGIN=1 GATEWAY=0
if [ -n "$PARTS" ]; then
  case " $PARTS " in *" plugin "*) ;; *) PLUGIN=0 ;; esac
  case " $PARTS " in *" gateway "*) GATEWAY=1 ;; esac
fi

OS=$(uname -s)
case $OS in
  Darwin) [ "$(uname -m)" = arm64 ] || die "Apple Silicon only"
    [ $GATEWAY = 0 ] || die "on macOS the gateway is the Mini Gateway app: brew install --cask gh-commiter/mini-gateway/mini-gateway (or the DMG)" ;;
  Linux) case $(uname -m) in x86_64|amd64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; *) die "Linux on x86_64 or arm64 only" ;; esac ;;
  *) die "macOS or Linux only (Windows: the MSI, docs/install.md)" ;;
esac
[ "$(id -u)" != 0 ] || die "run it as yourself, not as root: it installs for your user only"

# Uninstall: minigw uninstall (plugin, systemd unit, scratch; --purge also settings and profiles), then the files.
# Only a file this script installs (not Homebrew's link).
if [ $UNINSTALL = 1 ]; then
  M=""
  if [ "$OS" = Linux ]; then dirs="$HOME/.local/bin"; else dirs="$HOME/.local/bin $HOME/bin /opt/homebrew/bin /usr/local/bin"; fi
  for d in $dirs; do [ -f "$d/minigw" ] && [ ! -L "$d/minigw" ] && { M="$d/minigw"; break; }; done
  [ -n "$M" ] || die "no minigw installed by install.sh (Homebrew: brew uninstall --cask minigw)"
  if [ $PURGE = 1 ]; then "$M" uninstall --purge --yes; else "$M" uninstall --yes; fi || die "minigw uninstall failed; $M stays"
  rm -f "$M" "${M%/minigw}/minigw-tui"
  say "Removed: $M"
  exit 0
fi

# 1. Claude Code
if command -v claude >/dev/null 2>&1; then
  say "Claude Code: $(claude --version 2>/dev/null || echo installed)"
else
  BREW=$(command -v brew || { [ -x /opt/homebrew/bin/brew ] && echo /opt/homebrew/bin/brew; } || true)
  if [ "$OS" = Darwin ] && [ -n "$BREW" ]; then
    say "Installing Claude Code with Homebrew…"
    "$BREW" install --cask claude-code
  else
    say "Installing Claude Code with the native installer…"
    curl -fsSL https://claude.ai/install.sh | bash
  fi
fi

# 2. minigw: download, verify checksum and signature
if [ "$OS" = Linux ]; then
  ASSET="minigw-linux-$ARCH" TUI="minigw-tui-linux-$ARCH"
  command -v ssh-keygen >/dev/null 2>&1 || die "checking the release signature needs ssh-keygen: install OpenSSH (openssh-client)"
else
  ASSET=minigw.zip TUI=""
fi
sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
say "Downloading minigw…"
curl -fsSL -o "$TMP/$ASSET" "$BASE/$ASSET"
curl -fsSL -o "$TMP/SHA256SUMS" "$BASE/SHA256SUMS"
curl -fsSL -o "$TMP/SHA256SUMS.sig" "$BASE/SHA256SUMS.sig" || die "no SHA256SUMS.sig in the release"
# SHA256SUMS je podpisan s ključem Mini Gateway (ključ pripet tu); ssh-keygen je del macOS (Linux: openssh-client).
printf '%s\n' 'minigw-release namespaces="minigw-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINt++OHn+HHmR9mmA2UI+B0osuilO2GP0ddEzNp0GTrH' > "$TMP/signers"
ssh-keygen -Y verify -f "$TMP/signers" -I minigw-release -n minigw-release -s "$TMP/SHA256SUMS.sig" < "$TMP/SHA256SUMS" >/dev/null 2>&1 \
  || die "SHA256SUMS is not signed by Mini Gateway"
want() { grep " $1\$" "$TMP/SHA256SUMS" | cut -d' ' -f1 | head -1; }
[ -n "$(want "$ASSET")" ] && [ "$(sha "$TMP/$ASSET")" = "$(want "$ASSET")" ] || die "checksum mismatch"
# minigw gw settings (terminal UI, 0.5.20+): only when the release lists it
if [ -n "$TUI" ] && [ -n "$(want "$TUI")" ]; then
  curl -fsSL -o "$TMP/$TUI" "$BASE/$TUI" && [ "$(sha "$TMP/$TUI")" = "$(want "$TUI")" ] || die "checksum mismatch: $TUI"
fi
# #25: not back to an older signed release. Its version is the signed "VERSION <v>" line; a signed release without it
# is 0.5.17 or older (the line exists since 0.5.18; the URL is no help: GitHub redirects to its CDN).
REL=$(sed -n 's/^VERSION \([0-9][0-9.]*\)$/\1/p' "$TMP/SHA256SUMS" | head -1)
[ -n "$REL" ] || REL=0.5.17
HAVE=$(minigw --version 2>/dev/null | head -1 | sed -n 's/^minigw \([0-9][0-9.]*\)$/\1/p' || true)
[ -n "$HAVE" ] || HAVE=$("$HOME/.local/bin/minigw" --version 2>/dev/null | head -1 | sed -n 's/^minigw \([0-9][0-9.]*\)$/\1/p' || true)
# REL < HAVE po številskih poljih (awk; BusyBox nima sort -V)
if [ -n "$REL" ] && [ -n "$HAVE" ] && awk -v a="$REL" -v b="$HAVE" 'BEGIN { n = split(a, x, "."); m = split(b, y, "."); if (m > n) n = m
     for (i = 1; i <= n; i++) { if (x[i] + 0 < y[i] + 0) exit 0; if (x[i] + 0 > y[i] + 0) exit 1 }; exit 1 }' \
   && [ $ALLOW_DOWNGRADE = 0 ]; then
  die "this release ($REL) is older than the installed minigw $HAVE; to go back anyway: … | sh -s -- --allow-downgrade"
fi
mkdir -p "$TMP/out"
if [ "$OS" = Darwin ]; then
  ditto -x -k "$TMP/minigw.zip" "$TMP/out"
  codesign --verify --strict "$TMP/out/minigw" || die "invalid signature"
  codesign -dv "$TMP/out/minigw" 2>&1 | grep -q "TeamIdentifier=$TEAM_ID" || die "unexpected signer"
  # minigw gw settings (terminal UI, 0.5.20+): checked before anything is installed, same signer as minigw
  if [ -f "$TMP/out/minigw-tui" ]; then
    codesign --verify --strict "$TMP/out/minigw-tui" || die "invalid signature of minigw-tui"
    codesign -dv "$TMP/out/minigw-tui" 2>&1 | grep -q "TeamIdentifier=$TEAM_ID" || die "unexpected signer of minigw-tui"
  fi
else
  mv "$TMP/$ASSET" "$TMP/out/minigw"
  [ -z "$TUI" ] || [ ! -f "$TMP/$TUI" ] || mv "$TMP/$TUI" "$TMP/out/minigw-tui"
fi

# 3. Into the user's PATH: the usual place for own commands, writable without a password. Linux: always ~/.local/bin,
# where the plugin and minigw serve install find it.
DIR=""
if [ "$OS" = Darwin ]; then
  for d in "$HOME/.local/bin" "$HOME/bin" /opt/homebrew/bin /usr/local/bin; do
    case ":$PATH:" in *":$d:"*) [ -w "$d" ] && { DIR=$d; break; } ;; esac
  done
fi
IN_PATH=1
[ -n "$DIR" ] || { DIR="$HOME/.local/bin"; mkdir -p "$DIR"; case ":$PATH:" in *":$DIR:"*) ;; *) IN_PATH=0 ;; esac; }
install -m 755 "$TMP/out/minigw" "$DIR/minigw"
say "Installed: $DIR/minigw"
[ -f "$TMP/out/minigw-tui" ] && install -m 755 "$TMP/out/minigw-tui" "$DIR/minigw-tui"
RC='~/.zshrc'; [ "$OS" = Darwin ] || RC='~/.profile'
[ $IN_PATH = 1 ] || say "Add it to your PATH:  echo 'export PATH=\"$DIR:\$PATH\"' >> $RC"

# 4. Parts: the Claude Code plugin (compatible with the Claude Code CLI), and on Linux the gateway (minigw serve)
if [ $PLUGIN = 1 ]; then
  "$DIR/minigw" plugin install || say "The plugin was not added; later: minigw plugin install"
fi
if [ $GATEWAY = 1 ]; then
  "$DIR/minigw" serve init   # keeps an existing config; prints the connect line for clients
  "$DIR/minigw" serve install || die "minigw serve install failed"
fi

# 5. Optional: connect to the gateway (token read from the terminal, not the command line)
if [ $# -ge 1 ]; then
  printf 'Gateway token for %s: ' "$1" >/dev/tty
  stty -echo </dev/tty; read -r TOKEN </dev/tty; stty echo </dev/tty; printf '\n' >/dev/tty
  printf '%s\n' "$TOKEN" | "$DIR/minigw" connect "$1" --token -  # stdin: the token never appears in the process arguments
else
  say "Next: minigw connect <gateway>[:port] --token <token>"
fi
}

main "$@"
