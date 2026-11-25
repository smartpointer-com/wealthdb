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
# No ambient config reaches the script: every case runs the default
# config unless it names another.
unset WEALTHDB_CONFIG WEALTHDB_COLLECT WEALTHDB_PROGRAM

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
# Nothing here reads a real env file, or reaches launchd or a real
# Docker. The secrets folder is an empty one under $tmp, and the servers
# are plain processes. `launchctl` and `docker` on PATH are stubs that
# record their calls: this docker has a container only with
# STUB_CONTAINER set. A test that needs more of Docker defines a
# function of its own.
mkdir -p "$tmp/secrets" "$tmp/bin"
export WEALTHDB_SECRETS_DIR="$tmp/secrets" WEALTHDB_NO_LAUNCHD=1
cat > "$tmp/bin/launchctl" <<EOF
#!/bin/sh
echo "\$*" >> "$tmp/launchctl-calls"
EOF
cat > "$tmp/bin/docker" <<EOF
#!/bin/sh
echo "\$*" >> "$tmp/docker-stub-calls"
case "\$1 \$2" in
    "container inspect") [ -n "\${STUB_CONTAINER:-}" ] || exit 1
                         [ "\$3" != -f ] || printf '%s\n' "\${STUB_MOUNT:-/elsewhere/web/metabase}" ;;
esac
exit 0
EOF
chmod +x "$tmp/bin/launchctl" "$tmp/bin/docker"
export PATH="$tmp/bin:$PATH"

# shellcheck source=/dev/null
# The script under test runs the container engine, whatever ./configure
# recorded for this checkout. The native cases name a record of their own.
export WEALTHDB_BUILD_CONFIG=/dev/null
source "$HERE/web"
# The stubs below stand in for `python3`, whatever Python a ./configure
# of this checkout recorded.
PYTHON3=python3

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
check "never pulls"       "run -d --pull=never"                                 "$joined"
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
    check "...saying why"                  "did not confirm it reads the new snapshot; recreating $CONTAINER" "$out"
    check_not "...leaving the login to start's banner" "web: http://127.0.0.1:3100/" "$out"
done
echo "== a serving name that cannot be linked keeps the running container =="
out="$(swap_refresh link 0)"
check_not "the container is not stopped" "STOPPED" "$out"
check_not "...nor started"               "STARTED" "$out"
check     "...saying what serves"        "web: $CONTAINER keeps serving the previous snapshot." "$out"
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
check_eq  "...the image checked, and the server run, never stopped or removed" "image run" \
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
# With no image, start stops at once: it takes no first snapshot and
# makes no folder.
noimg="$(
    _load_config() { ENABLED=1; PORT=3000; GOLD_DB="$tmp/gold.db"; }
    _is_running()  { return 1; }
    _exists()      { return 1; }
    docker() {
        if [ "$1 $2" = "image inspect" ]; then
            echo "Error response from daemon: No such image: img" >&2
            return 1
        fi
    }
    _materialize() { echo MATERIALIZED; }
    _snapshot()    { echo SNAPSHOT; }
    DATA_DIR="$tmp/noimg" H2_DIR="$tmp/noimg/metabase" SPILL_DIR="$tmp/noimg/spill"
    SNAP="$tmp/noimg/snapshot/wealthdb.db" BIND=v4 IMAGE=img CONTAINER=wealthdb-metabase
    web_start 2>&1; echo "rc=$?"
)"
check     "a start with no image names the build" \
          "web: the image img is not built yet. Build it with: wealthdb web build" "$noimg"
check     "...and fails"                         "rc=1"         "$noimg"
check_not "...before gold is materialized"       "MATERIALIZED" "$noimg"
check_not "...and before the first snapshot"     "SNAPSHOT"     "$noimg"
if [ -e "$tmp/noimg" ]; then fail "...making no folder"; else pass "...making no folder"; fi
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
        # The container that remains is this setup's: it mounts its data.
        docker() {
            case "$1" in
                run)       ls "$SPILL_DIR" | tr '\n' ' ' > "$tmp/spill/at-run" ;;
                container) printf '%s\n' "$H2_DIR" ;;
            esac
        }
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

# ---- a setup of its own -------------------------------------------------
# A config that is not the default one keeps its dashboards apart: the
# container's name, the data folder and the env file follow the config's
# folder, and each override still wins.

# knobs NAME=VALUE...: what _resolve_knobs gives in an environment that
# holds HOME, the config dir /cfg and the pairs given.
knobs() {
    (
        unset WEALTHDB_WEB_DATA_DIR WEALTHDB_WEB_CONTAINER WEALTHDB_WEB_ENV_FILE \
            WEALTHDB_CONFIG WEALTHDB_DATA_ROOT XDG_DATA_HOME
        export HOME=/home/u XDG_CONFIG_HOME=/cfg
        for kv in "$@"; do export "${kv?}"; done
        _resolve_knobs
        printf '%s\n' "container=$CONTAINER" "data=$DATA_DIR" "h2=$H2_DIR" "program=$PROGRAM"
    )
}
echo "== _resolve_knobs: the default config =="
out="$(knobs)"
check_eq "wealthdb-metabase, the XDG data folder and plain commands" \
    "container=wealthdb-metabase
