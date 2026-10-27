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
check_re() { # desc, extended regex, actual
    if printf '%s' "$3" | grep -Eq -- "$2"; then pass "$1"
    else fail "$1"; printf '       wanted pattern: %s\n       in: %s\n' "$2" "$3"; fi
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

# duck_file PATH N writes a stand-in for a DuckDB file: its header with
# the magic and storage number N, then a body that reads GOLD.
duck_file() {
    python3 -c 'import struct, sys
open(sys.argv[1], "wb").write(bytes(8) + b"DUCK" + struct.pack("<Q", int(sys.argv[2])) + b"GOLD")' "$1" "$2"
}

echo "== _snapshot .wal guard =="
duck_file "$tmp/gold.db" "$SNAPSHOT_MAX_STORAGE"
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

echo "== _snapshot keeps gold's mtime where it cannot clone =="
python3 -c 'import os,sys; os.utime(sys.argv[1], (1000000, 1000000))' "$tmp/gold.db"
(
    cp() { case "$1" in -c) return 1 ;; esac; command cp "$@"; }
    _snapshot "$tmp/gold.db" "$tmp/plain.db"
) 2>/dev/null
check_eq "a plain copy carries gold's mtime" "$(_stat st_mtime_ns "$tmp/gold.db")" \
    "$(_stat st_mtime_ns "$tmp/plain.db")"

echo "== _snapshot refuses a format the driver cannot open =="
mkdir -p "$tmp/fmt" && duck_file "$tmp/fmt/old.db" 64
cp "$tmp/fmt/old.db" "$tmp/fmt/snap.db"
duck_file "$tmp/fmt/new.db" 999
fmt_out="$(_snapshot "$tmp/fmt/new.db" "$tmp/fmt/snap.db" 2>&1)"
fmt_rc=$?
if [ "$fmt_rc" -ne 0 ]; then pass "a newer format is refused"; else fail "a newer format is refused"; fi
check "...naming its format and the driver's" "storage format 999; the dashboards read $SNAPSHOT_MAX_STORAGE at most" "$fmt_out"
check "...and the command that rewrites it" "wealthdb compact" "$fmt_out"
check "...and that the old snapshot serves" "previous snapshot keeps serving" "$fmt_out"
check_eq "...leaving the old snapshot" "64" "$(_storage_number "$tmp/fmt/snap.db")"
check_eq "...and no temp file" "" "$(ls "$tmp/fmt" | grep '\.tmp\.')"
check "with no snapshot yet it says none was taken" "No snapshot was taken." \
    "$(_snapshot "$tmp/fmt/new.db" "$tmp/fmt/none.db" 2>&1)"
printf 'not a database' > "$tmp/fmt/junk.db"
if _snapshot "$tmp/fmt/junk.db" "$tmp/fmt/snap.db" 2>/dev/null; then fail "a file that is no DuckDB file is refused"
else pass "a file that is no DuckDB file is refused"; fi
check_eq "...leaving the old snapshot" "64" "$(_storage_number "$tmp/fmt/snap.db")"
if _snapshot "$tmp/fmt/old.db" "$tmp/fmt/older.db" 2>/dev/null; then pass "an older format is copied"
else fail "an older format is copied"; fi
# The engine writes v1.5's format; the compact test names its number.
v15="$(sed -n 's/^const v15Header = \([0-9]*\)$/\1/p' "$HERE/../wealthdb/cmd/wealthdb/cmd_compact_test.go")"
check_eq "the newest format read is the one the engine writes" "$v15" "$SNAPSHOT_MAX_STORAGE"

echo "== _gen_password complexity (Metabase rule) =="
p="$(_gen_password)"
case "$p" in *[A-Z]*) pass "has uppercase";; *) fail "has uppercase";; esac
case "$p" in *[a-z]*) pass "has lowercase";; *) fail "has lowercase";; esac
case "$p" in *[0-9]*) pass "has digit";; *) fail "has digit";; esac
case "$p" in *[!A-Za-z0-9]*) pass "has special";; *) fail "has special";; esac
if [ "${#p}" -ge 12 ]; then pass "length >= 12"; else fail "length >= 12"; fi
gen_out="$(openssl() { return 1; }; _gen_password; echo "rc=$?")"
check_eq "a failing openssl fails it, printing no password" "rc=1" "$gen_out"
gen_out="$(openssl() { echo '+/+/'; }; _gen_password; echo "rc=$?")"
check_eq "...as does one that gives too few letters and digits" "rc=1" "$gen_out"

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

