#!/bin/sh
# Verify a complete set of Sparky release assets and write the publish inputs.
#
# Usage: sh tools/release-metadata.sh --dir dist/release --version 0.6.3 \
#          --commit <40-hex> [--notes dist/RELEASE_NOTES.md]
#
# Run this in the publish job, after every build job has finished and its
# artifacts have been collected into one directory. It is the last gate before
# anything reaches a GitHub release, so every check is fatal:
#
#   * exactly the expected asset set is present: the Linux ARM64 and Mac
#     ARM64 CLI+installer archives and the Mac ARM64 Sparky.app zip. A missing
#     platform fails the release; so does an unexpected stray file.
#   * per-platform SHA256SUMS.<os>-<arch> files verify, and every digest is
#     recomputed here.
#   * each archive really contains bin/sparky, bin/sparky-install and a
#     BUILDINFO.json naming this tag, version, commit and platform, and the
#     CLI binaries embed the full commit.
#   * the app zip contains the native executable and its bundled CLI.
#
# It then writes, into --dir:
#
#   SHA256SUMS               checksums of the three published assets
#   release-metadata.json    tag, version, commit, platforms, assets, digests
#
# and, when --notes is given, the release notes file for 'gh release create'.
# This script never uploads, publishes, deploys, installs or restarts anything.
set -eu

DIR=""
VERSION=""
COMMIT=""
NOTES=""

die() {
	echo "release-metadata: $*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	--dir)
		[ $# -ge 2 ] || die "--dir needs a value"
		DIR=$2
		shift 2
		;;
	--version)
		[ $# -ge 2 ] || die "--version needs a value"
		VERSION=$2
		shift 2
		;;
	--commit)
		[ $# -ge 2 ] || die "--commit needs a value"
		COMMIT=$2
		shift 2
		;;
	--notes)
		[ $# -ge 2 ] || die "--notes needs a value"
		NOTES=$2
		shift 2
		;;
	*) die "unknown argument: $1" ;;
	esac
done

[ -n "$DIR" ] || die "--dir is required"
[ -n "$VERSION" ] || die "--version is required"
[ -n "$COMMIT" ] || die "--commit is required"
[ -d "$DIR" ] || die "no asset directory: $DIR"
case "$VERSION" in
*[!0-9.]* | *.*.*.* | *. | .*) die "version '$VERSION' is not plain numeric X.Y.Z" ;;
esac
if [ "$(printf '%s\n' "$VERSION" | tr '.' '\n' | grep -c . || true)" != "3" ]; then
	die "version '$VERSION' must have exactly three numeric components"
fi
[ "${#COMMIT}" -eq 40 ] || die "commit must be a full 40-character hash"
case "$COMMIT" in
*[!0-9a-f]*) die "commit '$COMMIT' is not lowercase hexadecimal" ;;
esac

TAG="v$VERSION"
LINUX_TGZ="sparky-$VERSION-linux-arm64.tar.gz"
MAC_TGZ="sparky-$VERSION-darwin-arm64.tar.gz"
MAC_ZIP="Sparky-$VERSION-darwin-arm64.app.zip"

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		die "no sha256sum or shasum available"
	fi
}

size_of() {
	wc -c <"$1" | tr -d ' '
}

require_file() { # require_file PATH LABEL
	if [ ! -f "$1" ] || [ ! -s "$1" ]; then
		die "$1 ($2) is missing or empty; every platform build must succeed before publishing"
	fi
}

# --- 1. the asset set is exactly what a release must contain ----------------
require_file "$DIR/$LINUX_TGZ" "Linux ARM64 CLI archive"
require_file "$DIR/$MAC_TGZ" "Mac ARM64 CLI archive"
require_file "$DIR/$MAC_ZIP" "Mac ARM64 app archive"

