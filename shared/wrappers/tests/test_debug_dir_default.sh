#!/usr/bin/env bash
# Behaviour test: the debug/trace cache defaults follow the XDG Base
# Directory spec — ${XDG_CACHE_HOME:-~/.cache}/wealthdb/debug/<name> —
# namespaced under wealthdb/ like the startupCache, and resolved
# identically by the docker wrappers (wrapper-lib) and the host-venv
# wrappers (host-lib) so a hybrid collector's halves agree.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PASS=0
FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ok   $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL $1"; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Resolve HOST_DEBUG through wrapper_init in a subshell, printing the
# result. $1 names the lib to exercise: wrapper (docker) or host.
resolve_debug() {
    (
        NAME=testcol; ENV_PREFIX=TESTCOL; ENV_VARS=()
        unset WEALTHDB_SECRETS_DIR WEALTHDB_DATA_ROOT TESTCOL_DEBUG_DIR
        case "$1" in
            wrapper)
                HAS_DEBUG=1
                # shellcheck disable=SC1091
                source "$HERE/../wrapper-lib.sh"
                wrapper_init
                printf '%s\n' "$HOST_DEBUG" ;;
            host)
                # shellcheck disable=SC1091
                source "$HERE/../host-lib.sh"
                host_resolve_dirs load
                printf '%s\n' "$DEBUG_DIR" ;;
        esac
    )
}

echo "debug-dir default (XDG):"

export XDG_CACHE_HOME="$TMP/xdgcache"
want="$TMP/xdgcache/wealthdb/debug/testcol"

if [[ "$(resolve_debug wrapper)" == "$want" ]]; then
    ok "wrapper-lib honours \$XDG_CACHE_HOME"
else
    bad "wrapper-lib: got $(resolve_debug wrapper), want $want"
fi

if [[ "$(resolve_debug host)" == "$want" ]]; then
    ok "host-lib resolves the same dir"
else
    bad "host-lib: got $(resolve_debug host), want $want"
fi

unset XDG_CACHE_HOME
export HOME="$TMP/home"
want="$TMP/home/.cache/wealthdb/debug/testcol"

if [[ "$(resolve_debug wrapper)" == "$want" ]]; then
    ok "unset \$XDG_CACHE_HOME falls back to ~/.cache"
else
    bad "fallback: got $(resolve_debug wrapper), want $want"
fi

echo
echo "debug-dir default tests: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
