# Shared functions for the collector wrappers that run a collector's
# .py directly on a host venv rather than in a container — the pure
# host-venv collectors and the host-side subcommands of the hybrid
# ones. The host-venv analogue of wrapper-lib.sh: no Docker, no
# mounts — it resolves the secrets / data / silver paths the same way and
# runs the collector's own .py under its .venv.
#
# Caller must SET (before sourcing):
#   HERE         absolute path to the wrapper's own directory. Convention:
#                HERE="$(cd "$(dirname "$0")" && pwd)".
#   NAME         short collector name, e.g. "schwab-api".
#   ENV_PREFIX   upper-snake env-var prefix, e.g. "SCHWAB_API".
#
# host_resolve_dirs "$@" POPULATES (for the caller):
#   SECRETS_DIR, DATA_DIR, SILVER_DB  — the resolved paths.
#   DEBUG_DIR                         — the host-side debug/trace cache
#                                       (only meaningful for a collector
#                                       that writes debug output).
#   FORWARD_ARGS                      — the args left after the dir flags
#                                       are consumed (pass to the .py).
#
# bash 3.2 compatibility: avoids namerefs / associative arrays; indirect
# expansion (${!var}) reads the per-prefix override variables.

# _host_envvar SUFFIX DEFAULT — echo ${ENV_PREFIX}_${SUFFIX} or DEFAULT.
_host_envvar() {
    local var="${ENV_PREFIX}_$1"
    echo "${!var:-$2}"
}

# host_python — the interpreter to run the collector's .py with. Prefers the
# collector's own .venv (built by `make build-<name>`); falls back to a
# system python3 for an un-built checkout.
host_python() {
    if [[ -x "$HERE/.venv/bin/python" ]]; then
        echo "$HERE/.venv/bin/python"
    else
        command -v python3 || command -v python
    fi
}

# host_verb_takes_silver — is `$1` a subcommand that consumes the silver DB?
# The set defaults to just `load`; a wrapper with another silver-consuming
# verb sets SILVER_SUBCOMMANDS to REPLACE that default. Mirrors
# wrapper-lib.sh's function of the same shape.
host_verb_takes_silver() {
    local want="$1" v
    local -a verbs
    if [[ -n "${SILVER_SUBCOMMANDS+x}" ]]; then
        verbs=(${SILVER_SUBCOMMANDS[@]+"${SILVER_SUBCOMMANDS[@]}"})
    else
        verbs=(load)
    fi
    for v in ${verbs[@]+"${verbs[@]}"}; do
        [[ "$want" == "$v" ]] && return 0
    done
    return 1
}

# host_resolve_dirs VERB [args...] — resolve the secrets / data / silver paths
# with the same precedence as the Docker wrappers (highest first):
#   1. --secrets-dir / --data-dir / --silver-db CLI flag
#   2. the per-collector ${ENV_PREFIX}_SECRETS_DIR / _DATA_DIR / _SILVER_DB
#   3. the fleet-wide WEALTHDB_SECRETS_DIR / WEALTHDB_DATA_ROOT
#   4. the ~/.secrets and ${XDG_DATA_HOME:-~/.local/share}/wealthdb/<name>
#      defaults
# SILVER_DB defaults to <data-dir>/<name>.db. Recognised flags are consumed;
# the rest land in FORWARD_ARGS.
#
# DEBUG_DIR is the host-side debug/trace cache (screenshots, HTML captures,
# Playwright traces) — outside the bronze tree, and resolved exactly as
# wrapper-lib.sh resolves HOST_DEBUG so a hybrid collector's Docker and
# host-side halves always name the same dir. It has no CLI flag here: the
# verbs that write it are the containerised ones. Unlike DATA_DIR it is NOT
# created — a debug dir that does not exist means no debug output was ever
# written, which `prune` treats as a no-op.
#
# VERB is the subcommand being run. --silver-db only means something on a
# verb that builds the silver DB, so on any other verb it is rejected: the
# wrapper consumes the flag and only the `load` arm reads SILVER_DB, so
# swallowing it would look honoured. The ${ENV_PREFIX}_SILVER_DB env var is
# ambient config naming where silver lives, not a per-verb assertion, so a
# non-silver verb ignores it rather than failing.
host_resolve_dirs() {
    local verb="${1:-}"
    shift || true
    local secrets_default="${WEALTHDB_SECRETS_DIR:-$HOME/.secrets}"
    SECRETS_DIR="$(_host_envvar SECRETS_DIR "$secrets_default")"

    # Default data root follows the XDG Base Directory spec
    # ($XDG_DATA_HOME, falling back to ~/.local/share).
    local data_default="${XDG_DATA_HOME:-$HOME/.local/share}/wealthdb/$NAME"
    [[ -n "${WEALTHDB_DATA_ROOT:-}" ]] && data_default="${WEALTHDB_DATA_ROOT%/}/$NAME"
    DATA_DIR="$(_host_envvar DATA_DIR "$data_default")"

    DEBUG_DIR="$(_host_envvar DEBUG_DIR "$HOME/.cache/${NAME}-debug")"

    SILVER_DB="$(_host_envvar SILVER_DB "")"

    FORWARD_ARGS=()
    local silver_from_flag=0
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --secrets-dir)   SECRETS_DIR="$2"; shift 2 ;;
            --secrets-dir=*) SECRETS_DIR="${1#*=}"; shift ;;
            --data-dir)      DATA_DIR="$2"; shift 2 ;;
            --data-dir=*)    DATA_DIR="${1#*=}"; shift ;;
            --silver-db)     SILVER_DB="$2"; silver_from_flag=1; shift 2 ;;
            --silver-db=*)   SILVER_DB="${1#*=}"; silver_from_flag=1; shift ;;
            *)               FORWARD_ARGS+=("$1"); shift ;;
        esac
    done
    if [[ $silver_from_flag == 1 ]] && ! host_verb_takes_silver "$verb"; then
        echo "$NAME: --silver-db does not apply to '${verb:-<no subcommand>}'" \
             "— it is only read by: $(host_silver_verbs_str)." >&2
        exit 2
    fi
    [[ -z "$SILVER_DB" ]] && SILVER_DB="$DATA_DIR/$NAME.db"
    mkdir -p "$DATA_DIR"
}

# The silver-consuming verbs as a display string, for the reject message.
host_silver_verbs_str() {
    if [[ -n "${SILVER_SUBCOMMANDS+x}" ]]; then
        echo "${SILVER_SUBCOMMANDS[*]}"
    else
        echo "load"
    fi
}

# host_source_env_file — best-effort: source $SECRETS_DIR/<name>.env (override
# the path via ${ENV_PREFIX}_ENV_FILE) so the collector's credential env vars
# are populated before the .py runs, removing the "source the env file
# yourself" friction. Syntax-checked with `bash -n` first, then sourced.
# Absent file is fine. The file wins over the current environment;
# that's idempotent when an orchestrator (wealthdb-nightly) already sourced
# the same credentials.
host_source_env_file() {
    local f
    f="$(_host_envvar ENV_FILE "$SECRETS_DIR/$NAME.env")"
    [[ -f "$f" ]] || return 0
    if ! bash -n "$f" 2>/dev/null; then
        echo "${NAME}: env file $f has a bash syntax error; not sourcing it" >&2
        return 0
    fi
    set -a
    # shellcheck disable=SC1090
    source "$f"
    set +a
}
