#!/bin/bash
# Builds a release of charm-bak: the app universal (Apple silicon and
# Intel), signed with a Developer ID, notarized by Apple and stapled, then
# zipped with its SHA-256 beside it in dist/.
#
#   scripts/release.sh             build, notarize, package
#   scripts/release.sh --publish   then create GitHub release v<version>
#                                  on this checkout's origin, with the zip
#
# Needs a "Developer ID Application" signing identity and notarization
# credentials stored once in the Keychain:
#   xcrun notarytool store-credentials notary --apple-id <id> --team-id <team>
# ("notary" is a general profile, not this project's: any app of the same
# team notarizes with it. NOTARY_PROFILE picks another, SIGN_ID another
# identity.)
set -euo pipefail

cd "$(dirname "$0")/.."
PUBLISH=""
[ "${1:-}" = "--publish" ] && PUBLISH=1
PROFILE="${NOTARY_PROFILE:-notary}"
VERSION="$(sed -n 's/^VERSION="\(.*\)"$/\1/p' app/build.sh)"
[ -n "$VERSION" ] || { echo "no VERSION in app/build.sh" >&2; exit 1; }
if [ -n "$(git status --porcelain)" ]; then
    echo "the tree has uncommitted changes: a release is built from a commit" >&2
    exit 1
fi

RELEASE=1 SIGN_ID="${SIGN_ID:-Developer ID Application}" app/build.sh

cd dist
APP=charm-bak.app
ZIP="charm-bak-$VERSION.zip"
rm -f "$ZIP" "$ZIP.sha256"
ditto -c -k --keepParent "$APP" "$ZIP"

echo "notarizing (Apple usually answers within minutes)…"
# notarytool exits non-zero for a rejected submission too: carry on to
# read its status and print Apple's reasons.
OUT="$(xcrun notarytool submit "$ZIP" --keychain-profile "$PROFILE" --wait --output-format json)" || true
STATUS="$(printf '%s' "$OUT" | /usr/bin/python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("status",""))
except Exception: print("")')"
if [ "$STATUS" != "Accepted" ]; then
    ID="$(printf '%s' "$OUT" | /usr/bin/python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("id",""))
except Exception: print("")')"
    echo "notarization: ${STATUS:-no answer}" >&2
    [ -n "$OUT" ] && printf '%s\n' "$OUT" >&2
    [ -n "$ID" ] && xcrun notarytool log "$ID" --keychain-profile "$PROFILE" >&2
    exit 1
fi

# The ticket goes into the app, so it opens without asking Apple first;
# the zip is made again around the stapled app.
xcrun stapler staple "$APP"
rm -f "$ZIP"
ditto -c -k --keepParent "$APP" "$ZIP"
spctl --assess --type execute --verbose "$APP"
shasum -a 256 "$ZIP" > "$ZIP.sha256"
echo "ready: dist/$ZIP ($(du -h "$ZIP" | cut -f1)), dist/$ZIP.sha256"

if [ -n "$PUBLISH" ]; then
    gh release create "v$VERSION" "$ZIP" "$ZIP.sha256" \
        --target "$(git rev-parse HEAD)" \
        --title "charm-bak $VERSION" \
        --notes "Download charm-bak-$VERSION.zip, unzip it, move charm-bak.app to Applications and open it. It's signed with a Developer ID and notarized by Apple; it runs on Apple silicon and Intel Macs with macOS 14 or later. The checksum is in charm-bak-$VERSION.zip.sha256.

charm-bak uses the restic you have installed (Homebrew's, for instance) if it's 0.18 or later, and otherwise downloads restic's own release for itself, checked against restic's signature."
fi
