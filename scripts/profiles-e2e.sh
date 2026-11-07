#!/bin/zsh
# E2E za profile (#17) na tem Macu: odjemalec Go (lasten HOME, datoteke, ne Keychain) z dvema prehodoma hkrati:
#   default = aplikacija Mini Gateway na 127.0.0.1:8765 (žeton z »Copy Token« prek Accessibility, nikoli izpisan)
#   serve   = minigw serve na »localhost.« (šifrirano prek Noise, a po zanki: macOS za nepodpisan binarni Go
#             zapre pot do LAN IP tega Maca, Local Network); mapa v načinu sync z ključem naprave
# Lažen claude kliče lokalni usmerjevalnik: združeni modeli, claude-cli-haiku@serve (pravi claude na serve, nekaj
# žetonov), napačen žeton 401. Nato serve ugasne: models in status ga izpustita, privzeti dela. Nato disconnect.
#   profiles-e2e.sh
emulate -L zsh; set -eu
cd ${0:A:h}/..
T=${$(mktemp -d):A}; SPID=""
S2PID=""
trap '[[ -n $SPID ]] && kill $SPID 2>/dev/null; [[ -n $S2PID ]] && kill $S2PID 2>/dev/null; pbcopy </dev/null; rm -rf $T' EXIT
fail() { print -u2 "FAIL: $*"; exit 1 }
(cd client && go build -o $T/minigw .)
M=$T/minigw; PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')
H=$T/home; mkdir -p $H/proj $H/bin; print hello > $H/proj/a.txt
gw() { HOME=$H "$M" "$@" }

# serve: lastna konfiguracija in vrata
"$M" serve init --config $T/serve/serve.json --port $PORT > $T/init.txt
TOKEN=$(grep -o "\-\-token '[^']*'" $T/init.txt | cut -d"'" -f2) || fail "serve init: no token"
"$M" serve --config $T/serve/serve.json > $T/serve.log 2>&1 &
SPID=$!
for i in {1..50}; do curl -s -o /dev/null http://127.0.0.1:$PORT/health/live && break; sleep 0.2; done
kill -0 $SPID 2>/dev/null || fail "serve did not start: $(<$T/serve.log)"

# default: aplikacija (Copy Token v meniju; odložišče se izprazni). Brez menija (aplikacija ni dostopna prek
# Accessibility) drug lokalni minigw serve na 127.0.0.1 s ključem (L3: tudi lokalno šifrirano).
DEFURL=http://127.0.0.1:8765
if osascript -e 'tell application "System Events" to tell (first process whose bundle identifier is "dev.trojar.minigateway")' \
     -e 'click menu bar item 1 of menu bar 2' -e 'delay 0.4' \
     -e 'click menu item "Copy Token" of menu 1 of menu bar item 1 of menu bar 2' -e 'end tell' >/dev/null 2>&1; then
  sleep 0.5
  pbpaste | gw connect 127.0.0.1 --token - > $T/c1.txt || fail "connect app: $(<$T/c1.txt)"
  pbcopy </dev/null
else
  print "(the app's menu is not reachable through Accessibility: a second minigw serve is the default gateway)"
  P2=$(python3 -c 'import socket; s=socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')
  "$M" serve init --config $T/serve2/serve.json --port $P2 > $T/init2.txt
  "$M" serve --config $T/serve2/serve.json > $T/serve2.log 2>&1 &
  S2PID=$!
  for i in {1..50}; do curl -s -o /dev/null http://127.0.0.1:$P2/health/live && break; sleep 0.2; done
  grep -o "\-\-token '[^']*'" $T/init2.txt | cut -d"'" -f2 | gw connect 127.0.0.1:$P2 --token - > $T/c1.txt || fail "connect serve2: $(<$T/c1.txt)"
  DEFURL=http://127.0.0.1:$P2
fi
[[ $(<$T/c1.txt) == "Connected to $DEFURL (encrypted)" ]] || fail "first connect output changed: $(<$T/c1.txt)"

