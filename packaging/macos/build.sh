#!/bin/sh
# Builds Signal Atlas.app for macOS (one universal app for Apple silicon and Intel Macs) and zips it.
# It must run on a Mac with Go and the Xcode command line tools (xcode-select --install).
# scripts/release.py runs it there; by hand:
#
#   VERSION=0.1.0 OUT=dist/0.1.0 sh packaging/macos/build.sh
set -eu
: "${VERSION:?set VERSION}"
: "${OUT:?set OUT}"
cd "$(dirname "$0")/../.."
NAME=signal-atlas
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
LD="-s -w -X main.Version=$VERSION"
# the flags Wails' own build uses on macOS; Go 1.25 and later need macOS 12
export CGO_ENABLED=1 CGO_CFLAGS="-mmacosx-version-min=12.0" CGO_LDFLAGS="-framework UniformTypeIdentifiers -mmacosx-version-min=12.0"
for arch in arm64 amd64; do
  echo "== macOS $arch"
  GOOS=darwin GOARCH=$arch go build -trimpath -tags desktop,production -ldflags "$LD" -o "$TMP/$NAME-$arch" .
done
APP="$TMP/Signal Atlas.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
lipo -create -output "$APP/Contents/MacOS/$NAME" "$TMP/$NAME-arm64" "$TMP/$NAME-amd64"
cp packaging/icon/icon.icns "$APP/Contents/Resources/icon.icns"
cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Signal Atlas</string>
  <key>CFBundleDisplayName</key><string>Signal Atlas</string>
  <key>CFBundleExecutable</key><string>$NAME</string>
  <key>CFBundleIdentifier</key><string>app.signalatlas.desktop</string>
  <key>CFBundleIconFile</key><string>icon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$VERSION</string>
  <key>CFBundleVersion</key><string>$VERSION</string>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
  <key>LSApplicationCategoryType</key><string>public.app-category.utilities</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSLocationWhenInUseUsageDescription</key><string>Signal Atlas opens its map where you are.</string>
  <key>NSRequiresAquaSystemAppearance</key><false/>
</dict>
</plist>
PLIST
# Signing: with SIGNAL_ATLAS_MAC_IDENTITY ("Developer ID Application: Name (TEAMID)") the app is
# signed for distribution (hardened runtime, timestamp); without it, ad hoc, which is enough to run
# on Apple silicon but not to pass Gatekeeper.
if [ -n "${SIGNAL_ATLAS_MAC_IDENTITY:-}" ]; then
  codesign --force --deep --options runtime --timestamp --sign "$SIGNAL_ATLAS_MAC_IDENTITY" "$APP"
else
  codesign --force --deep --sign - "$APP"
fi
mkdir -p "$OUT"
rm -f "$OUT/$NAME-$VERSION-macos-universal.zip"
ditto -c -k --keepParent "$APP" "$OUT/$NAME-$VERSION-macos-universal.zip"
# Notarizing: with SIGNAL_ATLAS_MAC_NOTARY_PROFILE (made once with `xcrun notarytool
# store-credentials`) Apple checks the signed app, the ticket is stapled to it, and it is zipped again.
if [ -n "${SIGNAL_ATLAS_MAC_IDENTITY:-}" ] && [ -n "${SIGNAL_ATLAS_MAC_NOTARY_PROFILE:-}" ]; then
  xcrun notarytool submit "$OUT/$NAME-$VERSION-macos-universal.zip" --keychain-profile "$SIGNAL_ATLAS_MAC_NOTARY_PROFILE" --wait
  xcrun stapler staple "$APP"
  rm -f "$OUT/$NAME-$VERSION-macos-universal.zip"
  ditto -c -k --keepParent "$APP" "$OUT/$NAME-$VERSION-macos-universal.zip"
fi
echo "== done: $OUT/$NAME-$VERSION-macos-universal.zip"
