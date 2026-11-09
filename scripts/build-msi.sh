#!/bin/zsh
# MSI za Windows iz že zgrajenih minigw-windows-<arch>.exe, minigw-tray-windows-<arch>.exe in minigw-tui-windows-<arch>.exe
# v <dir>:
#   build-msi.sh <version> <dir>   →  <dir>/minigw-windows-amd64.msi, <dir>/minigw-windows-arm64.msi
# wixl (brew install msitools) nima predloge arm64: paket arm64 ima predlogo x64 (Windows 11 na ARM ga namesti),
# datoteke v njem so arm64. --ext ui: brez nje wixl tabel oken (Dialog, Control, CheckBox) ne ustvari; okna so v
# client/minigw.wxs.
emulate -L zsh; set -eu
V=${1:?usage: build-msi.sh <version> <dir>}; D=${2:?usage: build-msi.sh <version> <dir>}
command -v wixl >/dev/null || { print -u2 "MSI needs wixl: brew install msitools"; exit 1 }
cd ${0:A:h}/..
for arch in amd64 arm64; do
  wixl --ext ui -a x64 -D Version=$V -D Icon=client/tray.ico \
    -D Exe=$D/minigw-windows-$arch.exe -D Tray=$D/minigw-tray-windows-$arch.exe -D Tui=$D/minigw-tui-windows-$arch.exe \
    -o $D/minigw-windows-$arch.msi client/minigw.wxs
done
