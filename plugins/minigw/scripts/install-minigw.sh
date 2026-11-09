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

base="${MINIGW_RELEASE_BASE:-https://github.com/$REPO/releases/latest/download}"  # testi: lažna izdaja (podpis je pripet)
# #8: SHA256SUMS je podpisan s ključem Mini Gateway (ssh-keygen -Y sign -n minigw-release); ključ je pripet tu.
SIGNER='minigw-release namespaces="minigw-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINt++OHn+HHmR9mmA2UI+B0osuilO2GP0ddEzNp0GTrH'
sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux; asset="minigw-linux-$arch" ;;
  MINGW*|MSYS*|CYGWIN*) os=windows; asset="minigw-windows-$arch.exe"; EXE="$BIN/minigw.exe" ;;  # Git Bash: »minigw« najde minigw.exe
  *) echo "minigw plugin: no minigw client for $(uname -s)."; exit 0 ;;
esac
mkdir -p "$BIN"


ver() { "$1" --version 2>/dev/null | head -1 | sed 's/^minigw //'; }
MIN=0.5.19  # najnižji minigw za ta vtičnik (profili, samo za branje z lokalnim prehodom, obseg veje le agenta)
older() { [ -n "$1" ] && [ -n "$2" ] && [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -1)" = "$1" ]; }

# verify_sums <sums> <sig>: najprej ssh-keygen; sicer že nameščen minigw, a le 0.5.17 ali novejši (pozna verify-sums)
# in le z natanko pričakovanim izpisom. Starejši minigw neznan ukaz preda claude, ki lahko vrne 0 (H1 0.5.17).
# 0 = podpisano, 1 = ni podpisano, 2 = ni s čim preveriti.
verify_sums() {
  if command -v ssh-keygen >/dev/null 2>&1; then
    a=$(mktemp) || return 1
    printf '%s\n' "$SIGNER" > "$a"
    ssh-keygen -Y verify -f "$a" -I minigw-release -n minigw-release -s "$2" < "$1" >/dev/null 2>&1; r=$?
    rm -f "$a"; [ $r -eq 0 ] && return 0 || return 1
  fi
  [ -x "$EXE" ] && [ ! -L "$EXE" ] || return 2
  v=$(ver "$EXE")
  case $v in *[!0-9.]*|"") return 2 ;; esac
  older "$v" 0.5.17 && return 2
  [ "$("$EXE" verify-sums "$1" "$2" 2>/dev/null)" = "SHA256SUMS: good signature" ] && return 0
  return 1
}

# Kontrolne vsote zadnje izdaje: po njih se prenesena kopija posodobi (brez omrežja ostane obstoječa). Le podpisane
# (#8): sicer ni posodobitve (obstoječa ostane) in ne prve namestitve.
sums="" unsigned=""
st=$(mktemp -d) && if curl -fsSL --max-time 8 -o "$st/s" "$base/SHA256SUMS" 2>/dev/null && curl -fsSL --max-time 8 -o "$st/sig" "$base/SHA256SUMS.sig" 2>/dev/null; then
  verify_sums "$st/s" "$st/sig"; case $? in
    0) sums=$(cat "$st/s") ;;
    2) unsigned="cannot check the release signature: install OpenSSH (ssh-keygen)" ;;
    *) unsigned="the release's SHA256SUMS is not signed by Mini Gateway" ;;
  esac
fi
rm -rf "$st"
# #25: brez vračanja na starejšo podpisano izdajo. Različica izdaje: podpisana vrstica »VERSION <v>« v SHA256SUMS;
# podpisana izdaja brez nje je 0.5.17 ali starejša (vrstica je od 0.5.18; oznaka v naslovu ni zanesljiva: GitHub
# preusmeri na CDN). Starejša od nameščenega minigw: ni posodobitve, razen z --allow-downgrade ali MINIGW_ALLOW_DOWNGRADE=1.
rel=""
if [ -n "$sums" ]; then
  rel=$(printf '%s\n' "$sums" | sed -n 's/^VERSION \([0-9][0-9.]*\)$/\1/p' | head -1)
  [ -n "$rel" ] || rel=0.5.17
fi
# Windows/Linux: nameščen minigw (MSI: %LOCALAPPDATA%\Programs\Mini Gateway, minigw serve install: ~/.local/bin), ki ni
# starejši od MIN, je edini minigw (0.5.20), kot brew/aplikacija na macOS: brez prenosa, posodablja ga namestitev.
inst="" iv=""
case $os in
  windows) [ -n "${LOCALAPPDATA:-}" ] && command -v cygpath >/dev/null 2>&1 && inst="$(cygpath -u "$LOCALAPPDATA")/Programs/Mini Gateway/minigw.exe" ;;
  linux) inst="$HOME/.local/bin/minigw" ;;
