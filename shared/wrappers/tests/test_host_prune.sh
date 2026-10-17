#!/usr/bin/env bash
# Behaviour test: wrapper_host_prune, the host-side `prune` of the docker
# wrappers. It runs the collector's prune.py with collectorkit on
# PYTHONPATH and --bronze-dir set to the data dir. A --data-dir flag
# retargets that dir, --secrets-dir is absorbed, and --silver-db is
# rejected. --debug-dir is passed only with HAS_DEBUG=1. Both defaults
# come before the forwarded args, so an explicit flag wins (argparse
# last-wins). A stub python3 on PATH prints what it was run with, so
# nothing real runs.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PASS=0
FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ok   $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL $1"; }
check() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/bin"
cat > "$TMP/bin/python3" <<'STUB'
#!/usr/bin/env bash
echo "PYTHONPATH=$PYTHONPATH"
printf '%s\n' "$@"
STUB
chmod +x "$TMP/bin/python3"

# prune_argv [args...]: what wrapper_host_prune runs for `prune args...`,
# one item per line after a PYTHONPATH= line. HAS_DEBUG comes from the
# caller's environment.
prune_argv() {
    (
        PATH="$TMP/bin:$PATH"
        NAME=testcol; ENV_PREFIX=TESTCOL; ENV_VARS=()
        # shellcheck disable=SC1091
        source "$HERE/../wrapper-lib.sh"
        HERE="$TMP/collectors/testcol"
        HOST_DATA="$TMP/data"; HOST_DEBUG="$TMP/debug"
        exec 2>&1
        wrapper_host_prune prune "$@"
    )
}

# argv_of OUTPUT: the argv lines only, joined by spaces.
argv_of() { printf '%s\n' "$1" | sed 1d | tr '\n' ' ' | sed 's/ $//'; }

echo "wrapper_host_prune:"
unset HAS_DEBUG
out="$(prune_argv)"
check "prune.py gets the data dir as --bronze-dir" "$(argv_of "$out")" \
    "$TMP/collectors/testcol/prune.py --bronze-dir $TMP/data"
case "$(printf '%s\n' "$out" | head -n1)" in
    "PYTHONPATH=$TMP/collectors/testcol/../../shared/collectorkit:"*)
        ok "collectorkit is on PYTHONPATH" ;;
    *)  bad "collectorkit is on PYTHONPATH (got: $(printf '%s\n' "$out" | head -n1))" ;;
esac

out="$(prune_argv --dry-run --min-age-hours 2)"
check "other args are forwarded after the defaults" "$(argv_of "$out")" \
    "$TMP/collectors/testcol/prune.py --bronze-dir $TMP/data --dry-run --min-age-hours 2"

out="$(prune_argv --data-dir /elsewhere --dry-run)"
check "--data-dir retargets the bronze dir" "$(argv_of "$out")" \
    "$TMP/collectors/testcol/prune.py --bronze-dir /elsewhere --dry-run"
out="$(prune_argv --data-dir=/elsewhere)"
check "--data-dir=VALUE retargets the bronze dir" "$(argv_of "$out")" \
    "$TMP/collectors/testcol/prune.py --bronze-dir /elsewhere"

out="$(prune_argv --secrets-dir /s --secrets-dir=/t --dry-run)"
check "--secrets-dir is absorbed in both forms" "$(argv_of "$out")" \
    "$TMP/collectors/testcol/prune.py --bronze-dir $TMP/data --dry-run"

out="$(prune_argv --silver-db /x.db)"; rc=$?
check "--silver-db is rejected" "$rc" "2"
out="$(prune_argv --silver-db=/x.db)"; rc=$?
check "--silver-db=VALUE is rejected" "$rc" "2"

out="$(HAS_DEBUG=1 prune_argv --dry-run)"
check "HAS_DEBUG=1 adds the debug dir before the forwarded args" "$(argv_of "$out")" \
    "$TMP/collectors/testcol/prune.py --bronze-dir $TMP/data --debug-dir $TMP/debug --dry-run"
out="$(HAS_DEBUG=0 prune_argv)"
check "HAS_DEBUG=0 passes no debug dir" "$(argv_of "$out")" \
    "$TMP/collectors/testcol/prune.py --bronze-dir $TMP/data"

echo
echo "wrapper_host_prune tests: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
