#!/usr/bin/env bash
#
# Unit tests for the web component — no Docker and no Metabase
# required. Run via `make test-web` or directly. Sources web/web (which
# only runs web_main when executed, not sourced) and exercises the
# arg-building and snapshot logic, then hands off to
# web/test_provision.py for the provisioning definitions, under
# WEALTHDB_WEB_PYTHON, else python3: it needs the duckdb module, which
# `make test-web` provides in web/.venv. bash 3.2 compatible (macOS).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Engine stub: web/web resolves WEALTHDB from WEALTHDB_BIN when it is
# SOURCED, so the stub must be exported before the source line. It
# records its argv (one arg per line) for the materialize and config
# tests, and prints $tmp/engine-out where that file exists.
cat > "$tmp/engine-stub" <<EOF
#!/bin/sh
printf '%s\n' "\$@" > "$tmp/engine-args"
cat "$tmp/engine-out" 2>/dev/null || true
EOF
chmod +x "$tmp/engine-stub"
export WEALTHDB_BIN="$tmp/engine-stub"
# The knobs resolve under $tmp, never in a real web data directory.
export WEALTHDB_WEB_DATA_DIR="$tmp/web-data"

# shellcheck source=/dev/null
source "$HERE/web"

fails=0
pass()  { printf '  ok   %s\n' "$1"; }
fail()  { printf '  FAIL %s\n' "$1"; fails=$((fails + 1)); }
check() { # desc, expected-substring, actual
    if grep -qF -- "$2" <<<"$3"; then pass "$1"
    else fail "$1"; printf '       wanted substring: %s\n       in: %s\n' "$2" "$3"; fi
}
check_re() { # desc, extended regex, actual
    if grep -Eq -- "$2" <<<"$3"; then pass "$1"
    else fail "$1"; printf '       wanted pattern: %s\n       in: %s\n' "$2" "$3"; fi
}
check_not() { # desc, unwanted-substring, actual
    if grep -qF -- "$2" <<<"$3"; then fail "$1"; else pass "$1"; fi
}
check_eq() { # desc, expected, actual
    if [ "$3" = "$2" ]; then pass "$1"
    else fail "$1"; printf '       wanted: %s\n       got:    %s\n' "$2" "$3"; fi
}

# A match on the first line of an input longer than grep's first read:
# grep quits there, and a pipe into it would read as failed under pipefail.
check "a long input does not break check" "needle" "needle
$(seq 1 300000)"

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
# In a subshell: the paths stand for a real data directory, and no
# later step may write beside them.
joined="$(
    PORT=3000 BIND=both IMAGE=wealthdb/metabase:latest CONTAINER=wealthdb-metabase \
        H2_DIR=/data/web/metabase SNAP=/data/web/snapshot/wealthdb.db \
        SPILL_DIR=/data/web/spill
    _docker_run_args
    printf '%s' "${DOCKER_ARGS[*]}"
)"
check "detached"          "-d"                                                  "$joined"
check "container name"    "--name wealthdb-metabase"                            "$joined"
check "memory cap"        "--memory 8g"                                         "$joined"
check "H2 volume"         "/data/web/metabase:/metabase-data"                   "$joined"
check "snapshot dir RO mount" "/data/web/snapshot:/gold:ro"                     "$joined"
check "spill mount"       "/data/web/spill:/spill"                              "$joined"
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

echo "== _load_config (stubbed engine) =="
printf 'WEALTHDB_WEB_ENABLED=1\nWEALTHDB_WEB_PORT=3100\nWEALTHDB_GOLD_DB=%s\n' "'$tmp/gold.db'" > "$tmp/engine-out"
cfg_out="$(
    docker() { :; }
    _load_config && printf '%s %s %s\n' "$ENABLED" "$PORT" "$GOLD_DB"
    tr '\n' ' ' < "$tmp/engine-args"; echo
    WEALTHDB_CONFIG="/cfg/wealthdb.cfg" _load_config
    tr '\n' ' ' < "$tmp/engine-args"
)"
check_eq "it asks the engine for config-env web and reads the settings back" \
    "1 3100 $tmp/gold.db
