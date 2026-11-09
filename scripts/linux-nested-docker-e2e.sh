#!/bin/zsh
# Linux minigw serve (Docker) z ugnezdenimi repozitoriji v načinu worktree in pravim Claude Code (brez prijave: lažni
# API Anthropic v node ga prek tool_use vodi z Read/Edit/Write/Bash). Odjemalec je Go minigw na tem Macu.
# Pozitivno: podmodul sub, nesleden vendor/lib (commit) in vendor/new (le git init), prezrt build/_deps/x → vsak svojo
# vejo in vrstico, vrhnja veja top.txt in gitlink sub, cherry-pick čist; agent teče v svojem imenskem prostoru PID.
# Negativno: user.max_user_namespaces=1 (le VM Dockerja, na koncu nazaj): zunanji bwrap dela, gnezdeni ne → zahteva z
# ugnezdenimi zavrnjena pred začetkom, repozitorij brez njih dela. AppArmor Ubuntu 24.04 se v Dockerju ne da posnemati.
# Preveri še, da je zavrnitev končna (enkrat, hitro) in da Bash v peskovniku dela (kratek TMPDIR teka).
# Svoj HOME na Macu in v vsebniku; uporabnikovih nastavitev, ključev in žetonov se ne dotakne.
#   linux-nested-docker-e2e.sh   (Docker, omrežje za apt in npm)
emulate -L zsh; set -eu
ROOT=${0:A:h:h}
T=${$(mktemp -d -t mgw-nested-e2e):A}
NAME=mgw-nested-e2e IMG=mgw-nested-e2e PORT=${PORT:-18767}
IP='[::ffff:127.0.0.1]' # šifrirano kot v serve-docker-e2e.sh
NS0=
cleanup() {
  [[ -n $NS0 ]] && docker run --rm --privileged $IMG sh -c "echo $NS0 > /proc/sys/user/max_user_namespaces" || true
  docker rm -f $NAME >/dev/null 2>&1 || true
  rm -rf $T
}
trap cleanup EXIT
fail() { print -u2 "FAIL $*"; docker exec $NAME sh -c "tail -30 /cfg/serve.log; cat /tmp/tools.log" >&2 2>/dev/null || true; exit 1 }

