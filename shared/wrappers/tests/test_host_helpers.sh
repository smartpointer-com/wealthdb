#!/usr/bin/env bash
# Behaviour tests for host-lib's helpers: host_has_flag,
# host_print_banner, the SILVER_DB_GIVEN that host_resolve_dirs sets,
# and host_begin, host_run and host_login_probe, run in a small wrapper
# whose interpreter is a stub that prints its argv.
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

echo "host_begin, host_run, host_login_probe:"
mkdir -p "$TMP/col/.venv/bin"
printf '#!/bin/sh\nprintf "%%s\\n" "$@"\n' > "$TMP/col/.venv/bin/python"
chmod +x "$TMP/col/.venv/bin/python"
cat > "$TMP/col/col" <<EOF
#!/usr/bin/env bash
# col: the banner.
set -euo pipefail
HERE="\$(cd "\$(dirname "\$0")" && pwd)"
NAME=col
source "$HERE/../host-lib.sh"
host_begin login load -- "\$@"
case "\$VERB" in
    login)
        host_login_probe probe.py --key "\$SECRETS_DIR/k"
        echo "col: no-op." >&2
        ;;
    load) host_run load.py --bronze-dir "\$DATA_DIR" ;;
esac
EOF
chmod +x "$TMP/col/col"
col() { "$TMP/col/col" "$@" 2>&1; }
check() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi; }
check "no verb prints the banner" "$(col)" "col: the banner."
check "-h prints the banner" "$(col -h)" "col: the banner."
check "an unknown verb names the verbs" "$(col bogus)" \
    "col: unknown command 'bogus' (expected: login|load)"
col bogus >/dev/null; check "an unknown verb exits 2" "$?" 2
[[ -e "$XDG_DATA_HOME/wealthdb/col" ]] && bad "an unknown verb made the data dir" \
    || ok "an unknown verb makes no data dir"
check "host_run passes its args, then the forwarded ones" \
    "$(col load --data-dir "$TMP/d" --force | tr '\n' ' ')" \
    "$TMP/col/load.py --bronze-dir $TMP/d --force "
check "an unset ENV_PREFIX comes from NAME" \
    "$(COL_DATA_DIR="$TMP/e" col load | tr '\n' ' ')" "$TMP/col/load.py --bronze-dir $TMP/e "
check "a bare login prints the notice" "$(col login)" "col: no-op."
col login >/dev/null; check "a bare login exits 0" "$?" 0
check "login --check runs the probe" \
    "$(col login --check --secrets-dir "$TMP/s" | tr '\n' ' ')" \
    "$TMP/col/probe.py --key $TMP/s/k --check "

summary "host helper tests"
