#!/bin/zsh
# Proof in containers: restore -> marked upstream link -> reconcile, with a REAL upstream gateway (docs/usage.md, upstream chapter;
# deploy/docker/backup.sh; client/core/upstream_restore.go, upstream_reconcile.go). scripts/compose-e2e.sh proves only that core.restored is
# consumed (its deployment has no upstream); client/upstream_reconcile_e2e_test.go runs both gateways as processes, not in containers.
#   hub  = the upstream gateway, edge = the local gateway; two Compose projects of their own ($P-hub, $P-edge, same compose.yaml of
#   deploy/docker, the image minigw:local with the binary of this tree put on top (no apt, no pull, no download)), one internal Docker network ($P-net), NOTHING published.
#   Commands go through `docker compose exec`; the clients that make and read notes are containers of the same image on that network.
# Story:  A pushed and accepted -> notes B, C, D, E made but not pushed -> BACKUP of the edge -> B, C, D, E pushed (the backup does not know
#         the pushes); B and C accepted on the hub, D and E rejected, C then edited ON THE HUB by another user; Z made, pushed, accepted
#         (gone locally after the restore); A edited (also lost by the restore) -> stop, restore, start.
# (The D steps found a defect in the first version of this script: a replayed rejected submission was reported as "create"; fixed in the pusher.)
# Expectation (worked out from the code and the docs BEFORE the first run):
#   - core.restored exists after backup.sh --restore and is gone after the start; the log says "restore pending file found: 1 upstream
#     links marked restored" and "... automatic pushes to them are paused"; `upstream status` says "restored: reconcile before pushing".
#   - a manual push sends and creates nothing ("created 0, replaced 0"); the hub's notes and submissions are the same before and after.
#   - reconcile --dry-run (docs/usage.md classes): A known (its receipt is in the backup); B candidate (no receipt, exactly one hub note ends
#     with its trailer; docs: an accepted submission is a note WITHOUT proof; content equal); C candidate, no proof (last written by another
#     user), content differs; D and E unknown (rejected on the hub: no hub note, no receipt); Z "orphan remote <id>" (nothing is imported or
#     deleted); "restored mark cleared: false": a dry run changes nothing.
#   - decisions: B and C `--note <id> --confirm <remote id>` (without proof a conflict with an unknown base is recorded, on both link modes),
#     D `--note <id> --new` (same text: the hub replays its earlier rejected submission, so the push reports D as skipped, rejected with the reason), E `--note <id> --skip`; the mark is cleared by the last decision ("restored mark cleared: true").
#   - then a push: A and B replaced, C a conflict (the line says "conflict <id>"; the docs' `CONFLICT: note N (changed there)` is the publish-mode wording), D skipped (rejected on the hub), E skipped; `resolve --ours` for C sends it as a replacing submission.
#   - the hub ends with ONE note each for A, B, C, Z and none for D and E.
# Rules: nothing is published on the host; no --privileged, no chown/chmod -R, no prune; it touches only its own projects, network and
# volumes (by name and label); no rm (work files stay in $D). Never prints a token: tokens travel through pipes only.
emulate -L zsh; set -u; setopt pipefail
ROOT=${0:A:h:h}
source "$ROOT/scripts/compose-e2e-common.zsh"
D=${SCRATCH_DIR:-"/Volumes/SAMSUNG990PRO/Mini Gateway Scratch/dev-builds/composeup-095"}
P=${UP_PREFIX:-mgup095}
BASE=${UP_BASE:-minigw:local}  # an image already present: only its binary is replaced below (no apt, no pull, no download)
TAG=${UP_TAG:-mgup095}; IMG=minigw:$TAG
RID=$P-$$-$RANDOM
T=$(date +%s)
NET=$P-net
CREATED=()
fail() { print -u2 "FAIL $*"; exit 1 }
ok() { print "ok   $*" }
evidence() { print -u2 -r -- "--- $1"; print -u2 -r -- "$2"; fail "$3" } # command, output, what was expected
cleanup() {
  local p v
  for p in hub edge; do
    (( ${CREATED[(Ie)$p]} )) && dc $p down -v --remove-orphans >/dev/null 2>&1
  done
  for v in ${(f)"$(docker volume ls -q --filter label=io.minigw.upe=$RID 2>/dev/null)"}; do docker volume rm $v >/dev/null 2>&1; done
  (( ${CREATED[(Ie)net]} )) && docker network rm $NET >/dev/null 2>&1
  docker image rm $IMG >/dev/null 2>&1 # the tag this run built (the base image stays)
}
trap cleanup EXIT
mkdir -p $D || fail "work folder $D"
umask 077
unset COMPOSE_FILE COMPOSE_PROFILES COMPOSE_ENV_FILES COMPOSE_PROJECT_NAME
docker image inspect $BASE >/dev/null 2>&1 || fail "image $BASE is missing: build it the way scripts/compose-e2e.sh does (no pull here)"
# this tree's binary for the architecture of the base image, on top of the base image (the same user, folders and entrypoint)
arch=$(docker image inspect --format '{{.Architecture}}' $BASE)
mkdir -p $D/img && (cd $ROOT/client && GOCACHE=${GOCACHE:-$D/go-cache} GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -o $D/img/minigw .) || fail "go build"
print -rl -- "FROM $BASE" 'COPY --chmod=0755 minigw /usr/local/bin/minigw' > $D/img/Dockerfile
docker build -q --pull=false -t $IMG $D/img >/dev/null || fail "docker build of $IMG"

