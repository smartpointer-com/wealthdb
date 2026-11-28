#!/bin/sh
# package.sh VERSION STAGE OUT — build the Debian package of the install
# staged in STAGE, into the file OUT.
#
# STAGE is what this, run in a tree that ./configure set up for the
# native engine with --prefix=/usr, stages:
#
#   make install DESTDIR=STAGE LIBEXECDIR=/usr/lib
#
# so STAGE holds usr/lib/wealthdb (the copy) and usr/bin (its two links).
# This adds the package's metadata and the copyright file and runs
# dpkg-deb. Every file belongs to root, whoever runs it.
#
# The package holds no Metabase: the dashboards download theirs on first
# start (web/web). What it depends on, by Ubuntu's names: the libraries
# the engine links, the Python of the launcher's helpers, the Java of the
# dashboards and the curl of their download. Docker, for the collectors
# that drive a browser, is recommended, not required.
#
# WEALTHDB_DEB_MAINTAINER is the package's Maintainer field, "Name
# <address>", with no default. The address is a GitHub noreply address:
# a problem goes to the repository's issues, not to someone's mail. A
# release build takes it from the git identity of whoever builds.
# WEALTHDB_DEB_ARCH overrides the architecture, dpkg's own by default.
# --control prints the control file only, and needs no dpkg-deb.
#
# POSIX sh.
set -eu

usage() { echo "usage: package.sh [--control] VERSION STAGE OUT" >&2; exit 2; }
fail() { echo "package.sh: $*" >&2; exit 1; }

control_only=0
if [ "${1:-}" = --control ]; then control_only=1; shift; fi
[ "$#" -eq 3 ] || usage
VERSION="$1" STAGE="$2" OUT="$3"

case "$VERSION" in
    v*) fail "the version is the tag without its v" ;;
    *[!0-9A-Za-z.+~-]* | [!0-9]* | '') fail "'$VERSION' is no version" ;;
esac
maintainer="${WEALTHDB_DEB_MAINTAINER:-}"
case "$maintainer" in
    ?*' <'?*'@users.noreply.github.com>') ;;
    *) fail "WEALTHDB_DEB_MAINTAINER is not 'Name <address>' with a GitHub noreply address" ;;
esac
case "$maintainer" in *"
"*) fail "WEALTHDB_DEB_MAINTAINER holds a line break" ;; esac
[ -x "$STAGE/usr/lib/wealthdb/wealthdb/wealthdb" ] ||
    fail "$STAGE holds no staged copy (make install DESTDIR=$STAGE LIBEXECDIR=/usr/lib)"
[ -L "$STAGE/usr/bin/wealthdb" ] || fail "$STAGE holds no usr/bin/wealthdb link"
[ -z "$(find "$STAGE" -name '*.jar' -print)" ] || fail "$STAGE holds a jar; the package carries no Metabase"

arch="${WEALTHDB_DEB_ARCH:-}"
if [ -z "$arch" ]; then
    command -v dpkg >/dev/null 2>&1 || fail "no dpkg here to name the architecture; set WEALTHDB_DEB_ARCH"
    arch="$(dpkg --print-architecture)"
fi
size="$(du -sk "$STAGE/usr" | cut -f1)"

control() {
    cat <<EOF
Package: wealthdb
Version: $VERSION
Architecture: $arch
Maintainer: $maintainer
Installed-Size: $size
Depends: libc6, libstdc++6, python3 (>= 3.10), openjdk-21-jre-headless, curl
Recommends: docker.io
Section: utils
Priority: optional
Homepage: https://github.com/smartpointer-com/wealthdb
Description: household finances from every bank and broker, in one local database
 wealthdb collects what banks and brokers show into one database on the
 computer, and reports on it. Run 'wealthdb config' to set it up.
 .
 Report a problem at https://github.com/smartpointer-com/wealthdb/issues
EOF
}

if [ "$control_only" = 1 ]; then
    control
    exit 0
fi
command -v dpkg-deb >/dev/null 2>&1 || fail "no dpkg-deb here"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
[ -f "$ROOT/LICENSE" ] || fail "no LICENSE beside this script's tree"

mkdir -p "$STAGE/DEBIAN" "$STAGE/usr/share/doc/wealthdb"
control > "$STAGE/DEBIAN/control"
cp "$ROOT/LICENSE" "$STAGE/usr/share/doc/wealthdb/copyright"
chmod 0644 "$STAGE/DEBIAN/control" "$STAGE/usr/share/doc/wealthdb/copyright"
dpkg-deb --root-owner-group --build "$STAGE" "$OUT" >/dev/null
echo "package.sh: $OUT"
