#!/usr/bin/env bash
#
# Unit tests for the mcp lifecycle script — no Docker and no engine
# required. Run via `make test-mcp` or directly. Sources mcp/mcp (which
# only runs mcp_main when executed, not sourced) against a stubbed
# engine and a stubbed docker, and checks the container geometry, the
# token, the auth gate and the outdated-image check. Then the same
# script in a tree of the native engine: the server's argv, its
# LaunchAgent, and a start, status and stop of a stand-in server, as a
# background process and under a stubbed launchd. bash 3.2 compatible
# (macOS).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
# No ambient config reaches the script: every case runs the default
# config unless it names another.
unset WEALTHDB_CONFIG WEALTHDB_COLLECT WEALTHDB_PROGRAM

# Nothing here reads a real env file or reaches launchd. The secrets
# folder is an empty one under $tmp, the servers are plain processes,
# and `launchctl` is a stub that records its calls (as `docker` below).
mkdir -p "$tmp/secrets" "$tmp/bin"
export WEALTHDB_SECRETS_DIR="$tmp/secrets" WEALTHDB_NO_LAUNCHD=1
cat > "$tmp/bin/launchctl" <<EOF
#!/bin/sh
echo "\$*" >> "$tmp/launchctl-calls"
EOF
chmod +x "$tmp/bin/launchctl"

# Engine stub: records its argv (one arg per line) and prints the
# `config-env mcp` lines in $tmp/config-env-mcp. mcp/mcp resolves
# WEALTHDB from WEALTHDB_BIN when it is SOURCED, so the stub is exported
# before the source line.
cat > "$tmp/engine-stub" <<EOF
#!/bin/sh
printf '%s\n' "\$@" > "$tmp/engine-args"
cat "$tmp/config-env-mcp"
EOF
chmod +x "$tmp/engine-stub"
export WEALTHDB_BIN="$tmp/engine-stub"
# The script under test runs the container engine, whatever ./configure
# recorded for this checkout. The native cases name a record of their own.
export WEALTHDB_BUILD_CONFIG=/dev/null

# Docker stub: records each call's argv (one arg per line; every call as
# one line of docker-calls) and answers the queries from STUB_*
# variables. STUB_RUNNING is a container that runs, STUB_EXISTS one that
# does not. It mounts a config (STUB_CFG_MOUNT) and a token (STUB_MOUNT).
# With STUB_HANG it is a daemon that does not answer.
cat > "$tmp/bin/docker" <<EOF
#!/bin/sh
printf '%s\n' "\$@" > "$tmp/docker-args"
echo "\$*" >> "$tmp/docker-calls"
[ -z "\${STUB_HANG:-}" ] || exec sleep 30
case "\$1 \$2" in
    "ps -q")          [ -n "\${STUB_RUNNING:-}" ] && echo abc123 ;;
    "ps -aq")         [ -n "\${STUB_RUNNING:-}\${STUB_EXISTS:-}" ] && echo abc123 ;;
    "container inspect") [ -n "\${STUB_RUNNING:-}\${STUB_EXISTS:-}" ] || exit 1
                         [ "\$3" != -f ] || printf '%s\n' "\${STUB_CFG_MOUNT:-/cfg/wealthdb.cfg}" "\${STUB_MOUNT:-/elsewhere/mcp/token}" ;;
    "inspect -f")     echo "\${STUB_RUNNING_IMAGE:-sha256:one}" ;;
    "image inspect")  [ -n "\${STUB_NO_IMAGE:-}" ] && { echo "Error response from daemon: No such image: \$3" >&2; exit 1; }; echo "\${STUB_CURRENT_IMAGE:-sha256:one}" ;;
esac
exit 0
EOF
chmod +x "$tmp/bin/docker"
export PATH="$tmp/bin:$PATH"

# shellcheck source=/dev/null
source "$HERE/mcp"

fails=0
pass()  { printf '  ok   %s\n' "$1"; }
fail()  { printf '  FAIL %s\n' "$1"; fails=$((fails + 1)); }
check() { # desc, expected-substring, actual
    if grep -qF -- "$2" <<<"$3"; then pass "$1"
    else fail "$1"; printf '       wanted substring: %s\n       in: %s\n' "$2" "$3"; fi
}
check_not() { # desc, unwanted-substring, actual
    if grep -qF -- "$2" <<<"$3"; then fail "$1"; else pass "$1"; fi
}

# A match on the first line of an input longer than grep's first read:
# grep quits there, and a pipe into it would read as failed under pipefail.
check "a long input does not break check" "needle" "needle
$(seq 1 300000)"

config() { # enabled port auth insecure
    printf 'WEALTHDB_MCP_ENABLED=%s\nWEALTHDB_MCP_PORT=%s\nWEALTHDB_MCP_CONFIG_AUTH=%s\nWEALTHDB_MCP_CONFIG_INSECURE=%s\nWEALTHDB_GOLD_DB=%s\n' \
        "$1" "$2" "$3" "$4" "'$tmp/gold.db'" > "$tmp/config-env-mcp"
}

echo "== _publish_args =="
out="$(_publish_args 3300 both)"
check     "both → IPv4 flag" "127.0.0.1:3300:3300" "$out"
check     "both → IPv6 flag" "[::1]:3300:3300"     "$out"
out="$(_publish_args 3400 v4)"
check     "v4 → IPv4 flag"   "127.0.0.1:3400:3400" "$out"
check_not "v4 → no IPv6"     "[::1]"               "$out"
if _publish_args 3300 all 2>/dev/null; then fail "bad bind refused"; else pass "bad bind refused"; fi

echo "== _docker_run_args (detached, read-only, hardened, loopback) =="
HOME=/home/u XDG_DATA_HOME= XDG_CONFIG_HOME=/cfg WEALTHDB_DATA_ROOT=/data WEALTHDB_CONFIG= WEALTHDB_IMAGE_TAG=latest _resolve_knobs
PORT=3300 BIND=both AUTH=token CHARS= ALLOW_HOSTS=
_docker_run_args
joined="${DOCKER_ARGS[*]}"
check     "detached"               "run -d"                                       "$joined"
check     "never pulls"            "run -d --pull=never"                          "$joined"
check     "container name"         "--name wealthdb-mcp"                          "$joined"
check     "restarts"               "--restart unless-stopped"                     "$joined"
check     "config read-only"       "/cfg/wealthdb.cfg:/cfg/wealthdb.cfg:ro"       "$joined"
check     "data root read-only"    "/data:/data:ro"                               "$joined"
check     "read-only rootfs"       "--read-only --tmpfs /tmp"                     "$joined"
check     "no capabilities"        "--cap-drop ALL --security-opt no-new-privileges" "$joined"
check     "memory cap"             "--memory 3g"                                  "$joined"
check     "token mounted RO"       "/home/u/.local/share/wealthdb/mcp/token:/run/wealthdb-mcp/token:ro" "$joined"
check     "publish IPv4 loopback"  "127.0.0.1:3300:3300"                          "$joined"
check     "publish IPv6 loopback"  "[::1]:3300:3300"                              "$joined"
check     "serves HTTP"            "wealthdb:latest -c /cfg/wealthdb.cfg mcp-serve --http :3300 --all-interfaces --token-file /run/wealthdb-mcp/token" "$joined"
check_not "no token in env"        "WEALTHDB_MCP_TOKEN"                           "$joined"
check_not "no -t"                  " -t "                                         "$joined"

