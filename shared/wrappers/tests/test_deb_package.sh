#!/usr/bin/env bash
# Behaviour test of shared/deb/package.sh over a small staged install:
#   - the control file names the package, its version, its architecture,
#     its maintainer and what it depends on, with no Metabase;
#   - it refuses a version with its v or of another shape, a maintainer
#     that is not "Name <address>", a stage with no copy, and a stage
#     that holds a jar;
#   - where dpkg-deb is (Linux), the package holds the copy and its
#     links, every file owned by root, and the copyright file.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"
P="$ROOT/shared/deb/package.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
S="$TMP/stage"
mkdir -p "$S/usr/lib/wealthdb/wealthdb" "$S/usr/lib/wealthdb/bin" "$S/usr/bin"
printf '#!/bin/sh\necho launcher\n' > "$S/usr/lib/wealthdb/wealthdb/wealthdb"
printf '#!/bin/sh\necho dispatcher\n' > "$S/usr/lib/wealthdb/bin/wealthdb-collect"
chmod +x "$S/usr/lib/wealthdb/wealthdb/wealthdb" "$S/usr/lib/wealthdb/bin/wealthdb-collect"
ln -s /usr/lib/wealthdb/wealthdb/wealthdb "$S/usr/bin/wealthdb"
ln -s /usr/lib/wealthdb/bin/wealthdb-collect "$S/usr/bin/wealthdb-collect"
export WEALTHDB_DEB_MAINTAINER="Example Maintainer <12345+example@users.noreply.github.com>"
export WEALTHDB_DEB_ARCH=amd64

echo "the control file:"
out="$(sh "$P" --control 1.2.3 "$S" "$TMP/x.deb" 2>&1)"; rc=$?
[ "$rc" = 0 ] && ok "it prints" || bad "exit $rc: $out"
has_item "the package" "Package: wealthdb" "$out"
has_item "its version" "Version: 1.2.3" "$out"
has_item "the architecture asked for" "Architecture: amd64" "$out"
has_item "the maintainer" "Maintainer: Example Maintainer <12345+example@users.noreply.github.com>" "$out"
has_item "problems go to the issues" " Report a problem at https://github.com/smartpointer-com/wealthdb/issues" "$out"
has_item "what it depends on" \
    "Depends: libc6, libstdc++6, python3 (>= 3.10), openjdk-21-jre-headless, curl" "$out"
has_item "Docker is recommended, not required" "Recommends: docker.io" "$out"
lacks "no Metabase among them" "metabase" "$out"
[ ! -e "$TMP/x.deb" ] && ok "--control builds nothing" || bad "--control wrote a package"

echo "what it refuses:"
# refused DESC NEEDLE ARGS...: package.sh fails with NEEDLE in its message.
refused() {
    local desc="$1" needle="$2" out rc
    shift 2
    out="$(sh "$P" "$@" 2>&1)"; rc=$?
    if [ "$rc" != 0 ] && grep -qF -- "$needle" <<<"$out"; then ok "$desc"; else bad "$desc (exit $rc: $out)"; fi
}
refused "a version with its v" "without its v" --control v1.2.3 "$S" "$TMP/x.deb"
refused "a version of another shape" "is no version" --control 'latest' "$S" "$TMP/x.deb"
refused "two arguments" "usage:" --control 1.2.3 "$S"
WEALTHDB_DEB_MAINTAINER="" refused "no maintainer" "WEALTHDB_DEB_MAINTAINER" --control 1.2.3 "$S" "$TMP/x.deb"
WEALTHDB_DEB_MAINTAINER="Just A Name" refused "a maintainer with no address" "GitHub noreply" \
    --control 1.2.3 "$S" "$TMP/x.deb"
WEALTHDB_DEB_MAINTAINER="A Name <someone@example.invalid>" refused "an address that is not GitHub's noreply" \
    "GitHub noreply" --control 1.2.3 "$S" "$TMP/x.deb"
refused "a stage with no copy" "holds no staged copy" --control 1.2.3 "$TMP/none" "$TMP/x.deb"
: > "$S/usr/lib/wealthdb/metabase.jar"
refused "a stage that holds a jar" "carries no Metabase" --control 1.2.3 "$S" "$TMP/x.deb"
rm -f "$S/usr/lib/wealthdb/metabase.jar"

echo "the package:"
if command -v dpkg-deb >/dev/null 2>&1; then
    out="$(sh "$P" 1.2.3 "$S" "$TMP/wealthdb.deb" 2>&1)"; rc=$?
    [ "$rc" = 0 ] && ok "it builds" || bad "exit $rc: $out"
    files="$(dpkg-deb -c "$TMP/wealthdb.deb" 2>&1)"
    has "it holds the launcher" "./usr/lib/wealthdb/wealthdb/wealthdb" "$files"
    has "the link on the PATH" "./usr/bin/wealthdb -> /usr/lib/wealthdb/wealthdb/wealthdb" "$files"
    has "and the copyright file" "./usr/share/doc/wealthdb/copyright" "$files"
    lacks "every file belongs to root" "$(id -un)/" "$files"
    has "the control file is in it" "Version: 1.2.3" "$(dpkg-deb -I "$TMP/wealthdb.deb" 2>&1)"
else
    ok "(no dpkg-deb here: the build is checked on Linux)"
fi

summary "deb package tests"
