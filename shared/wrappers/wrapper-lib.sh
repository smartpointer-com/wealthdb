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
#   HOST_STARTUPCACHE, CONTAINER_NAME, IMAGE.
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
#   4. the ~/.secrets and ${XDG_DATA_HOME:-~/.local/share}/wealthdb/<name>
#      defaults
wrapper_init() {
    IMAGE="$(_envvar IMAGE "wealthdb/${NAME}:latest")"
    CONTAINER_NAME="$(_envvar CONTAINER "$NAME")"

    local secrets_default="${WEALTHDB_SECRETS_DIR:-$HOME/.secrets}"
    HOST_SECRETS="$(_envvar SECRETS_DIR "$secrets_default")"

    # Default data root follows the XDG Base Directory spec
    # ($XDG_DATA_HOME, falling back to ~/.local/share).
    local data_default="${XDG_DATA_HOME:-$HOME/.local/share}/wealthdb/$NAME"
    [[ -n "${WEALTHDB_DATA_ROOT:-}" ]] && data_default="${WEALTHDB_DATA_ROOT%/}/$NAME"
    HOST_DATA="$(_envvar DATA_DIR "$data_default")"

    if [[ "${HAS_DEBUG:-0}" == "1" ]]; then
        # Debug/trace cache under the XDG cache dir, namespaced per
        # collector like the startupCache below.
        HOST_DEBUG="$(_envvar DEBUG_DIR "${XDG_CACHE_HOME:-$HOME/.cache}/wealthdb/debug/$NAME")"
    fi

    # Host cache dir for the browsers' relocated startupCache (see
    # collectorkit.launch). Mounted into every container so Firefox's
    # regenerable compiled-bytecode cache lands here rather than next to the
    # session cookie under /secrets. XDG cache path so it survives reboots
    # (keeping launches fast) yet is plainly non-sensitive; one generic dir,
    # not per-profile — the container keys per-profile subdirs itself.
    HOST_STARTUPCACHE="${XDG_CACHE_HOME:-$HOME/.cache}/wealthdb/startupcache"
}

# wrapper_verb_takes_silver — is `$1` a subcommand that consumes the silver
# DB? The set defaults to just `load`. A wrapper with another
# silver-consuming verb sets SILVER_SUBCOMMANDS to REPLACE that default —
# e.g. SILVER_SUBCOMMANDS=(load fetch-prices) for cointracking. Mirrors the
# VNC_SUBCOMMANDS convention below.
wrapper_verb_takes_silver() {
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
# container. SILVER_MOUNT holds the extra `-v` args (empty unless a silver
# path is in play).
#
# --silver-db only means something on a verb that builds the silver DB, so
# on any other verb it is rejected here rather than forwarded into a parser
# that would reject it naming an in-container path the caller never typed.
# The ${PREFIX}_SILVER_DB env var is different: it is ambient config naming
# where silver lives, so a non-silver verb ignores it rather than failing
# every `login` in a shell that exports it.
# bash 3.2 safe (no namerefs / associative arrays).
wrapper_resolve_dir_args() {
    FORWARD_ARGS=()
    SILVER_MOUNT=()
    local silver silver_from_flag=0
    silver="$(_envvar SILVER_DB "")"
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --secrets-dir)   HOST_SECRETS="$2"; shift 2 ;;
            --secrets-dir=*) HOST_SECRETS="${1#*=}"; shift ;;
            --data-dir)      HOST_DATA="$2"; shift 2 ;;
            --data-dir=*)    HOST_DATA="${1#*=}"; shift ;;
            --silver-db)     silver="$2"; silver_from_flag=1; shift 2 ;;
            --silver-db=*)   silver="${1#*=}"; silver_from_flag=1; shift ;;
            *)               FORWARD_ARGS+=("$1"); shift ;;
        esac
    done
    # The subcommand is the first arg that isn't one of the dir flags.
    local sub="${FORWARD_ARGS[0]:-}"
    if ! wrapper_verb_takes_silver "$sub"; then
        if [[ $silver_from_flag == 1 ]]; then
            echo "$NAME: --silver-db does not apply to '${sub:-<no subcommand>}'" \
                 "— it is only read by: $(wrapper_silver_verbs_str)." >&2
            exit 2
        fi
        return
    fi
    if [[ -n "$silver" ]]; then
        local d b
        d="$(cd "$(dirname "$silver")" 2>/dev/null && pwd)" || d="$(dirname "$silver")"
        b="$(basename "$silver")"
        mkdir -p "$d"
        SILVER_MOUNT=(-v "$d:/silver")
        FORWARD_ARGS+=(--silver-db "/silver/$b")
    fi
}

# The silver-consuming verbs as a display string, for the reject message.
wrapper_silver_verbs_str() {
    if [[ -n "${SILVER_SUBCOMMANDS+x}" ]]; then
        echo "${SILVER_SUBCOMMANDS[*]}"
    else
        echo "load"
    fi
}

# Three lines for the "Mounted into the container:" block in usage().
# Callers paste with $(wrapper_mounts_help).
wrapper_mounts_help() {
    echo "  $HOST_SECRETS  ->  /secrets"
    echo "  $HOST_DATA     ->  /data"
    echo "  $HOST_STARTUPCACHE  ->  /cache/startupcache"
    if [[ "${HAS_DEBUG:-0}" == "1" ]]; then
        echo "  $HOST_DEBUG    ->  /debug"
    fi
}

