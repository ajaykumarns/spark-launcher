#!/bin/sh
# Publish a fully built and verified Sparky release with the gh CLI.
#
# Usage: sh tools/release-publish.sh --dir dist/release --tag v0.6.3 \
#          --commit <40-hex> [--repo owner/repo] [--title TITLE] \
#          [--notes dist/RELEASE_NOTES.md]
#
# Draft-first, publish-last, and fail closed at every step:
#
#   1. the remote tag still resolves to the exact commit that was built
#      (a tag moved since preflight aborts the publish);
#   2. an existing published release for the tag is never touched: its assets
#      are not replaced, and the run fails with instructions instead;
#   3. an existing *draft* is reused, with the assets this run produced
#      removed first so a re-run publishes exactly these files;
#   4. assets are uploaded without --clobber, so any unexpected name collision
#      is an error rather than a silent overwrite;
#   5. the release is flipped from draft to published only after every asset
#      is present, and the published state is read back and verified.
#
# Requires gh with GH_TOKEN (or GITHUB_TOKEN) in the environment, and
# contents:write on that token. Nothing here deploys, installs, restarts a
# Spark, loads a model, or contacts any machine: the only network calls are to
# the GitHub release API.
set -eu

DIR=""
TAG=""
COMMIT=""
REPO=""
TITLE=""
NOTES=""

die() {
	echo "release-publish: $*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	--dir)
		[ $# -ge 2 ] || die "--dir needs a value"
		DIR=$2
		shift 2
		;;
	--tag)
		[ $# -ge 2 ] || die "--tag needs a value"
		TAG=$2
		shift 2
		;;
	--commit)
		[ $# -ge 2 ] || die "--commit needs a value"
		COMMIT=$2
		shift 2
		;;
	--repo)
		[ $# -ge 2 ] || die "--repo needs a value"
		REPO=$2
		shift 2
		;;
	--title)
		[ $# -ge 2 ] || die "--title needs a value"
		TITLE=$2
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
[ -n "$TAG" ] || die "--tag is required"
[ -n "$COMMIT" ] || die "--commit is required"
[ -d "$DIR" ] || die "no asset directory: $DIR"
case "$TAG" in
v[0-9]*) ;;
*) die "tag '$TAG' is not a vX.Y.Z release tag" ;;
esac
VERSION=${TAG#v}
case "$VERSION" in
*[!0-9.]* | *.*.*.* | *. | .*) die "tag '$TAG' is not a plain numeric vX.Y.Z" ;;
esac
[ "${#COMMIT}" -eq 40 ] || die "commit must be a full 40-character hash"
case "$COMMIT" in
*[!0-9a-f]*) die "commit '$COMMIT' is not lowercase hexadecimal" ;;
esac

command -v gh >/dev/null 2>&1 || die "gh is required"
if [ -z "${GH_TOKEN:-}" ] && [ -z "${GITHUB_TOKEN:-}" ]; then
	die "GH_TOKEN or GITHUB_TOKEN must be set (the release job grants contents:write only for this step)"
