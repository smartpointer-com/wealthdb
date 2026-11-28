# Shared functions for the per-collector `docker run` wrappers.
#
# Each collector wrapper is a thin file that:
#   1. sets a small config block (NAME, ENV_PREFIX, ENV_VARS, optional
#      feature flags),
#   2. sources this file,
#   3. prints its help text when wrapper_wants_help, and handles its own
#      special subcommands inline (pure-host-side shortcuts like
#      fidelity-web's `load`),
#   4. calls `wrapper_main "$@"`, which dispatches build and prune and
#      runs every other verb on the common docker-run path.
#
# Every verb mounts $HERE over /app, so a run uses the checkout's source
# and a code edit needs no rebuild. The image holds no source: its
# Dockerfile copies requirements.txt alone, and a direct `docker run`
# mounts the checkout too.
#
# Caller must SET (before sourcing):
#   HERE         absolute path to the wrapper's own directory. Convention:
#                HERE="$(cd "$(dirname "$0")" && pwd)" at the top of each
#                wrapper.
#   NAME         short collector name, e.g. "fidelity-web".
#   ENV_VARS     Bash array of env-var names to forward into the container
#                via `-e VAR` (e.g. (FIDELITY_USERNAME FIDELITY_PASSWORD)).
#
# Caller MAY SET (before sourcing):
#   ENV_PREFIX   upper-cased env-var prefix. Used to derive both override
#                knobs (${ENV_PREFIX}_SECRETS_DIR etc.) and the
#                force-replace switch (${ENV_PREFIX}_FORCE_REPLACE).
#                wrapper_init derives it from NAME when unset (see
#                paths-lib.sh): "fidelity-web" gives "FIDELITY_WEB".
#
# Caller MAY SET (before sourcing) to opt in to features:
#   HAS_DEBUG=1       Mount $HOST_DEBUG → /debug. Required for collectors
#                     whose --screenshot-dir / --trace flags write under
#                     /debug.
#   HAS_SAFETY=1      Refuse to clobber a RUNNING container with the same
#                     name. Required where a docker rm -f mid-MFA would
#                     burn a 2FA prompt or evict a VNC handoff. Override
#                     with ${ENV_PREFIX}_FORCE_REPLACE=1 when the live
#                     container is known-stuck.
#   VNC_SUBCOMMANDS   Bash array of the subcommands that get a forwarded
#                     host-side VNC port. Walks 5900-6000 for a free port
#                     (defends against macOS Screen Sharing, prior crashed
#                     runs, other VNC servers) and passes the chosen port
#                     as VNC_HOST_PORT inside the container. Unset or
#                     empty, no verb gets one.
#   SILVER_SUBCOMMANDS
#                     Bash array of the subcommands that read --silver-db.
#                     Replaces the default (load); see paths-lib.sh.
#   HAS_ENV_FILE=1    Source the credentials env file host-side before
#                     `docker run`, so the ENV_VARS it sets are forwarded
#                     (see wrapper_source_env_file).
#
# A wrapper with a config file calls `wrapper_source_cfg PREFIX DEFAULT`
# before wrapper_init. A wrapper whose `login` folds into `download`
# calls `wrapper_login_folds` before wrapper_main. Both wrapper_main and
# wrapper_login_folds run the setup check of $HERE/setup.txt
# (setup-lib.sh) before a verb that signs in.
#
# wrapper_init POPULATES (for the caller and the helpers below):
#   HOST_SECRETS, HOST_DATA, HOST_DEBUG (when HAS_DEBUG=1),
#   HOST_STARTUPCACHE, HOST_CACHE, CONTAINER_NAME, IMAGE.
#
# bash 3.2 compatibility: this lib avoids namerefs and associative
# arrays so it runs on Apple's /bin/bash. Indirect expansion (${!var})
# is used to read the per-prefix override variables.

# ----------------------------------------------------------------------
# Internals
# ----------------------------------------------------------------------

# The repository root, from this file's place in it, and the image-tag,
# env-file and path helpers beside this file.
_WRAPPER_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/image-tag.sh"
# shellcheck disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/engine-mode.sh"
# shellcheck disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/envfile-lib.sh"
# shellcheck disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/paths-lib.sh"
# shellcheck disable=SC1091
. "$(dirname "${BASH_SOURCE[0]}")/setup-lib.sh"

