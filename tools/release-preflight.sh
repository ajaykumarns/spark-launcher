#!/bin/sh
# Validate a Sparky release tag before any build minute is spent.
#
# Usage: sh tools/release-preflight.sh --tag v0.6.3
#          [--main-branch main] [--no-fetch] [--checkout-dir DIR]
#
# Every check is fatal (fail closed):
#   1. the tag name is exactly vX.Y.Z with three numeric components. No
#      prerelease (v1.2.3-rc.1) and no build metadata (v1.2.3+foo) is
#      accepted: this repository publishes stable numeric releases only.
#   2. refs/tags/<tag> really exists in this clone. An arbitrary ref, branch
#      name or raw commit SHA is never releasable, so a workflow_dispatch
#      input cannot smuggle an unchecked ref into a release.
#   3. the tag peels to a commit that is reachable from the tip of the main
#      branch, so a release can only ship what main has already accepted.
#   4. the commit is a full 40-hex hash, which is what every build job checks
#      out, so all platforms are built from one exact revision.
#
# When GITHUB_OUTPUT is set, tag/version/commit are appended to it for the
# downstream jobs. This script only reads the repository: it never pushes,
# tags, creates a release, deploys or restarts anything.
set -eu

TAG=""
MAIN_BRANCH="main"
FETCH=1

die() {
	echo "release-preflight: $*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	--tag)
		[ $# -ge 2 ] || die "--tag needs a value"
		TAG=$2
		shift 2
		;;
	--main-branch)
		[ $# -ge 2 ] || die "--main-branch needs a value"
		MAIN_BRANCH=$2
		shift 2
		;;
	--no-fetch)
		FETCH=0
		shift
		;;
	--checkout-dir)
		[ $# -ge 2 ] || die "--checkout-dir needs a value"
		cd "$2"
		shift 2
		;;
	*)
		die "unknown argument: $1"
		;;
	esac
done

[ -n "$TAG" ] || die "--tag is required"
git rev-parse --git-dir >/dev/null 2>&1 || die "not inside a git repository"

# 1. Tag shape: vX.Y.Z, numeric, nothing else.
case "$TAG" in
v[0-9]*) ;;
*) die "tag '$TAG' does not start with 'v' followed by a digit" ;;
esac
VERSION=${TAG#v}
case "$VERSION" in
*[!0-9.]* | *.*.*.* | *. | .*) die "tag '$TAG' is not a plain numeric vX.Y.Z (prerelease and build-metadata tags are not published)" ;;
esac
VERSION_COMPONENTS=$(printf '%s\n' "$VERSION" | tr '.' '\n' | grep -c . || true)
[ "$VERSION_COMPONENTS" = "3" ] || die "tag '$TAG' must have exactly three numeric components"
for component in $(printf '%s\n' "$VERSION" | tr '.' ' '); do
	case "$component" in
	'') die "tag '$TAG' has an empty numeric component" ;;
	esac
done

command -v git >/dev/null 2>&1 || die "git is required"

MAIN_REF="refs/remotes/origin/$MAIN_BRANCH"
if [ "$FETCH" = "1" ]; then
	# Fetch the tag and the main tip explicitly: the workflow clones main, and
	# a dispatch run must see the tag as it exists on the remote right now.
	# Both refspecs are fixed and the tag name is already validated, so nothing
	# attacker-controlled reaches git.
	git fetch --no-tags --prune --force origin \
		"refs/tags/$TAG:refs/tags/$TAG" \
		"+refs/heads/$MAIN_BRANCH:$MAIN_REF" >/dev/null ||
		die "could not fetch refs/tags/$TAG and $MAIN_BRANCH from origin"
fi

# 2. The tag must exist as a real tag ref.
git show-ref --verify --quiet "refs/tags/$TAG" ||
	die "tag '$TAG' does not exist in this repository; publish from an existing tag only"

# 3. Peel to a commit (annotated tags included) and require reachability.
TAG_TYPE=$(git cat-file -t "refs/tags/$TAG" 2>/dev/null) ||
	die "tag '$TAG' cannot be read"
if [ "$TAG_TYPE" = "tag" ]; then
	COMMIT=$(git rev-parse "refs/tags/$TAG^{commit}" 2>/dev/null) ||
		die "annotated tag '$TAG' does not point at a commit"
else
	[ "$TAG_TYPE" = "commit" ] ||
		die "tag '$TAG' points at a '$TAG_TYPE' object, not a commit"
	COMMIT=$(git rev-parse "refs/tags/$TAG" 2>/dev/null)
fi

[ "${#COMMIT}" -eq 40 ] ||
	die "tag '$TAG' did not resolve to a 40-character commit hash"
case "$COMMIT" in
*[!0-9a-f]*) die "commit '$COMMIT' is not lowercase hexadecimal" ;;
esac

git show-ref --verify --quiet "$MAIN_REF" ||
	die "$MAIN_REF is not available locally; the release must be checked against main"
git merge-base --is-ancestor "$COMMIT" "$MAIN_REF" ||
	die "tag '$TAG' ($COMMIT) is not reachable from $MAIN_REF; merge it to main before releasing"

echo "tag:    $TAG"
echo "version: $VERSION"
echo "commit: $COMMIT"
echo "main:   $MAIN_REF (contains $COMMIT)"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
	{
		echo "tag=$TAG"
		echo "version=$VERSION"
		echo "commit=$COMMIT"
	} >>"$GITHUB_OUTPUT"
fi
