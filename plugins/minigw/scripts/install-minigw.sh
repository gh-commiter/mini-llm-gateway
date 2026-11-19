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
sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" 2>/dev/null | cut -d' ' -f1; else shasum -a 256 "$1" 2>/dev/null | cut -d' ' -f1; fi; }
case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux; asset="minigw-linux-$arch" ;;
  MINGW*|MSYS*|CYGWIN*) os=windows; asset="minigw-windows-$arch.exe"; EXE="$BIN/minigw.exe" ;;  # Git Bash: »minigw« najde minigw.exe
  *) echo "minigw plugin: no minigw client for $(uname -s)."; exit 0 ;;
esac
mkdir -p "$BIN"
# Windows, uporabniki 0.8.1-0.8.9: tedanja skripta je pisala lupino bin/minigw in zato odmikala minigw.exe (.keep, .aside.lock). Zdaj ne več;
# ostanek se le pospravi: ustavljen .keep brez minigw.exe gre nazaj (en mv, cilj ne obstaja), ključavnica gre. Nič drugega.
if [ "$os" = windows ]; then
  [ -e "$BIN/minigw.exe.keep" ] && [ ! -e "$BIN/minigw.exe" ] && mv "$BIN/minigw.exe.keep" "$BIN/minigw.exe"
  [ -d "$BIN/.aside.lock" ] && { rm -f "$BIN/.aside.lock/pid"; rmdir "$BIN/.aside.lock" 2>/dev/null; }
fi
# Vsaka začasna datoteka je za ta zagon (.$$): dve seji hkrati si je ne delita. tidy zbriše le datoteke tega zagona (vsote, ki jih keepsums ni
# shranil, ne ostanejo) in ostanke mrtvih zagonov, starejše od 10 minut (tudi imena brez pripone iz starejših izdaj).
SN="$BIN/.sums.new.$$" SG="$BIN/.sig.new.$$" VN="$BIN/.verified.new.$$"
tidy() {
  rm -f "$SN" "$SG" "$VN" "$BIN/minigw.new.$$" "$BIN/minigw.exe.new.$$" "$BIN/SHA256SUMS.prev.new.$$" "$BIN/SHA256SUMS.prev.sig.new.$$" "$BIN/minigw.chk.$$" "$BIN/minigw.cmd.new.$$"
  find "$BIN" -maxdepth 1 \( -name '*.new.*' -o -name '*.chk.*' -o -name .sums.new -o -name .sig.new -o -name .verified.new -o -name minigw.new -o -name minigw.chk \) -mmin +10 -exec rm -f {} \; 2>/dev/null
  return 0
}
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
vcp="" vcv=0 m1p="" m1v="" m2p="" m2v="" vn="" vm="" vp="" SH=""
forget() { vcp="" m1p="" m2p=""; }
# shm <pot>: SH = SHA-256 datoteke (ne v podlupini: shrani se v spremenljivke)
shm() {
  if [ "$1" = "$m1p" ]; then SH=$m1v; elif [ "$1" = "$m2p" ]; then SH=$m2v
  else SH=$(sha "$1"); if [ -z "$m1p" ]; then m1p=$1 m1v=$SH; else m2p=$1 m2v=$SH; fi; fi
}
# verified <vsote> <podpis>: verify_sums enkrat na par vsot v tem zagonu.
verified() {
  case $1 in
    "$SN") [ -n "$vn" ] || { verify_sums "$1" "$2" && vn=0 || vn=1; }; return "$vn" ;;
    "$BIN/SHA256SUMS.prev") [ -n "$vp" ] || { verify_sums "$1" "$2" && vp=0 || vp=1; }; return "$vp" ;;
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
# Tri vira: shranjene vsote, PREJŠNJA generacija (SHA256SUMS.prev, glej keepsums) in vsote tega zagona (.sums.new).
trusted() { trustedby "$BIN/SHA256SUMS" "$BIN/SHA256SUMS.sig" "$1" || trustedby "$BIN/SHA256SUMS.prev" "$BIN/SHA256SUMS.prev.sig" "$1" || trustedby "$SN" "$SG" "$1"; }
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
# testexit <korak>: SAMO za preizkus (scripts/plugin-install-docker-test.sh): MINIGW_TEST_EXIT_AT=<korak> konča zagon (kot ubit) na tem koraku.
testexit() { [ "${MINIGW_TEST_EXIT_AT:-}" = "$1" ] && exit 9; return 0; }
# keepsums: po uspešnem prenosu (ali ko je kopija v bin/ enaka vnosu) shrani preverjene vsote ob kopijo. VRSTNI RED (zamenjava
# odjemalca, brez ključavnice; ubit zagon na katerem koli koraku mora pustiti overljiv odjemalec):
#   1. prenos v bin/minigw[.exe].new.<pid>, preverjen (hash v podpisanih vsotah)               (testexit dl)
#   2. veljavni par vsot (SHA256SUMS + .sig), če se preveri, se kopira v SHA256SUMS.prev[.sig]    (testexit prev)
#   3. nov par vsot: SHA256SUMS, nato SHA256SUMS.sig                                             (testexit sums, sig)
#   4. ENA zamenjava mv -f nov.tmp -> minigw[.exe]                                               (testexit exe)
# Stari odjemalec je do 4. koraka overljiv s PREJŠNJO generacijo (.prev), novi po njem s tekočo; neoverjen je le .new.<pid>, ki ga ne zažene nihče.
keepsums() {
  if [ -f "$SN" ] && [ -f "$SG" ]; then
    # Samo par, ki zdaj jamči za exe na mestu, postane PREJŠNJA generacija; če zanj jamči že .prev (ali nihče), se .prev ne dotakne
    # (sicer bi dva zaporedna neuspela prenosa pustila oba para novo izdajo in exe stare: nič ga ne bi overilo).
    kr=1; [ -f "$EXE" ] && { kts=$tsums; trustedby "$BIN/SHA256SUMS" "$BIN/SHA256SUMS.sig" "$EXE"; kr=$?; tsums=$kts; }
    if [ $kr -eq 0 ]; then
      cp "$BIN/SHA256SUMS" "$BIN/SHA256SUMS.prev.new.$$" && cp "$BIN/SHA256SUMS.sig" "$BIN/SHA256SUMS.prev.sig.new.$$" &&
        mv -f "$BIN/SHA256SUMS.prev.new.$$" "$BIN/SHA256SUMS.prev" && mv -f "$BIN/SHA256SUMS.prev.sig.new.$$" "$BIN/SHA256SUMS.prev.sig"
      vp=""
    fi
    testexit prev
    mv -f "$SN" "$BIN/SHA256SUMS"; testexit sums
    mv -f "$SG" "$BIN/SHA256SUMS.sig"; testexit sig
  fi
  vn="" vm="" vp=""; return 0
}