# _abs_dir DIR — echo the existing directory DIR as an absolute path.
# `docker run -v` reads a bare relative source such as `mydata` as a
# named volume, not a host path, so every mount source goes through this.
_abs_dir() {
    (CDPATH='' cd -- "$1" && pwd)
}

# ----------------------------------------------------------------------
# Initialisation + help-text helpers
# ----------------------------------------------------------------------

# wrapper_source_cfg PREFIX DEFAULT — source the collector's optional
# config file: ${PREFIX}_CFG when set, else DEFAULT, which is left in
# ${PREFIX}_CFG for the help text. Called before wrapper_init, so the
# file can set the per-prefix knobs (${PREFIX}_DATA_DIR etc.) as well as
# settings a verb reads. The file is bash: it is checked with `bash -n`,
# then sourced with every assignment exported, so a plain KEY=VALUE line
# reaches `docker run -e`. A missing file holds no settings; one that
# does not parse stops the wrapper.
wrapper_source_cfg() {
    local var="$1_CFG"
    local cfg="${!var:-$2}"
    printf -v "$var" '%s' "$cfg"
    [[ -f "$cfg" ]] || return 0
    if ! wealthdb_source_bash "$cfg"; then
        # A config file holds no secret, so bash may say where it fails.
        bash -n "$cfg"
        echo "${NAME}: config file $cfg has a bash syntax error" >&2
        exit 2
    fi
}

# The mount sources follow the precedence of paths-lib.sh. A
# --secrets-dir / --data-dir / --silver-db flag is parsed later, in
# wrapper_resolve_dir_args, so it overrides everything here.
wrapper_init() {
    collector_default_paths
    wealthdb_export_image_tag "$_WRAPPER_ROOT" || exit 2
    IMAGE="$(_collector_envvar IMAGE "wealthdb/${NAME}:${WEALTHDB_IMAGE_TAG}")"
    CONTAINER_NAME="$(_collector_envvar CONTAINER "$NAME")"

    HOST_SECRETS="$SECRETS_DIR"
    HOST_DATA="$DATA_DIR"
    if [[ "${HAS_DEBUG:-0}" == "1" ]]; then
        # Debug/trace cache under the XDG cache dir, namespaced per
        # collector like the startupCache below.
        HOST_DEBUG="$DEBUG_DIR"
    fi

    # Host cache dir for the browsers' relocated startupCache (see
    # collectorkit.launch). Mounted into every container so Firefox's
    # regenerable compiled-bytecode cache lands here rather than next to the
    # session cookie under /secrets. XDG cache path so it survives reboots
    # (keeping launches fast) yet is plainly non-sensitive; one generic dir,
    # not per-profile — the container keys per-profile subdirs itself.
    HOST_STARTUPCACHE="${XDG_CACHE_HOME:-$HOME/.cache}/wealthdb/startupcache"

    # The collector's own cache dir (collectorkit.extract.cache_root): the
    # parse and extraction caches a load replays. The container runs with
    # HOME=/tmp, so without this mount the cache would end with the
    # container.
    HOST_CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/wealthdb/$NAME"
}

# wrapper_resolve_dir_args ARGS... — consume the uniform directory-override
# flags (collector_parse_dir_flags) and map them onto the mounts:
#   --secrets-dir DIR   overrides HOST_SECRETS (the /secrets mount source)
#   --data-dir    DIR   overrides HOST_DATA    (the /data mount source —
#                       holds both the bronze dumps and the silver DB)
#   --silver-db   PATH  puts the silver DB at an explicit HOST path outside
#                       the data dir: bind-mounts its parent to /silver and
#                       rewrites the flag to the in-container path (the inner
#                       load.py accepts --silver-db, default /data/<name>.db)
# Everything else is left in FORWARD_ARGS for the container. SILVER_MOUNT
# holds the extra `-v` args (empty unless a silver path is in play), and
# only a verb that reads the silver DB gets one.
wrapper_resolve_dir_args() {
    SECRETS_DIR="$HOST_SECRETS"
    DATA_DIR="$HOST_DATA"
    collector_parse_dir_flags "$@"
    HOST_SECRETS="$SECRETS_DIR"
    HOST_DATA="$DATA_DIR"
    SILVER_MOUNT=()
    collector_verb_takes_silver "${FORWARD_ARGS[0]:-}" || return 0
    if [[ -n "$SILVER_DB" ]]; then
        local d b
        mkdir -p "$(dirname "$SILVER_DB")"
        d="$(_abs_dir "$(dirname "$SILVER_DB")")"
        b="$(basename "$SILVER_DB")"
        SILVER_MOUNT=(-v "$d:/silver")
        FORWARD_ARGS+=(--silver-db "/silver/$b")
    fi
}

