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
tidy() { rm -f "$BIN/.sums.new" "$BIN/.sig.new"; }  # preverjene vsote, ki jih ta zagon ni shranil (keepsums), ne ostanejo
unrecord() { rm -f "$BIN/.verified"; tidy; }  # #153: zapis sprejete datoteke za kb-hook.sh; zavrnitev ga zbriše


ver() { "$1" --version 2>/dev/null | head -1 | sed 's/^minigw //; s/[ -].*$//'; }  # »0.7.4-beta (x)« je 0.7.4
# goodver v: strogo 2 ali 3 števila s pikami, do 20 znakov; vse drugo (»v0.7.4«, prazno) velja za prestaro, nikoli za dovolj novo.
goodver() { [ ${#1} -ge 3 ] && [ ${#1} -le 20 ] && printf '%s\n' "$1" | grep -Eq '^[0-9]+(\.[0-9]+){1,2}$'; }
# macOS: podpisan minigw (Developer ID, ekipa in identifikator). Preveri se PREJ, kot se kaj zažene.
valid() {
  [ "$os" = darwin ] && [ -x "$1" ] || return 1
  [ "$1" = "$vcp" ] && return "$vcv"  # ista pot v tem zagonu: izid že znan (forget ga zbriše, ko skripta zamenja $EXE)
  vcp=$1; codesign --verify --strict -R="$REQ" "$1" 2>/dev/null && vcv=0 || vcv=1; return "$vcv"
}
# MIN: dodatna datoteka MIN_CLIENT (bump-version.sh jo piše ob izdaji; JSON se ne razčlenjuje); brez nje ali neveljavna: 0.7.30 in vrstica.
MIN=0.7.30 mc=""
{ read -r mc < "${0%/*}/MIN_CLIENT"; } 2>/dev/null
mc=${mc%"$(printf '\r')"}  # Windows (autocrlf): en končni CR
if goodver "$mc"; then MIN=$mc; else echo "minigw plugin: MIN_CLIENT is missing or invalid; assuming the minimum client $MIN."; fi
# Kako posodobiti nameščeni minigw (ena vrstica).
how() {
  case $os in
    darwin) echo "brew upgrade --cask gh-commiter/mini-gateway/minigw, or run the installer again: docs/install.md" ;;
    windows) echo "run the installer https://github.com/$REPO/releases/latest/download/minigw-windows-$arch.msi (docs/install.md)" ;;
    *) echo "curl -fsSL https://github.com/$REPO/releases/latest/download/install.sh | sh" ;;
  esac
}
stale="" clientonly=""  # stale: overjen, a prestar nameščen minigw (le ob neuspelem prenosu)
# san <besedilo>: brez nadzornih znakov (izpis gre Claudu kot kontekst).
san() { printf '%s' "$1" | tr -cd '[:print:]'; }
# older a b: a < b po številskih poljih (awk; BusyBox nima sort -V)
older() { [ -n "$1" ] && [ -n "$2" ] && awk -v a="$1" -v b="$2" 'BEGIN { n = split(a, x, "."); m = split(b, y, "."); if (m > n) n = m
  for (i = 1; i <= n; i++) { if (x[i] + 0 < y[i] + 0) exit 0; if (x[i] + 0 > y[i] + 0) exit 1 }; exit 1 }'; }

