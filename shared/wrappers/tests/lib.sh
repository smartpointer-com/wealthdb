# Shared helpers for the wrapper tests. A test sources this file; it is
# not a test itself (the Makefile runs only test_*.sh).

PASS=0
FAIL=0

ok()  { PASS=$((PASS+1)); echo "  ok   $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL $1"; }

# has DESC NEEDLE TEXT: some line of TEXT contains NEEDLE.
has()   { if printf '%s\n' "$3" | grep -qF -- "$2"; then ok "$1"; else bad "$1 (no '$2' in: $3)"; fi; }
lacks() { if printf '%s\n' "$3" | grep -qF -- "$2"; then bad "$1 ('$2' in: $3)"; else ok "$1"; fi; }

# has_item DESC ITEM ARGV: ARGV, one item per line, holds ITEM as a whole
# item. The failure message prints ARGV on one line.
has_item() {
    if printf '%s\n' "$3" | grep -qxF -- "$2"; then ok "$1"; else bad "$1 (no '$2' in: $(printf '%s ' $3))"; fi
}
lacks_item() {
    if printf '%s\n' "$3" | grep -qxF -- "$2"; then bad "$1 ('$2' in: $(printf '%s ' $3))"; else ok "$1"; fi
}

# summary LABEL: prints the tally and fails when any case failed. It is
# the last command of a test, so its status is the test's.
summary() {
    echo "$1: $PASS passed, $FAIL failed"
    [ "$FAIL" -eq 0 ]
}

# sandbox_env DIR: keeps a wrapper away from the live tree. It unsets the
# data and secrets roots and the TESTCOL_* dir overrides, and points the
# XDG data, cache and config dirs into DIR.
sandbox_env() {
    unset WEALTHDB_DATA_ROOT WEALTHDB_SECRETS_DIR \
        TESTCOL_DATA_DIR TESTCOL_SECRETS_DIR TESTCOL_SILVER_DB \
        TESTCOL_DEBUG_DIR TESTCOL_ENV_FILE
    export XDG_DATA_HOME="$1/xdgdata"
    export XDG_CACHE_HOME="$1/xdgcache"
    export XDG_CONFIG_HOME="$1/xdgconfig"
}