CHARS=4000 ALLOW_HOSTS="wealthdb-mcp,host.docker.internal"
_docker_run_args
joined="${DOCKER_ARGS[*]}"
check     "character budget"       "--chars 4000"                                 "$joined"
check     "allowed hosts"          "--allow-host wealthdb-mcp --allow-host host.docker.internal" "$joined"
CHARS= ALLOW_HOSTS=

AUTH=none
_docker_run_args
joined="${DOCKER_ARGS[*]}"
check     "auth none flags"        "--auth none --insecure"                       "$joined"
check_not "auth none mounts no token" "/run/wealthdb-mcp/token"                   "$joined"
AUTH=token

echo "== _stdio_args (pipe, not a terminal) =="
_stdio_args --privacy
joined="${DOCKER_ARGS[*]}"
check     "interactive, removed"   "run --rm -i"                                  "$joined"
check     "never pulls"            "run --rm -i --pull=never"                     "$joined"
check_not "no -t"                  " -t "                                         "$joined"
check_not "not detached"           " -d "                                         "$joined"
check_not "no publish"             " -p "                                         "$joined"
check_not "no token"               "token"                                        "$joined"
check     "data root read-only"    "/data:/data:ro"                               "$joined"
check     "serves stdio, privacy"  "mcp-serve --stdio --privacy"                  "$joined"

echo "== _resolve_knobs (character budget, allowed hosts) =="
HOME=/home/u XDG_DATA_HOME= XDG_CONFIG_HOME=/cfg WEALTHDB_DATA_ROOT=/data WEALTHDB_CONFIG= WEALTHDB_IMAGE_TAG=latest \
    WEALTHDB_MCP_CHARS=4000 WEALTHDB_MCP_ALLOW_HOSTS=wealthdb-mcp _resolve_knobs
_stdio_args --privacy
joined="${DOCKER_ARGS[*]}"
check     "stdio character budget" "mcp-serve --stdio --chars 4000"               "$joined"
check     "stdio allowed host"     "--allow-host wealthdb-mcp --privacy"          "$joined"
CHARS= ALLOW_HOSTS=

echo "== _check_auth =="
AUTH=token INSECURE=0; if _check_auth 2>/dev/null; then pass "token accepted"; else fail "token accepted"; fi
AUTH=none INSECURE=0
if out="$(_check_auth 2>&1)"; then fail "none alone refused"; else pass "none alone refused"; fi
check     "refusal names insecure" "\"insecure\": true"                           "$out"
AUTH=none INSECURE=true; if _check_auth 2>/dev/null; then pass "none with insecure accepted"; else fail "none with insecure accepted"; fi
AUTH=none INSECURE=YES;  if _check_auth 2>/dev/null; then pass "insecure in any case"; else fail "insecure in any case"; fi
AUTH=oauth INSECURE=1;   if _check_auth 2>/dev/null; then fail "unknown mode refused"; else pass "unknown mode refused"; fi
AUTH=token INSECURE=0

echo "== _ensure_token =="
DATA_DIR="$tmp/mcpdata" TOKEN_FILE="$tmp/mcpdata/token"
unset WEALTHDB_MCP_TOKEN
_ensure_token 2>/dev/null
tok="$(cat "$TOKEN_FILE")"
if [ "${#tok}" -eq 64 ]; then pass "generated token is 32 bytes of hex"; else fail "generated token length ${#tok}"; fi
# GNU's form first: BSD's -f means something else to GNU stat.
mode="$(stat -c '%a' "$TOKEN_FILE" 2>/dev/null || stat -f '%Lp' "$TOKEN_FILE")"
if [ "$mode" = 600 ]; then pass "token file mode 600"; else fail "token file mode is $mode, not 600"; fi
_ensure_token 2>/dev/null
if [ "$(cat "$TOKEN_FILE")" = "$tok" ]; then pass "token kept across starts"; else fail "token kept across starts"; fi
WEALTHDB_MCP_TOKEN="from-the-environment-0123456789" _ensure_token
check     "env token wins"         "from-the-environment-0123456789"              "$(cat "$TOKEN_FILE")"
if WEALTHDB_MCP_TOKEN=short _ensure_token 2>/dev/null; then fail "short env token refused"; else pass "short env token refused"; fi

echo "== _load_config (env forms win) =="
config 1 3300 token 0
unset WEALTHDB_MCP_AUTH WEALTHDB_MCP_INSECURE
WEALTHDB_CONFIG= _load_config
if [ "$(cat "$tmp/engine-args")" = "$(printf 'config-env\nmcp')" ]; then
    pass "asks the engine for config-env mcp"
else fail "asks the engine for config-env mcp"; cat "$tmp/engine-args"; fi
WEALTHDB_CONFIG=/cfg/wealthdb.cfg _load_config
if [ "$(cat "$tmp/engine-args")" = "$(printf -- '-c\n/cfg/wealthdb.cfg\nconfig-env\nmcp')" ]; then
    pass "...with -c WEALTHDB_CONFIG before it"
else fail "...with -c WEALTHDB_CONFIG before it"; cat "$tmp/engine-args"; fi
check     "port read back"         "3300"                                         "$PORT"
check     "auth read back"         "token"                                        "$AUTH"
WEALTHDB_MCP_AUTH=none WEALTHDB_MCP_INSECURE=1 _load_config
check     "env auth wins"          "none"                                         "$AUTH"
check     "env insecure wins"      "1"                                            "$INSECURE"

echo "== mcp_start refusals =="
config 0 3300 token 0
if out="$(mcp_start 2>&1)"; then fail "refuses when not enabled"; else pass "refuses when not enabled"; fi
check     "says how to turn it on" 'Run: wealthdb config mcp-server'               "$out"
config 1 3300 none 0
if out="$(mcp_start 2>&1)"; then fail "refuses auth none alone"; else pass "refuses auth none alone"; fi
check     "says what is missing"   "insecure"                                     "$out"
config 1 3300 token 0
if out="$(STUB_NO_IMAGE=1 mcp_start 2>&1)"; then fail "refuses without the image"; else pass "refuses without the image"; fi
check     "says to build"          "mcp: the image wealthdb:latest is not built yet. Build it with: wealthdb build" "$out"
if out="$(STUB_NO_IMAGE=1 mcp_stdio 2>&1)"; then fail "stdio refuses without the image"; else pass "stdio refuses without the image"; fi
check     "...and says to build"   "is not built yet. Build it with: wealthdb build" "$out"

echo "== mcp_help =="
out="$(mcp_help)"
check     "names the loopback addresses" "It listens on loopback only: 127.0.0.1, and as a container [::1] too." "$out"
check     "...and the native server's IPv6 switch" "WEALTHDB_MCP_BIND=v6 moves a native server to [::1]." "$out"

echo "== mcp_start (insecure, warned) =="
config 1 3300 none 1
out="$(mcp_start 2>&1)"
check     "started"                "is up"                                        "$out"
check_not "banner ends where it should" "mcp_help"                              "$out"
check     "insecure warning"       "INSECURE"                                     "$out"
check     "ran mcp-serve"          "--insecure"                                   "$(cat "$tmp/docker-args")"