esac
[ -n "$inst" ] && [ -x "$inst" ] && iv=$(ver "$inst")
case $iv in *[!0-9.]*) iv="" ;; esac
if [ -n "$iv" ] && ! older "$iv" "$MIN"; then :; else inst=""; fi
if [ -z "$inst" ] && [ -n "$sums" ] && [ -x "$EXE" ] && [ ! -L "$EXE" ]; then
  have=$(ver "$EXE"); case $have in *[!0-9.]*) have="" ;; esac
  if older "$rel" "$have" && [ "${1:-}" != --allow-downgrade ] && [ "${MINIGW_ALLOW_DOWNGRADE:-}" != 1 ]; then
    sums="" unsigned="the latest release ($rel) is older than the installed minigw $have (run with MINIGW_ALLOW_DOWNGRADE=1 to go back)"
  fi
fi
want() { printf '%s\n' "$sums" | grep " $1\$" | cut -d' ' -f1; }
stamp="$BIN/.asset.sha256"  # vsota prenesenega paketa (na macOS je to minigw.zip, ne izvršljiva datoteka)

# fetch <asset>: prenese, preveri vsoto (in na macOS podpis) ter namesti v $EXE.
fetch() {
  w=$(want "$1"); [ -n "$w" ] || return 1
  tmp=$(mktemp -d) || return 1
  if curl -fsSL --max-time 25 -o "$tmp/$1" "$base/$1" && [ "$(sha "$tmp/$1")" = "$w" ]; then
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
    [ -n "$c" ] && [ "$c" != "$EXE" ] && valid "$c" && ! older "$(ver "$c")" "$MIN" && { other=$c; break; }  # prestar: prenos
  done
  if [ -n "$other" ]; then
    [ "$(readlink "$EXE" 2>/dev/null)" = "$other" ] || { rm -f "$EXE"; ln -s "$other" "$EXE"; }
  elif [ -L "$EXE" ] || ! valid "$EXE" || { [ -n "$(want minigw.zip)" ] && [ "$(cat "$stamp" 2>/dev/null)" != "$(want minigw.zip)" ]; }; then
    fetch minigw.zip || valid "$EXE" || {
      echo "minigw plugin: could not download or verify minigw from github.com/$REPO${unsigned:+ ($unsigned)}; gateway skills are unavailable."; exit 0; }
  fi
else
  # Windows/Linux: ena izvršljiva datoteka (Go, brez odvisnosti); kontrolna vsota iz SHA256SUMS iste izdaje.
  # Nameščen minigw: Linux povezava; Windows lupini kot minigw plugin install (Git Bash najde bin/minigw brez pripone,
  # ne .cmd; minigw.cmd za cmd/PowerShell), kopija vtičnika (minigw.exe) gre.
  if [ -n "$inst" ] && [ "$os" = linux ]; then
    [ "$(readlink "$EXE" 2>/dev/null)" = "$inst" ] || { rm -f "$EXE"; ln -s "$inst" "$EXE"; } ||
      echo "minigw plugin: could not link $EXE to $inst."
  elif [ -n "$inst" ]; then
    q=$(cygpath -m "$inst" | sed "s/'/'\\\\''/g") c=$(cygpath -w "$inst" | sed 's/%/%%/g')
    { printf '%s\n' '#!/bin/sh' '# Mini Gateway: the installed minigw' "exec '$q' \"\$@\"" > "$BIN/minigw.new" &&
      printf '@"%s" %%*\r\n' "$c" > "$BIN/minigw.cmd" && mv -f "$BIN/minigw.new" "$BIN/minigw"; } ||
      echo "minigw plugin: could not point $BIN/minigw at $inst."
    rm -f "$EXE" 2>/dev/null  # prejšnja kopija; v rabi ostane, a bin/minigw ima prednost
    EXE=$inst
  elif [ ! -x "$EXE" ] || { [ -n "$(want "$asset")" ] && [ "$(sha "$EXE")" != "$(want "$asset")" ]; }; then
    [ "$os" = windows ] && rm -f "$BIN/minigw" "$BIN/minigw.cmd"  # brez namestitve: lupini na odstranjeni minigw gresta
    fetch "$asset" || [ -x "$EXE" ] || {
      echo "minigw plugin: could not download or verify $asset from github.com/$REPO${unsigned:+ ($unsigned)}; gateway skills are unavailable."; exit 0; }
  fi
fi

# Prestar minigw (zadnja izdaja ali nameščen ga še ne more zamenjati): skilli s profili tega ne smejo uporabiti.
cur=$(ver "$EXE")
case $cur in *[!0-9.]*) cur="" ;; esac
older "$cur" "$MIN" && echo "minigw plugin: minigw $cur is older than $MIN, which this plugin needs (profiles, read-only enforcement with a local gateway, the agent-only branch range): run /minigw:gw update."