config-env web 
-c /cfg/wealthdb.cfg config-env web " "$cfg_out"
rm -f "$tmp/engine-out"

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
    _provision()   { echo PROVISIONED; PROV_RC=0 PROV_SERVED=0 PROV_LOGIN="log in as a@example.test (password in /pw/admin-password.txt)"; }
    web_stop()     { echo STOPPED; }
    PORT=3100
    web_refresh
) >"$seq_same" 2>&1
check "unchanged gold skips the snapshot" "web: snapshot unchanged" "$(cat "$seq_same")"
check_eq "...and ends with the login and the password's file" \
    "web: http://127.0.0.1:3100/ — log in as a@example.test (password in /pw/admin-password.txt)" \
    "$(tail -n 1 "$seq_same")"
check_not "unchanged gold does not copy" "SNAPSHOT_RAN" "$(cat "$seq_same")"
check "unchanged gold still provisions" "PROVISIONED" "$(cat "$seq_same")"
check_not "unchanged gold does not restart" "STOPPED" "$(cat "$seq_same")"

echo "== a marker in another form recreates the container on an unchanged gold =="
# A container of another version mounts the snapshot file, and its
# marker holds the bare inode. Pointed at a serving name, it cannot read
# it, so even an unchanged refresh recreates it.
seq_oldmark="$tmp/refresh-oldmark"
(
    _load_config() { GOLD_DB="$tmp/gold.db"; SNAP="$tmp/snap/wealthdb.db"; }
    _load_config
    _stat st_ino "$SNAP" > "$SNAP.served"
    _materialize() { :; }
    _snapshot()    { echo SNAPSHOT_RAN; }
    _mtime()       { echo now; }
    _is_running()  { return 0; }
    _provision()   { PROV_RC=1 PROV_SERVED=0 PROV_LINKED=1; }
    web_stop()     { echo STOPPED; }
    web_start()    { echo STARTED; }
    web_refresh
) >"$seq_oldmark" 2>&1
check_not "an old marker is no unchanged snapshot" "web: snapshot unchanged" "$(cat "$seq_oldmark")"
check_not "...the snapshot is not copied"          "SNAPSHOT_RAN"            "$(cat "$seq_oldmark")"
check     "...and the container is recreated"      "STARTED"                 "$(cat "$seq_oldmark")"

echo "== an unchanged refresh that confirms the snapshot prunes kept names =="
rm -rf "$tmp/keep" && mkdir -p "$tmp/keep" && : > "$tmp/keep/wealthdb.db" && : > "$tmp/keep/old.db"
ln "$tmp/keep/old.db" "$tmp/keep/wealthdb-1.db"
(
    _load_config() { GOLD_DB="$tmp/gold.db"; SNAP="$tmp/keep/wealthdb.db"; }
    _load_config
    python3 -c 'import os,sys; os.utime(sys.argv[1], None)' "$SNAP"
    _mark_snapshot_served
    _materialize() { :; }
    _is_running()  { return 0; }
    _provision()   { PROV_RC=0 PROV_SERVED=1 PROV_LINKED=1; ln -f "$SNAP" "$(dirname "$SNAP")/$(_serving_name)"; }
    web_refresh >/dev/null 2>&1
)
check_eq "only the current serving name is left" \
    "old.db wealthdb-$(_stat st_ino "$tmp/keep/wealthdb.db")-$(_stat st_mtime_ns "$tmp/keep/wealthdb.db").db wealthdb.db wealthdb.db.served" \
    "$(cd "$tmp/keep" && ls | tr '\n' ' ' | sed 's/ $//')"

echo "== the serving name tells apart copies that share an inode number =="
# A freed inode number may name a later copy. That copy is newer, so
# its name differs.
mkdir -p "$tmp/reuse" && : > "$tmp/reuse/wealthdb.db"
python3 -c 'import os,sys; os.utime(sys.argv[1], ns=(1000, 1000))' "$tmp/reuse/wealthdb.db"
first="$(SNAP="$tmp/reuse/wealthdb.db" _serving_name)"
python3 -c 'import os,sys; os.utime(sys.argv[1], ns=(2000, 2000))' "$tmp/reuse/wealthdb.db"
check_eq "the name carries the inode and the mtime" \
    "wealthdb-$(_stat st_ino "$tmp/reuse/wealthdb.db")-1000.db" "$first"
check_eq "...so the same inode with a newer mtime is a new name" \
    "wealthdb-$(_stat st_ino "$tmp/reuse/wealthdb.db")-2000.db" "$(SNAP="$tmp/reuse/wealthdb.db" _serving_name)"

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

