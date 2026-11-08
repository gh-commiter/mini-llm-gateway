#!/bin/zsh
# E2E: agent vpraša drug model istega prehoda (minigw serve, Go) na tem Macu ali Linuxu. Lokalni serve na prostih vratih,
# odjemalec Go z lastnim HOME, šifrirano prek »localhost.«, način sync. »claude« prehoda je lažen agent, ki z minigw -p
# (MINIGW_AGENT_URL, prazen HOME: brez profilov in brez Claude Code) vpraša claude-codex; lažen »codex« zapiše codex.txt v isto mapo in
# vpraša claude-cli (zanka).
# Preveri: codex dela v mapi klicatelja in datoteka pride k odjemalcu; zanka je zavrnjena (403); minigw v agentu zavrne
# connect; s stikalom izklopljenim agent spremenljivk MINIGW_AGENT_* nima.
#   agent-calls-e2e.sh
emulate -L zsh; set -eu
cd ${0:A:h}/..
T=${$(mktemp -d):A}; SPID=""
trap '[[ -n $SPID ]] && kill $SPID 2>/dev/null; [[ -n ${KEEP:-} ]] && print -u2 "kept $T" || rm -rf $T' EXIT
fail() { print -u2 "FAIL: $*"; exit 1 }
(cd client && go build -o $T/minigw .)
M=$T/minigw; H=$T/home; CFG=$T/serve/serve.json
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')
mkdir -p $H/bin $T/gwbin $T/empty
gw() { HOME=$H "$M" "$@" }

# Prehod: claude vpraša codex; codex piše v mapo in vpraša claude (zanka).
ask() { print -r -- "HTTP_PROXY=\${E2E_PROXY:-} HOME=$T/empty PATH=/usr/bin:/bin \${E2E_ASKER:-$M} -p --model $1 go > $T/$2.out 2> $T/$2.err; echo \$? > $T/$2.rc" }
cat > $T/gwbin/claude <<EOF
#!/bin/sh
[ "\$1" = "--version" ] && { echo "2.1.99 (Claude Code)"; exit 0; }
cat > /dev/null
env | grep '^MINIGW_AGENT_' | cut -d= -f1 | sort > $T/claude-env
if [ -n "\$MINIGW_AGENT_URL" ]; then
  $(ask claude-codex asked)
  HOME=$T/empty $M connect localhost:1 --token x > /dev/null 2> $T/connect.err; echo \$? > $T/connect.rc
fi
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
echo '{"type":"result","subtype":"success","usage":{"output_tokens":1}}'
EOF
cat > $T/gwbin/codex <<EOF
#!/bin/sh
if [ "\$1" = debug ]; then echo '{"models":[]}'; exit 0; fi
out=""; prev=""
for a in "\$@"; do [ "\$prev" = "-o" ] && out="\$a"; prev="\$a"; done
cat > /dev/null
pwd > $T/codex-dir
echo codex > codex.txt
$(ask claude-cli-haiku loop)
echo '{"type":"thread.started","thread_id":"t1"}'
echo "codex answer" > "\$out"
EOF
chmod +x $T/gwbin/claude $T/gwbin/codex
"$M" serve init --config $CFG --port $PORT > $T/init.txt
TOKEN=$(grep -o "\-\-token '[^']*'" $T/init.txt | cut -d"'" -f2)
conf() {  # serve.json: Scratch, lažna agenta, stikali »May ask other models« ($1 claude, $2 codex)
  python3 - $CFG $T/scratch $T/gwbin $1 $2 <<'PY'
import json, sys, platform
p, scratch, bin, claude, codex = sys.argv[1:]
c = json.load(open(p))
c.update(scratch=scratch, claude=bin + "/claude", claudeAskOthers=claude == "1", allowLocalTools=True)
c["allowUnsandboxedBash"] = platform.system() != "Linux"  # Codex brez bwrap le z izrecnim dovoljenjem
c["codex"] = {"enabled": True, "executable": bin + "/codex", "models": None, "askOthers": codex == "1"}
json.dump(c, open(p, "w"))
PY
}
start() {  # $1, $2: E2E_ASKER (minigw v agentu), E2E_PROXY (posrednik kot v sandboxu Claude Code)
  E2E_ASKER=${1:-} E2E_PROXY=${2:-} "$M" serve --config $CFG > $T/serve.log 2>&1 &
  SPID=$!
  for i in {1..50}; do curl -s -o /dev/null http://127.0.0.1:$PORT/health/live && break; sleep 0.2; done
  kill -0 $SPID 2>/dev/null || fail "serve did not start: $(<$T/serve.log)"
}
stop() { kill $SPID; wait $SPID 2>/dev/null || true; SPID="" }
conf 1 1; start
print -r -- $TOKEN | gw connect localhost.:$PORT --token - >/dev/null || fail "connect"

