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
#   9. Linux: nameščen ~/.local/bin/minigw, ki ni starejši od izdaje → kopija v vtičnik (brez prenosa); starejši → prenos
#   8. izhod »minigw status« → connected / token rejected / device not approved (ID) / not connected; brez claude: namig (#6)
emulate -L zsh; set -eu
cd ${0:A:h}/..
T=$(mktemp -d); trap 'rm -rf $T' EXIT
fail() { print -u2 "FAIL: $*"; exit 1 }

# 0.5.16 (zadnji commit pred verify-sums) in trenutni minigw za Linux v vsebniku
mkdir -p $T/old $T/rel/good $T/rel/bad $T/rel/evil $T/rel/older $T/home/.config/minigw $T/home/bin
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
print '#!/bin/sh\necho EVIL' > $T/rel/evil/minigw-linux-arm64
(cd $T/rel/evil && shasum -a 256 minigw-linux-arm64 > SHA256SUMS && print garbage > SHA256SUMS.sig)
cp plugins/minigw/scripts/install-minigw.sh $T/real.sh
sed "s|^SIGNER=.*|SIGNER='minigw-release namespaces=\"minigw-release\" $(cut -d' ' -f1,2 $T/key.pub)'|" $T/real.sh > $T/testkey.sh

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
out=$(run /tmp/d9 older testkey); echo "$out"
cmp -s /tmp/home/.local/bin/minigw /tmp/d9/bin/minigw || fail "9: installed minigw not copied over the plugin copy"
! echo "$out" | grep -q "not updated" || fail "9: downgrade note although the installed minigw is used"
cp /t/minigw-old /tmp/home/.local/bin/minigw; cp /t/minigw-old /tmp/d9/bin/minigw
out=$(run /tmp/d9 good testkey); echo "$out"
cmp -s /t/rel/good/minigw-linux-arm64 /tmp/d9/bin/minigw || fail "9: older installed minigw copied instead of the release"
rm -rf /tmp/home/.local
echo "ok 9: installed minigw preferred when not older than the release"
' || fail "docker cases"
print "plugin install test ok"
