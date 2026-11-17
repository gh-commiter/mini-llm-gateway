#!/bin/sh
# Vtičnikovi kljuki za projektno bazo znanja (SessionStart, PreCompact, Stop, SessionEnd): `kb-hook.sh start|compact|stop|end`.
# Vsa logika je v odjemalcu (minigw kb hook <dogodek>, bere JSON kljuke na stdin); tu je le klic. Brez izpisa in z izhodom 0,
# če minigw ni nameščen ali odpove (kljuka seje nikoli ne moti). Izklopljeno, dokler uporabnik ne požene `minigw kb hooks on`.
EXE="${CLAUDE_PLUGIN_DATA:-$HOME/.claude/plugins/data/minigw}/bin/minigw"
[ -x "$EXE" ] || exit 0
"$EXE" kb hook "$1" 2>/dev/null
exit 0