# L4 (0.5.17): posodobitev zavrnjena (podpis), obstoječa ostane: povej, zakaj.
[ -n "$unsigned" ] && [ -z "$inst" ] && [ -x "$EXE" ] && echo "minigw plugin: not updated: $unsigned; the installed minigw stays."

# Nameščen prehod starejši od izdaje: le namig (posodobitev bi prekinila tekoče zahteve agentov).
old=""; new=$(ver "$EXE")
case "$os" in
  darwin) app="/Applications/Mini Gateway.app"
    [ -d "$app" ] && old=$(plutil -extract CFBundleShortVersionString raw "$app/Contents/Info.plist" 2>/dev/null)
    # minigw vtičnika je tu lahko kar minigw aplikacije: zadnja izdaja iz preusmeritve releases/latest
    new=$(curl -fsS -o /dev/null --max-time 5 -w '%{redirect_url}' "https://github.com/$REPO/releases/latest" 2>/dev/null | sed -n 's|.*/tag/v||p') ;;
  linux) [ -f "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/minigw-serve.service" ] && old=$(ver "$HOME/.local/bin/minigw") ;;
  windows) [ -n "${LOCALAPPDATA:-}" ] && command -v cygpath >/dev/null 2>&1 &&
    old=$(ver "$(cygpath -u "$LOCALAPPDATA")/Programs/Mini Gateway/minigw.exe") ;;
esac
# Le številke in pike: izpis gre Claudu kot kontekst (F6).
case $new in *[!0-9.]*) new="" ;; esac
case $old in *[!0-9.]*) old="" ;; esac
older "$old" "$new" && echo "Mini Gateway $old is installed, $new is available: /minigw:gw update"

# Claudov Bash najde »minigw« tudi brez poti.
line="export PATH=\"\$PATH:$BIN\""  # ena vrstica, tudi ob resume/clear/compact
[ -n "${CLAUDE_ENV_FILE:-}" ] && { grep -qxF "$line" "$CLAUDE_ENV_FILE" 2>/dev/null || echo "$line" >> "$CLAUDE_ENV_FILE"; }
# Povezava: izhod »minigw status« (izhodna koda in vrstici Status/Device privzetega profila); v kontekst le fiksni nizi in
# ID naprave (43 znakov base64url), nikoli prosto besedilo prehoda (F6). Skupaj z omejitvami zgoraj v 60 s kljuke.
ver_line="minigw $(ver "$EXE" | sed -n 's/^\([0-9][0-9.]*\)$/\1/p')"  # le številke in pike (L3)
if sout=$("$EXE" status 2>&1); then
  echo "minigw plugin: $ver_line, connected to Mini Gateway."
else
  blk=$(printf '%s\n' "$sout" | sed '/^$/q')  # blok privzetega profila (prvi; profili so ločeni s prazno vrstico)
  case $(printf '%s\n' "$blk" | sed -n 's/^Status:    //p' | tail -1) in
    "token rejected"*) echo "minigw plugin: $ver_line: the gateway token was rejected; run /minigw:connect <gateway> with a fresh token (Mini Gateway > Settings > General > Copy Token)." ;;
    "device not approved"*)
      id=$(printf '%s\n' "$blk" | sed -n 's/^Device:    \([A-Za-z0-9_-]\{43\}\) (.*/\1/p' | tail -1)
      if [ -n "$id" ]; then
        echo "minigw plugin: $ver_line: device not approved (ID $id); approve it on the gateway (Settings > Devices), then run /minigw:connect again."
      else
        echo "minigw plugin: $ver_line installed but not connected; use /minigw:connect <gateway> <token>."
      fi ;;
    *) echo "minigw plugin: $ver_line installed but not connected; use /minigw:connect <gateway> <token>." ;;
  esac
fi
# #6: agentni skilli zaženejo »claude« prek minigw; razširitev VS Code z vgrajenim CLI ni dovolj.
if ! command -v claude >/dev/null 2>&1; then
  case $os in
    windows) inst='irm https://claude.ai/install.ps1 | iex (in PowerShell)' ;;
    darwin) inst='curl -fsSL https://claude.ai/install.sh | bash (or brew install --cask claude-code)' ;;
    *) inst='curl -fsSL https://claude.ai/install.sh | bash' ;;
  esac
  echo "minigw plugin: the agent skills need the standalone claude command on PATH (the VS Code extension's bundled CLI is not enough): install it with: $inst"
fi
