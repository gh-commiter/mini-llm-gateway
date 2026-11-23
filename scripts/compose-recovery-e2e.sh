#!/bin/zsh
# Requires zsh (also on Linux), Docker Compose v2/BuildKit, Go, Python 3,
# curl, ssh-keygen, shasum, cached Go modules and the Dockerfile's pinned images.
# Run: zsh scripts/compose-recovery-e2e.sh. Evidence stays in SCRATCH_DIR.
# Default scratch follows downgrade-proof.sh: TMPDIR, else the platform user cache.
# SKIP exits 1 unless ALLOW_SKIP=1. Product failures always exit 1.
emulate -L zsh
set -eu
set -o pipefail
umask 077
ROOT=${0:A:h:h}
source "$ROOT/scripts/compose-e2e-common.zsh"
T0=$SECONDS
OLD=0.7.6; OLD_SCHEMA=12
CURRENT=$(python3 -c 'import re,sys; s=open(sys.argv[1]).read(); print(len(re.search(r"var migrations = .*?\{(.*?)\}",s).group(1).split(",")))' "$ROOT/client/core/core.go")
[[ $CURRENT == 26 ]] || { print -u2 'FAIL: expected current schema 26'; exit 1; }
if [[ -n ${TMPDIR:-} ]]; then CACHE=$TMPDIR
elif [[ $(uname -s) == Darwin ]]; then CACHE=$HOME/Library/Caches
else CACHE=${XDG_CACHE_HOME:-$HOME/.cache}; fi
D=${SCRATCH_DIR:-$CACHE/minigw-compose-recovery}
mkdir -p "$D"
R=$(mktemp -d "$D/run-XXXXXX")
P=minigw-recovery-$(date +%s)-$$
PROJECTS=($P $P-clean)
CREATED=()
STUB=$P-stub
fail() { print -u2 -r -- "FAIL: $*; evidence: $R"; exit 1; }
skip() { print -u2 -r -- "SKIP: $*; NOT TESTED; evidence: $R"; [[ ${ALLOW_SKIP:-0} == 1 ]] && exit 0; exit 1; }
ok() { print -r -- "PASS $*"; }
cleanup() {
  local rc=$1
  trap - EXIT ZERR
  docker rm -f "$STUB" >/dev/null 2>&1 || true
  for COMPOSE_PROJECT_NAME in $CREATED; do
    export COMPOSE_PROJECT_NAME
    docker compose down -v >/dev/null 2>&1 || true
  done
  if (( $#CREATED )); then docker image rm "$P:old" "$P:new" >/dev/null 2>&1 || true; fi
  print "Elapsed: $((SECONDS-T0))s; evidence: $R"
  exit $rc
}
trap 'recovery_rc=$?; set +e; cleanup $recovery_rc' EXIT
# zsh can leave a function on errexit without running the outer EXIT trap.
trap 'recovery_rc=$?; set +e; cleanup $recovery_rc' ZERR
trap 'exit 1' HUP INT TERM
for c in docker go python3 curl ssh-keygen shasum; do command -v $c >/dev/null || fail "$c required"; done
docker info > "$R/docker-info.log" 2>&1 || skip 'Docker is not reachable'
docker compose version || fail 'Compose v2 required'
ARCH=$(docker info --format '{{.Architecture}}')
case $ARCH in aarch64|arm64) ARCH=arm64;; x86_64|amd64) ARCH=amd64;; *) skip "unsupported Docker architecture $ARCH";; esac
export DOCKER_DEFAULT_PLATFORM=linux/$ARCH MINIGW_BIND=127.0.0.1 MINIGW_PORT=${PORT:-18773}
export COMPOSE_PROJECT_NAME=$P MINIGW_VERSION=$OLD
export COMPOSE_FILE=$ROOT/deploy/docker/compose.yaml
export MINIGW_LOCAL_DIR=$R/bin
mkdir "$R/bin" "$R/release"
# Same pinned signer and verification sequence as the Dockerfile's released path.
BASE=https://github.com/gh-commiter/mini-llm-gateway/releases/download/v$OLD
ASSET=minigw-linux-$ARCH
for n in $ASSET SHA256SUMS SHA256SUMS.sig; do
  # MINIGW_OLD_RELEASE_DIR: a kept copy of the old release's files (the release may be gone from GitHub); verified below all the same.
  if [[ -n ${MINIGW_OLD_RELEASE_DIR:-} ]]; then cp "$MINIGW_OLD_RELEASE_DIR/$n" "$R/release/$n" || fail "copy v$OLD asset $n from MINIGW_OLD_RELEASE_DIR"
  else curl -fsSL --connect-timeout 15 --max-time 180 "$BASE/$n" -o "$R/release/$n" || fail "download signed v$OLD asset $n (set MINIGW_OLD_RELEASE_DIR to a kept copy)"; fi
