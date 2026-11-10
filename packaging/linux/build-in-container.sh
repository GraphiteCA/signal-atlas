#!/bin/sh
# Builds Signal Atlas for Windows and Linux inside packaging/linux/Dockerfile's image.
# scripts/release.py runs it; it is not meant to be run by hand.
#
#   /src  the project, mounted read-only     /out  where the release files go
#   VERSION  e.g. 0.1.0     TARGETS  "windows linux" (the default) or one of them
#   OWNER    uid:gid to give the release files (on a Linux PC, so they aren't root's)
set -eu
: "${VERSION:?set VERSION}"
TARGETS=${TARGETS:-windows linux}
NAME=signal-atlas
W=/tmp/work
# a copy of the project without built files, release files or a development run's data
rm -rf "$W" && mkdir -p "$W"
tar -C /src --exclude=./build --exclude=./dist --exclude=./data --exclude=./.git -cf - . | tar -C "$W" -xf -
cd "$W"
LD="-s -w -X main.Version=$VERSION"
# 1.2.3 -> 1,2,3,0 for the Windows version resource (anything after a '-' is left out)
NUM=$(echo "${VERSION%%-*}" | awk -F. '{printf "%d,%d,%d,0", $1, $2, $3}')

want() { case " $TARGETS " in *" $1 "*) return 0;; esac; return 1; }

if want windows; then
echo "== Windows amd64"
cp packaging/icon/icon.ico packaging/windows/signal-atlas.exe.manifest /tmp/
# the icon is resource 3, which is where Wails looks for the window icon
cat > /tmp/res.rc <<RC
3 ICON "/tmp/icon.ico"
1 24 "/tmp/signal-atlas.exe.manifest"
1 VERSIONINFO
FILEVERSION $NUM
PRODUCTVERSION $NUM
FILEOS 0x40004
FILETYPE 0x1
BEGIN
  BLOCK "StringFileInfo"
  BEGIN
    BLOCK "040904B0"
    BEGIN
      VALUE "CompanyName", "Signal Atlas"
      VALUE "FileDescription", "Signal Atlas"
      VALUE "FileVersion", "$VERSION"
      VALUE "InternalName", "$NAME"
      VALUE "OriginalFilename", "$NAME.exe"
      VALUE "ProductName", "Signal Atlas"
      VALUE "ProductVersion", "$VERSION"
    END
  END
  BLOCK "VarFileInfo"
  BEGIN
    VALUE "Translation", 0x409, 1200
  END
END
RC
x86_64-w64-mingw32-windres --preprocessor=cpp -O coff -i /tmp/res.rc -o rsrc_windows_amd64.syso
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -tags desktop,production \
  -ldflags "$LD -H windowsgui" -o "/tmp/win/$NAME.exe" .
rm rsrc_windows_amd64.syso
mkdir -p "/tmp/win/$NAME-$VERSION-windows-amd64"
mv "/tmp/win/$NAME.exe" "/tmp/win/$NAME-$VERSION-windows-amd64/"
cp README.md NOTICE.md "/tmp/win/$NAME-$VERSION-windows-amd64/"
[ -f LICENSE ] && cp LICENSE "/tmp/win/$NAME-$VERSION-windows-amd64/"
(cd /tmp/win && rm -f "/out/$NAME-$VERSION-windows-amd64.zip" && zip -qr "/out/$NAME-$VERSION-windows-amd64.zip" "$NAME-$VERSION-windows-amd64")

fi

if want linux; then
echo "== Linux amd64"
go build -trimpath -tags desktop,production,webkit2_41 -ldflags "$LD" -o /tmp/lin/$NAME .
T="/tmp/lin/$NAME-$VERSION-linux-amd64"
mkdir -p "$T"
cp /tmp/lin/$NAME "$T/"
cp packaging/linux/signal-atlas.desktop "$T/"
cp packaging/icon/icon-256.png "$T/signal-atlas.png"
cp README.md NOTICE.md "$T/"
[ -f LICENSE ] && cp LICENSE "$T/"
tar -C /tmp/lin -czf "/out/$NAME-$VERSION-linux-amd64.tar.gz" "$NAME-$VERSION-linux-amd64"

echo "== Linux .deb"
D=/tmp/deb
rm -rf $D
mkdir -p $D/DEBIAN $D/usr/bin $D/usr/share/applications $D/usr/share/icons/hicolor/256x256/apps $D/usr/share/doc/$NAME
cp /tmp/lin/$NAME $D/usr/bin/
cp packaging/linux/signal-atlas.desktop $D/usr/share/applications/
cp packaging/icon/icon-256.png $D/usr/share/icons/hicolor/256x256/apps/signal-atlas.png
cp README.md NOTICE.md $D/usr/share/doc/$NAME/
[ -f LICENSE ] && cp LICENSE $D/usr/share/doc/$NAME/copyright
SIZE=$(du -sk $D/usr | cut -f1)
cat > $D/DEBIAN/control <<CTL
Package: $NAME
Version: $VERSION
Section: net
Priority: optional
Architecture: amd64
Depends: libgtk-3-0, libwebkit2gtk-4.1-0
Installed-Size: $SIZE
Maintainer: Signal Atlas maintainers <noreply@users.noreply.github.com>
Homepage: ${PROJECT_URL:-https://github.com}
Description: Wardriving analysis for logs too big for the browser
 Wardrive Compare compares WiGLE CSV drives on the road they share. The Signal
 Atlas explorer imports whole surveys and fits where each network's access
 point is. Everything runs on your computer.
CTL
chmod -R u=rwX,go=rX $D && chmod 755 $D/usr/bin/$NAME
dpkg-deb --root-owner-group --build $D "/out/${NAME}_${VERSION}_amd64.deb"

fi

[ -n "${OWNER:-}" ] && chown -R "$OWNER" /out
echo "== done"