echo "== mcp_status =="
config 1 3300 none 1
out="$(STUB_RUNNING=1 STUB_RUNNING_IMAGE=sha256:old STUB_CURRENT_IMAGE=sha256:new mcp_status)"
check     "running"                "mcp: running"                                 "$out"
check     "outdated image"         "image: outdated"                              "$out"
check     "insecure warned"        "INSECURE"                                     "$out"
config 1 3300 token 0
out="$(STUB_RUNNING=1 STUB_RUNNING_IMAGE=sha256:same STUB_CURRENT_IMAGE=sha256:same mcp_status)"
check     "current image"          "image: current"                               "$out"
check_not "no warning with a token" "INSECURE"                                    "$out"

echo "== mcp_stop (the container, and a server the native engine left) =="
: > "$tmp/docker-calls"
out="$(STUB_RUNNING=1 mcp_stop 2>&1)"
check     "stops the container"    "mcp: stopped wealthdb-mcp (the token is kept in $TOKEN_FILE)" "$out"
calls="$(tr '\n' '|' < "$tmp/docker-calls")"
check     "with docker stop, then rm" "|stop wealthdb-mcp|rm wealthdb-mcp|"       "$calls"
out="$(mcp_stop 2>&1)"
check     "no container: says so"  "mcp: no container 'wealthdb-mcp' to stop"     "$out"
# A stand-in for a native server that ran when ./configure chose the
# container engine: a process the pid file names.
standin="$(sleep 30 >/dev/null 2>&1 & echo $!)"
# A process just started still has its parent's command line for a
# moment, and a stop ends only the program its pid file should name.
for _ in $(seq 1 50); do
    case "$(_pid_command "$standin")" in sleep*) break ;; esac
    sleep 0.1
done
echo "$standin" > "$DATA_DIR/mcp.pid"
out="$(SERVICE_PROC=sleep mcp_stop 2>&1)"
check     "stop ends a native server too" "mcp: stopped the native engine's server" "$out"
check_not "and has no line on a container that is not there" "no container"      "$out"
if _pid_runs "$standin"; then fail "the native server still runs"; kill "$standin"; else pass "the native server is gone"; fi
if [ -e "$DATA_DIR/mcp.pid" ]; then fail "its pid file is left"; else pass "and so is its pid file"; fi

echo "== mcp_url =="
config 1 3300 token 0
printf 'tok-0123456789abcdef\n' > "$TOKEN_FILE"
out="$(mcp_url)"
check     "URL"                    "http://127.0.0.1:3300/mcp"                    "$out"
check     "privacy URL"            "http://127.0.0.1:3300/mcp/privacy"            "$out"
check     "header"                 "Authorization: Bearer tok-0123456789abcdef"   "$out"
out="$(BIND=v6 mcp_url)"
check     "IPv6-only URL"          "http://[::1]:3300/mcp"                        "$out"
check_not "no IPv4 URL when v6"    "127.0.0.1"                                    "$out"

echo "== an env file that does not parse stops mcp, and its line is not shown =="
# bash -n quotes the offending line, and in the env file that line may
# hold the token.
printf 'WEALTHDB_MCP_TOKEN=synthetic-marker-7)\n' > "$tmp/bad-mcp.env"
out="$( (WEALTHDB_MCP_ENV_FILE="$tmp/bad-mcp.env" mcp_main status) 2>&1)"; rc=$?
check     "a bad env file exits 1"           "1"                          "$rc"
check     "the message names the file"       "$tmp/bad-mcp.env has a bash syntax error" "$out"
check_not "the message hides the bad line"   "synthetic-marker-7"         "$out"
# With no WEALTHDB_MCP_ENV_FILE, the file is wealthdb-mcp.env in WEALTHDB_SECRETS_DIR.
mkdir -p "$tmp/secrets-alt"
printf 'if then\n' > "$tmp/secrets-alt/wealthdb-mcp.env"
out="$( (unset WEALTHDB_MCP_ENV_FILE; WEALTHDB_SECRETS_DIR="$tmp/secrets-alt" mcp_main status) 2>&1)"
case "$out" in
    *"$tmp/secrets-alt/wealthdb-mcp.env has a bash syntax error"*) pass "the env file follows WEALTHDB_SECRETS_DIR" ;;
    *) fail "the env file does not follow WEALTHDB_SECRETS_DIR: $out" ;;
esac

echo "== _resolve_knobs (image tag) =="
WEALTHDB_IMAGE_TAG=feature _resolve_knobs
check     "image follows the tag"  "wealthdb:feature"                             "$IMAGE"

# ---- a setup of its own -------------------------------------------------
# A config that is not the default one keeps its server apart: the
# container's name, the data folder, the env file and the data root
# follow the config's folder, and each override still wins.

