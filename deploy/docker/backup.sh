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
if [ -n "$container" ]; then
    # the gateway container's real mounts must be the volumes resolved above (catches a missing -f override that renames them)
    mounts=$(docker inspect --format '{{range .Mounts}}{{.Type}} {{.Destination}} {{.Name}}{{"\n"}}{{end}}' "$container") || fail 'cannot inspect the gateway container'
    lcv=$(printf '%s\n' "$mounts" | sed -n 's|^volume /var/lib/minigw/config \(.*\)$|\1|p'); lsv=$(printf '%s\n' "$mounts" | sed -n 's|^volume /var/lib/minigw/scratch \(.*\)$|\1|p')
    [ "$lcv" = "$cv" ] && [ "$lsv" = "$sv" ] || fail "the gateway container uses volumes ${lcv:-none}, ${lsv:-none} but Compose resolves $cv, $sv: run with the same COMPOSE_FILE and COMPOSE_PROJECT_NAME as the deployment"
fi
image=debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
if [ -n "$container" ]; then
    candidate=$(docker inspect --format '{{.Image}}' "$container")
    if docker run --rm --label "com.docker.compose.project=$project" --network none --entrypoint sh "$candidate" -c 'command -v tar && command -v sha256sum' >/dev/null 2>&1; then image=$candidate; fi
