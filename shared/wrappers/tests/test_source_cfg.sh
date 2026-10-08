#!/usr/bin/env bash
# Behaviour test: wrapper_source_cfg sources a collector's optional
# config file the way AGENTS.md §3 reads an env file (bash -n first,
# then every assignment exported), before wrapper_init, so the file can
# set both forwarded settings and the per-prefix knobs. The relevate and
# cointracking wrappers read theirs through it.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
PASS=0
FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ok   $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL $1"; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Source the lib, read the config, run wrapper_init, then print what a
# caller sees. Runs in a subshell so a failing config cannot leak.
resolve() {
    (
        NAME=testcol; ENV_PREFIX=TESTCOL; ENV_VARS=()
        unset WEALTHDB_SECRETS_DIR WEALTHDB_DATA_ROOT TESTCOL_DATA_DIR
        # shellcheck disable=SC1091
        source "$HERE/../wrapper-lib.sh"
        wrapper_source_cfg TESTCOL "$TMP/default.cfg"
        wrapper_init
        printf 'cfg=%s\n' "$TESTCOL_CFG"
        printf 'data=%s\n' "$HOST_DATA"
        printf 'setting=%s\n' "${TESTCOL_SETTING:-}"
        printf 'exported=%s\n' "$(env | grep -c '^TESTCOL_SETTING=')"
    )
}

echo "wrapper_source_cfg:"

out="$(resolve 2>&1)"
if [[ "$out" == *"cfg=$TMP/default.cfg"* && "$out" == *"setting="$'\n'* ]]; then
    ok "a missing file holds no settings; the default path is kept"
else
    bad "missing file: $out"
fi

cat > "$TMP/default.cfg" <<EOF
TESTCOL_SETTING='a value with spaces'
TESTCOL_DATA_DIR=$TMP/data
EOF
out="$(resolve 2>&1)"
if [[ "$out" == *"setting=a value with spaces"* && "$out" == *"exported=1"* ]]; then
    ok "a plain KEY=VALUE line is set and exported"
else
    bad "default file: $out"
fi
if [[ "$out" == *"data=$TMP/data"* ]]; then
    ok "the file sets a per-prefix knob wrapper_init reads"
else
    bad "per-prefix knob: $out"
fi

printf 'TESTCOL_SETTING=from-override\n' > "$TMP/override.cfg"
out="$(TESTCOL_CFG="$TMP/override.cfg" resolve 2>&1)"
if [[ "$out" == *"cfg=$TMP/override.cfg"* && "$out" == *"setting=from-override"* ]]; then
    ok "\$TESTCOL_CFG names another file"
else
    bad "override: $out"
fi

printf 'TESTCOL_SETTING=never\nif then\n' > "$TMP/broken.cfg"
out="$(TESTCOL_CFG="$TMP/broken.cfg" resolve 2>&1)"
rc=$?
if [[ $rc -ne 0 && "$out" == *"syntax error"* && "$out" != *"setting=never"* ]]; then
    ok "a file that does not parse stops the wrapper unsourced"
else
    bad "broken file (rc=$rc): $out"
fi

# The wrappers' help path runs wrapper_init and prints the mounts, so a
# config file's data dir shows there with no docker involved.
for c in relevate cointracking; do
    prefix="$(grep '^ENV_PREFIX=' "$REPO/collectors/$c/$c" | cut -d= -f2)"
    printf '%s_DATA_DIR=%s\n' "$prefix" "$TMP/$c-data" > "$TMP/$c.cfg"
    out="$(env -u WEALTHDB_DATA_ROOT -u "${prefix}_DATA_DIR" \
           "${prefix}_CFG=$TMP/$c.cfg" "$REPO/collectors/$c/$c" help 2>&1)"
    if [[ "$out" == *"$TMP/$c-data"* ]]; then
        ok "$c reads its config file before wrapper_init"
    else
        bad "$c help did not show the config's data dir: $out"
    fi
done

echo
echo "wrapper_source_cfg tests: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