fi
[ -n "$REPO" ] || die "--repo owner/name is required so the publish never depends on a local git remote"
case "$REPO" in
*/*) ;;
*) die "--repo must be owner/name" ;;
esac

if [ -z "$TITLE" ]; then
	TITLE="Sparky $TAG"
fi

ASSETS="sparky-$VERSION-linux-arm64.tar.gz sparky-$VERSION-darwin-arm64.tar.gz Sparky-$VERSION-darwin-arm64.app.zip"
EXTRA="SHA256SUMS release-metadata.json"

# Every asset must exist and be non-empty; a missing platform build is fatal.
for name in $ASSETS $EXTRA; do
	if [ ! -f "$DIR/$name" ] || [ ! -s "$DIR/$name" ]; then
		die "$DIR/$name is missing or empty; run every build and release-metadata first"
	fi
done
if [ -n "$NOTES" ] && [ ! -s "$NOTES" ]; then
	die "--notes file is missing or empty: $NOTES"
fi

# 1. The remote tag must still point at the commit everything was built from.
#    The repository is given explicitly so the publish never depends on a
#    local git remote or ambient repo context.
REMOTE_COMMIT=$(gh api "repos/$REPO/commits/$TAG" --jq .sha 2>/dev/null || true)
if [ -z "$REMOTE_COMMIT" ]; then
	die "tag $TAG does not exist on $REPO; releases can only be cut from an existing tag"
fi
if [ "$REMOTE_COMMIT" != "$COMMIT" ]; then
	die "tag $TAG now points at $REMOTE_COMMIT, but the assets were built from $COMMIT; re-run preflight"
fi

asset_names() {
	gh release view "$TAG" --json assets --jq '.assets[].name' --repo "$REPO" 2>/dev/null || true
}

EXISTS=0
if gh release view "$TAG" --json tagName --jq .tagName --repo "$REPO" >/dev/null 2>&1; then
	EXISTS=1
	IS_DRAFT=$(gh release view "$TAG" --json isDraft --jq .isDraft --repo "$REPO")
	if [ "$IS_DRAFT" != "true" ]; then
		die "release $TAG is already published on $REPO. Its assets are never overwritten; delete that release first if you truly intend to re-publish."
	fi
	echo "release-publish: reusing existing draft release for $TAG"
	for name in $ASSETS $EXTRA; do
		if asset_names | grep -q -x -- "$name"; then
			gh release delete-asset "$TAG" "$name" --repo "$REPO" -y ||
				die "could not remove the stale draft asset $name"
		fi
	done
else
	if [ -n "$NOTES" ]; then
		gh release create "$TAG" --repo "$REPO" --target "$COMMIT" --verify-tag \
			--draft --title "$TITLE" --notes-file "$NOTES" ||
			die "could not create the draft release for $TAG"
	else
		gh release create "$TAG" --repo "$REPO" --target "$COMMIT" --verify-tag \
			--draft --title "$TITLE" --notes "$TAG" ||
			die "could not create the draft release for $TAG"
	fi
	echo "release-publish: created draft release for $TAG at $COMMIT"
fi

# 2. Upload the verified artifacts. No --clobber: an unexpected collision must
#    stop the release rather than quietly replace someone else's bytes.
UPLOAD_ARGS=""
for name in $ASSETS $EXTRA; do
	UPLOAD_ARGS="$UPLOAD_ARGS $DIR/$name"
done
# shellcheck disable=SC2086
gh release upload "$TAG" $UPLOAD_ARGS --repo "$REPO" ||
	die "could not upload the release assets for $TAG"

# 3. Every expected asset must be attached before the draft is published.
ATTACHED=$(asset_names | LC_ALL=C sort)
# shellcheck disable=SC2086
EXPECTED=$(printf '%s\n' $ASSETS $EXTRA | LC_ALL=C sort)
if [ "$ATTACHED" != "$EXPECTED" ]; then
	echo "release-publish: attached assets do not match what was built" >&2
	echo "expected:" >&2
	printf '%s\n' "$EXPECTED" >&2
	echo "attached:" >&2
	printf '%s\n' "$ATTACHED" >&2
	die "refusing to publish an incomplete release"
fi

gh release edit "$TAG" --repo "$REPO" --draft=false ||
	die "could not publish the release for $TAG"

# 4. Read the published state back; never assume the edit took effect.
FINAL_DRAFT=$(gh release view "$TAG" --json isDraft --jq .isDraft --repo "$REPO")
FINAL_TAG=$(gh release view "$TAG" --json tagName --jq .tagName --repo "$REPO")
if [ "$FINAL_DRAFT" != "false" ] || [ "$FINAL_TAG" != "$TAG" ]; then
	die "release $TAG did not end up published as expected (isDraft=$FINAL_DRAFT tagName=$FINAL_TAG)"
fi

echo "release-publish: published $TAG ($COMMIT) on $REPO"
gh release view "$TAG" --json url --jq .url --repo "$REPO" || true