# serve: šifrirano (ime ni 127.0.0.1/localhost, zato Noise); po zanki naprava ne čaka na odobritev
print -r -- $TOKEN | gw connect localhost.:$PORT --name serve --token - > $T/c3.txt 2>&1 || fail "connect serve: $(<$T/c3.txt)"
grep -q "(encrypted) as profile serve" $T/c3.txt || fail "connect serve: $(<$T/c3.txt)"
[[ $(gw profiles) == "* default  $DEFURL"$'\n'"  serve  http://localhost.:$PORT" ]] || fail "profiles: $(gw profiles)"
# en ključ naprave za oba profila
CFG="$H/Library/Application Support/minigw"
[[ $(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["device"]==json.load(open(sys.argv[2]))["device"])' $CFG/connection.json $CFG/connections/serve.json) == True ]] || fail "device keys differ"
print "connect ok"

gw status > $T/st.txt || fail "status: $(<$T/st.txt)"
[[ $(grep -c '(reachable' $T/st.txt) == 2 && $(grep -c '^Profile:' $T/st.txt) == 2 ]] || fail "status: $(<$T/st.txt)"
gw models > $T/models.txt
grep -q '^claude-cli-haiku@serve ' $T/models.txt && grep -q '@default ' $T/models.txt || fail "models: $(<$T/models.txt)"
grep -q '^claude-cli-haiku@default ' $T/models.txt && ! grep -q '^claude-cli-haiku ' $T/models.txt && fail "default models missing without suffix"
print "status models ok"

# Seja: lažen claude prek lokalnega usmerjevalnika
cat > $H/bin/claude <<'EOF'
#!/bin/zsh
hdr=(); for l in ${(f)ANTHROPIC_CUSTOM_HEADERS}; hdr+=(-H $l)
A=(-s -H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" $hdr)
print -r -- $ANTHROPIC_BASE_URL > $E2E/base
curl $A $ANTHROPIC_BASE_URL/v1/models > $E2E/models.json
curl $A -H 'content-type: application/json' -d '{"model":"claude-cli-haiku@serve","max_tokens":64,"messages":[{"role":"user","content":"Reply with exactly the word pong and nothing else."}]}' $ANTHROPIC_BASE_URL/v1/messages > $E2E/serve.json
curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer wrong" $ANTHROPIC_BASE_URL/v1/models > $E2E/badauth
EOF
chmod +x $H/bin/claude
(cd $H/proj && E2E=$T PATH=$H/bin:$PATH MINIGW_WORK=sync gw -p hi > $T/run.txt 2>&1) || fail "session: $(<$T/run.txt)"
grep -q '2 gateways' $T/run.txt || fail "no router: $(<$T/run.txt)"
[[ $(<$T/base) == http://127.0.0.1:3* ]] || fail "base URL $(<$T/base)"
python3 - $T/models.json <<'EOF' || fail "session models: $(head -c 600 $T/models.json)"
import json, sys
ids = [m["id"] for m in json.load(open(sys.argv[1]))["data"]]
assert "claude-cli-haiku@serve" in ids and any(i.endswith("@default") for i in ids), ids
EOF
grep -qi pong $T/serve.json || fail "claude-cli-haiku@serve: $(head -c 600 $T/serve.json)"
[[ $(<$T/badauth) == 401 ]] || fail "wrong token: $(<$T/badauth)"
grep -q 'refused\|not allowed' $T/serve.log && fail "serve refused: $(grep -m3 'refused\|not allowed' $T/serve.log)"
print "session ok (claude-cli-haiku@serve answered through the synced folder)"

# serve ugasne: izpade iz seznama, status ga pokaže, privzeti dela naprej (izhod 0)
kill $SPID; wait $SPID 2>/dev/null || true; SPID=""
gw models > $T/m2.txt 2> $T/m2.err || fail "models with serve down: $(<$T/m2.err)"
grep -q '@serve' $T/m2.txt && fail "unreachable gateway listed"
grep -q 'serve .*not reachable' $T/m2.err || fail "no note: $(<$T/m2.err)"
gw status > $T/st2.txt || fail "status exit with only serve down: $(<$T/st2.txt)"
grep -q 'unreachable' $T/st2.txt || fail "status: $(<$T/st2.txt)"
print "unreachable ok"

gw disconnect --name serve | grep -q 'Disconnected profile serve' || fail "disconnect"
gw status > $T/st3.txt; grep -q '^Profile:' $T/st3.txt && fail "single profile output changed: $(<$T/st3.txt)"
print "profiles e2e ok"
