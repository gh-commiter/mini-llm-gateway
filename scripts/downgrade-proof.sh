#!/bin/zsh
# Proof of the way back: a minigw serve gateway upgraded from the stable 0.7.0 goes back to 0.7.0 WITH its data.
# Everything runs on this machine, offline: 127.0.0.1 only, a temp HOME and config folder, no network, no Keychain
# (MINIGW_SECRETS=file), nothing of the real user's gateway is read or written. One line per step; exit 0 only if all hold.
#   1. build 0.7.0 (git archive 71cfd5fb) and the current client from git into the scratch folder
#   2. a 0.7.0 database with a user, a project and a note (seeded with 0.7.0's own core package: there is no offline command
#      that writes notes), a 0.7.0 gateway (loopback) reads it
#   3. the current gateway migrates it 8 -> current: schema, note, snapshot core-8-<time>.db next to the database
#   4. three more snapshots the way a user gets them (gw core backup, then gw core restore x3); core-8 is STILL there
#      (pruneSnapshots, 0.7.37), and the 0.7.36 rule (newest 3 only) would have deleted it
#   5. 0.7.0 over the migrated data: what the user sees; then 0.7.0's gw core restore of the core-8 snapshot: project and
#      note are back, what was created after the upgrade is gone
# Usage: scripts/downgrade-proof.sh   (the scratch folder is only left behind, never cleaned: KEEP the evidence)
emulate -L zsh; set -eu
cd ${0:A:h}/..
REPO=$PWD
OLDREV=71cfd5fb
W="${WAYBACK_SCRATCH:-/Volumes/SAMSUNG990PRO/Mini Gateway Scratch/dev-builds/wayback-0738}"
R="$W/run-$(date +%Y%m%dT%H%M%S)"
PIDS=(); T0=$SECONDS
trap 'for p in $PIDS; do kill $p 2>/dev/null || true; done' EXIT  # exactly the pids this script started
fail() { print -u2 "FAIL: $*"; print -u2 "evidence: $R"; exit 1 }
ok() { print "PASS $*" }
mkdir -p "$W/old" "$W/cur" "$R/home" "$R/cfg"

# ---- 1. build ----
HELPER=$R/wbhelper.go
cat > $HELPER <<'EOF'
// wbhelper: offline seed/list/add on a core folder with the core package of the tree it is built in.
package main

import (
	"context"
	"fmt"
	"os"

	"minigw/core"
)

func main() {
	ctx := context.Background()
	st, err := core.Open(os.Args[2]+"/core", core.Options{})
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer st.Close()
	one := func(q string, a ...any) (v string) { st.DB().QueryRowContext(ctx, q, a...).Scan(&v); return }
	var uid, pid int64
	fmt.Sscan(one(`SELECT id FROM users WHERE name='ana'`), &uid)
	fmt.Sscan(one(`SELECT id FROM projects WHERE slug='wb'`), &pid)
	pr := core.Principal{UserID: uid, DeviceID: "dev-wb"}
	switch os.Args[1] {
	case "seed":
		u, e := st.CreateUser(ctx, "ana")
		must(e)
		p, e := st.CreateProject(ctx, "wb", "Way back")
		must(e)
		must(st.SetMember(ctx, p.ID, u.ID, core.Developer))
		must(st.SetBinding(ctx, "dev-wb", u.ID))
		_, _, e = st.CreateNote(ctx, core.Principal{UserID: u.ID, DeviceID: "dev-wb"}, p.ID, "k1", core.NoteInput{Class: "data", Title: os.Args[3], Body: "written by 0.7.0"})
		must(e)
	case "add":
		_, _, e := st.CreateNote(ctx, pr, pid, "k-"+os.Args[3], core.NoteInput{Class: "data", Title: os.Args[3], Body: "after the upgrade"})
		must(e)
	case "list":
		fmt.Println("schema=" + one(`SELECT value FROM meta WHERE key='schema'`))
		pg, e := st.ListNotes(ctx, pr, pid, "", "", 100)
		must(e)
		for _, n := range pg.Notes {
			fmt.Println("note=" + n.Title)
		}
	}
}

