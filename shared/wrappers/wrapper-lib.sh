# Shared functions for the per-collector `docker run` wrappers.
#
# Each collector wrapper is a thin file that:
#   1. sets a small config block (NAME, ENV_PREFIX, ENV_VARS, optional
#      feature flags),
#   2. sources this file,
#   3. handles its own special subcommands inline (build, help, plus
#      any pure-host-side shortcuts like fidelity-web's `load`),
#   4. calls `wrapper_main "$@"` for the common docker-run path.
#
# Caller must SET (before sourcing):
#   HERE         absolute path to the wrapper's own directory. Convention:
#                HERE="$(cd "$(dirname "$0")" && pwd)" at the top of each
#                wrapper.
#   NAME         short collector name, e.g. "fidelity-web".
#   ENV_PREFIX   upper-cased env-var prefix, e.g. "FIDELITY_WEB". Used to
#                derive both override knobs (${ENV_PREFIX}_SECRETS_DIR
#                etc.) and the force-replace switch
#                (${ENV_PREFIX}_FORCE_REPLACE).
#   ENV_VARS     Bash array of env-var names to forward into the container
#                via `-e VAR` (e.g. (FIDELITY_USERNAME FIDELITY_PASSWORD)).
#
# Caller MAY SET (before sourcing) to opt in to features:
#   HAS_DEBUG=1       Mount $HOST_DEBUG → /debug. Required for collectors
#                     whose --screenshot-dir / --trace flags write under
#                     /debug.
#   HAS_APP_MOUNT=1   Mount $HERE → /app (live host-side source). Used by
#                     collectors under active iteration so a code edit
#                     doesn't need a docker rebuild.
#   HAS_SAFETY=1      Refuse to clobber a RUNNING container with the same
#                     name. Required where a docker rm -f mid-MFA would
#                     burn a 2FA prompt or evict a VNC handoff. Override
#                     with ${ENV_PREFIX}_FORCE_REPLACE=1 when the live
#                     container is known-stuck.
#   HAS_VNC=1         Forward a host-side VNC port for the `vnc-login`
#                     subcommand. Walks 5900-6000 for a free port (defends
#                     against macOS Screen Sharing, prior crashed runs,
#                     other VNC servers) and passes the chosen port as
#                     VNC_HOST_PORT inside the container.
#
# wrapper_init POPULATES (for the caller and the helpers below):
#   HOST_SECRETS, HOST_DATA, HOST_DEBUG (when HAS_DEBUG=1),
#   CONTAINER_NAME, IMAGE.
#
# bash 3.2 compatibility: this lib avoids namerefs and associative
# arrays so it runs on Apple's /bin/bash. Indirect expansion (${!var})
# is used to read the per-prefix override variables.

# ----------------------------------------------------------------------
# Internals
# ----------------------------------------------------------------------

# _envvar SUFFIX DEFAULT — echo the value of ${ENV_PREFIX}_${SUFFIX},
# or DEFAULT if unset/empty.
_envvar() {
    local var="${ENV_PREFIX}_$1"
    echo "${!var:-$2}"
}

# ----------------------------------------------------------------------
# Initialisation + help-text helpers
# ----------------------------------------------------------------------

# Directory resolution precedence (highest first):
#   1. a --secrets-dir / --data-dir / --silver-db CLI flag (parsed later,
#      in wrapper_resolve_dir_args, so it overrides everything here)
#   2. the per-collector ${ENV_PREFIX}_SECRETS_DIR / _DATA_DIR env var
#   3. the fleet-wide WEALTHDB_SECRETS_DIR / WEALTHDB_DATA_ROOT env var
#      (lets wealthdb-nightly / wealthdb-refresh set one knob for all)
#   4. the ~/.secrets and ~/wealthdb/<name> defaults
wrapper_init() {
    IMAGE="$(_envvar IMAGE "wealthdb/${NAME}:latest")"
    CONTAINER_NAME="$(_envvar CONTAINER "$NAME")"

    local secrets_default="${WEALTHDB_SECRETS_DIR:-$HOME/.secrets}"
    HOST_SECRETS="$(_envvar SECRETS_DIR "$secrets_default")"

    local data_default="$HOME/wealthdb/$NAME"
    [[ -n "${WEALTHDB_DATA_ROOT:-}" ]] && data_default="${WEALTHDB_DATA_ROOT%/}/$NAME"
    HOST_DATA="$(_envvar DATA_DIR "$data_default")"

    if [[ "${HAS_DEBUG:-0}" == "1" ]]; then
        HOST_DEBUG="$(_envvar DEBUG_DIR "$HOME/.cache/${NAME}-debug")"
    fi
}

