#!/bin/zsh
# Zgradi z xcodebuild, sestavi "Mini Gateway.app" in ga podpiše.
#   ./build.sh            razvojna gradnja (podpis: Developer ID, sicer Apple Development)
#   ./build.sh --test     najprej testi
#   ./build.sh --release  Developer ID + notarizacija + stapler → build/release/Mini-Gateway-<različica>.{dmg,zip},
#                         minigw.zip + install.sh (odjemalec)
# Identiteta: SIGN_IDENTITY ali prva ustrezna v Keychainu. Notarizacija: NOTARY_PROFILE (privzeto minigateway-notary),
# ustvari ga: xcrun notarytool store-credentials minigateway-notary --apple-id <apple-id> --team-id <team-id>
set -euo pipefail
cd "${0:A:h}"

MODE=${1-}
DERIVED=.build/xcode
identities=$(security find-identity -v -p codesigning)
pick() { print -r -- "$identities" | grep -oE "\"$1: [^\"]+\"" | head -1 | tr -d '"' || true }
if [[ $MODE == --release ]]; then
  # Izdaja le iz commitane kode (tudi brez nesledenih datotek).
  [[ -z $(git status --porcelain -- Sources client Resources Package.swift) ]] \
    || { print -u2 "Release zahteva commitano kodo (git status -- Sources client Resources Package.swift ni prazen)."; exit 1 }
  IDENTITY=${SIGN_IDENTITY:-$(pick "Developer ID Application")}
  [[ $IDENTITY == "Developer ID Application:"* ]] || { print -u2 "Release zahteva potrdilo Developer ID Application (Xcode → Settings → Accounts → Manage Certificates → +)."; exit 1 }
else
  IDENTITY=${SIGN_IDENTITY:-$(pick "Developer ID Application")}
  [[ -n $IDENTITY ]] || IDENTITY=$(pick "Apple Development")
fi
[[ -n $IDENTITY ]] || { print -u2 "Ni identitete za podpisovanje (nastavi SIGN_IDENTITY)."; exit 1 }
print "Podpis: $IDENTITY"

if [[ $MODE == --test ]]; then
  xcodebuild test -scheme MiniGateway-Package -destination 'platform=macOS,arch=arm64' -derivedDataPath "$DERIVED" -quiet
fi

# Xcode 27 paketom privzeto vklopi pokritost kode → binarne datoteke pišejo default.profraw v mapo uporabnika.
for scheme in MiniGateway MiniGW; do
  xcodebuild build -scheme $scheme -configuration Release -destination 'platform=macOS,arch=arm64' -derivedDataPath "$DERIVED" ENABLE_CODE_COVERAGE=NO -quiet
done

