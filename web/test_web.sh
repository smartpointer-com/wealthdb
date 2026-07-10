#!/usr/bin/env bash
#
# Unit tests for web/web's pure helpers — no Docker required. Run via
# `make test-web` or directly. Sources web/web (which only runs
# web_main when executed, not sourced) and exercises the arg-building
# and snapshot logic. bash 3.2 compatible (macOS).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=/dev/null
source "$HERE/web"

fails=0
pass()  { printf '  ok   %s\n' "$1"; }
fail()  { printf '  FAIL %s\n' "$1"; fails=$((fails + 1)); }
check() { # desc, expected-substring, actual
    if printf '%s' "$3" | grep -qF -- "$2"; then pass "$1"
    else fail "$1"; printf '       wanted substring: %s\n       in: %s\n' "$2" "$3"; fi
}
check_not() { # desc, unwanted-substring, actual
    if printf '%s' "$3" | grep -qF -- "$2"; then fail "$1"; else pass "$1"; fi
}

echo "== _publish_args =="
out="$(_publish_args 3000 both)"
check     "both → IPv4 flag" "127.0.0.1:3000:3000" "$out"
check     "both → IPv6 flag" "[::1]:3000:3000"     "$out"
out="$(_publish_args 4567 v4)"
check     "v4 → IPv4 flag"   "127.0.0.1:4567:3000" "$out"
check_not "v4 → no IPv6"     "[::1]"               "$out"
out="$(_publish_args 4567 v6)"
check     "v6 → IPv6 flag"   "[::1]:4567:3000"     "$out"
check_not "v6 → no IPv4"     "127.0.0.1"           "$out"

echo "== _docker_run_args (dual-stack, read-only snapshot mount) =="
PORT=3000 BIND=both IMAGE=wealthdb/metabase:latest CONTAINER=wealthdb-metabase \
    H2_DIR=/data/web/metabase SNAP=/data/web/snapshot/wealthdb.db \
    SPILL_DIR=/data/web/spill
_docker_run_args
joined="${DOCKER_ARGS[*]}"
check "detached"          "-d"                                                  "$joined"
check "container name"    "--name wealthdb-metabase"                            "$joined"
check "memory cap"        "--memory 8g"                                         "$joined"
check "H2 volume"         "/data/web/metabase:/metabase-data"                   "$joined"
check "snapshot RO mount" "/data/web/snapshot/wealthdb.db:/gold/wealthdb.db:ro" "$joined"
check "spill mount"       "/data/web/spill:/gold/wealthdb.db.tmp"               "$joined"
check "publish IPv4"      "127.0.0.1:3000:3000"                                 "$joined"
check "publish IPv6"      "[::1]:3000:3000"                                     "$joined"
check "image is last"     "wealthdb/metabase:latest"                            "$joined"

echo "== _snapshot .wal guard =="
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
printf 'GOLD' > "$tmp/gold.db"
printf 'WAL'  > "$tmp/gold.db.wal"
if _snapshot "$tmp/gold.db" "$tmp/snap.db" 2>/dev/null; then
    fail "refuses copy when .wal present"
else
    pass "refuses copy when .wal present"
fi
if [ -f "$tmp/snap.db" ]; then fail "no snapshot written when .wal present"
else pass "no snapshot written when .wal present"; fi

echo "== _snapshot happy path =="
rm -f "$tmp/gold.db.wal"
if _snapshot "$tmp/gold.db" "$tmp/snap.db" 2>/dev/null; then pass "copies gold at rest"
else fail "copies gold at rest"; fi
check "snapshot contents" "GOLD" "$(cat "$tmp/snap.db" 2>/dev/null)"

echo "== _gen_password complexity (Metabase rule) =="
p="$(_gen_password)"
case "$p" in *[A-Z]*) pass "has uppercase";; *) fail "has uppercase";; esac
case "$p" in *[a-z]*) pass "has lowercase";; *) fail "has lowercase";; esac
case "$p" in *[0-9]*) pass "has digit";; *) fail "has digit";; esac
case "$p" in *[!A-Za-z0-9]*) pass "has special";; *) fail "has special";; esac
if [ "${#p}" -ge 12 ]; then pass "length >= 12"; else fail "length >= 12"; fi

echo "== web help smoke =="
hout="$(web_help)"
check "help mentions snapshot" "read-only SNAPSHOT" "$hout"
check "help lists refresh"     "refresh"            "$hout"

echo
if [ "$fails" -eq 0 ]; then echo "web tests: all passed"; exit 0
else echo "web tests: $fails failed"; exit 1; fi