# knobs NAME=VALUE...: what _resolve_knobs gives in an environment that
# holds HOME, the config dir /cfg and the pairs given.
knobs() {
    (
        unset WEALTHDB_MCP_DATA_DIR WEALTHDB_MCP_CONTAINER WEALTHDB_MCP_ENV_FILE \
            WEALTHDB_CONFIG WEALTHDB_DATA_ROOT XDG_DATA_HOME
        export HOME=/home/u XDG_CONFIG_HOME=/cfg WEALTHDB_IMAGE_TAG=latest
        for kv in "$@"; do export "${kv?}"; done
        _resolve_knobs
        printf '%s\n' "container=$CONTAINER" "data=$DATA_DIR" "token=$TOKEN_FILE" "root=$DATA_ROOT" \
            "cfg=$CFG" "program=$PROGRAM"
        if [ -n "${SHOW_ARGS:-}" ]; then
            PORT=3300 AUTH=token
            _docker_run_args
            echo "args=${DOCKER_ARGS[*]}"
        fi
    )
}
echo "== _resolve_knobs: the default config =="
out="$(knobs)"
if [ "$out" = "container=wealthdb-mcp
data=/home/u/.local/share/wealthdb/mcp
token=/home/u/.local/share/wealthdb/mcp/token
root=/home/u/.local/share/wealthdb
cfg=/cfg/wealthdb.cfg
program=wealthdb" ]; then pass "wealthdb-mcp, the XDG data folder and plain commands"
else fail "the default config resolves another way: $out"; fi
out="$(knobs WEALTHDB_DATA_ROOT=/data XDG_DATA_HOME=/xdg)"
check     "WEALTHDB_DATA_ROOT names the data root" "root=/data" "$out"
check     "XDG_DATA_HOME moves the data folder" "data=/xdg/wealthdb/mcp" "$out"
out="$(knobs WEALTHDB_CONFIG=/cfg//wealthdb.cfg WEALTHDB_DATA_ROOT=/data)"
check     "WEALTHDB_CONFIG on the default config: the same container" "container=wealthdb-mcp" "$out"
check     "...and the data root of the environment" "root=/data" "$out"
out="$(knobs WEALTHDB_MCP_CONTAINER=mine WEALTHDB_MCP_DATA_DIR=/d)"
check     "WEALTHDB_MCP_CONTAINER names the container" "container=mine" "$out"
check     "WEALTHDB_MCP_DATA_DIR names the data folder" "token=/d/token" "$out"
# A demo's call: the config dir, the config and the data root are one
# folder, and the names are given.
out="$(knobs XDG_CONFIG_HOME=/srv/demo WEALTHDB_CONFIG=/srv/demo/wealthdb.cfg WEALTHDB_DATA_ROOT=/srv/demo \
    WEALTHDB_MCP_CONTAINER=wealthdb-mcp-demo WEALTHDB_MCP_DATA_DIR=/srv/demo/mcp)"
if [ "$out" = "container=wealthdb-mcp-demo
data=/srv/demo/mcp
token=/srv/demo/mcp/token
root=/srv/demo
cfg=/srv/demo/wealthdb.cfg
program=wealthdb" ]; then pass "a demo's call: the names and the folders it gives"
else fail "a demo's call resolves another way: $out"; fi
out="$(knobs XDG_CONFIG_HOME=/srv/demo WEALTHDB_CONFIG=/srv/demo/wealthdb.cfg WEALTHDB_DATA_ROOT=/srv/demo)"
check     "...and with no names given it is the default config's server" "container=wealthdb-mcp" "$out"

echo "== _resolve_knobs: a setup of its own =="
out="$(SHOW_ARGS=1 knobs "WEALTHDB_CONFIG=/home/u/My Try/wealthdb.cfg" WEALTHDB_DATA_ROOT=/data XDG_DATA_HOME=/xdg)"
check     "the container is named after the folder" "container=wealthdb-mcp-my-try" "$out"
check     "the data folder is in it" "data=/home/u/My Try/mcp" "$out"
check     "the data root is the folder" "root=/home/u/My Try" "$out"
check_not "WEALTHDB_DATA_ROOT is not read" "/data" "$out"
check_not "nor the XDG data dir" "/xdg" "$out"
check     "commands name the config, quoted" "program=wealthdb -c '/home/u/My Try/wealthdb.cfg'" "$out"
check     "the container gets that name" "--name wealthdb-mcp-my-try" "$out"
check     "...mounts the folder as the data root" "-v /home/u/My Try:/home/u/My Try:ro" "$out"
check     "...is told so" "-e WEALTHDB_DATA_ROOT=/home/u/My Try" "$out"
check     "...mounts the config" "-v /home/u/My Try/wealthdb.cfg:/home/u/My Try/wealthdb.cfg:ro" "$out"
check     "...and the setup's own token" "-v /home/u/My Try/mcp/token:/run/wealthdb-mcp/token:ro" "$out"
out="$(knobs WEALTHDB_CONFIG=/home/u/TRY/wealthdb.cfg)"
check     "an upper-case folder gives a lower-case name" "container=wealthdb-mcp-try" "$out"
out="$(knobs WEALTHDB_CONFIG=/wealthdb.cfg)"
check     "a config in the root folder: the name is setup" "container=wealthdb-mcp-setup" "$out"
check     "...and the data folder /mcp" "data=/mcp" "$out"
out="$(knobs WEALTHDB_CONFIG=/home/u/try/wealthdb.cfg WEALTHDB_MCP_CONTAINER=mine)"
check     "WEALTHDB_MCP_CONTAINER still names the container" "container=mine" "$out"
check     "...and the data folder stays the setup's" "data=/home/u/try/mcp" "$out"
out="$(knobs WEALTHDB_CONFIG=/home/u/try/wealthdb.cfg WEALTHDB_MCP_DATA_DIR=/d)"
check     "WEALTHDB_MCP_DATA_DIR still names the data folder" "data=/d" "$out"
check     "...and the container stays the setup's" "container=wealthdb-mcp-try" "$out"

echo "== the env file: in the secrets folder, named after a setup of its own =="
# env_marker NAME=VALUE...: the marker the env file that was read sets.
mkdir -p "$tmp/Own Setup" "$tmp/envs"
printf 'ENV_MARKER=the-default-file\n' > "$tmp/secrets/wealthdb-mcp.env"
printf 'ENV_MARKER=the-file-of-the-setup\n' > "$tmp/secrets/wealthdb-mcp-own-setup.env"
printf 'ENV_MARKER=a-file-in-the-folder\n' > "$tmp/Own Setup/wealthdb-mcp.env"
printf 'ENV_MARKER=the-named-file\n' > "$tmp/envs/named.env"
env_marker() {
    (
        unset WEALTHDB_CONFIG WEALTHDB_MCP_ENV_FILE ENV_MARKER
        export XDG_CONFIG_HOME="$tmp/cfg"
        for kv in "$@"; do export "${kv?}"; done
        _source_env_file && echo "${ENV_MARKER:-none}"
    )
}
check     "the default config reads wealthdb-mcp.env in the secrets folder" "the-default-file" "$(env_marker)"
check     "a setup of its own reads the file named after its folder, there too" "the-file-of-the-setup" \
    "$(env_marker "WEALTHDB_CONFIG=$tmp/Own Setup/wealthdb.cfg")"
check     "...and neither the default file nor one in its own folder" "none" \
    "$(rm -f "$tmp/secrets/wealthdb-mcp-own-setup.env"; env_marker "WEALTHDB_CONFIG=$tmp/Own Setup/wealthdb.cfg")"
printf 'ENV_MARKER=the-file-of-the-setup\n' > "$tmp/secrets/wealthdb-mcp-own-setup.env"
mkdir -p "$tmp/secrets-alt"
printf 'ENV_MARKER=the-file-in-the-other-secrets-folder\n' > "$tmp/secrets-alt/wealthdb-mcp-own-setup.env"
check     "WEALTHDB_SECRETS_DIR moves it, as it moves the default file" "the-file-in-the-other-secrets-folder" \
    "$(env_marker "WEALTHDB_CONFIG=$tmp/Own Setup/wealthdb.cfg" "WEALTHDB_SECRETS_DIR=$tmp/secrets-alt")"
check     "WEALTHDB_MCP_ENV_FILE wins for a setup of its own" "the-named-file" \
    "$(env_marker "WEALTHDB_CONFIG=$tmp/Own Setup/wealthdb.cfg" "WEALTHDB_MCP_ENV_FILE=$tmp/envs/named.env")"
check     "...and for the default config" "the-named-file" "$(env_marker "WEALTHDB_MCP_ENV_FILE=$tmp/envs/named.env")"
rm -f "$tmp/secrets/wealthdb-mcp.env" "$tmp/secrets/wealthdb-mcp-own-setup.env" "$tmp/secrets-alt/wealthdb-mcp-own-setup.env"
mkdir -p "$tmp/own setup"

echo "== a start never takes over another setup's server =="
# The knobs of the cases above: the config /cfg/wealthdb.cfg, and the
# data folder that holds the token.
HOME=/home/u XDG_DATA_HOME= XDG_CONFIG_HOME=/cfg WEALTHDB_DATA_ROOT=/data WEALTHDB_CONFIG= WEALTHDB_IMAGE_TAG=latest \
    WEALTHDB_MCP_DATA_DIR="$tmp/mcpdata" _resolve_knobs
config 1 3300 token 0
# changed: the calls that stop, remove or start a container.
changed() { grep -E '^(stop|rm|run|kill) ' "$tmp/docker-calls" | cut -d' ' -f1 | tr '\n' ' ' | sed 's/ $//'; }
REFUSAL="mcp: wealthdb-mcp is the server of another setup (its files are not in $tmp/mcpdata). Stop that one first, or name this one with WEALTHDB_MCP_CONTAINER."
: > "$tmp/docker-calls"
out="$(STUB_EXISTS=1 STUB_CFG_MOUNT=/other/wealthdb.cfg mcp_start 2>&1)"; rc=$?
check     "a container of another config and another data folder stops the start" "1 $REFUSAL" "$rc $out"
check     "nothing is stopped, removed or started" "[]" "[$(changed)]"
: > "$tmp/docker-calls"
out="$(STUB_RUNNING=1 STUB_CFG_MOUNT=/other/wealthdb.cfg mcp_start 2>&1)"; rc=$?
check     "one that runs stops it too, with the same message" "1 $REFUSAL" "$rc $out"
check_not "...and not with the advice to restart" "already running" "$out"
check     "...and nothing is changed" "[]" "[$(changed)]"
# restart is a command of mcp_main, which resolves the knobs anew.
restart() {
    (
        export HOME="$tmp/home" XDG_DATA_HOME= XDG_CONFIG_HOME=/cfg WEALTHDB_DATA_ROOT=/data WEALTHDB_CONFIG= \
            WEALTHDB_MCP_DATA_DIR="$tmp/mcpdata"
        mcp_main restart
    ) 2>&1
}
: > "$tmp/docker-calls"
out="$(STUB_RUNNING=1 STUB_CFG_MOUNT=/other/wealthdb.cfg restart)"; rc=$?
check     "a restart is refused before it stops anything" "1 $REFUSAL" "$rc $out"
check     "...and nothing is changed" "[]" "[$(changed)]"
: > "$tmp/docker-calls"
out="$(STUB_EXISTS=1 STUB_CFG_MOUNT=/other/wealthdb.cfg STUB_MOUNT="$TOKEN_FILE" mcp_start 2>&1)"; rc=$?
check     "a container that mounts this setup's token is its own, and is replaced" "0" "$rc"
check     "...removed, then started" "[rm run]" "[$(changed)]"
# A server with no token mounts nothing of its data folder: the config
# it mounts tells whose it is.
config 1 3300 none 1
: > "$tmp/docker-calls"
out="$(STUB_EXISTS=1 mcp_start 2>&1)"; rc=$?
check     "a container that mounts this setup's config is its own too" "0" "$rc"
check     "...removed, then started" "[rm run]" "[$(changed)]"
: > "$tmp/docker-calls"
out="$(STUB_EXISTS=1 STUB_CFG_MOUNT=/cfg/wealthdb.cfg.bak mcp_start 2>&1)"; rc=$?
check     "a config whose name only starts the same is another config" "1 $REFUSAL" "$rc $out"
out="$(STUB_RUNNING=1 mcp_start 2>&1)"; rc=$?
check     "this setup's own server that runs: use restart" "mcp: already running (container wealthdb-mcp). Use 'wealthdb mcp restart'." "$out"
config 1 3300 token 0

echo "== the commands a setup of its own is told to run =="
OWNCFG="$tmp/own setup/wealthdb.cfg"
OWNCMD="wealthdb -c '$OWNCFG'"
# own_run CMD...: CMD with that config named, as `wealthdb -c` names it.
own_run() {
    (
        export WEALTHDB_CONFIG="$OWNCFG" XDG_CONFIG_HOME=/cfg
        unset WEALTHDB_MCP_DATA_DIR WEALTHDB_DATA_ROOT
        _resolve_knobs
        "$@" 2>&1
    )
}
out="$(own_run eval 'ENABLED=0; _require_enabled')"
check     "turning the server on" "mcp: the MCP server is off. Run: $OWNCMD config mcp-server" "$out"
out="$(own_run eval 'PORT=3300 AUTH=token GOLD_DB="$tmp/none.db"; _start_banner')"
check     "the banner's url" "Run '$OWNCMD mcp url' for the URL" "$out"
check     "the banner's stdio" "'$OWNCMD mcp stdio' as the command instead." "$out"
check     "the banner's restart" "After rebuilding the image, run '$OWNCMD mcp restart'." "$out"
check     "the banner's first load" "until '$OWNCMD init' and '$OWNCMD load -a' have run." "$out"
check     "the banner names the setup's container" "mcp: wealthdb-mcp-own-setup is up" "$out"
out="$(STUB_RUNNING=1 STUB_CFG_MOUNT="$OWNCFG" own_run mcp_start)"
check     "a server that runs" "mcp: already running (container wealthdb-mcp-own-setup). Use '$OWNCMD mcp restart'." "$out"
out="$(STUB_RUNNING=1 STUB_RUNNING_IMAGE=sha256:old STUB_CURRENT_IMAGE=sha256:new own_run mcp_status)"
check     "status, with an image that was rebuilt" "run '$OWNCMD mcp restart'" "$out"
check     "status names the token of the setup" "auth: bearer token in $tmp/own setup/mcp/token ('$OWNCMD mcp url' prints it)" "$out"
out="$(own_run mcp_url)"
check     "url, with no token yet" "mcp: no token yet; '$OWNCMD mcp start' generates one" "$out"
out="$(STUB_NO_IMAGE=1 own_run mcp_start)"
check     "a missing image names the build with the config" "Build it with: $OWNCMD build" "$out"
out="$(STUB_EXISTS=1 own_run mcp_start)"
check     "the guard names the setup's own container and folder" \
    "mcp: wealthdb-mcp-own-setup is the server of another setup (its files are not in $tmp/own setup/mcp)." "$out"
[ ! -e "$tmp/own setup/mcp" ] && pass "a refused start makes no folder" || fail "a refused start made the data folder"
# Back to the default config's knobs.
HOME=/home/u XDG_DATA_HOME= XDG_CONFIG_HOME=/cfg WEALTHDB_DATA_ROOT=/data WEALTHDB_CONFIG= WEALTHDB_IMAGE_TAG=latest \
    WEALTHDB_MCP_DATA_DIR="$tmp/mcpdata" _resolve_knobs

# ---- the native server -------------------------------------------------
# The same script in a tree of the native engine: mcp is sourced again
# with a record that says so, in a subshell, and with launchd out of
# reach so that nothing is started on this computer.
native_cases() {
    printf 'ENGINE=native\n' > "$tmp/native.mk"
    export WEALTHDB_BUILD_CONFIG="$tmp/native.mk" WEALTHDB_NO_LAUNCHD=1
    # A home of its own: an agent file is written under it.
    export HOME="$tmp/home"
    # shellcheck source=/dev/null
    source "$HERE/mcp"
    XDG_DATA_HOME= XDG_CONFIG_HOME=/cfg WEALTHDB_DATA_ROOT=/data WEALTHDB_CONFIG= \
        WEALTHDB_MCP_DATA_DIR="$tmp/native-data" _resolve_knobs
    ENGINE_BIN="$tmp/native-engine"

    echo "== native: the server's argv =="
    PORT=3300 BIND=both AUTH=token CHARS=4000 ALLOW_HOSTS=box
    _native_serve_args
    joined="${SERVE_ARGS[*]}"
    check     "serves HTTP on IPv4 loopback, with the token file" \
        "-c /cfg/wealthdb.cfg mcp-serve --http 127.0.0.1:3300 --token-file $tmp/native-data/token" "$joined"
    check_not "never beyond loopback"   "--all-interfaces"     "$joined"
    check     "character budget"        "--chars 4000"         "$joined"
    check     "allowed hosts"           "--allow-host box"     "$joined"
    BIND=v6 AUTH=none CHARS= ALLOW_HOSTS=
    _native_serve_args
    joined="${SERVE_ARGS[*]}"
    check     "v6 → IPv6 loopback"      "--http [::1]:3300"    "$joined"
    check     "auth none flags"         "--auth none --insecure" "$joined"
    check_not "auth none: no token file" "--token-file"        "$joined"
    check     "both and v4 → the IPv4 loopback address" "127.0.0.1 127.0.0.1" "$(_loopback_host both) $(_loopback_host v4)"
    check     "v6 → the IPv6 one"       "::1"                  "$(_loopback_host v6)"
    BIND=ipv6
    if out="$(_native_serve_args 2>&1)"; then fail "a BIND that is no address family is refused"
    else pass "a BIND that is no address family is refused"; fi
    check     "and the message names it" "invalid WEALTHDB_MCP_BIND 'ipv6' (want both|v4|v6)" "$out"
    BIND=both

    echo "== native: names, and the LaunchAgent =="
    CONTAINER=wealthdb-mcp
    check     "the label"               "com.smartpointer.wealthdb.mcp" "$(_service_label)"
    CONTAINER=wealthdb-mcp-demo
    check     "a second instance's label" "com.smartpointer.wealthdb.mcp-demo" "$(_service_label)"
    check     "its agent file"          "$tmp/home/Library/LaunchAgents/com.smartpointer.wealthdb.mcp-demo.plist" "$(_service_plist)"
    check     "the pid file"            "$tmp/native-data/mcp.pid" "$(_service_pidfile)"
    mkdir -p "$tmp/home"
    out="$(cd "$tmp/home" && WEALTHDB_ENTRY='/opt/x & y/bin/wealthdb' WEALTHDB_MCP_CHARS=4000 WEALTHDB_MCP_BIND=v4 \
        WEALTHDB_MCP_TOKEN=synthetic-token-marker WEALTHDB_CONFIG='/cfg/a<b>.cfg' WEALTHDB_DATA_ROOT=rel/data \
        _write_plist && cat "$(_service_plist)")"
    check     "the agent runs the entry command, escaped" "<string>/opt/x &amp; y/bin/wealthdb</string>" "$out"
    check     "with mcp serve"          "<string>mcp</string>" "$out"
    check     "(serve)"                 "<string>serve</string>" "$out"
    check     "it starts at login"      "<key>RunAtLoad</key>" "$out"
    check     "and again when it exits" "<key>KeepAlive</key>" "$out"
    check     "a relative path is handed on absolute" "<string>$tmp/home/rel/data</string>" "$out"
    check_not "and the agent depends on no working directory" "WorkingDirectory" "$out"
    check_not "as a job of the standard kind" "ProcessType"    "$out"
    check     "it hands on the config path, escaped" "<string>/cfg/a&lt;b&gt;.cfg</string>" "$out"
    check     "the knobs both servers have" "<key>WEALTHDB_MCP_BIND</key>" "$out"
    check     "always the data folder, which tells whose server it is" \
        "<key>WEALTHDB_MCP_DATA_DIR</key>|        <string>$tmp/native-data</string>" "$(printf '%s\n' "$out" | grep -A1 WEALTHDB_MCP_DATA_DIR | tr '\n' '|')"
    check     "and the MCP server's own" "<key>WEALTHDB_MCP_CHARS</key>" "$out"
    check_not "and never the token"     "synthetic-token-marker" "$out"
    check_not "nor its variable"        "WEALTHDB_MCP_TOKEN"   "$out"
    if command -v plutil >/dev/null 2>&1; then
        if plutil -lint "$(_service_plist)" >/dev/null 2>&1; then pass "the agent file is a valid property list"
        else fail "the agent file is not a valid property list"; fi
    fi

    echo "== native: lifecycle without a server =="
    CONTAINER=wealthdb-mcp
    rm -f "$(_service_plist)"
    config 1 3300 token false
    out="$(mcp_status 2>&1)"
    check     "status: not running"     "mcp: not running"     "$out"
    check_not "status names no container" "container"          "$out"
    out="$(mcp_stop 2>&1)"
    check     "stop: nothing to stop"   "mcp: no server to stop" "$out"

    echo "== native: stop ends a container the other engine left =="
    : > "$tmp/docker-calls"
    out="$(STUB_RUNNING=1 STUB_MOUNT="$TOKEN_FILE" mcp_stop 2>&1)"
    check     "stop says which container it stopped" "mcp: stopped container wealthdb-mcp, the container engine's server" "$out"
    check_not "and has no line on a server that is not there" "no server to stop" "$out"
    calls="$(tr '\n' '|' < "$tmp/docker-calls")"
    check     "docker is asked, then stops and removes it" "|stop wealthdb-mcp|rm wealthdb-mcp|" "$calls"
    : > "$tmp/docker-calls"
    out="$(STUB_RUNNING=1 mcp_stop 2>&1)"
    check     "a container of that name that serves another data folder is left alone" "mcp: no server to stop" "$out"
    check_not "docker stops nothing of it" "stop wealthdb-mcp" "$(cat "$tmp/docker-calls")"
    : > "$tmp/docker-calls"
    out="$(mcp_stop 2>&1)"
    check_not "with no container, docker stops nothing" "stop wealthdb-mcp" "$(cat "$tmp/docker-calls")"
    t0=$SECONDS
    out="$(STUB_HANG=1 SERVICE_DOCKER_WAIT=1 mcp_stop 2>&1)"; rc=$?
    check     "a docker that does not answer fails no stop" "0" "$rc"
    check     "which says what it found" "mcp: no server to stop" "$out"
    if [ $((SECONDS - t0)) -lt 10 ]; then pass "and waits for it a moment only"; else fail "stop waited $((SECONDS - t0))s for docker"; fi

    echo "== native: with no engine built =="
    out="$(mcp_start 2>&1)"; rc=$?
    check     "start with no engine built fails" "1"           "$rc"
    check     "and says how to build it" "Run: make all"       "$out"
    out="$(mcp_serve 2>&1)"
    check     "serve with no engine built fails too" "Run: make all" "$out"

    echo "== native: serve becomes the engine =="
    # An engine that records how it was run.
    cat > "$tmp/native-engine" <<EOS
#!/bin/sh
case "\$*" in
    *config-env*) cat "$tmp/config-env-mcp" ;;
    *) { printf '%s\n' "\$@"; echo "kind=\$WEALTHDB_ENGINE root=\$WEALTHDB_DATA_ROOT pid=\$\$"; } > "$tmp/native-served" ;;
esac
EOS
    chmod +x "$tmp/native-engine"
    # serve is a command of its own, whose process the engine becomes:
    # it runs here from a tree of the native engine, with the stub as
    # that tree's engine.
    tree="$tmp/tree"
    mkdir -p "$tree/mcp" "$tree/shared/wrappers" "$tree/wealthdb"
    cp "$HERE/mcp" "$tree/mcp/"
    cp "$HERE"/../shared/wrappers/*.sh "$tree/shared/wrappers/"
    cp "$tmp/native-engine" "$tree/wealthdb/wealthdb-engine"
    printf 'ENGINE=native\n' > "$tree/config.mk"
    ( unset WEALTHDB_BUILD_CONFIG
      XDG_DATA_HOME='' XDG_CONFIG_HOME=/cfg WEALTHDB_DATA_ROOT=/data WEALTHDB_CONFIG='' \
          WEALTHDB_MCP_DATA_DIR="$tmp/native-data" WEALTHDB_BIN="$tmp/native-engine" \
          "$tree/mcp/mcp" serve ) > "$tmp/serve.out" 2>&1; rc=$?
    [ "$rc" = 0 ] || sed "s/^/       serve: /" "$tmp/serve.out"
    check     "serve runs the engine"   "0"                    "$rc"
    out="$(cat "$tmp/native-served" 2>/dev/null)"
    check     "as the HTTP server"      "mcp-serve"            "$out"
    check     "on loopback"             "127.0.0.1:3300"       "$out"
    check     "told its kind and the data root" "kind=native root=/data" "$out"
    check     "under the pid the pid file names" "pid=$(cat "$(_service_pidfile)" 2>/dev/null)" "$out"
    [ -s "$TOKEN_FILE" ] && pass "serve made the token" || fail "serve made no token"
    check     "serve recorded the engine it runs" "$(_file_id "$tree/wealthdb/wealthdb-engine")" "$(cat "$SERVING_FILE" 2>/dev/null)"
    rm -f "$(_service_pidfile)"
    _file_id "$ENGINE_BIN" > "$SERVING_FILE"

    echo "== native: start, status and stop of a stand-in server =="
    # start runs `<entry> mcp serve` in the background. A stand-in for
    # that command records its pid, as serve does, and sleeps.
    printf '#!/bin/sh\necho $$ > "%s"\nexec sleep 30\n' "$(_service_pidfile)" > "$tmp/entry"
    chmod +x "$tmp/entry"
    export WEALTHDB_ENTRY="$tmp/entry"
    out="$(SERVICE_PROC=sleep mcp_start 2>&1)"; rc=$?
    check     "start succeeds"          "0"                    "$rc"
    check     "start says the server is up" "mcp: the server is up" "$out"
    check     "and what follows an upgrade" "After an upgrade, run 'wealthdb mcp restart'" "$out"
    check_not "a plain process promises no start at login" "at login" "$out"
    SERVICE_PROC=sleep
    if _is_running; then pass "the server runs"; else fail "the server does not run: $(cat "$(_service_logfile)" 2>/dev/null)"; fi
    # serve while a server runs: refused, and the pid file stays the
    # running server's.
    pid="$(_native_pid)"
    out="$( (mcp_serve) 2>&1)"; rc=$?
    check     "serve is refused while a server runs" "1"         "$rc"
    check     "and says that one runs"  "mcp: already running" "$out"
    out="$( (_serve_here) 2>&1)"; rc=$?
    check     "the pid file is not taken over" "1 mcp: already running (pid $pid)" "$rc $out"
    check     "it still names the running server" "$pid"       "$(cat "$(_service_pidfile)")"
    out="$(mcp_status 2>&1)"
    check     "status: running, with its pid" "mcp: running   pid=$(_native_pid)" "$out"
    check     "status: the engine is current" "engine: current"  "$out"
    check     "status names the log"    "log: $tmp/native-data/mcp.log" "$out"
    echo changed >> "$tmp/native-engine"
    out="$(mcp_status 2>&1)"
    check     "a rebuilt engine reads as outdated" "engine: outdated" "$out"
    out="$(mcp_start 2>&1)"; rc=$?
    check     "a second start is refused" "already running (the server)" "$out"
    pid="$(_native_pid)"
    out="$(mcp_stop 2>&1)"
    check     "stop says so"            "mcp: stopped the server" "$out"
    if _pid_runs "$pid"; then fail "the server still runs after stop"; else pass "the server is gone after stop"; fi
    [ ! -e "$(_service_pidfile)" ] && pass "stop removed the pid file" || fail "the pid file is left"
    [ -s "$TOKEN_FILE" ] && pass "stop kept the token" || fail "stop removed the token"
    BIND=ipv6
    out="$(mcp_start 2>&1)"; rc=$?
    check     "a start with a BIND that is no address family fails" "1 mcp: invalid WEALTHDB_MCP_BIND" "$rc $out"
    BIND=both

    # A pid file that names another program, or no process at all, names
    # no server: nothing runs, and a stop ends no one.
    other="$(sleep 30 >/dev/null 2>&1 & echo $!)"
    echo "$other" > "$(_service_pidfile)"
    SERVICE_PROC=no-such-marker
    if _is_running; then fail "another program's pid reads as the server"; else pass "another program's pid is not the server"; fi
    out="$(mcp_stop 2>&1)"
    if kill -0 "$other" 2>/dev/null; then pass "and stop leaves that program alone"; else fail "stop ended another program"; fi
    [ ! -e "$(_service_pidfile)" ] && pass "stop removes the pid file all the same" || fail "the stale pid file is left"
    kill "$other" 2>/dev/null
    echo not-a-number > "$(_service_pidfile)"
    if _is_running; then fail "a pid file with no number reads as the server"; else pass "a pid file with no number names no server"; fi
    rm -f "$(_service_pidfile)"
    # A process that has ended, whose parent never reads its exit
    # status: it ends a second after its parent has turned into a sleep,
    # which reads none. A signal still reaches it, and it is no server
    # all the same.
    sh -c 'sleep 1 & echo $! > "$1"; exec sleep 30' sh "$tmp/ended-pid" >/dev/null 2>&1 &
    keeper=$!
    ended=
    for _ in $(seq 1 50); do
        ended="$(cat "$tmp/ended-pid" 2>/dev/null)"
        case "$(ps -p "${ended:-0}" -o stat= 2>/dev/null)" in *Z*) break ;; esac
        sleep 0.1
    done
    if kill -0 "${ended:-0}" 2>/dev/null; then
        if _pid_runs "$ended"; then fail "a process that has ended reads as one that runs"; else pass "a process that has ended does not run"; fi
        echo "$ended" > "$(_service_pidfile)"
        SERVICE_PROC=sleep
        if _is_running; then fail "a pid file that names it reads as the server"; else pass "a pid file that names it names no server"; fi
        rm -f "$(_service_pidfile)"
        t0=$SECONDS
        _end_process "$ended"
        if [ $((SECONDS - t0)) -lt 5 ]; then pass "and ending it does not wait for it to go"
        else fail "ending it waited $((SECONDS - t0))s"; fi
    else
        fail "the stand-in for an ended process was not made"
    fi
    kill "$keeper" 2>/dev/null
    wait "$keeper" 2>/dev/null
    SERVICE_PROC=sleep
    # ps cuts a command line at COLUMNS where it is not told otherwise:
    # a marker far along the line is still read.
    other="$(sh -c 'while :; do sleep 1; done' padding-padding-padding-padding-padding-padding far-marker >/dev/null 2>&1 & echo $!)"
    out="$(export COLUMNS=20; _pid_command "$other")"
    check     "a process's whole command line is read" "far-marker" "$out"
    kill "$other" 2>/dev/null

    echo "== native: stdio =="
    rm -f "$tmp/native-served"
    out="$( (CHARS=4000 mcp_stdio --privacy) 2>&1)"
    served="$(tr '\n' ' ' < "$tmp/native-served" 2>/dev/null)"
    check     "stdio runs the engine, and passes --privacy on" "-c /cfg/wealthdb.cfg mcp-serve --stdio --chars 4000 --privacy" "$served"
    check     "told its kind and the data root" "kind=native root=/data" "$served"

    echo "== native: serve of a server that is off =="
    # serve with the server turned off: outside launchd it only says so.
    config 0 3300 token false
    out="$(XPC_SERVICE_NAME=0 mcp_serve 2>&1)"; rc=$?
    check     "serve of a server that is off fails" "1" "$rc"
    check     "and names the setup" "wealthdb config mcp-server" "$out"
    check_not "outside launchd it removes no agent" "removing the server from launchd" "$out"
    config 1 3300 token false

    # A start moves a log that has grown large aside.
    printf '%s\n' old-log-line > "$(_service_logfile)"
    SERVICE_LOG_MAX=5 _rotate_log "$(_service_logfile)"
    check     "a large log is moved aside at a start" "old-log-line" "$(cat "$(_service_logfile).1" 2>/dev/null)"
    [ ! -e "$(_service_logfile)" ] && pass "and a new one begins" || fail "the large log is still in place"
    printf 'x\n' > "$(_service_logfile)"
    _rotate_log "$(_service_logfile)"
    [ -e "$(_service_logfile)" ] && pass "a small log stays" || fail "a small log was moved"

    echo "== native: a server that cannot start =="
    # One that dies at once: start fails without its whole wait, shows
    # the log and leaves nothing behind.
    printf '#!/bin/sh\necho "the engine says no" >&2\nexit 3\n' > "$tmp/entry"
    t0=$SECONDS
    out="$(mcp_start 2>&1)"; rc=$?
    check     "a server that dies at once fails the start" "1" "$rc"
    if [ $((SECONDS - t0)) -lt 20 ]; then pass "at once, not after the wait a slow start gets"
    else fail "the start waited $((SECONDS - t0))s for a process that was gone"; fi
    check     "the start shows the end of its log" "the engine says no" "$out"
    [ ! -e "$(_service_pidfile)" ] && pass "and leaves no pid file" || fail "a pid file is left"
    # One that lives and never becomes the server: start gives up after
    # its wait, and ends the process it started.
    printf '#!/bin/sh\necho $$ > "%s"\nexec sleep 30\n' "$tmp/entry-pid" > "$tmp/entry"
    out="$(SERVICE_START_WAIT=1 mcp_start 2>&1)"; rc=$?
    check     "a server that never comes up fails the start" "1 mcp: the server did not start" "$rc $out"
    if _pid_runs "$(cat "$tmp/entry-pid")"; then fail "the process it started still runs"; kill "$(cat "$tmp/entry-pid")"
    else pass "and the process it started is ended"; fi

    echo "== native: under launchd (a stub) =="
    (
        # launchd is a function that records its calls. On `bootstrap`
        # it runs the agent: a stand-in that records its pid, as serve
        # does.
        _uses_launchd() { return 0; }
        launchctl() {
            echo "$*" >> "$tmp/launchctl-calls"
            case "$1" in
                bootstrap)
                    [ -z "${STUB_REFUSE:-}" ] || return 5
                    sleep 30 >/dev/null 2>&1 &
                    echo $! > "$(_service_pidfile)" ;;
            esac
        }
        domain="gui/$(id -u)" label=com.smartpointer.wealthdb.mcp
        plist="$tmp/home/Library/LaunchAgents/$label.plist"
        log="$tmp/home/Library/Logs/wealthdb/mcp.log"
        # An agent of this label that another setup wrote, whose file
        # names that setup's data folder, is not this start's to replace.
        (DATA_DIR=/elsewhere/mcp; _write_plist)
        cp "$plist" "$tmp/other.plist"
        : > "$tmp/launchctl-calls"
        out="$(mcp_start 2>&1)"; rc=$?
        check     "a start over another setup's agent is refused" \
            "1 mcp: $label is the server of another setup (its files are not in $tmp/native-data). Stop that one first, or name this one with WEALTHDB_MCP_CONTAINER." "$rc $out"
        check     "launchd is asked for nothing" "[]" "[$(cat "$tmp/launchctl-calls")]"
        if cmp -s "$plist" "$tmp/other.plist"; then pass "and the agent file is as it was"; else fail "the start changed the agent file"; fi
        # This setup's own agent, left by a server that died, is replaced.
        _write_plist
        : > "$tmp/launchctl-calls"
        out="$(mcp_start 2>&1)"; rc=$?
        check     "start succeeds"          "0"                    "$rc"
        check     "the agent file is written, with its log under the account's Logs" "<string>$log</string>" "$(cat "$plist" 2>/dev/null)"
        check     "an agent of that label is booted out, then this one in" \
            "bootout $domain/$label|bootstrap $domain $plist|" "$(tr '\n' '|' < "$tmp/launchctl-calls")"
        check     "the banner says it starts again at login" "It starts again at login. After an upgrade" "$out"
        out="$(mcp_status 2>&1)"
        check     "status names that log"   "log: $log"            "$out"
        pid="$(_native_pid)"
        : > "$tmp/launchctl-calls"
        out="$(mcp_stop 2>&1)"
        check     "stop says so"            "mcp: stopped the server" "$out"
        check     "stop boots the agent out" "bootout $domain/$label" "$(cat "$tmp/launchctl-calls")"
        [ ! -e "$plist" ] && pass "and removes its file, so the server stays stopped" || fail "the agent file is left"
        if _pid_runs "$pid"; then fail "the server still runs after stop"; kill "$pid"; else pass "the server is gone after stop"; fi

        out="$(STUB_REFUSE=1 mcp_start 2>&1)"; rc=$?
        check     "a start launchd refuses fails" "1 mcp: launchd did not take the server" "$rc $out"
        [ ! -e "$plist" ] && pass "and leaves no agent file" || fail "a refused start left its agent file"

        # serve of a server the config has turned off, in a process
        # launchd started under this label: the agent goes.
        _write_plist
        echo 1 > "$(_service_pidfile)"
        : > "$tmp/launchctl-calls"
        config 0 3300 token false
        out="$(XPC_SERVICE_NAME="$label" mcp_serve 2>&1)"; rc=$?
        check     "serve of a server that is off fails" "1"        "$rc"
        check     "under launchd it says that it leaves" "removing the server from launchd" "$out"
        check     "and boots its agent out" "bootout $domain/$label" "$(cat "$tmp/launchctl-calls")"
        [ ! -e "$plist" ] && [ ! -e "$(_service_pidfile)" ] && pass "its agent file and pid file are gone" ||
            fail "serve left the agent file or the pid file"
        config 1 3300 token false
        exit "$fails"
    )
    fails=$?
}
( native_cases; exit "$fails" )
fails=$?

echo "== serve is the native server's =="
out="$(mcp_main serve 2>&1)"; rc=$?
check     "serve in a tree of the container engine exits 2" "2" "$rc"
check     "and names the command that tree has" "wealthdb mcp start" "$out"

echo
if [ "$fails" -gt 0 ]; then
    echo "test_mcp.sh: $fails failure(s)"
    exit 1
fi
echo "test_mcp.sh: all passed"