data=/home/u/.local/share/wealthdb/web
h2=/home/u/.local/share/wealthdb/web/metabase
program=wealthdb" "$out"
out="$(knobs XDG_DATA_HOME=/xdg WEALTHDB_DATA_ROOT=/data)"
check    "XDG_DATA_HOME moves the data folder" "data=/xdg/wealthdb/web" "$out"
out="$(knobs WEALTHDB_CONFIG=/cfg/./wealthdb.cfg WEALTHDB_DATA_ROOT=/data)"
check_eq "WEALTHDB_CONFIG on the default config changes nothing" \
    "container=wealthdb-metabase data=/home/u/.local/share/wealthdb/web" "$(printf '%s\n' "$out" | sed -n '1,2p' | tr '\n' ' ' | sed 's/ $//')"
out="$(knobs WEALTHDB_WEB_CONTAINER=mine WEALTHDB_WEB_DATA_DIR=/d)"
check    "WEALTHDB_WEB_CONTAINER names the container" "container=mine" "$out"
check    "WEALTHDB_WEB_DATA_DIR names the data folder" "data=/d" "$out"
# A demo's call: the config dir, the config and the data root are one
# folder, and the names are given.
out="$(knobs XDG_CONFIG_HOME=/srv/demo WEALTHDB_CONFIG=/srv/demo/wealthdb.cfg WEALTHDB_DATA_ROOT=/srv/demo \
    WEALTHDB_WEB_CONTAINER=wealthdb-metabase-demo WEALTHDB_WEB_DATA_DIR=/srv/demo/web)"
check_eq "a demo's call: the names and the folders it gives" \
    "container=wealthdb-metabase-demo
data=/srv/demo/web
h2=/srv/demo/web/metabase
program=wealthdb" "$out"
out="$(knobs XDG_CONFIG_HOME=/srv/demo WEALTHDB_CONFIG=/srv/demo/wealthdb.cfg WEALTHDB_DATA_ROOT=/srv/demo)"
check    "...and with no names given it is the default config's server" "container=wealthdb-metabase" "$out"

echo "== _resolve_knobs: a setup of its own =="
out="$(knobs "WEALTHDB_CONFIG=/home/u/My Try/wealthdb.cfg" WEALTHDB_DATA_ROOT=/data XDG_DATA_HOME=/xdg)"
check_eq "the container is named after the folder, the data is in it, and commands name the config" \
    "container=wealthdb-metabase-my-try
data=/home/u/My Try/web
h2=/home/u/My Try/web/metabase
program=wealthdb -c '/home/u/My Try/wealthdb.cfg'" "$out"
out="$(knobs WEALTHDB_CONFIG=/home/u/TRY/wealthdb.cfg)"
check    "an upper-case folder gives a lower-case name" "container=wealthdb-metabase-try" "$out"
out="$(knobs WEALTHDB_CONFIG=/home/u/try//wealthdb.cfg)"
check    "a doubled slash changes nothing" "data=/home/u/try/web" "$out"
out="$(knobs WEALTHDB_CONFIG=/wealthdb.cfg)"
check    "a config in the root folder: the name is setup" "container=wealthdb-metabase-setup" "$out"
check    "...and the data folder /web" "data=/web" "$out"
out="$(knobs WEALTHDB_CONFIG=/home/u/try/wealthdb.cfg WEALTHDB_WEB_CONTAINER=mine)"
check    "WEALTHDB_WEB_CONTAINER still names the container" "container=mine" "$out"
check    "...and the data folder stays the setup's" "data=/home/u/try/web" "$out"
out="$(knobs WEALTHDB_CONFIG=/home/u/try/wealthdb.cfg WEALTHDB_WEB_DATA_DIR=/d)"
check    "WEALTHDB_WEB_DATA_DIR still names the data folder" "data=/d" "$out"
check    "...and the container stays the setup's" "container=wealthdb-metabase-try" "$out"

echo "== the env file: in the secrets folder, named after a setup of its own =="
# env_marker NAME=VALUE...: the marker the env file that was read sets.
mkdir -p "$tmp/Own Setup" "$tmp/envs"
printf 'ENV_MARKER=the-default-file\n' > "$tmp/secrets/wealthdb-web.env"
printf 'ENV_MARKER=the-file-of-the-setup\n' > "$tmp/secrets/wealthdb-web-own-setup.env"
printf 'ENV_MARKER=a-file-in-the-folder\n' > "$tmp/Own Setup/wealthdb-web.env"
printf 'ENV_MARKER=the-named-file\n' > "$tmp/envs/named.env"
env_marker() {
    (
        unset WEALTHDB_CONFIG WEALTHDB_WEB_ENV_FILE ENV_MARKER
        export XDG_CONFIG_HOME="$tmp/cfg"
        for kv in "$@"; do export "${kv?}"; done
        _source_env_file && echo "${ENV_MARKER:-none}"
    )
}
check_eq "the default config reads wealthdb-web.env in the secrets folder" "the-default-file" "$(env_marker)"
check_eq "a setup of its own reads the file named after its folder, there too" "the-file-of-the-setup" \
    "$(env_marker "WEALTHDB_CONFIG=$tmp/Own Setup/wealthdb.cfg")"
check_eq "...and neither the default file nor one in its own folder" "none" \
    "$(rm -f "$tmp/secrets/wealthdb-web-own-setup.env"; env_marker "WEALTHDB_CONFIG=$tmp/Own Setup/wealthdb.cfg")"