echo "== a failed materialize repeats the engine's error line =="
# An engine that runs a phase, then fails as DuckDB's assertion does:
# its error line, then a stack trace.
cat > "$tmp/engine-fail" <<'EOF'
#!/bin/sh
if read -r _line; then echo "stdin open" >&2; fi
echo "progress on stdout"
echo "timing: serving 1.0s" >&2
echo "wealthdb: MaterializeGains: INTERNAL Error: an assertion" >&2
echo "Stack Trace:" >&2
echo "  frame 1" >&2
exit 1
EOF
chmod +x "$tmp/engine-fail"
mkdir -p "$tmp/mat-tmp" "$tmp/mat-snap" && : > "$tmp/mat-snap/wealthdb.db"
mat_out="$(echo input | (WEALTHDB="$tmp/engine-fail" SNAP="$tmp/mat-snap/wealthdb.db" TMPDIR="$tmp/mat-tmp" _materialize) 2>"$tmp/mat-err")"
mat_rc=$?
mat_err="$(cat "$tmp/mat-err")"
if [ "$mat_rc" -eq 1 ]; then pass "it returns the engine's status"; else fail "it returns the engine's status (got $mat_rc)"; fi
check "the engine's stdout passes through" "progress on stdout" "$mat_out"
check "the engine's stderr passes through" "timing: serving 1.0s" "$mat_err"
check_not "...and stays apart from stdout" "INTERNAL Error" "$mat_out"
check_not "the engine reads no stdin, so its wrapper gives the container no terminal" "stdin open" "$mat_err"
check "the failure quotes the status and the error line" \
    "web: materialization failed (exit 1): wealthdb: MaterializeGains: INTERNAL Error: an assertion" "$mat_err"
check "...and says the previous snapshot serves" "The previous snapshot keeps serving." "$mat_err"
check_not "...and guesses no cause" "busy" "$mat_err"
check_eq "...and leaves no temp file" "" "$(ls "$tmp/mat-tmp")"
mat_err="$( (WEALTHDB="$tmp/engine-fail" SNAP="$tmp/mat-none/wealthdb.db" TMPDIR="$tmp/mat-tmp" _materialize) 2>&1 >/dev/null)"
check "with no snapshot yet it says none was taken" "No snapshot was taken." "$mat_err"
# web_start runs it under errexit and pipefail: the message still
# prints, with the fallback line, and the status comes back.
printf '#!/bin/sh\necho "timing: serving 1.0s" >&2\nexit 3\n' > "$tmp/engine-quiet" && chmod +x "$tmp/engine-quiet"
mat_err="$( (set -euo pipefail; WEALTHDB="$tmp/engine-quiet" SNAP="$tmp/mat-snap/wealthdb.db" TMPDIR="$tmp/mat-tmp" _materialize; echo NOT_STOPPED) 2>&1)"
mat_rc=$?
if [ "$mat_rc" -eq 3 ]; then pass "under errexit it stops with the engine's status"; else fail "under errexit it stops with the engine's status (got $mat_rc)"; fi
check "...after its message, the last line standing in for an error line" \
    "web: materialization failed (exit 3): timing: serving 1.0s" "$mat_err"
check_not "...and does not run on" "NOT_STOPPED" "$mat_err"
check_eq "...leaving no temp file" "" "$(ls "$tmp/mat-tmp")"
printf 'timing: x\n\nlast words\n\n' > "$tmp/mat-tmp/plain"
check_eq "an output without the engine's name ends in its last line" "last words" "$(_error_line "$tmp/mat-tmp/plain")"
: > "$tmp/mat-tmp/empty"
check_eq "an empty output says so" "(the engine printed nothing)" "$(_error_line "$tmp/mat-tmp/empty")"
rm -f "$tmp/mat-tmp/plain" "$tmp/mat-tmp/empty"

