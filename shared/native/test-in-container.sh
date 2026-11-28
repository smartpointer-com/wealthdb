#!/bin/sh
# test-in-container.sh SUITE... — what `make test-native` runs inside the
# wealthdb/native-test image: copy the tree from /src, configure it for
# the native engine, build, run the named suites of `make test`, install
# to a prefix, check what the copy holds, package the tree as a Debian
# package and check that, walk the installed tree end to end (e2e.sh),
# and uninstall. The exit status is the walk's when the
# walk fails.
#
# /src is the checkout, read-only. /cache persists between runs: Go's
# caches and the dashboards' two downloaded files. Everything else lives
# in the container and ends with it. The time zone is far from UTC, so
# an instant read as a local day fails a test.
set -eu

export HOME=/work/home XDG_CACHE_HOME=/work/cache
export GOCACHE=/cache/go-build GOMODCACHE=/cache/go-mod GOFLAGS=-buildvcs=false
export TZ=Pacific/Auckland
mkdir -p "$HOME" "$XDG_CACHE_HOME" /work/tree /cache/web-lib

fail() { echo "test-in-container: $*" >&2; exit 1; }

echo "==> copy the tree"
# Not what a build made on another system: the venvs, the caches, a
# native engine, and the checkout's own configure record.
tar -C /src -cf - \
    --exclude=./.git --exclude=.venv --exclude=__pycache__ --exclude=.pytest_cache --exclude=.ruff_cache \
    --exclude=./config.mk --exclude=./.image-tag --exclude=./.demo-root \
    --exclude=./wealthdb/wealthdb-engine --exclude=./web/lib --exclude=./site/public/shots . |
    tar -C /work/tree -xf -
cd /work/tree
# Metabase and its driver persist between runs in the mounted cache.
export WEALTHDB_WEB_LIB_DIR=/cache/web-lib

echo "==> configure"
./configure --engine=native --prefix=/work/prefix
echo "==> build"
make --no-print-directory build-wealthdb build-web
if [ "$#" -gt 0 ]; then
    make --no-print-directory test SUITES="$*"
fi

echo "==> install"
# What a collector's run can leave in a tree with no .git, which has no
# list of tracked files to copy by: neither may reach the copy.
: > collectors/plaid/core
: > collectors/plaid/x.log
make --no-print-directory install
COPY=/work/prefix/libexec/wealthdb
for f in config.mk wealthdb/wealthdb wealthdb/wealthdb-engine bin/wealthdb-collect \
    collectors/plaid/plaid; do
    [ -e "$COPY/$f" ] || fail "the copy lacks $f"
done
# The dashboards download their Metabase on first start: no copy holds one.
[ -z "$(find "$COPY" -name '*.jar' -print)" ] || fail "the copy holds a jar"
extra="$(find /work/prefix \( -name tests -o -name 'test_*' -o -name .venv -o -name __pycache__ \
    -o -name core -o -name '*.log' \) -print)"
[ -z "$extra" ] || fail "the copy holds what a run does not need: $extra"

echo "==> package"
# The same tree, staged for /usr as a Debian package stages it, and
# packaged (shared/deb/package.sh). Installing it needs root, which this
# run is not; the package's files are checked instead.
make --no-print-directory install DESTDIR=/work/stage PREFIX=/usr BINDIR=/usr/bin LIBEXECDIR=/usr/lib
WEALTHDB_DEB_MAINTAINER="wealthdb test <12345+test@users.noreply.github.com>" \
    shared/deb/package.sh 0.0.0 /work/stage /work/wealthdb.deb
files="$(dpkg-deb -c /work/wealthdb.deb)"
for f in ./usr/lib/wealthdb/wealthdb/wealthdb-engine ./usr/lib/wealthdb/config.mk \
    ./usr/lib/wealthdb/bin/wealthdb-collect "./usr/bin/wealthdb -> /usr/lib/wealthdb/wealthdb/wealthdb"; do
    case "$files" in *"$f"*) ;; *) fail "the package lacks $f" ;; esac
done
case "$files" in *.jar*) fail "the package holds a jar" ;; esac

echo "==> end to end, from the installed tree"
rc=0
shared/native/e2e.sh /work/prefix/bin/wealthdb /work/tree /work/e2e || rc=$?

echo "==> uninstall"
left=
make --no-print-directory uninstall || left="make uninstall failed"
for f in "$COPY" /work/prefix/bin/wealthdb /work/prefix/bin/wealthdb-collect; do
    if [ -e "$f" ] || [ -L "$f" ]; then left="$f is still there"; fi
done
if [ -n "$left" ]; then
    echo "test-in-container: uninstall: $left" >&2
    [ "$rc" -ne 0 ] || rc=1
fi
exit "$rc"