APP="build/Mini Gateway.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS"
cp "$DERIVED/Build/Products/Release/MiniGateway" "$APP/Contents/MacOS/MiniGateway"
cp Resources/Info.plist "$APP/Contents/Info.plist"
# Commit, iz katerega je zgrajeno (v meniju in About); -dirty, če koda ni commitana.
commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
[[ -z $(git status --porcelain -- Sources client Resources Package.swift 2>/dev/null) ]] || commit+=-dirty  # tudi nesledene
plutil -replace MGCommit -string "$commit" "$APP/Contents/Info.plist"
mkdir -p "$APP/Contents/Resources/en.lproj"  # izvorni jezik (fallback); prevodi iz katalogov spodaj
cp Resources/AppIcon.icns "$APP/Contents/Resources/AppIcon.icns"
for catalog in Resources/*.xcstrings; do
  xcrun xcstringstool compile "$catalog" --output-directory "$APP/Contents/Resources"
done
# Ukaz minigw: podpisan posebej (lasten identifikator → lasten vnos v Keychainu), nato aplikacija.
mkdir -p "$APP/Contents/Helpers"
cp "$DERIVED/Build/Products/Release/MiniGW" "$APP/Contents/Helpers/minigw"
codesign --force --options runtime --timestamp --identifier dev.trojar.minigateway.cli --sign "$IDENTITY" "$APP/Contents/Helpers/minigw"
codesign --force --options runtime --timestamp --sign "$IDENTITY" "$APP"
codesign --verify --strict "$APP"

if [[ $MODE == --release ]]; then
  VERSION=$(plutil -extract CFBundleShortVersionString raw Resources/Info.plist)
  OUT=build/release
  mkdir -p $OUT
  ZIP="$OUT/Mini-Gateway-$VERSION.zip"
  ditto -c -k --keepParent "$APP" "$ZIP"
  xcrun notarytool submit "$ZIP" --keychain-profile "${NOTARY_PROFILE:-minigateway-notary}" --wait
  xcrun stapler staple "$APP"                  # ticket v aplikacijo: Gatekeeper ga preveri tudi brez omrežja
  spctl --assess --type execute --verbose "$APP"
  rm "$ZIP" && ditto -c -k --keepParent "$APP" "$ZIP"

  # DMG: aplikacija + bližnjica do Applications (povleci za namestitev); podpisan, notariziran, staplan.
  DMG="$OUT/Mini-Gateway-$VERSION.dmg"
  STAGE=$(mktemp -d)
  ditto "$APP" "$STAGE/Mini Gateway.app"
  ln -s /Applications "$STAGE/Applications"
  rm -f "$DMG"
  hdiutil create -volname "Mini Gateway $VERSION" -srcfolder "$STAGE" -fs HFS+ -format UDZO -ov "$DMG" -quiet
  rm -rf "$STAGE"
  codesign --force --timestamp --sign "$IDENTITY" "$DMG"
  xcrun notarytool submit "$DMG" --keychain-profile "${NOTARY_PROFILE:-minigateway-notary}" --wait
  xcrun stapler staple "$DMG"
  spctl --assess --type open --context context:primary-signature --verbose "$DMG"

  # Odjemalec (drug Mac): samo ukaz minigw + install.sh; imeni brez različice za releases/latest/download/…
  CLI_ZIP="$OUT/minigw.zip"
  rm -f "$CLI_ZIP"
  ditto -c -k "$APP/Contents/Helpers/minigw" "$CLI_ZIP"
  xcrun notarytool submit "$CLI_ZIP" --keychain-profile "${NOTARY_PROFILE:-minigateway-notary}" --wait
  cp install.sh "$OUT/install.sh"

  # Odjemalec za Windows in Linux (client/, Go, le standardna knjižnica): ena izvršljiva datoteka na sistem.
  GO_BINS=()
  for t in windows/amd64 windows/arm64 linux/amd64 linux/arm64; do
    os=${t%/*}; arch=${t#*/}; name=minigw-$os-$arch; [[ $os == windows ]] && name+=.exe || true
    (cd client && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "../$OUT/$name" .)
    GO_BINS+=$name
  done
  # Pladenj za Windows: isti program brez konzole (minigw-tray*.exe zažene router z ikono v območju za obvestila).
  for arch in amd64 arm64; do
    name=minigw-tray-windows-$arch.exe
    (cd client && CGO_ENABLED=0 GOOS=windows GOARCH=$arch go build -trimpath -ldflags "-s -w -H=windowsgui" -o "../$OUT/$name" .)
    GO_BINS+=$name
  done

  (cd $OUT && shasum -a 256 "${ZIP:t}" "${DMG:t}" minigw.zip install.sh $GO_BINS | tee SHA256SUMS)
  # Homebrew (Casks/, tap = ta repozitorij): različica in kontrolni vsoti te izdaje
  for cask file in Casks/mini-gateway.rb "$ZIP" Casks/minigw.rb "$CLI_ZIP"; do
    sed -i '' -e "s/^  version \".*\"/  version \"$VERSION\"/" \
              -e "s/^  sha256 \".*\"/  sha256 \"$(shasum -a 256 "$file" | cut -d' ' -f1)\"/" "$cask"
  done
  print "$ZIP"
  print "$DMG"
  print "$CLI_ZIP"
else
  print "$APP"
fi