echo "== refresh prints a duration for materialize and the snapshot =="
seq_time="$tmp/refresh-time"
(
    _load_config() { GOLD_DB="$tmp/gold.db"; SNAP="$tmp/refresh-time-snap/wealthdb.db"; }
    _materialize() { :; }
    _snapshot()    { :; }
    _mtime()       { echo now; }
    _is_running()  { return 1; }
    web_refresh
) >"$seq_time" 2>&1 || true
check_re "materialize is timed" "web: timing: materialize [0-9]+\\.[0-9]{3}s" "$(cat "$seq_time")"
check_re "snapshot is timed"    "web: timing: snapshot [0-9]+\\.[0-9]{3}s"    "$(cat "$seq_time")"

echo "== a failed materialize still prints its duration and stops =="
seq_fail="$tmp/refresh-fail"
(
    set -e
    _load_config() { GOLD_DB="$tmp/gold.db"; SNAP="$tmp/refresh-fail-snap/wealthdb.db"; }
    _materialize() { return 7; }
    _snapshot()    { echo SNAPSHOT_RAN; }
    _mtime()       { echo now; }
    _is_running()  { return 1; }
    web_refresh
) >"$seq_fail" 2>&1
fail_rc=$?
check_re "failed materialize is timed" "web: timing: materialize [0-9]+\\.[0-9]{3}s" "$(cat "$seq_fail")"
check_not "snapshot does not run" "SNAPSHOT_RAN" "$(cat "$seq_fail")"
if [ "$fail_rc" -eq 7 ]; then pass "refresh exits with the materialize status"; else fail "refresh exits with the materialize status (got $fail_rc)"; fi

echo "== a failed config read stops refresh =="
seq_cfg="$tmp/refresh-cfg"
(
    set -e
    _load_config() { return 1; }
    _materialize() { echo MATERIALIZE_RAN; }
    web_refresh
) >"$seq_cfg" 2>&1
cfg_rc=$?
check_re "the failed config read is timed" "web: timing: config [0-9]+\\.[0-9]{3}s" "$(cat "$seq_cfg")"
check_not "materialize does not run" "MATERIALIZE_RAN" "$(cat "$seq_cfg")"
if [ "$cfg_rc" -eq 1 ]; then pass "refresh exits 1"; else fail "refresh exits 1 (got $cfg_rc)"; fi

echo "== an unchanged gold skips the snapshot and still provisions =="
seq_same="$tmp/refresh-same"
: > "$tmp/gold.db"
mkdir -p "$tmp/snap" && : > "$tmp/snap/wealthdb.db"
(
    _load_config() { GOLD_DB="$tmp/gold.db"; SNAP="$tmp/snap/wealthdb.db"; }
    _load_config
    _mark_snapshot_served
    _materialize() { :; }
    _snapshot()    { echo SNAPSHOT_RAN; }
    _mtime()       { echo now; }
    _is_running()  { return 0; }
    _provision()   { echo PROVISIONED; }
    web_stop()     { echo STOPPED; }
    web_refresh
) >"$seq_same" 2>&1
check "unchanged gold skips the snapshot" "web: snapshot unchanged" "$(cat "$seq_same")"
check_not "unchanged gold does not copy" "SNAPSHOT_RAN" "$(cat "$seq_same")"
check "unchanged gold still provisions" "PROVISIONED" "$(cat "$seq_same")"
check_not "unchanged gold does not restart" "STOPPED" "$(cat "$seq_same")"

echo "== a snapshot older than gold is copied =="
seq_old="$tmp/refresh-old"
: > "$tmp/gold.db"
mkdir -p "$tmp/snap" && : > "$tmp/snap/wealthdb.db"
python3 -c 'import os,sys; os.utime(sys.argv[1], (1, 1))' "$tmp/snap/wealthdb.db"
(
    _load_config() { GOLD_DB="$tmp/gold.db"; SNAP="$tmp/snap/wealthdb.db"; }
    _materialize() { :; }
    _snapshot()    { echo SNAPSHOT_RAN; }
    _mtime()       { echo now; }
    _is_running()  { return 1; }
    web_refresh
) >"$seq_old" 2>&1
check "an older snapshot is copied" "SNAPSHOT_RAN" "$(cat "$seq_old")"