# Kontrolne vsote zadnje izdaje: po njih se prenesena kopija posodobi (brez omrežja ostane obstoječa). Le podpisane
# (#8): sicer ni posodobitve (obstoječa ostane) in ne prve namestitve.
sums="" unsigned=""
st=$(mktemp -d) && if curl -fsSL --max-time 8 -o "$st/s" "$base/SHA256SUMS" 2>/dev/null && curl -fsSL --max-time 8 -o "$st/sig" "$base/SHA256SUMS.sig" 2>/dev/null; then
  verify_sums "$st/s" "$st/sig"; case $? in
    0) sums=$(cat "$st/s"); vn=""
      # kopija v bin/: pozitiven izid le, če sta OBE kopiji uspeli in sta enaki preverjenima; sicer brez obeh datotek (kot brez svežih vsot)
      if cp "$st/s" "$SN" && cp "$st/sig" "$SG" && cmp -s "$st/s" "$SN" && cmp -s "$st/sig" "$SG"; then vn=0
      else rm -f "$SN" "$SG"; fi ;;
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
# Windows: skripta NIKOLI ne ustvari, prepiše ali preimenuje bin/minigw in bin/minigw.cmd (Git Bash šteje »minigw« in minigw.exe za isto
# datoteko: zato je bila prej potrebna ključavnica). Nameščeni minigw velja le prek ZAGONSKE datoteke, ki jo napiše minigw plugin install
# (client/plugin.go shimSh): prava datoteka bin/minigw (ugotovljena iz IMENIKA, nikoli s test/stat na golem imenu), z natanko tistim
# besedilom in s kazalcem na minigw.exe, ki ga overijo podpisane vsote. Sicer velja lastna kopija bin/minigw.exe.
same() { "$1" > "$BIN/minigw.chk.$$" 2>/dev/null; cmp -s "$BIN/minigw.chk.$$" "$2"; ir=$?; rm -f "$BIN/minigw.chk.$$"; return $ir; }
shimtext() { q=$(cygpath -w "$inst" | tr '\\' / | sed "s/'/'\\\\''/g"); printf '%s\n' '#!/bin/sh' '# Mini Gateway: the installed minigw' "exec '$q' \"\$@\""; }
launcher="" usedlauncher="" viamsi=""  # Windows: ime minigw je v imeniku (prava datoteka); zagonska datoteka je veljala; nameščeni MSI brez nje
winlauncher() {
  for n in $(ls -A "$BIN" 2>/dev/null); do [ "$n" = minigw ] && launcher=1; done
  [ -n "$launcher" ] || return 0
  wp=$(sed -n "3s/^exec '\(.*\)' \"\$@\"\$/\1/p" "$BIN/minigw" 2>/dev/null)  # prava datoteka obstaja: ime je ta datoteka
  [ -n "$wp" ] || return 0
  wp=$(printf '%s\n' "$wp" | sed "s/'\\\\''/'/g")  # '\'' -> ' (kot ga napiše Go odjemalec); celotno besedilo se spodaj primerja natanko
  inst=$(cygpath -u "$wp" 2>/dev/null) || { inst=""; return 0; }
  same shimtext "$BIN/minigw" && usedlauncher=1 || inst=""  # natanko besedilo zagonske datoteke Go odjemalca
}
# dropshims: zagonska datoteka, ki ne velja, bi zasenčila lastno kopijo: odstrani se (majhna datoteka z znamenjem; prava datoteka obstaja,
# zato rm na tem imenu zadene zagonsko datoteko, ne minigw.exe) skupaj z minigw.cmd. Le če seznam imenika pokaže ime minigw.
dropshims() {
  [ -n "$launcher" ] || return 0
  f="$BIN/minigw"
  [ -f "$f" ] && [ ! -L "$f" ] && [ "$(wc -c < "$f")" -lt 1024 ] && grep -q 'Mini Gateway: the installed minigw' "$f" && { rm -f "$f"; forget; }
  g="$BIN/minigw.cmd"
  [ -f "$g" ] && [ "$(wc -c < "$g")" -lt 1024 ] && grep -q DisableDelayedExpansion "$g" && rm -f "$g"
  launcher=""; return 0
}
inst="" iv=""
case $os in
  windows)
    command -v cygpath >/dev/null 2>&1 && winlauncher
    # brez veljavne zagonske datoteke: nameščeni MSI minigw, če ga podpisane vsote overijo, se KOPIRA kot lastna kopija (copyinst); nič na golem imenu
    [ -z "$inst" ] && [ -n "${LOCALAPPDATA:-}" ] && command -v cygpath >/dev/null 2>&1 && { inst="$(cygpath -u "$LOCALAPPDATA")/Programs/Mini Gateway/minigw.exe"; viamsi=1; } ;;
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