# Parse the uniform directory-override flags out of the forwarded args:
#   --secrets-dir DIR   override HOST_SECRETS (the /secrets mount source)
#   --data-dir    DIR   override HOST_DATA    (the /data mount source —
#                       holds both the bronze dumps and the silver DB)
#   --silver-db   PATH  put the silver DB at an explicit HOST path outside
#                       the data dir: bind-mounts its parent to /silver and
#                       rewrites the flag to the in-container path (the inner
#                       load.py accepts --silver-db, default /data/<name>.db)
# Both `--flag VALUE` and `--flag=VALUE` forms are accepted. Recognised
# flags are CONSUMED; everything else is collected into FORWARD_ARGS for the
# container. SILVER_MOUNT holds the extra `-v` args (empty unless --silver-db
# was given). bash 3.2 safe (no namerefs / associative arrays).
wrapper_resolve_dir_args() {
    FORWARD_ARGS=()
    SILVER_MOUNT=()
    local silver=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --secrets-dir)   HOST_SECRETS="$2"; shift 2 ;;
            --secrets-dir=*) HOST_SECRETS="${1#*=}"; shift ;;
            --data-dir)      HOST_DATA="$2"; shift 2 ;;
            --data-dir=*)    HOST_DATA="${1#*=}"; shift ;;
            --silver-db)     silver="$2"; shift 2 ;;
            --silver-db=*)   silver="${1#*=}"; shift ;;
            *)               FORWARD_ARGS+=("$1"); shift ;;
        esac
    done
    if [[ -n "$silver" ]]; then
        local d b
        d="$(cd "$(dirname "$silver")" 2>/dev/null && pwd)" || d="$(dirname "$silver")"
        b="$(basename "$silver")"
        mkdir -p "$d"
        SILVER_MOUNT=(-v "$d:/silver")
        FORWARD_ARGS+=(--silver-db "/silver/$b")
    fi
}

# Three lines for the "Mounted into the container:" block in usage().
# Callers paste with $(wrapper_mounts_help).
wrapper_mounts_help() {
    echo "  $HOST_SECRETS  ->  /secrets"
    echo "  $HOST_DATA     ->  /data"
    if [[ "${HAS_DEBUG:-0}" == "1" ]]; then
        echo "  $HOST_DEBUG    ->  /debug"
    fi
}

# ----------------------------------------------------------------------
# Subcommand helpers
# ----------------------------------------------------------------------

# Implement `./<wrapper> build [extra args]` — exec docker build and
# pass through any extra args. Caller dispatches on $1 == "build".
wrapper_build() {
    shift  # drop "build"
    exec docker build -t "$IMAGE" "$@" "$HERE"
}

# ----------------------------------------------------------------------
# Pre-run sanity checks + docker-arg construction
# ----------------------------------------------------------------------

# Make sure the mount-point dirs exist on the host (docker would
# otherwise silently create them as root-owned). Errors if the secrets
# dir is missing — that's a configuration issue, not something the
# wrapper should paper over.
wrapper_check_mounts() {
    mkdir -p "$HOST_DATA"
    if [[ "${HAS_DEBUG:-0}" == "1" ]]; then
        mkdir -p "$HOST_DEBUG"
    fi
    if [[ ! -d "$HOST_SECRETS" ]]; then
        echo "${NAME}: secrets dir does not exist: $HOST_SECRETS" >&2
        echo "Create it with: mkdir -p $HOST_SECRETS && chmod 700 $HOST_SECRETS" >&2
        exit 1
    fi
}

# Allocate `-it` only when stdin AND stdout are TTYs, so the wrapper is
# safe to pipe (log capture, cron) while still giving interactive
# prompts at a terminal. Sets TTY_ARGS as an array; the `${arr[@]+...}`
# idiom at the call site keeps bash 3.2 happy under `set -u` when the
# array is empty.
wrapper_tty_args() {
    TTY_ARGS=()
    if [[ -t 0 && -t 1 ]]; then
        TTY_ARGS=(-it)
    fi
}

# Find a free TCP port in 5900-6000 for VNC forwarding. Default 5900
# can be taken (another VNC server, macOS Screen Sharing, a prior
# crashed container whose `docker run -p` lingers); we walk +1 until
# something is free, capped at +100 as a defensive limit. The
# container side always binds 5900 — only the host-side mapping moves.
# Sets vnc_host_port; aborts the wrapper if every port in the range is
# taken.
wrapper_vnc_port() {
    vnc_host_port=""
    local p
    for ((p = 5900; p <= 6000; p++)); do
        if ! lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
            vnc_host_port="$p"
            break
        fi
    done
    if [[ -z "$vnc_host_port" ]]; then
        echo "${NAME}: no free TCP port between 5900-6000 on this host." >&2
        echo "  Close some applications and retry." >&2
        exit 3
    fi
    if [[ "$vnc_host_port" != "5900" ]]; then
        echo "${NAME}: 127.0.0.1:5900 is taken; using ${vnc_host_port} instead." >&2
    fi
}

