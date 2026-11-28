#!/bin/sh
# test-install.sh DEB [RELEASE...] — install the Debian package DEB with
# apt into a clean x86-64 Ubuntu container of each RELEASE (default 22.04
# 24.04 26.04, the ones it supports), as a person would, and check that
# it runs there:
#   - apt resolves every dependency from Ubuntu's archive;
#   - `wealthdb version` names the engine, which loads its libraries;
#   - the Python and Java the copy recorded at build time exist;
#   - a config that is not set up yet is refused with the setup command;
#   - the package removes cleanly.
# Prints one ok / FAIL line per check and exits 1 when any failed.
#
# It needs an x86-64 machine: Rosetta runs the package's apt steps, but
# not its engine. The release check runs it (release-check.yml).
# WEALTHDB_DEB_DOCKER_CONTEXT is the Docker context, as for build.sh.
# DEB lives under the home folder, which Docker's VM shares.
set -eu

[ "$#" -ge 1 ] || { echo "usage: test-install.sh DEB [RELEASE...]" >&2; exit 2; }
DEB="$1"
shift
[ -f "$DEB" ] || { echo "test-install.sh: $DEB is no file" >&2; exit 2; }
DEB="$(cd "$(dirname "$DEB")" && pwd)/$(basename "$DEB")"
[ "$#" -gt 0 ] || set -- 22.04 24.04 26.04
ctx="${WEALTHDB_DEB_DOCKER_CONTEXT:-}"
dock() { if [ -n "$ctx" ]; then docker --context "$ctx" "$@"; else docker "$@"; fi; }

failed=0
for release in "$@"; do
    echo "Ubuntu $release:"
    if ! dock run --rm --platform linux/amd64 -v "$DEB:/tmp/wealthdb.deb:ro" "ubuntu:$release" sh -euc '
        ok()  { echo "  ok   $1"; }
        bad() { echo "  FAIL $1"; fails=$((fails + 1)); }
        fails=0
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -q >/dev/null
        if apt-get install -y -q /tmp/wealthdb.deb >/tmp/apt.log 2>&1; then ok "apt installs it, with its dependencies"
        else bad "apt install: $(tail -n 3 /tmp/apt.log | tr "\n" " ")"; exit 1; fi
        out="$(wealthdb version 2>&1)" && ok "wealthdb version: $out" || bad "wealthdb version: $out"
        cfg=/usr/lib/wealthdb/config.mk
        for tool in PYTHON JAVA; do
            p="$(sed -n "s/^$tool=//p" "$cfg")"
            if [ -n "$p" ] && [ -x "$p" ]; then ok "the recorded $tool is here ($p)"; else bad "the recorded $tool ($p) is not here"; fi
        done
        useradd -m someone
        out="$(su someone -c "wealthdb -c /home/someone/try/wealthdb.cfg status" 2>&1)" && rc=0 || rc=$?
        case "$rc $out" in
            2*"Not set up yet. Run: wealthdb -c /home/someone/try/wealthdb.cfg config"*) ok "a config not set up names the setup" ;;
            *) bad "status on no config: exit $rc: $out" ;;
        esac
        if apt-get remove -y -q wealthdb >/dev/null 2>&1 && [ ! -e /usr/bin/wealthdb ] && [ ! -e /usr/lib/wealthdb ]; then
            ok "it removes cleanly"
        else bad "the removal left files"; fi
        exit "$fails"
    '; then
        failed=1
    fi
done
[ "$failed" = 0 ] && echo "test-install.sh: all passed" || echo "test-install.sh: FAILED"
exit "$failed"
