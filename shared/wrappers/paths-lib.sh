# shellcheck shell=bash
# The paths of a collector verb, resolved once for both wrapper
# libraries: wrapper-lib.sh maps them onto `docker run` mounts, host-lib.sh
# passes them to the collector's .py on the host.
#
# Caller must SET (before calling):
#   NAME         short collector name, e.g. "fidelity-web".
# Caller MAY SET:
#   ENV_PREFIX   upper-snake env-var prefix. collector_default_paths
#                derives it from NAME when unset: "fidelity-web" gives
#                "FIDELITY_WEB".
#   SILVER_SUBCOMMANDS
#                Bash array of the subcommands that read --silver-db.
#                Replaces the default (load), e.g. (load fetch-prices)
#                for cointracking.
#
# Precedence, highest first:
#   1. a --secrets-dir / --data-dir / --silver-db flag
#      (collector_parse_dir_flags)
#   2. the per-collector ${ENV_PREFIX}_SECRETS_DIR / _DATA_DIR /
#      _DEBUG_DIR / _SILVER_DB
#   3. the fleet-wide WEALTHDB_SECRETS_DIR / WEALTHDB_DATA_ROOT, which
#      let an orchestrator set one knob for all
#   4. ~/.secrets, ${XDG_DATA_HOME:-~/.local/share}/wealthdb/<name> and
#      ${XDG_CACHE_HOME:-~/.cache}/wealthdb/debug/<name>
#
# bash 3.2 compatibility: no namerefs or associative arrays; indirect
# expansion (${!var}) reads the per-prefix override variables.

# _collector_envvar SUFFIX DEFAULT — echo ${ENV_PREFIX}_${SUFFIX}, or
# DEFAULT if unset or empty.
_collector_envvar() {
    local var="${ENV_PREFIX}_$1"
    echo "${!var:-$2}"
}

# collector_default_paths — derive ENV_PREFIX when unset, then set
# SECRETS_DIR, DATA_DIR and DEBUG_DIR from the environment and the
# defaults. DEBUG_DIR is the debug/trace cache (screenshots, HTML
# captures, Playwright traces), outside the bronze tree. It has no flag:
# the verbs that write it run in a container, and a hybrid collector's
# halves name the same dir through this one rule.
collector_default_paths() {
    : "${ENV_PREFIX:=$(printf '%s' "$NAME" | tr 'a-z-' 'A-Z_')}"
    local secrets_default="${WEALTHDB_SECRETS_DIR:-$HOME/.secrets}"
    SECRETS_DIR="$(_collector_envvar SECRETS_DIR "$secrets_default")"

    # The default data root follows the XDG Base Directory spec
    # ($XDG_DATA_HOME, falling back to ~/.local/share).
    local data_default="${XDG_DATA_HOME:-$HOME/.local/share}/wealthdb/$NAME"
    [[ -n "${WEALTHDB_DATA_ROOT:-}" ]] && data_default="${WEALTHDB_DATA_ROOT%/}/$NAME"
    DATA_DIR="$(_collector_envvar DATA_DIR "$data_default")"

    DEBUG_DIR="$(_collector_envvar DEBUG_DIR "${XDG_CACHE_HOME:-$HOME/.cache}/wealthdb/debug/$NAME")"
}

# collector_verb_takes_silver VERB — is VERB a subcommand that reads the
# silver DB? The set is SILVER_SUBCOMMANDS when set, else just `load`.
collector_verb_takes_silver() {
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

# The silver-reading verbs as a display string, for the reject message.
collector_silver_verbs_str() {
    if [[ -n "${SILVER_SUBCOMMANDS+x}" ]]; then
        echo "${SILVER_SUBCOMMANDS[*]}"
    else
        echo "load"
    fi
}

# collector_parse_dir_flags VERB [ARGS...] — consume the uniform
# directory-override flags, in both `--flag VALUE` and `--flag=VALUE`
# forms:
#   --secrets-dir DIR   sets SECRETS_DIR
#   --data-dir    DIR   sets DATA_DIR (the bronze dumps, and the silver
#                       DB by default)
#   --silver-db   PATH  sets SILVER_DB
# Every other arg lands in FORWARD_ARGS, in order. The verb is the first
# of them, so a flag may also come before it. SILVER_DB starts from
# ${ENV_PREFIX}_SILVER_DB, and SILVER_DB_GIVEN is 1 when the flag or that
# variable names the silver DB, else 0.
#
# --silver-db only means something on a verb that reads the silver DB, so
# on any other verb it is rejected with exit 2, rather than swallowed or
# forwarded into a parser that rejects it naming a path the caller never
# typed. ${ENV_PREFIX}_SILVER_DB is ambient config naming where silver
# lives, not a per-verb assertion, so a verb that does not read it
# ignores it rather than failing every `login` in a shell that exports
# it.
collector_parse_dir_flags() {
    FORWARD_ARGS=()
    SILVER_DB="$(_collector_envvar SILVER_DB "")"
    SILVER_DB_GIVEN=0
    [[ -n "$SILVER_DB" ]] && SILVER_DB_GIVEN=1
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
    local verb="${FORWARD_ARGS[0]:-}"
    if [[ $silver_from_flag == 1 ]] && ! collector_verb_takes_silver "$verb"; then
        echo "$NAME: --silver-db does not apply to '${verb:-<no subcommand>}'" \
             "— it is only read by: $(collector_silver_verbs_str)." >&2
        exit 2
    fi
    [[ $silver_from_flag == 1 ]] && SILVER_DB_GIVEN=1
    return 0
}