# Refuse to clobber a RUNNING container with the same name. Background:
# a wrapper invocation while a `download` or `login` is mid-MFA would
# otherwise `docker rm -f` the live container and burn the in-flight
# 2FA prompt (or evict a VNC handoff). Override with
# ${ENV_PREFIX}_FORCE_REPLACE=1 when the live container is known-stuck.
# A best-effort `docker rm -f` at the end cleans up any prior
# exited-but-not-cleaned-up container with this name.
wrapper_safety_guard() {
    local force_var="${ENV_PREFIX}_FORCE_REPLACE"
    local force="${!force_var:-0}"
    if docker ps --filter "name=^${CONTAINER_NAME}\$" --filter "status=running" -q | grep -q .; then
        if [[ "$force" != "1" ]]; then
            echo "${NAME}: a container named '$CONTAINER_NAME' is already running." >&2
            echo "  Refusing to evict it — that would kill any live ${NAME} session." >&2
            echo "  Options:" >&2
            echo "    * Use a different name:    ${ENV_PREFIX}_CONTAINER=otherthing $0 $*" >&2
            echo "    * Enter the existing one:  docker exec -it $CONTAINER_NAME sh" >&2
            echo "    * Force replace:           ${ENV_PREFIX}_FORCE_REPLACE=1 $0 $*" >&2
            exit 2
        fi
        echo "${NAME}: ${ENV_PREFIX}_FORCE_REPLACE=1 — evicting live container" >&2
    fi
    docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
}

# ----------------------------------------------------------------------
# Common dispatch
# ----------------------------------------------------------------------

# The common docker-run path. Caller has already handled its own
# special subcommands (build, help, any host-side shortcuts) before
# falling through here. Composes the docker argv from the active
# features and execs it.
wrapper_main() {
    # Pull the --secrets-dir/--data-dir/--silver-db flags out of the args
    # (they may override HOST_SECRETS/HOST_DATA + add the /silver mount),
    # then continue with the remaining args as the positional parameters.
    wrapper_resolve_dir_args "$@"
    set -- ${FORWARD_ARGS[@]+"${FORWARD_ARGS[@]}"}

    wrapper_check_mounts
    wrapper_tty_args

    local -a extra_args=()
    if [[ "${HAS_VNC:-0}" == "1" ]]; then
        local sub="${1:-}" need_vnc=0 v
        # Caller may declare extra subcommands that need VNC by setting
        # VNC_SUBCOMMANDS=(vnc-login explore …). 'vnc-login' is always
        # in the set so existing wrappers keep working without the var.
        for v in vnc-login ${VNC_SUBCOMMANDS[@]+"${VNC_SUBCOMMANDS[@]}"}; do
            [[ "$sub" == "$v" ]] && { need_vnc=1; break; }
        done
        if [[ $need_vnc == 1 ]]; then
            wrapper_vnc_port
            extra_args+=(-p "127.0.0.1:${vnc_host_port}:5900"
                         -e "VNC_HOST_PORT=${vnc_host_port}")
        fi
    fi

    if [[ "${HAS_SAFETY:-0}" == "1" ]]; then
        wrapper_safety_guard "$@"
    fi

    local -a docker_args=(
        --rm --name "$CONTAINER_NAME"
        ${TTY_ARGS[@]+"${TTY_ARGS[@]}"}
        ${extra_args[@]+"${extra_args[@]}"}
        --user "$(id -u):$(id -g)"
        -e HOME=/tmp
    )
    local v
    for v in ${ENV_VARS[@]+"${ENV_VARS[@]}"}; do
        docker_args+=(-e "$v")
    done
    docker_args+=(
        -v "$HOST_SECRETS:/secrets"
        -v "$HOST_DATA:/data"
        ${SILVER_MOUNT[@]+"${SILVER_MOUNT[@]}"}
    )
    if [[ "${HAS_DEBUG:-0}" == "1" ]]; then
        docker_args+=(-v "$HOST_DEBUG:/debug")
    fi
    if [[ "${HAS_APP_MOUNT:-0}" == "1" ]]; then
        docker_args+=(-v "$HERE:/app")
    fi

    exec docker run "${docker_args[@]}" "$IMAGE" "$@"
}
