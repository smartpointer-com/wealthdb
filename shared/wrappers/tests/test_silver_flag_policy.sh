#!/usr/bin/env bash
# Behaviour tests for the shared wrappers' --silver-db policy.
#
# The rule: reject a flag that does nothing on the verb being run, rather
# than swallowing it (host-lib) or forwarding it into a parser that rejects
# it naming an in-container path the caller never typed (wrapper-lib). The
# exception is uniformity — a flag every collector must ACCEPT so the fleet
# orchestrator can pass it everywhere (--lookback) stays accepted.
#
# ${PREFIX}_SILVER_DB is deliberately NOT symmetric with the flag: it is
# ambient config naming where silver lives, so a verb that has no use for it
# ignores it. Erroring instead would make `export VIAC_SILVER_DB=...` break
# every login in that shell.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PASS=0
FAIL=0

# The accepted path mkdir -p's the silver parent; keep that in a temp dir.
SILVERTMP="$(mktemp -d)"
trap 'rm -rf "$SILVERTMP"' EXIT

ok()   { PASS=$((PASS+1)); echo "  ok   $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  FAIL $1"; }
check() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi; }

# ---------------------------------------------------------------- wrapper-lib
# Drive wrapper_resolve_dir_args directly: the reject happens before any
# `docker run`, so no container is involved.
# Runs wrapper_resolve_dir_args in a subshell and echoes its outcome:
#   accepted -> "OK|<forwarded args>" with rc 0
#   rejected -> the reject message with rc 2 (the function exits, by design)
# The call is NOT wrapped in $(...) — that would set FORWARD_ARGS in a
# subshell of its own and lose it.
_wl() {  # _wl <env-silver> <args...>
    (
        local envval="$1"; shift
        NAME=testcol; ENV_PREFIX=TESTCOL
        HOST_SECRETS=/tmp/s; HOST_DATA=/tmp/d
        [[ -n "$envval" ]] && export TESTCOL_SILVER_DB="$envval"
        # shellcheck disable=SC1091
        source "$HERE/../wrapper-lib.sh" 2>/dev/null
        exec 2>&1
        wrapper_resolve_dir_args "$@"
        echo "OK|${FORWARD_ARGS[*]-}"
    )
}

echo "wrapper-lib (docker wrappers):"

r="$(_wl "" login --silver-db /tmp/x.db)"; rc=$?
check "--silver-db on login is rejected" "$rc" "2"
case "$r" in *"does not apply to 'login'"*) ok "reject message names the verb" ;;
             *) bad "reject message names the verb (got: $r)" ;; esac

r="$(_wl "" download --silver-db=/tmp/x.db)"; rc=$?
check "--silver-db=VALUE on download is rejected" "$rc" "2"

r="$(_wl "" load --silver-db "$SILVERTMP/x.db")"; rc=$?
check "--silver-db on load is accepted" "$rc" "0"
case "$r" in *"--silver-db /silver/x.db"*) ok "load rewrites to the /silver mount path" ;;
             *) bad "load rewrites to the /silver mount path (got: $r)" ;; esac

# The regression that motivated the env/flag split: an exported
# ${PREFIX}_SILVER_DB used to append --silver-db to EVERY verb, so a shell
# that exported it broke every login with an argparse "unrecognized
# arguments" error.
r="$(_wl /tmp/x.db login --check)"; rc=$?
check "exported SILVER_DB does not break login" "$rc" "0"
case "$r" in *--silver-db*) bad "exported SILVER_DB must not reach login (got: $r)" ;;
             *) ok "exported SILVER_DB is not forwarded to login" ;; esac

r="$(_wl "$SILVERTMP/x.db" load)"; rc=$?
case "$r" in *"--silver-db /silver/x.db"*) ok "exported SILVER_DB still applies to load" ;;
             *) bad "exported SILVER_DB still applies to load (got: $r)" ;; esac

# SILVER_SUBCOMMANDS REPLACES the default {load} (cointracking's fetch-prices
# writes prices straight into silver).
r="$(SILVER_SUBCOMMANDS=(load fetch-prices); _wl "" fetch-prices --silver-db "$SILVERTMP/x.db")"; rc=$?
check "SILVER_SUBCOMMANDS extends the accepting set" "$rc" "0"
r="$(SILVER_SUBCOMMANDS=(load fetch-prices); _wl "" login --silver-db "$SILVERTMP/x.db")"; rc=$?
check "SILVER_SUBCOMMANDS REPLACES rather than adds (login still rejects)" "$rc" "2"

# ------------------------------------------------------------------ host-lib
echo "host-lib (host-venv wrappers):"

_hl() {  # _hl <verb> <args...> -> "rc|stderr"
    (
        set +e
        NAME=testcol; ENV_PREFIX=TESTCOL
        export XDG_DATA_HOME="${TMPDIR:-/tmp}/wl-test-$$"
        # shellcheck disable=SC1091
        source "$HERE/../host-lib.sh" 2>/dev/null
        out="$(host_resolve_dirs "$@" 2>&1)"; rc=$?
        rm -rf "$XDG_DATA_HOME"
        echo "$rc|$out"
    )
}

r="$(_hl download --silver-db /tmp/x.db)"
check "--silver-db on download is rejected (not swallowed)" "${r%%|*}" "2"
case "${r#*|}" in *"does not apply to 'download'"*) ok "reject message names the verb" ;;
                  *) bad "reject message names the verb (got: ${r#*|})" ;; esac

r="$(_hl load --silver-db "$SILVERTMP/x.db")"
check "--silver-db on load is accepted" "${r%%|*}" "0"

r="$(_hl prune --silver-db /tmp/x.db)"
check "--silver-db on prune is rejected" "${r%%|*}" "2"

echo
echo "wrapper tests: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
