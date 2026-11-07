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
MIN=0.5.18  # najnižji minigw za ta vtičnik (profili: --name, @<profil>, --minigw-profile)
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
sums="" unsigned="" url=""
st=$(mktemp -d) && if url=$(curl -fsSL --max-time 20 -w '%{url_effective}' -o "$st/s" "$base/SHA256SUMS" 2>/dev/null) && curl -fsSL --max-time 20 -o "$st/sig" "$base/SHA256SUMS.sig" 2>/dev/null; then
  verify_sums "$st/s" "$st/sig"; case $? in
    0) sums=$(cat "$st/s") ;;
    2) unsigned="cannot check the release signature: install OpenSSH (ssh-keygen)" ;;
    *) unsigned="the release's SHA256SUMS is not signed by Mini Gateway" ;;
  esac
fi
rm -rf "$st"
# #25: brez vračanja na starejšo podpisano izdajo. Različica izdaje: podpisana vrstica »VERSION <v>« v SHA256SUMS,
# pri starejših izdajah brez nje oznaka iz končnega naslova (…/download/v<v>/SHA256SUMS). Starejša od nameščenega
# minigw: ni posodobitve (obstoječi ostane), razen z --allow-downgrade ali MINIGW_ALLOW_DOWNGRADE=1.
if [ -n "$sums" ] && [ -x "$EXE" ] && [ ! -L "$EXE" ]; then
  rel=$(printf '%s\n' "$sums" | sed -n 's/^VERSION \([0-9][0-9.]*\)$/\1/p' | head -1)
  [ -n "$rel" ] || rel=$(printf '%s\n' "$url" | sed -n 's#.*/download/v\([0-9][0-9.]*\)/SHA256SUMS$#\1#p')
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
  if [ ! -x "$EXE" ] || { [ -n "$(want "$asset")" ] && [ "$(sha "$EXE")" != "$(want "$asset")" ]; }; then
    fetch "$asset" || [ -x "$EXE" ] || {
      echo "minigw plugin: could not download or verify $asset from github.com/$REPO${unsigned:+ ($unsigned)}; gateway skills are unavailable."; exit 0; }
  fi
fi

# Prestar minigw (zadnja izdaja ali nameščen ga še ne more zamenjati): skilli s profili tega ne smejo uporabiti.
cur=$(ver "$EXE")
case $cur in *[!0-9.]*) cur="" ;; esac
older "$cur" "$MIN" && echo "minigw plugin: minigw $cur is older than $MIN, which this plugin needs for profiles (--name, @<profile>): run /minigw:gw update."

# L4 (0.5.17): posodobitev zavrnjena (podpis), obstoječa ostane: povej, zakaj.
[ -n "$unsigned" ] && [ -x "$EXE" ] && echo "minigw plugin: not updated: $unsigned; the installed minigw stays."

# Nameščen prehod starejši od izdaje: le namig (posodobitev bi prekinila tekoče zahteve agentov).
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
older "$old" "$new" && echo "Mini Gateway $old is installed, $new is available: /minigw:gw update"

# Claudov Bash najde »minigw« tudi brez poti.
[ -n "${CLAUDE_ENV_FILE:-}" ] && echo "export PATH=\"\$PATH:$BIN\"" >> "$CLAUDE_ENV_FILE"
if "$EXE" status >/dev/null 2>&1; then
  echo "minigw plugin: $("$EXE" --version 2>/dev/null | head -1), connected to Mini Gateway."
else
  echo "minigw plugin: $("$EXE" --version 2>/dev/null | head -1) installed but not connected; use /minigw:connect <gateway> <token>."
fi
