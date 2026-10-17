#!/usr/bin/env bash
# Behaviour test: under HAS_ENV_FILE=1, wrapper_main sources the
# credentials env file host-side before `docker run`, so the ENV_VARS it
# sets are forwarded. The file is <secrets-dir>/<name>.env, read after
# the --secrets-dir flag is resolved, or ${ENV_PREFIX}_ENV_FILE. The
# relevate wrapper reads its credentials through it.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PASS=0
FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ok   $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL $1"; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
unset WEALTHDB_DATA_ROOT TESTCOL_SECRETS_DIR TESTCOL_DATA_DIR TESTCOL_ENV_FILE \
      TESTCOL_LOGIN HAS_ENV_FILE

# A fake `docker` that prints the forwarded variable the way the
# container would see it, so wrapper_main's `exec docker run ...` is
# captured rather than really launching a container.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf 'login=%s\n' "${TESTCOL_LOGIN:-<unset>}"
EOF
chmod +x "$TMP/bin/docker"

mkdir -p "$TMP/secrets" "$TMP/other" "$TMP/work"
echo "TESTCOL_LOGIN='from default'" > "$TMP/secrets/testcol.env"
echo "TESTCOL_LOGIN='from flag dir'" > "$TMP/other/testcol.env"
echo "TESTCOL_LOGIN='from named file'" > "$TMP/named.env"
printf 'TESTCOL_LOGIN=SYNTHETIC-VALUE)x\n' > "$TMP/broken.env"

# run [args...]: drive wrapper_main for a collector with one forwarded
# credential, from a working dir under $TMP, printing what docker saw.
run() {
    (
        PATH="$TMP/bin:$PATH"
        export XDG_CACHE_HOME="$TMP/xdgcache"
        export XDG_DATA_HOME="$TMP/xdgdata"
        export WEALTHDB_SECRETS_DIR="$TMP/secrets"
        cd "$TMP/work" || exit 1
        NAME=testcol; ENV_PREFIX=TESTCOL; ENV_VARS=(TESTCOL_LOGIN)
        # shellcheck disable=SC1091
        source "$HERE/../wrapper-lib.sh"
        wrapper_init
        wrapper_main "$@"
    )
}

# is DESC WANT GOT
is() {
    if [[ "$3" == "$2" ]]; then ok "$1"; else bad "$1 (want '$2', got '$3')"; fi
}

echo "wrapper_source_env_file:"

is "without HAS_ENV_FILE nothing is sourced" \
   "login=<unset>" "$(run login 2>&1)"
is "the default secrets dir's file is sourced" \
   "login=from default" "$(HAS_ENV_FILE=1 run login 2>&1)"
is "--secrets-dir names the dir the file is read from" \
   "login=from flag dir" "$(HAS_ENV_FILE=1 run login --secrets-dir "$TMP/other" 2>&1)"
is "--secrets-dir=DIR does too" \
   "login=from flag dir" "$(HAS_ENV_FILE=1 run login --secrets-dir="$TMP/other" 2>&1)"
is "TESTCOL_ENV_FILE names another file" \
   "login=from named file" "$(HAS_ENV_FILE=1 TESTCOL_ENV_FILE="$TMP/named.env" run login 2>&1)"
is "a missing file is fine" \
   "login=<unset>" "$(HAS_ENV_FILE=1 TESTCOL_ENV_FILE="$TMP/none.env" run login 2>&1)"

out="$(HAS_ENV_FILE=1 TESTCOL_ENV_FILE="$TMP/broken.env" run login 2>&1)"
if [[ "$out" == *"has a bash syntax error; not sourcing it"* \
      && "$out" == *"login=<unset>"* ]]; then
    ok "a file that does not parse is skipped with a warning"
else
    bad "broken file: $out"
fi
if [[ "$out" != *"SYNTHETIC-VALUE"* ]]; then
    ok "the warning never quotes the file's line"
else
    bad "warning quotes the line: $out"
fi

echo
echo "wrapper_source_env_file tests: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