# fetch <asset>: prenese, preveri vsoto (in na macOS podpis) ter namesti v $EXE. Linux/Windows: vrstni red je zapisan pri keepsums
# (prenos pod začasnim imenom v isti mapi, objava vsot, ENA zamenjava mv); mv, ki ne uspe (exe teče), pusti stari overjeni exe.
fetch() {
  w=$(want "$1"); [ -n "$w" ] || return 1
  if [ "$os" = darwin ]; then
    tmp=$(mktemp -d) || return 1
    if curl -fsSL --max-time 25 -o "$tmp/$1" "$base/$1" && [ "$(sha "$tmp/$1")" = "$w" ]; then
      ditto -x -k "$tmp/$1" "$tmp/x" && valid "$tmp/x/minigw" && rm -f "$EXE" && mv "$tmp/x/minigw" "$EXE"; fs=$?; forget
      [ $fs -eq 0 ] && echo "$w" > "$stamp" && keepsums && { rm -rf "$tmp"; return 0; }
    fi
    rm -rf "$tmp"; return 1
  fi
  ft="$BIN/${EXE##*/}.new.$$"
  if curl -fsSL --max-time 25 -o "$ft" "$base/$1" && [ "$(sha "$ft")" = "$w" ] && chmod +x "$ft"; then
    testexit dl
    keepsums
    mv -f "$ft" "$EXE"; fs=$?; forget
    if [ $fs -ne 0 ]; then
      rm -f "$ft"; echo "minigw plugin: could not replace $(san "$EXE") (in use?); the verified copy stays and the update is tried again in the next session."; return 1
    fi
    testexit exe
    echo "$w" > "$stamp"; return 0
  fi
  rm -f "$ft"; return 1
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
  # Datoteke, ki je ni mogoče PREBRATI (odprta izključno, »Device or resource busy«), ne moremo preveriti: to NI »neoverjena«. V tem zagonu se je
  # ne briše in ne zamenja, zapis .verified ostane kot je (kljuka datoteke, katere hash ne more izračunati, ne zažene: neskladje = tiho);
  # ena vrstica, PATH se izvozi, naslednji zagon poskusi znova.
  if [ -f "$EXE" ] && [ ! -L "$EXE" ]; then
    shm "$EXE"
    if [ -z "$SH" ]; then
      echo "minigw plugin: the client in bin is in use and could not be checked this time; trying again at the next session start"
      qb=$(printf '%s' "$BIN" | sed "s/'/'\\\\''/g"); line="export PATH='$qb':\"\$PATH\""
      [ -n "${CLAUDE_ENV_FILE:-}" ] && { grep -qxF "$line" "$CLAUDE_ENV_FILE" 2>/dev/null || echo "$line" >> "$CLAUDE_ENV_FILE"; }
      tidy; exit 0
    fi
  fi
  # Windows/Linux: ena izvršljiva datoteka (Go, brez odvisnosti); kontrolna vsota iz SHA256SUMS iste izdaje.
  # Nameščen minigw: Linux povezava bin/minigw; Windows: zagonska datoteka bin/minigw, ki jo napiše minigw plugin install (glej winlauncher):
  # skripta je ne ustvari ne prepiše; brez veljavne zagonske datoteke velja lastna kopija bin/minigw.exe (v Git Bashu jo »minigw« najde).
  linkinst() {  # $inst: nameščen minigw (overjen, ali edini, ki ga je mogoče uporabiti)
    if [ "$os" = linux ]; then
      [ "$(readlink "$EXE" 2>/dev/null)" = "$inst" ] || { rm -f "$EXE"; forget; ln -s "$inst" "$EXE"; } ||
        echo "minigw plugin: could not link $(san "$EXE") to $(san "$inst")."
    else
      if [ -n "$viamsi" ]; then copyinst; return $?; fi
      EXE=$inst  # Windows: zagonska datoteka že obstaja (jo je napisal minigw plugin install); tu se ne piše nič
    fi
    return 0
  }
  # copyinst (Windows, nameščeni minigw brez zagonske datoteke): overjeni nameščeni minigw se KOPIRA kot lastna kopija bin/minigw.exe: cp v
  # minigw.exe.new.<pid>, kopija se preveri z istim parom vsot, vsote se objavijo PRED premikom (glej keepsums), ENA zamenjava mv -f. Kopija je
  # »najnovejši overjeni odjemalec, ki ga imamo«; ko MSI posodobi nameščenega, naslednji zagon kopira znova. Neoverjene datoteke se ne kopira.
  copyinst() {
    shm "$inst"; hi=$SH
    if [ -f "$EXE" ] && [ ! -L "$EXE" ] && { shm "$EXE"; [ "$SH" = "$hi" ]; }; then [ "$isums" = "$SN" ] && keepsums; return 0; fi  # že ta kopija
    ft="$BIN/${EXE##*/}.new.$$"
    if cp "$inst" "$ft" 2>/dev/null && chmod +x "$ft" && trusted "$ft"; then
      [ "$isums" = "$SN" ] && keepsums
      if mv -f "$ft" "$EXE"; then forget; return 0; fi
      forget; rm -f "$ft"; echo "minigw plugin: could not replace $(san "$EXE") (in use?); it stays and the installed minigw is copied again in the next session."; return 1
    fi
    rm -f "$ft"; echo "minigw plugin: could not copy the installed minigw into place."; return 1
  }
  [ -n "$inst" ] && { linkinst || inst=""; }
  if [ -n "$inst" ]; then
    [ -z "$viamsi" ] && [ "$isums" = "$SN" ] && keepsums  # overjen z vsotami tega zagona: shrani jih za brez omrežja
  elif [ -L "$EXE" ] || [ ! -x "$EXE" ] || ! authexe || { [ -n "$(want "$asset")" ] && { shm "$EXE"; [ "$SH" != "$(want "$asset")" ]; }; }; then
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
        fi; dropshims; unrecord; exit 0; }
    fi
  fi
  # Windows: zagonska datoteka, ki ne velja (neoverjen ali prestar odjemalec, ni besedila Go odjemalca), bi zasenčila lastno kopijo, ki
  # jo zdaj uporabljamo: odstrani se (dropshims). Veljavna ostane (inst je nastavljen), stara a overjena tudi (uporabljena kot stale).
  [ -n "$launcher" ] && { [ -z "$inst" ] || [ -n "$viamsi" ]; } && dropshims
