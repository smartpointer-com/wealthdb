#!/usr/bin/env bash
# Behaviour test: the docker wrappers mount a non-secrets cache dir for the
# browsers' relocated startupCache and point the container at it via
# WEALTHDB_STARTUPCACHE_DIR — so Firefox's regenerable compiled-bytecode
# cache lands there instead of next to the session cookie under /secrets.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PASS=0
FAIL=0
ok()  { PASS=$((PASS+1)); echo "  ok   $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL $1"; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# A fake `docker` that prints its argv (one item per line) and exits, so
# wrapper_main's `exec docker run ...` is captured rather than really
# launching a container.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@"
EOF
chmod +x "$TMP/bin/docker"

# Drive wrapper_main for a representative browser collector, capturing the
# docker argv. Everything is kept inside $TMP so the test writes nowhere
# else; HOST_SECRETS must exist (wrapper_check_mounts errors otherwise).
docker_argv() {
    (
        PATH="$TMP/bin:$PATH"
        export XDG_CACHE_HOME="$TMP/xdgcache"
        export XDG_DATA_HOME="$TMP/xdgdata"
        unset WEALTHDB_SECRETS_DIR WEALTHDB_DATA_ROOT
        NAME=testcol; ENV_PREFIX=TESTCOL; ENV_VARS=()
        HOST_SECRETS="$TMP/secrets"; mkdir -p "$HOST_SECRETS"
        # shellcheck disable=SC1091
        source "$HERE/../wrapper-lib.sh"
        wrapper_init
        wrapper_main login
    )
}

echo "wrapper-lib startupCache mount:"
out="$(docker_argv)"
cache="$TMP/xdgcache/wealthdb/startupcache"

if printf '%s\n' "$out" | grep -qxF "$cache:/cache/startupcache"; then
    ok "mounts the XDG cache dir at /cache/startupcache"
else
    bad "startupcache mount missing (got: $out)"
fi

if printf '%s\n' "$out" | grep -qxF "WEALTHDB_STARTUPCACHE_DIR=/cache/startupcache"; then
    ok "container env points at the mount"
else
    bad "WEALTHDB_STARTUPCACHE_DIR env missing"
fi

# The cache must not be co-located under the /secrets mount.
if printf '%s\n' "$out" | grep -q "/secrets/[^ ]*startupcache"; then
    bad "cache must not live under /secrets"
else
    ok "cache is not under /secrets"
fi

# The host cache dir is created up front (docker would otherwise make it
# root-owned).
if [[ -d "$cache" ]]; then
    ok "host cache dir created"
else
    bad "host cache dir not created"
fi

echo
echo "startupcache wrapper tests: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
