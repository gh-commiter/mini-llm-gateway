#!/bin/sh
# Run next to compose.yaml, with the same Compose environment.
set -eu
umask 077
fail() { printf 'backup: %s\n' "$*" >&2; exit 1; }
mode=backup; stop=no
case ${1:-} in --restore) mode=restore; shift;; --stop) stop=yes; shift;; esac
[ "$#" = 1 ] || fail 'usage: backup.sh [--stop] <target-dir> | --restore <backup-dir>'
command -v python3 >/dev/null || fail 'python3 is required'
config=$(docker compose config --format json) || fail 'cannot read Compose configuration'
field() { printf '%s' "$config" | python3 -c 'import json,sys; c=json.load(sys.stdin); print(c["name"] if sys.argv[1]=="project" else c["volumes"][sys.argv[1]]["name"])' "$1"; }
project=$(field project); cv=$(field config); sv=$(field scratch)
running=
containers=$(docker compose ps -aq) || fail 'cannot inspect services'
for id in $containers; do
    state=$(docker inspect --format '{{.State.Status}}' "$id") || fail 'cannot inspect service state'
    case $state in exited|created) ;; *) running=yes;; esac
done
container=$(docker compose ps -aq minigw | head -n 1)
image=debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
if [ -n "$container" ]; then
    candidate=$(docker inspect --format '{{.Image}}' "$container")
    if docker run --rm --label "com.docker.compose.project=$project" --network none --entrypoint sh "$candidate" -c 'command -v tar && command -v sha256sum' >/dev/null 2>&1; then image=$candidate; fi
fi
helper() { docker run --rm --label "com.docker.compose.project=$project" --network none --user 0:0 --entrypoint sh "$@"; }
exists() { docker volume inspect "$1" >/dev/null 2>&1; }
restart=no
finish() {
    rc=$?
    trap - EXIT
    if [ "$restart" = yes ]; then docker compose start >/dev/null 2>&1 || { printf 'backup: could not restart services\n' >&2; rc=1; }; fi
    exit "$rc"
}
trap finish EXIT
trap 'exit 1' HUP INT QUIT TERM
if [ "$mode" = backup ]; then
    for v in "$cv" "$sv"; do exists "$v" || fail "volume does not exist: $v"; done
    if [ -n "$running" ]; then
        [ "$stop" = yes ] || fail 'service is running; use --stop'
        restart=yes
        docker compose stop >/dev/null 2>&1 || fail 'could not stop services'
    fi
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
    [ -z "$running" ] || fail 'service is running; stop it before restore'
    source=$(cd "$1" && pwd) || fail 'backup directory not found'
    case $source in *','*|*'"'*) fail 'backup path cannot contain commas or quotes';; esac
    helper --mount "type=bind,src=$source,dst=/b,readonly" "$image" -c 'cd /b; test "$(wc -l < SHA256SUMS)" -eq 2 && grep -Eq "^[0-9a-f]{64}  config.tar$" SHA256SUMS && grep -Eq "^[0-9a-f]{64}  scratch.tar$" SHA256SUMS && sha256sum -c SHA256SUMS >/dev/null' >/dev/null 2>&1 || fail 'checksum verification failed'
    for v in "$cv" "$sv"; do
        if exists "$v"; then
            helper --mount "type=volume,src=$v,dst=/v,readonly" "$image" -c 'test -z "$(find /v -mindepth 1 -maxdepth 1 -print -quit)"' || fail "target volume is not empty: $v"
        fi
    done
    for kind in config scratch; do
        v=$cv; [ "$kind" = config ] || v=$sv
        exists "$v" || docker volume create --label "com.docker.compose.project=$project" --label "com.docker.compose.volume=$kind" "$v" >/dev/null
        helper --mount "type=volume,src=$v,dst=/v" --mount "type=bind,src=$source,dst=/b,readonly" "$image" -c "tar -C /v --no-same-owner -xf /b/$kind.tar && chown -R 10001:10001 /v" >/dev/null 2>&1 || fail 'restore failed; target is now non-empty'
    done
    printf 'Restored. Next: docker compose up -d\n'
fi
