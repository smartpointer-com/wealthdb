#!/usr/bin/env bash
# Behaviour test of bin/wealthdb-refresh under a stub launcher that
# records every call and answers as each case asks:
#   - each collector is checked first; a ready one is downloaded and
#     loaded, then `load -a`, then the dashboards' refresh when they are on;
#   - one that needs a person is skipped without a terminal and with
#     --unattended, named with its command; at a terminal it runs first,
#     before every ready one, or is skipped with s;
#   - a check with no verdict, a collector not set up, and a Plaid login
#     to renew are skipped and named;
#   - a failed step fails the run (1), a short download or a skip that
#     is no one's choice makes it 4, a usage error 2;
#   - --dry-run, --lookback, --no-web and named collectors.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"
R="$ROOT/bin/wealthdb-refresh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
STUB="$TMP/wealthdb"
cat > "$STUB" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$STUB_LOG"
up() { printf '%s' "$1" | tr 'a-z-' 'A-Z_'; }
case "$1 $2" in
    "config-env refresh") cat "$STUB_LIST" ;;
    "config-env web") echo "WEALTHDB_WEB_ENABLED=${STUB_WEB:-1}" ;;
    "load -a") exit "${STUB_GOLD:-0}" ;;
    "web refresh") exit 0 ;;
    collect\ *)
        name="$2" verb="$3"
        if [ "$verb" = login ] && [ "${4:-}" = --check ]; then key="STUB_CHECK_$(up "$name")"
        else key="STUB_$(up "$verb")_$(up "$name")"; fi
        echo "$name: $verb said its last line"
        exit "${!key:-0}" ;;
esac
EOF
chmod +x "$STUB"
LIST="$TMP/list"
cat > "$LIST" <<'EOF'
WEALTHDB_COLLECTOR='ready' WEALTHDB_START='login' WEALTHDB_ITEMS=''
WEALTHDB_COLLECTOR='person' WEALTHDB_START='login' WEALTHDB_ITEMS=''
WEALTHDB_COLLECTOR='quiet' WEALTHDB_START='login' WEALTHDB_ITEMS=''
WEALTHDB_COLLECTOR='unset' WEALTHDB_START='login' WEALTHDB_ITEMS=''
WEALTHDB_COLLECTOR='plaid' WEALTHDB_START='link --item {item}' WEALTHDB_ITEMS='first second'
EOF
export WEALTHDB_ENTRY="$STUB" STUB_LIST="$LIST" STUB_LOG="$TMP/log"
export STUB_CHECK_PERSON=1 STUB_CHECK_QUIET=3 STUB_CHECK_UNSET=78 STUB_CHECK_PLAID=1
unset WEALTHDB_PROGRAM WEALTHDB_COLLECT

# refresh ARGS...: the run's output and status, with no terminal.
refresh() { : > "$STUB_LOG"; out="$(python3 "$R" "$@" < /dev/null 2>&1)"; rc=$?; }
calls() { tr '\n' '|' < "$STUB_LOG"; }

echo "a run with no terminal:"
refresh
check_rc() { [ "$rc" = "$2" ] && ok "$1" || bad "$1 (exit $rc: $out)"; }
check_rc "a check with no verdict makes the run 4" 4
# The checks run side by side, in any order; every one comes before any
# download.
first_dl="$(grep -n ' download' "$STUB_LOG" | head -n 1 | cut -d: -f1)"
last_check="$(grep -n 'login --check' "$STUB_LOG" | tail -n 1 | cut -d: -f1)"
[ "$(grep -c 'login --check' "$STUB_LOG")" = 5 ] && [ "${last_check:-0}" -lt "${first_dl:-0}" ] &&
    ok "every collector is checked first, with no sign-in" || bad "the checks: $(calls)"
has "the ready one is downloaded and loaded" "collect ready download|collect ready load|" "$(calls)"
has "then gold, then the dashboards" "load -a|config-env web|web refresh|" "$(calls)"
lacks "the one that needs a person is not run" "collect person download" "$(calls)"
lacks "...nor signed in" "collect person login|" "$(calls)"
has "it is named with its command" "person  skipped  needs a person to sign in. Run: wealthdb-collect person login" "$out"
has "no verdict, with what the check said" "quiet   skipped  no verdict: quiet: login said its last line" "$out"
has "not set up" "unset   skipped  not set up:" "$out"
has "a Plaid login to renew names each link command" \
    "Run: wealthdb-collect plaid link --item first; wealthdb-collect plaid link --item second" "$out"
