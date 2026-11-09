#!/bin/zsh
# Preizkus paketov MSI (scripts/build-msi.sh, client/minigw.wxs): zgradi minigw za Windows, oba MSI, in z msiinfo in
# msiextract preveri datoteke, dele (Client, Gateway, Plugin), okno, vrednosti registra, PATH, bližnjico, zapiranje in
# zagon pladnja, vtičnik, PURGE, vrstni red korakov ter namestitev na uporabnika.
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
  (cd client && CGO_ENABLED=0 GOOS=windows GOARCH=$arch go build -trimpath -ldflags "-s -w" -o $OUT/minigw-tui-windows-$arch.exe ./cmd/minigw-tui)
done
scripts/build-msi.sh $V $OUT

for arch in amd64 arm64; do
  m=$OUT/minigw-windows-$arch.msi
  t() { msiinfo export $m $1 | tail -n +4 | tr -d '\r' }
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
  has Shortcut "ProgramMenuFolder	Mini Gateway	Gateway	[INSTALLDIR]minigw-tray.exe	tray"
  # Deli: Client (vedno, 16 = ni ga mogoče izklopiti), pod njim Gateway in Plugin (privzeto vklopljena)
  has Feature "Client		minigw client"
  t Feature | grep -q "^Client	.*	1	1		16$" || fail "$arch: Client is not always installed (Absent=disallow)"
  has Feature "Gateway	Client	Gateway	"
  has Feature "Plugin	Client	Claude Code plugin	"
  t Feature | grep -E '^(Gateway|Plugin)	' | grep -qv '	1		0$' && fail "$arch: Gateway or Plugin not on by default"
  for fc in "Client	Main" "Gateway	Gateway" "Plugin	Plugin"; do has FeatureComponents $fc; done
  has File "tui	Gateway	minigw-tui.exe"
  has File "tray	Main	minigw-tray.exe"
  has Registry "Software\Microsoft\Windows\CurrentVersion\Run	MiniGatewayRouter	"
  t Registry | grep -F 'MiniGatewayRouter' | grep -q '	Gateway$' || fail "$arch: Run value is not in the Gateway feature"
  # Vtičnik in PURGE: minigw.exe prek minigw-tray.exe msi … (brez okna konzole), odloženo kot uporabnik (1106 = 18 +
  # ignore 64 + deferred 1024, brez NoImpersonate 2048)
  has CustomAction "PluginInstall	1106	tray	msi plugin install"
  has CustomAction "PluginUninstall	1106	tray	msi plugin uninstall"
  has CustomAction "PluginUninstallUpgrading	1106	tray	msi plugin uninstall --upgrading"
  has CustomAction "Purge	1106	tray	msi uninstall --purge --yes"
  t CustomAction | awk -F'\t' '$2 % 64 == 18 && $3 != "tray"' | grep -q . && fail "$arch: a custom action runs a console exe"
  has InstallExecuteSequence 'PluginInstall	&Plugin=3'
  has InstallExecuteSequence 'PluginUninstall	&Plugin=2 AND !Plugin=3 AND NOT UPGRADINGPRODUCTCODE'
  has InstallExecuteSequence 'PluginUninstallUpgrading	&Plugin=2 AND !Plugin=3 AND UPGRADINGPRODUCTCODE'
  has InstallExecuteSequence 'Purge	REMOVE~="ALL" AND PURGE=1 AND NOT UPGRADINGPRODUCTCODE'
  # Vrstni red (wixl z After na dejanja po meri razvrsti napačno): zapri pladenj pred odstranitvijo starega paketa,
  # vtičnik in PURGE, ko je minigw.exe še tu, plugin install, ko je že
  seq() { t InstallExecuteSequence | awk -F'\t' -v a=$1 '$1 == a { print $3 }' }
  lt() { (( $(seq $1) < $(seq $2) )) || fail "$arch: $1 ($(seq $1)) not before $2 ($(seq $2))" }
  lt CostFinalize CloseTray; lt CloseTray InstallValidate; lt InstallValidate RemoveExistingProducts
  lt RemoveExistingProducts InstallInitialize
  for a in PluginUninstall PluginUninstallUpgrading Purge; do lt InstallInitialize $a; lt $a RemoveFiles; done
  lt PluginUninstall Purge; lt InstallFiles PluginInstall; lt PluginInstall InstallFinalize
  lt InstallFinalize StartTray; lt InstallFinalize RestartTray
  # Okno: izbira delov ob namestitvi, ob ponovnem zagonu še Remove s PURGE; nadgradnja brez okna
  for p in GATEWAY_BOX PLUGIN_BOX PURGE; do has CheckBox "$p	1"; done
  has InstallUISequence 'SetupDlg	NOT WIX_UPGRADE_DETECTED AND NOT WIX_SAME_VERSION_UPGRADE_DETECTED'
  has InstallUISequence 'GatewayBoxOn	NOT Installed OR !Gateway=3'
  has InstallUISequence 'PluginBoxOn	NOT Installed OR !Plugin=3'
  has ControlEvent "SetupDlg	Install	Remove	Gateway	NOT GATEWAY_BOX"
  has ControlEvent "SetupDlg	Install	Remove	Plugin	NOT PLUGIN_BOX"
  has ControlEvent "SetupDlg	Remove	Remove	ALL"
  has Dialog "ProgressDlg"
  # Okna, ki jih Windows Installer pokaže sam (brez njih napaka 2803): datoteke v rabi, napake, konec
  for d in FilesInUse MsiRMFilesInUse ErrorDlg FatalError UserExit ExitDialog; do t Dialog | grep -q "^$d	" || fail "$arch: no dialog $d (error 2803)"; done
  has Property "ErrorDialog	ErrorDlg"
  for e in "FatalError		-3" "UserExit		-2" "ExitDialog		-1"; do has InstallUISequence "$e"; done
  has ControlEvent "ExitDialog	Finish	EndDialog	Return"
  # Vsako okno, na katero kaže zaporedje, dogodek ali ErrorDialog, je v tabeli Dialog
  dialogs=" $(t Dialog | cut -f1 | tr '\n' ' ') "
  std=" FindRelatedProducts LaunchConditions ValidateProductID CostInitialize FileCost CostFinalize MigrateFeatureStates ExecuteAction AppSearch CCPSearch RMCCPSearch "
  for d in $(t InstallUISequence | cut -f1) $(t ControlEvent | awk -F'\t' '$3 ~ /^(NewDialog|SpawnDialog|SpawnWaitDialog)$/ { print $4 }') \
           $(t Property | awk -F'\t' '$1 == "ErrorDialog" { print $2 }'); do
    [[ $std == *" $d "* ]] && continue
    t CustomAction | grep -q "^$d	" && continue
    [[ $dialogs == *" $d "* ]] || fail "$arch: $d is referenced but not in the Dialog table (error 2803)"
  done
  # Napaka 2834: kjer imajo kontrolniki Control_Next, mora Control_First ležati v eni sami zanki čez vse take kontrolnike.
  bad=$(awk -F'\t' 'NR==FNR { first[$1]=$8; next } $11 != "" { nx[$1 SUBSEP $2]=$11; n[$1]++ }
    END { for (d in n) { c=first[d]; k=0; while ((d SUBSEP c) in nx && k < n[d]) { c=nx[d SUBSEP c]; k++; if (c == first[d]) break }
          if (c != first[d] || k != n[d]) print d } }' <(t Dialog) <(t Control))
  [[ -z $bad ]] || fail "$arch: tab loop broken or Control_First outside it in: $bad (error 2834)"
  # Pladenj zapre minigw-tray.exe quit (program brez konzole, tip 18 + 64): nobeno okno cmd med (od)namestitvijo.
  has CustomAction "CloseTray	82	tray	quit"
  # Tudi ob nadgradnji (pred InstallValidate, sicer datoteke v rabi), ne kot stari paket pri nadgradnji
  has InstallExecuteSequence 'CloseTray	(REMOVE~="ALL" AND NOT UPGRADINGPRODUCTCODE) OR (&Gateway=2 AND !Gateway=3) OR WIX_UPGRADE_DETECTED OR WIX_SAME_VERSION_UPGRADE_DETECTED'
  t CustomAction | grep -qiE 'cmd\.exe|tasklist|taskkill|powershell|PING' && fail "$arch: a custom action runs a console tool (window flashes)"
  has CustomAction "StartTray	210	tray	tray --quiet-if-running --config"
  has InstallExecuteSequence 'StartTray	&Gateway=3 AND NOT WIX_UPGRADE_DETECTED'
  # Nadgradnja: pladenj znova le, če je tekel
  has CustomAction "RestartTray	210	tray	tray --if-was-running --config"
  has InstallExecuteSequence 'RestartTray	&Gateway=3 AND (WIX_UPGRADE_DETECTED OR WIX_SAME_VERSION_UPGRADE_DETECTED)'
  has LaunchCondition "NOT WIX_DOWNGRADE_DETECTED"
  # Odstranitev ne briše nastavitev: nobena vrstica RemoveFile ali RemoveRegistry ne kaže na minigw v AppData.
  t RemoveFile | grep -qi 'AppData' && fail "$arch: removes AppData"
  x=$T/x-$arch; mkdir -p $x; msiextract -C $x $m >/dev/null
  for f in minigw minigw-tray minigw-tui; do
    got=$(find $x -name $f.exe | head -1); [[ -n $got ]] || fail "$arch: $f.exe not in the MSI"
    cmp -s $got $OUT/${f}-windows-$arch.exe || fail "$arch: $f.exe differs from the build"
  done
  file -b $x/**/minigw.exe(.[1]) | grep -q "$( [[ $arch == amd64 ]] && print x86-64 || print Aarch64)" || fail "$arch: wrong exe architecture"
  print "ok $arch: $(du -h $m | cut -f1) $m"
done
print "msi test ok"
