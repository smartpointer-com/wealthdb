#!/usr/bin/env bash
# Behaviour test of the docker argv wrapper_main composes:
#   - a non-secrets cache dir is mounted for the browsers' relocated
#     startupCache, and WEALTHDB_STARTUPCACHE_DIR points the container at
#     it, so Firefox's regenerable compiled-bytecode cache lands there
#     instead of next to the session cookie under /secrets;
#   - the collector's own cache dir is mounted at /cache/collector, made
#     kept 0700, and WEALTHDB_CACHE_DIR names it in the container;
#   - every mount source is an absolute host path, since `docker run -v`
#     reads a bare relative source as a named volume;
#   - the VNC port is forwarded only for the VNC_SUBCOMMANDS, on the first
#     free host port;
#   - ENV_PREFIX is derived from NAME when the wrapper does not set it;
#   - the words a setup of its own prints its commands with reach the
#     container by name, and the default config's argv holds neither;
#   - a run never pulls, and a missing image stops the verb with the
#     command that builds it;
#   - a verb that cannot have its container stops with status 2 and one
#     message, before any folder is made: in an installed copy whatever
#     Docker says, and in a checkout when Docker does not answer or is
#     not installed. A run that has its image asks Docker once.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
# No ambient override may steer a mount out of the sandbox; a case sets
# the one it tests.
unset WEALTHDB_DATA_ROOT TESTCOL_SECRETS_DIR TESTCOL_DATA_DIR TESTCOL_DEBUG_DIR \
      TESTCOL_SILVER_DB VNC_SUBCOMMANDS

# A fake `docker` that prints its argv (one item per line) and exits, so
# wrapper_main's `exec docker run ...` is captured rather than really
# launching a container. With STUB_NO_IMAGE it holds no image, and with
# STUB_DOWN it is a Docker that does not answer, in two lines. When
# STUB_CALLS names a file, the first two words of each call are appended
# to it. A fake `lsof` reports host port 5900 as taken.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/docker" <<'EOF'
#!/usr/bin/env bash
if [[ -n "${STUB_CALLS:-}" ]]; then echo "${1:-} ${2:-}" >> "$STUB_CALLS"; fi
if [[ -n "${STUB_DOWN:-}" ]]; then
    echo "failed to connect to the docker API at unix:///var/run/docker.sock; check if the path is correct and if the daemon is running" >&2
    echo "a second line of Docker's" >&2
    exit 1
fi
if [[ "${1:-} ${2:-}" == "image inspect" && -n "${STUB_NO_IMAGE:-}" ]]; then
    echo "Error response from daemon: No such image: ${*: -1}" >&2
    exit 1
fi
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
        NAME="${TEST_NAME:-testcol}"; ENV_PREFIX="${TEST_PREFIX-TESTCOL}"; ENV_VARS=()
        # TEST_LIB names the wrappers' folder of another tree, and
        # STUB_NO_CLIENT is a computer with no docker program on PATH.
        # shellcheck disable=SC1091
        source "${TEST_LIB:-$HERE/..}/wrapper-lib.sh"
        if [[ -n "${STUB_NO_CLIENT:-}" ]]; then
            command() {
                if [[ "$1" == -v && "$2" == docker ]]; then return 1; fi
                builtin command "$@"
            }
        fi
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
if grep -q "/secrets/[^ ]*startupcache" <<<"$out"; then
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

echo "wrapper-lib collector cache mount:"
own="$TMP/xdgcache/wealthdb/testcol"
has_item "mounts the collector's XDG cache dir at /cache/collector" "$own:/cache/collector" "$out"
has_item "container env names the collector's cache root" "WEALTHDB_CACHE_DIR=/cache/collector" "$out"
if [[ -d "$own" && "$(ls -ld "$own" | cut -c1-10)" == "drwx------" ]]; then
    ok "collector cache dir created 0700"
else
    bad "collector cache dir not created 0700"
fi
chmod 750 "$own"
docker_argv >/dev/null
if [[ "$(ls -ld "$own" | cut -c1-10)" == "drwx------" ]]; then
    ok "an existing collector cache dir is made 0700"
else
    bad "an existing collector cache dir was not made 0700"
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
out="$(VNC_SUBCOMMANDS=(vnc-login); docker_argv vnc-login 2>/dev/null)"
has_item "vnc-login gets the first free host port" "127.0.0.1:5901:5900" "$out"
has_item "the container learns the host port" "VNC_HOST_PORT=5901" "$out"
out="$(VNC_SUBCOMMANDS=(vnc-login); docker_argv login)"
lacks_item "a verb not named gets no VNC port" "VNC_HOST_PORT=5901" "$out"
out="$(VNC_SUBCOMMANDS=(explore login); docker_argv login 2>/dev/null)"
has_item "VNC_SUBCOMMANDS names the verbs that get one" "VNC_HOST_PORT=5901" "$out"
out="$(docker_argv vnc-login)"
lacks_item "no VNC_SUBCOMMANDS, no VNC port" "VNC_HOST_PORT=5901" "$out"
out="$(VNC_SUBCOMMANDS=(); docker_argv vnc-login)"
lacks_item "an empty VNC_SUBCOMMANDS, no VNC port" "VNC_HOST_PORT=5901" "$out"

echo "wrapper-lib ENV_PREFIX:"
out="$(TEST_NAME=test-col TEST_PREFIX='' TEST_COL_DATA_DIR=derived docker_argv login)"
has_item "an unset ENV_PREFIX is NAME in upper snake case" "$TMP/work/derived:/data" "$out"