fi
helper() { docker run --rm --label "com.docker.compose.project=$project" --network none --user 0:0 --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER --security-opt no-new-privileges --entrypoint sh "$@"; }
# A helper that cannot start (Docker exit 125/126/127; Docker's own message is on stderr) is not a verdict on the data.
hfail() { case $1 in 125|126|127) fail "cannot run the helper container (exit $1; Docker's message is above): image $image";; esac; fail "$2"; }
probe() { perr=$(helper "$image" -c true 2>&1) || fail "cannot run the helper container: $perr (image $image: the gateway's own image or debian:trixie-slim; it must be present or pullable)"; }
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
restart=no; target=; complete=no
finish() {
    rc=$?
    trap - EXIT
    if [ -n "$target" ] && [ "$complete" = no ]; then
        # this run's own fresh directory: remove what it wrote, before services start again on a possibly full disk
        rm -f "$target/config.tar" "$target/scratch.tar" "$target/SHA256SUMS" "$target/manifest.txt"
        rmdir "$target" 2>/dev/null && printf 'backup: incomplete backup removed: %s\n' "$target" >&2
    fi
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
    probe
    mkdir -p "$1"
    target=$(cd "$1" && pwd)/minigw-backup-$(date -u +%Y%m%dT%H%M%SZ)
    mkdir -m 700 "$target" || fail 'backup directory already exists'
    for kind in config scratch; do
        v=$cv; [ "$kind" = config ] || v=$sv
        helper --mount "type=volume,src=$v,dst=/v,readonly" "$image" -c 'tar -C /v --exclude=./core/core.restored -cf - .' > "$target/$kind.tar" || hfail $? 'archive failed (incomplete backup)'
    done
    # the archives are streamed to the helper: no host folder is bind-mounted
    names=$(helper -i "$image" -c 'tar -tf -' < "$target/config.tar") || hfail $? 'config archive cannot be listed'
    printf '%s\n' "$names" | grep -qx './serve.json' && printf '%s\n' "$names" | grep -qx './core/core.db' || fail 'config archive lacks serve.json or core database (empty source)'
    : > "$target/SHA256SUMS"
    for kind in config scratch; do
        h=$(helper -i "$image" -c 'sha256sum' < "$target/$kind.tar") || hfail $? 'checksum failed'
        printf '%s  %s.tar\n' "${h%% *}" "$kind" >> "$target/SHA256SUMS"
    done
    version=$(printf '%s' "$config" | python3 -c 'import json,sys; print(json.load(sys.stdin)["services"]["minigw"]["image"])')
    if [ -n "$container" ] && [ "$image" = "${candidate:-}" ]; then
        version=$(docker run --rm --label "com.docker.compose.project=$project" --network none --entrypoint minigw "$image" --version | head -n 1)
    fi
    printf 'date=%s\nproject=%s\nconfig=%s\nscratch=%s\nMINIGW_VERSION=%s\n' "$(date -u +%FT%TZ)" "$project" "$cv" "$sv" "$version" > "$target/manifest.txt"
    chmod 600 "$target/"*
    complete=yes
    printf 'Backup: %s\n' "$target"
else
    source=$(cd "$1" && pwd) || fail 'backup directory not found'
    [ -r "$source/manifest.txt" ] || fail 'manifest.txt not found in the backup directory'
    mproject=$(sed -n 's/^project=//p' "$source/manifest.txt" | head -n 1)
    [ -n "$mproject" ] || fail 'manifest.txt has no project name'
    mcv=$(sed -n 's/^config=//p' "$source/manifest.txt" | head -n 1); msv=$(sed -n 's/^scratch=//p' "$source/manifest.txt" | head -n 1)
    printf 'Restoring backup of %s (%s, %s) into %s (%s, %s)\n' "$mproject" "$mcv" "$msv" "$project" "$cv" "$sv"
    [ -z "$into" ] || [ "$into" = "$project" ] || fail "--into-project must name the current project ($project)"
    [ "$mproject" = "$project" ] && [ "$mcv" = "$cv" ] && [ "$msv" = "$sv" ] || [ "$into" = "$project" ] || fail "backup was made in project $mproject (volumes $mcv, $msv), the current project is $project (volumes $cv, $sv): run with the environment of the original, or give --into-project $project to restore into $project on purpose"
    [ -z "$running" ] || fail 'service is running; stop it before restore'
    probe
    [ -r "$source/SHA256SUMS" ] && [ "$(wc -l < "$source/SHA256SUMS")" -eq 2 ] || fail 'checksum verification failed'
    for kind in config scratch; do
        want=$(grep -E "^[0-9a-f]{64}  $kind[.]tar\$" "$source/SHA256SUMS") || fail 'checksum verification failed'
        have=$(helper -i "$image" -c 'sha256sum' < "$source/$kind.tar") || hfail $? 'checksum verification failed'
        [ "${have%% *}" = "${want%% *}" ] || fail 'checksum verification failed'
    done
    # The checksums guard against a damaged or mixed-up archive. A backup directory must be protected like the volume itself
    # (it holds the keys); restore does not inspect archive content.
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
        helper -i --mount "type=volume,src=$v,dst=/v" "$image" -c 'tar -C /v --no-same-owner --exclude=./core/core.restored -xf - && chown -R 10001:10001 /v' < "$source/$kind.tar" || hfail $? 'restore failed; target is now non-empty'
    done
    # The restored database no longer knows what it pushed upstream: the gateway marks its upstream links at the next start (it consumes this file).
    # Only a gateway 0.9.2 or newer consumes it; on an older image it would stay and travel in every later backup. A tag that is not a
    # version (local) is a build of this tree: write it.
    tag=$(printf '%s' "$config" | python3 -c 'import json,sys; print(json.load(sys.stdin)["services"]["minigw"]["image"].rpartition(":")[2])')
    if printf '%s' "$tag" | python3 -c 'import re,sys; t=sys.stdin.read(); m=re.match(r"(\d+)\.(\d+)\.(\d+)",t); sys.exit(1 if m and tuple(map(int,m.groups()))<(0,9,2) else 0)'; then
        helper --mount "type=volume,src=$cv,dst=/v" "$image" -c 'mkdir -p /v/core && : > /v/core/core.restored && chown 10001:10001 /v/core/core.restored && chmod 600 /v/core/core.restored' || hfail $? 'restore pending marker could not be written; target is now non-empty'
    else
        printf 'restore pending marker not written: the image to run is %s, older than 0.9.2, which would not consume it\n' "$tag"
    fi
    printf 'Restored. Next: docker compose up -d\n'
fi
