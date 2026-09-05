#!/usr/bin/env bash
#
# Unit tests for the web component — no Docker and no Metabase
# required. Run via `make test-web` or directly. Sources web/web (which
# only runs web_main when executed, not sourced) and exercises the
# arg-building and snapshot logic, then hands off to
# web/test_provision.py for provision.py's definition helpers. bash 3.2
# compatible (macOS).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Engine stub: web/web resolves WEALTHDB from WEALTHDB_BIN when it is
# SOURCED, so the stub must be exported before the source line. It
# records its argv (one arg per line) for the materialize tests.
cat > "$tmp/engine-stub" <<EOF
#!/bin/sh
printf '%s\n' "\$@" > "$tmp/engine-args"
EOF
chmod +x "$tmp/engine-stub"
export WEALTHDB_BIN="$tmp/engine-stub"

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
check "aggregated row cap"   "MB_AGGREGATED_QUERY_ROW_LIMIT=100000"             "$joined"
check "unaggregated row cap" "MB_UNAGGREGATED_QUERY_ROW_LIMIT=100000"           "$joined"
check "image is last"     "wealthdb/metabase:latest"                            "$joined"

echo "== _snapshot .wal guard =="
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

echo "== _materialize_returns (stubbed engine) =="
unset WEALTHDB_CONFIG
rm -f "$tmp/engine-args"
_materialize_returns 2>/dev/null
args="$(cat "$tmp/engine-args" 2>/dev/null)"
check     "invokes web-materialize"       "web-materialize" "$args"
check_not "no -c without WEALTHDB_CONFIG" "-c"              "$args"
WEALTHDB_CONFIG="/cfg/wealthdb.cfg"
rm -f "$tmp/engine-args"
_materialize_returns 2>/dev/null
args="$(cat "$tmp/engine-args" 2>/dev/null)"
check "passes -c with WEALTHDB_CONFIG" "-c"                "$args"
check "passes the configured path"     "/cfg/wealthdb.cfg" "$args"
check "still invokes web-materialize"  "web-materialize"   "$args"
unset WEALTHDB_CONFIG

echo "== refresh wiring: materialize runs, and before the snapshot =="
# Spy on the two steps in a subshell so the function overrides don't
# leak into later tests. _load_config and the restart branch are
# stubbed out (they need Docker); the assertion is purely about
# web_refresh's ordering: returns materialization, THEN the snapshot.
seq_file="$tmp/refresh-seq"
: > "$seq_file"
(
    _load_config()          { GOLD_DB="$tmp/gold.db"; }
    _materialize_returns()  { echo materialize >> "$seq_file"; }
    _snapshot()             { echo snapshot    >> "$seq_file"; }
    _mtime()                { echo now; }
    _is_running()           { return 1; }
    web_refresh >/dev/null 2>&1
)
if [ "$(printf '%s' "$(cat "$seq_file")")" = "materialize
snapshot" ]; then pass "web_refresh materializes, then snapshots"
else fail "web_refresh materializes, then snapshots"; printf '       got sequence: %s\n' "$(tr '\n' ' ' < "$seq_file")"; fi

echo "== web help smoke =="
hout="$(web_help)"
check "help mentions snapshot" "read-only SNAPSHOT" "$hout"
check "help lists refresh"     "refresh"            "$hout"
check "help mentions returns"  "materialize returns" "$hout"

# provision.py's definitions (models, cards, filters, dashboards) are
# pure functions of module constants, so they assert statically. The
# python script prints the same ok/FAIL lines and exits non-zero on
# failure; its own summary line is dropped in favour of this file's.
echo
if command -v python3 >/dev/null 2>&1; then
    prov_out="$(python3 "$HERE/test_provision.py" 2>&1)"
    prov_rc=$?
    printf '%s\n' "$prov_out" | grep -v '^provision tests: '
    if [ "$prov_rc" -ne 0 ]; then
        n="$(printf '%s\n' "$prov_out" | grep -c '^  FAIL ')"
        [ "$n" -gt 0 ] || n=1
        fails=$((fails + n))
    fi
else
    fail "python3 present (web/test_provision.py needs it, as does web/web)"
fi

echo
if [ "$fails" -eq 0 ]; then echo "web tests: all passed"; exit 0
else echo "web tests: $fails failed"; exit 1; fi
