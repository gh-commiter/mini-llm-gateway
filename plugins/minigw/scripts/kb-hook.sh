#!/bin/sh
# Vtičnikovi kljuki za projektno bazo znanja (SessionStart, PreCompact, Stop, SessionEnd): `kb-hook.sh start|compact|stop|end`.
# Vsa logika je v odjemalcu (minigw kb hook <dogodek>, bere JSON kljuke na stdin); tu je le klic. Brez izpisa in z izhodom 0,
# če minigw ni nameščen ali odpove (kljuka seje nikoli ne moti). Izklopljeno, dokler uporabnik ne požene `minigw kb hooks on`.
# H1: minigw je pogosto starejši (brew, aplikacija, MSI, ~/.local/bin). Starejši minigw ukaza `kb hook` ne pozna in neznane
# ukaze preda claudu prek prehoda: vsak dogodek v vsaki mapi bi sprožil tek. Zato se kliče le minigw, ki je dovolj nov.
MIN=0.7.13  # najnižji minigw z ukazom `kb hook` (preveri ob izdaji: prva izdaja s tem ukazom)
B="${CLAUDE_PLUGIN_DATA:-$HOME/.claude/plugins/data/minigw}/bin"
EXE="$B/minigw"; [ -x "$EXE" ] || EXE="$B/minigw.exe"
[ -x "$EXE" ] || exit 0
# #153: iz podatkovne mape se NIC ne zažene, dokler (a) kljuke niso vklopljene za vsaj en projekt (kb-hooks.json, ista mapa kot
# os.UserConfigDir v odjemalcu; le lupinska orodja) in (b) SHA-256 datoteke ni enak zapisu, ki ga je install-minigw.sh napisal ob
# sprejemu (bin/.verified, vrstice »sha  pot«, prva je $EXE). Dirka ob SessionStart: nova datoteka brez novega zapisa = neskladje = tiho.
case $(uname -s) in
  Darwin) C="$HOME/Library/Application Support" ;;
  MINGW*|MSYS*|CYGWIN*) C=$(cygpath -u "${APPDATA:-}" 2>/dev/null) ;;
  *) C="${XDG_CONFIG_HOME:-$HOME/.config}" ;;
esac
grep -Eq '"project"[[:space:]]*:' "$C/minigw/kb-hooks.json" 2>/dev/null || exit 0
[ -f "$B/.verified" ] || exit 0
sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" 2>/dev/null | cut -d' ' -f1; else shasum -a 256 "$1" 2>/dev/null | cut -d' ' -f1; fi; }
ok=0
while read -r h p; do
  p=${p%"$(printf '\r')"}
  if [ $ok = 0 ]; then [ "$p" = "$EXE" ] || break; fi
  [ -n "$h" ] && [ "$(sha "$p")" = "$h" ] || { ok=0; break; }
  ok=1
done < "$B/.verified"
[ $ok = 1 ] || exit 0
# older a b: a < b po številskih poljih (kot v install-minigw.sh; awk, BusyBox nima sort -V)
older() { [ -n "$1" ] && [ -n "$2" ] && awk -v a="$1" -v b="$2" 'BEGIN { n = split(a, x, "."); m = split(b, y, "."); if (m > n) n = m
  for (i = 1; i <= n; i++) { if (x[i] + 0 < y[i] + 0) exit 0; if (x[i] + 0 > y[i] + 0) exit 1 }; exit 1 }'; }
v=$("$EXE" --version 2>/dev/null | head -1 | sed 's/^minigw //')
case $v in *[!0-9.]*|""|.*|*..*|*.) exit 0 ;; esac
older "$v" "$MIN" && exit 0
"$EXE" kb hook "$1" 2>/dev/null
exit 0
