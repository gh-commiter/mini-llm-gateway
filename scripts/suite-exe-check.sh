#!/bin/zsh
# The Windows test-suite exe runs from an EMPTY folder (no source tree, no testdata/ beside it). This reproduces that on this
# machine: build the test binaries of client/ and client/cmd/minigw-tui, run the tests that read files from a fresh empty folder.
# Which tests: the Test functions whose own body mentions srcFile( os.ReadFile( os.Open( filepath.Glob( os.ReadDir( (grep at run time).
# MG_SIGNED_EXEC=<path to signed-test-exec.sh> runs the binary through the signed wrapper (macOS firewall); unset: run it directly.
# Exit: 0 when no test failed, 1 otherwise. Usage: scripts/suite-exe-check.sh
emulate -L zsh; set -u
root=${0:A:h:h}
tmp=$(mktemp -d "${TMPDIR:-/tmp}/suite-exe-check.XXXXXX") || exit 2
home=$tmp/home; mkdir -p $home
rc=0
for pkg in . ./cmd/minigw-tui; do
  src=$root/client/${pkg#./}
  bin=$tmp/${${pkg:t}:#.}.test
  (cd $root/client && go test -c -o $bin $pkg) || { print -u2 "suite-exe-check: build of $pkg failed"; exit 1 }
  names=$(awk '
    FNR==1 { fn="" }
    /^func Test[A-Za-z0-9_]*\(t \*testing\.T\)/ { fn=$2; sub(/\(.*/, "", fn) }
    /srcFile\(|os\.ReadFile\(|os\.Open\(|filepath\.Glob\(|os\.ReadDir\(/ { if (fn != "") hit[fn]=1 }
    END { for (n in hit) print n }' $src/*_test.go | sort | paste -sd'|' -)
  count=$(print -r -- $names | tr '|' '\n' | grep -c .)
  print "suite-exe-check: $pkg: $count tests read files"
  [[ $count -gt 0 ]] || continue
  run=$tmp/run-${pkg:t}; mkdir -p $run
  if [[ -n ${MG_SIGNED_EXEC:-} ]]; then runner=($MG_SIGNED_EXEC $bin); else runner=($bin); fi
  (cd $run && HOME=$home $runner -test.run "^($names)\$" -test.v < /dev/null) > $tmp/out-${pkg:t}.log 2>&1
  r=$?
  print "suite-exe-check: $pkg: $(grep -c '^--- PASS' $tmp/out-${pkg:t}.log) pass, $(grep -c '^--- FAIL' $tmp/out-${pkg:t}.log) fail, $(grep -c '^--- SKIP' $tmp/out-${pkg:t}.log) skip"
  if [[ $r -ne 0 ]]; then grep -E '^(--- FAIL|    .*_test\.go)' $tmp/out-${pkg:t}.log | head -40; rc=1; fi
done
print "suite-exe-check: logs in $tmp"
exit $rc
