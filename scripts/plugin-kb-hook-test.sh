#!/bin/zsh
# Preizkus plugins/minigw/scripts/kb-hook.sh (brief kb7b) na tem računalniku, brez Dockerja: lažen minigw zapiše argumente in stdin.
#   1. minigw ni nameščen → izhod 0, brez izpisa
#   2. argumenti (kb hook <dogodek>) in stdin gredo naprej, izpis minigw gre na stdout
#   3. pot s presledki in ' (CLAUDE_PLUGIN_DATA), tudi privzeta pot pod $HOME
#   4. lažna uname (MINGW64, kot Git Bash): enako
#   5. izhod minigw ≠ 0 (tudi 2) → izhod 0; stderr minigw ne uide
#   6. H1: minigw brez `kb hook` (--version 0.7.0, 0.7.12, smeti, nič, neštevilčno) se ne kliče; najnižja različica (0.7.13) in
#      novejše se
# Začasna mapa je stalna (ob vsakem teku se datoteke prepišejo), da test nič ne briše.
emulate -L zsh; set -u
cd ${0:A:h}/..
SCRIPT=$PWD/plugins/minigw/scripts/kb-hook.sh
T=${TMPDIR:-/tmp}/minigw-kb-hook-test
mkdir -p $T
fail() { print -u2 "FAIL: $*"; exit 1 }

# fake <mapa podatkov> <izhod> [<izpis --version>]: lažen minigw; zapiše argumente v $T/args in stdin v $T/stdin, na stdout »OUT«, na stderr »ERR«
fake() {
  mkdir -p "$1/bin"
  print "#!/bin/sh\n[ \"\$1\" = --version ] && { echo '${3-minigw 0.7.13}'; exit 0; }\nprintf '%s\\\\n' \"\$*\" > '$T/args'\ncat > '$T/stdin'\necho OUT\necho ERR >&2\nexit $2" > "$1/bin/minigw"
  chmod +x "$1/bin/minigw"
}
# run <spremenljivke okolja …>: kb-hook.sh start z JSON na stdin; $T/out, $T/err, $? v $rc
run() {
  print -n '{"session_id":"s1","cwd":"/x y"}' | env "$@" sh $SCRIPT start > $T/out 2> $T/err; rc=$?
}

# 1: ni nameščen
: > $T/args; : > $T/stdin
run CLAUDE_PLUGIN_DATA=$T/missing HOME=$T/nohome
[[ $rc == 0 ]] || fail "1: exit $rc"
[[ ! -s $T/out && ! -s $T/err && ! -s $T/args ]] || fail "1: output or a call without minigw"

# 2: argumenti, stdin, izpis
D=$T/data; fake $D 0
run CLAUDE_PLUGIN_DATA=$D HOME=$T/nohome
[[ $rc == 0 ]] || fail "2: exit $rc"
[[ $(<$T/args) == "kb hook start" ]] || fail "2: args: $(<$T/args)"
[[ $(<$T/stdin) == '{"session_id":"s1","cwd":"/x y"}' ]] || fail "2: stdin: $(<$T/stdin)"
[[ $(<$T/out) == OUT ]] || fail "2: stdout: $(<$T/out)"
[[ ! -s $T/err ]] || fail "2: stderr leaked: $(<$T/err)"
for ev in compact stop end; do
  print -n '{}' | CLAUDE_PLUGIN_DATA=$D sh $SCRIPT $ev > /dev/null 2>&1
  [[ $(<$T/args) == "kb hook $ev" ]] || fail "2: event $ev: $(<$T/args)"
done

# 3: pot s presledki in '; privzeta pot
P="$T/O'Brien data %1"; fake $P 0
run CLAUDE_PLUGIN_DATA=$P HOME=$T/nohome
[[ $rc == 0 && $(<$T/out) == OUT && $(<$T/args) == "kb hook start" ]] || fail "3: odd path (rc $rc)"
H="$T/home dir"; fake "$H/.claude/plugins/data/minigw" 0
: > $T/args
run -u CLAUDE_PLUGIN_DATA HOME=$H
[[ $rc == 0 && $(<$T/out) == OUT && $(<$T/args) == "kb hook start" ]] || fail "3: default path (rc $rc)"

# 4: Git Bash na Windows (lažna uname)
mkdir -p $T/fakeuname
print '#!/bin/sh\ncase $1 in -s) echo MINGW64_NT-10.0 ;; *) echo x86_64 ;; esac' > $T/fakeuname/uname; chmod +x $T/fakeuname/uname
: > $T/args
run CLAUDE_PLUGIN_DATA=$D HOME=$T/nohome PATH=$T/fakeuname:$PATH
[[ $rc == 0 && $(<$T/out) == OUT && $(<$T/args) == "kb hook start" ]] || fail "4: MINGW64 (rc $rc)"

# 5: minigw odpove
for code in 1 2 127; do
  fake $D $code
  run CLAUDE_PLUGIN_DATA=$D HOME=$T/nohome
  [[ $rc == 0 ]] || fail "5: minigw exit $code became $rc"
  [[ ! -s $T/err ]] || fail "5: stderr leaked: $(<$T/err)"
done

# 6: H1, različica
fake $D 0 "minigw 0.7.12" ; : > $T/args
run CLAUDE_PLUGIN_DATA=$D HOME=$T/nohome
[[ $rc == 0 && ! -s $T/out && ! -s $T/args ]] || fail "6: 0.7.12 was called (rc $rc)"
for old in "minigw 0.7.0" "minigw 0.6.9" "minigw 0.5.20" "minigw 0.7.9" "minigw 0.7.1"; do
  fake $D 0 "$old"; : > $T/args
  run CLAUDE_PLUGIN_DATA=$D HOME=$T/nohome
  [[ $rc == 0 && ! -s $T/out && ! -s $T/args ]] || fail "6: $old was called (rc $rc)"
done
for bad in "" "garbage" "minigw dev" "0.7.13-beta" "minigw ..13" "minigw 0.7." "error: unknown flag"; do
  fake $D 0 "$bad"; : > $T/args
  run CLAUDE_PLUGIN_DATA=$D HOME=$T/nohome
  [[ $rc == 0 && ! -s $T/out && ! -s $T/args ]] || fail "6: --version '$bad' was called (rc $rc)"
done
for ok in "minigw 0.7.13" "minigw 0.7.14" "minigw 0.8.0" "minigw 1.0" "minigw 0.7.100" "0.7.13"; do
  fake $D 0 "$ok"; : > $T/args
  run CLAUDE_PLUGIN_DATA=$D HOME=$T/nohome
  [[ $rc == 0 && $(<$T/out) == OUT && $(<$T/args) == "kb hook start" ]] || fail "6: --version '$ok' was not called (rc $rc)"
done

print "ok: kb-hook.sh"
