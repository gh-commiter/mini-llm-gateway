#!/bin/zsh
# End-to-end test of »minigw serve« on Linux (Docker) with the Go minigw on this Mac:
# init → connect (unknown device refused) → serve allow → connect → MINIGW_WORK=sync minigw -p → agent's change comes back.
# The router runs a fake »claude« that copies src/note.txt to agent.txt and prints valid stream-json.
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

dock() { docker run --rm -v $T/bin:/opt/mgw:ro -v $T/cfg:/cfg debian:stable-slim /opt/mgw/minigw "$@" }
LINE=$(dock serve init --config /cfg/serve.json | grep "minigw connect")
TOKEN=${${LINE#*--token \'}%\'}
start() {
  docker rm -f $NAME >/dev/null 2>&1 || true
  docker run -d --name $NAME -p $PORT:8765 -e PATH=/opt/mgw:/usr/bin:/bin -v $T/bin:/opt/mgw:ro -v $T/cfg:/cfg \
    debian:stable-slim /opt/mgw/minigw serve --config /cfg/serve.json >/dev/null
  for i in {1..50}; do curl -s -o /dev/null http://127.0.0.1:$PORT/health/live && return; sleep 0.2; done
  fail "router did not start"
}
start
export HOME=$T/home XDG_CONFIG_HOME=$T/home/.config
mg=$T/minigw

# Plain HTTP from another computer (Docker NAT) is refused.
[[ $(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:$PORT/v1/models -H "authorization: Bearer ${TOKEN%%#*}") == 426 ]] || fail "plain HTTP not refused"

# Unknown device: refused; allowed with the command from the router log.
$mg connect $IP:$PORT --token "$TOKEN" 2>/dev/null && fail "unknown device accepted"
ALLOW=$(docker logs $NAME 2>&1 | sed -n 's/.*to allow it run: \(minigw serve allow [^ ]*\).*/\1/p' | head -1)
[[ -n $ALLOW ]] || fail "no allow hint in the log"
dock ${=ALLOW#minigw } --config /cfg/serve.json
start
$mg connect $IP:$PORT --token "$TOKEN" | grep -q "(encrypted)" || fail "connect"
$mg models | grep -q claude-cli-haiku || fail "models"

# Direct mode is refused with a clear message.
cd $T/home/proj
print "pineapple-42" > src/note.txt
out=$(MINIGW_SHARE=1 MINIGW_WORK=direct $mg -p --model claude-cli-haiku "hi" 2>&1 || true)
print -r -- "$out" | grep -q "sync only" || fail "direct mode not refused clearly: $out"

# Sync: real Claude Code here → router in Docker → fake claude edits the copy → change comes back.
out=$(MINIGW_SHARE=1 MINIGW_WORK=sync $mg -p --model claude-cli-haiku "Copy src/note.txt to agent.txt" 2>&1) || fail "minigw -p: $out"
print -r -- "$out" | grep -q "done by fake claude" || fail "answer: $out"
[[ $(<agent.txt) == $'pineapple-42\nedited on linux' ]] || fail "agent.txt not synced back: $(cat agent.txt 2>&1)"
docker exec $NAME sh -c 'cat /root/.cache/minigw/scratch/sync/*/*/src/note.txt' | grep -q pineapple-42 || fail "copy on the router"
docker logs $NAME 2>&1 | grep -q 'step: "Write agent.txt"' || fail "agent step not logged"
print "serve e2e ok"