has "the summary names gold" "gold    loaded (load -a)" "$out"

echo "--unattended:"
STUB_CHECK_QUIET=0 STUB_CHECK_UNSET=0 refresh --unattended
check_rc "a skip for a person alone is 0" 0
lacks "...and runs no one's sign-in" "collect person login|" "$(calls)"

echo "at a terminal:"
# attended ANSWERS ARGS...: the run under a pseudo-terminal, ANSWERS typed
# at each prompt.
attended() {
    : > "$STUB_LOG"
    out="$(python3 - "$R" "$1" "${@:2}" <<'PY'
import os, pty, sys, time
r, answers, args = sys.argv[1], sys.argv[2].split(","), sys.argv[3:]
pid, fd = pty.fork()
if pid == 0:
    os.execvp(sys.executable, [sys.executable, r, *args])
out, i = b"", 0
while True:
    try:
        chunk = os.read(fd, 4096)
    except OSError:
        break
    if not chunk:
        break
    out += chunk
    if out.count(b"s to skip: ") > i and i < len(answers):
        os.write(fd, (answers[i] + "\n").encode())
        i += 1
_, status = os.waitpid(pid, 0)
sys.stdout.write(out.decode(errors="replace"))
sys.exit(os.waitstatus_to_exitcode(status))
PY
)"
    rc=$?
}
STUB_CHECK_QUIET=0 STUB_CHECK_UNSET=0 attended ""
check_rc "a person signs in, and the run is 0" 0
has "the one that needs a person is asked" "person: the sign-in needs you (person: login said its last line)." "$out"
has "...signed in, then downloaded, before the ready ones" \
    "collect person login|collect person download|collect ready download|" "$(calls)"
has "then the run says the rest needs no one" "every sign-in is done; the rest needs no one" "$out"
STUB_CHECK_QUIET=0 STUB_CHECK_UNSET=0 attended "s"
check_rc "s skips it, and the run is 0" 0
lacks "...and it is not signed in" "collect person login|" "$(calls)"
has "...and named" "person  skipped  skipped at the prompt. Run: wealthdb-collect person login" "$out"

echo "failures and short runs:"
STUB_DOWNLOAD_READY=1 refresh ready
check_rc "a failed download fails the run" 1
has "...and is named" "failed  download exit 1" "$out"
lacks "nothing loaded: no load -a" "load -a" "$(calls)"
STUB_DOWNLOAD_READY=4 refresh ready
check_rc "a short download makes it 4" 4
has "...and it is loaded all the same" "collect ready download|collect ready load|load -a|" "$(calls)"
STUB_CHECK_QUIET=0 STUB_CHECK_UNSET=0 STUB_GOLD=1 refresh
check_rc "a failed load -a fails the run" 1
lacks "...and the dashboards are not refreshed" "web refresh" "$(calls)"

echo "flags:"
STUB_CHECK_QUIET=0 STUB_CHECK_UNSET=0 refresh --dry-run
has "--dry-run asks each ready collector for its dry run" "collect ready download --dry-run|" "$(calls)"
lacks "...and loads nothing" "collect ready load" "$(calls)"
lacks "...nor gold" "load -a" "$(calls)"
STUB_CHECK_QUIET=0 STUB_CHECK_UNSET=0 refresh --lookback 1y --no-web
has "--lookback reaches every download" "collect ready download --lookback 1y|" "$(calls)"
lacks "--no-web refreshes no dashboards" "web refresh" "$(calls)"
STUB_CHECK_QUIET=0 STUB_CHECK_UNSET=0 STUB_WEB=0 refresh
lacks "dashboards that are off are not refreshed" "web refresh" "$(calls)"
refresh ready
check_rc "a named collector alone" 0
lacks "...and no other is checked" "collect person" "$(calls)"
refresh nobody
check_rc "a collector the config does not read is a usage error" 2
has "...naming what it reads" "the config reads no data of nobody" "$out"

echo "a setup of its own:"
STUB_CHECK_QUIET=0 STUB_CHECK_UNSET=0 WEALTHDB_COLLECT='wealthdb -c "/x/my try/wealthdb.cfg" collect' refresh
has "its commands name its config" 'Run: wealthdb -c "/x/my try/wealthdb.cfg" collect person login' "$out"

echo "a config that reads no bank yet:"
: > "$LIST"
refresh
check_rc "is no failure" 0
has "...and names the setup" "Run: wealthdb config banks" "$out"

summary "refresh tests"