print -rl -- 'services:' '  minigw:' '    ports: !reset []' '    networks:' '      up:' '        aliases: ["${UP_ALIAS}"]' \
  'networks:' '  up:' '    external: true' "    name: $NET" > $D/compose.up.yaml
# one Compose project per gateway, run from $D with the environment of that project (backup.sh reads the same environment)
penv() { local n=$1; shift; ( cd $D && COMPOSE_PROJECT_NAME=$P-$n MINIGW_VERSION=$TAG UP_ALIAS=$n COMPOSE_FILE=$ROOT/deploy/docker/compose.yaml:$D/compose.up.yaml "$@" ) }
dc() { local n=$1; shift; penv $n docker compose "$@" }
gw() { local n=$1; shift; dc $n exec -T minigw minigw gw "$@" }
token() { gw $1 token | grep -o 'mgw-[A-Za-z0-9_-]*#[A-Za-z0-9_-]*' } # to a pipe, never to a variable or the terminal
# a client container of the same image on the internal network (a simulated device with its own home volume)
cl() { local n=$1 u=$2 d=$3; shift 3
  docker run --rm -i --label com.docker.compose.project= --label io.minigw.upe=$RID --network $NET --user 10001:10001 --cap-drop ALL --security-opt no-new-privileges \
    --read-only --tmpfs /tmp:size=16m,mode=1777 -e HOME=/home/minigw -e MINIGW_SECRETS=file -v $P-home-$n-$u:/home/minigw -w $d --entrypoint minigw $IMG "$@" }
volfile() { # edge|hub, test expression on /v/core/core.restored: exit status of `test`
  docker run --rm --network none --label com.docker.compose.project= --mount type=volume,src=$P-$1_config,dst=/v,readonly --entrypoint sh $IMG -c "test $2 /v/core/core.restored" }
