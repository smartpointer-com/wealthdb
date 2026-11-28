#!/bin/sh
# build.sh VERSION OUT [REF] — build the Debian package wealthdb_VERSION_amd64.deb
# into the folder OUT, from the tree at REF (default HEAD; a release
# passes its tag, v VERSION).
#
# The tree is git's archive of REF, as the Homebrew formula's source is,
# so nothing git does not track reaches the package and the engine states
# the commit it was built from. It is built in wealthdb/deb-build
# (shared/images/deb-build.Dockerfile), an Ubuntu 22.04: configured for
# the native engine with the prefix /usr, staged (make install DESTDIR)
# and packaged (package.sh beside this file). The package is for x86-64.
# Where Docker runs arm64 (a Mac with Apple silicon), the container is
# arm64 and builds the engine for x86-64 with Ubuntu's cross compilers:
# nothing runs translated (Go's runtime faults under Rosetta), and the
# engine links 22.04's x86-64 glibc either way.
#
# WEALTHDB_DEB_DOCKER_CONTEXT is the Docker context, the current one by
# default. OUT is a folder under the home folder, which Docker's VM
# shares.
#
# WEALTHDB_DEB_MAINTAINER is the package's Maintainer (package.sh); unset,
# it is "SmartPointer AG <address>" with the git identity's address, when
# that is a GitHub noreply address.
set -eu

usage() { echo "usage: build.sh VERSION OUT [REF]" >&2; exit 2; }
fail() { echo "build.sh: $*" >&2; exit 1; }
[ "$#" -ge 2 ] && [ "$#" -le 3 ] || usage
VERSION="$1" OUT="$2" REF="${3:-HEAD}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
. "$ROOT/shared/wrappers/image-tag.sh"

case "$VERSION" in
    v*) fail "the version is the tag without its v" ;;
    *[!0-9A-Za-z.+~-]* | [!0-9]* | '') fail "'$VERSION' is no version" ;;
esac
commit="$(git -C "$ROOT" rev-parse --verify -q "$REF^{commit}")" || fail "$REF is no commit here"
mkdir -p "$OUT" && OUT="$(cd "$OUT" && pwd)"
case "$OUT" in "$HOME"/*) ;; *) fail "OUT must be under $HOME, which Docker's VM shares" ;; esac

if [ -z "${WEALTHDB_DEB_MAINTAINER:-}" ]; then
    email="$(git -C "$ROOT" config user.email || true)"
    case "$email" in
        ?*@users.noreply.github.com) WEALTHDB_DEB_MAINTAINER="SmartPointer AG <$email>" ;;
        *) fail "set WEALTHDB_DEB_MAINTAINER: the git identity has no GitHub noreply address" ;;
    esac
fi
export WEALTHDB_DEB_MAINTAINER

ctx="${WEALTHDB_DEB_DOCKER_CONTEXT:-}"
dock() { if [ -n "$ctx" ]; then docker --context "$ctx" "$@"; else docker "$@"; fi; }
# On arm64, the cross build: the image is arm64 with the x86-64 cross
# compilers, and cgo is pointed at them.
platform=linux/amd64 cross= cc= cxx=
case "$(dock info --format '{{.Architecture}}' 2>/dev/null)" in
    aarch64 | arm64)
        platform=linux/arm64 cross="gcc-x86-64-linux-gnu g++-x86-64-linux-gnu"
        cc=x86_64-linux-gnu-gcc cxx=x86_64-linux-gnu-g++ ;;
esac
image="wealthdb/deb-build:$(wealthdb_image_tag "$ROOT")"
go_image="$(wealthdb_from_ref "$ROOT/wealthdb/Dockerfile")"

echo "build.sh: $image (Ubuntu 22.04, $platform${cross:+, building for x86-64})"
dock build --quiet --provenance=false --platform "$platform" \
    --build-arg GO_IMAGE="$go_image" --build-arg CROSS="$cross" \
    -t "$image" - < "$ROOT/shared/images/deb-build.Dockerfile" >/dev/null

deb="wealthdb_${VERSION}_amd64.deb"
echo "build.sh: building $deb from $(printf '%.12s' "$commit")"
git -C "$ROOT" archive --format=tar --prefix=wealthdb/ "$commit" |
    dock run --rm -i --pull=never --platform "$platform" --user "$(id -u):$(id -g)" \
        -e HOME=/work/home -e GOFLAGS=-mod=readonly -e GOTOOLCHAIN=local \
        -e GOARCH=amd64 -e CGO_ENABLED=1 -e CC_FOR_TARGET="$cc" -e CXX_FOR_TARGET="$cxx" \
        -e WEALTHDB_DEB_MAINTAINER -e VERSION="$VERSION" -e DEB="$deb" \
        -v "$OUT:/out" "$image" sh -euc '
            mkdir -p "$HOME" && tar -xf -
            if [ -n "$CC_FOR_TARGET" ]; then export CC="$CC_FOR_TARGET" CXX="$CXX_FOR_TARGET"; fi
            cd wealthdb
            # The Python and Java every supported release puts at these
            # names once the package'"'"'s dependencies are installed.
            ./configure --engine=native --prefix=/usr PYTHON=/usr/bin/python3 JAVA=/usr/bin/java
            make --no-print-directory build-wealthdb
            make --no-print-directory install DESTDIR=/work/stage LIBEXECDIR=/usr/lib
            WEALTHDB_DEB_ARCH=amd64 shared/deb/package.sh "$VERSION" /work/stage "/out/$DEB"
        '
sum="$(shasum -a 256 "$OUT/$deb" 2>/dev/null || sha256sum "$OUT/$deb")"
echo "build.sh: sha256 $(printf '%s' "$sum" | cut -d' ' -f1)  $deb"