# swap_refresh PROVISION_STATUS SERVED: web_refresh on a running
# container whose marker names an older snapshot than the one in place
# (a copy taken by this refresh, or by one that died before the server
# took it up), with _provision stubbed to set PROV_RC and PROV_SERVED.
# The snapshot directory holds the old snapshot's serving name, which
# links the old inode. Prints refresh's output and its status; the stub
# records the snapshot inode it saw in $tmp/swap/provisioned.
swap_refresh() {
    rm -rf "$tmp/swap" && mkdir -p "$tmp/swap"
    : > "$tmp/swap/wealthdb.db"
    (
        prov_rc="$1" prov_served="$2"
        _load_config() { GOLD_DB="$tmp/gold.db"; SNAP="$tmp/swap/wealthdb.db"; }
        _load_config
        _mark_snapshot_served
        ln "$SNAP" "$tmp/swap/$(_serving_name)"
        cp "$SNAP" "$SNAP.new" && mv "$SNAP.new" "$SNAP"
        _materialize() { :; }
        _snapshot()    { echo SNAPSHOT_RAN; }
        _mtime()       { echo now; }
        _is_running()  { return 0; }
        # "link" stands for a link _provision cannot make: it fails
        # before provision.py runs, PROV_RC still 0.
        _provision()   { _stat st_ino "$SNAP" > "$tmp/swap/provisioned"
                         if [ "$prov_rc" = link ]; then PROV_RC=0 PROV_SERVED=0 PROV_LINKED=0; return 1; fi
                         PROV_RC="$prov_rc" PROV_SERVED="$prov_served" PROV_LINKED=1
                         PROV_LOGIN="log in as a@example.test (password in /pw/admin-password.txt)"; }
        # refresh sends the stop's output to /dev/null.
        web_stop()     { echo STOPPED >&2; }
        web_start()    { echo STARTED; }
        PORT=3100
        web_refresh
        echo "rc=$?"
    ) 2>&1
}

echo "== a running container takes up a swapped-in snapshot without a restart =="
out="$(swap_refresh 0 1)"
check     "the new snapshot is provisioned"  "$(_stat st_ino "$tmp/swap/wealthdb.db")" "$(cat "$tmp/swap/provisioned")"
check_not "...with no copy"                  "SNAPSHOT_RAN" "$out"
check_not "...no stop"                       "STOPPED"      "$out"
check_not "...and no start"                  "STARTED"      "$out"
check     "...and refresh succeeds"          "rc=0"         "$out"
check_eq  "...ending with the login and the password's file" \
          "web: http://127.0.0.1:3100/ — log in as a@example.test (password in /pw/admin-password.txt)" \
          "$(printf '%s\n' "$out" | tail -n 2 | head -n 1)"
check_eq  "the marker names the new snapshot's serving name" \
          "$(SNAP="$tmp/swap/wealthdb.db" _serving_name)" "$(cat "$tmp/swap/wealthdb.db.served")"
check_eq  "the old serving name is removed" "wealthdb.db wealthdb.db.served" \
          "$(cd "$tmp/swap" && ls | grep -v provisioned | tr '\n' ' ' | sed 's/ $//')"

echo "== a confirmed swap passes a stale snapshot's status on =="
out="$(swap_refresh 3 1)"
check     "a stale swap exits 3"      "rc=3"    "$out"
check_not "...without a restart"      "STOPPED" "$out"

echo "== an unconfirmed swap recreates the container =="
for case in "1 0" "0 0"; do
    out="$(swap_refresh $case)"
    check "provision ($case): the container is stopped" "STOPPED" "$out"
    check "...and started"                 "STARTED" "$out"
    check "...saying why"                  "did not confirm it reads the new snapshot" "$out"
    check_not "...leaving the login to start's banner" "web: http://127.0.0.1:3100/" "$out"
done
echo "== a serving name that cannot be linked keeps the running container =="
out="$(swap_refresh link 0)"
check_not "the container is not stopped" "STOPPED" "$out"
check_not "...nor started"               "STARTED" "$out"
check     "...saying what serves"        "keeps serving the previous snapshot" "$out"
check     "...and refresh fails"         "rc=1"    "$out"
check_eq  "...with the marker on the old snapshot, so the next refresh tries again" \
          "$(cat "$tmp/swap/wealthdb.db.served")" \
          "$(cd "$tmp/swap" && ls wealthdb-*.db)"
