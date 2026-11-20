#!/bin/sh
# Rehearsal of the team gateway deployment (docs/docker.md, "Rehearsal before inviting the team").
# POSIX sh (checked with dash and bash --posix). Run it ON THE SERVER, from a clone of the repository, as a user that
# may use Docker (rootful Docker with the Compose plugin): sh scripts/compose-team-rehearsal.sh
#
# It builds the release image the way docs/docker.md does (the build checks the signature of SHA256SUMS, the signed
# version and the checksum), starts an ISOLATED Compose project (own name, own ports on the bind address, own volumes),
# runs the team workflow through the owner CLI and through simulated devices, optionally the remote settings profile,
# a backup and a restore into a second isolated project, and writes rehearsal-report-<UTC>.md in the current
# directory without secrets, host names, addresses or user names. Exit code 0 only if no step FAILED.
# It never touches another Compose project: it refuses a name that is in use, and removes only what it created.
# Needs: docker compose v2, python3 (backup.sh needs it too); with --admin also openssl and curl.
set -u

DEPLOY=$(cd "$(dirname "$0")/../deploy/docker" 2>/dev/null && pwd) || { echo "deploy/docker not found next to scripts/" >&2; exit 2; }
SELF=$0
PROJ=minigw-rehearsal VERSION= PORT= APORT= BIND=127.0.0.1 ADMIN=0 ANAME=settings.rehearsal.test PWFILE= PROVIDER=
REUSE=0 CLEANUP=0 CLEANONLY=0 WORK=

usage() {
  cat >&2 <<'EOF'
usage: compose-team-rehearsal.sh [options]
  --project NAME            Compose project of the rehearsal (default minigw-rehearsal; never an existing one)
  --version X.Y.Z           release to build (default: MINIGW_VERSION of deploy/docker/.env)
  --port N                  gateway port on the bind address (default: a free one)
  --bind ADDR               address the rehearsal publishes on (default 127.0.0.1)
  --admin                   also rehearse the remote web settings profile (needs openssl and curl)
  --admin-name NAME         external DNS name of the page (default settings.rehearsal.test; reached with curl --resolve)
  --admin-port N            HTTPS port of the page (default: a free one)
  --admin-password-file F   password for the remote page (first line); default: generated, shown once on the terminal
  --provider-url URL        an OpenAI-compatible endpoint (for example LiteLLM) to add; the key is asked without echo
  --reuse                   continue a leftover rehearsal project made by this script (it must carry its marker volume)
  --cleanup                 remove what the rehearsal created when it ends (without it: ask on a terminal, else keep)
  --cleanup-only            only remove what an earlier run created in --project, then exit
  --workdir DIR             private working directory (default: ${TMPDIR:-/tmp}/<project>-work)
EOF
  exit 2
}

while [ $# -gt 0 ]; do
  case $1 in
    --project) PROJ=${2:?}; shift 2;;
    --version) VERSION=${2:?}; shift 2;;
    --port) PORT=${2:?}; shift 2;;
    --bind) BIND=${2:?}; shift 2;;
    --admin) ADMIN=1; shift;;
    --admin-name) ANAME=${2:?}; shift 2;;
    --admin-port) APORT=${2:?}; shift 2;;
    --admin-password-file) PWFILE=${2:?}; shift 2;;
    --provider-url) PROVIDER=${2:?}; shift 2;;
    --reuse) REUSE=1; shift;;
    --cleanup) CLEANUP=1; shift;;
    --cleanup-only) CLEANONLY=1; shift;;
    --workdir) WORK=${2:?}; shift 2;;
    *) usage;;
  esac
done

