#!/bin/sh
# Run next to compose.yaml, with the same Compose environment.
set -eu
umask 077
fail() { printf 'backup: %s\n' "$*" >&2; exit 1; }
mode=backup; stop=no; offline=no; into=
usage='usage: backup.sh [--stop] [--offline] <target-dir> | --restore [--into-project <name>] <backup-dir>'
while [ "$#" -gt 0 ]; do
    case $1 in
        --restore) mode=restore; shift;;
        --stop) stop=yes; shift;;
        --offline) offline=yes; shift;;
        --into-project) [ "$#" -ge 2 ] || fail "$usage"; into=$2; shift 2;;
        -*) fail "$usage";;
        *) break;;
    esac
done
[ "$#" = 1 ] || fail "$usage"
if [ "$mode" = backup ]; then [ -z "$into" ] || fail "$usage"; else [ "$stop" = no ] && [ "$offline" = no ] || fail "$usage"; fi
command -v python3 >/dev/null || fail 'python3 is required'
config=$(docker compose config --format json) || fail 'cannot read Compose configuration'
field() { printf '%s' "$config" | python3 -c 'import json,sys; c=json.load(sys.stdin); print(c["name"] if sys.argv[1]=="project" else c["volumes"][sys.argv[1]]["name"])' "$1"; }
project=$(field project); cv=$(field config); sv=$(field scratch)
printf 'Project: %s; volumes: %s, %s\n' "$project" "$cv" "$sv"
running=
containers=$(docker compose ps -aq) || fail 'cannot inspect services'
for id in $containers; do
    state=$(docker inspect --format '{{.State.Status}}' "$id") || fail 'cannot inspect service state'
    case $state in exited|created) ;; *) running=yes;; esac
done
svcs=$(docker compose ps --status running --services) || fail 'cannot inspect services'
container=$(docker compose ps -aq minigw | head -n 1)
image=debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
if [ -n "$container" ]; then
    candidate=$(docker inspect --format '{{.Image}}' "$container")
    if docker run --rm --label "com.docker.compose.project=$project" --network none --entrypoint sh "$candidate" -c 'command -v tar && command -v sha256sum' >/dev/null 2>&1; then image=$candidate; fi
fi
helper() { docker run --rm --label "com.docker.compose.project=$project" --network none --user 0:0 --entrypoint sh "$@"; }
# A failing inspect is not "no such volume": restore must never read an I/O error as an absent target.
exists() { out=$(docker volume inspect "$1" 2>&1) && return 0; case $out in *[Nn]o\ such\ volume*) return 1;; esac; fail "cannot inspect volume: $1"; }
# Any container that mounts either volume, whatever its project, must be stopped (after --stop stopped this project's services).
writers() {
    for v in "$cv" "$sv"; do
        list=$(docker ps -a --filter "volume=$v" --format '{{.Names}} {{.State}}') || fail "cannot list containers of volume: $v"
        while read -r n st; do
            case $st in ''|exited|created) ;; *) fail "container $n is $st and mounts volume $v; stop it first";; esac
        done <<EOF
$list
EOF
    done
}
restart=no
finish() {
    rc=$?
    trap - EXIT
    if [ "$restart" = yes ]; then
        # only what ran before (a service stopped on purpose stays stopped)
        # shellcheck disable=SC2086
        docker compose start $svcs >/dev/null 2>&1 || { printf 'backup: could not restart services\n' >&2; rc=1; }
    fi
    exit "$rc"
}
trap finish EXIT
trap 'exit 1' HUP INT QUIT TERM
if [ "$mode" = backup ]; then
    for v in "$cv" "$sv"; do exists "$v" || fail "volume does not exist: $v"; done
    # A wrong project has volumes of another deployment or none, and never containers of this one; a deployment
    # taken down with `docker compose down` has no containers either: that is what --offline is for.
    [ -n "$containers" ] || [ "$offline" = yes ] || fail "project $project has no containers: run backup.sh with the same COMPOSE_PROJECT_NAME and COMPOSE_FILE (or from the same folder and .env) as the deployment; use --offline only if it was taken down on purpose"
    if [ "$stop" = yes ]; then
        [ -z "$svcs" ] || restart=yes
        docker compose stop >/dev/null 2>&1 || fail 'could not stop services' # also when nothing seemed to run: a restart policy may bring a container back
    else
        [ -z "$running" ] || fail 'service is running; use --stop'
    fi
    writers
    mkdir -p "$1"
    target=$(cd "$1" && pwd)/minigw-backup-$(date -u +%Y%m%dT%H%M%SZ)
    case $target in *','*|*'"'*) fail 'backup path cannot contain commas or quotes';; esac
    mkdir -m 700 "$target" || fail 'backup directory already exists'
    for kind in config scratch; do
        v=$cv; [ "$kind" = config ] || v=$sv
        helper --mount "type=volume,src=$v,dst=/v,readonly" "$image" -c 'tar -C /v -cf - .' > "$target/$kind.tar" || fail 'archive failed (incomplete backup)'
    done
    helper --mount "type=bind,src=$target,dst=/b,readonly" "$image" -c 'tar -tf /b/config.tar | grep -qx "./serve.json" && tar -tf /b/config.tar | grep -qx "./core/core.db"' >/dev/null 2>&1 || fail 'config archive lacks serve.json or core database (empty source)'
    helper --mount "type=bind,src=$target,dst=/b,readonly" "$image" -c 'cd /b; sha256sum config.tar scratch.tar' > "$target/SHA256SUMS" || fail 'checksum failed'
    version=$(printf '%s' "$config" | python3 -c 'import json,sys; print(json.load(sys.stdin)["services"]["minigw"]["image"])')
    if [ -n "$container" ] && [ "$image" = "${candidate:-}" ]; then
        version=$(docker run --rm --label "com.docker.compose.project=$project" --network none --entrypoint minigw "$image" --version | head -n 1)
    fi
    printf 'date=%s\nproject=%s\nconfig=%s\nscratch=%s\nMINIGW_VERSION=%s\n' "$(date -u +%FT%TZ)" "$project" "$cv" "$sv" "$version" > "$target/manifest.txt"
    chmod 600 "$target/"*
    printf 'Backup: %s\n' "$target"
