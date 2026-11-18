# Failure summary of `go test -json` events (jq -rs -f): a failed test prints its own output and its subtests' ("Parent/Sub"), a package that
# failed without a failing test (build error, timeout) prints its package-level output; at most 200 lines (the last ones) each.
def cut: if length > 200 then ["... (\(length - 200) earlier lines cut)"] + .[-200:] else . end;
def lines: map(.Output) | add // "" | split("\n") | map(select(. != "")) | cut;
. as $all
| ([$all[] | select(.Action == "fail" and .Test != null and (.Test | contains("/") | not))]) as $top
| ($top | map(.Package) | unique) as $tpk
| ($top[] | . as $f
    | "--- FAIL \($f.Package) \($f.Test)",
      ([$all[] | select(.Action == "output" and .Package == $f.Package and .Test != null and (.Test == $f.Test or (.Test | startswith($f.Test + "/"))))] | lines[])),
  ([$all[] | select(.Action == "fail" and .Test == null and ([.Package] | inside($tpk) | not))] | .[] | . as $f
    | "--- FAIL package \($f.Package)",
      ([$all[] | select(.Action == "output" and .Package == $f.Package and .Test == null)] | lines[]))
