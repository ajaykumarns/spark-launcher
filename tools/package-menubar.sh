#!/bin/sh
# Build a reproducible local Sparky.app bundle with a bundled standalone CLI.
#
# Usage: sh tools/package-menubar.sh [OUT_APP] [--cli PATH]
#   OUT_APP defaults to dist/Sparky.app.
#   --cli PATH uses an existing CLI binary instead of dist/sparky.
#
# This is a local packaging step only. It does not install anything, does not
# touch login items or preferences, and does not claim notarization.
set -eu

OUT=dist/Sparky.app
CLI=""
while [ $# -gt 0 ]; do
	case "$1" in
	--cli)
		[ $# -ge 2 ] || { echo "--cli requires a PATH" >&2; exit 2; }
		CLI=$2
		shift 2
		;;
	*)
		OUT=$1
		shift
		;;
	esac
done

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
APP_BIN="$ROOT/dist/sparky-menubar"
if [ ! -x "$APP_BIN" ]; then
	echo "missing native app binary: run 'make menubar' first" >&2
	exit 1
fi

if [ -z "$CLI" ]; then
	CLI="$ROOT/dist/sparky"
	if [ ! -x "$CLI" ]; then
		echo "missing CLI: run 'make build' or pass --cli PATH" >&2
		exit 1
	fi
fi

case "$CLI" in
/*) ;;
*) echo "--cli must be an absolute path" >&2; exit 2 ;;
esac
[ -x "$CLI" ] || { echo "CLI is not executable: $CLI" >&2; exit 1; }

ICON_PNG="$ROOT/cmd/sparky-menubar/icon/tray.png"
[ -f "$ICON_PNG" ] || { echo "missing icon: $ICON_PNG" >&2; exit 1; }

# Assemble into a fresh task-owned staging bundle so no stale signature or
# resource from a previous output is ever reused. Replace the destination only
# after the staged bundle is complete and verified.
STAGE_ROOT=$(mktemp -d)
STAGE="$STAGE_ROOT/Sparky.app"
trap 'rm -rf "$STAGE_ROOT"' EXIT INT TERM

CONTENTS="$STAGE/Contents"
MACOS="$CONTENTS/MacOS"
RES="$CONTENTS/Resources"

mkdir -p "$MACOS" "$RES"

# Info.plist: LSUIElement keeps the app out of the Dock; no installer logic.
cat >"$CONTENTS/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key>
	<string>Sparky</string>
	<key>CFBundleDisplayName</key>
	<string>Sparky</string>
	<key>CFBundleIdentifier</key>
	<string>local.sparky.menubar</string>
	<key>CFBundleExecutable</key>
	<string>sparky-menubar</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>0.1.0</string>
	<key>CFBundleVersion</key>
	<string>1</string>
	<key>CFBundleIconFile</key>
	<string>AppIcon</string>
	<key>LSUIElement</key>
	<true/>
	<key>NSHighResolutionCapable</key>
	<true/>
</dict>
</plist>
PLIST

cp "$APP_BIN" "$MACOS/sparky-menubar"
cp "$CLI" "$RES/sparky"
chmod 755 "$MACOS/sparky-menubar" "$RES/sparky"

# App icon: convert the template PNG into an .icns when iconutil exists.
if command -v iconutil >/dev/null 2>&1 && command -v sips >/dev/null 2>&1; then
	ICONSET_ROOT=$(mktemp -d)
	trap 'rm -rf "$ICONSET_ROOT"' EXIT INT TERM
	ICONSET="$ICONSET_ROOT/AppIcon.iconset"
	mkdir -p "$ICONSET"
	for size in 16 32 128 256 512; do
		sips -z $size $size "$ICON_PNG" --out "$ICONSET/icon_${size}x${size}.png" >/dev/null
		double=$((size * 2))
		sips -z $double $double "$ICON_PNG" --out "$ICONSET/icon_${size}x${size}@2x.png" >/dev/null
	done
	iconutil -c icns "$ICONSET" -o "$RES/AppIcon.icns"
else
	echo "iconutil/sips unavailable; bundle has no .icns" >&2
	cp "$ICON_PNG" "$RES/AppIcon.png"
fi

# A tiny, stable manifest so packaging is easy to diff (before any signing).
{
	echo "Sparky.app 0.1.0"
	echo "cli=$(shasum -a 256 "$RES/sparky" | awk '{print $1}')"
	echo "app=$(shasum -a 256 "$MACOS/sparky-menubar" | awk '{print $1}')"
} >"$RES/manifest.txt"

if command -v plutil >/dev/null 2>&1; then
	plutil -lint "$CONTENTS/Info.plist" >/dev/null || {
		echo "Info.plist failed plutil -lint" >&2
		exit 1
	}
fi

# Local ad-hoc signing, not notarization. Sign the nested bundled CLI first,
# then the native executable, then the bundle itself. No --deep: signing order
# is explicit so every Mach-O has its own signature before the bundle seal.
# Failures are fatal rather than silently producing an unverifiable bundle.
if command -v codesign >/dev/null 2>&1; then
	codesign --force --sign - --timestamp=none "$RES/sparky"
	codesign --force --sign - --timestamp=none "$MACOS/sparky-menubar"
	codesign --force --sign - --timestamp=none "$STAGE"
	codesign --verify --deep --strict "$STAGE" || {
		echo "codesign --verify failed for staged bundle" >&2
		exit 1
	}
else
	echo "codesign unavailable; bundle left unsigned (not notarized)" >&2
fi

# Replace the previous output only after the staged bundle is built and, on
# macOS with codesign, verified. This discards any stale signature in the
# existing output instead of signing over it in place.
mkdir -p "$(dirname -- "$OUT")"
rm -rf "$OUT"
# ditto preserves signatures and Mach-O metadata; plain cp -R can add Finder
# detritus that makes codesign --verify fail on the copied bundle.
ditto "$STAGE" "$OUT"

if command -v codesign >/dev/null 2>&1; then
	# FinderInfo / File Provider detritus can be re-attached by the OS when the
	# bundle lives under a cloud-managed folder; it is metadata, not part of the
	# signature, and strict verification rejects it. Signatures live in
	# _CodeSignature and Mach-O load commands, so removing only these two
	# attributes does not weaken or fake the signature check below.
	# Retry once: a sync (or another process) can re-attach detritus right after
	# the first strip, which is exactly how the strict verify below can fail.
	strip_detritus() {
		if command -v xattr >/dev/null 2>&1; then
			find "$OUT" -exec xattr -d com.apple.FinderInfo {} \; 2>/dev/null || true
			find "$OUT" -exec xattr -d 'com.apple.fileprovider.fpfs#P' {} \; 2>/dev/null || true
		fi
	}
	strip_detritus
	if ! codesign --verify --deep --strict "$OUT" 2>/dev/null; then
		strip_detritus
	fi
	codesign --verify --deep --strict "$OUT" || {
		echo "codesign --verify failed for $OUT" >&2
		exit 1
	}
fi

echo "packaged $OUT (local ad-hoc signed; not notarized)"