out="$(WEALTHDB_WEB_NO_PROVISION=1 swap_refresh 0 1)"
check     "with provisioning off, the container is recreated" "STARTED" "$out"
if [ -f "$tmp/swap/provisioned" ]; then fail "...without provisioning first"; else pass "...without provisioning first"; fi

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
    _provision()   { PROV_RC=3 PROV_SERVED=0; return 0; }
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
mkdir -p "$tmp/prov" && : > "$tmp/prov/wealthdb.db"
(
    python3() {
        case "$1" in *provision.py) ;; *) command python3 "$@"; return ;; esac
        printf '%s\n' "$@" > "$tmp/prov-args"
        printf '%s' "${WEALTHDB_WEB_ADMIN_PASSWORD:-}" > "$tmp/prov-env"
    }
    PORT=3000 DATA_DIR="$tmp" SNAP="$tmp/prov/wealthdb.db" WEALTHDB_WEB_ADMIN_PASSWORD="Synthetic-pw-1!"
    _provision 2>/dev/null
)
prov_ino="$(_stat st_ino "$tmp/prov/wealthdb.db")"
check_not "password not in argv"     "Synthetic-pw-1!" "$(cat "$tmp/prov-args" 2>/dev/null)"
check_not "no --password flag"       "--password"      "$(cat "$tmp/prov-args" 2>/dev/null)"
check     "password in the env var"  "Synthetic-pw-1!" "$(cat "$tmp/prov-env" 2>/dev/null)"
check     "the source colours live beside the web data" "$tmp/source-colors.json" "$(cat "$tmp/prov-args" 2>/dev/null)"
prov_name="$(SNAP="$tmp/prov/wealthdb.db" _serving_name)"
check     "the database reads the snapshot's serving name" "/gold/$prov_name" \
          "$(tr '\n' ' ' < "$tmp/prov-args" 2>/dev/null)"
check     "...spilling under the spill mount" "--spill-dir /spill" "$(tr '\n' ' ' < "$tmp/prov-args" 2>/dev/null)"
check_eq  "...which links the snapshot" "$prov_ino" "$(_stat st_ino "$tmp/prov/$prov_name")"

echo "== _provision reads provision.py's status =="
# prov_status STATUS prints PROV_RC and PROV_SERVED after provision.py
# exits with STATUS.
prov_status() {
    (
        stub_rc="$1"
        python3() { case "$1" in *provision.py) return "$stub_rc" ;; *) command python3 "$@" ;; esac; }
        PORT=3000 DATA_DIR="$tmp" SNAP="$tmp/prov/wealthdb.db" WEALTHDB_WEB_ADMIN_PASSWORD="Synthetic-pw-1!"
        _provision 2>/dev/null
        echo "$PROV_RC $PROV_SERVED"
    )
}
check_eq "a clean provision confirms the serving name"  "0 1" "$(prov_status 0)"
check_eq "...as does a stale snapshot's"                "3 1" "$(prov_status 3)"
check_eq "a failure confirms nothing"                   "1 0" "$(prov_status 1)"
check_eq "an unverified login is no failure, and confirms nothing" "0 0" "$(prov_status 4)"
out="$(
    PORT=3000 DATA_DIR="$tmp" SNAP="$tmp/prov/missing.db" WEALTHDB_WEB_ADMIN_PASSWORD="Synthetic-pw-1!"
    python3() { if [ "${1##*/}" = provision.py ]; then echo PROVISION_RAN; else command python3 "$@"; fi; }
    _provision 2>&1; echo "rc=$? served=$PROV_SERVED"
)"
check     "a link it cannot make stops it" "could not link the snapshot" "$out"
check     "...and fails, confirming nothing" "rc=1 served=0"            "$out"
check_not "...before provision.py runs"    "PROVISION_RAN"              "$out"

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
rc="$(start_with 4)"
check_eq  "an unverified login leaves start's status 0"  "0"            "$rc"
check     "...and prints the login"        "log in as"     "$(cat "$tmp/start-err")"
rc="$(start_with 0)"
check_eq  "a clean provision starts with status 0"  "0"                 "$rc"
check     "...and prints the login"        "log in as"     "$(cat "$tmp/start-err")"
check_re  "...as the banner's last line"   "^  log in as admin@wealthdb.local \(password from .WEALTHDB_WEB_ADMIN_PASSWORD\)$" \
          "$(tail -n 1 "$tmp/start-err")"
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
    DATA_DIR="$tmp/no-such-dir/web" SNAP="$tmp/prov/wealthdb.db"
    unset WEALTHDB_WEB_ADMIN_PASSWORD
    python3() { if [ "${1##*/}" = provision.py ]; then echo PROVISION_RAN; else command python3 "$@"; fi; }
    _provision 2>&1; echo "rc=$?"
)"
check     "it says so"                 "could not save $tmp/no-such-dir/web/admin-password.txt" "$prov_err"
check     "...and fails"               "rc=1"          "$prov_err"
check_not "...before provision.py runs" "PROVISION_RAN" "$prov_err"

