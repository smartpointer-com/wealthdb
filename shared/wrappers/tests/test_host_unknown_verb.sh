#!/usr/bin/env bash
# Behaviour test: a host-venv wrapper rejects an unknown verb before it
# acts on disk. It exits 2, creates no data dir and sources no env file.
# Each wrapper runs with its dirs redirected into a temp dir, where an
# env file records being sourced.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
COLLECTORS="$HERE/../../../collectors"
PASS=0
FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ok   $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL $1"; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

unset WEALTHDB_DATA_ROOT
export XDG_DATA_HOME="$TMP/data"
export XDG_CACHE_HOME="$TMP/cache"
export WEALTHDB_SECRETS_DIR="$TMP/secrets"
mkdir -p "$WEALTHDB_SECRETS_DIR"

echo "host-venv wrappers, unknown verb:"
for name in curves fred manual plaid refdata schwab-api svb ubs-psn; do
    prefix="$(printf '%s' "$name" | tr 'a-z-' 'A-Z_')"
    printf ': > "%s"\n' "$TMP/sourced-$name" > "$WEALTHDB_SECRETS_DIR/$name.env"
    out="$(env -u "${prefix}_SECRETS_DIR" -u "${prefix}_DATA_DIR" -u "${prefix}_ENV_FILE" \
        "$COLLECTORS/$name/$name" no-such-verb 2>&1)"; rc=$?
    [[ $rc -eq 2 ]] && ok "$name exits 2" || bad "$name exit=$rc ($out)"
    grep -q "unknown command 'no-such-verb'" <<<"$out" \
        && ok "$name names the verb" || bad "$name message: $out"
    [[ -e "$XDG_DATA_HOME/wealthdb/$name" ]] \
        && bad "$name created its data dir" || ok "$name creates no data dir"
    [[ -e "$TMP/sourced-$name" ]] \
        && bad "$name sourced its env file" || ok "$name sources no env file"
done

echo
echo "unknown verb tests: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