echo "wrapper-lib command words:"
out="$(docker_argv login)"
lacks "the default config passes no WEALTHDB_COLLECT" "WEALTHDB_COLLECT" "$out"
lacks "...and no WEALTHDB_PROGRAM" "WEALTHDB_PROGRAM" "$out"
out="$(WEALTHDB_COLLECT= WEALTHDB_PROGRAM= docker_argv login)"
lacks "empty ones are not passed either" "WEALTHDB_" "$(grep -e COLLECT -e PROGRAM <<<"$out")"
out="$(WEALTHDB_COLLECT='wealthdb -c /x/w.cfg collect' WEALTHDB_PROGRAM='wealthdb -c /x/w.cfg' docker_argv login)"
for v in WEALTHDB_COLLECT WEALTHDB_PROGRAM; do
    [[ "$(grep -B1 -x "$v" <<<"$out")" == $'-e\n'"$v" ]] && ok "a setup of its own passes $v by name" ||
        bad "no -e $v in: $(printf '%s ' $out)"
done
lacks "...never their value in the argv" "/x/w.cfg" "$out"

echo "wrapper-lib image:"
has_item "a run never pulls" "--pull=never" "$(docker_argv login)"
out="$(STUB_NO_IMAGE=1 WEALTHDB_IMAGE_TAG=t docker_argv login 2>&1)"; rc=$?
[[ "$rc" == 2 ]] && ok "a missing image exits 2" || bad "a missing image exits $rc"
has "...naming the make target that builds it and its bases" \
    "testcol: the image wealthdb/testcol:t is not built yet. In a checkout, run: make build-testcol" "$out"
lacks "...and not the wrapper's own build, which builds no base" "testcol build" "$out"
lacks_item "...and nothing runs" "run" "$out"
STUB_CALLS="$TMP/calls" docker_argv login >/dev/null
[[ "$(tr '\n' '|' < "$TMP/calls")" == "image inspect|run --rm|" ]] &&
    ok "a run that has its image asks Docker once, then runs" || bad "the docker calls of a run: $(tr '\n' '|' < "$TMP/calls")"

echo "wrapper-lib, a checkout where Docker does not answer:"
# stopped DESC WANT ENV...: a `download` of the collector `nocol` in the
# environment ENV exits 2, prints WANT and nothing else, starts nothing
# and makes no data folder.
stopped() {
    local desc="$1" want="$2" out rc
    shift 2
    : > "$TMP/calls"
    out="$(export TEST_NAME=nocol TEST_PREFIX=NOCOL STUB_CALLS="$TMP/calls" "$@"; docker_argv download 2>&1)"; rc=$?
    [[ "$rc" == 2 ]] && ok "$desc: exit 2" || bad "$desc: exit $rc"
    if grep -q -e '^run' -e '^build' "$TMP/calls"; then bad "$desc: docker ran: $(tr '\n' '|' < "$TMP/calls")"
    else ok "$desc: nothing runs"; fi
    case " $* " in
        *" STUB_NO_CLIENT=1 "*)
            [[ ! -s "$TMP/calls" ]] && ok "$desc: Docker is not asked" || bad "$desc: docker was called" ;;
    esac
    [[ "$out" == "$want" ]] && ok "$desc: the message, and nothing else" || bad "$desc: says: $out"
    [[ ! -e "$TMP/xdgdata/wealthdb/nocol" ]] && ok "$desc: no data folder is made" || bad "$desc: the data folder was made"
}
stopped "Docker down" "nocol: Docker does not answer. Start it, then run this again. With Colima: colima start
    failed to connect to the docker API at unix:///var/run/docker.sock; check if the path is correct and if the daemon is running" \
    STUB_DOWN=1
stopped "no docker program" "nocol: this collector runs in a container, and Docker is not installed." STUB_NO_CLIENT=1
stopped "no docker program, whatever a Docker would say" \
    "nocol: this collector runs in a container, and Docker is not installed." STUB_NO_CLIENT=1 STUB_DOWN=1

echo "wrapper-lib, an installed copy:"
# An installed copy has the wrappers and no Makefile.
INST="$TMP/installed"
mkdir -p "$INST/shared/wrappers"
cp "$HERE"/../*.sh "$INST/shared/wrappers/"
INSTALLED="nocol: this collector runs in a container, and an installed copy has no image for it yet. Run it from a checkout of wealthdb."
stopped "Docker down" "$INSTALLED" TEST_LIB="$INST/shared/wrappers" STUB_DOWN=1
stopped "Docker up, no image" "$INSTALLED" TEST_LIB="$INST/shared/wrappers" STUB_NO_IMAGE=1
stopped "no docker program" "$INSTALLED" TEST_LIB="$INST/shared/wrappers" STUB_NO_CLIENT=1
out="$(TEST_LIB="$INST/shared/wrappers" docker_argv login 2>&1)"; rc=$?
[[ "$rc" == 0 ]] && ok "an image that is there: the verb goes on" || bad "an installed copy with its image exits $rc"
has_item "...to the run" "run" "$out"
: > "$TMP/calls"
out="$(STUB_CALLS="$TMP/calls" TEST_LIB="$INST/shared/wrappers" docker_argv build 2>&1)"; rc=$?
[[ "$rc" == 2 && "$out" == "testcol: this collector runs in a container, and an installed copy has no image for it yet. Run it from a checkout of wealthdb." ]] &&
    ok "build: an installed copy builds none" || bad "an installed copy's build: rc=$rc $out"
[[ ! -s "$TMP/calls" ]] && ok "...and Docker is not asked" || bad "an installed copy's build called docker"

echo
summary "wrapper_main docker argv tests"
