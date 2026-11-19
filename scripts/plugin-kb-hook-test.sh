#!/bin/zsh
# Preizkus plugins/minigw/scripts/kb-hook.sh (brief kb7b) na tem računalniku, brez Dockerja: lažen minigw zapiše argumente in stdin.
#   1. minigw ni nameščen → izhod 0, brez izpisa
#   2. argumenti (kb hook <dogodek>) in stdin gredo naprej, izpis minigw gre na stdout
#   3. pot s presledki in ' (CLAUDE_PLUGIN_DATA), tudi privzeta pot pod $HOME
#   4. lažna uname (MINGW64, kot Git Bash): enako
#   5. izhod minigw ≠ 0 (tudi 2) → izhod 0; stderr minigw ne uide
#   6. H1: minigw brez `kb hook` (--version 0.7.0, 0.7.12, smeti, nič, neštevilčno) se ne kliče; najnižja različica (0.7.13) in
#      novejše se
#   7. #153: brez vklopljenih kljuk (kb-hooks.json) ali brez zapisa bin/.verified, s starim zapisom (datoteka zamenjana), z zapisom
#      za drugo pot, z neskladno drugo vrstico: minigw se NE zažene (za start, compact, stop, end); veljaven zapis (tudi s CRLF,
#      dve vrstici kot Windows z nameščenim minigw): zažene se
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
  vrec "$1"
}
# vrec <mapa podatkov>: zapis, kot ga napiše install-minigw.sh (sha  pot)
vrec() { print -r -- "$(shasum -a 256 "$1/bin/minigw" | cut -d' ' -f1)  $1/bin/minigw" > "$1/bin/.verified" }
# hookson <HOME>: kb-hooks.json z enim vklopljenim projektom, kjer ga bere kljuka (os.UserConfigDir)
hookson() {
  local c="$1/.config/minigw"; [[ $(uname -s) == Darwin ]] && c="$1/Library/Application Support/minigw"
  mkdir -p $c; print '{\n  "version": 1,\n  "enabled": [\n    {\n      "gateway": "g",\n      "project": 3\n    }\n  ]\n}' > $c/kb-hooks.json
}
hookson $T/nohome
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
  print -n '{}' | CLAUDE_PLUGIN_DATA=$D HOME=$T/nohome sh $SCRIPT $ev > /dev/null 2>&1
  [[ $(<$T/args) == "kb hook $ev" ]] || fail "2: event $ev: $(<$T/args)"
done

# 3: pot s presledki in '; privzeta pot
P="$T/O'Brien data %1"; fake $P 0
run CLAUDE_PLUGIN_DATA=$P HOME=$T/nohome
[[ $rc == 0 && $(<$T/out) == OUT && $(<$T/args) == "kb hook start" ]] || fail "3: odd path (rc $rc)"
H="$T/home dir"; hookson $H; fake "$H/.claude/plugins/data/minigw" 0
: > $T/args
run -u CLAUDE_PLUGIN_DATA HOME=$H
[[ $rc == 0 && $(<$T/out) == OUT && $(<$T/args) == "kb hook start" ]] || fail "3: default path (rc $rc)"