# wrapper_source_env_file — under HAS_ENV_FILE=1, source the collector's
# credentials env file host-side, so the ENV_VARS it sets reach
# `docker run -e`. The file is ${ENV_PREFIX}_ENV_FILE when set, else
# <secrets-dir>/<name>.env; wrapper_main calls this after
# wrapper_resolve_dir_args, so a --secrets-dir flag names the dir. The
# file is bash: it is checked with `bash -n`, then sourced with every
# assignment exported, so a plain KEY=VALUE line is forwarded too. A
# missing file is fine. One that does not parse is skipped with a
# warning, and its error output is dropped, since it may quote a secret.
wrapper_source_env_file() {
    [[ "${HAS_ENV_FILE:-0}" == "1" ]] || return 0
    local f
    f="$(_collector_envvar ENV_FILE "$HOST_SECRETS/$NAME.env")"
    [[ -f "$f" ]] || return 0
    wealthdb_source_bash "$f" ||
        echo "${NAME}: env file $f has a bash syntax error; not sourcing it" >&2
}

# The mount lines for the "Mounted into the container:" block in usage().
# Callers paste with $(wrapper_mounts_help).
wrapper_mounts_help() {
    echo "  $HOST_SECRETS  ->  /secrets"
    echo "  $HOST_DATA     ->  /data"
    echo "  $HOST_STARTUPCACHE  ->  /cache/startupcache"
    echo "  $HOST_CACHE  ->  /cache/collector"
    if [[ "${HAS_DEBUG:-0}" == "1" ]]; then
        echo "  $HOST_DEBUG    ->  /debug"
    fi
    echo "  $HERE  ->  /app"
}

# ----------------------------------------------------------------------
# Subcommand helpers
# ----------------------------------------------------------------------

# _wrapper_build_hint — print the sentence that says how this
# collector's image and its base images are built. `make build-<name>`
# builds the bases first, and only a checkout has the Makefile and their
# Dockerfiles.
_wrapper_build_hint() {
    echo "In a checkout, run: make build-$NAME"
}

# _wrapper_installed_copy — is this tree an installed copy? It has no
# Makefile, holds no image and builds none.
_wrapper_installed_copy() {
    [[ ! -f "$_WRAPPER_ROOT/Makefile" ]]
}

# _wrapper_stop_installed — stop, with status 2, a verb that needs an
# image an installed copy does not have. A missing image, a Docker that
# does not answer and no Docker at all end in this one message there:
# none of them can be mended in an installed copy.
_wrapper_stop_installed() {
    echo "$NAME: this collector runs in a container, and an installed copy has no image for it yet. Run it from a checkout of wealthdb." >&2
    exit 2
}

# _wrapper_require_docker — stop, with status 2, when the answer of
# wealthdb_image_state says there is no Docker to run or build with: no
# docker program, or one that does not answer. Docker's own first line
# follows the message, so the cause is not lost. Any other answer goes
# on.
_wrapper_require_docker() {
    case "$IMAGE_STATE" in
        noclient)
            echo "$NAME: this collector runs in a container, and Docker is not installed." >&2
            ;;
        down)
            echo "$NAME: Docker does not answer. Start it, then run this again. With Colima: colima start" >&2
            if [[ -n "$DOCKER_ERROR" ]]; then echo "    $DOCKER_ERROR" >&2; fi
            ;;
        *) return 0 ;;
    esac
    exit 2
}

# wrapper_require_image — stop the verb, with status 2 and one message,
# unless Docker holds the collector's image. It runs before anything
# else of a verb that needs a container, and asks Docker once
# (wealthdb_image_state): a run that goes on asks nothing more. A run
# passes --pull=never as well, so a missing image is never fetched from
# a registry, where the name may belong to anyone.
wrapper_require_image() {
    wealthdb_image_state "$IMAGE"
    [[ "$IMAGE_STATE" == present ]] && return 0
    if _wrapper_installed_copy; then _wrapper_stop_installed; fi
    _wrapper_require_docker
    echo "$NAME: the image $IMAGE is not built yet. $(_wrapper_build_hint)" >&2
    exit 2
}

