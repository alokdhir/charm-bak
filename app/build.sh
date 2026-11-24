#!/bin/bash
# Builds charm-bak.app (the Swift menu bar shell with the Go daemon inside
# it), signs it with a fixed identity and installs it to ~/Applications. Same bundle ID + same signing identity on every build is
# what lets macOS keep the app's Full Disk Access across rebuilds (TREK-11).
#
#   app/build.sh            build, sign, install, relaunch if it was running
#   SIGN_ID="Developer ID Application: …" app/build.sh   sign with another identity
#   SIGN_ID=- app/build.sh  ad-hoc signature (for comparison only)
set -euo pipefail

cd "$(dirname "$0")"
BUNDLE_ID="${BUNDLE_ID:-io.github.alokdhir.charm-bak}"
SIGN_ID="${SIGN_ID:-Apple Development}"
DEST="${DEST:-$HOME/Applications/charm-bak.app}"
BUILD="$(date +%Y%m%d.%H%M%S)"
VERSION="0.3.0"

swift build -c release --quiet
BIN="$(swift build -c release --show-bin-path)/CharmBak"
# Everything installed is stripped (Alok, 2026-11-22): Go builds drop the
# symbol table and DWARF (-s -w; panics still name functions, from
# pclntab), the Swift app its local symbols. Debugging uses the dev tree.
# -buildvcs=false: no git revision stamped in, so a binary changes only
# when its code does; otherwise every commit would push a "new" daemon to
# every remote (self_update compares hashes).
GOBUILD=(go build -trimpath -buildvcs=false -ldflags "-s -w")

STAGE="$(mktemp -d)/charm-bak.app"
mkdir -p "$STAGE/Contents/MacOS" "$STAGE/Contents/Resources"
cp "$BIN" "$STAGE/Contents/MacOS/charm-bak"
strip -x "$STAGE/Contents/MacOS/charm-bak"
# The daemon, built for this Mac, lives beside the app's executable; the
# app starts it as a child so it inherits the app's Full Disk Access.
(cd .. && CGO_ENABLED=0 "${GOBUILD[@]}" -o "$STAGE/Contents/MacOS/charm-bak-daemon" ./cmd/charm-bak-daemon)
# The TUI too, so the menu's "Open TUI" always runs the matching version.
(cd .. && CGO_ENABLED=0 "${GOBUILD[@]}" -o "$STAGE/Contents/MacOS/charm-bak-tui" .)
# Linux daemons, which pairing hands to remotes (Contents/Resources is
# data to codesign, not code it checks).
for arch in amd64 arm64; do
    (cd .. && CGO_ENABLED=0 GOOS=linux GOARCH=$arch "${GOBUILD[@]}" -o "$STAGE/Contents/Resources/linux-$arch/charm-bak-daemon" ./cmd/charm-bak-daemon)
done
ICONSET="$(dirname "$STAGE")/AppIcon.iconset"
"$BIN" --render-icon "$ICONSET"
iconutil -c icns "$ICONSET" -o "$STAGE/Contents/Resources/AppIcon.icns"
# macOS daemons for Mac remotes, each in a small bundle of its own
# (Contents/Resources/darwin-<arch>/charm-bak.app): Full Disk Access is
# granted to an app, and the remote's launchd starts the executable
# inside it. It has the app's icon (Login Items shows it) and no version;
# remotes take the whole bundle when it changes (self_update).
for arch in amd64 arm64; do
    RB="$STAGE/Contents/Resources/darwin-$arch/charm-bak.app"
    mkdir -p "$RB/Contents/MacOS" "$RB/Contents/Resources"
    cp "$STAGE/Contents/Resources/AppIcon.icns" "$RB/Contents/Resources/"
    (cd .. && CGO_ENABLED=0 GOOS=darwin GOARCH=$arch "${GOBUILD[@]}" -o "$RB/Contents/MacOS/charm-bak-daemon" ./cmd/charm-bak-daemon)
    cat > "$RB/Contents/Info.plist" <<RPLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key><string>$BUNDLE_ID.remote</string>
  <key>CFBundleName</key><string>charm-bak</string>
  <key>CFBundleDisplayName</key><string>charm-bak</string>
  <key>CFBundleExecutable</key><string>charm-bak-daemon</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>LSMinimumSystemVersion</key><string>13.0</string>
  <key>LSBackgroundOnly</key><true/>
