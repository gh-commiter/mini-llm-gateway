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
#  11. M1 Linux: nameščen minigw 0.5.19 (≥ MIN), podpisana izdaja 0.5.99 → povezava; brez enote systemd namig za
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
(cd $T/rel/newer && { print "VERSION 0.5.99"; shasum -a 256 minigw-linux-arm64; } > SHA256SUMS && ssh-keygen -Y sign -q -f $T/key -n minigw-release < SHA256SUMS > SHA256SUMS.sig)
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

cp plugins/minigw/scripts/install-minigw.sh $T/real.sh
sed "s|^SIGNER=.*|SIGNER='minigw-release namespaces=\"minigw-release\" $(cut -d' ' -f1,2 $T/key.pub)'|" $T/real.sh > $T/testkey.sh

# 10: Windows brez Windows: lažna uname (MINGW64) in cygpath (-u: ista pot, -m/-w: C: spredaj); pot z ' in %.
W="$T/win/O'Brien 100%"; mkdir -p $T/win/fake "$W/Programs/Mini Gateway" $T/win/data/bin $T/win/home
print '#!/bin/sh\ncase $1 in -s) echo MINGW64_NT-10.0 ;; *) echo x86_64 ;; esac' > $T/win/fake/uname
print '#!/bin/sh\ncase $1 in -u) printf "%s\\n" "$2" ;; -m) printf "C:%s\\n" "$2" ;; -w) printf "C:%s\\n" "$2" | tr / "\\\\" ;; esac' > $T/win/fake/cygpath
print '#!/bin/sh\n[ "$1" = --version ] && echo minigw 0.5.20; [ "$1" = status ] && echo "Status:    connected"; exit 0' > "$W/Programs/Mini Gateway/minigw.exe"
chmod +x $T/win/fake/* "$W/Programs/Mini Gateway/minigw.exe"; print old > $T/win/data/bin/minigw.exe
winrun() { CLAUDE_PLUGIN_DATA=$T/win/data CLAUDE_ENV_FILE= LOCALAPPDATA=$W APPDATA=$T/win/roaming MINIGW_RELEASE_BASE=file://$T/rel/${1:-nonexistent} HOME=$T/win/home PATH=$T/win/fake:$PATH sh $T/testkey.sh }
out=$(winrun newer); print -r -- $out
[[ $out == *"minigw 0.5.20 is installed, 0.5.99 is available: run the installer https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/minigw-windows-amd64.msi (check it first: docs/install.md, Check a download)"* && $out != *"/minigw:gw update"* ]] ||
  fail "10: no MSI update hint for a client-only install"
mkdir -p $T/win/roaming/minigw; print '{}' > $T/win/roaming/minigw/serve.json
out=$(winrun newer); print -r -- $out
[[ $out == *"Mini Gateway 0.5.20 is installed, 0.5.99 is available: /minigw:gw update"* ]] || fail "10: no update hint (M1)"
exe="$W/Programs/Mini Gateway/minigw.exe" sq="'" rep="'\''"
[[ "$(<$T/win/data/bin/minigw)" == "#!/bin/sh"$'\n'"# Mini Gateway: the installed minigw"$'\n'"exec 'C:${exe//$sq/$rep}' \"\$@\"" ]] || fail "10: sh shim: $(<$T/win/data/bin/minigw)"
[[ "$(<$T/win/data/bin/minigw.cmd)" == "@setlocal DisableDelayedExpansion"$'\r\n'"@\"%LOCALAPPDATA%\\Programs\\Mini Gateway\\minigw.exe\" %*"$'\r' ]] || fail "10: cmd shim: $(<$T/win/data/bin/minigw.cmd)"
[[ ! -e $T/win/data/bin/minigw.exe && $out == *"connected to Mini Gateway"* ]] || fail "10: plugin copy kept or status not read"
rm "$exe"; out=$(winrun); print -r -- $out
[[ ! -e $T/win/data/bin/minigw && ! -e $T/win/data/bin/minigw.cmd ]] || fail "10: shims kept without an installed minigw"
print "ok 10: Windows shims point at the installed minigw; client-only MSI gets the MSI hint"

docker run --rm -v $T:/t:ro debian:stable-slim sh -c '
set -u
apt-get update -qq >/dev/null 2>&1; apt-get install -y -qq curl >/dev/null 2>&1
fail() { echo "FAIL: $*"; exit 1; }
run() { CLAUDE_PLUGIN_DATA=$1 CLAUDE_ENV_FILE= MINIGW_RELEASE_BASE=file:///t/rel/$2 HOME=/tmp/home PATH=/tmp/home/bin:$PATH sh /t/$3.sh; }
cp -r /t/home /tmp/home

mkdir -p /tmp/d1/bin; cp /t/minigw-old /tmp/d1/bin/minigw
out=$(run /tmp/d1 evil real); echo "$out"
[ "$(/tmp/d1/bin/minigw --version | head -1)" = "minigw 0.5.16" ] || fail "1: old minigw replaced"
echo "$out" | grep -q "not updated: cannot check the release signature" || fail "1: no reason printed"
echo "ok 1: 0.5.16 minigw does not verify a garbage signature"

out=$(run /tmp/d2 good real); echo "$out"
[ ! -e /tmp/d2/bin/minigw ] || fail "2: installed without a signature check"
echo "ok 2: first install without ssh-keygen refused"

apt-get install -y -qq openssh-client >/dev/null 2>&1
out=$(run /tmp/d3 good testkey); echo "$out"
[ -x /tmp/d3/bin/minigw ] || fail "3: not installed with a good signature"
echo "ok 3: good signature installs"

mkdir -p /tmp/d4/bin; cp /t/minigw-old /tmp/d4/bin/minigw
out=$(run /tmp/d4 bad testkey); echo "$out"
[ "$(/tmp/d4/bin/minigw --version | head -1)" = "minigw 0.5.16" ] || fail "4: replaced despite a wrong signature"
echo "$out" | grep -q "not updated: the release.s SHA256SUMS is not signed" || fail "4: no reason printed"
echo "ok 4: wrong namespace refused, reason printed"

new=$(/tmp/d3/bin/minigw --version | head -1)
mkdir -p /tmp/d5/bin; cp /tmp/d3/bin/minigw /tmp/d5/bin/minigw
out=$(run /tmp/d5 older testkey); echo "$out"
[ "$(/tmp/d5/bin/minigw --version | head -1)" = "$new" ] || fail "5: rolled back to an older signed release"
echo "$out" | grep -q "not updated: the latest release (0.5.16) is older than the installed" || fail "5: no reason printed"
echo "ok 5: older signed release refused (#25)"

out=$(MINIGW_ALLOW_DOWNGRADE=1 run /tmp/d5 older testkey); echo "$out"
[ "$(/tmp/d5/bin/minigw --version | head -1)" = "minigw 0.5.16" ] || fail "6: MINIGW_ALLOW_DOWNGRADE=1 did not install"
echo "ok 6: downgrade with MINIGW_ALLOW_DOWNGRADE=1"

mkdir -p /tmp/d7/bin; printf "#!/bin/sh\necho minigw 0.5.19\n" > /tmp/d7/bin/minigw; chmod +x /tmp/d7/bin/minigw
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
chmod +x /tmp/d8/bin/minigw
ID=AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCdE
st() { printf "%s\n" "$1" > /tmp/status.out; echo $2 > /tmp/status.rc; out=$(run /tmp/d8 nonexistent real); echo "$out"; }
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
rm /tmp/home/bin/claude; out=$(PATH=/tmp/home/bin:/usr/bin:/bin run /tmp/d8 nonexistent real); echo "$out"
echo "$out" | grep -q "standalone claude command on PATH" || fail "8: no claude hint"
echo "ok 8: hook reports status reasons and a missing claude"

mkdir -p /tmp/home/.local/bin; cp /t/rel/good/minigw-linux-arm64 /tmp/home/.local/bin/minigw
mkdir -p /tmp/d9/bin; cp /t/minigw-old /tmp/d9/bin/minigw
for rel in older good; do
  out=$(run /tmp/d9 $rel testkey); echo "$out"
  [ "$(readlink /tmp/d9/bin/minigw)" = /tmp/home/.local/bin/minigw ] || fail "9: plugin bin not linked to the installed minigw ($rel)"
  ! echo "$out" | grep -q "not updated" || fail "9: downgrade note although the installed minigw is used"
done
cp /t/minigw-old /tmp/home/.local/bin/minigw
out=$(run /tmp/d9 good testkey); echo "$out"
[ ! -L /tmp/d9/bin/minigw ] && cmp -s /t/rel/good/minigw-linux-arm64 /tmp/d9/bin/minigw || fail "9: minigw older than MIN linked instead of the release"
rm -rf /tmp/home/.local
echo "ok 9: installed minigw (not older than MIN) linked, never downloaded"

mkdir -p /tmp/home/.local/bin /tmp/d11/bin; printf "#!/bin/sh\necho minigw 0.5.20\n" > /tmp/home/.local/bin/minigw; chmod +x /tmp/home/.local/bin/minigw
out=$(run /tmp/d11 newer testkey); echo "$out"
[ "$(readlink /tmp/d11/bin/minigw)" = /tmp/home/.local/bin/minigw ] || fail "11: not linked to the installed minigw"
echo "$out" | grep -q "minigw 0.5.20 is installed, 0.5.99 is available: run curl -fsSL https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/install.sh | sh (or replace /tmp/home/.local/bin/minigw with minigw-linux-arm64 from the latest release" || fail "11: no client-only update hint (M1)"
mkdir -p /tmp/home/.config/systemd/user; : > /tmp/home/.config/systemd/user/minigw-serve.service
out=$(run /tmp/d11 newer testkey); echo "$out"
echo "$out" | grep -q "Mini Gateway 0.5.20 is installed, 0.5.99 is available: /minigw:gw update" || fail "11: no gateway update hint"
rm -rf /tmp/home/.config/systemd
rm -rf /tmp/home/.local
echo "ok 11: newer signed release than the installed minigw: hint, no download"

# 13 (#48): ~/.local/bin/minigw, ki ni last uporabnika ali je (z mapo) zapisljiv za skupino ali druge, se ne zažene in ne poveže
for bad in file-gw dir-ow owner; do
  mkdir -p /tmp/home/.local/bin /tmp/d13/bin; printf "#!/bin/sh\ntouch /tmp/ran13; echo minigw 0.5.20\n" > /tmp/home/.local/bin/minigw
  chmod 755 /tmp/home/.local/bin/minigw; rm -f /tmp/ran13 /tmp/d13/bin/minigw
  case $bad in file-gw) chmod 775 /tmp/home/.local/bin/minigw ;; dir-ow) chmod 757 /tmp/home/.local/bin ;; owner) chown 1234 /tmp/home/.local/bin/minigw ;; esac
  out=$(run /tmp/d13 nonexistent testkey); echo "$out"
  [ ! -e /tmp/ran13 ] && [ ! -L /tmp/d13/bin/minigw ] && echo "$out" | grep -q "not using /tmp/home/.local/bin/minigw" || fail "13: unsafe installed minigw used ($bad)"
  rm -rf /tmp/home/.local
done
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
