#!/bin/zsh
# Preizkus plugins/minigw/scripts/install-minigw.sh (podpis SHA256SUMS, #8; H1/L4 0.5.17) v Linuxu (Docker), z lažno
# izdajo prek file:// in testnim ključem (pravi ključ izdaj se tu nikoli ne uporabi).
#   1. nameščen minigw 0.5.16 (brez verify-sums) + claude, ki vrne 0, brez ssh-keygen, smetni podpis → stari ostane
#   2. prva namestitev brez ssh-keygen → zavrnjena
#   3. ssh-keygen in podpis testnega ključa → nameščen
#   4. napačen imenski prostor, obstoječi ostane → zavrnjen, z razlogom (L4)
#   5. #25: podpisana starejša izdaja (VERSION 0.5.16) čez novejši nameščeni → zavrnjena, z razlogom
#   6. ista z MINIGW_ALLOW_DOWNGRADE=1 → nameščena
#   7. N1: podpisana izdaja brez VERSION (= 0.5.17) čez nameščeni 0.5.19 → zavrnjena
#   9. Linux: nameščen ~/.local/bin/minigw, ki ni starejši od MIN → povezava (brez prenosa, tudi ob novejši izdaji);
#      starejši od MIN → prenos
#  10. Windows (lažna uname/cygpath, na tem Macu): nameščen minigw.exe → lupini bin/minigw in bin/minigw.cmd kot
#      »minigw plugin install« (client/plugin.go shimSh/shimCmd), kopija vtičnika gre; brez namestitve lupini gresta;
#      M1: novejša podpisana izdaja → brez serve.json (MSI le z odjemalcem) namig na MSI, s serve.json /minigw:gw update
#  11. M1 Linux: nameščen minigw 0.5.19 (≥ MIN), podpisana izdaja 0.99.0 → povezava; brez enote systemd namig za
#      odjemalca (install.sh ali zamenjaj ~/.local/bin/minigw), z enoto /minigw:gw update
#  12. install.sh v Linuxu (kot uporabnik, ne root): privzeto odjemalec + vtičnik v ~/.local/bin, --client, --gateway
#      (serve init + serve install z lažnim systemctl), --uninstall [--purge], napačen podpis → nič nameščeno
#   8. izhod »minigw status« → connected / token rejected / device not approved (ID) / not connected; brez claude: namig (#6)
emulate -L zsh; set -eu
cd ${0:A:h}/..
T=$(mktemp -d); trap 'rm -rf $T' EXIT
fail() { print -u2 "FAIL: $*"; exit 1 }

# 0.5.16 (zadnji commit pred verify-sums) in trenutni minigw za Linux v vsebniku
mkdir -p $T/old $T/rel/good $T/rel/bad $T/rel/evil $T/rel/older $T/rel/newer $T/home/.config/minigw $T/home/bin
git archive 4994f84 client | tar -x -C $T/old
(cd $T/old/client && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o $T/minigw-old .)
(cd client && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o $T/rel/good/minigw-linux-arm64 .)
print '{"url":"http://127.0.0.1:9","token":"t"}' > $T/home/.config/minigw/connection.json
print '#!/bin/sh\nexit 0' > $T/home/bin/claude; chmod +x $T/home/bin/claude