echo "== a running container on an older inode is restarted without a copy =="
seq_inode="$tmp/refresh-inode"
python3 -c 'import os,sys; os.utime(sys.argv[1], None)' "$tmp/snap/wealthdb.db"
rm -f "$tmp/snap/wealthdb.db.served"
(
    _load_config() { GOLD_DB="$tmp/gold.db"; SNAP="$tmp/snap/wealthdb.db"; }
    _materialize() { :; }
    _snapshot()    { echo SNAPSHOT_RAN; }
    _mtime()       { echo now; }
    _is_running()  { return 0; }
    web_stop()     { echo STOPPED > "$tmp/did-stop"; }
    web_start()    { echo STARTED; }
    web_refresh
) >"$seq_inode" 2>&1
if [ -f "$tmp/did-stop" ]; then pass "an unmarked snapshot restarts"; else fail "an unmarked snapshot restarts"; fi
check "an unmarked snapshot starts" "STARTED" "$(cat "$seq_inode")"
check_not "an unmarked snapshot is not copied" "SNAPSHOT_RAN" "$(cat "$seq_inode")"

echo "== a running container on a swapped-in snapshot is restarted without a copy =="
# A refresh that copied and died before the restart leaves the marker
# naming the inode the container still serves.
seq_swap="$tmp/refresh-swap"
rm -f "$tmp/did-stop"
(
    _load_config() { GOLD_DB="$tmp/gold.db"; SNAP="$tmp/snap/wealthdb.db"; }
    _load_config
    _mark_snapshot_served
    cp "$SNAP" "$SNAP.new" && mv "$SNAP.new" "$SNAP"
    _materialize() { :; }
    _snapshot()    { echo SNAPSHOT_RAN; }
    _mtime()       { echo now; }
    _is_running()  { return 0; }
    web_stop()     { echo STOPPED > "$tmp/did-stop"; }
    web_start()    { echo STARTED; }
    web_refresh
) >"$seq_swap" 2>&1
if [ -f "$tmp/did-stop" ]; then pass "a swapped snapshot restarts"; else fail "a swapped snapshot restarts"; fi
check "a swapped snapshot starts" "STARTED" "$(cat "$seq_swap")"
check_not "a swapped snapshot is not copied" "SNAPSHOT_RAN" "$(cat "$seq_swap")"

echo "== a failed copy keeps the old snapshot =="
# The copy runs under _timed, with errexit off: _snapshot itself must
# fail and clean up. The cp stub leaves a partial file and fails.
seq_torn="$tmp/refresh-torn"
mkdir -p "$tmp/torn" && printf 'OLD' > "$tmp/torn/wealthdb.db"
python3 -c 'import os,sys; os.utime(sys.argv[1], (1, 1))' "$tmp/torn/wealthdb.db"
(
    set -e
    _load_config() { GOLD_DB="$tmp/gold.db"; SNAP="$tmp/torn/wealthdb.db"; }
    _materialize() { :; }
    _mtime()       { echo now; }
    _is_running()  { return 1; }
    cp() { eval "printf PART > \"\${$#}\""; return 1; }
    web_refresh
) >"$seq_torn" 2>&1
torn_rc=$?
if [ "$torn_rc" -ne 0 ]; then pass "a failed copy fails refresh"; else fail "a failed copy fails refresh"; fi
check_eq "...leaves the old snapshot" "OLD" "$(cat "$tmp/torn/wealthdb.db")"
check_eq "...leaves no temp file" "" "$(ls "$tmp/torn" | grep '\.tmp\.')"
check_not "...and reports no update" "snapshot updated" "$(cat "$seq_torn")"

echo "== a missing gold file is not an unchanged snapshot =="
seq_nogold="$tmp/refresh-nogold"
rm -f "$tmp/gold.db"
(
    _load_config() { GOLD_DB="$tmp/gold.db"; SNAP="$tmp/snap/wealthdb.db"; }
    _materialize() { :; }
    _snapshot()    { echo SNAPSHOT_RAN; return 1; }
    _mtime()       { echo now; }
    _is_running()  { return 1; }
    web_refresh
) >"$seq_nogold" 2>&1 || true
check "a missing gold is copied" "SNAPSHOT_RAN" "$(cat "$seq_nogold")"
check_not "a missing gold is not unchanged" "web: snapshot unchanged" "$(cat "$seq_nogold")"
: > "$tmp/gold.db"

