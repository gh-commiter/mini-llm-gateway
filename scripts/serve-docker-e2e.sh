#!/bin/zsh
# End-to-end test of »minigw serve« on Linux (Docker) with the Go minigw on this Mac:
# init → connect (unknown device refused) → serve allow → connect → MINIGW_WORK=sync minigw -p → agent's change comes back,
# then a gateway: serve gateway add (key on stdin) → minigw -p with the gateway model → answer from the fake gateway.
# The router runs a fake »claude« that copies src/note.txt to agent.txt and prints valid stream-json, and a fake
# Anthropic-compatible gateway on 127.0.0.1:9000 inside the container.
# Uses its own HOME for minigw and Claude Code, so the user's own connection is not touched.
emulate -L zsh; set -eu
ROOT=${0:A:h:h}
T=$(mktemp -d -t mgw-serve-e2e)
NAME=mgw-serve-e2e
PORT=${PORT:-18765}
# 127.0.0.1 would make minigw skip encryption; a LAN IP needs macOS Local Network permission. The IPv4-mapped
# loopback is encrypted by minigw, and Docker NAT makes the router see a remote peer.
IP='[::ffff:127.0.0.1]'
trap 'docker rm -f $NAME >/dev/null 2>&1 || true' EXIT
fail() { print -u2 "FAIL $*"; docker logs $NAME 2>&1 | tail -30 >&2; exit 1 }

mkdir -p $T/bin $T/cfg $T/home/proj/src
(cd $ROOT/client && GOOS=linux GOARCH=arm64 go build -o $T/bin/minigw . && go build -o $T/minigw .)
cat > $T/bin/claude <<'EOF'
#!/bin/sh
# Fake Claude Code: agent requests run in the synced copy (has src/note.txt); others are chat.
cat > /dev/null
if [ -f src/note.txt ]; then
  { cat src/note.txt; echo "edited on linux"; } > agent.txt
  echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"agent.txt"}}]}}'
fi
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done by fake claude"}}}'
echo '{"type":"result","subtype":"success","usage":{"input_tokens":5,"output_tokens":4}}'
EOF
chmod +x $T/bin/claude
mkdir $T/fakegw
cat > $T/fakegw/main.go <<'EOF'
package main

import (
	"fmt"
	"net/http"
)

