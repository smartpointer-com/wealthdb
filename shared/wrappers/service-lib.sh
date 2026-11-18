# shellcheck shell=bash
# service-lib.sh — the lifecycle helpers web/web and mcp/mcp share: the
# Docker check, the engine's settings, the loopback publish, the
# container's state and the optional env file. Bash; sourced by both,
# which keep their own subcommands and their own `docker run` argv.
#
# Caller must SET (before sourcing):
#   SERVICE        the service's name, `web` or `mcp`. It heads every
#                  message, names the engine's `config-env` section and
#                  the default env file, and gives WEALTHDB_<SERVICE>_*
#                  the variables below are read from.
#   WEALTHDB       the engine wrapper (`config-env` runs through it).
# Caller MAY SET:
#   SERVICE_CPORT  the port the server listens on inside the container;
#                  unset, the published port.
# _is_running and _exists read CONTAINER at call time.

# shellcheck disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/envfile-lib.sh"

SERVICE_ENV="WEALTHDB_$(printf '%s' "$SERVICE" | tr '[:lower:]' '[:upper:]')"

_require_docker() {
    command -v docker >/dev/null 2>&1 || { echo "$SERVICE: docker not found on PATH" >&2; return 1; }
}

# _config_env evaluates the engine's hidden `config-env SERVICE`: the
# service's resolved settings and the gold path, as shell assignments.
# The engine is the one component that parses and validates
# wealthdb.cfg; WEALTHDB_CONFIG names a non-default file.
_config_env() {
    _require_docker || return 1
    local out
    if [ -n "${WEALTHDB_CONFIG:-}" ]; then
        out="$("$WEALTHDB" -c "$WEALTHDB_CONFIG" config-env "$SERVICE")" || out=""
    else
        out="$("$WEALTHDB" config-env "$SERVICE")" || out=""
    fi
    if [ -z "$out" ]; then
        echo "$SERVICE: could not read settings via 'wealthdb config-env $SERVICE' (is wealthdb.cfg valid?)" >&2
        return 1
    fi
    eval "$out"
}

# _publish_args PORT BIND → prints `docker run` -p flags (one token
# per line) for loopback IPv4 and/or IPv6, onto SERVICE_CPORT in the
# container.
_publish_args() {
    local port="$1" bind="$2" cport="${SERVICE_CPORT:-$1}"
    case "$bind" in
        v4)   printf '%s\n' -p "127.0.0.1:${port}:${cport}" ;;
        v6)   printf '%s\n' -p "[::1]:${port}:${cport}" ;;
        both) printf '%s\n' -p "127.0.0.1:${port}:${cport}" -p "[::1]:${port}:${cport}" ;;
        *)    echo "$SERVICE: invalid ${SERVICE_ENV}_BIND '$bind' (want both|v4|v6)" >&2; return 1 ;;
    esac
}

_is_running() { [ -n "$(docker ps  -q -f "name=^${CONTAINER}\$" -f status=running 2>/dev/null)" ]; }
_exists()     { [ -n "$(docker ps -aq -f "name=^${CONTAINER}\$" 2>/dev/null)" ]; }

# _source_env_file sources the optional env file for the secrets and
# the knobs: ${SERVICE_ENV}_ENV_FILE, else ~/.secrets/wealthdb-SERVICE.env
# (repo convention: secrets live in ~/.secrets, never in the config
# file). It is a bash script: syntax-checked, then sourced with every
# assignment exported. A file that does not parse returns 1, and bash's
# message is not shown, since it quotes the line.
_source_env_file() {
    local var="${SERVICE_ENV}_ENV_FILE" envf
    envf="${!var:-$HOME/.secrets/wealthdb-$SERVICE.env}"
    if [ -f "$envf" ] && ! wealthdb_source_bash "$envf"; then
        echo "$SERVICE: $envf has a bash syntax error" >&2
        return 1
    fi
}