echo "== a skipped copy still returns a stale provision status =="
seq_stale="$tmp/refresh-stale"
python3 -c 'import os,sys; os.utime(sys.argv[1], None)' "$tmp/snap/wealthdb.db"
(
    _load_config() { GOLD_DB="$tmp/gold.db"; SNAP="$tmp/snap/wealthdb.db"; }
    _load_config
    _mark_snapshot_served
    _materialize() { :; }
    _snapshot()    { echo SNAPSHOT_RAN; }
    _mtime()       { echo now; }
    _is_running()  { return 0; }
    _provision()   { PROV_RC=3; return 0; }
    web_stop()     { echo STOPPED; }
    web_refresh
) >"$seq_stale" 2>&1
stale_rc=$?
check "an unchanged gold with a stale provision skips the copy" "web: snapshot unchanged" "$(cat "$seq_stale")"
if [ "$stale_rc" -eq 3 ]; then pass "refresh exits with the provision status"; else fail "refresh exits with the provision status (got $stale_rc)"; fi

echo "== refresh wiring: materialize runs, and before the snapshot =="
# Spy on the two steps in a subshell so the function overrides don't
# leak into later tests. _load_config and the restart branch are
# stubbed out (they need Docker); the assertion is purely about
# web_refresh's ordering: materialization, THEN the snapshot.
seq_file="$tmp/refresh-seq"
: > "$seq_file"
(
    _load_config()          { GOLD_DB="$tmp/gold.db"; SNAP="$tmp/refresh-seq-snap/wealthdb.db"; }
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
        python3()      { case "$1" in *provision.py) return "$stub_rc" ;; *) command python3 "$@" ;; esac; }
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
err="$(cat "$tmp/start-err")"
check_re  "...timing the config read"       "web: timing: config [0-9]+\.[0-9]{3}s"          "$err"
check_re  "...the container's start"        "web: timing: container start [0-9]+\.[0-9]{3}s" "$err"
check_re  "...and the provisioning"         "web: timing: provision [0-9]+\.[0-9]{3}s"       "$err"
# After refresh read the config, start does not read it again.
cfg_reads="$(
    _load_config() { echo CONFIG_READ; ENABLED=1; PORT=3000; GOLD_DB="$tmp/gold.db"; }
    _load_config >/dev/null
    CONFIG_LOADED=1
    _is_running()  { return 1; }
    _exists()      { return 1; }
    docker()       { :; }
    _provision()   { PROV_RC=0; }
    _start_banner() { :; }
    DATA_DIR="$tmp/start" H2_DIR="$tmp/start/metabase" SPILL_DIR="$tmp/start/spill"
    SNAP="$tmp/start/snapshot/wealthdb.db" BIND=v4 IMAGE=img CONTAINER=wealthdb-metabase
    web_start 2>/dev/null
)"
check_not "a start after refresh's config read does not read it again" "CONFIG_READ" "$cfg_reads"

echo "== provisioning stops on a password it cannot save =="
# It runs under _timed with errexit off: the step itself must stop it,
# before provision.py creates an admin whose password is saved nowhere.
prov_err="$(
    DATA_DIR="$tmp/no-such-dir/web"
    unset WEALTHDB_WEB_ADMIN_PASSWORD
    python3() { if [ "${1##*/}" = provision.py ]; then echo PROVISION_RAN; else command python3 "$@"; fi; }
    _provision 2>&1; echo "rc=$?"
)"
check     "it says so"                 "could not save $tmp/no-such-dir/web/admin-password.txt" "$prov_err"
check     "...and fails"               "rc=1"          "$prov_err"
check_not "...before provision.py runs" "PROVISION_RAN" "$prov_err"

echo "== provisioning stops on a password it cannot generate =="
prov_err="$(
    DATA_DIR="$tmp/gen-fail"
    mkdir -p "$DATA_DIR"
    unset WEALTHDB_WEB_ADMIN_PASSWORD
    openssl() { return 1; }
    python3() { if [ "${1##*/}" = provision.py ]; then echo PROVISION_RAN; else command python3 "$@"; fi; }
    _provision 2>&1; echo "rc=$?"
)"
check     "it says so"                  "could not generate a password" "$prov_err"
check     "...and fails"                "rc=1"          "$prov_err"
check_not "...before provision.py runs" "PROVISION_RAN" "$prov_err"
if [ -e "$tmp/gen-fail/admin-password.txt" ]; then fail "...saving no password"; else pass "...saving no password"; fi

