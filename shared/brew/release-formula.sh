#!/bin/sh
# release-formula.sh VERSION [ARCHIVE] — print the Homebrew formula of the
# release vVERSION, for the tap, after holding the source archive it
# will name to this checkout's own tag.
#
# The formula tells Homebrew to download GitHub's archive of the tag and
# gives its checksum. A checksum taken from a download vouches only for
# what was downloaded. So the archive is first compared, member by
# member, with what `git archive` makes of the tag here (same_tree.py
# beside this file): the same names, each a file with the same bytes and
# execute bit, a link with the same target, or a folder. Nothing is
# unpacked. An archive that holds anything else, or lacks anything, is
# refused, and no formula is printed.
#
# ARCHIVE is a file to check in place of the download. The formula's
# pins come from web/Dockerfile, read by formula.sh through
# shared/wrappers/image-tag.sh, so those two files and shared/brew/ must
# be as the tag has them.
#
# Run it on a checkout whose tags are trusted: the tag is the reference.
set -eu

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
fail() { echo "release-formula.sh: $*" >&2; exit 1; }

VERSION="${1:-}" ARCHIVE="${2:-}"
case "$#" in
    1 | 2) ;;
    *) echo "usage: release-formula.sh VERSION [ARCHIVE]" >&2; exit 2 ;;
esac
case "$VERSION" in
    v*) echo "release-formula.sh: the version is the tag without its v" >&2; exit 2 ;;
    *[!0-9A-Za-z.-]* | [!0-9]* | '') echo "release-formula.sh: '$VERSION' is no version" >&2; exit 2 ;;
esac
TAG="v$VERSION" REF="refs/tags/v$VERSION"
command -v python3 >/dev/null 2>&1 || fail "python3 is needed to compare the archive"

git -C "$ROOT" rev-parse -q --verify "$REF^{commit}" >/dev/null ||
    fail "this checkout has no tag $TAG"
git -C "$ROOT" diff --quiet "$REF" -- web/Dockerfile shared/brew shared/wrappers/image-tag.sh ||
    fail "web/Dockerfile, shared/brew or shared/wrappers/image-tag.sh differs from $TAG here; check the tag out first"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
if [ -n "$ARCHIVE" ]; then
    [ -f "$ARCHIVE" ] || fail "$ARCHIVE is no file"
    cp -- "$ARCHIVE" "$tmp/archive.tar.gz"
else
    url="https://github.com/smartpointer-com/wealthdb/archive/refs/tags/$TAG.tar.gz"
    curl -fsSL --proto '=https' --tlsv1.2 -o "$tmp/archive.tar.gz" "$url" ||
        fail "could not download $url"
fi

git -C "$ROOT" archive --format=tar -o "$tmp/tag.tar" "$REF" ||
    fail "git could not archive $TAG"
if ! python3 -I "$ROOT/shared/brew/same_tree.py" "$tmp/tag.tar" "$tmp/archive.tar.gz" > "$tmp/diff" 2>&1; then
    echo "release-formula.sh: the archive is not the tree of $TAG:" >&2
    cut -c1-200 "$tmp/diff" >&2
    exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
    sum="$(sha256sum "$tmp/archive.tar.gz" | cut -d' ' -f1)"
else
    sum="$(shasum -a 256 "$tmp/archive.tar.gz" | cut -d' ' -f1)"
fi
echo "release-formula.sh: the archive is the tree of $TAG; sha256 $sum" >&2
# No exec: the trap above removes the archive when this shell ends.
"$ROOT/shared/brew/formula.sh" "$VERSION" "$sum"