run() { # what, command...: stdout and stderr in $out, must succeed
  local what=$1; shift
  out=$("$@" 2>&1) || evidence "$what" "$out" "command failed"
}
has() { [[ $out == *$1* ]] || evidence "$2" "$out" "expected: $1" } # needle, what
hasnt() { [[ $out != *$1* ]] || evidence "$2" "$out" "not expected: $1" }
hubnotes() { cl hub hub-viewer /home/minigw/p note list 2>&1 } # one line per readable note on the hub
nnotes() { hubnotes | grep -c -- "$1" }                          # notes of the hub whose line holds this title
accept_pending() { # accept every pending submission of the hub (owner review, the version line it printed)
  local ro id h
  ro=$(gw hub kb review shop 2>&1) || evidence "gw kb review" "$ro" "review failed"
  for id in ${(f)"$(print -r -- "$ro" | sed -n 's/^submission \([0-9][0-9]*\) .*pending.*/\1/p')"}; do
    h=$(print -r -- "$ro" | awk -v n=$id '$1=="submission" && $2==n {f=1; next} f && $1=="version" {print $2; exit} $1=="submission" {f=0}')
    out=$(gw hub kb accept $id --version $h 2>&1) || evidence "gw kb accept $id" "$out" "accept failed"
  done
}
noteid() { print -r -- "$out" | grep -o '[0-9][0-9]*' | head -n 1 }
device_of() { cl $1 $2 /home/minigw status 2>/dev/null | sed -n 's/^Device: *\([^ ]*\).*/\1/p' }
onboard() { # node user role: a client device, allowed and bound by the owner of that gateway, project folder p linked
  local n=$1 u=$2 role=$3 d
  docker volume create --label io.minigw.upe=$RID $P-home-$n-$u >/dev/null || fail "home volume $n/$u"
  token $n | cl $n $u /home/minigw connect $n:8765 --token - >/dev/null 2>&1
  d=$(device_of $n $u); [[ -n $d ]] || fail "$n/$u: no device id"
  gw $n devices allow $d --name $u >/dev/null && gw $n bind $d $u >/dev/null || fail "$n/$u: allow and bind"
  gw $n member shop set $u $role >/dev/null || fail "$n/$u: role"
  out=$(token $n | cl $n $u /home/minigw connect $n:8765 --token - 2>&1) || evidence "connect $n/$u" "$out" "client did not connect"
  docker run --rm --label com.docker.compose.project= --label io.minigw.upe=$RID --network none --user 10001:10001 --cap-drop ALL -v $P-home-$n-$u:/home/minigw --entrypoint mkdir $IMG -p /home/minigw/p || fail "$n/$u: folder"
  run "project link $n/$u" cl $n $u /home/minigw/p project link shop
}
newnote() { # title body: from the edge author
  out=$(print -r -- "$2" | cl edge edge-author /home/minigw/p note push --class structure --title "$1" - 2>&1) || evidence "note push $1" "$out" "note push failed"
}

# ---------- 1. two gateways, an isolated Compose project each, an internal network, nothing published ----------
docker network inspect $NET >/dev/null 2>&1 && fail "network $NET exists already (a leftover or another run): refusing to touch it"
for n in hub edge; do [[ -z $(docker compose -p $P-$n ps -aq 2>/dev/null) ]] || fail "Compose project $P-$n has containers already: refusing to touch it"; done
docker network create --internal --label io.minigw.upe=$RID $NET >/dev/null || fail "network create"; CREATED+=(net)
for n in hub edge; do CREATED+=($n); run "up $n" dc $n up -d --no-build; healthy $P-$n || fail "$n not healthy"; done
for n in hub edge; do
  c=$(dc $n ps -aq minigw)
  out=$(docker inspect --format '{{range $p,$b := .NetworkSettings.Ports}}{{if $b}}published:{{$p}} {{end}}{{end}}{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' $c)
  [[ $out == $NET' ' ]] || evidence "docker inspect $n" "$out" "nothing published, only on $NET"
done
[[ $(docker network inspect --format '{{.Internal}}' $NET) == true ]] || fail "network is not internal"
ok "hub and edge up and healthy in own Compose projects ($P-hub, $P-edge) on the internal network $NET, no port published"