printf 'ENV_MARKER=the-file-of-the-setup\n' > "$tmp/secrets/wealthdb-web-own-setup.env"
mkdir -p "$tmp/secrets-alt"
printf 'ENV_MARKER=the-file-in-the-other-secrets-folder\n' > "$tmp/secrets-alt/wealthdb-web-own-setup.env"
check_eq "WEALTHDB_SECRETS_DIR moves it, as it moves the default file" "the-file-in-the-other-secrets-folder" \
    "$(env_marker "WEALTHDB_CONFIG=$tmp/Own Setup/wealthdb.cfg" "WEALTHDB_SECRETS_DIR=$tmp/secrets-alt")"
check_eq "WEALTHDB_WEB_ENV_FILE wins for a setup of its own" "the-named-file" \
    "$(env_marker "WEALTHDB_CONFIG=$tmp/Own Setup/wealthdb.cfg" "WEALTHDB_WEB_ENV_FILE=$tmp/envs/named.env")"
check_eq "...and for the default config" "the-named-file" "$(env_marker "WEALTHDB_WEB_ENV_FILE=$tmp/envs/named.env")"
rm -f "$tmp/secrets/wealthdb-web.env" "$tmp/secrets/wealthdb-web-own-setup.env" "$tmp/secrets-alt/wealthdb-web-own-setup.env"
mkdir -p "$tmp/own setup"

echo "== a start never takes over another setup's server =="
# guarded MOUNT RUNNING CMD...: CMD against a Docker that holds a
# container of this name, which mounts MOUNT and runs when RUNNING is 1,
# until it is removed. Prints CMD's output and status; the docker calls
# land in $tmp/guard-calls. GUARD_DATA spells the data folder another
# way, and GUARD_CONFIG names a config.
guarded() {
    local mount="$1" running="$2"
    shift 2
    : > "$tmp/guard-calls"
    (
        _load_config() { ENABLED=1; PORT=3000; GOLD_DB="$tmp/gold.db"; CONFIG_LOADED=1; }
        docker() {
            printf '%s\n' "$*" >> "$tmp/guard-calls"
            if grep -q '^rm ' "$tmp/guard-calls"; then return 0; fi
            case "$1 $2" in
                "ps -aq")            echo abc123 ;;
                "ps -q")             if [ "$running" = 1 ]; then echo abc123; fi ;;
                "container inspect") printf '%s\n' "$mount" ;;
            esac
        }
        _provision()    { PROV_RC=0 PROV_SERVED=1; }
        _start_banner() { :; }
        _materialize()  { :; }
        export WEALTHDB_WEB_DATA_DIR="${GUARD_DATA:-$tmp/guard}" WEALTHDB_WEB_BIND=v4
        if [ -n "${GUARD_CONFIG:-}" ]; then export WEALTHDB_CONFIG="$GUARD_CONFIG"; fi
        mkdir -p "$tmp/guard/snapshot" && : > "$tmp/guard/snapshot/wealthdb.db"
        # A subshell: web_main turns errexit on.
        ( "$@" ) 2>&1
        echo "rc=$?"
    )
}
# changed: the calls that stop, remove or start a container.
changed() { grep -E '^(stop|rm|run|kill) ' "$tmp/guard-calls" | cut -d' ' -f1 | tr '\n' ' ' | sed 's/ $//'; }
out="$(guarded /elsewhere/web/metabase 0 web_main start)"
check    "a container that serves another data folder stops the start" "rc=1" "$out"
check    "the message names it, the folder and the way out" \
    "web: wealthdb-metabase is the server of another setup (its files are not in $tmp/guard). Stop that one first, or name this one with WEALTHDB_WEB_CONTAINER." "$out"
check_eq "nothing is stopped, removed or started" "" "$(changed)"
out="$(guarded /elsewhere/web/metabase 1 web_main start)"
check    "one that runs stops it too, with the same message" "is the server of another setup" "$out"
check_not "...and not with the advice to restart" "already running" "$out"
check_eq "...and nothing is changed" "" "$(changed)"
out="$(guarded /elsewhere/web/metabase 1 web_main restart)"
check    "a restart is refused before it stops anything" "rc=1" "$out"
check    "...with the message" "is the server of another setup" "$out"
check_eq "...and nothing is changed" "" "$(changed)"
out="$(guarded "$tmp/guard/metabase" 0 web_main start)"
check    "a container of this data folder that does not run is replaced" "rc=0" "$out"
check_eq "...removed, then started" "rm run" "$(changed)"
out="$(GUARD_DATA="$tmp/guard/" guarded "$tmp/guard/metabase" 0 web_main start)"
check_eq "a data folder spelled with a trailing slash is the same folder" "rm run" "$(changed)"
out="$(guarded "$tmp/guard/metabase" 1 web_main start)"
check    "one that runs is this setup's: use restart" "web: already running (container wealthdb-metabase). Use 'wealthdb web restart'." "$out"
out="$(guarded "$tmp/guard/metabase" 1 web_main restart)"
check_eq "a restart of this setup's server stops, removes and starts it" "stop rm run" "$(changed)"
out="$(guarded "$tmp/guard-other" 0 web_main start)"
check    "a folder whose name only starts the same is another folder" "is the server of another setup" "$out"
# refresh with another setup's server under the name: the snapshot is
# this setup's to refresh, and the server is left as it is.
out="$(guarded /elsewhere/web/metabase 1 web_main refresh)"
check    "a refresh beside another setup's server succeeds" "rc=0" "$out"
check_eq "...and stops, removes and starts nothing" "" "$(changed)"

