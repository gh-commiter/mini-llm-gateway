#!/bin/zsh
# End-to-end test of the Docker Compose deployment (deploy/docker, docs/docker.md) on this Mac, for linux/arm64 and
# linux/amd64 (the second only where Docker can emulate it: otherwise a clearly marked SKIP, never a silent pass).
#   1. this branch's binary (compose.local.yaml): up, healthy, gw status, a provider by Compose service name
#      (http://litellm:4000), settings and admin API NOT reachable from outside, restart keeps the config, clean stop
#   2. the released path (MINIGW_VERSION, default 0.7.22): build verifies signature, version and checksum; up; healthy
#   3. the build FAILS on: a binary that does not match SHA256SUMS, a SHA256SUMS the signature does not cover, a bad
#      signature, a missing asset (no such version), a signed version that is not the one asked for. A fixture server
#      (a container, published on 127.0.0.1 only) serves the real release files and tampered copies of them.
# Rules: containers publish ports on 127.0.0.1 only; no --privileged, no chown/chmod -R, no volume or system prune.
emulate -L zsh; set -u
ROOT=${0:A:h:h}
V=${MINIGW_VERSION:-0.7.22}
D=${SCRATCH_DIR:-"/Volumes/SAMSUNG990PRO/Mini Gateway Scratch/dev-builds/docker2-0723"}
PORT=${PORT:-18767}; FPORT=${FPORT:-18769}
REL=https://github.com/gh-commiter/mini-llm-gateway/releases/download/v$V
FIX=minigw-docker2-fixture
STUB=litellm
SKIPPED=()
fail() { print -u2 "FAIL $*"; exit 1 }
ok() { print "ok   $*" }
cleanup() { docker stop $FIX $STUB >/dev/null 2>&1; (cd $ROOT/deploy/docker; for p in arm64 amd64 arm64-rel amd64-rel; do COMPOSE_PROJECT_NAME=minigw-docker2-$p MINIGW_VERSION=$V docker compose stop >/dev/null 2>&1; done) }
trap cleanup EXIT
mkdir -p $D/bin $D/fix

healthy() { # project: wait until the container is healthy
  local i st
  for i in {1..40}; do
    st=$(docker inspect --format '{{.State.Health.Status}}' $1-minigw-1 2>/dev/null)
    [[ $st == healthy ]] && return 0
    sleep 5
  done
  return 1
}

run_platform() { # arm64|amd64
  local arch=$1 plat=linux/$1 proj=minigw-docker2-$1 out
  if ! docker run --rm --platform $plat debian:trixie-slim true >/dev/null 2>&1; then
    print "SKIP $plat: Docker cannot run it on this machine (no emulation); NOT TESTED"; SKIPPED+=($plat); return
  fi
  export DOCKER_DEFAULT_PLATFORM=$plat COMPOSE_PROJECT_NAME=$proj MINIGW_PORT=$PORT MINIGW_BIND=127.0.0.1 MINIGW_LOCAL_DIR=$D/bin
  (cd $ROOT/client && GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -o $D/bin/minigw-linux-$arch .) || fail "$arch: go build"
  cd $ROOT/deploy/docker
  local local=(-f compose.yaml -f compose.local.yaml)
  # 1. this branch's binary
  MINIGW_VERSION=local docker compose $local up -d --build >/dev/null 2>&1 || fail "$arch: up (local binary)"
  healthy $proj || fail "$arch: not healthy"; ok "$plat: local binary healthy"
  [[ $(docker image inspect --format '{{.Architecture}}' minigw:local) == $arch ]] || fail "$arch: wrong image architecture"
  docker compose $local exec -T minigw minigw gw status | grep -q "gateway: running" || fail "$arch: gw status"; ok "$plat: gw status through exec"
  docker compose $local exec -T minigw minigw serve healthcheck || fail "$arch: serve healthcheck"; ok "$plat: serve healthcheck"
  # a provider by Compose service name: the stub is a container named litellm on the project network
  docker stop $STUB >/dev/null 2>&1
  docker run --rm -d --name $STUB --network ${proj}_default python:3-alpine python -c "
import http.server,json
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(s):
        s.send_response(200);s.send_header('Content-Type','application/json');s.end_headers();s.wfile.write(json.dumps({'data':[{'id':'fake-model'}]}).encode())
http.server.HTTPServer(('',4000),H).serve_forever()" >/dev/null || fail "$arch: stub"
  sleep 3
  print fakekey | docker compose $local exec -T minigw minigw gw provider add Stub http://litellm:4000 --insecure 2>&1 | grep -q "1 models" || fail "$arch: provider by service name"
  ok "$plat: provider http://litellm:4000 (service name) added"
  docker compose $local exec -T minigw minigw gw provider add Bad http://example.com:4000 --insecure </dev/null >/dev/null 2>&1 && fail "$arch: public http name accepted"
  ok "$plat: public http name refused"
  docker stop $STUB >/dev/null 2>&1
  # settings page and admin API are not reachable from outside the container
  for p in /minigw/settings /minigw/admin/v1/status; do
    c=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 http://127.0.0.1:$PORT$p)
    [[ $c == 426 || $c == 403 || $c == 401 || $c == 404 || $c == 000 ]] || fail "$arch: $p answered $c from outside"
    ok "$plat: $p from outside -> $c (refused: plain HTTP from outside needs the encrypted protocol)"
  done
  MINIGW_VERSION=local docker compose $local restart minigw >/dev/null 2>&1; healthy $proj || fail "$arch: not healthy after restart"
  docker compose $local exec -T minigw minigw gw provider list | grep -q "^Stub " || fail "$arch: config lost on restart"; ok "$plat: restart keeps the config"
  MINIGW_VERSION=local docker compose $local stop >/dev/null 2>&1
  docker compose $local logs 2>&1 | grep -q "shutdown complete" || fail "$arch: no clean shutdown line"
  [[ $(docker inspect --format '{{.State.ExitCode}}' $proj-minigw-1) == 0 ]] || fail "$arch: exit code"; ok "$plat: clean shutdown, exit 0"
  # 2. the released path, in its own project (fresh volumes: a release older than this branch must not read this branch's config)
  export COMPOSE_PROJECT_NAME=$proj-rel; proj=$proj-rel
  out=$(MINIGW_VERSION=$V docker compose build --progress=plain 2>&1) || { print -r -- $out | tail -20 >&2; fail "$arch: released build"; }
  print -r -- $out | grep -q "verified: minigw v$V $arch" || fail "$arch: no verification line"; ok "$plat: released v$V built and verified"
  MINIGW_VERSION=$V docker compose up -d >/dev/null 2>&1; healthy $proj || fail "$arch: released not healthy"
  docker compose exec -T minigw minigw --version | head -1 | grep -q "$V" || fail "$arch: version"; ok "$plat: released v$V healthy"
  MINIGW_VERSION=$V docker compose stop >/dev/null 2>&1
  unset DOCKER_DEFAULT_PLATFORM; cd $ROOT
}