# 4: Git Bash na Windows (lažna uname)
mkdir -p $T/fakeuname
print '#!/bin/sh\ncase $1 in -s) echo MINGW64_NT-10.0 ;; *) echo x86_64 ;; esac' > $T/fakeuname/uname
print '#!/bin/sh\nprintf "%s\\n" "$2"' > $T/fakeuname/cygpath; chmod +x $T/fakeuname/*
mkdir -p $T/appdata/minigw; cp "$(
  [[ $(uname -s) == Darwin ]] && print -r -- "$T/nohome/Library/Application Support" || print -r -- $T/nohome/.config)/minigw/kb-hooks.json" $T/appdata/minigw/
: > $T/args
run CLAUDE_PLUGIN_DATA=$D HOME=$T/nohome APPDATA=$T/appdata PATH=$T/fakeuname:$PATH
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

# 7: #153, nepreverjen ali nepovezan minigw se ne zažene; marker nastane le, če se zažene
M=$T/marker; E=$T/data7
evil() { mkdir -p $E/bin; print "#!/bin/sh\ntouch '$M'\n[ \"\$1\" = --version ] && echo 'minigw 0.8.0'\nexit 0" > $E/bin/minigw; chmod +x $E/bin/minigw }
# nobody <HOME> <opis>: vsi štirje dogodki, marker ne sme nastati
nobody() {
  for ev in start compact stop end; do
    mv -f $M $T/marker.old 2> /dev/null
    print -n '{}' | env CLAUDE_PLUGIN_DATA=$E HOME=$1 APPDATA=$T/nowhere sh $SCRIPT $ev > $T/out 2> $T/err; rc=$?
    [[ $rc == 0 && ! -e $M && ! -s $T/out && ! -s $T/err ]] || fail "7: $2, event $ev (rc $rc, marker $([[ -e $M ]] && print yes || print no))"
  done
}
mkdir -p $T/offhome
# hooks ON, tampered, stale record (the record is of an earlier file)
fake $E 0; evil; nobody $T/nohome "tampered binary with a stale record"
# hooks ON, no record at all
mv -f $E/bin/.verified $T/verified.old; nobody $T/nohome "no record"
# hooks OFF, no record; and hooks OFF with a valid record of the very file
nobody $T/offhome "hooks off, no record"
vrec $E; nobody $T/offhome "hooks off, valid record"
# hooks ON, record of a different path (hash right)
print -r -- "$(shasum -a 256 $E/bin/minigw | cut -d' ' -f1)  $T/elsewhere/minigw" > $E/bin/.verified; nobody $T/nohome "record for another path"
# hooks ON, record file unreadable garbage / empty
print garbage > $E/bin/.verified; nobody $T/nohome "garbage record"; : > $E/bin/.verified; nobody $T/nohome "empty record"
# hooks ON, state file with an empty enabled list
mkdir -p $T/emptyhome; hookson $T/emptyhome
c=$([[ $(uname -s) == Darwin ]] && print -r -- "$T/emptyhome/Library/Application Support/minigw" || print -r -- $T/emptyhome/.config/minigw)
print '{"version":1,"enabled":[]}' > $c/kb-hooks.json; vrec $E; nobody $T/emptyhome "no project switched on"
# the matching record with hooks on runs it (marker appears); CRLF record too
vrec $E; print -n '{}' | CLAUDE_PLUGIN_DATA=$E HOME=$T/nohome sh $SCRIPT stop > /dev/null 2>&1; [[ -e $M ]] || fail "7: verified binary was not run"
mv -f $M $T/marker.old; r=$(<$E/bin/.verified); print -r -- "$r"$'\r' > $E/bin/.verified
print -n '{}' | CLAUDE_PLUGIN_DATA=$E HOME=$T/nohome sh $SCRIPT stop > /dev/null 2>&1; [[ -e $M ]] || fail "7: CRLF record was not accepted"
# two lines (Windows with an installed minigw): the second file must match too
mv -f $M $T/marker.old; X=$T/second; print second > $X
print -r -- "$(shasum -a 256 $E/bin/minigw | cut -d' ' -f1)  $E/bin/minigw" > $E/bin/.verified; print -r -- "0000000000000000000000000000000000000000000000000000000000000000  $X" >> $E/bin/.verified
nobody $T/nohome "second line does not match"
print -r -- "$(shasum -a 256 $X | cut -d' ' -f1)  $X" > $T/l2; { head -1 $E/bin/.verified; cat $T/l2; } > $E/bin/.verified.n; mv -f $E/bin/.verified.n $E/bin/.verified
print -n '{}' | CLAUDE_PLUGIN_DATA=$E HOME=$T/nohome sh $SCRIPT stop > /dev/null 2>&1; [[ -e $M ]] || fail "7: two matching lines were not accepted"
mv -f $M $T/marker.old
# a symlinked bin/minigw is hashed through the link (replace the target: stale record)
print "#!/bin/sh\ntouch '$M'\necho 'minigw 0.8.0'" > $T/target; chmod +x $T/target
mv -f $E/bin/minigw $T/minigw.old; ln -s $T/target $E/bin/minigw; vrec $E
print -n '{}' | CLAUDE_PLUGIN_DATA=$E HOME=$T/nohome sh $SCRIPT stop > /dev/null 2>&1; [[ -e $M ]] || fail "7: verified link target was not run"
mv -f $M $T/marker.old; print "#!/bin/sh\ntouch '$M'\necho 'minigw 0.8.0 tampered'" > $T/target
nobody $T/nohome "link target replaced after the record"

# 7b (#153 review): cilj je pot iz zapisa (minigw ali minigw.exe), ne -x na »minigw«; zadnja vrstica brez konca vrstice se preveri tudi
E=$T/data7b; mkdir -p $E/bin; M2=$T/marker2
print "#!/bin/sh\ntouch '$M'\n[ \"\$1\" = --version ] && echo 'minigw 0.8.0'\nexit 0" > $E/bin/minigw.exe; chmod +x $E/bin/minigw.exe
print "#!/bin/sh\ntouch '$M2'\n[ \"\$1\" = --version ] && echo 'minigw 0.8.0'\nexit 0" > $E/bin/minigw; chmod +x $E/bin/minigw
hx() { print -n -- "$(shasum -a 256 $E/bin/minigw.exe | cut -d' ' -f1)  $E/bin/minigw.exe" > $E/bin/.verified }  # brez konca vrstice
hx; rm -f $M $M2 2> /dev/null; print -n '{}' | CLAUDE_PLUGIN_DATA=$E HOME=$T/nohome sh $SCRIPT stop > /dev/null 2>&1
[[ -e $M && ! -e $M2 ]] || fail "7b: record for minigw.exe (a shim minigw next to it): exe not run, or the shim was"
mv -f $M $T/marker.old
print -r -- "$(shasum -a 256 $E/bin/minigw | cut -d' ' -f1)  $E/bin/minigw" > $E/bin/.verified
print -n '{}' | CLAUDE_PLUGIN_DATA=$E HOME=$T/nohome sh $SCRIPT stop > /dev/null 2>&1; [[ -e $M2 && ! -e $M ]] || fail "7b: record for the shim did not run the shim"
mv -f $M2 $T/marker2.old
print -r -- "$(shasum -a 256 $E/bin/minigw.exe | cut -d' ' -f1)  $E/bin/minigw.exe" > $E/bin/.verified; print -rn -- "0000000000000000000000000000000000000000000000000000000000000000  $X" >> $E/bin/.verified
nobody7() { for ev in start stop; do print -n '{}' | CLAUDE_PLUGIN_DATA=$E HOME=$T/nohome sh $SCRIPT $ev > /dev/null 2>&1; [[ ! -e $M && ! -e $M2 ]] || fail "7b: $1 ($ev)"; done }
nobody7 "unterminated bad second line was accepted"
print -r -- "x  $E/bin/minigw.exe.evil" > $E/bin/.verified; nobody7 "a first line naming another file was accepted"
print -r -- "$(shasum -a 256 $E/bin/minigw.exe | cut -d' ' -f1)  $E/bin/minigw.exe" > $E/bin/.verified; print -r -- "0000  /nonexistent" >> $E/bin/.verified
nobody7 "bad second line (terminated) was accepted"

print "ok: kb-hook.sh"