done
print -r -- 'minigw-release namespaces="minigw-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINt++OHn+HHmR9mmA2UI+B0osuilO2GP0ddEzNp0GTrH' > "$R/release/signers"
ssh-keygen -Y verify -f "$R/release/signers" -I minigw-release -n minigw-release -s "$R/release/SHA256SUMS.sig" < "$R/release/SHA256SUMS" || fail 'release signature'
[[ $(sed -n 's/^VERSION //p' "$R/release/SHA256SUMS") == $OLD ]] || fail 'signed version'
WANT=$(awk -v n=$ASSET '$2==n {print $1}' "$R/release/SHA256SUMS")
[[ -n $WANT && $(shasum -a 256 "$R/release/$ASSET" | cut -d' ' -f1) == $WANT ]] || fail 'release checksum'
chmod 755 "$R/release/$ASSET"
# Copy the verified release through the local Dockerfile stage, avoiding a second download.
cp "$R/release/$ASSET" "$R/bin/$ASSET"
chmod 755 "$R/bin/$ASSET"
print -rl -- 'services:' '  minigw:' '    build:' '      args:' '        SOURCE: local' '      additional_contexts:' "        local: $R/bin" "    image: $P:old" > "$R/released.yaml"
OLD_FILES=$COMPOSE_FILE:$R/released.yaml
NEW_FILES=$COMPOSE_FILE:$ROOT/deploy/docker/compose.local.yaml:$R/current.yaml
print -rl -- 'services:' '  minigw:' "    image: $P:new" > "$R/current.yaml"
export COMPOSE_FILE=$OLD_FILES
# Assert exact names before authorizing cleanup or a destructive step.
safe() {
  docker compose config --format json | python3 -c 'import json,sys; c=json.load(sys.stdin); p=sys.argv[1]; assert c["name"]==p; assert all(c["volumes"][k]["name"]==p+"_"+k for k in ("config","scratch")); assert all(x["host_ip"]=="127.0.0.1" for x in c["services"]["minigw"]["ports"])' "$COMPOSE_PROJECT_NAME" || fail 'unsafe project configuration'
}
for name in $PROJECTS; do
  ! docker volume inspect ${name}_config >/dev/null 2>&1 || fail "pre-existing volume ${name}_config"
  ! docker volume inspect ${name}_scratch >/dev/null 2>&1 || fail "pre-existing volume ${name}_scratch"
  [[ -z $(docker ps -aq --filter label=com.docker.compose.project=$name) ]] || fail "pre-existing project $name"
done
safe
CREATED+=($P)
docker compose build > "$R/old-build.log" 2>&1 || fail 'released image build (old-build.log)'
docker run --rm --network none --entrypoint minigw "$P:old" --version > "$R/old-probe.log" 2>&1 ||
  skip "Docker cannot execute verified v$OLD (old-probe.log)"
docker compose up -d --no-build >> "$R/old-build.log" 2>&1 || fail 'released deployment start (old-build.log)'
wait_healthy() {
  healthy "$COMPOSE_PROJECT_NAME" 60 2 && return 0
  docker compose logs > "$R/unhealthy-$COMPOSE_PROJECT_NAME.log" 2>&1
  return 1
}
wait_healthy || fail "verified v$OLD does not become healthy"
gw() { docker compose exec -T minigw minigw gw "$@"; }
member() { docker compose exec -T -e HOME=/var/lib/minigw/scratch/member -e MINIGW_SECRETS=file -w /var/lib/minigw/scratch/member minigw minigw "$@"; }
[[ $(docker compose exec -T minigw minigw --version) == *$OLD* ]] || fail 'old version'
gw status > "$R/old-status.txt"
grep -q 'projects: on' "$R/old-status.txt" || fail 'old core unavailable'
for u in ada ben; do gw user add $u; done
gw team add rehearsal
gw project add drill 'Recovery drill'
for u in ada ben; do gw team member add rehearsal $u; done
gw team project add rehearsal drill
gw member drill set ada admin
gw member drill set ben developer
# Member CLI state belongs to scratch, so the backup recovers its device and project link.
docker compose exec -T minigw mkdir -p /var/lib/minigw/scratch/member
TOKEN=$(gw token | grep -o 'mgw-[A-Za-z0-9_-]*#[A-Za-z0-9_-]*' | head -1)
[[ -n $TOKEN ]] || fail 'connect token'
member connect 127.0.0.1:8765 --token "$TOKEN"
DEVICE=$(member status | sed -n 's/^Device: *\([^ ]*\).*/\1/p')
[[ -n $DEVICE ]] || fail 'member device'
gw devices allow "$DEVICE" --name recovery-member
gw bind "$DEVICE" ada
member project link drill
for cls in structure data private; do
  print -r -- "recoveryneedle $cls text" | member note push --class $cls --title "Recovery $cls" --key recovery-$cls -