negative() { # build must FAIL with the given text; $1 case (path under the fixture), $2 version, $3 text
  local out
  out=$(docker build --progress=plain --target fetch-release --build-arg VERSION=$2 \
        --build-arg RELEASE_BASE=http://host.docker.internal:$FPORT/$1 -f $ROOT/deploy/docker/Dockerfile $ROOT/deploy/docker 2>&1)
  [[ $? -ne 0 ]] || fail "negative $1: the build SUCCEEDED"
  print -r -- $out | grep -q -- "$3" || { print -r -- $out | tail -8 >&2; fail "negative $1: failed, but not with '$3'"; }
  ok "build fails: $1 ($3)"
}

# 3. fixtures: the real release files and tampered copies; the server is a container on 127.0.0.1
fixtures() {
  local arch=$(uname -m); arch=${${arch/x86_64/amd64}/arm64/arm64}
  local f=minigw-linux-$arch
  mkdir -p $D/fix/real/v$V
  for n in $f SHA256SUMS SHA256SUMS.sig; do [[ -s $D/fix/real/v$V/$n ]] || curl -fsSL -o $D/fix/real/v$V/$n $REL/$n || fail "download $n"; done
  local r=$D/fix/real/v$V
  mkdir -p $D/fix/badbin/v$V $D/fix/badsums/v$V $D/fix/badsig/v$V $D/fix/noasset/v$V $D/fix/wrongver/v0.0.1
  cp $r/SHA256SUMS $r/SHA256SUMS.sig $D/fix/badbin/v$V/; cp $r/$f $D/fix/badbin/v$V/$f; printf 'x' >> $D/fix/badbin/v$V/$f
  cp $r/$f $r/SHA256SUMS.sig $D/fix/badsums/v$V/
  awk -v f="$f" '$2 == f { c=substr($1,1,1); $1=(c=="0"?"1":"0") substr($1,2) } { print $1 "  " $2 }' $r/SHA256SUMS > $D/fix/badsums/v$V/SHA256SUMS
  cp $r/$f $r/SHA256SUMS $D/fix/badsig/v$V/
  awk 'NR==3 { c=substr($0,1,1); $0=(c=="A"?"B":"A") substr($0,2) } { print }' $r/SHA256SUMS.sig > $D/fix/badsig/v$V/SHA256SUMS.sig
  cp $r/SHA256SUMS $r/SHA256SUMS.sig $D/fix/noasset/v$V/
  cp $r/$f $r/SHA256SUMS $r/SHA256SUMS.sig $D/fix/wrongver/v0.0.1/
  cmp -s $r/SHA256SUMS $D/fix/badsums/v$V/SHA256SUMS && fail "fixture badsums is not tampered"
  cmp -s $r/SHA256SUMS.sig $D/fix/badsig/v$V/SHA256SUMS.sig && fail "fixture badsig is not tampered"
  docker stop $FIX >/dev/null 2>&1
  docker run --rm -d --name $FIX -p 127.0.0.1:$FPORT:8000 -v $D/fix:/srv:ro python:3-alpine python -m http.server 8000 -d /srv >/dev/null || fail "fixture server"
  sleep 2
  curl -fsS -o /dev/null http://127.0.0.1:$FPORT/real/v$V/SHA256SUMS || fail "fixture server not answering"
}

for a in arm64 amd64; do run_platform $a; done
fixtures
# control: the untampered files through the fixture server build fine, so the failures below come from the tampering
out=$(docker build --progress=plain --target fetch-release --build-arg VERSION=$V --build-arg RELEASE_BASE=http://host.docker.internal:$FPORT/real \
      -f $ROOT/deploy/docker/Dockerfile $ROOT/deploy/docker 2>&1) || { print -r -- $out | tail -8 >&2; fail "control build through the fixture server"; }
ok "control: real files through the fixture server verify"
negative badbin $V "checksum mismatch"
negative badsums $V "is not signed by Mini Gateway"
negative badsig $V "is not signed by Mini Gateway"
negative noasset $V "exit code: 22"
negative real 0.0.9 "exit code: 22"      # no such version: nothing to download
negative wrongver 0.0.1 "signed version"
$ROOT/scripts/compose-admin-e2e.sh || fail "admin profile e2e (scripts/compose-admin-e2e.sh)"
(( $#SKIPPED )) && print "DONE with SKIPPED: $SKIPPED (not tested)" || print "DONE: all platforms and cases passed"
