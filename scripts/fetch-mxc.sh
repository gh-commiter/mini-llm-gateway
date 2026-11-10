#!/bin/zsh
# #68: wxc-exec.exe (Microsoft MXC) za MSI: izdaja po točni oznaki, zip in oba programa preverjena s scripts/mxc.sha256
# (iste vrednosti so v client/mxc.go, wxcHashes). Zip (~540 MB) ostane v predpomnilniku gradnje.
#   fetch-mxc.sh <dir>   →  <dir>/wxc-exec-windows-amd64.exe, <dir>/wxc-exec-windows-arm64.exe
emulate -L zsh; set -eu
D=${1:?usage: fetch-mxc.sh <dir>}
cd ${0:A:h}/..
TAG=v1.0.1
SUMS=scripts/mxc.sha256
CACHE=${XDG_CACHE_HOME:-$HOME/Library/Caches}/minigw-build
ZIP=$CACHE/mxc-$TAG-release-binaries.zip
mkdir -p $CACHE $D
sum() { shasum -a 256 $1 | cut -d' ' -f1 }
want() { awk -v f=$1 '$2 == f { print $1 }' $SUMS }
if [[ ! -f $ZIP || "$(sum $ZIP)" != "$(want mxc-release-binaries.zip)" ]]; then
  curl -fL --retry 3 -o $ZIP.part https://github.com/microsoft/mxc/releases/download/$TAG/mxc-release-binaries.zip
  [[ "$(sum $ZIP.part)" == "$(want mxc-release-binaries.zip)" ]] || { rm -f $ZIP.part; print -u2 "mxc zip: SHA-256 is not the pinned one ($SUMS)"; exit 1 }
  mv $ZIP.part $ZIP
fi
T=$(mktemp -d); trap 'rm -rf $T' EXIT
for pair in x64:amd64 arm64:arm64; do
  src=${pair%:*}; arch=${pair#*:}
  unzip -q -o -d $T $ZIP $src/wxc-exec.exe
  [[ "$(sum $T/$src/wxc-exec.exe)" == "$(want $src/wxc-exec.exe)" ]] || { print -u2 "$src/wxc-exec.exe: SHA-256 is not the pinned one ($SUMS)"; exit 1 }
  cp $T/$src/wxc-exec.exe $D/wxc-exec-windows-$arch.exe
done