else
    source=$(cd "$1" && pwd) || fail 'backup directory not found'
    case $source in *','*|*'"'*) fail 'backup path cannot contain commas or quotes';; esac
    [ -r "$source/manifest.txt" ] || fail 'manifest.txt not found in the backup directory'
    mproject=$(sed -n 's/^project=//p' "$source/manifest.txt" | head -n 1)
    [ -n "$mproject" ] || fail 'manifest.txt has no project name'
    [ -z "$into" ] || [ "$into" = "$project" ] || fail "--into-project must name the current project ($project)"
    [ "$mproject" = "$project" ] || [ "$into" = "$project" ] || fail "backup was made in project $mproject, the current project is $project: run with COMPOSE_PROJECT_NAME=$mproject, or give --into-project $project to restore it into $project on purpose"
    [ -z "$running" ] || fail 'service is running; stop it before restore'
    helper --mount "type=bind,src=$source,dst=/b,readonly" "$image" -c 'cd /b; test "$(wc -l < SHA256SUMS)" -eq 2 && grep -Eq "^[0-9a-f]{64}  config[.]tar$" SHA256SUMS && grep -Eq "^[0-9a-f]{64}  scratch[.]tar$" SHA256SUMS && sha256sum -c SHA256SUMS >/dev/null' >/dev/null 2>&1 || fail 'checksum verification failed'
    # What a real deployment writes: the gateway's own files and, in scratch, copies of work trees and agent homes that can
    # hold symbolic links (client/serve_scratchmove.go copies them, client/serve_agents.go makes auth.json links). So symlinks
    # are allowed when the target stays inside the volume (relative without escaping, or under /var/lib/minigw/); hard links,
    # devices, FIFOs, absolute names and any .. component are refused. A backup directory is as trusted as the volume itself:
    # this is a guard against a damaged or mixed-up archive, not a signature.
    rc=0
    helper -i --mount "type=bind,src=$source,dst=/b,readonly" "$image" -s >/dev/null 2>&1 <<'EOF' || rc=$?
cd /b || exit 14
for f in config.tar scratch.tar; do
    tar -tf "$f" > /tmp/n || exit 14
    tar -tvf "$f" > /tmp/v || exit 14
    r=0; grep -Eq '^/|(^|/)[.][.](/|$)' /tmp/n || r=$?; [ "$r" = 1 ] || exit 11
    r=0; grep -Eq '^[^-dl]' /tmp/v || r=$?; [ "$r" = 1 ] || exit 12
    awk '
/^l/ {
  i = index($0, " -> ")
  if (i == 0) { bad = 1; next }
  pre = substr($0, 1, i - 1); t = substr($0, i + 4)
  sub(/^[^ ]+ +[^ ]+ +[^ ]+ +[^ ]+ +[^ ]+ /, "", pre)
  if (t ~ /^\//) { if (t !~ /^\/var\/lib\/minigw\// || t ~ /(^|\/)\.\.(\/|$)/) bad = 1; next }
  n = split(pre, a, "/"); d = 0
  for (k = 1; k < n; k++) if (a[k] != "" && a[k] != ".") s[++d] = a[k]
  m = split(t, b, "/")
  for (k = 1; k <= m; k++) {
    if (b[k] == "" || b[k] == ".") continue
    if (b[k] == "..") { if (d == 0) bad = 1; else d-- } else s[++d] = b[k]
  }
}
END { exit bad }' /tmp/v || exit 13
done
EOF
    case $rc in
        0) ;;
        11) fail 'archive refused: absolute path or .. component';;
        12) fail 'archive refused: hard link, device, FIFO or other special entry';;
        13) fail 'archive refused: symbolic link that leaves the volume';;
        *) fail 'archive refused: cannot list the archives';;
    esac
    writers
    for v in "$cv" "$sv"; do
        if exists "$v"; then
            listing=$(helper --mount "type=volume,src=$v,dst=/v,readonly" "$image" -c 'find /v -mindepth 1 -maxdepth 1 -print -quit') || fail "cannot inspect volume: $v"
            [ -z "$listing" ] || fail "target volume is not empty: $v"
        fi
    done
    for kind in config scratch; do
        v=$cv; [ "$kind" = config ] || v=$sv
        exists "$v" || docker volume create --label "com.docker.compose.project=$project" --label "com.docker.compose.volume=$kind" "$v" >/dev/null
        helper --mount "type=volume,src=$v,dst=/v" --mount "type=bind,src=$source,dst=/b,readonly" "$image" -c "tar -C /v --no-same-owner -xf /b/$kind.tar && chown -R 10001:10001 /v" >/dev/null 2>&1 || fail 'restore failed; target is now non-empty'
    done
    printf 'Restored. Next: docker compose up -d\n'
fi