fi

# #153: zapis bin/.verified (za kb-hook.sh) le, če je preverjanje v tem zagonu res uspelo: macOS podpis Developer ID; Linux/Windows SHA-256
# v podpisanih vsotah (trusted). Izjema brez ssh-keygen (neoverjena lastna kopija) zapisa NIMA. SHA-256 (po povezavi) gre atomarno, 600.
# Windows z nameščenim minigw: kljuka zažene lupino bin/minigw, ta pa nameščeni minigw: zapis ima obe vrstici (»sha  pot«).
ent=$EXE; [ "$os" = windows ] && [ -n "$usedlauncher" ] && [ -n "$inst" ] && ent="$BIN/minigw"
# Pravilo: preverjanje ali zagonska datoteka v nasprotju = zapis se zbriše; sicer se napiše (neuspel zapis pusti obstoječega: neskladje po
# hashu kljuko tako ali tako utiša). Zagonska datoteka (Windows, inst) mora imeti natanko besedilo Go odjemalca; zapis je atomaren (.new.<pid> + mv).
if { if [ "$os" = darwin ]; then valid "$EXE"; else trusted "$EXE"; fi; } &&
   { if [ "$ent" = "$EXE" ]; then shm "$ent"; else  # lupina mora biti ta, ki jo je zapisal ta zagon (primerjava vsebine)
       [ -f "$ent" ] && [ ! -L "$ent" ] && same shimtext "$ent" && shm "$ent"; fi; } && h1=$SH && h2=$h1 &&
   { [ "$ent" = "$EXE" ] || { shm "$EXE"; h2=$SH; }; } && [ -n "$h1" ] && [ -n "$h2" ]; then
  ( umask 077; { printf '%s  %s\n' "$h1" "$ent"; [ "$ent" = "$EXE" ] || printf '%s  %s\n' "$h2" "$EXE"; } > "$VN" ) &&
    mv -f "$VN" "$BIN/.verified" || rm -f "$VN"