mkdir -p $T/opt/bin
(cd $ROOT/client && GOOS=linux GOARCH=arm64 go build -o $T/opt/bin/minigw . && go build -o $T/minigw .)
docker build -q -t $IMG - <<'EOF' >/dev/null
FROM node:22-bookworm-slim
RUN apt-get update -qq && apt-get install -y -qq bubblewrap git socat ripgrep procps >/dev/null && rm -rf /var/lib/apt/lists/*
RUN npm install -g --silent @anthropic-ai/claude-code
EOF
# prehod odstrani ANTHROPIC_*: ovoj claude jih nastavi za lažni API
cat > $T/opt/bin/claude <<'EOF'
#!/bin/sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:18001 ANTHROPIC_API_KEY=sk-fake-e2e CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
exec /usr/local/bin/claude "$@"
EOF
chmod +x $T/opt/bin/claude
cat > $T/opt/fake.js <<'JS'
// Lažni API Anthropic: korak = število tool_result v pogovoru; scenarij iz /opt/t/scenario; izidi orodij v /tmp/tools.log.
const http = require("http"), fs = require("fs");
const start = {type: "message_start", message: {id: "m1", type: "message", role: "assistant", model: "claude-haiku-4-5", content: [], stop_reason: null, usage: {input_tokens: 1, output_tokens: 1}}};
const end = r => [["message_delta", {type: "message_delta", delta: {stop_reason: r}, usage: {output_tokens: 3}}], ["message_stop", {type: "message_stop"}]];
const text = t => [["message_start", start], ["content_block_start", {type: "content_block_start", index: 0, content_block: {type: "text", text: ""}}],
  ["content_block_delta", {type: "content_block_delta", index: 0, delta: {type: "text_delta", text: t}}], ["content_block_stop", {type: "content_block_stop", index: 0}], ...end("end_turn")];
const tool = (n, name, input) => [["message_start", start], ["content_block_start", {type: "content_block_start", index: 0, content_block: {type: "tool_use", id: "toolu_" + n, name, input: {}}}],
  ["content_block_delta", {type: "content_block_delta", index: 0, delta: {type: "input_json_delta", partial_json: JSON.stringify(input)}}],
  ["content_block_stop", {type: "content_block_stop", index: 0}], ...end("tool_use")];
function sse(res, ev) { res.writeHead(200, {"content-type": "text/event-stream"}); for (const [n, d] of ev) res.write(`event: ${n}\ndata: ${JSON.stringify(d)}\n\n`); res.end(); }
// ravni imenskih prostorov PID procesa claude (NSpid iz /proc vsebnika): 2 = lasten imenski prostor (pin), 1 = brez
function levels() {
  let n = 0;
  for (const p of fs.readdirSync("/proc").filter(p => /^\d+$/.test(p))) try {
    const a = fs.readFileSync(`/proc/${p}/cmdline`, "utf8").split("\0");
    if (!a.includes("--session-id") || a[1] === "__minigw-agent" || /\/bwrap$/.test(a[0])) continue;
    n = Math.max(n, fs.readFileSync(`/proc/${p}/status`, "utf8").match(/NSpid:\s*(.*)/)[1].trim().split(/\s+/).length);
  } catch {}
  return n;
}
const bash = {name: "Bash", input: {command: "echo bash-ok > bash.txt", description: "bash"}};
http.createServer((req, res) => { let b = ""; req.on("data", c => b += c); req.on("end", () => {
  if (!req.url.startsWith("/v1/messages")) { res.writeHead(200, {"content-type": "application/json"}); return res.end("{}"); }
  if (req.url.includes("count_tokens")) { res.writeHead(200, {"content-type": "application/json"}); return res.end('{"input_tokens":1}'); }
  let j = {}; try { j = JSON.parse(b); } catch {}
  if (!(j.tools || []).some(t => t.name === "Bash")) return sse(res, text("ok"));
  const results = (j.messages || []).flatMap(m => Array.isArray(m.content) ? m.content : []).filter(c => c.type === "tool_result");
  fs.appendFileSync("/tmp/tools.log", results.length ? JSON.stringify(results[results.length - 1]) + "\n" : "NSPID-LEVELS " + levels() + "\n");
  const wd = (JSON.stringify(j.system).match(/clone here at ([^\\\s",]+)/) || [])[1];
  const f = p => wd + "/" + p;
  const top = {name: "Write", input: {file_path: f("top.txt"), content: "top\n"}};
  const steps = fs.readFileSync("/opt/t/scenario", "utf8").trim() === "plain" ? [top, bash] : [
    {name: "Read", input: {file_path: f("sub/README")}},
    {name: "Edit", input: {file_path: f("sub/README"), old_string: "sub readme", new_string: "sub readme\nagent edit sub"}},
    {name: "Write", input: {file_path: f("vendor/new/b.txt"), content: "b\nagent edit new\n"}},
    {name: "Write", input: {file_path: f("vendor/lib/a.txt"), content: "a\nagent edit lib\n"}}, top, bash];
  const s = steps[results.length];
  sse(res, s ? tool(results.length, s.name, s.input) : text("DONE"));
}); }).listen(18001, "127.0.0.1");
JS

docker rm -f $NAME >/dev/null 2>&1 || true
# --init: bwrap pusti osirotel proces, ki ga mora nekdo požeti (zombi drži uporabniški imenski prostor)
docker run -d --init --name $NAME --privileged -p $PORT:8765 -v $T/opt:/opt/t $IMG sleep infinity >/dev/null
dx() { docker exec -u node -e HOME=/home/node -e PATH=/opt/t/bin:/usr/local/bin:/usr/bin:/bin "$@" }
docker exec $NAME sh -c 'mkdir -p /cfg && chown node /cfg'
dx -d $NAME node /opt/t/fake.js
print nested > $T/opt/scenario
LINE=$(dx $NAME minigw serve init --config /cfg/serve.json | grep "minigw connect")
TOKEN=${${LINE#*--token \'}%\'}
# Bash je privzeto izklopljen
dx $NAME node -e 'const f="/cfg/serve.json",c=require(f);c.tools="Read,Grep,Glob,Edit,Write,MultiEdit,Bash";require("fs").writeFileSync(f,JSON.stringify(c,null,2))'
gw() { # (ponovni) zagon prehoda: preizkusa bwrap tečeta ob prvi zahtevi procesa
  dx $NAME sh -c 'pkill -x minigw; while pgrep -x minigw >/dev/null; do sleep 0.1; done; : > /cfg/serve.log' || true
  dx -d $NAME sh -c 'exec minigw serve --config /cfg/serve.json >>/cfg/serve.log 2>&1'
  for i in {1..50}; do curl -s -o /dev/null http://127.0.0.1:$PORT/health/live && return; sleep 0.2; done
  fail "gateway did not start"
}
gw
export HOME=$T/home XDG_CONFIG_HOME=$T/home/.config
mkdir -p $HOME
mg=$T/minigw
mgr() { dx -i $NAME minigw "$@" --config /cfg/serve.json }
$mg connect $IP:$PORT --token "$TOKEN" >/dev/null 2>&1 && fail "unknown device accepted"
ALLOW=$(docker exec $NAME cat /cfg/serve.log | sed -n 's/.*to allow it run: \(minigw [a-z ]*allow [^ ]*\).*/\1/p' | head -1)
[[ -n $ALLOW ]] || fail "no allow hint"
mgr ${=ALLOW#minigw } >/dev/null
$mg connect $IP:$PORT --token "$TOKEN" | grep -q "(encrypted)" || fail "connect"
$mg models | grep -q claude-cli-haiku || fail "models"

# repozitorij: podmodul sub, nesleden vendor/lib (commit) in vendor/new (le init), prezrt build/_deps/x
gc() { git -c user.name=t -c user.email=t@x -c core.hooksPath=/dev/null "$@" }
S=$T/subsrc C=$HOME/top
git init -q -b main $S; print "sub readme" > $S/README; gc -C $S add README; gc -C $S commit -qm sub
git init -q -b main $C; print a > $C/a.txt; print 'build/' > $C/.gitignore; gc -C $C add -A; gc -C $C commit -qm first
gc -C $C -c protocol.file.allow=always submodule add -q $S sub; gc -C $C commit -qm "add sub"
git init -q -b main $C/vendor/lib; print a > $C/vendor/lib/a.txt; gc -C $C/vendor/lib add a.txt; gc -C $C/vendor/lib commit -qm lib
git init -q -b main $C/vendor/new; print b > $C/vendor/new/b.txt
git init -q -b main $C/build/_deps/x; print d > $C/build/_deps/x/d.txt
subhead=$(git -C $C/sub rev-parse HEAD)
wt() { (cd $1 && MINIGW_SHARE=1 MINIGW_WORK=worktree $mg -p --model claude-cli-haiku "Do the task." > $T/out 2> $T/err) }
nspid() { docker exec $NAME sed -n 's/^NSPID-LEVELS //p' /tmp/tools.log | head -1 }
# Bash v peskovniku Claude Code (gnezden bwrap pod pin): preveri se na koncu, da ostalo pride na vrsto
BASH=()
bashok() { [[ $(git -C $1 show ${2}:bash.txt 2>/dev/null) == bash-ok ]] || BASH+=("$3: $(docker exec $NAME grep -h -o 'Sandbox[^\"]*' /tmp/tools.log | head -1)") }

# 1: z ugnezdenimi
docker exec $NAME rm -f /tmp/tools.log
wt $C || fail "minigw -p: $(<$T/err)"
grep -q "nested repositories need" $T/err && fail "refused: $(<$T/err)"
docker exec $NAME grep -q "nested bwrap does not work" /cfg/serve.log && fail "nested bwrap probe failed"
[[ $(grep -c "^.*agent work is on branch [^ ]*: review with git diff" $T/err) == 1 ]] || fail "top line: $(<$T/err)"
br=$(sed -n 's/.*agent work is on branch \([^ ]*\): review with git diff.*/\1/p' $T/err)
base=$(sed -n 's/.*: review with git diff \([0-9a-f]\{40\}\)\.\..*/\1/p' $T/err)
for p in sub vendor/lib vendor/new; do
  [[ $(grep -cF "agent work is on branch $br in nested repository $p: review with git -C $p diff" $T/err) == 1 ]] || fail "no line for $p: $(<$T/err)"
done
grep -q "build/_deps" $T/err && fail "line for build/_deps: $(<$T/err)"
[[ -z $(git -C $C/build/_deps/x for-each-ref 'refs/heads/minigw/*') ]] || fail "build/_deps/x got a branch"
rdiff() { git -C $C/$1 diff $(sed -n "s|.*in nested repository $1: review with git -C [^ ]* diff \([^ ]*\) .*|\1|p" $T/err | head -1) }
rdiff sub | grep -q '^+agent edit sub' || fail "sub diff: $(rdiff sub)"
rdiff vendor/lib | grep -q '^+agent edit lib' || fail "vendor/lib diff: $(rdiff vendor/lib)"
rdiff vendor/new | grep -q '^+agent edit new' || fail "vendor/new diff: $(rdiff vendor/new)"
[[ $(git -C $C show ${br}:top.txt) == top ]] || fail "top.txt not on $br"
[[ $(git -C $C ls-tree $br sub) == "160000 commit $(git -C $C/sub rev-parse $br)"* && $(git -C $C/sub rev-parse $br) != $subhead ]] || fail "gitlink: $(git -C $C ls-tree $br sub)"
[[ -z $(git -C $C ls-tree -r $br vendor) ]] || fail "vendor on the top branch: $(git -C $C ls-tree -r $br vendor)"
K=$T/pick; gc clone -q $C $K; gc -C $K fetch -q $C "${br}:$br"
gc -C $K cherry-pick -n $base..$br >/dev/null 2>&1 || fail "cherry-pick -n $base..$br not clean"
[[ $(git -C $C/sub rev-parse HEAD) == $subhead ]] || fail "user's sub HEAD moved"
n1=$(nspid); [[ $n1 == 2 ]] || fail "claude not in its own PID namespace: NSpid levels '$n1'"
bashok $C $br nested
print "ok: nested ($br in ., sub, vendor/lib, vendor/new; build/_deps none; cherry-pick clean; claude NSpid levels $n1: own PID namespace)"

# 2: brez gnezdenih uporabniških imenskih prostorov (velja za ves VM Dockerja; cleanup vrne vrednost)
NS0=$(docker exec $NAME cat /proc/sys/user/max_user_namespaces)
# 1: bwrap brez --dev (finalizeBwrap) dela, z --dev (bwrapReady, dva imenska prostora) in gnezdeni ne. 2 ni zanesljivo:
# osirotel bwrap še nekaj časa drži svoj imenski prostor, preizkus gnezdenega uspe šele ob ponovitvi.
docker exec $NAME sh -c 'echo 1 > /proc/sys/user/max_user_namespaces'
gw
docker exec $NAME rm -f /tmp/tools.log
before=$(git -C $C for-each-ref 'refs/heads/minigw/' | wc -l)
t0=$SECONDS; wt $C && fail "nested repos not refused: $(<$T/err)"; secs=$(( SECONDS - t0 ))
cat $T/out $T/err | grep -F "nested repositories need bwrap inside bwrap for Claude CLI on this Linux gateway" | grep -q "use sync" || fail "refusal: $(cat $T/out $T/err)"
docker exec $NAME test -s /tmp/tools.log && fail "agent ran before the refusal"
[[ $(git -C $C for-each-ref 'refs/heads/minigw/' | wc -l) == $before ]] || fail "branch after refusal"
n=$(cat $T/out $T/err | grep -c 'nested repositories need')
(( n == 1 && secs < 20 )) || fail "refusal shown ${n}x after ${secs}s (want once, final): $(<$T/err)"
print "ok: refused before start, once, in ${secs}s ($(cat $T/out $T/err | grep -o 'nested repositories need bwrap inside bwrap[^;]*' | head -1); … use sync)"
# spet vse dovoljeno: claude ostane brez pin (bwrapReady velja do konca procesa), Bash v peskovniku Claude Code pa dela
docker exec $NAME sh -c "echo $NS0 > /proc/sys/user/max_user_namespaces"; NS0=
P=$HOME/plain; git init -q -b main $P; print p > $P/p.txt; gc -C $P add -A; gc -C $P commit -qm p
print plain > $T/opt/scenario
wt $P || fail "plain repo: $(<$T/err)"
brp=$(sed -n 's/.*agent work is on branch \([^ ]*\): review with git diff.*/\1/p' $T/err)
[[ -n $brp && $(git -C $P show ${brp}:top.txt) == top ]] || fail "plain branch: $(<$T/err)"
[[ $(nspid) == 1 ]] || fail "plain run: NSpid levels '$(nspid)', expected 1 (no pin)"
bashok $P $brp plain
print "ok: plain repo in worktree mode without nested bwrap ($brp; claude NSpid levels 1)"
(( ! $#BASH )) || fail "Bash in Claude Code's sandbox failed in the clone: ${(j:; :)BASH}"
print "ok: Bash in Claude Code's sandbox (nested and plain)"
# TMPDIR tekov (/tmp/mgw-*/<tek>) po teku pobrisan
left=$(docker exec $NAME sh -c 'find /tmp/mgw-* -mindepth 1 2>/dev/null' || true)
[[ -z $left ]] || fail "run temp left: $left"
print "ok: run temp folders removed"
print "linux nested docker e2e ok"
