#!/usr/bin/env bash
#
# Unit tests for the mcp lifecycle script — no Docker and no engine
# required. Run via `make test-mcp` or directly. Sources mcp/mcp (which
# only runs mcp_main when executed, not sourced) against a stubbed
# engine and a stubbed docker, and checks the container geometry, the
# token, the auth gate and the outdated-image check. bash 3.2
# compatible (macOS).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Engine stub: prints the mcp-config lines in $tmp/mcp-config. mcp/mcp
# resolves WEALTHDB from WEALTHDB_BIN when it is SOURCED, so the stub is
# exported before the source line.
cat > "$tmp/engine-stub" <<EOF
#!/bin/sh
cat "$tmp/mcp-config"
EOF
chmod +x "$tmp/engine-stub"
export WEALTHDB_BIN="$tmp/engine-stub"

# Docker stub: records each call's argv (one arg per line) and answers
# the queries from STUB_* variables.
mkdir -p "$tmp/bin"
cat > "$tmp/bin/docker" <<EOF
#!/bin/sh
printf '%s\n' "\$@" > "$tmp/docker-args"
case "\$1 \$2" in
    "ps -q"|"ps -aq") [ -n "\${STUB_RUNNING:-}" ] && echo abc123 ;;
    "inspect -f")     echo "\${STUB_RUNNING_IMAGE:-sha256:one}" ;;
    "image inspect")  [ -n "\${STUB_NO_IMAGE:-}" ] && exit 1; echo "\${STUB_LATEST_IMAGE:-sha256:one}" ;;
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
    if printf '%s' "$3" | grep -qF -- "$2"; then pass "$1"
    else fail "$1"; printf '       wanted substring: %s\n       in: %s\n' "$2" "$3"; fi
}
check_not() { # desc, unwanted-substring, actual
    if printf '%s' "$3" | grep -qF -- "$2"; then fail "$1"; else pass "$1"; fi
}

config() { # enabled port auth insecure
    printf 'WEALTHDB_MCP_ENABLED=%s\nWEALTHDB_MCP_PORT=%s\nWEALTHDB_MCP_CONFIG_AUTH=%s\nWEALTHDB_MCP_CONFIG_INSECURE=%s\nWEALTHDB_GOLD_DB=%s\n' \
        "$1" "$2" "$3" "$4" "'$tmp/gold.db'" > "$tmp/mcp-config"
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
HOME=/home/u XDG_DATA_HOME= XDG_CONFIG_HOME=/cfg WEALTHDB_DATA_ROOT=/data WEALTHDB_CONFIG= _resolve_knobs
PORT=3300 BIND=both AUTH=token ROWS= MAX_ROWS= ALLOW_HOSTS=
_docker_run_args
joined="${DOCKER_ARGS[*]}"
check     "detached"               "run -d"                                       "$joined"
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

ROWS=200 MAX_ROWS=500 ALLOW_HOSTS="wealthdb-mcp,host.docker.internal"
_docker_run_args
joined="${DOCKER_ARGS[*]}"
check     "row knobs"              "--rows 200 --max-rows 500"                    "$joined"
check     "allowed hosts"          "--allow-host wealthdb-mcp --allow-host host.docker.internal" "$joined"
ROWS= MAX_ROWS= ALLOW_HOSTS=

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
check_not "no -t"                  " -t "                                         "$joined"
check_not "not detached"           " -d "                                         "$joined"
check_not "no publish"             " -p "                                         "$joined"
check_not "no token"               "token"                                        "$joined"
check     "data root read-only"    "/data:/data:ro"                               "$joined"
check     "serves stdio, privacy"  "mcp-serve --stdio --privacy"                  "$joined"

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
mode="$(stat -f '%Lp' "$TOKEN_FILE" 2>/dev/null || stat -c '%a' "$TOKEN_FILE")"
check     "token file mode 600"    "600"                                          "$mode"
_ensure_token 2>/dev/null
if [ "$(cat "$TOKEN_FILE")" = "$tok" ]; then pass "token kept across starts"; else fail "token kept across starts"; fi
WEALTHDB_MCP_TOKEN="from-the-environment-0123456789" _ensure_token
check     "env token wins"         "from-the-environment-0123456789"              "$(cat "$TOKEN_FILE")"
if WEALTHDB_MCP_TOKEN=short _ensure_token 2>/dev/null; then fail "short env token refused"; else pass "short env token refused"; fi

echo "== _load_config (env forms win) =="
config 1 3300 token 0
unset WEALTHDB_MCP_AUTH WEALTHDB_MCP_INSECURE
_load_config
check     "port read back"         "3300"                                         "$PORT"
check     "auth read back"         "token"                                        "$AUTH"
WEALTHDB_MCP_AUTH=none WEALTHDB_MCP_INSECURE=1 _load_config
check     "env auth wins"          "none"                                         "$AUTH"
check     "env insecure wins"      "1"                                            "$INSECURE"

echo "== mcp_start refusals =="
config 0 3300 token 0
if out="$(mcp_start 2>&1)"; then fail "refuses when not enabled"; else pass "refuses when not enabled"; fi
check     "says how to enable"     '"mcp": { "enabled": true'                      "$out"
config 1 3300 none 0
if out="$(mcp_start 2>&1)"; then fail "refuses auth none alone"; else pass "refuses auth none alone"; fi
check     "says what is missing"   "insecure"                                     "$out"
config 1 3300 token 0
if out="$(STUB_NO_IMAGE=1 mcp_start 2>&1)"; then fail "refuses without the image"; else pass "refuses without the image"; fi
check     "says to build"          "wealthdb build"                               "$out"

echo "== mcp_start (insecure, warned) =="
config 1 3300 none 1
out="$(mcp_start 2>&1)"
check     "started"                "is up"                                        "$out"
check_not "banner ends where it should" "mcp_help"                              "$out"
check     "insecure warning"       "INSECURE"                                     "$out"
check     "ran mcp-serve"          "--insecure"                                   "$(cat "$tmp/docker-args")"

echo "== mcp_status =="
config 1 3300 none 1
out="$(STUB_RUNNING=1 STUB_RUNNING_IMAGE=sha256:old STUB_LATEST_IMAGE=sha256:new mcp_status)"
check     "running"                "mcp: running"                                 "$out"
check     "outdated image"         "image: outdated"                              "$out"
check     "insecure warned"        "INSECURE"                                     "$out"
config 1 3300 token 0
out="$(STUB_RUNNING=1 STUB_RUNNING_IMAGE=sha256:same STUB_LATEST_IMAGE=sha256:same mcp_status)"
check     "current image"          "image: current"                               "$out"
check_not "no warning with a token" "INSECURE"                                    "$out"

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

echo
if [ "$fails" -gt 0 ]; then
    echo "test_mcp.sh: $fails failure(s)"
    exit 1
fi
echo "test_mcp.sh: all passed"