# ---------- environment: nothing of the operator's own deployment may leak in ----------
unset COMPOSE_FILE COMPOSE_PROFILES COMPOSE_ENV_FILES COMPOSE_PATH_SEPARATOR COMPOSE_PROJECT_NAME MINIGW_ADMIN_BIND MINIGW_ADMIN_PORT MINIGW_ADMIN_NAME MINIGW_ADMIN_CERTS
umask 077
exec 3<&0 </dev/null   # fd 3 keeps the terminal for the provider key and the cleanup question; everything else reads nothing
case $PROJ in *[!a-z0-9_-]*|'') echo "project name: lowercase letters, digits, - and _ only" >&2; exit 2;; esac
[ "$PROJ" != minigw ] || { echo "refusing the project name minigw: that is the default production project" >&2; exit 2; }
PROJ2=$PROJ-restore
[ -n "$WORK" ] || WORK=${TMPDIR:-/tmp}/$PROJ-work
case $WORK in /*) ;; *) WORK=$(pwd)/$WORK;; esac
MARK=$WORK/.rehearsal-marker
DEB=debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2   # the Dockerfile's base, used by backup.sh too
FAILS=0 LAST=$(date +%s) START=$LAST
UP=0 USERS=0 FLOW=0 SENT=0 PW= S= TOKEN= KEY= PRE=0 P2=
RID=

# ---------- helpers ----------
now() { date +%s; }
rnd() { od -An -N6 -tx1 /dev/urandom | tr -d ' \n'; }
get() { eval "printf '%s' \"\${$1:-}\""; }
put() { eval "$1=\$2"; }
tailc() { printf '%s' "$1" | tail -c 300 | tr '\n' ' '; }

record() { # status id text
  rs=$1; ri=$2; shift 2; rt=$(now); re=$((rt - LAST)); LAST=$rt
  [ -z "${STEPS:-}" ] || printf '%s\t%s\t%s\t%s\n' "$rs" "$ri" "$re" "$*" >>"$STEPS"
}
ok() { record ok "$1" "$2"; printf 'ok   %s %s\n' "$1" "$2"; }
bad() { FAILS=$((FAILS + 1)); record FAIL "$1" "$2"; printf 'FAIL %s %s\n' "$1" "$2"; }
todo() { # id text then the exact commands (one per line) and what to expect, from stdin
  record TODO "$1" "$2"; printf 'TODO(operator) %s %s\n' "$1" "$2"
  cat >"$WORK/todo.tmp"; sed 's/^/    /' "$WORK/todo.tmp"
  { printf '### %s %s\n\n```sh\n' "$1" "$2"; cat "$WORK/todo.tmp"; printf '```\n\n'; } >>"$WORK/todo.md"
}

# ---------- the working directory belongs to this script: created by it, marked, private ----------
work_check() { # 0 only for a directory this script made for $PROJ: ours, mode 700, no symlinks, marker file mode 600
  [ -d "$WORK" ] && [ ! -L "$WORK" ] && [ -O "$WORK" ] || return 1
  [ -z "$(find "$WORK" -maxdepth 0 -perm -077 2>/dev/null)" ] || return 1
  [ -f "$MARK" ] && [ ! -L "$MARK" ] && [ -O "$MARK" ] || return 1
  [ -z "$(find "$MARK" -maxdepth 0 -perm -077 2>/dev/null)" ] || return 1
  grep -q '^marker=minigw-compose-team-rehearsal$' "$MARK" && [ "$(mk project)" = "$PROJ" ] || return 1
  [ -z "$(find "$WORK" -type l 2>/dev/null)" ] || return 1
}
mk() { sed -n "s/^$1=//p" "$MARK" 2>/dev/null | head -n 1; }
save_marker() { # what a later --reuse or --cleanup-only needs
  { printf 'marker=minigw-compose-team-rehearsal\nproject=%s\nrun=%s\nversion=%s\nport=%s\np2=%s\naport=%s\nbind=%s\nadmin=%s\naname=%s\n' \
      "$PROJ" "$RID" "$VERSION" "$PORT" "$P2" "$APORT" "$BIND" "$ADMIN" "$ANAME" >"$MARK.new" && mv -f "$MARK.new" "$MARK"; } || return 1
}
remove_work() { # only a validated directory; one file at a time, never recursive; the marker goes last
  work_check || { printf 'not removing %s: it is not a working directory made by this script for %s\n' "$WORK" "$PROJ" >&2; return 1; }
  find "$WORK" -type f ! -name .rehearsal-marker -exec rm -- {} + 2>/dev/null
  find "$WORK" -mindepth 1 -depth -type d -exec rmdir -- {} + 2>/dev/null
  if [ -e "$WORK/certs" ]; then   # keys owned by the container users (Linux): remove them through a container, not with host sudo
    docker run --rm --label "io.minigw.rehearsal.run=$RID" --label "io.minigw.rehearsal.project=$PROJ" --network none --user 0:0 --cap-drop ALL --cap-add DAC_OVERRIDE --cap-add FOWNER \
      --security-opt no-new-privileges --mount "type=bind,src=$WORK/certs,dst=/c" --entrypoint sh "$DEB" \
      -c 'find /c -type f -exec rm -- {} + ; find /c -mindepth 1 -depth -type d -exec rmdir -- {} +' >/dev/null 2>&1
    rmdir "$WORK/certs" 2>/dev/null
  fi
  [ -z "$(find "$WORK" -mindepth 1 ! -name .rehearsal-marker 2>/dev/null | head -n 1)" ] && [ ! -e "$WORK/certs" ] || { printf 'could not remove everything in %s (the marker stays); remove the rest by hand, then the directory\n' "$WORK" >&2; return 1; }
  rm -- "$MARK" && rmdir "$WORK"
}

# ---------- Docker resources: ownership is proven by labels carrying this run's id, never by a name ----------
# Compose labels everything it creates with the project; the override file in the working directory adds the run id to
# the containers, volumes and networks of Compose and gives the rehearsal image its own tag, so the shared minigw:<version> is never touched.
vlabel() { docker volume inspect --format "{{ index .Labels \"$2\" }}" "$1" 2>/dev/null; }
marker_ok() { # project: the marker volume carries this run's id and the project
  [ -n "$RID" ] && [ "$(vlabel "${1}_rehearsal_marker" io.minigw.rehearsal.run)" = "$RID" ] && [ "$(vlabel "${1}_rehearsal_marker" io.minigw.rehearsal.project)" = "$1" ]
}
found() { # everything with this project's name prefix, for a refusal message
  docker ps -a --filter "label=com.docker.compose.project=$1" --format 'container {{.Names}}'
  docker volume ls -q | grep "^${1}_" | sed 's/^/volume /'
  docker network ls --format '{{.Name}}' | grep "^${1}_" | sed 's/^/network /'
}
in_use() { # project name: any container, network or volume of it
  [ -z "$(docker ps -aq --filter "label=com.docker.compose.project=$1")" ] || return 0
  docker volume ls -q | grep -q "^${1}_" && return 0
  docker network ls --format '{{.Name}}' | grep -q "^${1}_" && return 0
  return 1
}
leftovers() { # project: what of this run still exists (containers, networks, volumes)
  { docker ps -aq --filter "label=io.minigw.rehearsal.run=$RID" --filter "label=com.docker.compose.project=$1"
    docker ps -aq --filter "label=io.minigw.rehearsal.run=$RID" --filter "label=io.minigw.rehearsal.project=$1"
    docker network ls -q --filter "label=io.minigw.rehearsal.run=$RID" --filter "label=com.docker.compose.project=$1"
    docker volume ls -q --filter "label=io.minigw.rehearsal.run=$RID" | grep "^${1}_" | grep -v '_rehearsal_marker$'
    [ "$1" != "$PROJ2" ] || { docker volume ls -q | grep -x -e "${PROJ2}_config" -e "${PROJ2}_scratch"; }
  } 2>/dev/null
}
remove_run() { # project: remove exactly the containers, networks and volumes of THIS run in that project, verify, then the marker
  rp=$1; rc=0
  marker_ok "$rp" || { printf 'not removing project %s: no marker volume carries this run (%s); found:\n' "$rp" "${RID:-none}" >&2; found "$rp" | sed 's/^/  /' >&2; return 1; }
  for c in $(docker ps -aq --filter "label=io.minigw.rehearsal.run=$RID" --filter "label=com.docker.compose.project=$rp") \
           $(docker ps -aq --filter "label=io.minigw.rehearsal.run=$RID" --filter "label=io.minigw.rehearsal.project=$rp"); do
    docker rm -f "$c" >/dev/null 2>&1 || { printf 'could not remove container %s\n' "$c" >&2; rc=1; }
  done
  for n in $(docker network ls -q --filter "label=io.minigw.rehearsal.run=$RID" --filter "label=com.docker.compose.project=$rp"); do
    docker network rm "$n" >/dev/null 2>&1 || { printf 'could not remove network %s\n' "$n" >&2; rc=1; }
  done
  for v in $(docker volume ls -q --filter "label=io.minigw.rehearsal.run=$RID" | grep "^${rp}_" | grep -v '_rehearsal_marker$'); do
    docker volume rm "$v" >/dev/null 2>&1 || { printf 'could not remove volume %s\n' "$v" >&2; rc=1; }
  done
  if [ "$rp" = "$PROJ2" ]; then   # the volumes backup.sh --restore created carry no label: exact names, in a project whose marker is ours
    for v in "${PROJ2}_config" "${PROJ2}_scratch"; do
      docker volume inspect "$v" >/dev/null 2>&1 || continue
      docker volume rm "$v" >/dev/null 2>&1 || { printf 'could not remove volume %s\n' "$v" >&2; rc=1; }
    done
  fi
  left=$(leftovers "$rp")
  if [ $rc -ne 0 ] || [ -n "$left" ]; then printf 'project %s: not everything was removed (the marker stays so that --cleanup-only can be run again); left:\n%s\n' "$rp" "$left" >&2; return 1; fi
  docker volume rm "${rp}_rehearsal_marker" >/dev/null 2>&1 || { printf 'could not remove the marker volume of %s\n' "$rp" >&2; return 1; }
  return 0
}
do_cleanup() { # both projects of the run, the image tag of the run, then the working directory; reports every failure
  cr=0
  if docker volume inspect "${PROJ2}_rehearsal_marker" >/dev/null 2>&1; then remove_run "$PROJ2" || cr=1; fi
  if docker volume inspect "${PROJ}_rehearsal_marker" >/dev/null 2>&1; then remove_run "$PROJ" || cr=1; else printf 'no marker volume for %s (already removed, or never created)\n' "$PROJ" >&2; fi
  if [ $cr -eq 0 ] && docker image inspect "minigw-rehearsal:$RID" >/dev/null 2>&1; then
    docker rmi "minigw-rehearsal:$RID" >/dev/null 2>&1 || { printf 'could not remove the image tag minigw-rehearsal:%s\n' "$RID" >&2; cr=1; }
  fi
  [ $cr -eq 0 ] || { printf 'cleanup incomplete: fix the messages above, then run: sh %s --cleanup-only --project %s --workdir %s\n' "$SELF" "$PROJ" "$WORK" >&2; return 1; }
  remove_work
}

# ---------- Compose in the rehearsal context ----------
BASEFILES=compose.yaml:$WORK/compose.rehearsal.yaml
ADMINFILES=compose.yaml:$WORK/compose.rehearsal.yaml:compose.admin.yaml:$WORK/compose.rehearsal-admin.yaml
OVERLAY=$BASEFILES
write_override() { # the run id on every Compose resource, and a private image tag
  cat >"$WORK/compose.rehearsal.yaml" <<EOF
services:
  minigw:
    image: minigw-rehearsal:$RID
    labels:
      io.minigw.rehearsal.run: "$RID"
volumes:
  config:
    labels:
      io.minigw.rehearsal.run: "$RID"
  scratch:
    labels:
      io.minigw.rehearsal.run: "$RID"
networks:
  default:
    labels:
      io.minigw.rehearsal.run: "$RID"
EOF
  cat >"$WORK/compose.rehearsal-admin.yaml" <<EOF
services:
  admin-proxy:
    labels:
      io.minigw.rehearsal.run: "$RID"
networks:
  admin:
    labels:
      io.minigw.rehearsal.run: "$RID"
EOF
}
use_project() { # name port
  CPROJ=$1; CPORT=$2
  export COMPOSE_PROJECT_NAME=$CPROJ MINIGW_PORT=$CPORT MINIGW_VERSION=$VERSION MINIGW_BIND=$BIND \
    MINIGW_ADMIN_BIND=$BIND MINIGW_ADMIN_PORT=${APORT:-8443} MINIGW_ADMIN_NAME=$ANAME MINIGW_ADMIN_CERTS=$WORK/certs
  export COMPOSE_FILE=$OVERLAY
  NET=${CPROJ}_default
}
dc() { ( cd "$DEPLOY" && docker compose "$@" ); }
gw() { dc exec -T minigw minigw gw "$@"; }
# a simulated device: a client run from a second container of the same image on the Compose network, with its own home volume
cl() { # user dir args...
  cu=$1; cd_=$2; shift 2
  # the image carries Compose's project label from its build: override it so that these clients never count as containers of a project
  docker run --rm -i --label com.docker.compose.project= --label "io.minigw.rehearsal.run=$RID" --label "io.minigw.rehearsal.project=$PROJ" --network "$NET" \
    --user 10001:10001 --cap-drop ALL --security-opt no-new-privileges --read-only \
    --tmpfs /tmp:size=16m,mode=1777 -e HOME=/home/minigw -e MINIGW_SECRETS=file \
    -v "${PROJ}_home_$cu:/home/minigw" -w "$cd_" --entrypoint minigw "$IMAGE" "$@"
}
wait_healthy() {
  wi=0
  while [ $wi -lt 60 ]; do
    wc=$(dc ps -q minigw 2>/dev/null)
    [ -z "$wc" ] || [ "$(docker inspect --format '{{.State.Health.Status}}' "$wc" 2>/dev/null)" != healthy ] || return 0
    sleep 3; wi=$((wi + 1))
  done
  return 1
}
port_free() { python3 -c 'import socket,sys; s=socket.socket(); s.bind((sys.argv[1],int(sys.argv[2])))' "$1" "$2" 2>/dev/null; }
pick_port() { python3 -c 'import socket,sys; s=socket.socket(); s.bind((sys.argv[1],0)); print(s.getsockname()[1])' "$1"; }

on_interrupt() {
  stty echo <&3 2>/dev/null
  printf '\ninterrupted. What was created is recorded in %s. To remove exactly that, and nothing else:\n  sh %s --cleanup-only --project %s --workdir %s\n' "$WORK" "$SELF" "$PROJ" "$WORK" >&2
  exit 130
}

# ---------- start: validate everything BEFORE the first write ----------
command -v docker >/dev/null || { echo "docker not found" >&2; exit 2; }
if [ "$CLEANONLY" = 1 ]; then
  if ! work_check; then
    printf 'refusing: %s is not a working directory made by this script for project %s (no valid marker). Docker resources found under that name, left untouched:\n' "$WORK" "$PROJ" >&2
    { found "$PROJ"; found "$PROJ2"; } | sed 's/^/  /' >&2
    exit 1
  fi
  RID=$(mk run); VERSION=$(mk version); IMAGE=minigw-rehearsal:$RID
  [ -n "$RID" ] || { echo "the marker has no run id" >&2; exit 1; }
  do_cleanup && echo "removed what run $RID created: project $PROJ (and $PROJ2 if it was left), the image tag and the working directory" && exit 0
  exit 1
fi
# the placeholder-free version: the one asked for, else MINIGW_VERSION of deploy/docker/.env
[ -n "$VERSION" ] || VERSION=$(sed -n 's/^MINIGW_VERSION=//p' "$DEPLOY/.env" 2>/dev/null | tail -n 1)
if [ "$REUSE" = 1 ]; then
  work_check || { echo "refusing --reuse: $WORK is not a working directory made by this script for project $PROJ (valid marker file, mode 700, no symlinks)" >&2; exit 2; }
  RID=$(mk run); [ -n "$RID" ] || { echo "the marker has no run id" >&2; exit 2; }
  [ -n "$VERSION" ] || VERSION=$(mk version)
  [ -n "$PORT" ] || PORT=$(mk port); [ -n "$APORT" ] || APORT=$(mk aport); P2=$(mk p2)
else
  RID=$(date -u +%Y%m%d%H%M%S)-$(rnd)
fi
[ -n "$VERSION" ] || { echo "no release: pass --version X.Y.Z or set MINIGW_VERSION in deploy/docker/.env (docs/docker.md, first setup, step 2)" >&2; exit 2; }
IMAGE=minigw-rehearsal:$RID
if [ "$REUSE" != 1 ]; then
  mkdir -m 700 "$WORK" || { echo "cannot create the working directory $WORK: it must not exist (the script makes it; --reuse only for its own leftover)" >&2; exit 2; }
  ( set -C; : >"$MARK" ) || { echo "cannot create the marker in $WORK" >&2; exit 2; }
  chmod 600 "$MARK"
  save_marker || { echo "cannot write the marker" >&2; exit 2; }
fi
trap on_interrupt INT TERM HUP
write_override
STEPS=$WORK/steps.tsv; : >"$STEPS"; : >"$WORK/todo.md"; : >"$WORK/matrix.txt"; : >>"$WORK/patterns"
echo "Rehearsal of the team gateway: project $PROJ, release $VERSION, run $RID, working directory $WORK"

# ====================================================================================================
# 1. Preconditions
# ====================================================================================================
stage_pre() {
  PRE=1
  dv=$(docker version --format '{{.Server.Version}}' 2>&1) && ok 1.docker "docker server $dv" || { bad 1.docker "docker is not usable: $(tailc "$dv")"; PRE=0; }
  cv=$(docker compose version --short 2>&1) && case $cv in 2.*|v2.*|[3-9]*|v[3-9]*) ok 1.compose "docker compose $cv";; *) bad 1.compose "Compose v2 is needed, found: $cv"; PRE=0;; esac || { bad 1.compose "docker compose plugin missing"; PRE=0; }
  command -v python3 >/dev/null && ok 1.python3 "python3 present (backup.sh needs it too)" || { bad 1.python3 "python3 is missing: backup.sh needs it"; PRE=0; }
  if [ "$ADMIN" = 1 ]; then
    command -v openssl >/dev/null && command -v curl >/dev/null && ok 1.tools "openssl and curl present (admin profile)" || { bad 1.tools "--admin needs openssl and curl"; PRE=0; }
  fi
  [ "$PRE" = 1 ] || return
  # isolation: nothing of this name may exist
  for p in "$PROJ" "$PROJ2"; do
    if in_use "$p"; then
      if [ "$p" = "$PROJ" ] && [ "$REUSE" = 1 ] && marker_ok "$p"; then ok 1.name "reusing the leftover rehearsal project $p (its marker volume carries this run's id)"
      elif [ "$p" = "$PROJ2" ] && [ "$REUSE" = 1 ] && marker_ok "$p" && remove_run "$p"; then ok 1.name "removed the leftover restore project $p of this run (marker verified)"
      else bad 1.name "Compose project $p already has containers, networks or volumes and was not made by this run: pick another --project (--reuse only continues this script's own leftover, proven by the marker volume and the working directory)"; PRE=0; fi
    else ok 1.name "project name $p is unused (no containers, networks or volumes)"; fi
  done
  [ "$PRE" = 1 ] || return
  # ports
  [ -n "$PORT" ] || PORT=$(pick_port "$BIND"); P2=$(pick_port "$BIND")
  [ -n "$APORT" ] || APORT=$(pick_port "$BIND")
  while [ "$P2" = "$PORT" ] || [ "$P2" = "$APORT" ]; do P2=$(pick_port "$BIND"); done
  if [ "$REUSE" = 1 ]; then ok 1.ports "ports $PORT (gateway), $P2 (restore), $APORT (page): leftover project keeps its own"; else
    pf=1; for pp in $PORT $P2 $APORT; do port_free "$BIND" "$pp" || { bad 1.ports "port $pp is not free on $BIND"; pf=0; PRE=0; }; done
    [ "$pf" = 0 ] || ok 1.ports "free ports on the bind address: gateway $PORT, restore $P2, remote page $APORT"
  fi
  # disk: the Docker data directory if it is visible from here, else the working directory's file system
  root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null); [ -d "$root" ] || root=$WORK
  free=$(df -Pk "$root" 2>/dev/null | awk 'NR==2{print $4}')
  if [ -n "$free" ] && [ "$free" -ge 2097152 ]; then ok 1.disk "free disk for images and volumes: $((free / 1024)) MiB (needs 2048)"; else bad 1.disk "less than 2 GiB free (${free:-unknown} KiB) where Docker stores its data"; PRE=0; fi
  save_marker || { bad 1.marker "cannot write the run marker"; PRE=0; }
  [ "$BIND" = 127.0.0.1 ] || echo "note: the rehearsal publishes on $BIND: protect it with the host firewall like the real deployment"
  [ "$PRE" = 1 ] || return
  # the marker volume anchors everything this run creates (labels carry the run id); fresh runs only, and only for names found unused above
  if [ "$REUSE" != 1 ]; then
    docker volume create --label "io.minigw.rehearsal.run=$RID" --label "io.minigw.rehearsal.project=$PROJ" "${PROJ}_rehearsal_marker" >/dev/null 2>&1 && marker_ok "$PROJ" || { bad 1.marker "cannot create the marker volume of this run"; PRE=0; return; }
  fi
  # the release image, built the documented way into a PRIVATE tag (minigw-rehearsal:<run id>, from compose.rehearsal.yaml), never the shared minigw:<version>
  use_project "$PROJ" "$PORT"
  bo=$(dc build --progress=plain 2>&1); brc=$?
  if [ $brc -eq 0 ] && printf '%s' "$bo" | grep -q "verified: minigw v$VERSION "; then
    IMGID=$(docker image inspect --format '{{.Id}}' "$IMAGE" 2>/dev/null | cut -c1-19)
    ok 1.release "release v$VERSION built from the GitHub release into the private tag $IMAGE (image $IMGID); signature of SHA256SUMS, signed version and checksum verified by the build"
  else bad 1.release "the release image of v$VERSION was not built and verified: $(tailc "$bo")"; PRE=0; fi
}

# ====================================================================================================
# 2. Bring up, health, readiness
# ====================================================================================================
stage_up() {
  [ "${PRE:-0}" = 1 ] || { bad 2.up "not run: preconditions failed"; return; }
  uo=$(dc up -d 2>&1) || { bad 2.up "docker compose up failed: $(tailc "$uo")"; return; }
  wait_healthy && ok 2.health "container healthy (Docker HEALTHCHECK: minigw serve healthcheck)" || { bad 2.health "container not healthy within 3 minutes: $(tailc "$(dc logs --tail 5 minigw 2>&1)")"; return; }
  ho=$(dc exec -T minigw minigw serve healthcheck 2>&1) && ok 2.liveness "docker compose exec minigw minigw serve healthcheck" || { bad 2.liveness "serve healthcheck: $(tailc "$ho")"; return; }
  so=$(gw status 2>&1) || { bad 2.status "gw status: $(tailc "$so")"; return; }
  if printf '%s' "$so" | grep -q '^gateway: running' && printf '%s' "$so" | grep -q '^projects: on' && printf '%s' "$so" | grep -q "version $VERSION"; then
    ok 2.status "gw status: gateway running, version $VERSION, projects: on (core database open)"; UP=1
  else bad 2.status "gw status does not say running, version $VERSION and projects: on: $(tailc "$so")"; fi
}

# ====================================================================================================
# 3. Bootstrap: team, projects, users, devices
# ====================================================================================================
ensure() { # kind name args... ; kind: user|team|project
  ek=$1; en=$2; shift 2
  if gw "$ek" list 2>/dev/null | grep -qw -- "$en"; then return 0; fi
  gw "$ek" add "$en" "$@"
}
onboard() { # user: allow and bind the simulated device
  ou=$1
  docker volume create --label "io.minigw.rehearsal.run=$RID" --label "io.minigw.rehearsal.project=$PROJ" "${PROJ}_home_$ou" >/dev/null || return 1
  printf '%s\n' "$TOKEN" | cl "$ou" /home/minigw connect minigw:8765 --token - >/dev/null 2>&1
  od=$(cl "$ou" /home/minigw status 2>/dev/null | sed -n 's/^Device: *\([^ ]*\).*/\1/p')
  [ -n "$od" ] || return 1
  gw devices allow "$od" --name "$ou" >/dev/null && gw bind "$od" "$ou" >/dev/null || return 1
  printf '%s\n' "$TOKEN" | cl "$ou" /home/minigw connect minigw:8765 --token - 2>&1 | grep -q '^Connected' || return 1
  docker run --rm --label com.docker.compose.project= --label "io.minigw.rehearsal.run=$RID" --label "io.minigw.rehearsal.project=$PROJ" --network none --user 10001:10001 --cap-drop ALL --security-opt no-new-privileges -v "${PROJ}_home_$ou:/home/minigw" --entrypoint mkdir "$IMAGE" -p /home/minigw/p1 /home/minigw/p2 || return 1
  put "DEV_$(printf %s "$ou" | tr - _)" "$od"
}
stage_boot() {
  [ "$UP" = 1 ] || { bad 3.bootstrap "not run: the gateway did not come up"; return; }
  TOKEN=$(gw token 2>/dev/null | grep -o 'mgw-[A-Za-z0-9_-]*#[A-Za-z0-9_-]*' | head -n 1)
  [ -n "$TOKEN" ] || { bad 3.token "gw token printed no connect line"; return; }
  ok 3.token "owner CLI: gw token printed the connect line (not shown here)"
  bo=
  for t in rh-team; do ensure team "$t" >/dev/null 2>&1 || bo="$bo team $t"; done
  ensure project rh-p1 "Rehearsal P1" >/dev/null 2>&1 || bo="$bo project rh-p1"
  ensure project rh-p2 "Rehearsal P2" >/dev/null 2>&1 || bo="$bo project rh-p2"
  for u in rh-reader rh-developer rh-admin; do
    ensure user "$u" >/dev/null 2>&1 || bo="$bo user $u"
    gw team member add rh-team "$u" >/dev/null 2>&1 || bo="$bo member $u"
  done
  gw team project add rh-team rh-p1 >/dev/null 2>&1 || bo="$bo assign p1"
  gw team project add rh-team rh-p2 >/dev/null 2>&1 || bo="$bo assign p2"
  # p1: three direct roles. p2: the developer through the team, the admin direct, the reader only in the team (no role)
  gw member rh-p1 set rh-reader reader >/dev/null 2>&1 || bo="$bo p1-reader"
  gw member rh-p1 set rh-developer developer >/dev/null 2>&1 || bo="$bo p1-developer"
  gw member rh-p1 set rh-admin admin >/dev/null 2>&1 || bo="$bo p1-admin"
  gw team access set rh-team rh-p2 rh-developer developer >/dev/null 2>&1 || bo="$bo p2-developer"
  gw member rh-p2 set rh-admin admin >/dev/null 2>&1 || bo="$bo p2-admin"
  [ -z "$bo" ] || { bad 3.bootstrap "owner CLI commands failed:$bo"; return; }
  ok 3.bootstrap "team rh-team, projects rh-p1 and rh-p2, users reader/developer/admin and their roles created with the owner CLI (docker compose exec)"
  bo=
  for u in rh-reader rh-developer rh-admin; do onboard "$u" || bo="$bo $u"; done
  [ -z "$bo" ] || { bad 3.devices "simulated device could not be connected, approved and bound for:$bo"; return; }
  ok 3.devices "3 SIMULATED devices: a client run from a second container of the same image on the Compose network, each with its own home volume; device allowed and bound with gw devices allow / gw bind"
  bo=
  for u in rh-reader rh-developer rh-admin; do cl "$u" /home/minigw/p1 project link rh-p1 >/dev/null 2>&1 || bo="$bo $u@p1"; done
  cl rh-developer /home/minigw/p2 project link rh-p2 >/dev/null 2>&1 || bo="$bo rh-developer@p2"
  cl rh-admin /home/minigw/p2 project link rh-p2 >/dev/null 2>&1 || bo="$bo rh-admin@p2"
  if cl rh-reader /home/minigw/p2 project link rh-p2 >/dev/null 2>&1; then bo="$bo reader-could-link-p2"; fi
  [ -z "$bo" ] || { bad 3.links "project link: unexpected result for:$bo"; return; }
  ok 3.links "project link works for each member and is refused for the reader on rh-p2 (team membership alone gives no project access)"
  USERS=1
}