# Implement `./<wrapper> build [extra args]`, the step `make build-<name>`
# runs once the base images are built. A Dockerfile FROM a wealthdb/
# base that is not local stops the build with status 2
# (wealthdb_require_local_bases), so no registry is asked for it. When
# the image's label matches a hash of its inputs and of its base image,
# print that it is unchanged and exit. The inputs are the Dockerfile and
# requirements.txt alone: the image holds no source, so an edit to the
# code or the docs leaves it as it is. Returning would fall through into
# docker run. Extra arguments are passed through, and the image is
# built. A hash that fails (no python3 on the host) builds an unlabeled
# image, which is never skipped.
# wrapper_main dispatches $1 == "build" here.
# --provenance=false: buildx's default provenance attestation embeds build
# metadata, so the image digest changes on every build even when every layer
# is CACHED — which defeats the cache for anything built FROM this image.
# These images are local-only and never pushed. WEALTHDB_IMAGE_TAG names
# the shared base the Dockerfile builds FROM, so a checkout with its own
# tag builds on its own bases.
wrapper_build() {
    shift  # drop "build"
    if _wrapper_installed_copy; then _wrapper_stop_installed; fi
    # The base image's id is part of the inputs: a rebuilt base is a
    # different image even when its own inputs did not change. The
    # question that reads it also tells whether Docker answers at all.
    _wfrom="$(wealthdb_from_ref "$HERE/Dockerfile")"
    if [[ -n "$_wfrom" && "$_wfrom" != missing ]]; then
        wealthdb_image_state "$_wfrom"
        _wrapper_require_docker
        _wfrom="${IMAGE_ID:-missing}"
    else
        _wfrom=missing
    fi
    wealthdb_require_local_bases "$NAME" "$HERE/Dockerfile" "$(_wrapper_build_hint)" || exit 2
    _whash="$(wealthdb_inputs_hash "$HERE/Dockerfile" "$HERE/requirements.txt" \
        -- "$_wfrom")" || _whash=
    if wealthdb_build_unchanged "$IMAGE" "$_whash" "$#"; then
        exit 0
    fi
    exec docker build --provenance=false \
        ${_whash:+--label "wealthdb.inputs=$_whash"} \
        --build-arg WEALTHDB_IMAGE_TAG="$WEALTHDB_IMAGE_TAG" \
        -t "$IMAGE" "$@" "$HERE"
}

# Implement `./<wrapper> prune [extra args]` host-side — exec the
# collector's prune.py under the host Python (the one ./configure
# recorded, else python3 on PATH) with collectorkit on
# PYTHONPATH, defaulting --bronze-dir to the bronze root ($HOST_DATA;
# an explicit --bronze-dir in the extra args overrides it, argparse
# last-wins). Runs on the host, not in a container: the prune engine is
# a pure stdlib file walk that needs no image deps, and running it
# host-side bypasses the single-writer safety guard so it can reclaim
# disk while a download container is mid-flight (prune's own --min-age
# guard protects an in-flight dump). The container entrypoints keep a
# `prune)` arm too, for a direct `docker run`. wrapper_main dispatches
# $1 == "prune" here; a wrapper with a prune of its own handles it
# before calling wrapper_main.
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
# bronze root, and --secrets-dir, meaningless to a bronze file-walk, is
# absorbed rather than forwarded. --silver-db is rejected, as on every
# other verb that does not read it. Everything else forwards to prune.py.
wrapper_host_prune() {
    DATA_DIR="$HOST_DATA"
    collector_parse_dir_flags "$@"
    local -a rest=("${FORWARD_ARGS[@]:1}")
    # Before "$rest" so an explicit --debug-dir in the forwarded args wins
    # (argparse last-wins), matching --bronze-dir's treatment.
    local -a debug_args=()
    if [[ "${HAS_DEBUG:-0}" == "1" ]]; then
        debug_args=(--debug-dir "$HOST_DEBUG")
    fi
    exec env PYTHONPATH="$HERE/../../shared/collectorkit:${PYTHONPATH:-}" \
        "$(wealthdb_tool "$_WRAPPER_ROOT" PYTHON python3)" "$HERE/prune.py" --bronze-dir "$DATA_DIR" \
        ${debug_args[@]+"${debug_args[@]}"} \
        ${rest[@]+"${rest[@]}"}
}

