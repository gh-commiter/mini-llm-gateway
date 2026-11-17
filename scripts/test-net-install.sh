#!/bin/zsh
# Builds, signs and installs "Mini Gateway Test Net.app": the one signed program that tests use for every socket outside 127.0.0.1
# (Sources/MGTestNet, product mg-test-net). The macOS firewall asks the owner for permission per unsigned program that accepts connections; with a
# fixed bundle identifier and a Developer ID signature the permission binds to this app and survives rebuilds. Tests find it through TestNet.locate()
# (MG_TEST_NET, else the install location below).
#   scripts/test-net-install.sh            build (release), sign, verify, install
# Identity: SIGN_IDENTITY or the first "Developer ID Application" in the Keychain, as build.sh. Install location: $MG_SCRATCH/test-tools
# (default /Volumes/SAMSUNG990PRO/Mini Gateway Scratch). Not for CI: the runner has no such firewall and tests use the package's own build there.
emulate -L zsh; set -euo pipefail
cd ${0:A:h}/..

[[ -z ${CI:-} ]] || { print -u2 "test-net-install.sh: refusing to run in CI (the runner has no firewall; tests use the unsigned build of the target there)."; exit 1 }

ID=dev.trojar.minigateway.testnet
SCRATCH=${MG_SCRATCH:-/Volumes/SAMSUNG990PRO/Mini Gateway Scratch}
TOOLS=$SCRATCH/test-tools
APP=$TOOLS/Mini\ Gateway\ Test\ Net.app
STAGE=$SCRATCH/dev-builds/test-net-install

identities=$(security find-identity -v -p codesigning)
pick() { print -r -- "$identities" | grep -oE "\"$1: [^\"]+\"" | head -1 | tr -d '"' || true }
IDENTITY=${SIGN_IDENTITY:-$(pick "Developer ID Application")}
[[ -n $IDENTITY ]] || { print -u2 "No Developer ID Application identity in the Keychain (set SIGN_IDENTITY)."; exit 1 }
print "Identity: $IDENTITY"

# 1. build (release, into Scratch, never into the worktree)
swift build -c release --product mg-test-net --scratch-path "$STAGE/build" 2>&1 | tail -3
BIN=$(swift build -c release --product mg-test-net --scratch-path "$STAGE/build" --show-bin-path)/mg-test-net
[[ -x $BIN ]] || { print -u2 "Built program not found: $BIN"; exit 1 }

# 2. assemble the bundle next to the install location (same volume: the final move is a rename)
mkdir -p "$TOOLS"
NEW=$TOOLS/.Mini\ Gateway\ Test\ Net.new-$$.app
mkdir -p "$NEW/Contents/MacOS"
cp "$BIN" "$NEW/Contents/MacOS/mg-test-net"
cat > "$NEW/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key>
	<string>mg-test-net</string>
	<key>CFBundleIdentifier</key>
	<string>$ID</string>
	<key>CFBundleName</key>
	<string>Mini Gateway Test Net</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>1</string>
	<key>CFBundleVersion</key>
	<string>1</string>
	<key>LSBackgroundOnly</key>
	<true/>
	<key>LSMinimumSystemVersion</key>
	<string>27.0</string>
</dict>
</plist>
PLIST
plutil -lint "$NEW/Contents/Info.plist" >/dev/null

# 3. sign (hardened runtime, fixed identifier) and verify
codesign --force --options runtime --timestamp --identifier "$ID" --sign "$IDENTITY" "$NEW"
codesign --verify --strict --verbose=2 "$NEW"
print "Designated requirement (what the firewall permission binds to):"
codesign -d -r- "$NEW" 2>&1 | sed -n 's/^designated => /  /p'
[[ $(codesign -d --verbose=2 "$NEW" 2>&1 | sed -n 's/^Identifier=//p') == $ID ]] || { print -u2 "Identifier of the signed app is not $ID"; exit 1 }

# 4. install: the new bundle is complete and verified before it replaces the old one (kept aside, nothing is deleted)
if [[ -e $APP ]]; then
  mkdir -p "$TOOLS/.previous"
  OLD=$TOOLS/.previous/Mini\ Gateway\ Test\ Net.$(date +%Y%m%d-%H%M%S).app
  mv "$APP" "$OLD"
  print "Previous version moved to: $OLD"
fi
mv "$NEW" "$APP"
codesign --verify --strict "$APP"
print "Installed: $APP/Contents/MacOS/mg-test-net"
print "Tests find it by themselves; MG_TEST_NET overrides the path."