# Povzetek (memo) znotraj ENEGA zagona: podpis para vsot in SHA-256 datoteke se preverita največ enkrat (na Windowsu je vsak
# klic dražji od sekunde). Ključ je pot; vsak zapis skripte v $EXE (prenos, povezava, premik, brisanje) pokliče forget, vsote
# (.sums.new, SHA256SUMS) pa se po zapisu (keepsums) ali kopiji preverjenih vsot nastavijo znova: zamenjana datoteka ne podeduje izida.
vcp="" vcv=0 m1p="" m1v="" m2p="" m2v="" vn="" vm="" SH=""
forget() { vcp="" m1p="" m2p=""; }
# shm <pot>: SH = SHA-256 datoteke (ne v podlupini: shrani se v spremenljivke)
shm() {
  if [ "$1" = "$m1p" ]; then SH=$m1v; elif [ "$1" = "$m2p" ]; then SH=$m2v
  else SH=$(sha "$1"); if [ -z "$m1p" ]; then m1p=$1 m1v=$SH; else m2p=$1 m2v=$SH; fi; fi
}
# verified <vsote> <podpis>: verify_sums enkrat na par vsot v tem zagonu.
verified() {
  case $1 in
    */.sums.new) [ -n "$vn" ] || { verify_sums "$1" "$2" && vn=0 || vn=1; }; return "$vn" ;;
    *) [ -n "$vm" ] || { verify_sums "$1" "$2" && vm=0 || vm=1; }; return "$vm" ;;
  esac
}
# verify_sums <sums> <sig>: najprej ssh-keygen; sicer že nameščen minigw, a le 0.5.17 ali novejši (pozna verify-sums)
# in le z natanko pričakovanim izpisom. Starejši minigw neznan ukaz preda claude, ki lahko vrne 0 (H1 0.5.17).
# 0 = podpisano, 1 = ni podpisano, 2 = ni s čim preveriti. IZJEMA: brez ssh-keygen preverja lastna kopija v bin/ (neoverjena).
verify_sums() {
  if command -v ssh-keygen >/dev/null 2>&1; then
    a=$(mktemp) || return 1
    printf '%s\n' "$SIGNER" > "$a"
    ssh-keygen -Y verify -f "$a" -I minigw-release -n minigw-release -s "$2" < "$1" >/dev/null 2>&1; r=$?
    rm -f "$a"; [ $r -eq 0 ] && return 0 || return 1
  fi
  [ -x "$EXE" ] && [ ! -L "$EXE" ] || return 2
  [ "$os" != darwin ] || valid "$EXE" || return 2
  v=$(ver "$EXE")
  case $v in *[!0-9.]*|"") return 2 ;; esac
  older "$v" 0.5.17 && return 2
  [ "$("$EXE" verify-sums "$1" "$2" 2>/dev/null)" = "SHA256SUMS: good signature" ] && return 0
  return 1
}

# Linux in Windows: sistem nima podpisa datotek, zato velja za overjeno le datoteka, katere SHA-256 je v SHA256SUMS, ki jih je
# vtičnik prenesel in preveril (shranjeni v bin/ ob kopiji) in katerih podpis se znova preveri s pripetim ključem ob vsaki rabi.
# Neoverjene datoteke (nameščen minigw drugje, kopija v bin/ brez vsot) se ne zaženejo, niti za --version: veljajo za odsotne.
# hashof <ime>: SHA-256 vnosa »<64 šestnajstiških> [*]<ime>« iz stdin; ime celo, brez regularnega izraza.
hashof() { awk -v n="$1" 'length($1) == 64 && ($2 == n || $2 == "*" n) { print $1; exit }'; }
# trustedby <vsote> <podpis> <datoteka>: tsums = vsote, ki so datoteko overile.
tsums=""
trustedby() {
  [ -f "$1" ] && [ -f "$2" ] && command -v ssh-keygen >/dev/null 2>&1 && verified "$1" "$2" &&
    { shm "$3"; h=$SH; [ -n "$h" ]; } && [ "$(hashof "$asset" < "$1")" = "$h" ] && tsums=$1
}
# Shranjene vsote ali vsote, preverjene v tem zagonu (.sums.new): nameščen minigw, ki je ta izdaja, je overjen že v prvi seji.
trusted() { trustedby "$BIN/SHA256SUMS" "$BIN/SHA256SUMS.sig" "$1" || trustedby "$BIN/.sums.new" "$BIN/.sig.new" "$1"; }
# exever <datoteka>: različica overjene datoteke je v podpisani vrstici VERSION vsot, ki so jo overile (zaženemo --version le brez nje).
exever() {
  xv=""
  [ "$os" != darwin ] && trusted "$1" && xv=$(sed -n 's/^VERSION \([0-9][0-9.]*\)$/\1/p' "$tsums" | head -1)
  [ -n "$xv" ] || xv=$(ver "$1")
  printf '%s\n' "$xv"
}
# authexe: ali sme vtičnik zagnati $EXE. macOS: podpis Developer ID. Drugje: trusted; IZJEMA (dokumentirana): brez ssh-keygen se
# nič ne da preveriti, zato ostane obstoječa lastna kopija v bin/ (nikoli nameščen minigw drugje), da sistem ne izgubi odjemalca.
authexe() {
  if [ "$os" = darwin ]; then valid "$EXE"; else trusted "$EXE" || { ! command -v ssh-keygen >/dev/null 2>&1 && [ -x "$EXE" ] && [ ! -L "$EXE" ]; }; fi
}
# keepsums: po uspešnem prenosu (ali ko je kopija v bin/ enaka vnosu) shrani preverjene vsote ob kopijo.
keepsums() { [ -f "$BIN/.sums.new" ] && [ -f "$BIN/.sig.new" ] && mv -f "$BIN/.sums.new" "$BIN/SHA256SUMS" && mv -f "$BIN/.sig.new" "$BIN/SHA256SUMS.sig"; vn="" vm=""; return 0; }