# ----------------------------------------------------------------------
# Pre-run sanity checks + docker-arg construction
# ----------------------------------------------------------------------

# Make sure the mount-point dirs exist on the host (docker would
# otherwise silently create them as root-owned), then make each mount
# source absolute (see _abs_dir). Errors if the secrets dir is missing —
# that's a configuration issue, not something the wrapper should paper
# over.
wrapper_check_mounts() {
    mkdir -p "$HOST_DATA"
    # Per-collector, not the shared root: the tree below holds the
    # source's own financial record, and the container runs as this uid.
    chmod 700 "$HOST_DATA"
    HOST_DATA="$(_abs_dir "$HOST_DATA")"
    # 0700 here is authoritative: the container's own chmod of the mounted
    # cache root is rejected by the VM file share (non-owner uid), so
    # collectorkit treats that as best-effort and relies on this.
    mkdir -p "$HOST_STARTUPCACHE"
    chmod 700 "$HOST_STARTUPCACHE"
    HOST_STARTUPCACHE="$(_abs_dir "$HOST_STARTUPCACHE")"
    # The collector cache holds extracted statement text and parsed rows,
    # so it is kept 0700 like the data dir, whoever created it.
    mkdir -p "$HOST_CACHE"
    chmod 700 "$HOST_CACHE"
    HOST_CACHE="$(_abs_dir "$HOST_CACHE")"
    if [[ "${HAS_DEBUG:-0}" == "1" ]]; then
        mkdir -p "$HOST_DEBUG"
        HOST_DEBUG="$(_abs_dir "$HOST_DEBUG")"
    fi
    if [[ ! -d "$HOST_SECRETS" ]]; then
        echo "${NAME}: secrets dir does not exist: $HOST_SECRETS" >&2
        echo "Create it with: mkdir -p $HOST_SECRETS && chmod 700 $HOST_SECRETS" >&2
        exit 1
    fi
    HOST_SECRETS="$(_abs_dir "$HOST_SECRETS")"
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
# crashed container whose `docker run -p` lingers); the walk moves +1 until
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

# wrapper_login_folds [PASS_FLAG...] -- ARGS... — is this the bare
# `login` of a collector whose sign-in folds into `download`? True when
# ARGS name the `login` verb and hold none of the PASS_FLAGs. The caller
# then prints its notice and exits 0 without starting a container. That
# keeps an orchestrator's login -> download -> load from tripping and
# saves a container start. A PASS_FLAG, such as a session probe's
# `--check` or a help flag, takes `login` to the container. Before it
# returns true, the setup check runs (setup_check): a missing credential
# prints what to do and exits SETUP_EXIT in place of the notice.
# A `--check` that is no PASS_FLAG probes a collector with no session to
# probe: every download signs in. It exits 6, a person has to sign in
# (collectorkit.cli), and never 0, which an orchestrator would read as
# leave to run the download with no one there (wealthdb refresh).
wrapper_login_folds() {
    local -a pass=()
    while [[ $# -gt 0 && "$1" != -- ]]; do
        pass+=("$1")
        shift
    done
    shift
    [[ "${1:-}" == login ]] || return 1
    local a p
    for a in "$@"; do
        for p in ${pass[@]+"${pass[@]}"}; do
            [[ "$a" == "$p" ]] && return 1
        done
    done
    setup_check docker "$(setup_secrets_dir "$HOST_SECRETS" "$@")" "$@"
    for a in "$@"; do
        if [[ "$a" == --check ]]; then
            echo "${NAME}: no session to check: its download signs in each time it runs. Run: ${WEALTHDB_COLLECT:-wealthdb-collect} ${NAME} download" >&2
            exit 6
        fi
    done
    setup_arm_next "$@"
    return 0
}

# wrapper_wants_help [args...] — are the args a request for the
# wrapper's help: none at all, or help / --help / -h as the verb?
wrapper_wants_help() {
    [[ $# -eq 0 || "$1" == help || "$1" == --help || "$1" == -h ]]
}

# The common dispatch. Caller has already printed its help and handled
# its own special subcommands (any host-side shortcuts) before falling
# through here. build and prune go to wrapper_build and
# wrapper_host_prune, which exit or exec. Every other verb takes the
# docker-run path: compose the docker argv from the active features and
# exec it.
wrapper_main() {
    case "${1:-}" in
        build) wrapper_build "$@" ;;
        prune) wrapper_host_prune "$@" ;;
    esac
    # First of all: no container runs without the image, and a verb
    # that cannot run makes no folder and asks for no credential.
    wrapper_require_image
    # Pull the --secrets-dir/--data-dir/--silver-db flags out of the args
    # (they may override HOST_SECRETS/HOST_DATA + add the /silver mount),
    # then continue with the remaining args as the positional parameters.
    wrapper_resolve_dir_args "$@"
    set -- ${FORWARD_ARGS[@]+"${FORWARD_ARGS[@]}"}
    # Before a verb that signs in: stop with instructions when a
    # credential the collector's setup.txt declares is missing.
    setup_check docker "$HOST_SECRETS" "$@"
    setup_arm_next "$@"
    wrapper_source_env_file

    wrapper_check_mounts
    wrapper_tty_args

    # A verb of VNC_SUBCOMMANDS gets a forwarded VNC port.
    local -a extra_args=()
    local v
    for v in ${VNC_SUBCOMMANDS[@]+"${VNC_SUBCOMMANDS[@]}"}; do
        if [[ "${1:-}" == "$v" ]]; then
            wrapper_vnc_port
            extra_args+=(-p "127.0.0.1:${vnc_host_port}:5900"
                         -e "VNC_HOST_PORT=${vnc_host_port}")
            break
        fi
    done

    if [[ "${HAS_SAFETY:-0}" == "1" ]]; then
        wrapper_safety_guard "$@"
    fi

    local -a docker_args=(
        --rm --pull=never --name "$CONTAINER_NAME"
        ${TTY_ARGS[@]+"${TTY_ARGS[@]}"}
        ${extra_args[@]+"${extra_args[@]}"}
        --user "$(id -u):$(id -g)"
        -e HOME=/tmp
    )
    for v in ${ENV_VARS[@]+"${ENV_VARS[@]}"}; do
        docker_args+=(-e "$v")
    done
    # The words a command the collector prints starts with, which the
    # engine launcher sets for a setup of its own. Unset, nothing is
    # passed, and the collector prints `wealthdb-collect` and `wealthdb`.
    for v in WEALTHDB_COLLECT WEALTHDB_PROGRAM; do
        if [[ -n "${!v:-}" ]]; then docker_args+=(-e "$v"); fi
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
        # The collector's cache root, kept on the host across runs.
        -v "$HOST_CACHE:/cache/collector"
        -e "WEALTHDB_CACHE_DIR=/cache/collector"
        ${SILVER_MOUNT[@]+"${SILVER_MOUNT[@]}"}
    )
    if [[ "${HAS_DEBUG:-0}" == "1" ]]; then
        docker_args+=(-v "$HOST_DEBUG:/debug")
    fi
    docker_args+=(-v "$HERE:/app")

    wrapper_silver_lock "$@"
    setup_exec ${SILVER_LOCK[@]+"${SILVER_LOCK[@]}"} docker run "${docker_args[@]}" "$IMAGE" "$@"
}

# wrapper_silver_lock VERB... sets SILVER_LOCK, the command the container
# runs through when the verb writes the collector's database in a tree
# of the native engine: the engine's hold-lock, on the lock file beside
# that database. The native engine reads the database on this computer,
# and SQLite's own locks do not reach it from Docker's virtual machine,
# so the two meet on this lock instead (silverLockName in the engine).
# Empty for any other verb, and in a tree of the container engine. A
# config.mk that names no kind of engine stops the wrapper with status
# 2, as it stops the launcher: the verb never runs unlocked.
wrapper_silver_lock() {
    SILVER_LOCK=()
    collector_verb_takes_silver "${1:-}" || return 0
    local mode
    mode="$(wealthdb_engine_mode "$_WRAPPER_ROOT")" || exit 2
    [[ "$mode" == native ]] || return 0
    local engine dir="$HOST_DATA"
    engine="$(wealthdb_native_engine "$_WRAPPER_ROOT")"
    # No engine built yet: no load can read the database either.
    [[ -x "$engine" ]] || return 0
    if [[ -n "${SILVER_DB:-}" ]]; then dir="$(_abs_dir "$(dirname "$SILVER_DB")")"; fi
    SILVER_LOCK=("$engine" hold-lock "$dir/.wealthdb-silver.lock" --)
}