echo "== provisioning stops on a password it cannot generate =="
prov_err="$(
    DATA_DIR="$tmp/gen-fail" SNAP="$tmp/prov/wealthdb.db"
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
check_eq "start records the serving name of the snapshot it serves" \
    "$(SNAP="$tmp/start/snapshot/wealthdb.db" _serving_name)" \
    "$(cat "$tmp/start/snapshot/wealthdb.db.served" 2>/dev/null)"
# start_spill: web_start over a spill directory that holds a killed
# DuckDB's leftovers. Prints what the directory holds when the server
# comes up. EXISTS is 1 for a container start cannot remove.
start_spill() {
    rm -rf "$tmp/spill" && mkdir -p "$tmp/spill/spill/wealthdb-1-2.db.tmp" "$tmp/spill/spill/other"
    : > "$tmp/spill/spill/wealthdb-1-2.db.tmp/duckdb_temp_storage-0.tmp"
    (
        exists="$1"
        _load_config() { ENABLED=1; PORT=3000; GOLD_DB="$tmp/gold.db"; }
        _is_running()  { return 1; }
        _exists()      { [ "$exists" = 1 ]; }
        docker()       { case "$1" in run) ls "$SPILL_DIR" | tr '\n' ' ' > "$tmp/spill/at-run" ;; esac; }
        DATA_DIR="$tmp/spill" H2_DIR="$tmp/spill/metabase" SPILL_DIR="$tmp/spill/spill"
        SNAP="$tmp/spill/snapshot/wealthdb.db" BIND=v4 IMAGE=img CONTAINER=wealthdb-metabase
        WEALTHDB_WEB_NO_PROVISION=1
        mkdir -p "$(dirname "$SNAP")" && : > "$SNAP"
        web_start >/dev/null 2>&1
        sed 's/ $//' "$tmp/spill/at-run"
    )
}
check_eq "start clears a killed DuckDB's spill directory before the server comes up" \
    "other" "$(start_spill 0)"
check_eq "...but not while a container it could not remove remains" \
    "other wealthdb-1-2.db.tmp" "$(start_spill 1)"
# start_links SERVED: web_start over a snapshot directory that holds a
# serving name of an older snapshot, with _provision stubbed to set
# PROV_SERVED. Prints the inodes the names link when the server comes
# up and after start, as "NAME:INODE" pairs.
start_links() {
    rm -rf "$tmp/links" && mkdir -p "$tmp/links/snapshot"
    (
        prov_served="$1"
        SNAP="$tmp/links/snapshot/wealthdb.db"
        : > "$SNAP.old"
        ln "$SNAP.old" "$tmp/links/snapshot/wealthdb-1.db"
        : > "$SNAP"
        _load_config() { ENABLED=1; PORT=3000; GOLD_DB="$tmp/gold.db"; }
        _is_running()  { return 1; }
        _exists()      { return 1; }
        names() { for f in "$tmp/links/snapshot"/wealthdb-*.db; do
                      [ -e "$f" ] && printf '%s:%s ' "${f##*/}" "$(_stat st_ino "$f")"; done; }
        docker()       { echo "at run: $(names)" > "$tmp/links/at-run"; }
        _provision()   { PROV_RC=0 PROV_SERVED="$prov_served" PROV_LINKED=1; ln -f "$SNAP" "$(dirname "$SNAP")/$(_serving_name)"; }
        _start_banner() { :; }
        DATA_DIR="$tmp/links" H2_DIR="$tmp/links/metabase" SPILL_DIR="$tmp/links/spill"
        BIND=v4 IMAGE=img CONTAINER=wealthdb-metabase
        web_start 2>/dev/null
        cat "$tmp/links/at-run"
        echo "after: $(names)"
    )
}
out="$(start_links 1)"
cur="$(_stat st_ino "$tmp/links/snapshot/wealthdb.db")"
check    "start links an older serving name to the current snapshot" "at run: wealthdb-1.db:$cur" "$out"
check_eq "...and keeps only the current one once provisioning confirms it" \
         "after: $(SNAP="$tmp/links/snapshot/wealthdb.db" _serving_name):$cur" "$(echo "$out" | grep '^after' | sed 's/ $//')"