# ====================================================================================================
# 4. Access matrix
# ====================================================================================================
expect_vis() { # user project class
  case $1:$2:$3 in
    rh-reader:p1:data|rh-reader:p1:private|rh-reader:p2:*|rh-developer:p2:private) echo n;;   # a reader reads structure only
    *) echo y;;
  esac
}
stage_matrix() {
  [ "$USERS" = 1 ] || { bad 4.sentinels "not run: users and devices were not set up"; return; }
  bo=
  for pc in p1:rh-developer p2:rh-admin; do
    p=${pc%%:*}; author=${pc#*:}
    for c in structure data private; do
      s=rh$(printf %s "$c" | cut -c1)$p$(rnd)
      printf 'rehearsal %s note %s\n' "$c" "$s" | cl "$author" /home/minigw/$p note push --class "$c" --title "$s" - >"$WORK/push.out" 2>&1
      id=$(sed -n 's/^note \([0-9][0-9]*\) created.*/\1/p' "$WORK/push.out")
      [ -n "$id" ] || { bo="$bo $p/$c"; continue; }
      put "SENT_${p}_$c" "$s"; put "NID_${p}_$c" "$id"
    done
  done
  [ -z "$bo" ] || { bad 4.sentinels "could not push the sentinel notes:$bo"; return; }
  ok 4.sentinels "6 sentinel notes (structure, data, private in each project) with random strings made at run time"
  printf '%-14s %-4s %-9s %-5s %-6s %-5s %-8s %s\n' user proj class list search read expected result >"$WORK/matrix.txt"
  mbad=0
  for u in rh-reader rh-developer rh-admin; do
    for p in p1 p2; do
      for c in structure data private; do
        s=$(get "SENT_${p}_$c"); id=$(get "NID_${p}_$c"); slug=rh-$p
        L=n; S=n; R=n
        cl "$u" /home/minigw/$p note list 2>&1 | grep -qF -- "$s" && L=y
        cl "$u" /home/minigw/$p kb search "$slug" "$s" 2>&1 | grep -qF -- "$s" && S=y
        cl "$u" /home/minigw/$p note read "$id" 2>&1 | grep -qF -- "$s" && R=y
        want=$(expect_vis "$u" "$p" "$c")
        if [ "$L$S$R" = "$want$want$want" ]; then res=ok; else res=FAIL; mbad=1; fi
        printf '%-14s %-4s %-9s %-5s %-6s %-5s %-8s %s\n' "$u" "$p" "$c" "$L" "$S" "$R" "$want" "$res" >>"$WORK/matrix.txt"
      done
    done
  done
  # who sees which project in their own list
  for u in rh-reader rh-developer rh-admin; do
    pl=$(cl "$u" /home/minigw/p1 project list 2>&1)
    for p in rh-p1 rh-p2; do
      case $u:$p in rh-reader:rh-p2) want=n;; *) want=y;; esac
      have=n; printf '%s' "$pl" | grep -qw -- "$p" && have=y
      if [ "$have" = "$want" ]; then res=ok; else res=FAIL; mbad=1; fi
      printf '%-14s %-4s %-9s %-5s %-6s %-5s %-8s %s\n' "$u" "${p#rh-}" project-list "$have" - - "$want" "$res" >>"$WORK/matrix.txt"
    done
  done
  cat "$WORK/matrix.txt"
  if [ "$mbad" = 0 ]; then ok 4.matrix "access matrix as expected: the reader sees structure only and nothing of rh-p2, the developer does not see the admin's private note, the admin sees all; list, search and read agree"; SENT=1
  else bad 4.matrix "access matrix differs from the expectation (see the matrix)"; fi
}