echo "== the commands a setup of its own is told to run =="
OWNCFG="$tmp/own setup/wealthdb.cfg"
OWNCMD="wealthdb -c '$OWNCFG'"
# own_run CMD...: CMD with that config named, as `wealthdb -c` names it.
own_run() {
    (
        export WEALTHDB_CONFIG="$OWNCFG"
        unset WEALTHDB_WEB_DATA_DIR
        _resolve_knobs
        "$@" 2>&1
    )
}
out="$(own_run eval 'ENABLED=0; _require_enabled')"
check_eq "turning the dashboards on" "web: the dashboards are off. Run: $OWNCMD config dashboards" "$out"
out="$(own_run eval '_snapshot "$tmp/no-gold.db" "$tmp/x/s.db"')"
check    "a database that is not there" "(run '$OWNCMD init' / '$OWNCMD load' first)" "$out"
out="$(own_run eval 'PORT=3000 BIND=v4 PROV_LOGIN=x; _start_banner')"
check    "the banner's refresh" "Run '$OWNCMD web refresh' after each '$OWNCMD load' to update." "$out"
check    "the banner names the setup's container" "web: wealthdb-metabase-own-setup is up" "$out"
out="$(own_run eval '_is_running() { return 0; }; _load_config() { ENABLED=1; PORT=3000; GOLD_DB=x; CONFIG_LOADED=1; }; _start_guard() { return 0; }; web_start')"
check    "a server that runs" "Use '$OWNCMD web restart'." "$out"
out="$(own_run eval '_config_env() { return 0; }; SNAP="$tmp/none.db"; web_status')"
check    "status, with the dashboards off" "note: the dashboards are off ($OWNCMD config dashboards)" "$out"
check    "status, with no snapshot" "created on '$OWNCMD web start' / 'refresh'" "$out"
out="$(own_run eval 'WEALTHDB=true; _config_env')"
check    "a config the engine cannot read is named with its path" \
    "web: could not read settings via '$OWNCMD config-env web' (is $OWNCFG valid?)" "$out"
out="$(GUARD_CONFIG="$OWNCFG" guarded /elsewhere/web/metabase 0 web_main start)"
check    "the guard names the setup's own container" "web: wealthdb-metabase-own-setup is the server of another setup" "$out"
check_eq "...and nothing is changed" "" "$(changed)"
# The default config's texts name no config.
out="$( (_resolve_knobs; WEALTHDB=true; _config_env) 2>&1)"
check_eq "the default config: the engine's command and the config by its name" \
    "web: could not read settings via 'wealthdb config-env web' (is wealthdb.cfg valid?)" "$out"
out="$( (_resolve_knobs; ENABLED=0; _require_enabled) 2>&1)"
check_eq "the default config: turning the dashboards on" "web: the dashboards are off. Run: wealthdb config dashboards" "$out"

