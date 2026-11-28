#!/usr/bin/env bash
# e2e.sh WEALTHDB TREE WORKDIR — walk a tree of the native engine end to
# end, on the synthetic demo household:
#   - build the demo's data, create the database, load it, read reports;
#   - the database states the native engine, and refuses the other kind;
#   - a load waits for a collector that writes, and skips the source;
#   - a collector in plain Python runs with no venv of its own;
#   - the MCP server starts, asks for its token, answers, stops;
#   - the dashboards start, are provisioned on the snapshot, take a
#     refresh after a load, and stop.
# WEALTHDB is the launcher to test: a checkout's wealthdb/wealthdb, or
# the one a prefix install linked. TREE holds the demo generator, which
# runs on the Python TREE's config.mk records (PYTHON names another).
# WORKDIR is made, and holds what the walk writes: the data, the config,
# the secrets folder and the cache.
#
# Where there is launchd, which is every Mac unless WEALTHDB_NO_LAUNCHD=1
# is set, the servers run as LaunchAgents of this account, and two
# things lie outside WORKDIR, under ~/Library:
#   - the two agent files, LaunchAgents/com.smartpointer.wealthdb.mcp-e2e.plist
#     and …wealthdb.metabase-e2e.plist. The labels are the walk's own,
#     so the agent of a server in use is never touched. A stop removes
#     the file, and the walk stops both servers when it ends.
#   - their logs, Logs/wealthdb/mcp-e2e.log and metabase-e2e.log, which
#     stay.
# On Linux with a systemd user manager, unless WEALTHDB_NO_SYSTEMD=1, the
# servers are user units of this account in the same way, their files in
# ~/.config/systemd/user under the walk's own labels, removed at stop.
# Otherwise the servers are plain background processes, and their logs
# are in WORKDIR too.
#
# E2E_MCP_PORT and E2E_WEB_PORT move the ports (3490, 3190).
# E2E_SKIP_WEB=1 leaves the dashboards out, which need Java, and
# E2E_SKIP_CARDS=1 only the run of every dashboard card, which takes
# minutes.
set -uo pipefail

W="${1:?usage: e2e.sh WEALTHDB TREE WORKDIR}"
TREE="${2:?usage: e2e.sh WEALTHDB TREE WORKDIR}"
WORK="${3:?usage: e2e.sh WEALTHDB TREE WORKDIR}"
MCP_PORT="${E2E_MCP_PORT:-3490}"
WEB_PORT="${E2E_WEB_PORT:-3190}"
# shellcheck disable=SC1091
. "$TREE/shared/wrappers/engine-mode.sh" || exit 2
PYTHON="${PYTHON:-$(wealthdb_tool "$TREE" PYTHON python3)}"

