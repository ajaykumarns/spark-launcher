#!/bin/sh
# Build the published Sparky archives for one platform.
#
# Usage: sh tools/release-bundle.sh --platform darwin/arm64 --version 0.6.3 \
#          --commit <40-hex> [--dist dist] [--app dist/Sparky.app]
#
# Run it through make, which builds the binaries first:
#
#   make release-linux VERSION=0.6.3 COMMIT=<40-hex>   # any Go host
#   make release-macos VERSION=0.6.3 COMMIT=<40-hex>   # native macOS arm64
#
# Outputs, all under <dist>/release:
#
#   sparky-<version>-<os>-<arch>.tar.gz   bin/sparky, bin/sparky-install,
#                                         LICENSE, BUILDINFO.json
#   Sparky-<version>-<os>-<arch>.app.zip  Sparky.app (macOS only, ad-hoc signed)
#   SHA256SUMS.<os>-<arch>                checksums of the files just written
#
# The archive layout is a public contract: extraction always yields
# <top-dir>/bin/sparky and <top-dir>/bin/sparky-install, so the documented
# "extract, then run sparky-install --source bin/sparky" never has to guess.
#
# Every check is fatal. Nothing here uploads, deploys, installs, restarts a
# Spark, or touches a model: it only reads dist/ and writes dist/release.
set -eu

VERSION=""
COMMIT=""
PLATFORM=""
DIST="dist"
APP=""
OUTDIR=""

die() {
	echo "release-bundle: $*" >&2
	exit 1
}

usage() {
	echo "usage: sh tools/release-bundle.sh --platform darwin/arm64 --version X.Y.Z --commit <40-hex> [--dist DIR] [--app PATH]" >&2
	exit 2
}

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

while [ $# -gt 0 ]; do
	case "$1" in
	--platform)
		[ $# -ge 2 ] || usage
		PLATFORM=$2
		shift 2
		;;
	--version)
		[ $# -ge 2 ] || usage
		VERSION=$2
		shift 2
		;;
	--commit)
		[ $# -ge 2 ] || usage
		COMMIT=$2
		shift 2
		;;
	--dist)
		[ $# -ge 2 ] || usage
		DIST=$2
		shift 2
		;;
	--app)
		[ $# -ge 2 ] || usage
		APP=$2
		shift 2
		;;
	--out)
		[ $# -ge 2 ] || usage
		OUTDIR=$2
		shift 2
		;;
	*) usage ;;
	esac
done

# --- inputs are validated before a byte is written --------------------------
[ -n "$PLATFORM" ] || usage
[ -n "$VERSION" ] || usage
[ -n "$COMMIT" ] || usage
case "$DIST" in
'' | / | . | ..) die "refusing to use --dist '$DIST'" ;;
esac

OS=${PLATFORM%/*}
ARCH=${PLATFORM##*/}
case "$OS/$ARCH" in
darwin/arm64) RELEASE_TARGET="release-macos" ;;
linux/arm64) RELEASE_TARGET="release-linux" ;;
*) die "unsupported platform '$PLATFORM' (want darwin/arm64 or linux/arm64)" ;;
esac

# Plain numeric X.Y.Z only: no 'v', no prerelease, no build metadata, no
# slashes or dots that could escape the output directory.
case "$VERSION" in
*[!0-9.]* | *.*.*.* | *. | .*) die "version '$VERSION' is not plain numeric X.Y.Z" ;;
esac
if [ "$(printf '%s\n' "$VERSION" | tr '.' '\n' | grep -c . || true)" != "3" ]; then
	die "version '$VERSION' must have exactly three numeric components"
fi
[ "${#COMMIT}" -eq 40 ] || die "commit must be a full 40-character hash, got ${#COMMIT} characters"
case "$COMMIT" in
*[!0-9a-f]*) die "commit '$COMMIT' is not lowercase hexadecimal" ;;
esac

TAG="v$VERSION"
if [ -z "$OUTDIR" ]; then
	OUTDIR="$DIST/release"
fi

if [ "$OS" = darwin ]; then
	[ -n "$APP" ] || die "--app is required for darwin/arm64: Sparky.app is a release asset"
else
	[ -z "$APP" ] || die "--app is only valid for darwin/arm64"
fi

hex_at() { # hex_at FILE OFFSET COUNT
	od -An -v -tx1 -j "$2" -N "$3" "$1" 2>/dev/null | tr -d ' \t\n'
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		die "no sha256sum or shasum available"
	fi
}

verify_sums() { # verify_sums DIR FILE
	if command -v sha256sum >/dev/null 2>&1; then
		(cd "$1" && sha256sum -c "$2") >/dev/null
	elif command -v shasum >/dev/null 2>&1; then
		(cd "$1" && shasum -a 256 -c "$2") >/dev/null
	else
		die "no checksum verifier available"
	fi
}