# Kontrolne vsote zadnje izdaje: po njih se prenesena kopija posodobi (brez omrežja ostane obstoječa). Le podpisane
# (#8): sicer ni posodobitve (obstoječa ostane) in ne prve namestitve.
sums="" unsigned=""
st=$(mktemp -d) && if curl -fsSL --max-time 8 -o "$st/s" "$base/SHA256SUMS" 2>/dev/null && curl -fsSL --max-time 8 -o "$st/sig" "$base/SHA256SUMS.sig" 2>/dev/null; then
  verify_sums "$st/s" "$st/sig"; case $? in
    0) sums=$(cat "$st/s"); vn=""
      # kopija v bin/: pozitiven izid le, če sta OBE kopiji uspeli in sta enaki preverjenima; sicer brez obeh datotek (kot brez svežih vsot)
      if cp "$st/s" "$BIN/.sums.new" && cp "$st/sig" "$BIN/.sig.new" && cmp -s "$st/s" "$BIN/.sums.new" && cmp -s "$st/sig" "$BIN/.sig.new"; then vn=0
      else rm -f "$BIN/.sums.new" "$BIN/.sig.new"; fi ;;
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
# #48: Linux ~/.local/bin/minigw (in njegova mapa) le, če je last uporabnika in ni zapisljiv za skupino ali druge (pred
# zagonom); mape nad njo do / last uporabnika ali roota, zapisljive za skupino ali druge le s sticky (/tmp), kot safePath.
mine() { [ ! -L "$1" ] && m=$(stat -c '%u %a' "$1" 2>/dev/null) && [ "${m%% *}" = "$(id -u)" ] && [ $((0${m#* } & 022)) -eq 0 ]; }
above() {
  d=$1
  case $d in /*) ;; *) return 1 ;; esac
  while [ "$d" != / ]; do
    d=${d%/*}; d=${d:-/}
    m=$(stat -L -c '%u %a' "$d" 2>/dev/null) || return 1
    [ "${m%% *}" = 0 ] || [ "${m%% *}" = "$(id -u)" ] || return 1
    [ $((0${m#* } & 022)) -eq 0 ] || [ $((0${m#* } & 01000)) -ne 0 ] || return 1
  done
}
if [ "$os" = linux ] && [ -e "$inst" ] && ! { mine "$inst" && mine "${inst%/*}" && above "${inst%/*}"; }; then
  echo "minigw plugin: not using $(san "$inst"): it or a folder above it is not yours or is writable by others."; inst=""
fi
isums=""; instx=""; [ -n "$inst" ] && [ -x "$inst" ] && instx=1
instok=""; [ -n "$instx" ] && trusted "$inst" && { instok=1; isums=$tsums; iv=$(exever "$inst"); }  # neoverjen: odsoten, ne zažene se
if [ -n "$instx" ] && [ -z "$instok" ]; then  # ne zaženemo ga, a posodobitev ponudimo (prej je to povedala vrstica »X is installed«)
  u=$(how)
  [ "$os" = linux ] && [ -f "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/minigw-serve.service" ] && u="/minigw:gw update"
  [ "$os" = windows ] && [ -n "${APPDATA:-}" ] && [ -f "$(cygpath -u "$APPDATA")/minigw/serve.json" ] && u="/minigw:gw update"
  echo "minigw plugin: the minigw installed at $(san "$inst") is not the current release${rel:+ $rel} (or could not be verified); update it: $u"
fi
case $iv in *[!0-9.]*) iv="" ;; esac
if [ -n "$instok" ] && goodver "$iv" && ! older "$iv" "$MIN"; then :; else [ -n "$instok" ] && stale=$inst; inst=""; fi
if [ -z "$inst" ] && [ -n "$sums" ] && [ -x "$EXE" ] && [ ! -L "$EXE" ] && authexe; then
  have=$(exever "$EXE"); case $have in *[!0-9.]*) have="" ;; esac
  if older "$rel" "$have" && [ "${1:-}" != --allow-downgrade ] && [ "${MINIGW_ALLOW_DOWNGRADE:-}" != 1 ]; then
    sums="" unsigned="the latest release ($rel) is older than the installed minigw $have (run with MINIGW_ALLOW_DOWNGRADE=1 to go back)"
  fi
fi
want() { printf '%s\n' "$sums" | hashof "$1"; }
stamp="$BIN/.asset.sha256"  # vsota prenesenega paketa (na macOS je to minigw.zip, ne izvršljiva datoteka)

# fetch <asset>: prenese, preveri vsoto (in na macOS podpis) ter namesti v $EXE.
fetch() {
  w=$(want "$1"); [ -n "$w" ] || return 1
  tmp=$(mktemp -d) || return 1
  if curl -fsSL --max-time 25 -o "$tmp/$1" "$base/$1" && [ "$(sha "$tmp/$1")" = "$w" ]; then
    if [ "$os" = darwin ]; then
      ditto -x -k "$tmp/$1" "$tmp/x" && valid "$tmp/x/minigw" && rm -f "$EXE" && mv "$tmp/x/minigw" "$EXE"; fs=$?; forget; [ $fs -eq 0 ]
    else
      chmod +x "$tmp/$1" && mv -f "$tmp/$1" "$EXE"; fs=$?; forget; [ $fs -eq 0 ]
    fi && echo "$w" > "$stamp" && keepsums && { rm -rf "$tmp"; return 0; }
  fi
  rm -rf "$tmp"; return 1
}

if [ "$os" = darwin ]; then
  # macOS: podpisan minigw (Developer ID, ekipa in identifikator) iz minigw.zip ali že nameščen (brew).
  # Nameščen minigw (brew, aplikacija) ima vedno prednost pred preneseno kopijo: posodablja ga brew.
  other=""
  for c in "$(command -v minigw 2>/dev/null || true)" /opt/homebrew/bin/minigw /usr/local/bin/minigw \
           "/Applications/Mini Gateway.app/Contents/Helpers/minigw"; do
    [ -n "$c" ] && [ "$c" != "$EXE" ] && valid "$c" || continue
    cv=$(ver "$c")
    goodver "$cv" && ! older "$cv" "$MIN" && { other=$c; stale=""; break; }  # neznana ali prestara različica: prenos
    [ -n "$stale" ] || stale=$c  # overjen, a prestar: le če prenos ne uspe
  done
  if [ -n "$other" ]; then
    [ "$(readlink "$EXE" 2>/dev/null)" = "$other" ] || { rm -f "$EXE"; forget; ln -s "$other" "$EXE"; }
  elif [ -L "$EXE" ] || ! valid "$EXE" || { [ -n "$(want minigw.zip)" ] && [ "$(cat "$stamp" 2>/dev/null)" != "$(want minigw.zip)" ]; }; then
    fetch minigw.zip || valid "$EXE" || {
      if [ -n "$stale" ]; then rm -f "$EXE"; forget; ln -s "$stale" "$EXE"
      else
        { [ -e "$EXE" ] || [ -L "$EXE" ]; } && { mv -f "$EXE" "$BIN/minigw.rejected"; forget; }  # nepodpisana kopija: ne sme ostati, kjer jo zaženejo skilli
        echo "minigw plugin: could not download or verify minigw from github.com/$REPO${unsigned:+ ($unsigned)}; gateway skills are unavailable."; unrecord; exit 0; fi; }
  fi
else
  # Windows/Linux: ena izvršljiva datoteka (Go, brez odvisnosti); kontrolna vsota iz SHA256SUMS iste izdaje.
  # Nameščen minigw: Linux povezava; Windows lupini kot minigw plugin install (Git Bash najde bin/minigw brez pripone,
  # ne .cmd; minigw.cmd za cmd/PowerShell), kopija vtičnika (minigw.exe) gre.
  # Git Bash (MSYS) za obstoječo minigw.exe pozna »minigw« kot isto datoteko: mv/rm/> na golem imenu zadene ali ZAMENJA minigw.exe.
  # BRANJE je neškodljivo, če vsebina odloči (lupina je majhna in ima besedilo; 19 MB exe ne): isshim, oldshims. Edini zapis na golo ime
  # (prvi prehod, lupina še ni prava) teče v aside: minigw.exe se odmakne (.keep), ukaz teče, vrne se VEDNO. Ključavnica mkdir (atomarna)
  # varuje prehod pred drugo sejo; zasedena = ta zagon lupine ne zapiše (izhod 3, brez izpisa); starejša od 5 min (find -mmin) je zastarela.
  aside() {
    l="$BIN/.aside.lock"; k="$BIN/minigw.exe.keep"; had=""
    mkdir "$l" 2>/dev/null || { [ -n "$(find "$l" -maxdepth 0 -mmin +5 2>/dev/null)" ] && rmdir "$l" 2>/dev/null && mkdir "$l" 2>/dev/null; } || return 3
    [ -e "$k" ] && [ ! -e "$BIN/minigw.exe" ] && mv "$k" "$BIN/minigw.exe"  # ostanek prekinjenega zagona (le pod ključavnico)
    if [ -f "$BIN/minigw.exe" ]; then mv -f "$BIN/minigw.exe" "$k" || { rmdir "$l"; return 1; }; had=1; fi  # tekoče exe Windows ne pusti premakniti: lupina se ne zapiše
    "$@"; r=$?
    [ -z "$had" ] || mv -f "$k" "$BIN/minigw.exe" || r=1
    rmdir "$l"; return $r
  }
  # isshim <pot>: vsebina je natanko lupina za $inst (cmp na kratki datoteki; exe se razlikuje v prvih bajtih)
  isshim() { shimtext > "$BIN/minigw.chk" 2>/dev/null; cmp -s "$BIN/minigw.chk" "$1"; ir=$?; rm -f "$BIN/minigw.chk"; return $ir; }
  # oldshims: lupini iz prejšnje seje (majhni besedilni datoteki tega skripta); exe prepozna po velikosti in besedilu
  oldshims() {
    for f in "$BIN/minigw" "$BIN/minigw.cmd"; do
      [ -f "$f" ] && [ ! -L "$f" ] && [ "$(wc -c < "$f")" -lt 1024 ] && grep -q 'Mini Gateway: the installed minigw\|DisableDelayedExpansion' "$f" && rm -f "$f"
    done
  }
  # shimtext: besedilo lupine bin/minigw za $inst (en vir: zapis ga napiše, zapis .verified ga primerja)
  shimtext() { q=$(cygpath -m "$inst" | sed "s/'/'\\\\''/g"); printf '%s\n' '#!/bin/sh' '# Mini Gateway: the installed minigw' "exec '$q' \"\$@\""; }
  linkinst() {  # $inst: nameščen minigw (overjen, ali edini, ki ga je mogoče uporabiti)
    if [ "$os" = linux ]; then
      [ "$(readlink "$EXE" 2>/dev/null)" = "$inst" ] || { rm -f "$EXE"; forget; ln -s "$inst" "$EXE"; } ||
        echo "minigw plugin: could not link $(san "$EXE") to $(san "$inst")."
    else
      # minigw.cmd: brez zakasnjene razširitve, pot MSI kot %LOCALAPPDATA%\… (ASCII; enako kot shimCmd v client/plugin.go)
      { shimtext > "$BIN/minigw.new" &&
        printf '@setlocal DisableDelayedExpansion\r\n@"%%LOCALAPPDATA%%\\Programs\\Mini Gateway\\minigw.exe" %%*\r\n' > "$BIN/minigw.cmd" &&
        { isshim "$BIN/minigw" || { aside mv -f "$BIN/minigw.new" "$BIN/minigw"; ar=$?; [ $ar = 0 ] || [ $ar = 3 ]; }; }; } ||
        echo "minigw plugin: could not point $(san "$BIN")/minigw at $(san "$inst")."
      rm -f "$BIN/minigw.new"
      # kopija vtičnika (minigw.exe) ostane: pot vanjo je lahko v nastavitvi »kb mcp --print-config«; bin/minigw ima prednost
      EXE=$inst
    fi
    return 0
  }
  # Windows: lupini iz prejšnje seje (majhni besedilni datoteki tega skripta) ne smeta preživeti nameščenega minigw, ki ni več overjen.
  if [ "$os" = windows ] && [ -z "$inst" ]; then
    oldshims
  fi
  if [ -n "$inst" ]; then
    linkinst
    [ "$isums" = "$BIN/.sums.new" ] && keepsums  # overjen z vsotami tega zagona: shrani jih za brez omrežja
  elif [ -L "$EXE" ] || [ ! -x "$EXE" ] || ! authexe || { [ -n "$(want "$asset")" ] && { shm "$EXE"; [ "$SH" != "$(want "$asset")" ]; }; }; then
    [ "$os" = windows ] && oldshims  # brez namestitve: lupini na odstranjeni minigw gresta
    if [ -n "$sums" ] && [ ! -L "$EXE" ] && [ -x "$EXE" ] && { shm "$EXE"; [ "$SH" = "$(want "$asset")" ]; }; then
      keepsums  # ista datoteka kot v podpisanih vsotah: overjena brez prenosa
    else
      fetch "$asset" || { authexe && [ -x "$EXE" ]; } || { [ -n "$stale" ] && { inst=$stale; linkinst; }; } || {
        if [ -n "$instx" ] || [ -e "$EXE" ] || [ -L "$EXE" ]; then
          rm -f "$EXE"; forget; unrecord  # zapis v kopijo vedno sledi forget; tu sledi le izhod brez nove odločitve o zaupanju
          if [ -n "$unsigned" ]; then why="$unsigned; gateway skills are unavailable until the release can be verified"
          elif [ -n "$sums" ]; then why="the download failed or did not match the signed checksum; gateway skills are unavailable until it works"
          else why="github.com/$REPO could not be reached; gateway skills are unavailable until the network is back"; fi
          echo "minigw plugin: cannot verify the installed minigw and cannot download a verified one: $why."
        else
          echo "minigw plugin: could not download or verify $asset from github.com/$REPO${unsigned:+ ($unsigned)}; gateway skills are unavailable."
        fi; unrecord; exit 0; }
    fi
  fi
fi

# #153: zapis bin/.verified (za kb-hook.sh) le, če je preverjanje v tem zagonu res uspelo: macOS podpis Developer ID; Linux/Windows SHA-256
# v podpisanih vsotah (trusted). Izjema brez ssh-keygen (neoverjena lastna kopija) zapisa NIMA. SHA-256 (po povezavi) gre atomarno, 600.
# Windows z nameščenim minigw: kljuka zažene lupino bin/minigw, ta pa nameščeni minigw: zapis ima obe vrstici (»sha  pot«).
ent=$EXE; [ "$os" = windows ] && [ -n "$inst" ] && ent="$BIN/minigw"
if [ "$os" = darwin ]; then valid "$EXE"; else trusted "$EXE"; fi &&
   { if [ "$ent" = "$EXE" ]; then shm "$ent"; else  # lupina mora biti ta, ki jo je zapisal ta zagon (primerjava vsebine)
       [ -f "$ent" ] && [ ! -L "$ent" ] && isshim "$ent" && shm "$ent"; fi; } && h1=$SH && h2=$h1 &&
   { [ "$ent" = "$EXE" ] || { shm "$EXE"; h2=$SH; }; } &&
   [ -n "$h1" ] && [ -n "$h2" ] && ( umask 077; { printf '%s  %s\n' "$h1" "$ent"; [ "$ent" = "$EXE" ] || printf '%s  %s\n' "$h2" "$EXE"; } > "$BIN/.verified.new" ) &&
   mv -f "$BIN/.verified.new" "$BIN/.verified" || { unrecord; rm -f "$BIN/.verified.new"; }

# Izbran minigw (že overjen na macOS) je lahko prestar (brez prenosa, zadnja izdaja starejša, ali ga namestitev še ni posodobila):
# nikoli tiho, ena vrstica iz dejanskega izida. Različica le v strogi obliki.
[ "$os" != darwin ] && ! command -v ssh-keygen >/dev/null 2>&1 && [ -x "$EXE" ] &&
  echo "minigw plugin: ssh-keygen is not installed, so the plugin's minigw could not be verified against the release signature; install OpenSSH to enable the check"
cur=$(exever "$EXE")
if ! goodver "$cur"; then
  echo "minigw plugin: using the installed minigw, whose version could not be read; this plugin's commands need $MIN or newer and may not work until it is updated ($(how))"
elif older "$cur" "$MIN"; then
  echo "minigw plugin: using the installed minigw $cur, older than $MIN which this plugin's commands need; some commands will not work until it is updated ($(how))"
elif [ -n "$stale" ]; then
  echo "minigw plugin: using its own minigw $cur; the installed minigw is older than $MIN or its version could not be read: update it ($(how))"
fi

# L4 (0.5.17): posodobitev zavrnjena (podpis), obstoječa ostane: povej, zakaj.
[ -n "$unsigned" ] && [ -z "$inst" ] && [ -x "$EXE" ] && echo "minigw plugin: not updated: $unsigned; the installed minigw stays."

# Nameščen prehod starejši od izdaje: le namig (posodobitev bi prekinila tekoče zahteve agentov).
old=""; new=$(exever "$EXE")
case "$os" in
  darwin) app="/Applications/Mini Gateway.app"
    [ -d "$app" ] && old=$(plutil -extract CFBundleShortVersionString raw "$app/Contents/Info.plist" 2>/dev/null)
    # brez aplikacije: nameščeni odjemalec, na katerega kaže vtičnik (brew)
    [ -z "$old" ] && [ -n "$other" ] && { old=$(ver "$other"); clientonly=1; }
    # minigw vtičnika je tu lahko kar minigw aplikacije: zadnja izdaja iz preusmeritve releases/latest
    new=$(curl -fsS -o /dev/null --max-time 5 -w '%{redirect_url}' "https://github.com/$REPO/releases/latest" 2>/dev/null | sed -n 's|.*/tag/v||p') ;;
  # M1: nameščeni minigw (ne $EXE, ki je povezava nanj) proti podpisani izdaji, tudi brez enote systemd
  *) old=$iv new=${rel:-$new} ;;
esac
# Le številke in pike: izpis gre Claudu kot kontekst (F6).
case $new in *[!0-9.]*) new="" ;; esac
case $old in *[!0-9.]*) old="" ;; esac
# Linux brez enote minigw serve (le odjemalec): /minigw:gw update ga ne posodobi (serve install potrebuje prehod).
# Windows brez serve.json (MSI le z odjemalcem): enako, namig na MSI zadnje izdaje (nadgradnja obdrži izbrane dele).
wcfg=""
[ "$os" = windows ] && [ -n "${APPDATA:-}" ] && wcfg="$(cygpath -u "$APPDATA")/minigw/serve.json"
if [ "$os" = linux ] && [ ! -f "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/minigw-serve.service" ]; then
  older "$old" "$new" && echo "minigw $old is installed, $new is available: run curl -fsSL https://github.com/$REPO/releases/latest/download/install.sh | sh (or replace $HOME/.local/bin/minigw with $asset from the latest release; check it first: docs/install.md, Check a download)"
elif [ "$os" = windows ] && [ ! -f "$wcfg" ]; then
  older "$old" "$new" && echo "minigw $old is installed, $new is available: run the installer https://github.com/$REPO/releases/latest/download/minigw-windows-$arch.msi (check it first: docs/install.md, Check a download)"
elif [ -n "$clientonly" ]; then
  older "$old" "$new" && echo "minigw $old is installed, $new is available: brew upgrade --cask gh-commiter/mini-gateway/minigw, or run the installer again (docs/install.md)"
else
  older "$old" "$new" && echo "Mini Gateway $old is installed, $new is available: /minigw:gw update"
fi

# Claudov Bash najde »minigw« tudi brez poti.
qb=$(printf '%s' "$BIN" | sed "s/'/'\\\\''/g")
line="export PATH='$qb':\"\$PATH\""  # ena vrstica, tudi ob resume/clear/compact
[ -n "${CLAUDE_ENV_FILE:-}" ] && { grep -qxF "$line" "$CLAUDE_ENV_FILE" 2>/dev/null || echo "$line" >> "$CLAUDE_ENV_FILE"; }
# Povezava: izhod »minigw status« (izhodna koda in vrstici Status/Device privzetega profila); v kontekst le fiksni nizi in
# ID naprave (43 znakov base64url), nikoli prosto besedilo prehoda (F6). Skupaj z omejitvami zgoraj v 60 s kljuke.
ver_line="minigw $(exever "$EXE" | sed -n 's/^\([0-9][0-9.]*\)$/\1/p')"  # le številke in pike (L3)
if sout=$("$EXE" status 2>&1); then
  echo "minigw plugin: $ver_line, connected to Mini Gateway."
else
  blk=$(printf '%s\n' "$sout" | sed '/^$/q')  # blok privzetega profila (prvi; profili so ločeni s prazno vrstico)
  case $(printf '%s\n' "$blk" | sed -n 's/^Status:    //p' | tail -1) in
    "token rejected"*) echo "minigw plugin: $ver_line: the gateway token was rejected; copy a fresh token (Mini Gateway > Settings > General > Copy Token) and run minigw reconnect [--name P]." ;;
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
tidy
