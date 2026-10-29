#!/usr/bin/env bash
# Behaviour test of the docker argv wrapper_main composes:
#   - a non-secrets cache dir is mounted for the browsers' relocated
#     startupCache, and WEALTHDB_STARTUPCACHE_DIR points the container at
#     it, so Firefox's regenerable compiled-bytecode cache lands there
#     instead of next to the session cookie under /secrets;
#   - every mount source is an absolute host path, since `docker run -v`
#     reads a bare relative source as a named volume;
#   - the VNC port is forwarded only for the VNC_SUBCOMMANDS, on the first
#     free host port.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
# No ambient override may steer a mount out of the sandbox; a case sets
# the one it tests.
unset WEALTHDB_DATA_ROOT TESTCOL_SECRETS_DIR TESTCOL_DATA_DIR TESTCOL_DEBUG_DIR \
      TESTCOL_SILVER_DB HAS_VNC VNC_SUBCOMMANDS

# A fake `docker` that prints its argv (one item per line) and exits, so
# wrapper_main's `exec docker run ...` is captured rather than really
# launching a container. A fake `lsof` reports host port 5900 as taken.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@"
EOF
cat > "$TMP/bin/lsof" <<'EOF'
#!/usr/bin/env bash
[[ " $* " == *" -iTCP:5900 "* ]]
EOF
chmod +x "$TMP/bin/docker" "$TMP/bin/lsof"

# docker_argv [args...]: drive wrapper_main for a representative browser
# collector with args (default: login), from a working dir under $TMP,
# capturing the docker argv. Everything is kept inside $TMP so the test
# writes nowhere else; the secrets dir must exist (wrapper_check_mounts
# errors otherwise).
docker_argv() {
    (
        PATH="$TMP/bin:$PATH"
        export XDG_CACHE_HOME="$TMP/xdgcache"
        export XDG_DATA_HOME="$TMP/xdgdata"
        export WEALTHDB_SECRETS_DIR="$TMP/secrets"
        mkdir -p "$TMP/secrets" "$TMP/work"
        cd "$TMP/work" || exit 1
        NAME=testcol; ENV_PREFIX=TESTCOL; ENV_VARS=()
        # shellcheck disable=SC1091
        source "$HERE/../wrapper-lib.sh"
        wrapper_init
        wrapper_main "${@:-login}"
    )
}

echo "wrapper-lib startupCache mount:"
out="$(docker_argv)"
cache="$TMP/xdgcache/wealthdb/startupcache"
has_item "mounts the XDG cache dir at /cache/startupcache" "$cache:/cache/startupcache" "$out"
has_item "container env points at the mount" "WEALTHDB_STARTUPCACHE_DIR=/cache/startupcache" "$out"

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

echo "wrapper-lib relative mount sources:"
out="$(docker_argv load --data-dir mydata --silver-db silver/x.db)"
has_item "a relative --data-dir mounts as an absolute host path" "$TMP/work/mydata:/data" "$out"
has_item "a relative --silver-db parent mounts as an absolute host path" "$TMP/work/silver:/silver" "$out"
has_item "--silver-db is rewritten to the /silver path" "/silver/x.db" "$out"
mkdir -p "$TMP/work/mysecrets"
out="$(docker_argv login --secrets-dir=mysecrets)"
has_item "a relative --secrets-dir mounts as an absolute host path" "$TMP/work/mysecrets:/secrets" "$out"
out="$(TESTCOL_DATA_DIR=envdata docker_argv login)"
has_item "a relative TESTCOL_DATA_DIR mounts as an absolute host path" "$TMP/work/envdata:/data" "$out"

echo "wrapper-lib VNC port:"
out="$(HAS_VNC=1 docker_argv vnc-login 2>/dev/null)"
has_item "vnc-login gets the first free host port" "127.0.0.1:5901:5900" "$out"
has_item "the container learns the host port" "VNC_HOST_PORT=5901" "$out"
out="$(HAS_VNC=1 docker_argv login)"
lacks_item "login gets no VNC port by default" "VNC_HOST_PORT=5901" "$out"
out="$(HAS_VNC=1 VNC_SUBCOMMANDS=(explore login); docker_argv login 2>/dev/null)"
has_item "VNC_SUBCOMMANDS names the verbs that get one" "VNC_HOST_PORT=5901" "$out"
out="$(HAS_VNC=1 VNC_SUBCOMMANDS=(explore login); docker_argv vnc-login)"
lacks_item "VNC_SUBCOMMANDS replaces the default" "VNC_HOST_PORT=5901" "$out"

echo
summary "wrapper_main docker argv tests"