mkdir -p "$WORK" || exit 2
WORK="$(cd "$WORK" && pwd)"
# The engine beside the launcher, which a link on PATH may name.
real="$W"
while [ -L "$real" ]; do
    link="$(readlink "$real")"
    case "$link" in /*) real="$link" ;; *) real="$(dirname "$real")/$link" ;; esac
done
ENGINE_BIN="$(cd "$(dirname "$real")" && pwd)/wealthdb-engine"
R="$WORK/root"
# The dashboards' Metabase stays where `web build` put it, outside the
# walk's own cache, so a walk does not download it again.
export WEALTHDB_WEB_LIB_DIR="${WEALTHDB_WEB_LIB_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/wealthdb/metabase}"
export WEALTHDB_DATA_ROOT="$R" XDG_CONFIG_HOME="$R" WEALTHDB_CONFIG="$R/wealthdb.cfg"
export WEALTHDB_SECRETS_DIR="$WORK/secrets" XDG_CACHE_HOME="$WORK/cache"
export WEALTHDB_MCP_CONTAINER=wealthdb-mcp-e2e WEALTHDB_MCP_DATA_DIR="$R/mcp" WEALTHDB_MCP_ENV_FILE="$R/mcp.env"
export WEALTHDB_WEB_CONTAINER=wealthdb-metabase-e2e WEALTHDB_WEB_DATA_DIR="$R/web" WEALTHDB_WEB_ENV_FILE="$R/web.env"
unset WEALTHDB_ENGINE

FAIL=0
ok()   { echo "  ok   $1"; }
bad()  { echo "  FAIL $1"; FAIL=$((FAIL + 1)); }
step() { echo "== $1"; }
# must DESC CMD...: CMD succeeds. Its output is shown when it does not.
must() {
    local desc="$1" out; shift
    if out="$("$@" 2>&1)"; then ok "$desc"; else bad "$desc"; printf '%s\n' "$out" | tail -n 20 | sed 's/^/       /'; fi
}
# says DESC NEEDLE CMD...: CMD's output holds NEEDLE.
says() {
    local desc="$1" needle="$2" out; shift 2
    out="$("$@" 2>&1)"
    if grep -qF -- "$needle" <<<"$out"; then ok "$desc"; else bad "$desc (no '$needle')"; printf '%s\n' "$out" | tail -n 12 | sed 's/^/       /'; fi
}
stop_servers() {
    "$W" mcp stop >/dev/null 2>&1
    "$W" web stop >/dev/null 2>&1
}
trap stop_servers EXIT

step "the demo household"
# The demo ends two days ago, so that the dashboards' refresh below has
# new days to take.
first_end="$("$PYTHON" -c 'import datetime as d; print(d.datetime.now(d.timezone.utc).date() - d.timedelta(days=2))')"
# generate ARGS... runs the demo's generator, then moves the servers to
# this walk's ports: the generator writes the demo's config each time.
generate() {
    "$PYTHON" "$TREE/demo/generate.py" --root "$R" --seed harlow-19 "$@" || return 1
    sed -i.bak -e "s/\"port\": 3400/\"port\": $MCP_PORT/" -e "s/\"port\": 3100/\"port\": $WEB_PORT/" "$R/wealthdb.cfg" &&
        rm -f "$R/wealthdb.cfg.bak"
}
must "the generator writes the demo's data" generate --as-of "$first_end"
must "config validate" "$W" config validate
must "init" "$W" init
must "load -a" "$W" load -a
says "a report reads the database" "positions_value" "$W" -r holdings global
must "returns" "$W" -r returns global
must "spending" "$W" -r spending summary
must "status" "$W" -r status

step "one kind of engine per database"
[ "$(cat "$R/wealthdb.db.wealthdb.engine" 2>/dev/null)" = native ] && ok "the database states the native engine" ||
    bad "the database does not state the native engine"
if [ -x "$ENGINE_BIN" ]; then
    says "an engine of the other kind is refused a write" "the native engine writes this database" \
        env WEALTHDB_ENGINE=container "$ENGINE_BIN" -c "$R/wealthdb.cfg" load -a
else
    bad "no engine at $ENGINE_BIN"
fi

step "a collector that writes"
# The demo's silver files stand in a folder of their own; a stand-in
# collector holds the lock beside the first source's file.
silver="$(find "$R/silver" -type f \( -name '*.db' -o -name '*.duckdb' \) | sort | sed -n '1p')"
if [ -n "$silver" ] && [ -x "$ENGINE_BIN" ]; then
    lock="$(dirname "$silver")/.wealthdb-silver.lock"
    "$ENGINE_BIN" hold-lock "$lock" -- sleep 25 &
    holder=$!
    sleep 1
    says "a load skips a source whose collector writes" "its collector is writing" "$W" load -a
    kill "$holder" 2>/dev/null; wait "$holder" 2>/dev/null
    must "and loads it once the collector is done" "$W" load -a
else
    bad "no silver file under $R/silver"
fi

step "a collector in plain Python"
says "it runs with no Python environment of its own" "No bank is linked through Plaid yet" "$W" collect plaid banks
says "the dispatcher lists the sources" "plaid" "$W" collect list

step "the MCP server"
must "mcp start" "$W" mcp start
says "mcp status: running" "mcp: running" "$W" mcp status
token="$(tr -d '[:space:]' < "$R/mcp/token" 2>/dev/null)"
init='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}'
says "it answers with the token" '"instructions"' curl -s -m 20 -X POST "http://127.0.0.1:$MCP_PORT/mcp" \
    -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' -d "$init"
code="$(curl -s -m 20 -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$MCP_PORT/mcp" -H 'Content-Type: application/json' -d "$init")"
[ "$code" = 401 ] && ok "it refuses a call with no token" || bad "a call with no token: HTTP $code"
says "a second start says it runs" "already running" "$W" mcp start
says "one session on stdin and stdout" '"instructions"' bash -c "{ echo '$init'; sleep 2; } | '$W' mcp stdio 2>/dev/null"
must "mcp restart" "$W" mcp restart
says "running after the restart" "mcp: running" "$W" mcp status
must "mcp stop" "$W" mcp stop
says "mcp status: not running" "mcp: not running" "$W" mcp status
code="$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$MCP_PORT/mcp")"
[ "$code" = 000 ] && ok "nothing listens after the stop" || bad "after the stop: HTTP $code"

if [ "${E2E_SKIP_WEB:-0}" = 1 ]; then
    echo "== the dashboards: skipped (E2E_SKIP_WEB=1)"
else
    step "the dashboards"
    must "web start (Metabase comes up and is provisioned)" "$W" web start
    says "web status: running" "web: running" "$W" web status
    says "Metabase is healthy" '"ok"' curl -s -m 20 "http://127.0.0.1:$WEB_PORT/api/health"
    says "a refresh with nothing new changes nothing" "snapshot unchanged" "$W" web refresh
    must "the demo gains two days" generate --append
    must "load -a" "$W" load -a
    must "web refresh takes the new snapshot" "$W" web refresh
    says "the server still runs" "web: running" "$W" web status
    if [ "${E2E_SKIP_CARDS:-0}" != 1 ]; then
        must "every dashboard card runs" "$PYTHON" "$TREE/demo/check_dashboards.py" \
            --base "http://127.0.0.1:$WEB_PORT" --password-file "$R/web/admin-password.txt" --workers 2
    fi
    must "web stop" "$W" web stop
    says "web status: not running" "web: not running" "$W" web status
fi

echo
if [ "$FAIL" -eq 0 ]; then echo "native end to end: all passed"; else echo "native end to end: $FAIL failed"; fi
[ "$FAIL" -eq 0 ]
