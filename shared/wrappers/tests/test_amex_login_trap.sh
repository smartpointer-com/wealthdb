#!/usr/bin/env bash
# Behaviour test for the amex wrapper's `login` trap: a bare `login` is a
# host-side no-op that never starts a container, while `login --check` passes
# through. Runs the wrapper with a stubbed docker on PATH, so nothing real is
# invoked.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
WRAPPER="${1:-$HERE/../../../collectors/amex/amex}"
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ok   $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL $1"; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# A docker stub that records that it was called.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/docker" <<'STUB'
#!/usr/bin/env bash
# Records the SUBCOMMAND, not just that docker was reached: the wrapper's
# safety guard runs `docker ps` before any verb, so "docker was called" is
# satisfied even by a verb that was trapped.
echo "$1" >> "$DOCKER_CALLS"
exit 0
STUB
chmod +x "$TMP/bin/docker"
export DOCKER_CALLS="$TMP/calls"
: > "$DOCKER_CALLS"
export PATH="$TMP/bin:$PATH"

# Everything the wrapper resolves is redirected into $TMP so the test writes
# nowhere else: wrapper_check_mounts mkdir -p's the data, startupCache and
# debug dirs, and errors out unless the secrets dir already exists. The
# per-collector overrides are cleared so an ambient one cannot steer a mount
# back out of the sandbox.
unset AMEX_SECRETS_DIR AMEX_DATA_DIR AMEX_DEBUG_DIR WEALTHDB_DATA_ROOT
export XDG_DATA_HOME="$TMP/xdgdata"
export XDG_CACHE_HOME="$TMP/xdgcache"
export WEALTHDB_SECRETS_DIR="$TMP/secrets"
mkdir -p "$WEALTHDB_SECRETS_DIR"

out="$("$WRAPPER" login 2>&1)"; rc=$?
[[ $rc -eq 0 ]] && ok "bare login exits 0" || bad "bare login exit=$rc"
grep -q "folds into 'download'" <<<"$out" \
    && ok "bare login explains itself" || bad "bare login message: $out"
grep -qx "run" "$DOCKER_CALLS" \
    && bad "bare login started a container" \
    || ok "bare login starts no container"

: > "$DOCKER_CALLS"
"$WRAPPER" login --check >/dev/null 2>&1
grep -qx "run" "$DOCKER_CALLS" \
    && ok "login --check runs the container" \
    || bad "login --check never reached docker run"

echo "amex wrapper tests: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
