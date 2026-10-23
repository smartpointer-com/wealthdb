#!/usr/bin/env bash
# Behaviour test: host_source_env_file sources the collector's own
# <secrets-dir>/<name>.env, or ${ENV_PREFIX}_ENV_FILE, with every
# assignment exported. Given a file name, it sources that file from the
# same dir, or the path in <FILE in upper snake case>_ENV_FILE. refdata
# reads FINRA's credential this way. A missing file is fine. One that does
# not parse is skipped with a warning that never quotes it.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PASS=0
FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ok   $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL $1"; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
unset WEALTHDB_DATA_ROOT TESTCOL_SECRETS_DIR TESTCOL_DATA_DIR TESTCOL_ENV_FILE \
      OTHER_SRC_ENV_FILE TESTCOL_LOGIN OTHER_SRC_KEY

mkdir -p "$TMP/secrets" "$TMP/flagdir"
echo "TESTCOL_LOGIN='own file'" > "$TMP/secrets/testcol.env"
echo "OTHER_SRC_KEY='other file'" > "$TMP/secrets/other-src.env"
echo "OTHER_SRC_KEY='flag dir'" > "$TMP/flagdir/other-src.env"
echo "OTHER_SRC_KEY='named file'" > "$TMP/named.env"
printf 'OTHER_SRC_KEY=SYNTHETIC-VALUE)x\n' > "$TMP/broken.env"

# run FILE [args...]: resolve the dirs of a collector `testcol` from
# args, source its env file (FILE empty) or FILE's, and print what a
# child process sees.
run() {
    (
        export XDG_DATA_HOME="$TMP/xdgdata"
        export WEALTHDB_SECRETS_DIR="$TMP/secrets"
        NAME=testcol; ENV_PREFIX=TESTCOL
        # shellcheck disable=SC1091
        source "$HERE/../host-lib.sh"
        file="$1"; shift
        host_resolve_dirs download "$@"
        if [[ -n "$file" ]]; then host_source_env_file "$file"; else host_source_env_file; fi
        bash -c 'printf "login=%s key=%s\n" "${TESTCOL_LOGIN:-<unset>}" "${OTHER_SRC_KEY:-<unset>}"'
    )
}

# is DESC WANT GOT
is() {
    if [[ "$3" == "$2" ]]; then ok "$1"; else bad "$1 (want '$2', got '$3')"; fi
}

echo "host_source_env_file:"

is "the collector's own file is sourced and exported" \
   "login=own file key=<unset>" "$(run "" 2>&1)"
is "the collector's variable names another path for its own file" \
   "login=<unset> key=named file" "$(TESTCOL_ENV_FILE="$TMP/named.env" run "" 2>&1)"
is "a named file is sourced from the same dir" \
   "login=<unset> key=other file" "$(run other-src 2>&1)"
is "--secrets-dir names the dir a named file is read from" \
   "login=<unset> key=flag dir" "$(run other-src --secrets-dir "$TMP/flagdir" 2>&1)"
is "the named file's own variable names another path" \
   "login=<unset> key=named file" "$(OTHER_SRC_ENV_FILE="$TMP/named.env" run other-src 2>&1)"
is "the collector's variable does not move a named file" \
   "login=<unset> key=other file" "$(TESTCOL_ENV_FILE="$TMP/named.env" run other-src 2>&1)"
is "a missing named file is fine" \
   "login=<unset> key=<unset>" "$(run no-such-src 2>&1)"

out="$(OTHER_SRC_ENV_FILE="$TMP/broken.env" run other-src 2>&1)"
if [[ "$out" == *"has a bash syntax error; not sourcing it"* && "$out" == *"key=<unset>"* ]]; then
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
echo "host_source_env_file tests: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