func main() {
	http.ListenAndServe("127.0.0.1:9000", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer sk-e2e-gateway-key" {
			http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`, 401)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"echo","context_length":100000},{"id":"other"}]}`)
		case "/v1/messages/count_tokens":
			fmt.Fprint(w, `{"input_tokens":10}`)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			for _, e := range [][2]string{
				{"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"echo","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":0}}}`},
				{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
				{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong from fake gateway"}}`},
				{"content_block_stop", `{"type":"content_block_stop","index":0}`},
				{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`},
				{"message_stop", `{"type":"message_stop"}`},
			} {
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e[0], e[1])
				w.(http.Flusher).Flush()
			}
		}
	}))
}
EOF
(cd $T/fakegw && go mod init fakegw >/dev/null 2>&1 && GOOS=linux GOARCH=arm64 go build -o $T/bin/fakegw .)

dock() { docker run --rm -v $T/bin:/opt/mgw:ro -v $T/cfg:/cfg debian:stable-slim /opt/mgw/minigw "$@" }
LINE=$(dock serve init --config /cfg/serve.json | grep "minigw connect")
TOKEN=${${LINE#*--token \'}%\'}
start() {
  docker rm -f $NAME >/dev/null 2>&1 || true
  docker run -d --name $NAME -p $PORT:8765 -e PATH=/opt/mgw:/usr/bin:/bin -v $T/bin:/opt/mgw:ro -v $T/cfg:/cfg \
    debian:stable-slim sh -c '/opt/mgw/fakegw & exec /opt/mgw/minigw serve --config /cfg/serve.json' >/dev/null
  for i in {1..50}; do curl -s -o /dev/null http://127.0.0.1:$PORT/health/live && return; sleep 0.2; done
  fail "gateway did not start"
}
start
export HOME=$T/home XDG_CONFIG_HOME=$T/home/.config
mg=$T/minigw

# Plain HTTP from another computer (Docker NAT) is refused.
[[ $(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:$PORT/v1/models -H "authorization: Bearer ${TOKEN%%#*}") == 426 ]] || fail "plain HTTP not refused"

# Unknown device: refused, listed as waiting, allowed live with the command from the router log (admin API, no restart).
$mg connect $IP:$PORT --token "$TOKEN" 2>/dev/null && fail "unknown device accepted"
ALLOW=$(docker logs $NAME 2>&1 | sed -n 's/.*to allow it run: \(minigw serve allow [^ ]*\).*/\1/p' | head -1)
[[ -n $ALLOW ]] || fail "no allow hint in the log"
mgr() { docker exec -i $NAME /opt/mgw/minigw "$@" --config /cfg/serve.json }
mgr gw status | grep -q "gateway: running" || fail "gw status"
mgr router status | grep -q "gateway: running" || fail "router alias"
mgr gw devices | grep -q "^waiting: ${ALLOW##* }" || fail "device not waiting"
mgr ${=ALLOW#minigw }
# The admin API answers only on the router computer: from here (Docker NAT) it is refused even with the secret
# (426 for plain HTTP; over an encrypted connection the API itself answers 403, see TestAdminAPIAuth).
[[ $(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:$PORT/minigw/admin/v1/status \
  -H "authorization: Bearer $(<$T/cfg/admin.secret)" -H "host: 127.0.0.1:$PORT") == 426 ]] || fail "admin API reachable from outside"
print -rn -- "$TOKEN" | $mg connect $IP:$PORT --token - | grep -q "(encrypted)" || fail "connect (token from stdin)"
$mg models | grep -q claude-cli-haiku || fail "models"

# Direct mode is refused with a clear message.
cd $T/home/proj
print "pineapple-42" > src/note.txt
out=$(MINIGW_SHARE=1 MINIGW_WORK=direct $mg -p --model claude-cli-haiku "hi" 2>&1 || true)
print -r -- "$out" | grep -q "sync only" || fail "direct mode not refused clearly: $out"
# A folder outside HOME is not shared automatically: minigw warns, the router says so too (not "sync only").
mkdir -p $T/noshare
out=$(cd $T/noshare && MINIGW_WORK=sync $mg -p --model claude-cli-haiku "hi" 2>&1 || true)
print -r -- "$out" | grep -q "is not shared with the gateway (not shared automatically)" || fail "no client warning: $out"
print -r -- "$out" | grep -q "Claude CLI agents cannot work in it" || fail "unshared folder: $out"
print -r -- "$out" | grep -q "sync only" && fail "unshared folder reported as direct mode: $out"

# Sync: real Claude Code here → router in Docker → fake claude edits the copy → change comes back.
out=$(MINIGW_SHARE=1 MINIGW_WORK=sync $mg -p --model claude-cli-haiku "Copy src/note.txt to agent.txt" 2>&1) || fail "minigw -p: $out"
print -r -- "$out" | grep -q "done by fake claude" || fail "answer: $out"
[[ $(<agent.txt) == $'pineapple-42\nedited on linux' ]] || fail "agent.txt not synced back: $(cat agent.txt 2>&1)"
docker exec $NAME sh -c 'cat /root/.cache/minigw/scratch/sync/*/*/src/note.txt' | grep -q pineapple-42 || fail "copy on the router"
docker logs $NAME 2>&1 | grep -q 'step: "Write agent.txt"' || fail "agent step not logged"

# Gateway: http only with --insecure; the key from stdin goes to a 0600 file (no secret-tool here), never serve.json.
# Settings page: the router gives a one-time link; from outside the container it is refused (login and CSRF: Go tests).
LINK=$(mgr gw settings link | tail -1 | tr -d ' ')
[[ $LINK == http://127.0.0.1:8765/minigw/settings\#t=* ]] || fail "settings link: $LINK"  # žeton v fragmentu
[[ $(curl -s -o /dev/null -w "%{http_code}" "${LINK/127.0.0.1:8765/127.0.0.1:$PORT}") == 426 ]] || fail "settings link reachable from outside"
# Added and changed live through the admin API: no restart.
print sk-e2e-gateway-key | mgr serve gateway add fake http://127.0.0.1:9000 2>/dev/null && fail "http gateway without --insecure accepted"
print sk-e2e-gateway-key | mgr gw provider add fake http://127.0.0.1:9000 --insecure | grep -q "2 models, 2 enabled" || fail "provider add"
mgr gw provider models fake --disable other | grep -q '^\[ \] other' || fail "provider models"
grep -q sk-e2e $T/cfg/serve.json && fail "API key in serve.json"
[[ $(stat -f %Lp $T/cfg/gateway-keys.json) == 600 ]] || fail "key file mode"
$mg models | grep -q claude-echo || fail "gateway model not listed"
$mg models | grep -q claude-other && fail "disabled gateway model listed"
out=$(MINIGW_WORK=sync $mg -p --model claude-echo "ping" 2>&1) || fail "minigw -p gateway: $out"
print -r -- "$out" | grep -q "pong from fake gateway" || fail "gateway answer: $out"
docker logs $NAME 2>&1 | grep -q 'model="claude-echo" status=200' || fail "gateway request not logged"
docker logs $NAME 2>&1 | grep -q 'provider fake: API key: WARNING: no Secret Service' || fail "no key file warning"
docker logs $NAME 2>&1 | grep -q sk-e2e && fail "API key in the log"
print "serve e2e ok"