ssh-keygen -q -t ed25519 -N "" -C test -f $T/key
(cd $T/rel/good && shasum -a 256 minigw-linux-arm64 > SHA256SUMS && ssh-keygen -Y sign -q -f $T/key -n minigw-release < SHA256SUMS > SHA256SUMS.sig)
cp $T/rel/good/minigw-linux-arm64 $T/rel/good/SHA256SUMS $T/rel/bad/
cp $T/minigw-old $T/rel/older/minigw-linux-arm64
(cd $T/rel/older && { print "VERSION 0.5.16"; shasum -a 256 minigw-linux-arm64; } > SHA256SUMS && ssh-keygen -Y sign -q -f $T/key -n minigw-release < SHA256SUMS > SHA256SUMS.sig)
ssh-keygen -Y sign -q -f $T/key -n other < $T/rel/bad/SHA256SUMS > $T/rel/bad/SHA256SUMS.sig
cp $T/rel/good/minigw-linux-arm64 $T/rel/newer/
(cd $T/rel/newer && { print "VERSION 0.99.0"; shasum -a 256 minigw-linux-arm64; } > SHA256SUMS && ssh-keygen -Y sign -q -f $T/key -n minigw-release < SHA256SUMS > SHA256SUMS.sig)
print '#!/bin/sh\necho EVIL' > $T/rel/evil/minigw-linux-arm64
(cd $T/rel/evil && shasum -a 256 minigw-linux-arm64 > SHA256SUMS && print garbage > SHA256SUMS.sig)
# 12: izdaja za install.sh (minigw + lažni minigw-tui, VERSION), podpisana s testnim ključem; ena z napačnim imenskim prostorom
V=$(grep -o 'const version = "[^"]*"' client/main.go | cut -d'"' -f2)
mkdir -p $T/rel/inst $T/rel/instbad $T/fakebin
cp $T/rel/good/minigw-linux-arm64 $T/rel/inst/; print '#!/bin/sh\necho tui' > $T/rel/inst/minigw-tui-linux-arm64
(cd $T/rel/inst && { print "VERSION $V"; shasum -a 256 minigw-linux-arm64 minigw-tui-linux-arm64; } > SHA256SUMS && ssh-keygen -Y sign -q -f $T/key -n minigw-release < SHA256SUMS > SHA256SUMS.sig)
cp $T/rel/inst/* $T/rel/instbad/; ssh-keygen -Y sign -q -f $T/key -n other < $T/rel/instbad/SHA256SUMS > $T/rel/instbad/SHA256SUMS.sig
sed "s|ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINt++OHn+HHmR9mmA2UI+B0osuilO2GP0ddEzNp0GTrH|$(cut -d' ' -f1,2 $T/key.pub)|" install.sh > $T/install-test.sh
grep -q "$(cut -d' ' -f2 $T/key.pub)" $T/install-test.sh || fail "12: test key not pinned in the install.sh copy"
print '#!/bin/sh\n[ "$1" = --version ] && echo 2.1.300; exit 0' > $T/fakebin/claude
print '#!/bin/sh\nexit 0' > $T/fakebin/systemctl; cp $T/fakebin/systemctl $T/fakebin/loginctl; chmod +x $T/fakebin/*

# Kot v pravem vtičniku: MIN_CLIENT leži poleg skripte (brez njega bi skripta uporabila rezervno vrednost in to povedala).
MINV=$(<plugins/minigw/scripts/MIN_CLIENT); print -r -- $MINV > $T/MIN_CLIENT; print -r -- $MINV > $T/minv
[[ $V == $MINV ]] || fail "9: the client built from the source ($V) is not MIN_CLIENT ($MINV): the \"installed current\" client would be too old"
cp ${INSTALL_SCRIPT:-plugins/minigw/scripts/install-minigw.sh} $T/real.sh;  # INSTALL_SCRIPT: preizkus starejše skripte (da se vidi, da test pade)
 cp plugins/minigw/scripts/kb-hook.sh $T/kb-hook.sh
sed "s|^SIGNER=.*|SIGNER='minigw-release namespaces=\"minigw-release\" $(cut -d' ' -f1,2 $T/key.pub)'|" $T/real.sh > $T/testkey.sh

# 10: Windows brez Windows: lažna uname (MINGW64) in cygpath (-u: ista pot, -m/-w: C: spredaj); pot z ' in %.
W="$T/win/O'Brien 100%"; mkdir -p $T/win/fake "$W/Programs/Mini Gateway" $T/win/data/bin $T/win/home
print '#!/bin/sh\ncase $1 in -s) echo MINGW64_NT-10.0 ;; *) echo x86_64 ;; esac' > $T/win/fake/uname
print '#!/bin/sh\ncase $1 in -u) printf "%s\\n" "$2" ;; -m) printf "C:%s\\n" "$2" ;; -w) printf "C:%s\\n" "$2" | tr / "\\\\" ;; esac' > $T/win/fake/cygpath
print "#!/bin/sh\n[ \"\$1\" = --version ] && echo minigw $MINV; [ \"\$1\" = status ] && echo \"Status:    connected\"; exit 0" > "$W/Programs/Mini Gateway/minigw.exe"
# Git Bash zamenja golo ime »minigw«, ki ne obstaja, z minigw.exe; POSIX lupina tega ne zna, zato mv/rm ovojnici ujameta klic na golem imenu, ki ga
# ni, ko minigw.exe obstaja (VIOL), in zabeležita vsak klic na minigw, minigw.exe in .keep (OPS: v ustaljenem stanju sme biti prazen)
# (test/>/cat/cmp ostanejo nepreizkušeni: to pokrije le pravi Windows)
for c in mv rm; do print '#!/bin/sh\nfor a; do case $a in */bin/minigw|*/bin/minigw.exe|*/bin/minigw.exe.keep) echo "'$c' $*" >> "$OPS" ;; esac\ncase $a in */bin/minigw) [ ! -e "$a" ] && [ -e "$a.exe" ] && echo "'$c' $*" >> "$VIOL" ;; esac; done\nexec /bin/'$c' "$@"' > $T/win/fake/$c; done
chmod +x $T/win/fake/* "$W/Programs/Mini Gateway/minigw.exe"; print old > $T/win/data/bin/minigw.exe
# Linux in Windows zaženeta le datoteko, ki je v preverjenih SHA256SUMS ob kopiji (podpis testnega ključa): wtrust jo vpiše
wtrust() { { print "VERSION $2"; print "$(shasum -a 256 "$1" | cut -d' ' -f1)  minigw-windows-amd64.exe"; } > $T/win/data/bin/SHA256SUMS
  ssh-keygen -Y sign -q -f $T/key -n minigw-release < $T/win/data/bin/SHA256SUMS > $T/win/data/bin/SHA256SUMS.sig }
wtrust "$W/Programs/Mini Gateway/minigw.exe" $MINV
winrun() { OPS=$T/win/ops VIOL=$T/win/viol CLAUDE_PLUGIN_DATA=$T/win/data CLAUDE_ENV_FILE= LOCALAPPDATA=$W APPDATA=$T/win/roaming MINIGW_RELEASE_BASE=file://$T/rel/${1:-nonexistent} HOME=$T/win/home PATH=$T/win/fake:$PATH sh $T/testkey.sh }
out=$(winrun newer); print -r -- $out
[[ $out == *"minigw $MINV is installed, 0.99.0 is available: run the installer https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/minigw-windows-amd64.msi (check it first: docs/install.md, Check a download)"* && $out != *"/minigw:gw update"* ]] ||
  fail "10: no MSI update hint for a client-only install"
mkdir -p $T/win/roaming/minigw; print '{}' > $T/win/roaming/minigw/serve.json
out=$(winrun newer); print -r -- $out
[[ $out == *"Mini Gateway $MINV is installed, 0.99.0 is available: /minigw:gw update"* ]] || fail "10: no update hint (M1)"
exe="$W/Programs/Mini Gateway/minigw.exe" sq="'" rep="'\''"
[[ "$(<$T/win/data/bin/minigw)" == "#!/bin/sh"$'\n'"# Mini Gateway: the installed minigw"$'\n'"exec 'C:${exe//$sq/$rep}' \"\$@\"" ]] || fail "10: sh shim: $(<$T/win/data/bin/minigw)"
[[ "$(<$T/win/data/bin/minigw.cmd)" == "@setlocal DisableDelayedExpansion"$'\r\n'"@\"%LOCALAPPDATA%\\Programs\\Mini Gateway\\minigw.exe\" %*"$'\r' ]] || fail "10: cmd shim: $(<$T/win/data/bin/minigw.cmd)"
# #153: zapis sprejetega za kb-hook.sh: lupina bin/minigw in nameščeni minigw.exe, vsak z lastnim SHA-256, mode 600
rec=$T/win/data/bin/.verified; sh_=$(shasum -a 256 $T/win/data/bin/minigw | cut -d" " -f1); ex_=$(shasum -a 256 "$exe" | cut -d" " -f1)
[[ "$(<$rec)" == "$sh_  $T/win/data/bin/minigw"$'\n'"$ex_  $exe" && "$(ls -l $rec | cut -c1-10)" == -rw------- && ! -e $rec.new ]] || fail "10: .verified: $(<$rec)"
[[ "$(<$T/win/data/bin/minigw.exe)" == old && ! -e $T/win/data/bin/minigw.exe.keep && ! -e $T/win/viol ]] || fail "10: the plugin's own minigw.exe was touched by a shim write: $(cat $T/win/viol 2>&1)"
[[ ! -e $T/win/data/bin/.sums.new && ! -e $T/win/data/bin/.sig.new ]] || fail "10: verified sums left behind (#159)"
# ustaljeno stanje (lupina že prava): nobenega mv/rm na minigw, minigw.exe ali .keep v dveh zaporednih zagonih
rm -f $T/win/ops; winrun newer > /dev/null; winrun newer > /dev/null
[[ ! -e $T/win/ops && "$(<$T/win/data/bin/minigw.exe)" == old && ! -e $T/win/viol ]] || fail "10: steady state touched minigw/minigw.exe: $(cat $T/win/ops 2>&1)"
# ustaljeno stanje ne zapiše niti minigw.cmd ne lupine (nobena datoteka minigw* novejša od oznake)
touch $T/win/mark; sleep 1.1; winrun newer > /dev/null
[[ -z "$(find $T/win/data/bin -name 'minigw*' -type f -newer $T/win/mark)" ]] || fail "10: steady state rewrote $(find $T/win/data/bin -name 'minigw*' -type f -newer $T/win/mark)"
# ključavnica zasedena (druga seja), lupine ni: ta zagon je ne zapiše, minigw.exe se ne dotakne
rm $T/win/data/bin/minigw; mkdir $T/win/data/bin/.aside.lock; rm -f $T/win/ops $T/win/data/bin/.verified; winrun newer > /dev/null
[[ ! -e $T/win/data/bin/minigw && ! -e $T/win/ops && ! -e $T/win/data/bin/.verified && -d $T/win/data/bin/.aside.lock && "$(<$T/win/data/bin/minigw.exe)" == old ]] ||
  fail "10: a held lock did not stop the shim write or the own exe was touched"
rmdir $T/win/data/bin/.aside.lock; winrun newer > /dev/null
[[ -f $T/win/data/bin/minigw && -s $T/win/data/bin/.verified && "$(<$T/win/data/bin/minigw.exe)" == old && ! -e $T/win/data/bin/.aside.lock && ! -e $T/win/viol ]] || fail "10: first shim write after the lock went"
# dve seji hkrati (10 ponovitev, vsakič prvi prehod): obe končata 0, zapis je in ustreza lupini, brez ostankov in napak
for i in 1 2 3 4 5 6 7 8 9 10; do
  rm -f $T/win/data/bin/minigw $T/win/data/bin/minigw.cmd $T/win/data/bin/.verified
  winrun newer > $T/win/p1.out 2> $T/win/p1.err & p1=$!
  winrun newer > $T/win/p2.out 2> $T/win/p2.err & p2=$!
  wait $p1 && wait $p2 || fail "10 parallel $i: exit status"
  [[ ! -s $T/win/p1.err && ! -s $T/win/p2.err ]] || fail "10 parallel $i: stderr: $(cat $T/win/p1.err $T/win/p2.err)"
  [[ "$(head -1 $T/win/data/bin/.verified | cut -d' ' -f1)" == "$(shasum -a 256 $T/win/data/bin/minigw | cut -d' ' -f1)" && -f $T/win/data/bin/minigw.cmd ]] || fail "10 parallel $i: no record, or it does not match the shim"
  [[ "$(<$T/win/data/bin/minigw.exe)" == old && -z "$(find $T/win/data/bin \( -name '*.new.*' -o -name '*.chk.*' -o -name '.aside.lock' -o -name '*.keep' \))" && ! -e $T/win/viol ]] ||
    fail "10 parallel $i: leftovers or the own exe touched: $(ls -A $T/win/data/bin)"
done
[[ -e $T/win/data/bin/minigw.exe && $out == *"connected to Mini Gateway"* ]] || fail "10: the plugin copy was deleted (a kb mcp config points at it) or status not read"
rm "$exe"; out=$(winrun); print -r -- $out
[[ ! -e $T/win/data/bin/minigw && ! -e $T/win/data/bin/minigw.cmd ]] || fail "10: shims kept without an installed minigw"
[[ ! -e $T/win/viol ]] || fail "10: removing the shims went through the bare name: $(cat $T/win/viol)"  # (the unverified own exe is removed by the reject path, by name)
# 10b: stari MSI minigw (0.5.20) in prenos ne uspe (ni izdaje): stari ostane, a glasno (enako kot macOS in Linux)
print '#!/bin/sh\n[ "$1" = --version ] && echo minigw 0.5.20; [ "$1" = status ] && echo "Status:    connected"; exit 0' > "$exe"; chmod +x "$exe"; wtrust "$exe" 0.5.20
out=$(winrun); print -r -- $out
[[ $out == *"using the installed minigw 0.5.20, older than $MINV which this plugin's commands need; some commands will not work until it is updated (run the installer"* && -e $T/win/data/bin/minigw && -e $T/win/data/bin/minigw.cmd ]] ||
  fail "10b: an authentic old MSI client is not kept offline, or silently"
# 10c: isti minigw.exe, a zamenjan (hash ni v preverjenih vsotah): ne zažene se, ne poveže
print '#!/bin/sh\ntouch '$T'/ran10\necho minigw 9.9.9' > "$exe"; chmod +x "$exe"; rm -f $T/ran10
out=$(winrun); print -r -- $out
[[ ! -e $T/ran10 && ! -e $T/win/data/bin/minigw && $out == *"cannot verify the installed minigw and cannot download a verified one"* ]] || fail "10c: a substituted MSI minigw was run or linked"
[[ ! -e $T/win/data/bin/.verified ]] || fail "10c: the record of the rejected minigw is still there (#153)"
# 10e: lupini iz seje 1 (overjen nameščeni minigw.exe), potem je exe zamenjan (hash ni v vsotah): lupini izgineta, zažene se kopija vtičnika
print '#!/bin/sh\n[ "$1" = --version ] && echo minigw '$MINV'; [ "$1" = status ] && echo "Status:    connected"; exit 0' > "$exe"; chmod +x "$exe"; wtrust "$exe" $MINV
cp "$exe" $T/win/data/bin/minigw.exe; winrun newer > /dev/null
[[ -e $T/win/data/bin/minigw && -e $T/win/data/bin/minigw.cmd ]] || fail "10e: session 1 wrote no shims"
print '#!/bin/sh\ntouch '$T'/ran10e' > "$exe"; rm -f $T/ran10e; out=$(winrun newer); print -r -- $out
[[ ! -e $T/win/data/bin/minigw && ! -e $T/win/data/bin/minigw.cmd && ! -e $T/ran10e && -e $T/win/data/bin/minigw.exe ]] || fail "10e: a stale shim still points at the substituted installed minigw"
# 10f (#153 review 2): star lupina bin/minigw (drug cilj) + zapis lupine se ne zgodi (ključavnica zasedena) → brez .verified, kljuka ne zažene starega cilja
print '#!/bin/sh\n[ "$1" = --version ] && echo minigw '$MINV'; [ "$1" = status ] && echo "Status:    connected"; exit 0' > "$exe"; chmod +x "$exe"; wtrust "$exe" $MINV
print '#!/bin/sh\n# Mini Gateway: the installed minigw\ntouch '$T'/ran10f' > $T/win/data/bin/minigw; chmod +x $T/win/data/bin/minigw; mkdir -p $T/win/data/bin/.aside.lock; rm -f $T/ran10f $T/win/data/bin/.verified
winrun newer > /dev/null
[[ ! -e $T/win/data/bin/.verified ]] || fail "10f: a record was written next to a shim this run did not write"
mkdir -p $T/win/roaming/minigw; print '{"version":1,"enabled":[{"gateway":"g","project":3}]}' > $T/win/roaming/minigw/kb-hooks.json
print -n '{}' | CLAUDE_PLUGIN_DATA=$T/win/data HOME=$T/win/home APPDATA=$T/win/roaming PATH=$T/win/fake:$PATH sh plugins/minigw/scripts/kb-hook.sh stop > /dev/null 2>&1
[[ ! -e $T/ran10f ]] || fail "10f: the hook ran the old shim's target"
rmdir $T/win/data/bin/.aside.lock; winrun newer > /dev/null   # kontrola: ko zapis uspe, je zapis tu
[[ -s $T/win/data/bin/.verified && ! -e $T/ran10f ]] || fail "10f: no record after a successful shim write"
rm -f $T/win/roaming/minigw/kb-hooks.json
# 10d: BREZ vnaprej vpisanih vsot (stanje prave naprave): nameščen minigw.exe z neznanim hashem se ne zažene, a vrstica ponudi posodobitev
print '#!/bin/sh\n[ "$1" = --version ] && echo minigw 0.7.0; exit 0' > "$exe"; chmod +x "$exe"; rm -f $T/win/data/bin/SHA256SUMS $T/win/data/bin/SHA256SUMS.sig
out=$(winrun newer); print -r -- $out
[[ $out == *"the minigw installed at "*"is not the current release 0.99.0 (or could not be verified); update it: /minigw:gw update"* ]] || fail "10d: no update line for an untrusted installed gateway client"
rm $T/win/roaming/minigw/serve.json; out=$(winrun newer)
[[ $out == *"is not the current release 0.99.0 (or could not be verified); update it: run the installer https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/minigw-windows-amd64.msi"* ]] || fail "10d: no MSI line for an untrusted client-only install"
[[ ! -e $T/win/viol ]] || fail "10: a write/mv/rm on bin/minigw while minigw.exe exists: $(cat $T/win/viol)"
print "ok 10: Windows shims point at the installed minigw; client-only MSI gets the MSI hint"

docker run --rm -v $T:/t:ro debian:stable-slim@sha256:fbfb4059d2127120aa05b6e6064523d2102552f0cf0026ddf0701c12f1f8f3e0 sh -c '
set -u
apt-get update -qq >/dev/null 2>&1; apt-get install -y -qq curl >/dev/null 2>&1
fail() { echo "FAIL: $*"; exit 1; }
run() { CLAUDE_PLUGIN_DATA=$1 CLAUDE_ENV_FILE= MINIGW_RELEASE_BASE=file:///t/rel/$2 HOME=/tmp/home PATH=/tmp/home/bin:$PATH sh /t/$3.sh; }
cp -r /t/home /tmp/home
# trust DIR FILE: SHA256SUMS ob kopiji v DIR/bin s hashem FILE, podpisan s testnim ključem (potem ko je nameščen ssh-keygen)
trust() { mkdir -p $1/bin; { echo "VERSION ${3:-0.5.19}"; echo "$(sha256sum $2 | cut -d" " -f1)  minigw-linux-arm64"; } > $1/bin/SHA256SUMS
  ssh-keygen -Y sign -q -f /t/key -n minigw-release < $1/bin/SHA256SUMS > $1/bin/SHA256SUMS.sig; }
MINV=$(cat /t/minv)

mkdir -p /tmp/d1/bin; cp /t/minigw-old /tmp/d1/bin/minigw
out=$(run /tmp/d1 evil real); echo "$out"
[ "$(/tmp/d1/bin/minigw --version | head -1)" = "minigw 0.5.16" ] || fail "1: old minigw replaced"
echo "$out" | grep -q "not updated: cannot check the release signature" || fail "1: no reason printed"
echo "ok 1: 0.5.16 minigw does not verify a garbage signature"
# 1b (#153): brez ssh-keygen se obstoječa lastna kopija obdrži (izjema), a BREZ zapisa .verified; kljuka je tiha (marker odsoten)
mkdir -p /tmp/d1b/bin /tmp/home/.config/minigw; printf "#!/bin/sh\ntouch /tmp/marker1b\necho minigw 0.5.16\n" > /tmp/d1b/bin/minigw; chmod +x /tmp/d1b/bin/minigw
echo stale > /tmp/d1b/bin/.verified
out=$(run /tmp/d1b evil real); echo "$out"
[ -x /tmp/d1b/bin/minigw ] && [ ! -e /tmp/d1b/bin/.verified ] || fail "1b: the unverified own copy got a record, or was removed"
echo "{\"version\":1,\"enabled\":[{\"gateway\":\"g\",\"project\":3}]}" > /tmp/home/.config/minigw/kb-hooks.json; rm -f /tmp/marker1b
echo "{}" | CLAUDE_PLUGIN_DATA=/tmp/d1b HOME=/tmp/home sh /t/kb-hook.sh stop; [ ! -e /tmp/marker1b ] || fail "1b: the hook ran an unverified copy"
rm -f /tmp/home/.config/minigw/kb-hooks.json
echo "ok 1b: no record for an unverified own copy, the hook stays silent"

mkdir -p /tmp/d2/bin; echo stale > /tmp/d2/bin/.verified  # #153: zavrnitev zbriše zapis
out=$(run /tmp/d2 good real); echo "$out"
[ ! -e /tmp/d2/bin/minigw ] || fail "2: installed without a signature check"
[ ! -e /tmp/d2/bin/.verified ] || fail "2: the record survived a refused install (#153)"
echo "ok 2: first install without ssh-keygen refused"

apt-get install -y -qq openssh-client >/dev/null 2>&1
out=$(run /tmp/d3 good testkey); echo "$out"
[ -x /tmp/d3/bin/minigw ] || fail "3: not installed with a good signature"
[ "$(cat /tmp/d3/bin/.verified)" = "$(sha256sum /tmp/d3/bin/minigw | cut -d" " -f1)  /tmp/d3/bin/minigw" ] || fail "3: no record of the accepted download (#153)"
[ "$(stat -c %a /tmp/d3/bin/.verified)" = 600 ] || fail "3: the record is not mode 600"
echo "ok 3: good signature installs"
# 3b (#153): dve seji hkrati v svežih mapah: obe končata 0, zapis ustreza datoteki, brez ostankov in napak
for i in 1 2 3 4 5 6 7 8 9 10; do
  d=/tmp/dpar$i
  run $d good testkey > /tmp/p1.out 2> /tmp/p1.err & p1=$!
  run $d good testkey > /tmp/p2.out 2> /tmp/p2.err & p2=$!
  wait $p1 && wait $p2 || fail "3b $i: exit status"
  [ ! -s /tmp/p1.err ] && [ ! -s /tmp/p2.err ] || fail "3b $i: stderr: $(cat /tmp/p1.err /tmp/p2.err)"
  [ "$(cut -d" " -f1 $d/bin/.verified)" = "$(sha256sum $d/bin/minigw | cut -d" " -f1)" ] || fail "3b $i: no record, or it does not match"
  [ -z "$(find $d/bin -name "*.new.*" -o -name "*.chk.*" -o -name .aside.lock)" ] || fail "3b $i: leftovers: $(ls -A $d/bin)"
done
echo "ok 3b: two session starts at once keep the record"

mkdir -p /tmp/d4/bin; cp /t/minigw-old /tmp/d4/bin/minigw; cp /t/rel/older/SHA256SUMS /t/rel/older/SHA256SUMS.sig /tmp/d4/bin/  # overjena stara kopija
out=$(run /tmp/d4 bad testkey); echo "$out"
[ "$(/tmp/d4/bin/minigw --version | head -1)" = "minigw 0.5.16" ] || fail "4: replaced despite a wrong signature"
echo "$out" | grep -q "not updated: the release.s SHA256SUMS is not signed" || fail "4: no reason printed"
echo "ok 4: wrong namespace refused, reason printed"

new=$(/tmp/d3/bin/minigw --version | head -1)
mkdir -p /tmp/d5/bin; cp /tmp/d3/bin/minigw /tmp/d3/bin/SHA256SUMS /tmp/d3/bin/SHA256SUMS.sig /tmp/d5/bin/
out=$(run /tmp/d5 older testkey); echo "$out"
[ "$(/tmp/d5/bin/minigw --version | head -1)" = "$new" ] || fail "5: rolled back to an older signed release"
echo "$out" | grep -q "not updated: the latest release (0.5.16) is older than the installed" || fail "5: no reason printed"
echo "ok 5: older signed release refused (#25)"

out=$(MINIGW_ALLOW_DOWNGRADE=1 run /tmp/d5 older testkey); echo "$out"
[ "$(/tmp/d5/bin/minigw --version | head -1)" = "minigw 0.5.16" ] || fail "6: MINIGW_ALLOW_DOWNGRADE=1 did not install"
echo "ok 6: downgrade with MINIGW_ALLOW_DOWNGRADE=1"

mkdir -p /tmp/d7/bin; printf "#!/bin/sh\necho minigw 0.5.19\n" > /tmp/d7/bin/minigw; chmod +x /tmp/d7/bin/minigw; trust /tmp/d7 /tmp/d7/bin/minigw
out=$(run /tmp/d7 good testkey); echo "$out"
[ "$(/tmp/d7/bin/minigw --version | head -1)" = "minigw 0.5.19" ] || fail "7: a signed release without VERSION replaced 0.5.19"
echo "$out" | grep -q "not updated: the latest release (0.5.17) is older than the installed minigw 0.5.19" || fail "7: no reason printed"
echo "ok 7: signed release without VERSION counts as 0.5.17 (N1)"

# 8: kljuka prebere izhod »minigw status« (lažni minigw); v kontekst le fiksni nizi in ID naprave
mkdir -p /tmp/d8/bin; cat > /tmp/d8/bin/minigw <<EOS
#!/bin/sh
[ "\$1" = --version ] && { echo minigw 0.5.19; exit 0; }
cat /tmp/status.out; exit \$(cat /tmp/status.rc)
EOS
chmod +x /tmp/d8/bin/minigw; trust /tmp/d8 /tmp/d8/bin/minigw
ID=AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCdE
st() { printf "%s\n" "$1" > /tmp/status.out; echo $2 > /tmp/status.rc; out=$(run /tmp/d8 nonexistent testkey); echo "$out"; }
st "Status:    connected" 0
echo "$out" | grep -q "connected to Mini Gateway" || fail "8: connected"
st "Gateway: x
Status:    token rejected: EVIL text" 1
echo "$out" | grep -q "gateway token was rejected" && ! echo "$out" | grep -q EVIL || fail "8: token rejected"
st "Device:    $ID (waiting for approval)
Status:    device not approved: EVIL text" 1
echo "$out" | grep -q "device not approved (ID $ID)" && ! echo "$out" | grep -q EVIL || fail "8: device not approved"
st "Status:    device not approved: x" 1
echo "$out" | grep -q "installed but not connected" && ! echo "$out" | grep -q "(ID" || fail "8: device not approved without a Device line"
st "Status:    gateway not reachable" 1
echo "$out" | grep -q "installed but not connected" || fail "8: not connected"
! echo "$out" | grep -q "standalone claude command" || fail "8: claude hint with claude present"
rm /tmp/home/bin/claude; out=$(PATH=/tmp/home/bin:/usr/bin:/bin run /tmp/d8 nonexistent testkey); echo "$out"
echo "$out" | grep -q "standalone claude command on PATH" || fail "8: no claude hint"
echo "ok 8: hook reports status reasons and a missing claude"

mkdir -p /tmp/home/.local/bin; cp /t/rel/good/minigw-linux-arm64 /tmp/home/.local/bin/minigw
mkdir -p /tmp/d9/bin; cp /t/minigw-old /tmp/d9/bin/minigw; cp /t/rel/good/SHA256SUMS /t/rel/good/SHA256SUMS.sig /tmp/d9/bin/  # prenesena izdaja: overjen je tudi nameščeni iz iste izdaje
for rel in older good newer; do
  out=$(run /tmp/d9 $rel testkey); echo "$out"
  [ "$(readlink /tmp/d9/bin/minigw)" = /tmp/home/.local/bin/minigw ] || fail "9: plugin bin not linked to the installed minigw ($rel)"
  [ "$(cat /tmp/d9/bin/.verified)" = "$(sha256sum /tmp/home/.local/bin/minigw | cut -d" " -f1)  /tmp/d9/bin/minigw" ] || fail "9: no record of the linked installed minigw ($rel, #153)"
  ! echo "$out" | grep -q "not updated" || fail "9: downgrade note although the installed minigw is used"
done
rm -f /tmp/home/.local/bin/minigw; cp /t/minigw-old /tmp/home/.local/bin/minigw  # rm: the client of the last run may still be running
out=$(run /tmp/d9 good testkey); echo "$out"
[ ! -L /tmp/d9/bin/minigw ] && cmp -s /t/rel/good/minigw-linux-arm64 /tmp/d9/bin/minigw || fail "9b: minigw older than MIN linked instead of the release"
# 9c: nameščen minigw, ki ni v preverjenih vsotah (zamenjan): se ne zažene, ne poveže; velja prenesena overjena kopija
rm -f /tmp/home/.local/bin/minigw /tmp/ran9c; printf "#!/bin/sh\ntouch /tmp/ran9c\necho minigw 9.9.9\n" > /tmp/home/.local/bin/minigw; chmod 755 /tmp/home/.local/bin/minigw
rm -rf /tmp/d9/bin; mkdir -p /tmp/d9/bin
out=$(run /tmp/d9 good testkey); echo "$out"
[ ! -e /tmp/ran9c ] && [ ! -L /tmp/d9/bin/minigw ] && cmp -s /t/rel/good/minigw-linux-arm64 /tmp/d9/bin/minigw || fail "9c: a substituted installed minigw was run or linked"
[ "$(cut -d" " -f1 /tmp/d9/bin/.verified)" = "$(sha256sum /tmp/d9/bin/minigw | cut -d" " -f1)" ] || fail "9c: the record is not of the downloaded copy (#153)"
[ -f /tmp/d9/bin/SHA256SUMS.sig ] || fail "9c: the verified sums were not kept next to the downloaded copy"
rm -rf /tmp/home/.local
echo "ok 9: installed minigw (verified, not older than MIN) linked, never downloaded; 9c: a substituted one never run"

mkdir -p /tmp/home/.local/bin /tmp/d11/bin; printf "#!/bin/sh\necho minigw $MINV\n" > /tmp/home/.local/bin/minigw; chmod +x /tmp/home/.local/bin/minigw; trust /tmp/d11 /tmp/home/.local/bin/minigw $MINV
out=$(run /tmp/d11 newer testkey); echo "$out"
[ "$(readlink /tmp/d11/bin/minigw)" = /tmp/home/.local/bin/minigw ] || fail "11: not linked to the installed minigw"
echo "$out" | grep -q "minigw $MINV is installed, 0.99.0 is available: run curl -fsSL https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/install.sh | sh (or replace /tmp/home/.local/bin/minigw with minigw-linux-arm64 from the latest release" || fail "11: no client-only update hint (M1)"
mkdir -p /tmp/home/.config/systemd/user; : > /tmp/home/.config/systemd/user/minigw-serve.service
out=$(run /tmp/d11 newer testkey); echo "$out"
echo "$out" | grep -q "Mini Gateway $MINV is installed, 0.99.0 is available: /minigw:gw update" || fail "11: no gateway update hint"
rm -rf /tmp/home/.config/systemd
# 11b: BREZ vnaprej vpisanih vsot: nameščen 0.7.0-podoben minigw (neznan hash) se ne zažene, vrstica ponudi posodobitev, plugin prenese svojega
printf "#!/bin/sh\ntouch /tmp/ran11b\necho minigw 0.7.0\n" > /tmp/home/.local/bin/minigw; rm -f /tmp/ran11b
out=$(run /tmp/d11b newer testkey); echo "$out"
echo "$out" | grep -q "the minigw installed at /tmp/home/.local/bin/minigw is not the current release 0.99.0 (or could not be verified); update it: curl -fsSL https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/install.sh | sh" || fail "11b: no update line for an untrusted installed client"
[ ! -e /tmp/ran11b ] && [ ! -L /tmp/d11b/bin/minigw ] && cmp -s /t/rel/newer/minigw-linux-arm64 /tmp/d11b/bin/minigw || fail "11b: the untrusted client was run or linked, or no own copy"
# 11c: nameščen je prav ta izdaja (hash v vsotah, preverjenih v tem zagonu): overjen že v prvi seji, brez vrstice
rm -f /tmp/home/.local/bin/minigw; cp /t/rel/good/minigw-linux-arm64 /tmp/home/.local/bin/minigw
out=$(run /tmp/d11c good testkey); echo "$out"
[ "$(readlink /tmp/d11c/bin/minigw)" = /tmp/home/.local/bin/minigw ] && ! echo "$out" | grep -q "not the current release" || fail "11c: the current installed client is not trusted in the first session"
[ -f /tmp/d11c/bin/SHA256SUMS ] || fail "11c: the verified sums were not kept"
rm -rf /tmp/home/.local
echo "ok 11: newer signed release than the installed minigw: hint, no download"

# 13 (#48): ~/.local/bin/minigw, ki ni last uporabnika ali je (z mapo ali mapo nad njo) zapisljiv za skupino ali druge, se ne
# zažene in ne poveže; višja mapa s sticky, last roota, sme
for bad in file-gw dir-ow owner up-ow up-foreign-sticky; do
  mkdir -p /tmp/home/.local/bin /tmp/d13/bin; printf "#!/bin/sh\ntouch /tmp/ran13; echo minigw $MINV\n" > /tmp/home/.local/bin/minigw
  chmod 755 /tmp/home/.local/bin/minigw; rm -f /tmp/ran13 /tmp/d13/bin/minigw
  case $bad in file-gw) chmod 775 /tmp/home/.local/bin/minigw ;; dir-ow) chmod 757 /tmp/home/.local/bin ;; owner) chown 1234 /tmp/home/.local/bin/minigw ;;
    up-ow) chmod 777 /tmp/home/.local ;; up-foreign-sticky) chmod 1777 /tmp/home/.local; chown 1234 /tmp/home/.local ;; esac
  out=$(run /tmp/d13 nonexistent testkey); echo "$out"
  [ ! -e /tmp/ran13 ] && [ ! -L /tmp/d13/bin/minigw ] && echo "$out" | grep -q "not using /tmp/home/.local/bin/minigw" || fail "13: unsafe installed minigw used ($bad)"
  rm -rf /tmp/home/.local
done
mkdir -p /tmp/home/.local/bin /tmp/d13/bin; printf "#!/bin/sh\necho minigw $MINV\n" > /tmp/home/.local/bin/minigw; chmod 755 /tmp/home/.local/bin/minigw; trust /tmp/d13 /tmp/home/.local/bin/minigw $MINV
chmod 1777 /tmp/home/.local; rm -f /tmp/d13/bin/minigw
out=$(run /tmp/d13 nonexistent testkey); echo "$out"
[ "$(readlink /tmp/d13/bin/minigw)" = /tmp/home/.local/bin/minigw ] || fail "13: root-owned sticky folder above refused"
rm -rf /tmp/home/.local
echo "ok 13: unsafe ~/.local/bin/minigw refused (#48)"

for u in a b c d; do useradd -m u$u; done
inst() { u=u$1; shift; setpriv --reuid=$u --regid=$u --clear-groups env -i HOME=/home/$u PATH=/t/fakebin:/usr/bin:/bin MINIGW_BASE=file:///t/rel/${REL:-inst} sh /t/install-test.sh "$@"; }
out=$(inst a 2>&1); echo "$out"
cmp -s /t/rel/inst/minigw-linux-arm64 /home/ua/.local/bin/minigw && [ -x /home/ua/.local/bin/minigw-tui ] || fail "12: minigw or minigw-tui not in ~/.local/bin"
grep -q "\"minigw@mini-gateway\": true" /home/ua/.claude/settings.json || fail "12: plugin not added by default"
[ "$(readlink /home/ua/.claude/plugins/data/minigw-mini-gateway/bin/minigw)" = /home/ua/.local/bin/minigw ] || fail "12: plugin not linked to ~/.local/bin/minigw"
inst b --client >/dev/null 2>&1 || fail "12: --client failed"
[ -x /home/ub/.local/bin/minigw ] && [ ! -e /home/ub/.claude/settings.json ] || fail "12: --client added the plugin or no minigw"
inst c --gateway >/dev/null 2>&1 || fail "12: --gateway failed"
[ -f /home/uc/.config/minigw/serve.json ] && [ -f /home/uc/.config/systemd/user/minigw-serve.service ] && [ ! -e /home/uc/.claude/settings.json ] ||
  fail "12: --gateway: no serve.json or systemd unit, or the plugin added"
inst a --purge >/dev/null 2>&1 && fail "12: --purge without --uninstall accepted"
out=$(inst a --uninstall 2>&1); echo "$out"
[ ! -e /home/ua/.local/bin/minigw ] && [ ! -e /home/ua/.local/bin/minigw-tui ] && ! grep -q minigw@mini-gateway /home/ua/.claude/settings.json || fail "12: --uninstall left minigw or the plugin"
inst c --uninstall --purge >/dev/null 2>&1 || fail "12: --uninstall --purge failed"
[ ! -e /home/uc/.config/minigw ] && [ ! -e /home/uc/.config/systemd/user/minigw-serve.service ] && [ ! -e /home/uc/.local/bin/minigw ] || fail "12: --purge left settings, unit or minigw"
out=$(REL=instbad inst d 2>&1); echo "$out"
[ ! -e /home/ud/.local/bin/minigw ] && echo "$out" | grep -q "not signed by Mini Gateway" || fail "12: wrong signature installed"
out=$(env -i PATH=/usr/bin:/bin sh /t/install-test.sh 2>&1); echo "$out" | grep -q "not as root" || fail "12: runs as root"
echo "ok 12: install.sh on Linux: client + plugin, --client, --gateway, --uninstall [--purge], signature"
' || fail "docker cases"
print "plugin install test ok"
