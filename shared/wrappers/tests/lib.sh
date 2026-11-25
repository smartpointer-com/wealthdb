# Shared helpers for the wrapper tests. A test sources this file; it is
# not a test itself (the Makefile runs only test_*.sh).

PASS=0
FAIL=0

# The wrappers under test run the container engine, whatever ./configure
# recorded for this checkout (engine-mode.sh). A test of the native
# engine names a record of its own.
export WEALTHDB_BUILD_CONFIG=/dev/null
# No ambient config reaches a wrapper, nor the words a setup of its own
# prints its commands with: every case runs the default config unless it
# names another.
unset WEALTHDB_CONFIG WEALTHDB_COLLECT WEALTHDB_PROGRAM

ok()  { PASS=$((PASS+1)); echo "  ok   $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL $1"; }

# has DESC NEEDLE TEXT: some line of TEXT contains NEEDLE.
has()   { if grep -qF -- "$2" <<<"$3"; then ok "$1"; else bad "$1 (no '$2' in: $3)"; fi; }
lacks() { if grep -qF -- "$2" <<<"$3"; then bad "$1 ('$2' in: $3)"; else ok "$1"; fi; }

# has_item DESC ITEM ARGV: ARGV, one item per line, holds ITEM as a whole
# item. The failure message prints ARGV on one line.
has_item() {
    if grep -qxF -- "$2" <<<"$3"; then ok "$1"; else bad "$1 (no '$2' in: $(printf '%s ' $3))"; fi
}
lacks_item() {
    if grep -qxF -- "$2" <<<"$3"; then bad "$1 ('$2' in: $(printf '%s ' $3))"; else ok "$1"; fi
}

# summary LABEL: prints the tally and fails when any case failed. It is
# the last command of a test, so its status is the test's.
summary() {
    echo "$1: $PASS passed, $FAIL failed"
    [ "$FAIL" -eq 0 ]
}

# stub_docker DIR: writes a fake `docker` into DIR. It answers `ps` with
# no container and `image inspect` with an image's id, so a wrapper
# finds its image, starts nothing and exits 0. With STUB_NO_IMAGE it
# holds no image, and with STUB_DOWN it is a Docker that does not
# answer. When STUB_LOG names a file, each call is appended to
# it: "docker", then the argv one item per line, then, for each `-e NAME`
# that passes a name only, the value NAME holds in the stub's
# environment, since that value is what would reach the container.
stub_docker() {
    mkdir -p "$1"
    cat > "$1/docker" <<'STUB'
#!/usr/bin/env bash
if [ -n "${STUB_LOG:-}" ]; then
    {
        echo docker
        if [ $# -gt 0 ]; then printf '  %s\n' "$@"; fi
        prev=
        for a in "$@"; do
            if [ "$prev" = -e ] && [[ "$a" != *=* ]]; then
                echo "  [env] $a=${!a-<unset>}"
            fi
            prev="$a"
        done
    } >> "$STUB_LOG"
fi
if [ -n "${STUB_DOWN:-}" ]; then
    echo "failed to connect to the docker API at unix:///var/run/docker.sock; check if the path is correct and if the daemon is running" >&2
    echo "a second line of Docker's" >&2
    exit 1
fi
if [ "${1:-}" = image ]; then
    if [ -n "${STUB_NO_IMAGE:-}" ]; then
        echo "Error response from daemon: No such image: ${*: -1}" >&2
        exit 1
    fi
    echo sha256:stub
fi
exit 0
STUB
    chmod +x "$1/docker"
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
