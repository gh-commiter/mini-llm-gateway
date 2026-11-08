#!/bin/zsh
# Linux: pravi Claude Code v svojem peskovniku (bwrap + posrednik socat) z lažnim API Anthropic (brez prijave). Model zahteva
# Bash »minigw -p --model …«; minigw v načinu agenta prebere žeton iz MINIGW_AGENT_AUTH_FILE (allowRead le za to datoteko)
# in gre prek posrednika peskovnika na loopback vrata (lažna vrata izvajanja preverijo žeton). Nato še negativno: brez
# allowRead datoteke minigw žetona ne prebere (peskovnik je res dejaven).
#   agent-calls-linux-claude-sandbox.sh   (Docker, omrežje za apt in npm)
emulate -L zsh; set -eu
cd ${0:A:h}/..
T=${$(mktemp -d):A}
trap 'rm -rf $T' EXIT
fail() { print -u2 "FAIL: $*"; exit 1 }
arch=$(docker info --format '{{.Architecture}}'); [[ $arch == aarch64 ]] && arch=arm64; [[ $arch == x86_64 ]] && arch=amd64
(cd client && GOOS=linux GOARCH=$arch go build -o $T/minigw .)
cat > $T/fake.js <<'JS'
// Lažni Anthropic API (model) in lažna vrata izvajanja (preveri žeton iz datoteke).
const http = require("http"), fs = require("fs");
const [modelPort, runPort, authFile, minigw] = process.argv.slice(2);
function sse(res, events) {
  res.writeHead(200, {"content-type": "text/event-stream"});
  for (const [n, d] of events) res.write(`event: ${n}\ndata: ${JSON.stringify(d)}\n\n`);
  res.end();
}
const start = {type: "message_start", message: {id: "m1", type: "message", role: "assistant", model: "claude-sonnet-4-5", content: [], stop_reason: null, usage: {input_tokens: 1, output_tokens: 1}}};
function text(t) { return [["message_start", start], ["content_block_start", {type: "content_block_start", index: 0, content_block: {type: "text", text: ""}}],
  ["content_block_delta", {type: "content_block_delta", index: 0, delta: {type: "text_delta", text: t}}], ["content_block_stop", {type: "content_block_stop", index: 0}],
  ["message_delta", {type: "message_delta", delta: {stop_reason: "end_turn"}, usage: {output_tokens: 3}}], ["message_stop", {type: "message_stop"}]]; }
function body(req) { return new Promise(r => { let b = ""; req.on("data", c => b += c); req.on("end", () => r(b)); }); }
http.createServer(async (req, res) => {
  const b = await body(req);
  if (!req.url.startsWith("/v1/messages")) { res.writeHead(200, {"content-type": "application/json"}); return res.end("{}"); }
  if (req.url.includes("count_tokens")) { res.writeHead(200, {"content-type": "application/json"}); return res.end('{"input_tokens":1}'); }
  let j = {}; try { j = JSON.parse(b); } catch {}
  const msgs = JSON.stringify(j.messages || []);
  const hasBash = (j.tools || []).some(t => t.name === "Bash");
  if (msgs.includes("tool_result")) {
    const m = msgs.match(/RUN-PORT-OK[^"\\]*/); fs.writeFileSync("/work/result.txt", m ? m[0] : msgs.slice(-600));
    return sse(res, text("DONE"));
  }
  if (!hasBash) return sse(res, text("ok"));
  const cmd = `${minigw} -p --model claude-fake-model hello 2>&1`;
  sse(res, [["message_start", start], ["content_block_start", {type: "content_block_start", index: 0, content_block: {type: "tool_use", id: "toolu_1", name: "Bash", input: {}}}],
    ["content_block_delta", {type: "content_block_delta", index: 0, delta: {type: "input_json_delta", partial_json: JSON.stringify({command: cmd, description: "ask"})}}],
    ["content_block_stop", {type: "content_block_stop", index: 0}],
    ["message_delta", {type: "message_delta", delta: {stop_reason: "tool_use"}, usage: {output_tokens: 3}}], ["message_stop", {type: "message_stop"}]]);
}).listen(+modelPort, "127.0.0.1");
http.createServer(async (req, res) => {
  await body(req);
  const want = "Bearer " + fs.readFileSync(authFile, "utf8").trim();
  if (req.headers.authorization !== want) { res.writeHead(401); return res.end('{"error":{"message":"bad token"}}'); }
  fs.appendFileSync("/work/runport.log", req.method + " " + req.url + " via " + req.socket.remoteAddress + "\n");
  sse(res, text("RUN-PORT-OK from the run port"));
}).listen(+runPort, "127.0.0.1");
JS
cat > $T/run.sh <<'SH'
set -e
apt-get update -qq >/dev/null && apt-get install -y -qq bubblewrap socat ripgrep >/dev/null 2>&1
npm install -g --silent @anthropic-ai/claude-code >/dev/null 2>&1
claude --version
mkdir -p /work/proj /work/run && chmod 700 /work/run && printf 'tok-123' > /work/run/minigw-agent-auth && chmod 600 /work/run/minigw-agent-auth
node /opt/t/fake.js 18001 18002 /work/run/minigw-agent-auth /opt/t/minigw & sleep 1
SETTINGS='{"sandbox":{"enabled":true,"failIfUnavailable":true,"autoAllowBashIfSandboxed":true,"allowUnsandboxedCommands":false,"network":{"allowedDomains":["*"]},"filesystem":{"denyRead":["/root","/work/run"],"allowRead":["/work/proj","/opt/t/minigw","/work/run/minigw-agent-auth"],"allowWrite":["/work/proj"]}}}'
cd /work/proj
READ_AUTH=${READ_AUTH:-1}; [ "$READ_AUTH" = 1 ] || SETTINGS=$(echo "$SETTINGS" | sed 's|,"/work/run/minigw-agent-auth"]|]|'); ANTHROPIC_BASE_URL=http://127.0.0.1:18001 ANTHROPIC_API_KEY=sk-fake MINIGW_AGENT_URL=http://127.0.0.1:18002 MINIGW_AGENT_AUTH_FILE=/work/run/minigw-agent-auth \
  timeout 180 claude -p --model sonnet --tools default --permission-mode acceptEdits --settings "$SETTINGS" --setting-sources "" --strict-mcp-config "ask the other model" > /work/claude.out 2>&1 || echo "claude rc=$?"
echo "--- claude:"; tail -5 /work/claude.out
echo "--- run port log:"; cat /work/runport.log 2>/dev/null || echo "(no request reached the run port)"
echo "--- tool result:"; cat /work/result.txt 2>/dev/null || echo "(none)"
SH
docker run --rm --privileged -v $T:/opt/t node:22-bookworm-slim bash /opt/t/run.sh > $T/pos.log 2>&1 || fail "docker: $(tail -5 $T/pos.log)"
grep -q "^RUN-PORT-OK" $T/pos.log && grep -q "POST /v1/messages via 127.0.0.1" $T/pos.log || fail "claude → minigw → run port: $(tail -8 $T/pos.log)"
print "ok: Claude Code's Linux sandbox reaches the run port through its proxy ($(head -1 $T/pos.log))"
docker run --rm --privileged -e READ_AUTH=0 -v $T:/opt/t node:22-bookworm-slim bash /opt/t/run.sh > $T/neg.log 2>&1 || fail "docker (negative)"
grep -q "no request reached the run port" $T/neg.log || fail "the token file was readable without allowRead: $(tail -8 $T/neg.log)"
print "ok: without allowRead the token file stays hidden in the sandbox"