for stray in "$DIR"/* "$DIR"/.[!.]* "$DIR"/..?*; do
	[ -e "$stray" ] || continue
	name=$(basename "$stray")
	case "$name" in
	"$LINUX_TGZ" | "$MAC_TGZ" | "$MAC_ZIP") ;;
	SHA256SUMS.linux-arm64 | SHA256SUMS.darwin-arm64 | SHA256SUMS | release-metadata.json) ;;
	*) die "unexpected file in the release directory: $name" ;;
	esac
	if [ -d "$stray" ]; then
		die "directories are not release assets: $name"
	fi
done

# --- 2. checksums: the builder's own file first, then recompute -------------
for sums in "$DIR"/SHA256SUMS.linux-arm64 "$DIR"/SHA256SUMS.darwin-arm64; do
	[ -f "$sums" ] || continue
	base=$(basename "$sums")
	if command -v sha256sum >/dev/null 2>&1; then
		(cd "$DIR" && sha256sum -c "$base") >/dev/null ||
			die "$base does not verify"
	elif command -v shasum >/dev/null 2>&1; then
		(cd "$DIR" && shasum -a 256 -c "$base") >/dev/null ||
			die "$base does not verify"
	fi
done

LINUX_SUM=$(sha256_of "$DIR/$LINUX_TGZ")
MAC_SUM=$(sha256_of "$DIR/$MAC_TGZ")
ZIP_SUM=$(sha256_of "$DIR/$MAC_ZIP")
LINUX_SIZE=$(size_of "$DIR/$LINUX_TGZ")
MAC_SIZE=$(size_of "$DIR/$MAC_TGZ")
ZIP_SIZE=$(size_of "$DIR/$MAC_ZIP")

# --- 3. open every asset and prove what is inside it ------------------------
WORK=$(mktemp -d "${TMPDIR:-/tmp}/sparky-verify.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM

verify_tree() { # verify_tree DIR TOP PLATFORM
	top=$2
	platform=$3
	for want in "$top/bin/sparky" "$top/bin/sparky-install" "$top/BUILDINFO.json"; do
		if [ ! -f "$1/$want" ] || [ ! -s "$1/$want" ]; then
			die "$top is missing $want"
		fi
	done
	if [ ! -x "$1/$top/bin/sparky" ]; then
		die "$top/bin/sparky is not executable"
	fi
	if ! LC_ALL=C grep -a -q -- "$COMMIT" "$1/$top/bin/sparky" 2>/dev/null; then
		die "$top/bin/sparky does not embed commit $COMMIT"
	fi
	for field in "\"version\": \"$VERSION\"" "\"commit\": \"$COMMIT\"" "\"platform\": \"$platform\""; do
		if ! grep -q -F -- "$field" "$1/$top/BUILDINFO.json"; then
			die "$top/BUILDINFO.json does not record $field"
		fi
	done
}

LINUX_OUT="$WORK/linux"
MAC_OUT="$WORK/darwin"
APP_OUT="$WORK/app"
mkdir -p "$LINUX_OUT" "$MAC_OUT" "$APP_OUT"

tar -xzf "$DIR/$LINUX_TGZ" -C "$LINUX_OUT" || die "could not unpack $LINUX_TGZ"
tar -xzf "$DIR/$MAC_TGZ" -C "$MAC_OUT" || die "could not unpack $MAC_TGZ"
verify_tree "$LINUX_OUT" "sparky-$VERSION-linux-arm64" "linux/arm64"
verify_tree "$MAC_OUT" "sparky-$VERSION-darwin-arm64" "darwin/arm64"

if command -v unzip >/dev/null 2>&1; then
	unzip -tq "$DIR/$MAC_ZIP" >/dev/null || die "$MAC_ZIP is corrupt"
	unzip -q "$DIR/$MAC_ZIP" -d "$APP_OUT" || die "could not unpack $MAC_ZIP"
	for want in Sparky.app/Contents/MacOS/sparky-menubar \
		Sparky.app/Contents/Resources/sparky Sparky.app/Contents/Info.plist; do
		if [ ! -f "$APP_OUT/$want" ] || [ ! -s "$APP_OUT/$want" ]; then
			die "$MAC_ZIP is missing $want"
		fi
	done
	if ! LC_ALL=C grep -a -q -- "$COMMIT" "$APP_OUT/Sparky.app/Contents/Resources/sparky" 2>/dev/null; then
		die "$MAC_ZIP bundles a CLI that does not embed commit $COMMIT"
	fi
else
	# unzip is present on the runner images; refusing to skip the check keeps
	# a stripped-down host from publishing an unverified bundle.
	die "unzip is required to verify $MAC_ZIP"
fi

# --- 4. publish inputs ------------------------------------------------------
SUMS="$DIR/SHA256SUMS"
printf '%s  %s\n' "$LINUX_SUM" "$LINUX_TGZ" >"$SUMS"
printf '%s  %s\n' "$MAC_SUM" "$MAC_TGZ" >>"$SUMS"
printf '%s  %s\n' "$ZIP_SUM" "$MAC_ZIP" >>"$SUMS"
if command -v sha256sum >/dev/null 2>&1; then
	(cd "$DIR" && sha256sum -c SHA256SUMS) >/dev/null || die "regenerated SHA256SUMS does not verify"
elif command -v shasum >/dev/null 2>&1; then
	(cd "$DIR" && shasum -a 256 -c SHA256SUMS) >/dev/null || die "regenerated SHA256SUMS does not verify"
fi
SUMS_SUM=$(sha256_of "$SUMS")

cat >"$DIR/release-metadata.json" <<METADATA
{
  "name": "sparky",
  "tag": "$TAG",
  "version": "$VERSION",
  "commit": "$COMMIT",
  "platforms": ["darwin/arm64", "linux/arm64"],
  "signing": "adhoc",
  "notarized": false,
  "checksums_file": "SHA256SUMS",
  "checksums_sha256": "$SUMS_SUM",
  "assets": [
    {
      "file": "$LINUX_TGZ",
      "platform": "linux/arm64",
      "kind": "cli-and-installer",
      "layout": "sparky-$VERSION-linux-arm64/bin/{sparky,sparky-install}",
      "sha256": "$LINUX_SUM",
      "size_bytes": $LINUX_SIZE
    },
    {
      "file": "$MAC_TGZ",
      "platform": "darwin/arm64",
      "kind": "cli-and-installer",
      "layout": "sparky-$VERSION-darwin-arm64/bin/{sparky,sparky-install}",
      "sha256": "$MAC_SUM",
      "size_bytes": $MAC_SIZE
    },
    {
      "file": "$MAC_ZIP",
      "platform": "darwin/arm64",
      "kind": "menubar-app",
      "layout": "Sparky.app",
      "sha256": "$ZIP_SUM",
      "size_bytes": $ZIP_SIZE
    }
  ]
}
METADATA

if [ -n "$NOTES" ]; then
	mkdir -p "$(dirname "$NOTES")"
	# Backticks are escaped: this heredoc expands variables, and the notes
	# contain literal markdown code spans.
	cat >"$NOTES" <<NOTES
## Sparky $TAG

Built from commit \`$COMMIT\` (tag \`$TAG\`). Nothing here deploys, installs or
restarts anything on the Mac or on the Sparks: downloading and installing stay
explicit operator actions.

| File | Platform | SHA256 |
| --- | --- | --- |
| \`$LINUX_TGZ\` | linux/arm64 (both Sparks) | \`$LINUX_SUM\` |
| \`$MAC_TGZ\` | darwin/arm64 (Mac) | \`$MAC_SUM\` |
| \`$MAC_ZIP\` | darwin/arm64 menu bar app | \`$ZIP_SUM\` |

Download, verify, extract and install (the layout never changes between
releases). \`SHA256SUMS\` lists all three assets, so verify only the archive you
downloaded: the \`awk\` filter keeps its single line before the digest is
checked. Set VERSION and download from the public release URL, then let the
bundled installer swap the binary.

On the Mac (Apple Silicon):

\`\`\`sh
set -eu
VERSION=$VERSION
BASE=https://github.com/ajaykumarns/spark-launcher/releases/download/v\${VERSION}
ASSET=$MAC_TGZ
curl -fLO "\${BASE}/\${ASSET}"
curl -fLO "\${BASE}/SHA256SUMS"
awk -v a="\${ASSET}" '\$2 == a' SHA256SUMS | shasum -a 256 -c -
tar -xzf "\${ASSET}"
./sparky-$VERSION-darwin-arm64/bin/sparky-install \\
    --source ./sparky-$VERSION-darwin-arm64/bin/sparky
sparky --version                 # sparky $VERSION (commit $COMMIT)
\`\`\`

On a Spark (linux/arm64), use the linux archive and \`sha256sum\`:

\`\`\`sh
set -eu
VERSION=$VERSION
BASE=https://github.com/ajaykumarns/spark-launcher/releases/download/v\${VERSION}
ASSET=$LINUX_TGZ
curl -fLO "\${BASE}/\${ASSET}"
curl -fLO "\${BASE}/SHA256SUMS"
awk -v a="\${ASSET}" '\$2 == a' SHA256SUMS | sha256sum -c -
tar -xzf "\${ASSET}"
./sparky-$VERSION-linux-arm64/bin/sparky-install \\
    --source ./sparky-$VERSION-linux-arm64/bin/sparky
sparky --version                 # sparky $VERSION (commit $COMMIT)
\`\`\`

Unzip \`$MAC_ZIP\` from the same base URL and copy \`Sparky.app\` to
\`~/Applications\` for the menu bar app. The app is ad-hoc signed by the build
runner and is **not notarized**, so Gatekeeper may warn on first launch. The
installer keeps a private backup of the previous binary and swaps it in
atomically. See the README "Releases" section for the full steps and rollback.
NOTES
fi

echo "release-metadata: $TAG from $COMMIT"
cat "$SUMS"
echo "release-metadata: wrote $DIR/release-metadata.json"