out="$(start_links 0)"
cur="$(_stat st_ino "$tmp/links/snapshot/wealthdb.db")"
check    "unconfirmed, start keeps the older name, on the current snapshot" "wealthdb-1.db:$cur" \
         "$(echo "$out" | grep '^after')"
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
    _provision()   { PROV_RC=1 PROV_SERVED=0 PROV_LINKED=1; }
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
    _provision()   { PROV_RC=1 PROV_SERVED=0 PROV_LINKED=1; }
    web_stop()     { :; }
    web_start()    { :; }
    web_refresh 2>&1
)"
check_re "the config read is timed" "web: timing: config [0-9]+\.[0-9]{3}s" "$out"
check_re "...and the stop"          "web: timing: stop [0-9]+\.[0-9]{3}s"   "$out"

echo "== an env file that does not parse stops web, and its line is not shown =="
# bash -n quotes the offending line, and in the env file that line may
# hold the admin password.
printf 'WEALTHDB_WEB_ADMIN_PASSWORD=Synthetic-marker-7)\n' > "$tmp/bad-web.env"
out="$( (WEALTHDB_WEB_ENV_FILE="$tmp/bad-web.env" web_main status) 2>&1)"; rc=$?
check_eq  "a bad env file exits 1"           "1"                          "$rc"
check     "the message names the file"       "$tmp/bad-web.env has a bash syntax error" "$out"
check_not "the message hides the bad line"   "Synthetic-marker-7"         "$out"
# With no WEALTHDB_WEB_ENV_FILE, the file is wealthdb-web.env in WEALTHDB_SECRETS_DIR.
mkdir -p "$tmp/secrets-alt"
printf 'if then\n' > "$tmp/secrets-alt/wealthdb-web.env"
out="$( (unset WEALTHDB_WEB_ENV_FILE; WEALTHDB_SECRETS_DIR="$tmp/secrets-alt" web_main status) 2>&1)"
case "$out" in
    *"$tmp/secrets-alt/wealthdb-web.env has a bash syntax error"*) pass "the env file follows WEALTHDB_SECRETS_DIR" ;;
    *) fail "the env file does not follow WEALTHDB_SECRETS_DIR: $out" ;;
esac

echo "== web help smoke =="
hout="$(web_help)"
check "help mentions snapshot" "read-only SNAPSHOT" "$hout"
check "help lists refresh"     "refresh"            "$hout"
check "help mentions serving"  "re-materialize the serving and report tables" "$(echo "$hout" | tr -s ' \n' ' ')"
check "help says refresh copies a changed gold" "snapshot when gold changed" "$(echo "$hout" | tr -s ' \n' ' ')"
# The banner names the dashboards by hand, so a dashboard added to or
# renamed in DASHBOARDS (dashboards/__init__.py) must be named here too.
prov_python="${WEALTHDB_WEB_PYTHON:-python3}"
want="$("$prov_python" -c 'import sys; sys.path.insert(0, sys.argv[1]); import dashboards; print(" / ".join(d.name for d in dashboards.DASHBOARDS))' "$HERE" 2>&1)"
bout="$(CONTAINER=c PORT=1 _start_banner 2>&1 | tr -s ' \n' ' ')"
check_eq "the start banner names every base dashboard, in DASHBOARDS' order" "$want" \
    "$(printf '%s' "$bout" | sed -n 's/.*questions and the \(.*\) dashboards, most .*/\1/p')"

# web/web reads provision.py's exit statuses by number.
check_eq "web/web's provision statuses are provision.py's" \
    "$("$prov_python" -c 'import sys; sys.path.insert(0, sys.argv[1]); import provision as p; print(p.EXIT_STALE_SNAPSHOT, p.EXIT_UNVERIFIED)' "$HERE" 2>&1)" \
    "$PROV_STALE_SNAPSHOT $PROV_UNVERIFIED"

# Everything provisioning builds before it talks to the API is a pure
# function of its declarations, so it asserts statically. The
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
