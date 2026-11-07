#!/bin/zsh
# profiles-swift-e2e.sh (po ./build.sh): podpisan Swift minigw (build.sh) with a throwaway profile e2e-tmp in its Keychain (app's SecItem code, no
# `security` CLI) against a local minigw serve; removes the profile at the end. Every minigw call has a 30 s alarm:
# exit 142 = probably a Keychain prompt (stop).
emulate -L zsh; set -eu
W=${0:A:h}/..
SM="$W/build/Mini Gateway.app/Contents/Helpers/minigw"
T=${$(mktemp -d):A}; SPID=""
cleanup() {
  perl -e 'alarm shift; exec @ARGV' 30 "$SM" disconnect --name e2e-tmp >/dev/null 2>&1 || true
  [[ -n $SPID ]] && kill $SPID 2>/dev/null; rm -rf $T
}
trap cleanup EXIT
fail() { print -u2 "FAIL: $*"; exit 1 }
sm() { perl -e 'alarm shift; exec @ARGV' 30 "$SM" "$@" }
(cd $W/client && go build -o $T/gw .)
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')
"$T/gw" serve init --config $T/serve/serve.json --port $PORT > $T/init.txt
python3 -c 'import json,sys; p=sys.argv[1]; c=json.load(open(p)); c["scratch"]=sys.argv[2]; json.dump(c,open(p,"w"))' $T/serve/serve.json $T/scratch
"$T/gw" serve --config $T/serve/serve.json > $T/serve.log 2>&1 &
SPID=$!
for i in {1..50}; do curl -s -o /dev/null http://127.0.0.1:$PORT/health/live && break; sleep 0.2; done
before=$(sm profiles) || fail "profiles: rc $?"

grep -o "\-\-token '[^']*'" $T/init.txt | cut -d"'" -f2 | sm connect localhost.:$PORT --name e2e-tmp --token - > $T/c.txt 2>&1 || fail "connect rc $?: $(<$T/c.txt)"
grep -q "(encrypted) as profile e2e-tmp" $T/c.txt || fail "connect: $(<$T/c.txt)"
sm profiles | grep -q "^  e2e-tmp  http://localhost.:$PORT" || fail "profiles: $(sm profiles)"
sm status > $T/st.txt || true
grep -q '^Profile:   e2e-tmp' $T/st.txt && grep -q "localhost.:$PORT (reachable, encrypted)" $T/st.txt || fail "status: $(<$T/st.txt)"
sm models > $T/m.txt 2>&1 || fail "models: $(<$T/m.txt)"
grep -q '^claude-cli-haiku@e2e-tmp ' $T/m.txt || fail "models: $(<$T/m.txt)"
print "connect profiles status models ok"

# Session through the Swift ProfileMux: chat model first (no share), then an agent model (sync share with e2e-tmp).
mkdir -p $T/bin $T/proj; print hello > $T/proj/a.txt
cat > $T/bin/claude <<'EOF'
#!/bin/zsh
hdr=(); for l in ${(f)ANTHROPIC_CUSTOM_HEADERS}; hdr+=(-H $l)
A=(-s -H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" $hdr -H 'content-type: application/json')
print -r -- $ANTHROPIC_BASE_URL > $E2E/base
curl $A $ANTHROPIC_BASE_URL/v1/models > $E2E/models.json
curl $A -d '{"model":"claude-cli-haiku@e2e-tmp","max_tokens":64,"messages":[{"role":"user","content":"Reply with exactly the word pong and nothing else."}]}' $ANTHROPIC_BASE_URL/v1/messages > $E2E/agent.json
curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer wrong" $ANTHROPIC_BASE_URL/v1/models > $E2E/badauth
EOF
chmod +x $T/bin/claude
(cd $T/proj && E2E=$T PATH=$T/bin:$PATH MINIGW_SHARE=1 MINIGW_WORK=sync perl -e 'alarm shift; exec @ARGV' 300 "$SM" -p hi > $T/run.txt 2>&1) || fail "session: $(<$T/run.txt)"
grep -q '2 gateways' $T/run.txt || fail "no local proxy: $(<$T/run.txt)"
grep -q '"claude-cli-haiku@e2e-tmp"' $T/models.json || fail "session models: $(head -c 400 $T/models.json)"
grep -qi pong $T/agent.json || fail "agent@e2e-tmp: $(head -c 600 $T/agent.json) / $(tail -5 $T/serve.log)"
[[ $(<$T/badauth) == 401 ]] || fail "wrong token: $(<$T/badauth)"
print "session ok (Swift local proxy, claude-cli-haiku@e2e-tmp through the synced folder)"

sm disconnect --name e2e-tmp | grep -q 'Disconnected profile e2e-tmp' || fail "disconnect"
[[ $(sm profiles) == $before ]] || fail "profiles changed: $(sm profiles)"
print "swift profiles e2e ok (profiles back to: $before)"