# ---- the native server -------------------------------------------------
# The same script in a tree of the native engine: web is sourced again
# with a record that says so, in a subshell, with a home of its own and
# launchd out of reach, so that nothing is started on this computer.
native_cases() {
    printf 'ENGINE=native\nJAVA=%s\n' "$tmp/java" > "$tmp/native.mk"
    export WEALTHDB_BUILD_CONFIG="$tmp/native.mk" WEALTHDB_NO_LAUNCHD=1 HOME="$tmp/home"
    # shellcheck source=/dev/null
    source "$HERE/web"
    PYTHON3=python3
    WEALTHDB_WEB_DATA_DIR="$tmp/nweb" _resolve_knobs
    LIB_DIR="$tmp/nlib" MB_JAR="$tmp/nlib/metabase.jar" DRIVER_JAR="$tmp/nlib/duckdb.metabase-driver.jar"

    echo "== native: Metabase's two files, held to their pins =="
    check_re  "the Dockerfile pins a Metabase version"  '^v[0-9.]+$'      "$(_dockerfile_arg MB_VERSION)"
    check_re  "and its checksum"                        '^[0-9a-f]{64}$'  "$(_dockerfile_arg MB_SHA256)"
    check_re  "the driver's address"                    '^https://'       "$(_dockerfile_arg DRIVER_URL)"
    check     "(the driver's releases)" "metabase_duckdb_driver/releases/download" "$(_dockerfile_arg DRIVER_URL)"
    check_re  "and the driver's checksum"               '^[0-9a-f]{64}$'  "$(_dockerfile_arg DRIVER_SHA256)"
    # The two downloads are the image's: the address of the Dockerfile's
    # ADD line with the pinned version in it, and the pinned checksums.
    (
        _fetch_pinned() { printf '%s\n' "$2 $3 $4" >> "$tmp/fetched"; }
        _fetch_metabase
    )
    add="$(sed -n 's/^ADD .*\(https:[^ ]*metabase\.jar\) .*/\1/p' "$HERE/Dockerfile")"
    check_eq  "Metabase comes from the address the image adds it from, to its pin" \
        "${add/\$\{MB_VERSION\}/$(_dockerfile_arg MB_VERSION)} $(_dockerfile_arg MB_SHA256) $MB_JAR" "$(sed -n '1p' "$tmp/fetched")"
    check_eq  "and the driver from its address, to its pin" \
        "$(_dockerfile_arg DRIVER_URL) $(_dockerfile_arg DRIVER_SHA256) $DRIVER_JAR" "$(sed -n '2p' "$tmp/fetched")"
    printf 'a jar\n' > "$tmp/src.jar"
    sum="$(_sha256 "$tmp/src.jar")"
    out="$(_fetch_pinned "the file" "file://$tmp/src.jar" "$sum" "$tmp/nlib/one.jar" 2>&1)"; rc=$?
    check_eq  "a download that matches its pin is kept" "0 a jar" "$rc $(cat "$tmp/nlib/one.jar" 2>/dev/null)"
    check_eq  "with its checksum beside it" "$sum" "$(cat "$tmp/nlib/one.jar.sha256" 2>/dev/null)"
    rm -f "$tmp/src.jar"
    out="$(_fetch_pinned "the file" "file://$tmp/src.jar" "$sum" "$tmp/nlib/one.jar" 2>&1)"; rc=$?
    check_eq  "a file that passed is not downloaded again" "0" "$rc"
    printf 'another jar\n' > "$tmp/src.jar"
    out="$(_fetch_pinned "the file" "file://$tmp/src.jar" "$sum" "$tmp/nlib/two.jar" 2>&1)"; rc=$?
    check_eq  "a download that does not match its pin fails" "1" "$rc"
    check     "and says which checksum it has" "has the checksum $(_sha256 "$tmp/src.jar")" "$out"
    [ ! -e "$tmp/nlib/two.jar" ] && pass "and leaves no file" || fail "a file with a wrong checksum was kept"
    out="$(_fetch_pinned "the file" "file://$tmp/none.jar" "$sum" "$tmp/nlib/three.jar" 2>&1)"; rc=$?
    check_eq  "a download that fails, fails" "1" "$rc"
    # A download that is cut off: a curl that writes part of the file,
    # then ends the shell that runs it.
    out="$(
        curl() { while [ "$1" != -o ]; do shift; done; printf 'part' > "$2"; exit 130; }
        _fetch_pinned "the file" "file://$tmp/src.jar" "$sum" "$tmp/nlib/four.jar" 2>&1
    )"
    check_eq  "a download that is cut off leaves no file behind" "one.jar one.jar.sha256" "$(ls "$tmp/nlib" | tr '\n' ' ' | sed 's/ $//')"
    : > "$tmp/nlib/five.jar"
    out="$(_fetch_pinned "the file" "file://$tmp/src.jar" "" "$tmp/nlib/five.jar" 2>&1)"; rc=$?
    check_eq  "a pin with no checksum is refused, with a file in place too" "1" "$rc"
    check     "and the message says what is missing" "web/Dockerfile pins no checksum for the file" "$out"
    rm -f "$tmp/nlib/five.jar"

    echo "== native: what the server needs =="
    out="$(_require_native_web 2>&1)"; rc=$?
    check_eq  "no Java: refused" "1" "$rc"
    check     "and says what to install" "need Java 21 or later" "$out"
    printf '#!/bin/sh\n{ printf "%%s\\n" "$@"; env | grep -E "^(MB_|TZ=|MALLOC_)" | sort; echo "cwd=$(pwd)"; echo "pid=$$"; } > "%s"\n' \
        "$tmp/java-ran" > "$tmp/java"
    chmod +x "$tmp/java"
    WEALTHDB_WEB_DATA_DIR="$tmp/nweb" _resolve_knobs
    LIB_DIR="$tmp/nlib" MB_JAR="$tmp/nlib/metabase.jar" DRIVER_JAR="$tmp/nlib/duckdb.metabase-driver.jar"
    out="$(_require_native_web 2>&1)"; rc=$?
    check     "no files yet: names the command that downloads them" "Run: wealthdb web build" "$out"
    : > "$MB_JAR"; : > "$DRIVER_JAR"
    if _require_native_web 2>/dev/null; then pass "Java and both files: ready"; else fail "Java and both files: not ready"; fi

    echo "== native: the paths provisioning gives Metabase =="
    mkdir -p "$tmp/nweb/snapshot" && : > "$tmp/nweb/snapshot/wealthdb.db"
    (
        python3() {
            case "$1" in *provision.py) ;; *) command python3 "$@"; return ;; esac
            printf '%s\n' "$@" > "$tmp/nprov-args"
        }
        PORT=3111 BIND=both WEALTHDB_WEB_ADMIN_PASSWORD="Synthetic-pw-1!"
        _provision 2>/dev/null
    )
    nargs="$(tr '\n' ' ' < "$tmp/nprov-args" 2>/dev/null)"
    nname="$(_serving_name)"
    check     "the snapshot under its own path" "--gold-path $tmp/nweb/snapshot/$nname " "$nargs"
    check     "the spill directory under its own path" "--spill-dir $tmp/nweb/spill " "$nargs"
    check     "Metabase on loopback" "--base http://127.0.0.1:3111 " "$nargs"

    echo "== native: lifecycle without a server =="
    CONTAINER=wealthdb-metabase
    check_eq  "the label" "com.smartpointer.wealthdb.metabase" "$(_service_label)"
    printf 'WEALTHDB_WEB_ENABLED=1\nWEALTHDB_WEB_PORT=3111\nWEALTHDB_GOLD_DB=%s\n' "'$tmp/gold.db'" > "$tmp/engine-out"
    out="$(web_status 2>&1)"
    check     "status: not running" "web: not running" "$out"
    check_not "status names no container" "container" "$out"
    out="$(web_stop 2>&1)"
    check     "stop: nothing to stop" "web: no server to stop" "$out"
    out="$(PORT=3111 BIND=v6 _base_url)"
    check_eq  "v6 → the IPv6 loopback address" "http://[::1]:3111" "$out"
    out="$(PORT=3111 BIND=both PROV_LOGIN= _start_banner 2>&1)"
    check     "the banner says where to open the dashboards" "web: the dashboards are up → http://127.0.0.1:3111/" "$out"
    check_not "a plain process promises no start at login" "at login" "$out"

    echo "== native: a refresh names the server, not a container =="
    out="$(swap_refresh 1 0)"
    check     "an unconfirmed swap restarts the server" "did not confirm it reads the new snapshot; restarting the server" "$out"
    out="$(swap_refresh link 0)"
    check     "a serving name that cannot be linked keeps the server" "web: the server keeps serving the previous snapshot." "$out"

    echo "== native: stop ends a container the other engine left =="
    : > "$tmp/docker-stub-calls"
    out="$(STUB_CONTAINER=1 STUB_MOUNT="$H2_DIR" web_stop 2>&1)"
    check     "stop says which container it stopped" "web: stopped container wealthdb-metabase, the container engine's server" "$out"
    check_not "and has no line on a server that is not there" "no server to stop" "$out"
    check     "docker is asked, then stops and removes it" \
        "|stop wealthdb-metabase|rm wealthdb-metabase|" "$(tr '\n' '|' < "$tmp/docker-stub-calls")"
    : > "$tmp/docker-stub-calls"
    out="$(STUB_CONTAINER=1 web_stop 2>&1)"
    check     "a container of that name that serves another data folder is left alone" "web: no server to stop" "$out"
    check_not "docker stops nothing of it" "stop wealthdb-metabase" "$(cat "$tmp/docker-stub-calls")"

    echo "== native: start, status and stop of a stand-in server =="
    # start runs `<entry> web serve` in the background, then provisions.
    # A stand-in for that command records its pid, as serve does, and
    # sleeps. Provisioning is a stub.
    printf '#!/bin/sh\necho $$ > "%s"\nexec sleep 30\n' "$tmp/nweb/web.pid" > "$tmp/nentry"
    chmod +x "$tmp/nentry"
    export WEALTHDB_ENTRY="$tmp/nentry"
    SERVICE_PROC=sleep
    _provision() { PROV_RC=0 PROV_SERVED=1 PROV_LINKED=1 PROV_LOGIN="log in as a stand-in"; }
    out="$(web_start 2>&1)"; rc=$?
    check_eq  "start succeeds" "0" "$rc"
    check_re  "it times the server's start" "web: timing: server start [0-9]+\.[0-9]{3}s" "$out"
    check     "and says the dashboards are up" "web: the dashboards are up → http://127.0.0.1:3111/" "$out"
    check     "with the login" "log in as a stand-in" "$out"
    if _is_running; then pass "the server runs"; else fail "the server does not run: $(cat "$(_service_logfile)" 2>/dev/null)"; fi
    pid="$(_native_pid)"
    out="$(web_status 2>&1)"
    check     "status: running, with its pid" "web: running   pid=$pid" "$out"
    check     "status names the log" "log: $tmp/nweb/web.log" "$out"
    out="$(web_start 2>&1)"; rc=$?
    check     "a second start is refused" "1 web: already running (the server)" "$rc $(printf '%s\n' "$out" | grep already)"
    # serve while a server runs: refused, and the pid file stays the
    # running server's.
    out="$( (web_serve) 2>&1)"; rc=$?
    check     "serve is refused while a server runs" "1 web: already running (pid $pid)" "$rc $out"
    check_eq  "and the pid file still names the running server" "$pid" "$(cat "$tmp/nweb/web.pid")"
    out="$(web_stop 2>&1)"
    check     "stop says so" "web: stopped the server (Metabase metadata kept in $tmp/nweb/metabase)" "$out"
    if _pid_runs "$pid"; then fail "the server still runs after stop"; else pass "the server is gone after stop"; fi
    out="$(BIND=ipv6 web_start 2>&1)"; rc=$?
    check     "a start with a BIND that is no address family fails" "1 web: invalid WEALTHDB_WEB_BIND 'ipv6'" "$rc $(printf '%s\n' "$out" | grep invalid)"
    SERVICE_PROC=metabase.jar

    echo "== native: serve of dashboards that are off =="
    printf 'WEALTHDB_WEB_ENABLED=0\nWEALTHDB_WEB_PORT=3111\nWEALTHDB_GOLD_DB=%s\n' "'$tmp/gold.db'" > "$tmp/engine-out"
    out="$(XPC_SERVICE_NAME=0 web_serve 2>&1)"; rc=$?
    check     "serve fails, and names the setup" "1 web: the dashboards are off. Run: wealthdb config dashboards" "$rc $out"
    check_not "outside launchd it removes no agent" "removing the server from launchd" "$out"
    (
        # Under launchd, which is a function that records its calls: in
        # a process launchd started under this label, the agent goes.
        _uses_launchd() { return 0; }
        launchctl() { echo "$*" >> "$tmp/launchctl-calls"; }
        label="$(_service_label)"
        _write_plist
        echo 1 > "$(_service_pidfile)"
        : > "$tmp/launchctl-calls"
        out="$(XPC_SERVICE_NAME="$label" web_serve 2>&1)"; rc=$?
        check     "under launchd serve fails too" "1 web: the dashboards are off" "$rc $out"
        check     "and says that it leaves" "web: turned off in the config; removing the server from launchd" "$out"
        check_eq  "it boots its agent out" "bootout gui/$(id -u)/$label" "$(cat "$tmp/launchctl-calls")"
        [ ! -e "$(_service_plist)" ] && [ ! -e "$(_service_pidfile)" ] && pass "its agent file and pid file are gone" ||
            fail "serve left the agent file or the pid file"
        out="$(PORT=3111 BIND=both PROV_LOGIN= _start_banner 2>&1)"
        check     "under launchd the banner says the server starts again at login" \
            "Open that address in a browser on this computer. The server starts again at login." "$out"
        exit "$fails"
    )
    fails=$?
    printf 'WEALTHDB_WEB_ENABLED=1\nWEALTHDB_WEB_PORT=3111\nWEALTHDB_GOLD_DB=%s\n' "'$tmp/gold.db'" > "$tmp/engine-out"

    echo "== native: a start never takes over another setup's LaunchAgent =="
    (
        # launchd is a function that records its calls. On `bootstrap`
        # it runs the agent: a stand-in that records its pid, as serve
        # does.
        _uses_launchd() { return 0; }
        launchctl() {
            echo "$*" >> "$tmp/launchctl-calls"
            case "$1" in
                bootstrap)
                    sleep 30 >/dev/null 2>&1 &
                    echo $! > "$(_service_pidfile)" ;;
            esac
        }
        unset XDG_DATA_HOME
        SERVICE_PROC=sleep
        plist="$(_service_plist)" label="$(_service_label)"
        # An agent of this label that another setup wrote: its file
        # names that setup's data folder.
        (DATA_DIR="/elsewhere/a<b>&c/web"; _write_plist)
        cp "$plist" "$tmp/other.plist"
        check     "an agent file names its data folder, escaped" \
            "<key>WEALTHDB_WEB_DATA_DIR</key>|        <string>/elsewhere/a&lt;b&gt;&amp;c/web</string>" \
            "$(grep -A1 WEALTHDB_WEB_DATA_DIR "$plist" | tr '\n' '|')"
        check_eq  "...which is read back as it was" "/elsewhere/a<b>&c/web" "$(_plist_value WEALTHDB_WEB_DATA_DIR)"
        : > "$tmp/launchctl-calls"
        out="$(web_start 2>&1)"; rc=$?
        check_eq  "a start is refused" "1" "$rc"
        check     "the message names the agent, the folder and the way out" \
            "web: $label is the server of another setup (its files are not in $tmp/nweb). Stop that one first, or name this one with WEALTHDB_WEB_CONTAINER." "$out"
        check_eq  "launchd is asked for nothing" "" "$(cat "$tmp/launchctl-calls")"
        if cmp -s "$plist" "$tmp/other.plist"; then pass "and the agent file is as it was"; else fail "the start changed the agent file"; fi
        out="$( (export WEALTHDB_WEB_DATA_DIR="$tmp/nweb"; web_main restart) 2>&1)"; rc=$?
        check     "a restart is refused before it stops anything" "1 web: $label is the server of another setup" "$rc $out"
        check_eq  "...launchd is asked for nothing" "" "$(cat "$tmp/launchctl-calls")"
        if cmp -s "$plist" "$tmp/other.plist"; then pass "...and the agent file is as it was"; else fail "the restart changed the agent file"; fi
        # An agent file that names no data folder was written for the
        # default one, under the XDG data dir it names.
        drop_key() { awk -v key="<key>$1</key>" 'skip { skip = 0; next } index($0, key) { skip = 1; next } { print }' "$plist" > "$plist.tmp" && mv "$plist.tmp" "$plist"; }
        drop_key WEALTHDB_WEB_DATA_DIR
        if (DATA_DIR="$HOME/.local/share/wealthdb/web"; _agent_serves_here); then pass "an agent file with no data folder serves the default one"
        else fail "an agent file with no data folder is not the default setup's"; fi
        if _agent_serves_here; then fail "an agent file with no data folder reads as this folder's"
        else pass "...and no other"; fi
        (export XDG_DATA_HOME=/xdg; DATA_DIR=/xdg/wealthdb/web; _write_plist)
        drop_key WEALTHDB_WEB_DATA_DIR
        if (DATA_DIR=/xdg/wealthdb/web/; _agent_serves_here); then pass "...under the XDG data dir it names"
        else fail "an agent file's XDG data dir is not read"; fi
        # This setup's own agent, left by a server that died, is replaced.
        _write_plist
        : > "$tmp/launchctl-calls"
        out="$(web_start 2>&1)"; rc=$?
        check_eq  "a start over this setup's own agent succeeds" "0" "$rc"
        check     "the agent is booted out, then in" "bootout gui/$(id -u)/$label|bootstrap gui/$(id -u) $plist|" \
            "$(tr '\n' '|' < "$tmp/launchctl-calls")"
        web_stop >/dev/null 2>&1
        [ ! -e "$plist" ] && pass "and a stop removes it" || fail "the agent file is left"
        exit "$fails"
    )
    fails=$?

    echo "== native: serve becomes Metabase =="
    # serve is a command of its own, whose process Java becomes: it runs
    # from a tree of the native engine, with the stub as that tree's Java.
    tree="$tmp/ntree"
    mkdir -p "$tree/web/lib" "$tree/shared/wrappers"
    cp "$HERE/web" "$HERE/Dockerfile" "$tree/web/"
    cp "$HERE"/../shared/wrappers/*.sh "$tree/shared/wrappers/"
    : > "$tree/web/lib/metabase.jar"; : > "$tree/web/lib/duckdb.metabase-driver.jar"
    printf 'ENGINE=native\nJAVA=%s\n' "$tmp/java" > "$tree/config.mk"
    ( unset WEALTHDB_BUILD_CONFIG
      WEALTHDB_WEB_DATA_DIR="$tmp/nweb" WEALTHDB_BIN="$tmp/engine-stub" WEALTHDB_WEB_BIND=both TZ=Europe/Zurich \
          "$tree/web/web" serve ) > "$tmp/nserve.out" 2>&1; rc=$?
    [ "$rc" = 0 ] || sed "s/^/       serve: /" "$tmp/nserve.out"
    check_eq  "serve runs Java" "0" "$rc"
    ran="$(cat "$tmp/java-ran" 2>/dev/null)"
    check     "on the Metabase file" "$tree/web/lib/metabase.jar" "$ran"
    check     "with the heap capped" "-Xmx2g" "$ran"
    check     "in UTC" "-Duser.timezone=UTC" "$ran"
    check     "(the process too)" "TZ=UTC" "$ran"
    check     "on loopback" "MB_JETTY_HOST=127.0.0.1" "$ran"
    check     "on the configured port" "MB_JETTY_PORT=3111" "$ran"
    check     "its own database under the web data" "MB_DB_FILE=$tmp/nweb/metabase/metabase.db" "$ran"
    check     "its drivers from a folder it may write" "MB_PLUGINS_DIR=$tmp/nweb/plugins" "$ran"
    check     "the row limits" "MB_UNAGGREGATED_QUERY_ROW_LIMIT=100000" "$ran"
    check     "under the pid the pid file names" "pid=$(cat "$tmp/nweb/web.pid" 2>/dev/null)" "$ran"
    [ -L "$tmp/nweb/plugins/duckdb.metabase-driver.jar" ] && pass "the driver is linked into that folder" ||
        fail "the driver is not linked into the plugins folder"
    ( unset WEALTHDB_BUILD_CONFIG
      WEALTHDB_WEB_DATA_DIR="$tmp/nweb" WEALTHDB_BIN="$tmp/engine-stub" WEALTHDB_WEB_BIND=v6 \
          "$tree/web/web" serve ) > "$tmp/nserve.out" 2>&1; rc=$?
    check     "v6 → Metabase on the IPv6 loopback address" "0 MB_JETTY_HOST=::1" "$rc $(grep MB_JETTY_HOST "$tmp/java-ran" 2>/dev/null)"
    rm -f "$tmp/java-ran"
    ( unset WEALTHDB_BUILD_CONFIG
      WEALTHDB_WEB_DATA_DIR="$tmp/nweb" WEALTHDB_BIN="$tmp/engine-stub" WEALTHDB_WEB_BIND=ipv6 \
          "$tree/web/web" serve ) > "$tmp/nserve.out" 2>&1; rc=$?
    check     "a BIND that is no address family fails serve" "1 web: invalid WEALTHDB_WEB_BIND 'ipv6' (want both|v4|v6)" "$rc $(cat "$tmp/nserve.out")"
    [ ! -e "$tmp/java-ran" ] && pass "before Java runs" || fail "Java ran with a BIND that is no address family"
}
( native_cases; exit "$fails" )
fails=$?

echo "== serve is the native server's =="
out="$(web_main serve 2>&1)"; rc=$?
check_eq  "serve in a tree of the container engine exits 2" "2" "$rc"
check     "and names the command that tree has" "wealthdb web start" "$out"

echo "== stop ends a server the native engine left =="
# In this tree of the container engine: a stand-in for a native server
# that ran when ./configure chose the container engine, which the pid
# file names. No container is there.
standin="$(sleep 30 >/dev/null 2>&1 & echo $!)"
# A process just started still has its parent's command line for a
# moment, and a stop ends only the program its pid file should name.
for _ in $(seq 1 50); do
    case "$(_pid_command "$standin")" in sleep*) break ;; esac
    sleep 0.1
done
out="$(
    _resolve_knobs
    mkdir -p "$DATA_DIR" && echo "$standin" > "$DATA_DIR/web.pid"
    SERVICE_PROC=sleep
    web_stop 2>&1
    [ -e "$DATA_DIR/web.pid" ] && echo PID-FILE-LEFT
    web_stop 2>&1
)"
check     "stop says which server it stopped" "web: stopped the native engine's server (Metabase metadata kept in $tmp/web-data/metabase)" "$out"
check_not "its pid file is gone" "PID-FILE-LEFT" "$out"
if _pid_runs "$standin"; then fail "the native server still runs"; kill "$standin"; else pass "the native server is gone"; fi
check_eq  "a stop with neither kind there says so" "web: no container 'wealthdb-metabase' to stop" "$(printf '%s\n' "$out" | tail -n 1)"
out="$(METABASE_HEAP=-Xmx2g; _resolve_knobs; PORT=3000 BIND=v4 H2_DIR=/h SNAP=/s/wealthdb.db SPILL_DIR=/sp; _docker_run_args; printf '%s\n' "${DOCKER_ARGS[@]}")"
check     "the container gets the same settings as the native server" "MB_UNAGGREGATED_QUERY_ROW_LIMIT=100000" "$out"
check     "and the same heap" "JAVA_TOOL_OPTIONS=-Xmx2g" "$out"

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