# ----------------------------------------------------------------------
# Subcommand helpers
# ----------------------------------------------------------------------

# Implement `./<wrapper> build [extra args]` — exec docker build and
# pass through any extra args. Caller dispatches on $1 == "build".
# --provenance=false: buildx's default provenance attestation embeds build
# metadata, so the image digest changes on every build even when every layer
# is CACHED — which defeats the cache for anything built FROM this image.
# These images are local-only and never pushed.
wrapper_build() {
    shift  # drop "build"
    exec docker build --provenance=false -t "$IMAGE" "$@" "$HERE"
}

# Implement `./<wrapper> prune [extra args]` host-side — exec the
# collector's prune.py under host python3 with collectorkit on
# PYTHONPATH, defaulting --bronze-dir to the bronze root ($HOST_DATA;
# an explicit --bronze-dir in the extra args overrides it, argparse
# last-wins). Runs on the host, not in a container: the prune engine is
# a pure stdlib file walk that needs no image deps, and running it
# host-side bypasses the single-writer safety guard so it can reclaim
# disk while a download container is mid-flight (prune's own --min-age
# guard protects an in-flight dump). The container entrypoints keep a
# `prune)` arm too, for a direct `docker run`. Caller dispatches on
# $1 == "prune".
#
# With HAS_DEBUG=1 the reclaim extends past bronze to $HOST_DEBUG (the
# /debug mount source), passed as --debug-dir: the screenshots / traces
# written there accumulate outside the bronze tree and nothing else
# reclaims them. It is only forwarded for HAS_DEBUG=1 collectors —
# HOST_DEBUG is unset otherwise (see wrapper_init), and the rest have no
# debug dir to reclaim.
#
# The uniform directory-override flags are honoured here too (the
# dispatcher promises them on every verb): --data-dir retargets the
# bronze root, while --secrets-dir and --silver-db are meaningless to a
# bronze file-walk and are absorbed rather than forwarded (prune.py's
# parser knows only --bronze-dir/--debug-dir/--dry-run/--min-age-hours,
# so a forwarded --silver-db would argparse-reject). Both `--flag VALUE`
# and `--flag=VALUE`; everything else forwards to prune.py.
wrapper_host_prune() {
    shift  # drop "prune"
    local bronze="$HOST_DATA"
    local -a rest=()
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --data-dir)      bronze="$2"; shift 2 ;;
            --data-dir=*)    bronze="${1#*=}"; shift ;;
            --secrets-dir)   shift 2 ;;
            --secrets-dir=*) shift ;;
            --silver-db)     shift 2 ;;
            --silver-db=*)   shift ;;
            *)               rest+=("$1"); shift ;;
        esac
    done
    # Before "$rest" so an explicit --debug-dir in the forwarded args wins
    # (argparse last-wins), matching --bronze-dir's treatment.
    local -a debug_args=()
    if [[ "${HAS_DEBUG:-0}" == "1" ]]; then
        debug_args=(--debug-dir "$HOST_DEBUG")
    fi
    exec env PYTHONPATH="$HERE/../../shared/collectorkit:${PYTHONPATH:-}" \
        python3 "$HERE/prune.py" --bronze-dir "$bronze" \
        ${debug_args[@]+"${debug_args[@]}"} \
        ${rest[@]+"${rest[@]}"}
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
    # 0700 here is authoritative: the container's own chmod of the mounted
    # cache root is rejected by the VM file share (non-owner uid), so
    # collectorkit treats that as best-effort and relies on this.
    mkdir -p "$HOST_STARTUPCACHE"
    chmod 700 "$HOST_STARTUPCACHE"
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
        # The set of subcommands that get a forwarded VNC port. When unset it
        # defaults to just `vnc-login` (the collectors that implement that
        # verb). A wrapper whose VNC surface is different sets VNC_SUBCOMMANDS
        # to REPLACE that default — e.g. VNC_SUBCOMMANDS=(explore login) for a
        # collector whose VNC verbs are explore/login and which has NO
        # vnc-login — so it never allocates a port for (nor advertises) a
        # vnc-login it doesn't implement.
        local -a vnc_set
        if [[ -n "${VNC_SUBCOMMANDS+x}" ]]; then
            vnc_set=(${VNC_SUBCOMMANDS[@]+"${VNC_SUBCOMMANDS[@]}"})
        else
            vnc_set=(vnc-login)
        fi
        for v in ${vnc_set[@]+"${vnc_set[@]}"}; do
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
        # Generic cache mount + env: the browsers relocate their regenerable
        # startupCache here (keyed per profile in-container) instead of into
        # /secrets. One mount covers every browser collector; non-browser
        # ones simply never write to it. The host and container paths never
        # coincide, so the in-profile symlink reads as dangling on the host
        # — a pointer, not data (see collectorkit.launch.redirect_startup_cache).
        -v "$HOST_STARTUPCACHE:/cache/startupcache"
        -e "WEALTHDB_STARTUPCACHE_DIR=/cache/startupcache"
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