# Odjemalec: lažen claude pošlje eno zahtevo z orodji.
cat > $H/bin/claude <<'EOF'
#!/bin/zsh
hdr=(); for l in ${(f)ANTHROPIC_CUSTOM_HEADERS}; hdr+=(-H $l)
code=$(curl -s -o $E2E/resp -w '%{http_code}' $ANTHROPIC_BASE_URL/v1/messages -H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" $hdr \
  -H 'content-type: application/json' -d '{"model":"claude-cli-haiku","max_tokens":50,"tools":[{"name":"x","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"go"}]}')
[[ $code == 200 ]] || { print -u2 "messages: HTTP $code $(head -c 400 $E2E/resp)"; exit 1 }
EOF
chmod +x $H/bin/claude
C=$H/client; mkdir -p $C; print one > $C/a.txt
run() { (cd $C && E2E=$T PATH=$H/bin:$PATH MINIGW_WORK=sync gw -p go) 2> $T/err || fail "minigw: $(<$T/err) $(tail -5 $T/serve.log)" }

# a: claude → codex v isti mapi; codex → claude zavrnjen; connect v agentu zavrnjen
run
[[ $(<$T/claude-env) == $'MINIGW_AGENT_AUTH\nMINIGW_AGENT_URL' ]] || fail "a: claude env: $(<$T/claude-env)"
[[ $(<$T/asked.rc) == 0 && $(<$T/asked.out) == "codex answer" ]] || fail "a: claude → codex: rc $(<$T/asked.rc) $(<$T/asked.out) $(<$T/asked.err)"
[[ $(<$C/codex.txt) == codex ]] || fail "a: codex.txt did not come back to the client (codex worked in $(<$T/codex-dir))"
[[ $(<$T/loop.rc) != 0 ]] && grep -q "an agent asked by another agent cannot ask further models (claude → codex)" $T/loop.err \
  || fail "a: loop not refused: rc $(<$T/loop.rc) $(<$T/loop.err)"
[[ $(<$T/connect.rc) != 0 ]] && grep -q "inside an agent of the gateway" $T/connect.err || fail "a: connect in an agent: $(<$T/connect.err)"
print "a ok (codex in $(<$T/codex-dir))"

# c: prek posrednika s prijavo (kot sandbox Claude Code: loopback le prek HTTP_PROXY); Go in na macOS še Swift minigw
cat > $T/proxy.py <<'PY'
import base64, http.client, sys, urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
class H(BaseHTTPRequestHandler):
    def do(self):
        if self.headers.get("Proxy-Authorization") != "Basic " + base64.b64encode(b"u:p@").decode():
            self.send_response(407); self.end_headers(); return
        u = urllib.parse.urlsplit(self.path)
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        h = {k: v for k, v in self.headers.items() if not k.lower().startswith("proxy-") and k.lower() != "connection"}
        c = http.client.HTTPConnection(u.hostname, u.port, timeout=120)
        c.request(self.command, u.path, body, h)
        r = c.getresponse()
        self.send_response(r.status)
        for k, v in r.getheaders():
            if k.lower() not in ("transfer-encoding", "connection", "content-length"): self.send_header(k, v)
        self.end_headers()
        while chunk := r.read1(4096): self.wfile.write(chunk); self.wfile.flush()
        open(sys.argv[2], "a").write(u.path + "\n")
    do_GET = do_POST = do
    def log_message(self, *a): pass
ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
PPORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')
python3 $T/proxy.py $PPORT $T/proxied & PPID2=$!
trap '[[ -n $SPID ]] && kill $SPID 2>/dev/null; kill $PPID2 2>/dev/null; [[ -n ${KEEP:-} ]] && print -u2 "kept $T" || rm -rf $T' EXIT
sleep 0.5
askers=($M); [[ $(uname) == Darwin ]] && { swift build --product MiniGW > /dev/null 2>&1 && askers+=(${$(swift build --show-bin-path)}/MiniGW) || fail "swift build MiniGW" }
for asker in $askers; do
  stop; start $asker http://u:p%40@127.0.0.1:$PPORT
  rm -f $T/asked.* $T/loop.* $T/proxied $C/codex.txt
  run
  [[ $(<$T/asked.out) == "codex answer" ]] || fail "c ($asker): claude → codex via proxy: $(<$T/asked.out) $(<$T/asked.err)"
  grep -q "an agent asked by another agent cannot ask further models" $T/loop.err || fail "c ($asker): loop via proxy: $(<$T/loop.err)"
  [[ $(grep -c /v1/messages $T/proxied) == 2 ]] || fail "c ($asker): requests did not go through the proxy: $(cat $T/proxied 2>/dev/null)"
  print "c ok (${asker:t}, proxy)"
done

# b: stikalo izklopljeno → agent nima MINIGW_AGENT_*
stop; conf 0 0; start
rm -f $T/asked.rc $C/codex.txt
run
[[ -z $(<$T/claude-env) && ! -e $T/asked.rc ]] || fail "b: env with the switch off: $(<$T/claude-env)"
print "b ok"
print "agent calls e2e ok"