else unrecord; fi

# Izbran minigw (že overjen na macOS) je lahko prestar (brez prenosa, zadnja izdaja starejša, ali ga namestitev še ni posodobila):
# nikoli tiho, ena vrstica iz dejanskega izida. Različica le v strogi obliki.
[ "$os" != darwin ] && ! command -v ssh-keygen >/dev/null 2>&1 && [ -x "$EXE" ] &&
  echo "minigw plugin: ssh-keygen is not installed, so the plugin's minigw could not be verified against the release signature; install OpenSSH to enable the check"
cur=$(exever "$EXE")
# Kateri minigw se res uporablja: nameščeni (Windows/Linux inst, macOS brew/aplikacija: other) ali lastna kopija vtičnika.
usedinst=""
case $os in darwin) { [ -n "${other:-}" ] || [ -L "$EXE" ]; } && usedinst=1 ;; *) [ -n "$inst" ] && usedinst=1 ;; esac
if [ -n "$usedinst" ]; then src="the installed minigw"; else src="the plugin's own copy of minigw"; fi
if ! goodver "$cur"; then
  echo "minigw plugin: using $src, whose version could not be read; this plugin's commands need $MIN or newer and may not work until it is updated ($(how))"
elif older "$cur" "$MIN" && [ -n "$usedinst" ]; then
  echo "minigw plugin: using the installed minigw $cur, older than $MIN which this plugin's commands need; some commands will not work until it is updated ($(how))"
elif older "$cur" "$MIN"; then
  sc=""; [ -n "$stale" ] && older "$iv" "$cur" && sc=" (the installed minigw $iv is older still: $(how))"
  echo "minigw plugin: using the plugin's own copy of minigw $cur (the latest release), older than $MIN which this plugin's commands need; some commands will not work until a newer minigw is released or you install a beta from https://github.com/$REPO/releases${sc}"
elif [ -n "$stale" ]; then
  echo "minigw plugin: the installed minigw${iv:+ $iv} is older than $MIN: the plugin uses its own copy $cur (update the installed one: $(how))"
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
