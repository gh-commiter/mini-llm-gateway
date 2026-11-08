#!/bin/zsh
# Preizkus paketov MSI (scripts/build-msi.sh, client/minigw.wxs): zgradi minigw za Windows, oba MSI, in z msiinfo in
# msiextract preveri datoteke, vrednosti registra, PATH, bližnjico, zapiranje in zagon pladnja ter namestitev na uporabnika.
#   msi-test.sh [mapa]   MSI ostaneta v mapi (privzeto začasna)
emulate -L zsh; set -eu
cd ${0:A:h}/..
T=$(mktemp -d); trap 'rm -rf $T' EXIT
OUT=${1:-$T/out}; mkdir -p $OUT
fail() { print -u2 "FAIL: $*"; exit 1 }
V=$(grep -o 'const version = "[^"]*"' client/main.go | cut -d'"' -f2)

for arch in amd64 arm64; do
  (cd client && CGO_ENABLED=0 GOOS=windows GOARCH=$arch go build -trimpath -ldflags "-s -w" -o $OUT/minigw-windows-$arch.exe .)
  (cd client && CGO_ENABLED=0 GOOS=windows GOARCH=$arch go build -trimpath -ldflags "-s -w -H=windowsgui" -o $OUT/minigw-tray-windows-$arch.exe .)
done
scripts/build-msi.sh $V $OUT

for arch in amd64 arm64; do
  m=$OUT/minigw-windows-$arch.msi
  t() { msiinfo export $m $1 | tail -n +4 }
  has() { t $1 | grep -qF -- "$2" || fail "$arch: $1 has no $2" }
  msiinfo suminfo $m | grep -q '^Source: 10 ' || fail "$arch: not compressed per-user without elevation (Word Count 10)"
  t Property | grep -q '^ALLUSERS' && fail "$arch: ALLUSERS set (per-machine)"
  has Property "ProductVersion	$V"
  has Directory "INSTALLDIR	ProgramsDir	Mini Gateway"
  has Directory "ProgramsDir	LocalAppDataFolder	Programs"
  has Registry 'Software\Microsoft\Windows\CurrentVersion\Run	MiniGatewayRouter	"[INSTALLDIR]minigw-tray.exe" tray --config "[AppDataFolder]minigw\serve.json"'
  has Environment "=-PATH	[~];[LocalAppDataFolder]Programs\Mini Gateway"
  # Ista različica znova zamenja prejšnjo (769 = MigrateFeatures + VersionMin/MaxInclusive, ne le zaznava)
  has Upgrade "	$V	$V		769		WIX_SAME_VERSION_UPGRADE_DETECTED"
  has Shortcut "ProgramMenuFolder	Mini Gateway	Main	[INSTALLDIR]minigw-tray.exe	tray"
  # Pladenj zapre minigw-tray.exe quit (program brez konzole, tip 18 + 64): nobeno okno cmd med (od)namestitvijo.
  has CustomAction "CloseTray	82	tray	quit"
  has InstallExecuteSequence 'CloseTray	REMOVE~="ALL"'
  t CustomAction | grep -qiE 'cmd\.exe|tasklist|taskkill|powershell|PING' && fail "$arch: a custom action runs a console tool (window flashes)"
  has CustomAction "StartTray	210	tray	tray --quiet-if-running --config"
  has InstallExecuteSequence 'StartTray	NOT (REMOVE~="ALL")'
  has LaunchCondition "NOT WIX_DOWNGRADE_DETECTED"
  # Odstranitev ne briše nastavitev: nobena vrstica RemoveFile ali RemoveRegistry ne kaže na minigw v AppData.
  t RemoveFile | grep -qi 'AppData' && fail "$arch: removes AppData"
  x=$T/x-$arch; mkdir -p $x; msiextract -C $x $m >/dev/null
  for f in minigw minigw-tray; do
    got=$(find $x -name $f.exe | head -1); [[ -n $got ]] || fail "$arch: $f.exe not in the MSI"
    cmp -s $got $OUT/${f}-windows-$arch.exe || fail "$arch: $f.exe differs from the build"
  done
  file -b $x/**/minigw.exe(.[1]) | grep -q "$( [[ $arch == amd64 ]] && print x86-64 || print Aarch64)" || fail "$arch: wrong exe architecture"
  print "ok $arch: $(du -h $m | cut -f1) $m"
done
print "msi test ok"