</dict>
</plist>
RPLIST
    codesign --force --options runtime --timestamp=none --sign "$SIGN_ID" --identifier "$BUNDLE_ID.remote" "$RB"
done
cat > "$STAGE/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key><string>$BUNDLE_ID</string>
  <key>CFBundleName</key><string>charm-bak</string>
  <key>CFBundleDisplayName</key><string>charm-bak</string>
  <key>CFBundleExecutable</key><string>charm-bak</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$VERSION</string>
  <key>CFBundleVersion</key><string>$BUILD</string>
  <key>LSMinimumSystemVersion</key><string>14.0</string>
  <key>LSUIElement</key><true/>
</dict>
</plist>
PLIST

# Nested code first, then the bundle (codesign --deep is discouraged).
codesign --force --options runtime --timestamp=none --sign "$SIGN_ID" --identifier "$BUNDLE_ID.daemon" "$STAGE/Contents/MacOS/charm-bak-daemon"
codesign --force --options runtime --timestamp=none --sign "$SIGN_ID" --identifier "$BUNDLE_ID.tui" "$STAGE/Contents/MacOS/charm-bak-tui"
codesign --force --options runtime --timestamp=none --sign "$SIGN_ID" --identifier "$BUNDLE_ID" "$STAGE"

# Match the app by its bundle path: the TUI on PATH is also "charm-bak".
APP_EXE='charm-bak\.app/Contents/MacOS/charm-bak$'
running=0
if pgrep -qf "$APP_EXE"; then
    running=1
    osascript -e 'quit app id "'"$BUNDLE_ID"'"' 2>/dev/null || pkill -f "$APP_EXE" || true
    # Wait until it's really gone: quitting waits up to 10s for the daemon
    # to stop a running backup, and `open` on a bundle whose old instance
    # is still exiting would only activate that one, leaving nothing running.
    for _ in $(seq 1 75); do
        pgrep -qf "$APP_EXE" || break
        sleep 0.2
    done
    if pgrep -qf "$APP_EXE"; then
        pkill -9 -f "$APP_EXE" || true
        sleep 0.5
    fi
fi
pkill -x charm-bak-daemon 2>/dev/null || true # an orphan from a crashed app
mkdir -p "$HOME/Applications"
rm -rf "$DEST"
mv "$STAGE" "$DEST"
echo "installed $DEST ($VERSION build $BUILD, signed: $SIGN_ID)"

# charm-bak on PATH: a link to the bundled TUI, so rebuilds update it and
# it's always the signed binary. Never replaces anything that isn't ours.
LINK="${LINK:-/usr/local/bin/charm-bak}"
if [ -L "$LINK" ] && [[ "$(readlink "$LINK")" == *charm-bak.app/Contents/MacOS/charm-bak-tui ]]; then
    ln -sfn "$DEST/Contents/MacOS/charm-bak-tui" "$LINK" ||
        echo "warning: couldn't update $LINK; left alone" >&2
elif [ ! -e "$LINK" ] && [ ! -L "$LINK" ] && [ -w "$(dirname "$LINK")" ]; then
    ln -s "$DEST/Contents/MacOS/charm-bak-tui" "$LINK"
    echo "linked $LINK"
else
    echo "warning: $LINK exists and isn't charm-bak's link, or its folder isn't writable; left alone" >&2
fi
if [ "$running" = 1 ]; then
    open "$DEST"
    # Say so if it didn't come back: without the app, nothing backs up.
    for _ in $(seq 1 25); do
        pgrep -qf "$APP_EXE" && break
        sleep 0.2
    done
    pgrep -qf "$APP_EXE" || echo "warning: $DEST didn't start; open it by hand" >&2
fi
