#!/usr/bin/env bash
#
# Unit tests for the web component — no Docker and no Metabase
# required. Run via `make test-web` or directly. Sources web/web (which
# only runs web_main when executed, not sourced) and exercises the
# arg-building and snapshot logic, then hands off to
# web/test_provision.py for provision.py's definition helpers, under
# WEALTHDB_WEB_PYTHON, else python3: it needs the duckdb module, which
# `make test-web` provides in web/.venv. bash 3.2 compatible (macOS).
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
check_eq() { # desc, expected, actual
    if [ "$3" = "$2" ]; then pass "$1"
    else fail "$1"; printf '       wanted: %s\n       got:    %s\n' "$2" "$3"; fi
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

echo "== _resolve_knobs (image tag) =="
WEALTHDB_WEB_IMAGE= WEALTHDB_IMAGE_TAG=feature _resolve_knobs
check "image follows the tag"        "wealthdb/metabase:feature" "$IMAGE"
WEALTHDB_WEB_IMAGE=other/mb:1 WEALTHDB_IMAGE_TAG=feature _resolve_knobs
check "WEALTHDB_WEB_IMAGE wins"      "other/mb:1"                "$IMAGE"

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

echo "== _materialize (stubbed engine) =="
unset WEALTHDB_CONFIG
rm -f "$tmp/engine-args"
_materialize 2>/dev/null
args="$(cat "$tmp/engine-args" 2>/dev/null)"
check     "invokes web-materialize"       "web-materialize" "$args"
check_not "no -c without WEALTHDB_CONFIG" "-c"              "$args"
WEALTHDB_CONFIG="/cfg/wealthdb.cfg"
rm -f "$tmp/engine-args"
_materialize 2>/dev/null
args="$(cat "$tmp/engine-args" 2>/dev/null)"
check "passes -c with WEALTHDB_CONFIG" "-c"                "$args"
check "passes the configured path"     "/cfg/wealthdb.cfg" "$args"
check "still invokes web-materialize"  "web-materialize"   "$args"
unset WEALTHDB_CONFIG

echo "== refresh prints a duration for materialize and the snapshot =="
seq_time="$tmp/refresh-time"
(
    _load_config() { GOLD_DB="$tmp/gold.db"; }
    _materialize() { :; }
    _snapshot()    { :; }
    _mtime()       { echo now; }
    _is_running()  { return 1; }
    web_refresh
) >"$seq_time" 2>&1 || true
check "materialize is timed" "web: timing: materialize " "$(cat "$seq_time")"
check "snapshot is timed"    "web: timing: snapshot "    "$(cat "$seq_time")"

echo "== a failed materialize still prints its duration and stops =="
seq_fail="$tmp/refresh-fail"
(
    set -e
    _load_config() { GOLD_DB="$tmp/gold.db"; }
    _materialize() { return 7; }
    _snapshot()    { echo SNAPSHOT_RAN; }
    _mtime()       { echo now; }
    _is_running()  { return 1; }
    web_refresh
) >"$seq_fail" 2>&1
fail_rc=$?
check "failed materialize is timed" "web: timing: materialize " "$(cat "$seq_fail")"
check_not "snapshot does not run" "SNAPSHOT_RAN" "$(cat "$seq_fail")"
if [ "$fail_rc" -eq 7 ]; then pass "refresh exits with the materialize status"; else fail "refresh exits with the materialize status (got $fail_rc)"; fi

echo "== refresh wiring: materialize runs, and before the snapshot =="
# Spy on the two steps in a subshell so the function overrides don't
# leak into later tests. _load_config and the restart branch are
# stubbed out (they need Docker); the assertion is purely about
# web_refresh's ordering: materialization, THEN the snapshot.
seq_file="$tmp/refresh-seq"
: > "$seq_file"
(
    _load_config()          { GOLD_DB="$tmp/gold.db"; }
    _materialize()          { echo materialize >> "$seq_file"; }
    _snapshot()             { echo snapshot    >> "$seq_file"; }
    _mtime()                { echo now; }
    _is_running()           { return 1; }
    web_refresh >/dev/null 2>&1
)
if [ "$(printf '%s' "$(cat "$seq_file")")" = "materialize
snapshot" ]; then pass "web_refresh materializes, then snapshots"
else fail "web_refresh materializes, then snapshots"; printf '       got sequence: %s\n' "$(tr '\n' ' ' < "$seq_file")"; fi

echo "== _provision: the admin password reaches provision.py by env, never argv =="
# A python3 stub records its argv and the env var; run in a subshell so
# the override does not leak into the provision.py hand-off below.
(
    python3() {
        printf '%s\n' "$@" > "$tmp/prov-args"
        printf '%s' "${WEALTHDB_WEB_ADMIN_PASSWORD:-}" > "$tmp/prov-env"
    }
    PORT=3000 DATA_DIR="$tmp" WEALTHDB_WEB_ADMIN_PASSWORD="Synthetic-pw-1!"
    _provision 2>/dev/null
)
check_not "password not in argv"     "Synthetic-pw-1!" "$(cat "$tmp/prov-args" 2>/dev/null)"
check_not "no --password flag"       "--password"      "$(cat "$tmp/prov-args" 2>/dev/null)"
check     "password in the env var"  "Synthetic-pw-1!" "$(cat "$tmp/prov-env" 2>/dev/null)"

echo "== web start and refresh read provision.py's exit status =="
# web_start against stubs, in a subshell: no Docker, a snapshot already
# in place (so start takes none), and a python3 stub that exits with the
# status under test. Prints start's status; its stderr lands in
# $tmp/start-err and its docker calls in $tmp/docker-calls.
start_with() { # provision.py's exit status
    : > "$tmp/docker-calls"
    (
        stub_rc="$1"
        _load_config() { ENABLED=1; PORT=3000; GOLD_DB="$tmp/gold.db"; }
        _is_running()  { return 1; }
        _exists()      { return 1; }
        docker()       { printf '%s\n' "$*" >> "$tmp/docker-calls"; }
        python3()      { return "$stub_rc"; }
        DATA_DIR="$tmp/start" H2_DIR="$tmp/start/metabase" SPILL_DIR="$tmp/start/spill"
        SNAP="$tmp/start/snapshot/wealthdb.db" BIND=v4 IMAGE=img CONTAINER=wealthdb-metabase
        WEALTHDB_WEB_ADMIN_PASSWORD="Synthetic-pw-1!"
        mkdir -p "$(dirname "$SNAP")" && : > "$SNAP"
        web_start >/dev/null 2> "$tmp/start-err"
        echo "$?"
    )
}
rc="$(start_with 3)"
err="$(cat "$tmp/start-err")"
check_eq  "a stale snapshot fails start with provision.py's status" "3" "$rc"
check_not "...without the browser hint"  "finish setup in the browser"  "$err"
check     "...naming refresh"   "'wealthdb web refresh' re-snapshots a migrated gold" "$err"
check     "...with the login, which stands"  "log in as"                "$err"
check_eq  "...and the server run, never stopped or removed" "run" \
          "$(cut -d' ' -f1 < "$tmp/docker-calls" | tr '\n' ' ' | sed 's/ $//')"
rc="$(start_with 1)"
err="$(cat "$tmp/start-err")"
check_eq  "any other failure leaves start's status 0"  "0"              "$rc"
check     "...and points at the browser"   "finish setup in the browser"  "$err"
check_not "...not at refresh"              "re-snapshots a migrated gold" "$err"
rc="$(start_with 0)"
check_eq  "a clean provision starts with status 0"  "0"                 "$rc"
check     "...and prints the login"        "log in as"     "$(cat "$tmp/start-err")"
# Refresh re-snapshots first, so a stale result there means gold itself is
# older than the cards: the restart's failure is refresh's.
rc="$(
    _load_config() { GOLD_DB="$tmp/gold.db"; }
    _materialize() { :; }
    _snapshot()    { :; }
    _mtime()       { echo now; }
    _is_running()  { return 0; }
    web_stop()     { :; }
    web_start()    { return 3; }
    web_refresh >/dev/null 2>&1
    echo "$?"
)"
check_eq "refresh passes a stale restart's status on" "3" "$rc"

echo "== web help smoke =="
hout="$(web_help)"
check "help mentions snapshot" "read-only SNAPSHOT" "$hout"
check "help lists refresh"     "refresh"            "$hout"
check "help mentions serving"  "materialize serving tables" "$hout"
check "help names the yield tables" "exposure and yield," "$(echo "$hout" | tr -s ' \n' ' ')"
# The banner names the dashboards by hand, so one added to or renamed in
# provision.py must be named here too.
prov_python="${WEALTHDB_WEB_PYTHON:-python3}"
want="$("$prov_python" -c 'import sys; sys.path.insert(0, sys.argv[1]); import provision as p; print(" / ".join(p.base_dashboards()))' "$HERE" 2>&1)"
bout="$(CONTAINER=c PORT=1 _start_banner 2>&1 | tr -s ' \n' ' ')"
check_eq "the start banner names every base dashboard, in provision.py's order" "$want" \
    "$(printf '%s' "$bout" | sed -n 's/.*questions and the \(.*\) dashboards, most .*/\1/p')"

# provision.py's definitions (models, cards, filters, dashboards) are
# pure functions of module constants, so they assert statically. The
# python script prints the same ok/FAIL lines and exits non-zero on
# failure; its own summary line is dropped in favour of this file's.
echo
if command -v "$prov_python" >/dev/null 2>&1; then
    prov_out="$("$prov_python" "$HERE/test_provision.py" 2>&1)"
    prov_rc=$?
    printf '%s\n' "$prov_out" | grep -v '^provision tests: '
    if [ "$prov_rc" -ne 0 ]; then
        n="$(printf '%s\n' "$prov_out" | grep -c '^  FAIL ')"
        [ "$n" -gt 0 ] || n=1
        fails=$((fails + n))
    fi
else
    fail "$prov_python present (web/test_provision.py needs it)"
fi

echo
if [ "$fails" -eq 0 ]; then echo "web tests: all passed"; exit 0
else echo "web tests: $fails failed"; exit 1; fi