# check_binary confirms the artifact really is the platform/arch it is about
# to be labelled with, using the object-file headers rather than trusting the
# filename. A wrong-arch release asset is otherwise only discovered on the
# Spark that tries to run it.
check_binary() { # check_binary FILE LABEL
	if [ ! -f "$1" ]; then
		die "$2 is missing: $1"
	fi
	if [ ! -s "$1" ]; then
		die "$2 is empty: $1"
	fi
	if [ ! -x "$1" ]; then
		die "$2 is not executable: $1"
	fi
	case "$OS/$ARCH" in
	linux/arm64)
		if [ "$(hex_at "$1" 0 4)" != "7f454c46" ]; then
			die "$2 is not an ELF binary: $1"
		fi
		# e_type must be ET_EXEC (2) or ET_DYN (3, a PIE), never a DSO or object.
		case "$(hex_at "$1" 16 2)" in
		0200 | 0300) ;;
		*) die "$2 is not an ELF executable or PIE: $1" ;;
		esac
		if [ "$(hex_at "$1" 18 2)" != "b700" ]; then
			die "$2 is not AArch64 (EM_AARCH64): $1"
		fi
		;;
	darwin/arm64)
		# MH_MAGIC_64 little-endian on disk, then CPU_TYPE_ARM64, then
		# MH_EXECUTE: a dylib or a bundle stub must never ship as the CLI.
		if [ "$(hex_at "$1" 0 4)" != "cffaedfe" ]; then
			die "$2 is not a 64-bit Mach-O binary: $1"
		fi
		if [ "$(hex_at "$1" 4 4)" != "0c000001" ]; then
			die "$2 is not arm64 (CPU_TYPE_ARM64): $1"
		fi
		if [ "$(hex_at "$1" 12 4)" != "02000000" ]; then
			die "$2 is not a Mach-O executable (MH_EXECUTE): $1"
		fi
		;;
	esac
}

# check_commit_embedded proves the -ldflags -X injection actually happened. A
# release that silently kept the development version would be untraceable, so
# this is fatal rather than a warning.
check_commit_embedded() { # check_commit_embedded FILE LABEL
	if ! LC_ALL=C grep -a -q -- "$COMMIT" "$1" 2>/dev/null; then
		die "$1 does not embed release commit $COMMIT; build it with 'make $RELEASE_TARGET VERSION=$VERSION COMMIT=$COMMIT'"
	fi
}

SRC_DIR="$DIST/$OS-$ARCH"
if [ ! -d "$SRC_DIR" ]; then
	die "no binaries in $SRC_DIR; run 'make $RELEASE_TARGET VERSION=$VERSION COMMIT=<40-hex>'"
fi
check_binary "$SRC_DIR/sparky" "CLI"
check_commit_embedded "$SRC_DIR/sparky" "CLI"
check_binary "$SRC_DIR/sparky-install" "installer"

mkdir -p "$OUTDIR"
# Drop only this platform's previous output, so a re-run is clean and a stale
# artifact can never ride along into a publish.
rm -f "$OUTDIR/sparky-$VERSION-$OS-$ARCH.tar.gz"
rm -f "$OUTDIR/Sparky-$VERSION-$OS-$ARCH.app.zip"
rm -f "$OUTDIR/SHA256SUMS.$OS-$ARCH"

STAGE_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/sparky-bundle.XXXXXX")
trap 'rm -rf "$STAGE_ROOT"' EXIT INT TERM
TOP="sparky-$VERSION-$OS-$ARCH"
STAGE="$STAGE_ROOT/$TOP"
mkdir -p "$STAGE/bin"

cp "$SRC_DIR/sparky" "$STAGE/bin/sparky"
cp "$SRC_DIR/sparky-install" "$STAGE/bin/sparky-install"
chmod 755 "$STAGE/bin/sparky" "$STAGE/bin/sparky-install"

if [ -f "$ROOT/LICENSE" ]; then
	cp "$ROOT/LICENSE" "$STAGE/LICENSE"
	chmod 644 "$STAGE/LICENSE"
fi

CLI_SUM=$(sha256_of "$STAGE/bin/sparky")
INSTALLER_SUM=$(sha256_of "$STAGE/bin/sparky-install")

# BUILDINFO.json travels inside every archive so an extracted install can
# always say which tag and commit it came from. No timestamps: the content is
# a pure function of the inputs.
if [ "$OS" = darwin ]; then
	SIGNING="adhoc"
else
	SIGNING="none"
fi
cat >"$STAGE/BUILDINFO.json" <<BUILDINFO
{
  "name": "sparky",
  "version": "$VERSION",
  "tag": "$TAG",
  "commit": "$COMMIT",
  "platform": "$OS/$ARCH",
  "signing": "$SIGNING",
  "notarized": false,
  "binaries": [
    { "path": "bin/sparky", "sha256": "$CLI_SUM" },
    { "path": "bin/sparky-install", "sha256": "$INSTALLER_SUM" }
  ]
}
BUILDINFO
chmod 644 "$STAGE/BUILDINFO.json"