func must(err error) {
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
EOF
git archive $OLDREV client | tar -x -C "$W/old"
git archive HEAD client | tar -x -C "$W/cur"
for t in old cur; do
  mkdir -p "$W/$t/client/cmd/wbhelper"; cp $HELPER "$W/$t/client/cmd/wbhelper/main.go"
  # no GOFLAGS=-mod=mod, no network: a module missing from the cache is a stop, not a download
  (cd "$W/$t/client" && GOFLAGS=-mod=readonly GOPROXY=off go build -trimpath -buildvcs=false -o "$R/build-minigw-$t" . && GOFLAGS=-mod=readonly GOPROXY=off go build -trimpath -buildvcs=false -o "$R/build-wbhelper-$t" ./cmd/wbhelper) ||
    fail "1: build of $t failed (modules not in the module cache? no downloads are allowed)"
done
# Every program that runs here (it listens or connects, even on loopback) runs from a STABLE path with a STABLE signature, so a
# firewall / Little Snitch sees one program, not a new one per build (as scripts' signed-test-exec.sh in the mg-build skill):
# ${TMPDIR}/mg-signed-tests/downgrade-<name>.<sha16>, Developer ID, identifier dev.trojar.minigateway.gotest.downgrade-<name>.
IDENT=${MG_TEST_SIGN_IDENTITY:-Developer ID Application: Uros Trojar (Y2M489RDV8)}
SIGN=0
[[ $(uname -s) == Darwin ]] && /usr/bin/security find-identity -v -p codesigning 2>/dev/null | grep -qF "$IDENT" && SIGN=1
(( SIGN )) || print "NOTE: no signing identity here (or not macOS): running the built binaries unsigned"
SD=${TMPDIR:-/tmp}/mg-signed-tests; mkdir -p -m 700 $SD
REALHOME=$(/usr/bin/dscl . -read /Users/$(id -un) NFSHomeDirectory 2>/dev/null | awk '{print $2}'); [[ -d $REALHOME ]] || REALHOME=$HOME
prog() { # prog <name>: the path to run for $R/build-<name>
  local b=$R/build-$1
  (( SIGN )) || { print -r -- $b; return }
  local d=$SD/downgrade-$1.$(/usr/bin/shasum -a 256 $b | cut -c1-16)
  if [[ ! -x $d ]]; then
    cp -f $b $d.$$ && HOME=$REALHOME /usr/bin/codesign --force --sign "$IDENT" --identifier "dev.trojar.minigateway.gotest.downgrade-$1" $d.$$ >/dev/null 2>&1 &&
      mv -f $d.$$ $d || fail "cannot sign $1; not running an unsigned program"
  fi
  print -r -- $d
}
MO=$(prog minigw-old); MC=$(prog minigw-cur); HO=$(prog wbhelper-old); HC=$(prog wbhelper-cur)
V_OLD=$("$MO" --version 2>&1 | head -1); V_CUR=$("$MC" --version 2>&1 | head -1)
[[ $V_OLD == *0.7.0* ]] || fail "1: old client says: $V_OLD"
ok "1: built $V_OLD ($OLDREV) and $V_CUR ($(git rev-parse --short HEAD)) into $R"

# ---- common: every call has its own HOME, MINIGW_SECRETS=file, --config; a minimal PATH for the gateways (no claude/codex) ----
export HOME=$R/home MINIGW_SECRETS=file
CFG=$R/cfg/serve.json; CORE=$R/cfg/core
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
"$MO" serve init --config $CFG --port $PORT > $R/init.log 2>&1 || fail "2: serve init: $(<$R/init.log)"
python3 - $CFG "$R/scratch" <<'EOF'
import json, sys
c = json.load(open(sys.argv[1])); c["address"] = "127.0.0.1"; c["scratch"] = sys.argv[2]
json.dump(c, open(sys.argv[1], "w"), indent=2)
EOF
[[ $(jq -r .address $CFG) == 127.0.0.1 ]] || fail "2: config is not loopback only"

SPID=""
start() { # start <client> <log>: the gateway of that client on 127.0.0.1, waits for the admin API
  PATH=/usr/bin:/bin "$1" serve --config $CFG > "$2" 2>&1 &
  SPID=$!; PIDS+=($SPID)
  local i; for i in {1..100}; do
    "$1" gw status --config $CFG 2>/dev/null | grep -q "Running" && break; sleep 0.2
  done
  kill -0 $SPID 2>/dev/null || fail "gateway died: $(<$2)"
  local l; l=$(lsof -nP -a -p $SPID -iTCP -sTCP:LISTEN 2>/dev/null | tail -n +2)
  [[ -n $l && $l != *'*:'* && $l != *0.0.0.0* && $l == *127.0.0.1:$PORT* ]] || fail "gateway does not listen on 127.0.0.1 only: $l"
}
stop() { kill $SPID; wait $SPID 2>/dev/null || true; SPID="" }
gw() { local c=$1; shift; "$c" gw "$@" --config $CFG 2>&1 || true }

# ---- 2. a 0.7.0 database ----
"$HO" seed $R/cfg before-upgrade | tee $R/seed.log | grep -q . && fail "2: seed: $(<$R/seed.log)"
[[ $("$HO" list $R/cfg) == $'schema=8\nnote=before-upgrade' ]] || fail "2: 0.7.0 database is not schema 8 with the note: $("$HO" list $R/cfg)"
start $MO $R/serve-070-a.log
P=$(gw $MO project list)
# 2b. the NEW client against this 0.7.0 gateway (its own HOME: connection.json and device key live there, MINIGW_SECRETS=file)
TOKEN=$(grep -o "'mgw-[^']*'" $R/init.log | head -1 | tr -d "'")
HNEW=$R/home-new; WNEW=$R/work-new; mkdir -p $HNEW $WNEW
cl() { local h=$1 c=$2; shift 2; (cd $WNEW && HOME=$h "$c" "$@" 2>&1) }  # exit status is the client's own
cl $HNEW $MC connect localhost:$PORT --token "$TOKEN" > $R/connect-new.log || fail "2b: connect: $(<$R/connect-new.log)"
NS=$(cl $HNEW $MC status)
[[ $NS == *"gateway 0.7.0"* && $NS == *"Status:    connected"* ]] || fail "2b: status: $NS"
DEV=$(print -r -- $NS | sed -n 's/^Device: *\([^ ]*\) .*/\1/p')
[[ -n $DEV ]] || fail "2b: no device id in: $NS"
# project routes need the device on the approved list (loopback status says "approved" without it) AND bound to a user
gw $MO devices allow $DEV --name proof-client > $R/allow.log; gw $MO bind $DEV ana > $R/bind.log
[[ $(gw $MO status) == *"Providers:"*"claude-cli ✗"* || $(gw $MO status) != *Providers:*✓* ]] || fail "2b: the temp gateway has a provider"
HINT_KB="this gateway is 0.7.0; kb needs 0.7.10 or newer: update the gateway (/minigw:gw update there)"
HINT_TEAM="this gateway is 0.7.0; team needs 0.7.1 or newer: update the gateway (/minigw:gw update there)"
cl $HNEW $MC project link wb > $R/link-new.log || fail "2b: project link: $(<$R/link-new.log)"
for c in "kb search test" "kb submit wb --kind finding -" "team list"; do
  want=$HINT_KB; [[ $c == team* ]] && want=$HINT_TEAM
  out=$(print "one line" | cl $HNEW $MC ${=c}) && fail "2b: '$c' exited 0: $out"
  [[ $out == *"$want"* && $out != *"not found or not visible"* ]] || fail "2b: '$c' did not give the hint: $out"
  print "     new client, 0.7.0 gateway: minigw $c -> $out"
done
stop
ok "2b: the 0.7.37 client against the 0.7.0 gateway: status connected (gateway 0.7.0); kb search, kb submit and team list each fail with the version hint, no model run"
[[ $P == *wb* ]] || fail "2: 0.7.0 gateway does not list the project: $P"
ok "2: 0.7.0 database schema 8: user ana, project wb, note before-upgrade; 0.7.0 gateway lists: $P"

# ---- 3. the current gateway migrates ----
start $MC $R/serve-cur-a.log
P=$(gw $MC project list)
# 3b. the 0.7.0 client against the current gateway (no provider on this gateway: asserted)
HOLD=$R/home-old; mkdir -p $HOLD
[[ $(gw $MC status) != *Providers:*✓* ]] || fail "3b: the temp gateway has a provider"
cl $HOLD $MO connect localhost:$PORT --token "$TOKEN" > $R/connect-old.log || fail "3b: 0.7.0 connect: $(<$R/connect-old.log)"
OS=$(cl $HOLD $MO status); [[ $OS == *"Status:    connected"* ]] || fail "3b: 0.7.0 status: $OS"
OM=$(cl $HOLD $MO models) || fail "3b: 0.7.0 models failed: $OM"
OK=$(cd $WNEW && HOME=$HOLD timeout 2 $MO kb search test 2>&1) && OKRC=0 || OKRC=$?
print "     0.7.0 client, current gateway: status -> ${${(f)OS}[(r)Status:*]}; models -> ${OM//$'\n'/ | } (rc 0); kb search test -> rc=$OKRC: $OK"
stop
[[ $P == *wb* ]] || fail "3: current gateway does not list the project: $P (log: $(<$R/serve-cur-a.log))"
L=$("$HC" list $R/cfg)
SCHEMA=${${(f)L}[1]#schema=}
(( SCHEMA > 8 )) || fail "3: schema after the upgrade: $L"
[[ $L == *note=before-upgrade* ]] || fail "3: note not readable after the upgrade: $L"
SNAP8=(${CORE}/core-8-*.db(N)); (( $#SNAP8 == 1 )) || fail "3: expected one core-8 snapshot: $(ls $CORE)"
"$HC" add $R/cfg after-upgrade
ok "3b: the 0.7.0 client against the current gateway: status connected, models answers (see above); kb search test is a model request there (see above)"
ok "3: current gateway migrated 8 -> $SCHEMA, note readable, snapshot ${SNAP8[1]:t}; core folder: $(ls $CORE | tr '\n' ' ')"

# ---- 4. three more snapshots; core-8 survives ----
start $MC $R/serve-cur-b.log
B=$(gw $MC core backup upgraded.db); stop
[[ -f $CORE/backups/upgraded.db ]] || fail "4: no backup: $B"
for i in 1 2 3; do
  sleep 0.1
  O=$("$MC" gw core restore upgraded.db --yes --config $CFG 2>&1) || fail "4: current restore $i: $O"
done
ALL=(${CORE}/core-*-*.db(N)); [[ -f ${SNAP8[1]} ]] || fail "4: the core-8 snapshot is gone: $(ls $CORE)"
NEWEST3=(${(f)"$(print -l ${ALL:t} | sort -t- -k3 | tail -3)"}) # base names: the folder's own dashes would break the sort fields
(( ${NEWEST3[(Ie)${SNAP8[1]:t}]} == 0 )) || fail "4: core-8 would survive the old rule too (not a proof of the change)"
git show dcb69041^:client/core/migrate.go | grep -q "for len(all) > keepSnaps" || fail "4: could not read the 0.7.36 rule"
ok "4: after 3 restores core-8 is kept; the 0.7.36 rule ('keeps the newest keepSnaps=3', migrate.go before dcb69041) would keep only: ${(j: :)NEWEST3}"

# ---- 5. back to 0.7.0 ----
start $MO $R/serve-070-b.log
STATUS=$(gw $MO status); PL=$(gw $MO project list); stop
WARN=$(grep -i -E "WARNING|newer schema" $R/serve-070-b.log || true)
[[ $PL == *"not available"* && $WARN == *"newer schema"* ]] || fail "5: 0.7.0 over the migrated data did not refuse as claimed: $PL / $WARN"
print "     gw status    : ${${(f)STATUS}[(r)*rojects*]}"
print "     gw project list: $PL"
print "     gateway log  : $WARN"
RO=$("$MO" gw core restore ${SNAP8[1]} --yes --config $CFG 2>&1) || fail "5: 0.7.0 restore: $RO"
print "     gw core restore: ${RO//$'\n'/ | }"
start $MO $R/serve-070-c.log
PL2=$(gw $MO project list); stop
L2=$("$HO" list $R/cfg)
[[ $PL2 == *wb* ]] || fail "5: project not back: $PL2"
[[ $L2 == $'schema=8\nnote=before-upgrade' ]] || fail "5: after the restore: $L2 (expected schema 8, only the note from before the upgrade)"
ok "5: 0.7.0 restored ${SNAP8[1]:t}: project wb listed, schema 8, note before-upgrade present, after-upgrade gone"
print "evidence in $R; elapsed $((SECONDS - T0)) s; programs: ${MO:t} ${MC:t}"
