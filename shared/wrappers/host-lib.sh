# Shared functions for the collector wrappers that run a collector's
# .py directly on a host venv rather than in a container — the pure
# host-venv collectors and the host-side subcommands of the hybrid
# ones. The host-venv analogue of wrapper-lib.sh: no Docker, no
# mounts — it resolves the secrets / data / silver paths the same way
# (paths-lib.sh) and runs the collector's own .py under its .venv.
#
# Caller must SET (before sourcing):
#   HERE         absolute path to the wrapper's own directory. Convention:
#                HERE="$(cd "$(dirname "$0")" && pwd)".
#   NAME         short collector name, e.g. "schwab-api".
# Caller MAY SET:
#   ENV_PREFIX   upper-snake env-var prefix, derived from NAME when unset
#                (see paths-lib.sh).
#
# A host wrapper reads, in order:
#   host_begin VERB... -- "$@"   help, the verb check, the paths
#   host_source_env_file         where the collector takes a credential
#   case "$VERB" in ... esac     one host_run per verb; a `login` with
#                                no session calls host_login_probe,
#                                then prints its notice
#
# host_resolve_dirs "$@" POPULATES (for the caller):
#   SECRETS_DIR, DATA_DIR, SILVER_DB  — the resolved paths.
#   DEBUG_DIR                         — the host-side debug/trace cache
#                                       (only meaningful for a collector
#                                       that writes debug output).
#   FORWARD_ARGS                      — the args left after the dir flags
#                                       are consumed (pass to the .py).
#   SILVER_DB_GIVEN                   — 1 when --silver-db or
#                                       ${ENV_PREFIX}_SILVER_DB names the
#                                       silver DB, else 0.
#
# bash 3.2 compatibility: avoids namerefs / associative arrays.

# shellcheck disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/envfile-lib.sh"
# shellcheck disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/paths-lib.sh"

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

# host_resolve_dirs VERB [args...] — resolve the secrets / data / silver
# paths by the precedence of paths-lib.sh. The dir flags among args are
# consumed and the rest land in FORWARD_ARGS. SILVER_DB defaults to
# <data-dir>/<name>.db. VERB is the subcommand being run: a --silver-db
# on a verb that does not read the silver DB exits 2.
#
# DEBUG_DIR comes from collector_default_paths, like wrapper-lib.sh's
# HOST_DEBUG, so a hybrid collector's halves name one dir. Unlike
# DATA_DIR it is NOT created — a debug dir that does not exist
# means no debug output was ever written, which `prune` treats as a no-op.
host_resolve_dirs() {
    collector_default_paths
    collector_parse_dir_flags "${1:-}" "${@:2}"
    FORWARD_ARGS=("${FORWARD_ARGS[@]:1}")
    [[ -z "$SILVER_DB" ]] && SILVER_DB="$DATA_DIR/$NAME.db"
    mkdir -p "$DATA_DIR"
    # Same reasoning as the Docker wrappers: per-collector, owner-only.
    chmod 700 "$DATA_DIR"
}

# host_begin VERB... -- [ARGS...] — the start of a host wrapper. With no
# ARGS, or help / --help / -h as the first, print the wrapper's banner and
# exit 0. A first arg that is none of the VERBs exits 2, before any
# directory is made or any env file read. Otherwise set VERB to it and
# resolve the paths of the rest (host_resolve_dirs).
host_begin() {
    local -a verbs=()
    while [[ "$1" != -- ]]; do
        verbs+=("$1")
        shift
    done
    shift
    VERB="${1:-help}"
    [[ $# -gt 0 ]] && shift
    case "$VERB" in
        -h|--help|help)
            host_print_banner "${BASH_SOURCE[1]}"
            exit 0
            ;;
    esac
    local v known=0
    for v in "${verbs[@]}"; do
        [[ "$VERB" == "$v" ]] && known=1
    done
    if [[ $known == 0 ]]; then
        local IFS='|'
        echo "$NAME: unknown command '$VERB' (expected: ${verbs[*]})" >&2
        exit 2
    fi
    host_resolve_dirs "$VERB" "$@"
}

# host_run SCRIPT [ARGS...] — exec the collector's SCRIPT on its
# interpreter (host_python) with ARGS, then FORWARD_ARGS. ARGS name the
# resolved paths, so a flag in FORWARD_ARGS overrides them (argparse
# last-wins).
host_run() {
    local py script="$1"
    shift
    py="$(host_python)"
    exec "$py" "$HERE/$script" "$@" ${FORWARD_ARGS[@]+"${FORWARD_ARGS[@]}"}
}

# host_login_probe SCRIPT [ARGS...] — the session probe in the `login`
# of a collector with no session to mint. With --check among
# FORWARD_ARGS, host_run SCRIPT ARGS: the script probes the credential or
# the source. Returns otherwise, and the caller prints its notice. The
# notice is never this call's stdin, so SCRIPT reads the wrapper's own.
host_login_probe() {
    if host_has_flag --check ${FORWARD_ARGS[@]+"${FORWARD_ARGS[@]}"}; then
        host_run "$@"
    fi
}

# host_print_banner FILE — print FILE's leading comment banner (every
# line after the shebang up to the first line of code) as help, so the
# help text is the file's own header whatever its length.
host_print_banner() {
    awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "$1"
}

# host_has_flag FLAG ARGS... — true when FLAG is one of ARGS.
host_has_flag() {
    local flag="$1" a
    shift
    for a; do
        [[ "$a" == "$flag" ]] && return 0
    done
    return 1
}

# host_source_env_file [FILE] — source $SECRETS_DIR/<FILE>.env, so the
# credential env vars are set before the .py runs. FILE defaults to the
# collector's NAME. A collector that reads another source's credential
# names that source's file (refdata: finra). ${PREFIX}_ENV_FILE names
# another path. PREFIX is ENV_PREFIX for the collector's own file, else
# FILE in upper snake case (FINRA_ENV_FILE). The file is checked with
# `bash -n`, then sourced with every assignment exported. A missing file
# is fine. One that does not parse is skipped with a warning, and its
# error output is dropped, since it may quote a secret. The file wins
# over the environment, which changes nothing when an orchestrator
# already sourced the same file.
host_source_env_file() {
    local name="$NAME" prefix="$ENV_PREFIX" var f
    if [[ -n "${1:-}" ]]; then
        name="$1"
        prefix="$(collector_env_prefix "$name")"
    fi
    var="${prefix}_ENV_FILE"
    f="${!var:-$SECRETS_DIR/$name.env}"
    [[ -f "$f" ]] || return 0
    wealthdb_source_bash "$f" ||
        echo "${NAME}: env file $f has a bash syntax error; not sourcing it" >&2
}