# Reproducible tar options when the local tar understands them (sorted order,
# no owner or mtime noise); plain tar otherwise.
REPRO=""
PROBE=$(mktemp -d "${TMPDIR:-/tmp}/sparky-probe.XXXXXX")
mkdir -p "$PROBE/p"
: >"$PROBE/p/f"
if (cd "$PROBE" && tar -czf /dev/null --sort=name --owner=0 --group=0 \
	--numeric-owner --mtime=@0 p) 2>/dev/null; then
	REPRO="--sort=name --owner=0 --group=0 --numeric-owner --mtime=@0"
fi
rm -rf "$PROBE"

ARCHIVE="$OUTDIR/sparky-$VERSION-$OS-$ARCH.tar.gz"
# COPYFILE_DISABLE stops macOS from injecting ._ AppleDouble entries; the
# listing check below fails closed if any appear anyway.
if ! COPYFILE_DISABLE=1 tar $REPRO -czf "$ARCHIVE" -C "$STAGE_ROOT" "$TOP"; then
	die "could not create $ARCHIVE"
fi

LISTING=$(tar -tzf "$ARCHIVE")
for want in "$TOP/bin/sparky" "$TOP/bin/sparky-install" "$TOP/BUILDINFO.json"; do
	if ! printf '%s\n' "$LISTING" | grep -q -x -- "$want"; then
		die "$ARCHIVE is missing $want"
	fi
done
if printf '%s\n' "$LISTING" | grep -q -E '(^|/)\.|^/|^\.\.(/|$)'; then
	die "$ARCHIVE contains a dot-entry, an absolute path, or a parent path"
fi

# --- Sparky.app zip (macOS only) --------------------------------------------
APP_ARCHIVE=""
if [ -n "$APP" ]; then
	if [ ! -d "$APP" ]; then
		die "app bundle is missing: $APP (run 'make menubar' on a macOS runner)"
	fi
	if [ ! -x "$APP/Contents/MacOS/sparky-menubar" ]; then
		die "app bundle has no native executable: $APP/Contents/MacOS/sparky-menubar"
	fi
	if [ ! -f "$APP/Contents/Info.plist" ]; then
		die "app bundle has no Info.plist: $APP"
	fi
	BUNDLED_CLI="$APP/Contents/Resources/sparky"
	if [ ! -f "$BUNDLED_CLI" ]; then
		die "app bundle has no bundled CLI: $BUNDLED_CLI"
	fi
	check_commit_embedded "$BUNDLED_CLI" "bundled CLI"
	if ! command -v ditto >/dev/null 2>&1; then
		die "ditto is required to zip the app bundle while preserving its signature"
	fi
	APP_ARCHIVE="$OUTDIR/Sparky-$VERSION-$OS-$ARCH.app.zip"
	rm -f "$APP_ARCHIVE"
	# --keepParent keeps the Sparky.app directory inside the zip, and ditto
	# preserves the ad-hoc signature and Mach-O metadata that plain zip drops.
	if ! ditto -c -k --keepParent "$APP" "$APP_ARCHIVE"; then
		die "could not create $APP_ARCHIVE"
	fi
	if [ "$(hex_at "$APP_ARCHIVE" 0 4)" != "504b0304" ]; then
		die "$APP_ARCHIVE is not a zip archive"
	fi
	if command -v unzip >/dev/null 2>&1; then
		if ! unzip -tq "$APP_ARCHIVE" >/dev/null; then
			die "$APP_ARCHIVE is corrupt"
		fi
		if ! unzip -Z1 "$APP_ARCHIVE" | grep -q -x -- "Sparky.app/Contents/MacOS/sparky-menubar"; then
			die "$APP_ARCHIVE does not contain Sparky.app/Contents/MacOS/sparky-menubar"
		fi
	fi
fi

# --- checksums for exactly what this run produced ---------------------------
SUMS_NAME="SHA256SUMS.$OS-$ARCH"
SUMS="$OUTDIR/$SUMS_NAME"
{
	printf '%s  %s\n' "$(sha256_of "$ARCHIVE")" "sparky-$VERSION-$OS-$ARCH.tar.gz"
	if [ -n "$APP_ARCHIVE" ]; then
		printf '%s  %s\n' "$(sha256_of "$APP_ARCHIVE")" "Sparky-$VERSION-$OS-$ARCH.app.zip"
	fi
} >"$SUMS"
verify_sums "$OUTDIR" "$SUMS_NAME"

echo "release-bundle: $PLATFORM $TAG ($COMMIT)"
cat "$SUMS"
echo "release-bundle: layout $TOP/bin/sparky $TOP/bin/sparky-install $TOP/BUILDINFO.json $TOP/LICENSE"
