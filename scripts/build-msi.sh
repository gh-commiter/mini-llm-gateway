#!/bin/zsh
# MSI za Windows iz že zgrajenih minigw-windows-<arch>.exe, minigw-tray-windows-<arch>.exe in minigw-tui-windows-<arch>.exe
# v <dir> (wxc-exec-windows-<arch>.exe prenese scripts/fetch-mxc.sh, če ga ni):
#   build-msi.sh <version> <dir>   →  <dir>/minigw-windows-amd64.msi, <dir>/minigw-windows-arm64.msi
# wixl (brew install msitools) nima predloge arm64: paket arm64 ima predlogo x64 (Windows 11 na ARM ga namesti),
# datoteke v njem so arm64. --ext ui: brez nje wixl tabel oken (Dialog, Control, CheckBox) ne ustvari; okna so v
# client/minigw.wxs.
emulate -L zsh; set -eu
V=${1:?usage: build-msi.sh <version> <dir>}; D=${2:?usage: build-msi.sh <version> <dir>}
command -v wixl >/dev/null || { print -u2 "MSI needs wixl: brew install msitools"; exit 1 }
cd ${0:A:h}/..
# #68: wxc-exec.exe (MXC) za del Gateway, preverjen s scripts/mxc.sha256
[[ -f $D/wxc-exec-windows-amd64.exe && -f $D/wxc-exec-windows-arm64.exe ]] || scripts/fetch-mxc.sh $D
# L4: tudi že prisotna programa vedno preverjena (zamenjan program v $D ne gre v MSI)
for pair in amd64:x64 arm64:arm64; do
  want=$(grep " ${pair#*:}/wxc-exec.exe\$" scripts/mxc.sha256 | cut -d' ' -f1)
  got=$(shasum -a 256 $D/wxc-exec-windows-${pair%%:*}.exe | cut -d' ' -f1)
  [[ -n $want && $got == $want ]] || { echo "wxc-exec-windows-${pair%%:*}.exe does not match scripts/mxc.sha256" >&2; exit 1 }
done
for arch in amd64 arm64; do
  wixl --ext ui -a x64 -D Version=$V -D Icon=client/tray.ico \
    -D Exe=$D/minigw-windows-$arch.exe -D Tray=$D/minigw-tray-windows-$arch.exe -D Tui=$D/minigw-tui-windows-$arch.exe \
    -D Wxc=$D/wxc-exec-windows-$arch.exe -D WxcLicense=scripts/mxc-LICENSE.txt \
    -o $D/minigw-windows-$arch.msi client/minigw.wxs
  # Zapiranje pred InstallValidate teče iz kopije pladnja tega paketa (Binary stopper, tip 66 = 2 + ignore 64), ne iz
  # nameščenega starega: wixl BinaryKey z ExeCommand ne zna, zato tu (client/minigw.wxs, CloseTray).
  msibuild $D/minigw-windows-$arch.msi -q "UPDATE \`CustomAction\` SET \`Type\` = 66, \`Source\` = 'stopper' WHERE \`Action\` = 'CloseTray' OR \`Action\` = 'StopProcesses' OR \`Action\` = 'StopTui'"
done