echo "== a missing docker stops the config read =="
cfg_out="$(
    PATH=/nonexistent
    _load_config 2>&1; echo "rc=$?"
)"
check     "it says docker is missing"   "docker not found" "$cfg_out"
check     "...and fails"                "rc=1"             "$cfg_out"
check_not "...without asking the engine" "could not read settings" "$cfg_out"
# The real python3 this time: start records the inode it mounted.
(
    _load_config() { ENABLED=1; PORT=3000; GOLD_DB="$tmp/gold.db"; }
    _is_running()  { return 1; }
    _exists()      { return 1; }
    docker()       { :; }
    DATA_DIR="$tmp/start" H2_DIR="$tmp/start/metabase" SPILL_DIR="$tmp/start/spill"
    SNAP="$tmp/start/snapshot/wealthdb.db" BIND=v4 IMAGE=img CONTAINER=wealthdb-metabase
    WEALTHDB_WEB_NO_PROVISION=1
    rm -f "$SNAP.served"
    web_start >/dev/null 2>&1
)
check_eq "start records the snapshot inode it serves" \
    "$(_stat st_ino "$tmp/start/snapshot/wealthdb.db")" \
    "$(cat "$tmp/start/snapshot/wealthdb.db.served" 2>/dev/null)"
# A start with no snapshot yet materializes and takes one, timing both.
out="$(
    _load_config() { ENABLED=1; PORT=3000; GOLD_DB="$tmp/gold.db"; }
    _is_running()  { return 1; }
    _exists()      { return 1; }
    docker()       { :; }
    _materialize() { echo MATERIALIZED; }
    _snapshot()    { echo SNAPSHOT_RAN; }
    DATA_DIR="$tmp/first" H2_DIR="$tmp/first/metabase" SPILL_DIR="$tmp/first/spill"
    SNAP="$tmp/first/snapshot/wealthdb.db" BIND=v4 IMAGE=img CONTAINER=wealthdb-metabase
    WEALTHDB_WEB_NO_PROVISION=1
    web_start 2>&1
)"
check    "a first start materializes"   "MATERIALIZED" "$out"
check    "...and takes a snapshot"      "SNAPSHOT_RAN" "$out"
check_re "...timing the materialize"    "web: timing: materialize [0-9]+\\.[0-9]{3}s" "$out"
check_re "...and the snapshot"          "web: timing: snapshot [0-9]+\\.[0-9]{3}s"    "$out"
# Refresh re-snapshots first, so a stale result there means gold itself is
# older than the cards: the restart's failure is refresh's.
rc="$(
    _load_config() { GOLD_DB="$tmp/gold.db"; SNAP="$tmp/refresh-restart-snap/wealthdb.db"; }
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
# A refresh that restarts times the config read (an engine run) and the
# stop. Start times the container's start and the provisioning (the
# start-wiring tests).
out="$(
    _load_config() { GOLD_DB="$tmp/gold.db"; SNAP="$tmp/refresh-restart-snap/wealthdb.db"; }
    _materialize() { :; }
    _snapshot()    { :; }
    _mtime()       { echo now; }
    _is_running()  { return 0; }
    web_stop()     { :; }
    web_start()    { :; }
    web_refresh 2>&1
)"
check_re "the config read is timed" "web: timing: config [0-9]+\.[0-9]{3}s" "$out"
check_re "...and the stop"          "web: timing: stop [0-9]+\.[0-9]{3}s"   "$out"

echo "== web help smoke =="
hout="$(web_help)"
check "help mentions snapshot" "read-only SNAPSHOT" "$hout"
check "help lists refresh"     "refresh"            "$hout"
check "help mentions serving"  "re-materialize the serving and report tables" "$(echo "$hout" | tr -s ' \n' ' ')"
check "help says refresh copies a changed gold" "snapshot when gold changed" "$(echo "$hout" | tr -s ' \n' ' ')"
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