done
print 'Awaiting review' | member kb submit drill --title 'Pending recovery' -
# Only this gateway's namespace; the stub listens on loopback, with no published port.
PY=python:3-alpine@sha256:0f9552bb28711333da16915ec146f5e6bb44a7fdac9ced15dc17c2e0206346dd
docker run --rm -d --name "$STUB" --network container:$P-minigw-1 --cap-drop ALL --security-opt no-new-privileges "$PY" python -c '
import http.server
class H(http.server.BaseHTTPRequestHandler):
 def do_GET(self):
  self.send_response(200); self.end_headers(); self.wfile.write(b"{\"data\":[{\"id\":\"dummy-model\"}]}")
http.server.HTTPServer(("127.0.0.1",4000),H).serve_forever()
' || fail 'loopback provider stub'
sleep 2
print dummy-key | gw provider add Stub http://127.0.0.1:4000 --insecure
# Use the same released CLI for listings on both gateways: display formats have changed.
released_cli() {
  docker run --rm -i --label com.docker.compose.project=$COMPOSE_PROJECT_NAME \
    --network container:$COMPOSE_PROJECT_NAME-minigw-1 --pid container:$COMPOSE_PROJECT_NAME-minigw-1 \
    --volumes-from $COMPOSE_PROJECT_NAME-minigw-1 \
    -e HOME=/var/lib/minigw/scratch/member -e MINIGW_SECRETS=file -w /var/lib/minigw/scratch/member \
    --cap-drop ALL --security-opt no-new-privileges --entrypoint minigw "$P:old" "$@"
}
listing() { released_cli gw "$@"; }
# Schema 14 adds entryKind=finding. New reads also report the computed origin:
# validate it against the backed-up gateway id, then compare all original fields.
note_json() {
  local dest=$1; shift
  member "$@" > "$dest.raw"
  python3 -c 'import json,sys; c=json.load(sys.stdin)
def walk(v):
 if isinstance(v,dict):
  if "class" in v and "title" in v:
   if sys.argv[2]=="local":
    assert "entryKind" in v, "missing upgraded entryKind"
    if "body" in v: assert "origin" in v, "missing upgraded origin"
   v.setdefault("entryKind","finding")
  if "origin" in v:
   assert v.pop("origin")==sys.argv[1]+":"+str(v["project"]), "wrong note origin"
  for x in v.values(): walk(x)
 elif isinstance(v,list):
  for x in v: walk(x)
walk(c); print(json.dumps(c,sort_keys=True))' "${ORIGIN_GATEWAY:-}" "$MINIGW_VERSION" < "$dest.raw" > "$dest"
}
record() {
  local dest=$1
  mkdir "$dest"
  listing user list > "$dest/users"
  listing team list > "$dest/teams"
  released_cli team show rehearsal > "$dest/team-roster"
  listing team access list rehearsal > "$dest/team-access"
  listing project list > "$dest/projects"
  listing member drill list > "$dest/members"
  listing bind list > "$dest/bindings"
  listing provider list > "$dest/providers"
  docker compose exec -T minigw sha256sum /var/lib/minigw/config/gateway-keys.json > "$dest/provider-keys"
  listing kb review > "$dest/review"
  note_json "$dest/notes" note list --json
  local id
  for id in 1 2 3; do note_json "$dest/note-$id" note read $id --json; done
  note_json "$dest/submissions" kb submissions drill --json
  docker compose exec -T minigw cat /var/lib/minigw/config/serve.json | python3 -c 'import json,sys,hashlib; c=json.load(sys.stdin); print(hashlib.sha256(json.dumps([c["key"],c["instance"]]).encode()).hexdigest())' > "$dest/identity"
}
compare() { record "$R/$1"; diff -ru --exclude='*.raw' "$R/before" "$R/$1" || fail "listings changed: $1"; }
backup() {
  local target=$1
  mkdir "$R/$target"
  sh "$ROOT/deploy/docker/backup.sh" --stop "$R/$target" | tee "$R/$target.log"
}
archive() { local dirs=($R/$1/minigw-backup-*(/)); (( $#dirs == 1 )) || fail 'backup count'; print -r -- $dirs[1]; }
schema() {
  python3 -c '
import os,sqlite3,sys,tarfile,tempfile
with tarfile.open(sys.argv[1]+"/config.tar") as t, tempfile.TemporaryDirectory(dir=sys.argv[2]) as d:
 p=os.path.join(d,"core.db")
 for suffix in ("","-wal","-shm"):
  name="./core/core.db"+suffix
  if name in t.getnames():
   with open(p+suffix,"wb") as f: f.write(t.extractfile(name).read())
 c=sqlite3.connect("file:"+p+"?mode=ro",uri=True)
 print(c.execute("select value from meta where key=?",(sys.argv[3],)).fetchone()[0]); c.close()
' "$1" "$R" "${2:-schema}"
}
record "$R/before"
backup pre
PRE=$(archive pre)
ORIGIN_GATEWAY=$(schema "$PRE" gateway_id)
[[ $(schema "$PRE") == $OLD_SCHEMA ]] || fail 'old schema'
ok "v$OLD schema $OLD_SCHEMA populated and backed up"
docker rm -f "$STUB" >/dev/null
(cd "$ROOT/client" && GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 GOPROXY=off go build -mod=readonly -o "$R/bin/$ASSET" .) || fail 'current Linux build'
chmod 755 "$R/bin/$ASSET"
new() { export COMPOSE_FILE=$NEW_FILES MINIGW_VERSION=local; }
old() { export COMPOSE_FILE=$OLD_FILES MINIGW_VERSION=$OLD; }
upgrade() {
  new
  docker compose up -d --build || fail 'update'
  wait_healthy || fail 'updated health'
  local want have
  want=$(shasum -a 256 "$R/bin/$ASSET" | cut -d' ' -f1)
  have=$(docker compose exec -T minigw sha256sum /usr/local/bin/minigw)
  [[ ${have%% *} == $want ]] || fail 'updated container does not contain this branch binary'
  gw status > "$R/status-$1.txt"
  grep -q 'projects: on' "$R/status-$1.txt" || fail 'updated core unavailable'
  compare "$1"
  member kb search drill recoveryneedle --json | grep -q recoveryneedle || fail 'search after upgrade'
  docker compose exec -T minigw sh -c "ls /var/lib/minigw/config/core/core-$OLD_SCHEMA-*.db" || fail 'pre-migration snapshot'
}
empty() {
  safe
  docker compose down || fail 'down'
  docker volume rm ${COMPOSE_PROJECT_NAME}_config ${COMPOSE_PROJECT_NAME}_scratch || fail 'remove exact test volumes'
}
restore() { sh "$ROOT/deploy/docker/backup.sh" --restore "$@" || fail 'restore'; }
upgrade upgraded
# Publish a note and a replacement with this binary, so the backup carries history.
print 'historyoldneedle exact old body' | member kb submit drill --title 'History recovery' --key history-first -
HISTORY_SUB=$(member kb submissions drill --json | python3 -c 'import json,sys; print(next(v["id"] for v in json.load(sys.stdin)["submissions"] if v["title"]=="History recovery"))')
history_version() {
  gw kb review drill | awk -v id="$HISTORY_SUB" '$1=="submission" {found=($2==id)} found && $1=="version" {print $2}'
}
HISTORY_VERSION=$(history_version)
gw kb accept "$HISTORY_SUB" --version "$HISTORY_VERSION"
HISTORY_NOTE=$(member note list --json | python3 -c 'import json,sys; print(next(v["id"] for v in json.load(sys.stdin)["notes"] if v["title"]=="History recovery"))')
print 'historynewneedle replacement body' | member kb submit drill --title 'History replacement' --key history-second --replaces "$HISTORY_NOTE" --revision 1 -
HISTORY_SUB=$(member kb submissions drill --json | python3 -c 'import json,sys; print(next(v["id"] for v in json.load(sys.stdin)["submissions"] if v["title"]=="History replacement"))')
HISTORY_VERSION=$(history_version)
gw kb accept "$HISTORY_SUB" --version "$HISTORY_VERSION"
backup post
POST=$(archive post)
[[ $(schema "$POST") == $CURRENT && $CURRENT -gt $OLD_SCHEMA ]] || fail 'current schema'
history_backup() {
  python3 -c '
import os,sqlite3,sys,tarfile,tempfile
with tarfile.open(sys.argv[1]+"/config.tar") as t, tempfile.TemporaryDirectory(dir=sys.argv[2]) as d:
 p=os.path.join(d,"core.db")
 for suffix in ("","-wal","-shm"):
  name="./core/core.db"+suffix
  if name in t.getnames():
   with open(p+suffix,"wb") as f: f.write(t.extractfile(name).read())
 c=sqlite3.connect("file:"+p+"?mode=ro",uri=True)
 assert c.execute("select body from note_history where note_id=? and revision=1",(int(sys.argv[3]),)).fetchall()==[("historyoldneedle exact old body\n",)]
 assert c.execute("select body from notes where id=?",(int(sys.argv[3]),)).fetchone()[0]=="historynewneedle replacement body\n"
 c.close()
' "$1" "$R" "$HISTORY_NOTE" || fail 'history backup content'
}
history_backup "$POST"
ok "upgrade $OLD_SCHEMA -> $CURRENT, listings, search and snapshot"
empty
old
restore "$PRE"
docker compose up -d --no-build || fail 'start old after restore'
wait_healthy || fail 'restored old health'
[[ $(docker compose exec -T minigw minigw --version) == *$OLD* ]] || fail 'restored old version'
compare back
ok 'old version + pre-update backup'
# Recovery needs only the directory, not the source volumes or container.
empty
export COMPOSE_PROJECT_NAME=$P-clean
CREATED+=($COMPOSE_PROJECT_NAME)
new
safe
restore --into-project "$COMPOSE_PROJECT_NAME" "$PRE"
upgrade clean
backup clean-schema
[[ $(schema "$(archive clean-schema)") == $CURRENT ]] || fail 'clean host schema'
ok 'separate project restored from backup alone and upgraded'
# Keeping the new binary while restoring the pre-update backup upgrades again.
empty
restore --into-project "$COMPOSE_PROJECT_NAME" "$PRE"
upgrade again
ok 'pre-update backup with new binary upgrades again'
# Restore the post-update backup under the new binary and inspect a fresh copy.
empty
restore --into-project "$COMPOSE_PROJECT_NAME" "$POST"
docker compose up -d --no-build || fail 'start new with history backup'
wait_healthy || fail 'restored history health'
backup history-restored
[[ $(schema "$(archive history-restored)") == 26 ]] || fail 'restored history schema'
history_backup "$(archive history-restored)"
ok 'new binary restores old body in note_history and current replacement'
# A newer backup under the old binary must refuse the core and leave its bytes intact.
empty
old
restore --into-project "$COMPOSE_PROJECT_NAME" "$POST"
IMAGE=$P:old
hashdb() {
  docker run --rm --network none --entrypoint sh --mount type=volume,src=${COMPOSE_PROJECT_NAME}_config,dst=/v,readonly "$IMAGE" \
    -c 'sha256sum /v/core/core.db; if [ -s /v/core/core.db-wal ]; then sha256sum /v/core/core.db-wal; fi'
}
BEFORE=$(hashdb)
docker compose up -d --no-build || fail 'old start over newer backup'
wait_healthy || fail 'old liveness on refused core'
MESSAGE="core database has a newer schema than this program (schema $CURRENT, program $OLD_SCHEMA)"
gw status > "$R/refused-status.txt"
grep -Fq "$MESSAGE" "$R/refused-status.txt" || fail 'missing newer-schema refusal'
if gw project list > "$R/refused-projects.txt" 2>&1; then fail 'old program accepted newer schema'; fi
grep -q 'not available' "$R/refused-projects.txt" || fail 'unreadable project refusal'
docker compose logs > "$R/refused.log"
docker compose stop || fail 'stop refused gateway'
[[ $(hashdb) == $BEFORE ]] || fail 'PRODUCT DEFECT: old binary changed newer database bytes; see refused.log'
ok "$MESSAGE; project command refused; database byte-identical"
ok 'Compose recovery rehearsal complete'