# ---------- 2. setup by gw commands (docs/usage.md, upstream chapter steps 1-3) ----------
for n in hub edge; do run "project $n" gw $n project add shop "Shop"; done
run "user gw-mini" gw hub user add gw-mini
run "user hub-viewer" gw hub user add hub-viewer
run "user hub-admin" gw hub user add hub-admin
run "user edge-author" gw edge user add edge-author
onboard hub hub-viewer reader
onboard hub hub-admin admin
onboard edge edge-author developer
out=$(token hub | gw edge upstream add hub hub:8765 --device-name edge-gateway 2>&1); rc=$?
[[ $out != *mgw-* ]] || fail "upstream add printed a token"
dev=$(print -r -- "$out" | grep -oE '[A-Za-z0-9_-]{43}' | head -n 1)
[[ -n $dev ]] || evidence "gw upstream add (exit $rc)" "$out" "no device id printed"
run "devices allow" gw hub devices allow $dev --name edge-gateway
run "bind" gw hub bind $dev gw-mini
run "role" gw hub member shop set gw-mini developer
run "link" gw edge upstream link shop hub --class structure
ok "upstream hub added on the edge (token on stdin only), device allowed + bound + developer role on the hub, submit link made"

# ---------- 3. A pushed and accepted; B, C, D, E made; backup of the edge; then what the backup does not know ----------
newnote "A-$T" "body of A $T"; nA=$(noteid)
run "push A" gw edge upstream push shop; has "created 1" "first push"
accept_pending
[[ $(nnotes "A-$T") == 1 ]] || evidence "hub note list" "$(hubnotes)" "A on the hub once"
for t in B C D E; do newnote "$t-$T" "body of $t $T"; eval "n$t=$(noteid)"; done
out=$(penv edge sh $ROOT/deploy/docker/backup.sh --stop $D/backups-edge-$T 2>&1) || evidence "backup.sh --stop" "$out" "backup failed"
BK=${out##*Backup: }; BK=${BK%%$'\n'*}; [[ -r $BK/manifest.txt ]] || evidence "backup.sh" "$out" "no backup directory printed"
healthy $P-edge || fail "edge not healthy after the backup"
ok "A (note $nA) pushed and accepted; B $nB, C $nC, D $nD, E $nE made, not pushed; backup of the edge taken with backup.sh --stop"

run "push B C D E" gw edge upstream push shop; has "created 4" "push after the backup"
ro=$(gw hub kb review shop 2>&1)
for t in D E; do # reject D and E on the hub: no hub note, and the backup has no receipt of them
  id=$(print -r -- "$ro" | sed -n "s/^submission \([0-9][0-9]*\) .*pending.* $t-$T\$/\1/p")
  [[ -n $id ]] || evidence "gw kb review" "$ro" "submission of $t"
  run "reject $t" gw hub kb reject $id --reason "rehearsal"
done
accept_pending
RC=$(hubnotes | grep -- "C-$T" | grep -o '[0-9][0-9]*' | head -n 1); [[ -n $RC ]] || evidence "hub note list" "$(hubnotes)" "C on the hub"
# another user edits C on the hub: the text changes, the trailer of this gateway stays (the edit is not by the gateway's device)
out=$(cl hub hub-admin /home/minigw/p note read $RC 2>&1) || evidence "note read C on the hub" "$out" "hub read failed"
body=${out#*$'\n\n'}; body=${body/body of C $T/body of C changed on the hub $T}
out=$(print -r -- "$body" | cl hub hub-admin /home/minigw/p note edit $RC --revision 1 - 2>&1) || evidence "note edit C on the hub" "$out" "hub edit failed"
newnote "Z-$T" "body of Z $T"
run "push Z" gw edge upstream push shop; has "created 1" "push Z"; accept_pending
out=$(print -r -- "A edited after the backup $T" | cl edge edge-author /home/minigw/p note edit $nA --revision 1 - 2>&1) || evidence "note edit A" "$out" "edit failed"
for t in A B C Z; do [[ $(nnotes "$t-$T") == 1 ]] || evidence "hub note list" "$(hubnotes)" "$t on the hub once"; done
for t in D E; do [[ $(nnotes "$t-$T") == 0 ]] || evidence "hub note list" "$(hubnotes)" "$t rejected, not on the hub"; done
HUBBEFORE=$(hubnotes | wc -l | tr -d " ")
ok "hub holds A, B, C, Z once each (D and E rejected, C edited on the hub): $HUBBEFORE notes; the backup knows none of that"

# ---------- 4. stop the edge, restore it with backup.sh --restore, start it ----------
# the target volumes must be empty: the edge's own project is taken down with its volumes first (down -v of THIS project only)
run "down edge" dc edge down -v
out=$(penv edge sh $ROOT/deploy/docker/backup.sh --restore $BK 2>&1) || evidence "backup.sh --restore" "$out" "restore failed"
volfile edge -e || evidence "backup.sh --restore" "$out" "core/core.restored was not written"
ok "backup.sh --restore wrote core/core.restored"
run "up edge" dc edge up -d --no-build; healthy $P-edge || fail "restored edge not healthy"
volfile edge -e && fail "core/core.restored is still there after the start"
ok "core/core.restored consumed by the start"
logs=$(dc edge logs minigw 2>&1)
[[ $logs == *"restore pending file found: 1 upstream links marked restored"* ]] || evidence "docker compose logs" "$logs" "restore pending line"
[[ $logs == *"1 upstream links are marked restored; automatic pushes to them are paused"* ]] || evidence "docker compose logs" "$logs" "paused-links line"
ok "start-up log: restore pending file found, automatic pushes paused"
run "status" gw edge upstream status shop; has "restored: reconcile before pushing" "status after restore"
ok "gw upstream status shows the restored mark"
run "push (manual)" gw edge upstream push shop; has "created 0, replaced 0" "blocked push"
[[ $(hubnotes | wc -l | tr -d " ") == $HUBBEFORE ]] || evidence "hub note list" "$(hubnotes)" "the hub's notes changed by a blocked push"
out=$(gw hub kb review shop 2>&1); [[ $out == "no submissions"* ]] || evidence "gw kb review" "$out" "a blocked push left a submission on the hub"
ok "manual push of a marked link created 0, replaced 0; the hub's notes and submissions are unchanged"

# ---------- 5. reconcile ----------
run "reconcile --dry-run" gw edge upstream reconcile shop --upstream hub --dry-run
print -r -- "$out" | sed 's/^/     /'
has "known note $nA " "dry run: A"; has "candidate note $nB " "dry run: B"; has "candidate note $nC " "dry run: C"
has "unknown note $nD " "dry run: D"; has "unknown note $nE " "dry run: E"; has "restored mark cleared: false" "dry run"
[[ $(print -r -- "$out" | grep -c '^orphan remote ') == 1 ]] || evidence "reconcile --dry-run" "$out" "exactly one orphan (Z)"
RB=$(print -r -- "$out" | sed -n "s/^candidate note $nB .*, remote \([0-9][0-9]*\) .*/\1/p"); [[ -n $RB ]] || fail "no remote id of B in the listing"
RC2=$(print -r -- "$out" | sed -n "s/^candidate note $nC .*, remote \([0-9][0-9]*\) .*/\1/p"); [[ $RC2 == $RC ]] || fail "remote id of C in the listing: ${RC2:-none}, expected $RC"
# FINDING when the proof differs from what the docs promise (docs/usage.md lists "an accepted submission" among the notes with "no proof")
print -r -- "$out" | grep "^candidate note $nB " | grep -q 'no proof' || print "FINDING: candidate B (an unchanged accepted submission of this gateway) is shown WITH proof, docs/usage.md lists an accepted submission among the notes without proof"
print -r -- "$out" | grep "^candidate note $nC " | grep -q 'no proof' || print "FINDING: candidate C (edited on the hub by another user) is not shown without proof"
run "status" gw edge upstream status shop; has "restored: reconcile before pushing" "mark after a dry run"
ok "reconcile --dry-run: A known, B and C candidates, D and E unknown, Z orphan; nothing changed"

# a normal listing (stores it), then every decision the way the docs describe, until the mark is cleared
run "reconcile listing" gw edge upstream reconcile shop --upstream hub; has "restored mark cleared: false" "listing"
run "decision B" gw edge upstream reconcile shop --upstream hub --note $nB --confirm $RB; has "restored mark cleared: false" "mark after only B"
run "decision C" gw edge upstream reconcile shop --upstream hub --note $nC --confirm $RC; has "restored mark cleared: false" "mark after only B and C"
run "decision D" gw edge upstream reconcile shop --upstream hub --note $nD --new; has "restored mark cleared: false" "mark after B, C and D"
run "decision E" gw edge upstream reconcile shop --upstream hub --note $nE --skip; has "restored mark cleared: true" "mark after the last decision"
print -r -- "$out" | sed 's/^/     /'
run "status" gw edge upstream status shop; hasnt "restored: reconcile before pushing" "status after the decisions"
ok "B, C confirmed, D new, E skipped; the restored mark is cleared by the last decision"

# ---------- 6. pushes after the reconcile: no second copies ----------
for t in A B C; do n=${(P)${:-n$t}}; out=$(print -r -- "$t edited after the restore $T" | cl edge edge-author /home/minigw/p note edit $n --revision 1 - 2>&1) || evidence "note edit $t" "$out" "edit failed"; done
run "push" gw edge upstream push shop
print -r -- "$out" | sed 's/^/     /'
has "replace $nA " "push: A"; has "replace $nB " "push: B"; has "skip $nD " "push: D"; has "which it rejected (rehearsal)" "push: D as rejected, with the reason"; hasnt "create $nD " "push: D is not reported as created"
has "conflict $nC " "push: C"; has "skip $nE " "push: E skipped"; has "created 0, replaced 2, skipped 2, refused 0, conflicts 1" "push counts"
run "resolve C" gw edge upstream resolve shop $nC --upstream hub --ours
run "push C" gw edge upstream push shop; has "replace $nC " "push after resolve --ours"
accept_pending
for t in A B C Z; do [[ $(nnotes "$t-$T") == 1 ]] || evidence "hub note list" "$(hubnotes)" "$t on the hub exactly once"; done
[[ $(nnotes "E-$T") == 0 ]] || evidence "hub note list" "$(hubnotes)" "E (skipped) on the hub"
out=$(cl hub hub-viewer /home/minigw/p note read $RC 2>&1); has "C edited after the restore $T" "hub note C after resolve --ours"
run "push again" gw edge upstream push shop; has "created 0, replaced 0" "second push"
ok "after the reconcile: A, B, C replaced (not copied), Z untouched, E skipped: the hub holds exactly ONE note each for A, B, C, Z and none of E"
# D was rejected on the hub before the restore. `--note D --new` allows the creation, but the text is the same, so the hub answers the same
# idempotency key with the earlier, rejected submission. The push reports it as skipped with the reason (never "create"), and the hub keeps no D.
[[ $(nnotes "D-$T") == 0 && $(hubnotes | wc -l | tr -d " ") == 4 ]] || evidence "hub note list" "$(hubnotes)" "the hub holds no D and four notes (A, B, C, Z)"
out=$(gw hub kb review shop 2>&1); [[ $out == "no submissions"* ]] || evidence "gw kb review" "$out" "nothing pending on the hub (D was not sent around its rejection)"
ok "D (rejected on the hub, then --new after the restore): reported as rejected with the reason, not created; the hub holds exactly ONE note each for A, B, C, Z and none for D and E"
