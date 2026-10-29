#!/usr/bin/env bash
# Behaviour tests for host-lib's small helpers: host_has_flag,
# host_print_banner and the SILVER_DB_GIVEN that host_resolve_dirs sets.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
sandbox_env "$TMP"

# shellcheck disable=SC1091
source "$HERE/../host-lib.sh"

echo "host_has_flag:"
host_has_flag --check --dry-run --check && ok "finds the flag among others" \
    || bad "misses a flag among others"
host_has_flag --check --checks x && bad "matches a longer word" \
    || ok "matches whole arguments only"
host_has_flag --check "a --check b" && bad "matches inside an argument" \
    || ok "does not look inside an argument"
host_has_flag --check && bad "matches an empty list" \
    || ok "an empty list holds no flag"

echo "host_print_banner:"
printf '#!/bin/bash\n# first line\n#\n#   indented\nset -e\n# not banner\n' > "$TMP/wrapper"
banner="$(host_print_banner "$TMP/wrapper")"
[[ "$banner" == $'first line\n\n  indented' ]] && ok "prints the header up to the first code line" \
    || bad "banner was: $banner"

echo "SILVER_DB_GIVEN:"
# given ARGS...: SILVER_DB_GIVEN after host_resolve_dirs load ARGS.
given() {
    (
        NAME=testcol; ENV_PREFIX=TESTCOL
        host_resolve_dirs load "$@" >/dev/null 2>&1 || exit 1
        echo "$SILVER_DB_GIVEN"
    )
}
[[ "$(given)" == 0 ]] && ok "no flag and no variable: 0" || bad "default: $(given)"
[[ "$(given --silver-db "$TMP/x.db")" == 1 ]] && ok "--silver-db: 1" \
    || bad "--silver-db: $(given --silver-db "$TMP/x.db")"
[[ "$(given --silver-db="$TMP/x.db")" == 1 ]] && ok "--silver-db=PATH: 1" \
    || bad "--silver-db=PATH: $(given --silver-db="$TMP/x.db")"
[[ "$(TESTCOL_SILVER_DB="$TMP/x.db" given)" == 1 ]] && ok "TESTCOL_SILVER_DB: 1" \
    || bad "TESTCOL_SILVER_DB: $(TESTCOL_SILVER_DB="$TMP/x.db" given)"

summary "host helper tests"
