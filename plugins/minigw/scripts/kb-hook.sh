#!/bin/sh
# Vtičnikovi kljuki za projektno bazo znanja (SessionStart, PreCompact, Stop, SessionEnd): `kb-hook.sh start|compact|stop|end`.
# Vsa logika je v odjemalcu (minigw kb hook <dogodek>, bere JSON kljuke na stdin); tu je le klic. Brez izpisa in z izhodom 0,
# če minigw ni nameščen ali odpove (kljuka seje nikoli ne moti). Izklopljeno, dokler uporabnik ne požene `minigw kb hooks on`.
# H1: minigw je pogosto starejši (brew, aplikacija, MSI, ~/.local/bin). Starejši minigw ukaza `kb hook` ne pozna in neznane
# ukaze preda claudu prek prehoda: vsak dogodek v vsaki mapi bi sprožil tek. Zato se kliče le minigw, ki je dovolj nov.
MIN=0.7.13  # najnižji minigw z ukazom `kb hook` (preveri ob izdaji: prva izdaja s tem ukazom)
EXE="${CLAUDE_PLUGIN_DATA:-$HOME/.claude/plugins/data/minigw}/bin/minigw"
[ -x "$EXE" ] || exit 0
# older a b: a < b po številskih poljih (kot v install-minigw.sh; awk, BusyBox nima sort -V)
older() { [ -n "$1" ] && [ -n "$2" ] && awk -v a="$1" -v b="$2" 'BEGIN { n = split(a, x, "."); m = split(b, y, "."); if (m > n) n = m
  for (i = 1; i <= n; i++) { if (x[i] + 0 < y[i] + 0) exit 0; if (x[i] + 0 > y[i] + 0) exit 1 }; exit 1 }'; }
v=$("$EXE" --version 2>/dev/null | head -1 | sed 's/^minigw //')
case $v in *[!0-9.]*|""|.*|*..*|*.) exit 0 ;; esac
older "$v" "$MIN" && exit 0
"$EXE" kb hook "$1" 2>/dev/null
exit 0