# ====================================================================================================
# 5. Knowledge flow
# ====================================================================================================
sub_id() { sed -n 's/^submission \([0-9][0-9]*\) pending.*/\1/p' | head -n 1; }
review_hash() { # submission id; reads the review output
  awk -v n="$1" '$1=="submission" && $2==n {f=1; next} f && $1=="version" {print $2; exit} $1=="submission" {f=0}'
}
stage_flow() {
  [ "$SENT" = 1 ] || { bad 5.flow "not run: the access matrix step did not complete"; return; }
  t1=rhf$(rnd); t2=rhg$(rnd)
  fo=$(printf 'finding body %s\n' "$t1" | cl rh-developer /home/minigw/p1 kb submit rh-p1 --kind finding --title "F-$t1" - 2>&1); fid=$(printf '%s' "$fo" | sub_id)
  [ -n "$fid" ] || { bad 5.submit "member submission failed: $(tailc "$fo")"; return; }
  if cl rh-developer /home/minigw/p1 kb search rh-p1 "$t1" 2>&1 | grep -qF -- "$t1"; then bad 5.submit "a pending submission is already visible in search"; return; fi
  ok 5.submit "a member submitted a finding (submission $fid); it is not visible in search while pending"
  ro=$(gw kb review rh-p1 2>&1); fh=$(printf '%s' "$ro" | review_hash "$fid")
  { printf '%s' "$ro" | grep -qF -- "$t1" && [ ${#fh} -eq 64 ]; } || { bad 5.review "gw kb review rh-p1 did not show the submission and its version hash: $(tailc "$ro")"; return; }
  ao=$(gw kb accept "$fid" --version "$fh" 2>&1) || { bad 5.accept "gw kb accept --version: $(tailc "$ao")"; return; }
  fsrch=$(cl rh-developer /home/minigw/p1 kb search rh-p1 "$t1" 2>&1); nid=$(printf '%s' "$fsrch" | awk -v t="$t1" 'index($0,t){print $1; exit}')
  [ -n "$nid" ] || { bad 5.accept "after the accept, search does not find the finding: $(tailc "$fsrch")"; return; }
  ok 5.accept "owner review (gw kb review), accept with --version, the finding is found by search as note $nid"
  ro2=$(printf 'replacement body %s\n' "$t2" | cl rh-developer /home/minigw/p1 kb submit rh-p1 --kind finding --title "G-$t2" --replaces "$nid" --revision 1 - 2>&1); rid=$(printf '%s' "$ro2" | sub_id)
  [ -n "$rid" ] || { bad 5.replace "replacement submission failed: $(tailc "$ro2")"; return; }
  ro=$(gw kb review rh-p1 2>&1); rh=$(printf '%s' "$ro" | review_hash "$rid")
  if printf '%s' "$ro" | grep -q 'Current text (will be replaced)' && printf '%s' "$ro" | grep -qF -- "$t1" && printf '%s' "$ro" | grep -qF -- "$t2" && [ ${#rh} -eq 64 ]; then
    ok 5.replace "the review of the replacement shows the current text and the new text side by side"
  else bad 5.replace "the review of a replacement does not show both texts: $(tailc "$ro")"; return; fi
  ao=$(gw kb accept "$rid" --version "$rh" 2>&1) || { bad 5.replace "accept of the replacement: $(tailc "$ao")"; return; }
  hi=$(cl rh-admin /home/minigw/p1 note history "$nid" 2>&1)
  if printf '%s' "$hi" | grep -q 'revision 1 ' && printf '%s' "$hi" | grep -q 'revision 2 (current)'; then ok 5.history "note history shows revision 1 (the earlier version) and revision 2 (current)"
  else bad 5.history "note history of $nid: $(tailc "$hi")"; return; fi
  if cl rh-reader /home/minigw/p1 note restore "$nid" --revision 1 --current 2 >/dev/null 2>&1 || cl rh-developer /home/minigw/p1 note restore "$nid" --revision 1 --current 2 >/dev/null 2>&1; then
    bad 5.restore "a reader or the author could restore a note: restore is for project admins"; return; fi
  rs=$(cl rh-admin /home/minigw/p1 note restore "$nid" --revision 1 --current 2 2>&1) || { bad 5.restore "note restore by the admin: $(tailc "$rs")"; return; }
  rd=$(cl rh-admin /home/minigw/p1 note read "$nid" 2>&1)
  if printf '%s' "$rd" | grep -qF -- "$t1" && ! printf '%s' "$rd" | grep -qF -- "$t2"; then ok 5.restore "restore is refused for the reader and the author; the admin restored revision 1 as a new revision"
  else bad 5.restore "after the restore the note does not hold the earlier text: $(tailc "$rd")"; return; fi
  # two more submissions stay pending: the review pages and the backup comparison use them
  W1=rhw$(rnd); W2=rhx$(rnd)
  for w in $W1 $W2; do
    printf 'pending body %s\n' "$w" | cl rh-developer /home/minigw/p1 kb submit rh-p1 --kind finding --title "W-$w" - >/dev/null 2>&1 || { bad 5.pending "could not leave two submissions pending"; return; }
  done
  FLOW=1
}

# ====================================================================================================
# 6. Remote web settings (--admin)
# ====================================================================================================
CA=
req() { curl -sS --max-time 15 --cacert "$CA" --resolve "$ANAME:$APORT:127.0.0.1" -o "$WORK/body" -D "$WORK/head" -w '%{http_code}' "$@" 2>"$WORK/err" || true; }
jfind() { # the id of the first object whose JSON text contains $1
  python3 -c '
import json,sys
w=sys.argv[1]
def walk(x):
    if isinstance(x,dict):
        if "id" in x and w in json.dumps(x): print(x["id"]); raise SystemExit
        for v in x.values(): walk(v)
    elif isinstance(x,list):
        for v in x: walk(v)
walk(json.load(sys.stdin))' "$1"
}
login() { # password origin -> HTTP code, body in $WORK/body
  printf '%s' "$1" | python3 -c 'import json,sys; print(json.dumps({"password": sys.stdin.read()}))' |
    req -X POST -H "Origin: $2" -H 'Content-Type: application/json' -H 'X-Minigw-Remote-Request: 1' --data-binary @- "$URL/minigw/settings/login"
}
stage_admin() {
  if [ "$ADMIN" != 1 ]; then
    todo 6.remote "remote web settings were not rehearsed (run again with --admin)" <<EOF
# one command, then read the 6.* rows of the report:
sh $SELF --admin --project $PROJ-admin
# by hand (docs/docker.md, "Remote settings from a browser"): admin-certs.sh link and browser, gw settings password, the remoteSettings block in serve.json, COMPOSE_FILE=compose.yaml:compose.admin.yaml
# expect: the login page over HTTPS, a wrong password refused, routes outside the list answering 404
EOF
    return
  fi
  [ "$FLOW" = 1 ] || { bad 6.remote "not run: the knowledge flow did not complete"; return; }
  URL=https://$ANAME:$APORT; CERTS=$WORK/certs
  if [ -n "$PWFILE" ]; then PW=$(head -n 1 "$PWFILE" 2>/dev/null); [ -n "$PW" ] || { bad 6.password "the password file is empty or unreadable"; return; }
  else
    PW=$(python3 -c 'import secrets; print(secrets.token_urlsafe(18))')
    { printf 'Remote page password for this rehearsal (shown once, not stored in the report): %s\n' "$PW" >/dev/tty; } 2>/dev/null || printf 'Remote page password for this rehearsal (shown once, not stored in the report): %s\n' "$PW" >&2
  fi
  ( cd "$DEPLOY" && sh admin-certs.sh link "$CERTS" --origin "$URL" >"$WORK/link.out" 2>&1 && sh admin-certs.sh browser "$CERTS" "$ANAME" >"$WORK/browser.out" 2>&1 ) || { bad 6.certs "admin-certs.sh failed: $(tailc "$(cat "$WORK/link.out" "$WORK/browser.out" 2>/dev/null)")"; return; }
  if grep -qi 'PRIVATE KEY' "$WORK/link.out" "$WORK/browser.out"; then bad 6.certs "admin-certs.sh printed a key"; return; fi
  if [ "$(uname -s)" = Linux ] && [ "$(id -u)" != 0 ]; then
    if ! { sudo -n chown 10001:10001 "$CERTS/gateway" "$CERTS"/gateway/* && sudo -n chown 65532:65532 "$CERTS/proxy" "$CERTS"/proxy/*; } >/dev/null 2>&1; then
      bad 6.certs "the certificate folders must belong to uid 10001 (gateway/) and 65532 (proxy/): run the rehearsal as root or allow sudo (docs/docker.md, step 1 of the remote settings)"; return; fi
  fi
  CA=$CERTS/ca/browser-ca.pem
  ok 6.certs "admin-certs.sh link and browser created the certificates (private CA, name-constrained browser CA for $ANAME), no key printed"
  printf '%s\n' "$PW" | gw settings password >/dev/null 2>&1 || { bad 6.password "gw settings password failed"; return; }
  # the remoteSettings block of docs/docker.md, step 3 (allowedSourceCIDRs open: on this host the page's source address is the Docker gateway)
  dc exec -T minigw cat /var/lib/minigw/config/serve.json >"$WORK/serve.in" 2>/dev/null &&
  python3 -c '
import json,sys
c=json.load(open(sys.argv[1]))
c["remoteSettings"]={"enabled":True,"listen":"0.0.0.0:8766","origin":sys.argv[2],"allowedSourceCIDRs":["0.0.0.0/0"],"proxyIdentity":"settings-proxy.internal","proxyCA":"/admin-tls/link-ca.pem","certificate":"/admin-tls/gateway.pem","privateKey":"/admin-tls/gateway.key"}
json.dump(c,open(sys.argv[3],"w"),indent=1)' "$WORK/serve.in" "$URL" "$WORK/serve.out" &&
  [ -s "$WORK/serve.out" ] && dc exec -T minigw sh -c 'cat > /var/lib/minigw/config/serve.json' <"$WORK/serve.out" || { bad 6.config "could not write the remoteSettings block into serve.json"; return; }
  OVERLAY=compose.yaml:compose.admin.yaml; use_project "$PROJ" "$PORT"
  uo=$(dc up -d 2>&1) || { bad 6.proxy "docker compose up with compose.admin.yaml: $(tailc "$uo")"; return; }
  wait_healthy || { bad 6.proxy "gateway not healthy after enabling the admin profile"; return; }
  i=0; c=000; while [ $i -lt 30 ]; do c=$(req "$URL/minigw/settings"); [ "$c" = 200 ] && break; sleep 2; i=$((i + 1)); done
  if [ "$c" = 200 ] && grep -qi password "$WORK/body"; then ok 6.page "the login page loads over HTTPS with the browser CA"; else bad 6.page "the login page answered $c: $(tailc "$(cat "$WORK/err")")"; return; fi
  grep -qi '^strict-transport-security: max-age=' "$WORK/head" && ok 6.hsts "HSTS header present" || bad 6.hsts "no HSTS header"
  # a forged Origin never gets a session
  c=$(login "$PW" https://evil.example)
  if [ "$c" != 200 ] && ! grep -q '"session"' "$WORK/body"; then ok 6.origin "login with the right password and a forged Origin refused ($c)"; else bad 6.origin "forged Origin answered $c"; fi
  c=$(login "$PW" "$URL")
  S=$(sed -n 's/.*"session": *"\([0-9a-f]*\)".*/\1/p' "$WORK/body")
  if [ "$c" = 200 ] && [ -n "$S" ]; then ok 6.login "login with the password over HTTPS gives a session"; else bad 6.login "login answered $c"; return; fi
  SH="X-Minigw-Remote-Session: $S"
  c=$(req "$URL/minigw/admin/v1/providers"); [ "$c" = 401 ] && ok 6.nosession "an allowed route without a session: 401" || bad 6.nosession "no-session request answered $c"
  # routes outside the allow-list must be refused at the proxy
  rb=0
  for p in /minigw/admin/v1/status /minigw/admin/v1/core/projects /minigw/settings/link /v1/models /minigw/admin/v1/kb/notes /minigw/admin/v1/projects/1/grants; do
    c=$(req -H "$SH" "$URL$p"); [ "$c" = 404 ] || { rb=1; echo "  $p answered $c"; }
  done
  c=$(req -X DELETE -H 'Origin: '"$URL" -H "$SH" "$URL/minigw/admin/v1/kb/submissions/1"); [ "$c" = 404 ] || { rb=1; echo "  DELETE kb/submissions/1 answered $c"; }
  c=$(req --path-as-is -H "$SH" "$URL/minigw/admin/v1/..%2fstatus"); [ "$c" = 404 ] || { rb=1; echo "  unclean path answered $c"; }
  [ $rb = 0 ] && ok 6.allowlist "routes outside the allow-list (status, core, link, inference, notes, grants, DELETE, unclean path) answer 404" || bad 6.allowlist "a route outside the allow-list was not refused"
  # the Team read-only page's JSON routes
  tb=0
  for pair in users:rh-reader bindings:$DEV_rh_developer teams:rh-team projects:rh-p1; do
    p=${pair%%:*}; want=${pair#*:}
    c=$(req -H "$SH" "$URL/minigw/admin/v1/$p"); { [ "$c" = 200 ] && grep -qF -- "$want" "$WORK/body"; } || { tb=1; echo "  GET $p answered $c"; }
  done
  c=$(req -H "$SH" "$URL/minigw/admin/v1/teams"); TID=$(jfind rh-team <"$WORK/body")
  c=$(req -H "$SH" "$URL/minigw/admin/v1/projects"); PID=$(jfind rh-p1 <"$WORK/body")
  case ${TID:-x}:${PID:-x} in *[!0-9:]*|:*|*:) tb=1; echo "  team/project id not found";; esac
  if [ $tb = 0 ]; then
    for pair in teams/$TID:rh-reader teams/$TID/assignments:rh-developer projects/$PID/members:rh-developer; do
      p=${pair%%:*}; want=${pair#*:}
      c=$(req -H "$SH" "$URL/minigw/admin/v1/$p"); { [ "$c" = 200 ] && grep -qF -- "$want" "$WORK/body"; } || { tb=1; echo "  GET $p answered $c"; }
      c=$(req -X POST -H "Origin: $URL" -H "$SH" -d '{}' "$URL/minigw/admin/v1/$p"); [ "$c" = 404 ] || { tb=1; echo "  POST $p answered $c"; }
    done
  fi
  [ $tb = 0 ] && ok 6.team "Team read-only routes: users, bindings, teams, projects, a team, its assignments and a project's members answer 200 with the data; a write answers 404" || bad 6.team "a Team read-only route failed"
  # Review: list, read, reject
  KB=$URL/minigw/admin/v1/kb/submissions
  c=$(req -H "$SH" "$KB"); lw=0
  { [ "$c" = 200 ] && grep -qF -- "W-$W1" "$WORK/body" && grep -qF -- "W-$W2" "$WORK/body"; } || lw=1
  id1=$(jfind "W-$W1" <"$WORK/body"); id2=$(jfind "W-$W2" <"$WORK/body")
case ${id1:-x}:${id2:-x} in *[!0-9:]*|:*|*:) lw=1;; esac
[ $lw = 0 ] && ok 6.review.list "GET kb/submissions lists the two pending submissions" || { bad 6.review.list "the review list answered $c without both submissions"; return; }
  c=$(req -H "$SH" "$KB/$id1")
  if [ "$c" = 200 ] && grep -qF -- "$W1" "$WORK/body" && grep -qE '"reviewHash":"[0-9a-f]{64}"' "$WORK/body"; then ok 6.review.read "GET kb/submissions/$id1 shows the text and a review hash"; else bad 6.review.read "read answered $c"; fi
  c=$(req -X POST -H "Origin: $URL" -H 'Content-Type: application/json' -H 'X-Minigw-Remote-Request: 1' -H "$SH" -d '{"reason":"rehearsal"}' "$KB/$id2/reject")
  if [ "$c" = 200 ] && grep -qF '"state":"rejected"' "$WORK/body"; then ok 6.review.reject "POST kb/submissions/$id2/reject with a reason: rejected"; else bad 6.review.reject "reject answered $c"; fi
  gw kb review rh-p1 2>&1 | grep -qF -- "$W2" && bad 6.review.cli "the rejected submission is still pending in the owner's review" || ok 6.review.cli "the owner's gw kb review no longer lists the rejected submission"
  # where the gateway saw the browser (informational: Docker hides the browser address on Docker Desktop and rootless Docker)
  seen=$(dc exec -T minigw tail -n 20 /var/lib/minigw/config/remote-settings.audit.jsonl 2>/dev/null | sed -n 's/.*"IP":"\([^"]*\)".*/\1/p' | tail -n 1)
  case $seen in 127.*|10.*|172.1[6-9].*|172.2*|172.3[01].*|192.168.*) echo "INFO the gateway audited a private/Docker source for the login: the source filter allowedSourceCIDRs is proven only from a second machine (see TODO 9.browser)";; esac
  # last: throttling (five wrong attempts lock the source out for 15 minutes; the sessions already made keep working)
  codes=
  for n in 1 2 3 4 5; do c=$(login "rehearsal-wrong-password-$n" "$URL"); codes="$codes $c"; done
  c=$(login "$PW" "$URL")
  if [ "$c" != 200 ]; then ok 6.throttle "five wrong passwords (answers:$codes), then the right password is refused ($c): the source is locked out"; else bad 6.throttle "no lockout after five wrong passwords (answers:$codes, then the right one: $c)"; fi
  # no secret in the logs of either service
  { printf '%s\n' "$PW" "$S" "$TOKEN"; for n in 1 2 3 4 5; do printf '%s\n' "rehearsal-wrong-password-$n"; done; } >"$WORK/patterns"
  dc logs --no-color >"$WORK/logs.txt" 2>&1
  if grep -F -q -f "$WORK/patterns" "$WORK/logs.txt"; then bad 6.logs "a password, session or token value is in the container logs"; else ok 6.logs "the logs of the gateway and the proxy hold no password, session or token value"; fi
}

# ====================================================================================================
# 7. Backup and restore into a second project
# ====================================================================================================
snapshot() { # output file; runs in the current project context
  {
    for c in "user list" "team list" "project list" "bind list" "devices" "team access list rh-team" "member rh-p1 list" "member rh-p2 list" "kb review rh-p1"; do
      echo "## gw $c"; gw $c 2>&1
    done
    echo "## identity"
    dc exec -T minigw cat /var/lib/minigw/config/serve.json 2>/dev/null | python3 -c 'import json,sys,hashlib; c=json.load(sys.stdin); print(hashlib.sha256(json.dumps([c["key"],c["instance"]]).encode()).hexdigest())'
    for p in p1 p2; do
      echo "## admin note list $p"; cl rh-admin /home/minigw/$p note list 2>&1
      for c in structure data private; do echo "## admin search $p $c"; cl rh-admin /home/minigw/$p kb search rh-$p "$(get "SENT_${p}_$c")" 2>&1; done
    done
    echo "## reader search p1"; cl rh-reader /home/minigw/p1 kb search rh-p1 "$(get SENT_p1_structure)" 2>&1
  } >"$1"
}
stage_backup() {
  [ "$FLOW" = 1 ] || { bad 7.backup "not run: the knowledge flow did not complete"; return; }
  if ! snapshot "$WORK/snap-source.txt"; then bad 7.backup "could not read the source state"; return; fi
  BK=$WORK/backups
  bo=$( cd "$DEPLOY" && sh backup.sh --stop "$BK" 2>&1 ) || { bad 7.backup "backup.sh --stop: $(tailc "$bo")"; return; }
  bdir=$(printf '%s\n' "$bo" | sed -n 's/^Backup: //p' | tail -n 1)
  [ -n "$bdir" ] && [ -f "$bdir/config.tar" ] && [ -f "$bdir/SHA256SUMS" ] || { bad 7.backup "backup.sh printed no usable Backup: directory: $(tailc "$bo")"; return; }
  ok 7.backup "backup.sh --stop of the rehearsal project (services stopped as documented, restarted afterwards)"
  wait_healthy || { bad 7.backup "the source gateway did not come back after the backup"; return; }
  # the restore: a SECOND isolated project, made by this script and marked
  docker volume create --label "io.minigw.rehearsal.run=$RID" --label "io.minigw.rehearsal.project=$PROJ2" "${PROJ2}_rehearsal_marker" >/dev/null 2>&1 && marker_ok "$PROJ2" || { bad 7.restore "cannot create the marker volume of the restore project"; return; }
  use_project "$PROJ2" "$P2"
  ro=$( cd "$DEPLOY" && sh backup.sh --restore --into-project "$PROJ2" "$bdir" 2>&1 ) || { bad 7.restore "backup.sh --restore into $PROJ2: $(tailc "$ro")"; use_project "$PROJ" "$PORT"; return; }
  uo=$(dc up -d minigw 2>&1) || { bad 7.restore "starting the restored project: $(tailc "$uo")"; use_project "$PROJ" "$PORT"; return; }
  if ! wait_healthy; then bad 7.restore "the restored gateway is not healthy"; remove_run "$PROJ2"; use_project "$PROJ" "$PORT"; return; fi
  ok 7.restore "restored into the separate project $PROJ2 (own volumes, own port) and healthy"
  snapshot "$WORK/snap-restored.txt"
  if cmp -s "$WORK/snap-source.txt" "$WORK/snap-restored.txt"; then
    ok 7.compare "users, device bindings, roles, devices, pending review, notes, search results and the gateway identity are identical in both projects (the simulated devices connected to the restored gateway with their own keys)"
  else bad 7.compare "the restored project differs from the source: $(diff "$WORK/snap-source.txt" "$WORK/snap-restored.txt" | head -n 6 | tr '\n' '|' | cut -c1-300)"; fi
  remove_run "$PROJ2" && ok 7.remove "second project $PROJ2 removed (containers, network, its volumes)" || bad 7.remove "could not remove $PROJ2"
  use_project "$PROJ" "$PORT"
}

# ====================================================================================================
# 8. Provider, 9. operator-attended items
# ====================================================================================================
stage_provider() {
  OPDC="cd $DEPLOY && COMPOSE_PROJECT_NAME=$PROJ MINIGW_VERSION=$VERSION MINIGW_PORT=$PORT docker compose"
  if [ -z "$PROVIDER" ]; then
    todo 8.provider "no provider was added (pass --provider-url, or by hand)" <<EOF
$OPDC exec minigw minigw gw provider add LiteLLM <url of your LiteLLM or OpenAI-compatible endpoint> [--insecure for http]   # asks for the key
$OPDC exec minigw minigw gw provider models LiteLLM --enable all
# expect: "N models" after the add, then one request from a machine with Claude Code and the minigw client:
minigw -p --model <a model id from: minigw models> -- "Reply with the single word ok"
EOF
    return
  fi
  [ "$UP" = 1 ] || { bad 8.provider "not run: the gateway did not come up"; return; }
  ins=; case $PROVIDER in http://*) ins=--insecure;; esac
  if [ -t 3 ]; then
    printf 'API key of the provider (not shown): ' >&2; stty -echo <&3 2>/dev/null; read -r KEY <&3; stty echo <&3 2>/dev/null; printf '\n' >&2
  else read -r KEY <&3; fi
  [ -n "${KEY:-}" ] || { bad 8.provider "no API key given"; return; }
  po=$(printf '%s\n' "$KEY" | gw provider add rehearsal "$PROVIDER" $ins 2>&1); prc=$?
  KEY=
  [ $prc -eq 0 ] || { bad 8.provider "gw provider add: $(tailc "$po")"; return; }
  gw provider models rehearsal --enable all >/dev/null 2>&1 || { bad 8.provider "gw provider models --enable all failed"; return; }
  if gw provider list 2>&1 | grep -q '^rehearsal '; then ok 8.provider "provider added the documented way (key read without echo, not stored in the report)"; else bad 8.provider "the provider is not listed"; return; fi
  mo=$(cl rh-developer /home/minigw/p1 models 2>&1)
  if printf '%s' "$mo" | grep -qi 'rehearsal'; then ok 8.models "the minigw client of a member lists the provider's models through the gateway"; else bad 8.models "the provider's models are not listed for a member: $(tailc "$mo")"; fi
  todo 8.request "one model request needs Claude Code (the container has none)" <<EOF
# on a machine with Claude Code and the minigw client connected to this gateway:
minigw -p --model <a model id of provider rehearsal, see: minigw models> -- "Reply with the single word ok"
# expect: the model's answer; $OPDC exec minigw minigw gw usage shows the request
EOF
}
stage_operator() {
  todo 9.mac "connect one real Mac client from a second machine, device approval" <<EOF
# run the rehearsal with --bind <the server's LAN address> so that a second machine can reach port $PORT, and keep the project (answer n to the cleanup)
# on the server: the connect line
$OPDC exec minigw minigw gw token
# on the Mac (minigw client installed, docs/install.md): paste the token when asked
minigw connect <server address>:$PORT --token -
minigw status          # shows the device ID and "waiting for approval"
# on the server: compare the ID, then approve and bind
$OPDC exec minigw minigw gw devices
$OPDC exec minigw minigw gw devices allow <device id> --name rehearsal-mac
$OPDC exec minigw minigw gw bind <device id> rh-developer
# on the Mac: connect again, then
minigw connect <server address>:$PORT --token -
minigw project link rh-p1 && minigw kb search rh-p1 <a word of a note>
# expect: Status connected; the search returns what rh-developer may read
EOF
  todo 9.windows "connect one real Windows client from a second machine, device approval" <<EOF
# same as 9.mac with the Windows MSI client (docs/install.md), in PowerShell:
minigw connect <server address>:$PORT --token -
minigw status
# approve on the server as in 9.mac, binding to rh-reader this time
$OPDC exec minigw minigw gw bind <device id> rh-reader
minigw project link rh-p1
# expect: the reader sees structure and data notes of rh-p1 and nothing of rh-p2 (minigw project link rh-p2 is refused)
EOF
  todo 9.browser "second-machine browser login to the remote page" <<EOF
# needs a rehearsal run with --admin --bind <the server's LAN address> --admin-name <a name that machine resolves>, kept (not cleaned up)
# on the second machine: resolve the name to the server and trust $WORK/certs/ca/browser-ca.pem (copy only that .pem), then open
https://<admin name>:<admin port>/minigw/settings
# sign in with the rehearsal password; Team (read-only) and Review pages load
# on the server, check which source the gateway saw (a LAN address of that machine, not a Docker address):
$OPDC exec minigw tail -n 3 /var/lib/minigw/config/remote-settings.audit.jsonl
# expect: the IP field is the second machine's address; set allowedSourceCIDRs to your LAN to prove the filter
EOF
  todo 9.upstream "the upstream link from a local gateway" <<EOF
# on a local gateway (for example your Mac): copy THIS gateway's token, add it as an upstream and link a project (docs/commands.md)
minigw gw upstream add rehearsal <server address>:$PORT        # the token on stdin
minigw gw upstream link <local project> rehearsal --remote-project rh-p1 --mode submit
minigw gw upstream push <local project> --dry-run
minigw gw upstream status <local project>
# expect: the link listed and a dry run that names the notes that would be sent; a real push arrives as a pending submission in $PROJ:
$OPDC exec minigw minigw gw kb review rh-p1
EOF
}

# ====================================================================================================
# 10. Report and cleanup
# ====================================================================================================
sanitize() {
  python3 -I -c '
import re,sys
t=sys.stdin.read()
for rep,lit in sorted(zip(sys.argv[1::2],sys.argv[2::2]),key=lambda x:-len(x[1])):
    if len(lit)<3: continue
    t=t.replace(lit,rep) if lit.startswith("/") else re.sub(r"(?<![A-Za-z0-9-])"+re.escape(lit)+r"(?![A-Za-z0-9-])",rep,t)
t=re.sub(r"\b\d{1,3}(?:\.\d{1,3}){3}\b","<ip>",t)
sys.stdout.write(t)' "<work>" "$WORK" "<deploy>" "$DEPLOY" "<home>" "${HOME:-}" "<host>" "$(hostname 2>/dev/null)" "<host>" "$(hostname -s 2>/dev/null)" "<user>" "$(id -un 2>/dev/null)"
}
report() {
  REP=rehearsal-report-$RID.md
  total=$(( $(now) - START ))
  gv=$(docker version --format '{{.Server.Version}}' 2>/dev/null); cv=$(docker compose version --short 2>/dev/null)
  mv_=$(cl rh-developer /home/minigw/p1 --version 2>/dev/null | head -n 1)
  {
    printf '# Team gateway rehearsal report\n\n'
    printf -- '- Date (UTC): %s\n- Release rehearsed: v%s (built from the GitHub release; signature, signed version and checksum verified by the image build)\n' "$RID" "$VERSION"
    printf -- '- Docker server: %s; Compose: %s; platform: %s\n- Client in the simulated devices: %s\n' "${gv:-unknown}" "${cv:-unknown}" "$(uname -sm)" "${mv_:-unknown}"
    printf -- '- Project %s, ports on %s: gateway %s, remote page %s (admin profile: %s)\n- Total time: %s s\n\n' "$PROJ" "$BIND" "$PORT" "${APORT:-n/a}" "$([ "$ADMIN" = 1 ] && echo rehearsed || echo not rehearsed)" "$total"
    printf '## What is simulated\n\nThe devices are not real computers: each is a minigw client run from a second container of the same image on the Compose network, with its own home volume and device key. They prove the server side (approval, binding, roles, notes, review). Real Mac and Windows clients, a second-machine browser and the upstream link are the operator-attended items below.\n\n'
    printf '## Steps\n\n| Step | Result | Seconds | What |\n|---|---|---|---|\n'
    awk -F'\t' '{gsub(/\|/,"/",$4); printf "| %s | %s | %s | %s |\n",$2,$1,$3,$4}' "$STEPS"
    printf '\n## Access matrix\n\nlist = `note list`, search = `kb search`, read = `note read <id>`; y = the sentinel was returned.\n\n```\n'
    cat "$WORK/matrix.txt"
    printf '```\n\n## Operator-attended items\n\n'
    if [ -s "$WORK/todo.md" ]; then cat "$WORK/todo.md"; else printf 'None.\n\n'; fi
    printf '## Result\n\n%s step(s) FAILED. %s\n' "$FAILS" "$([ "$FAILS" = 0 ] && echo 'All automated steps passed; the TODO(operator) items remain before inviting the team.' || echo 'The rehearsal failed: do not invite the team before the FAIL rows are understood.')"
  } | sanitize >"$REP"
  printf 'report: %s\n' "$REP"
}

finish() {
  if [ "$PRE" != 1 ] && ! marker_ok "$PROJ"; then
    remove_work && echo "nothing was created in Docker; the working directory is removed" || FAILS=$((FAILS + 1)); return; fi
  yes=$CLEANUP
  if [ "$yes" != 1 ] && [ -t 3 ] && [ -t 1 ]; then
    printf 'Remove the rehearsal project %s, its volumes, its image tag and the working directory now? [y/N] ' "$PROJ"; read -r ans <&3
    case $ans in y|Y|yes) yes=1;; esac
  fi
  if [ "$yes" = 1 ]; then
    if do_cleanup; then echo "removed what run $RID created"; else FAILS=$((FAILS + 1)); echo "CLEANUP FAILED: see above"; fi
  else echo "kept. To remove exactly what this run created: sh $SELF --cleanup-only --project $PROJ --workdir $WORK"; fi
}

stage_pre
stage_up
stage_boot
stage_matrix
stage_flow
stage_admin
stage_backup
stage_provider
stage_operator
[ "$PRE" = 1 ] && use_project "$PROJ" "${PORT:-0}" 2>/dev/null
report
if [ "$FAILS" = 0 ]; then echo "REHEARSAL PASSED (TODO(operator) items remain)"; else echo "REHEARSAL FAILED: $FAILS step(s)"; fi
finish
[ "$FAILS" = 0 ]
