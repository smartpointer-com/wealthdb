# shellcheck shell=bash
# Shared dispatch and browser bootstrap for the containerised collector
# entrypoints.
#
# Baked into every base image at /opt/entrypoint-lib.sh and SOURCED (not
# executed) from each collector's /app/entrypoint.sh, so every collector
# shares one copy of the verb dispatch and the Xvfb + x11vnc +
# browser-cache setup. An entrypoint is then one line per verb:
#
#   verb="${1:-help}"; [[ $# -gt 0 ]] && shift
#   case "$verb" in
#       download)      run xvfb login.py --cli-mfa --bronze-dir /data -- "$@" ;;
#       load)          run plain load.py --bronze-dir /data -- "$@" ;;
#       prune|sh|bash) entrypoint_common "$verb" "$@" ;;
#       help|--help|-h) cat <<'EOF' ... EOF ;;
#       *)             exec "$verb" "$@" ;;
#   esac
#
# The pass-through stays in each entrypoint, so bash names that file
# when the command is not found. The caller runs under
# `set -euo pipefail`; keep this file source-safe.

VFB_DISPLAY=99

# Symlink the pre-staged camoufox cache (fetched at image build time by
# fetch-camoufox.py and copied to /opt) into the runtime user's
# $HOME/.cache/camoufox so camoufox skips its first-run download. HOME is
# /tmp inside the container (set by the wrapper for the non-root user). The
# symlink is always replaced, so a stale /tmp/.cache/camoufox from a
# previous container with a different runtime uid doesn't shadow it.
mkdir -p /tmp/.cache
if [[ -d /opt/camoufox-cache ]]; then
    # rm first: `ln -snf` into a stale real directory would nest the
    # link inside it instead of replacing it. Removing a symlink here
    # never touches the /opt target.
    rm -rf /tmp/.cache/camoufox
    ln -sn /opt/camoufox-cache /tmp/.cache/camoufox
fi

# start_xvfb — bring up the virtual X11 display a headed browser needs in
# the container, then export DISPLAY. Polls for the socket (a
# version-independent ready signal); returns non-zero if Xvfb never comes up.
#
# Xvfb is started directly, not via `xvfb-run`. The Ubuntu Noble
# xvfb-run script's SIGUSR1 ready-signalling hangs when the parent shell
# is non-root: it sets the parent's SIGUSR1 handler to `:` rather than
# SIG_IGN, so Xvfb never sends the ready signal and the parent waits
# forever.
start_xvfb() {
    Xvfb ":$VFB_DISPLAY" -screen 0 1280x800x24 -nolisten tcp \
        >/tmp/xvfb.log 2>&1 &
    for _ in $(seq 1 50); do
        if [[ -S "/tmp/.X11-unix/X$VFB_DISPLAY" ]]; then
            export DISPLAY=":$VFB_DISPLAY"
            return 0
        fi
        sleep 0.1
    done
    echo "entrypoint: Xvfb failed to start within 5s; /tmp/xvfb.log:" >&2
    tail -20 /tmp/xvfb.log >&2 || true
    return 1
}

# exec_if_help <script> [args…] — exec /app/<script> with the args when one
# of them asks for help, so reading a verb's flags starts no display and
# announces no single-use VNC password. Returns otherwise.
exec_if_help() {
    local script="$1"
    shift
    local a
    for a in "$@"; do
        case "$a" in
            --help|-h) exec python3 "/app/$script" "$@" ;;
        esac
    done
}

# start_x11vnc <label> — serve the Xvfb display over VNC and print the tunnel
# instructions prefixed with <label> (the subcommand: explore / login /
# vnc-login).
start_x11vnc() {
    # Single-use password. openssl rand -hex 8 is a single command, so
    # `set -euo pipefail` does not trip on SIGPIPE the way a piped
    # `tr -dc ... | head -c 16` would.
    VNC_PASSWORD=$(openssl rand -hex 8)
    x11vnc -display ":$VFB_DISPLAY" -passwd "$VNC_PASSWORD" \
        -forever -shared -rfbport 5900 -bg \
        -o /tmp/x11vnc.log >/dev/null 2>&1
    # The wrapper picks the host-side port (it knows which are free) and
    # passes it as VNC_HOST_PORT, so the messages below print the
    # real port to tunnel through. Defaults to 5900 for direct
    # `docker run` invocations that skip the wrapper.
    local label="$1"
    local host_port="${VNC_HOST_PORT:-5900}"
    echo "$label: VNC ready on 127.0.0.1:${host_port}" >&2
    echo "$label: password (single-use):  $VNC_PASSWORD" >&2
    echo "$label: tunnel from your laptop with" >&2
    echo "$label:   ssh -L ${host_port}:127.0.0.1:${host_port} <host>" >&2
    echo "$label: then on the laptop:" >&2
    echo "$label:   open vnc://localhost:${host_port}" >&2
}

# run DISPLAY SCRIPT [FIXED...] -- [ARGS...] — exec /app/SCRIPT with the
# FIXED flags, then ARGS. DISPLAY names what the verb needs first:
#   plain        nothing
#   xvfb         the virtual display (start_xvfb)
#   vnc:LABEL    the virtual display, served over VNC (start_x11vnc LABEL)
# A display verb asked for help execs SCRIPT with ARGS alone and starts
# nothing (exec_if_help).
run() {
    local display="$1" script="$2"
    shift 2
    local -a fixed=()
    while [[ "$1" != -- ]]; do
        fixed+=("$1")
        shift
    done
    shift
    case "$display" in
        plain) ;;
        xvfb)
            exec_if_help "$script" "$@"
            start_xvfb
            ;;
        vnc:*)
            exec_if_help "$script" "$@"
            start_xvfb
            start_x11vnc "${display#vnc:}"
            ;;
        *)
            echo "entrypoint: unknown display '$display'" >&2
            exit 2
            ;;
    esac
    exec python3 "/app/$script" ${fixed[@]+"${fixed[@]}"} "$@"
}

# login_folds DISPLAY SCRIPT -- [ARGS...] — the session probe in the
# `login` of a collector whose sign-in folds into `download`. A help flag
# among ARGS execs SCRIPT, and so does `--check`, after starting DISPLAY
# (as `run` does). Returns otherwise, and the caller prints its notice.
# The notice is never this call's stdin, so SCRIPT reads the
# entrypoint's own.
login_folds() {
    local display="$1" script="$2"
    shift 3
    exec_if_help "$script" "$@"
    local a
    for a in "$@"; do
        if [[ "$a" == --check ]]; then
            run "$display" "$script" -- "$@"
        fi
    done
}

# entrypoint_common VERB [ARGS...] — the verbs every image answers
# alike: `prune`, the bronze reclaim (usually run host-side by the
# wrapper), and `sh` / `bash`, an interactive shell.
entrypoint_common() {
    local verb="$1"
    shift
    case "$verb" in
        prune)   exec python3 /app/prune.py "$@" ;;
        sh|bash) exec /bin/bash "$@" ;;
    esac
    echo "entrypoint: '$verb' is not a common verb" >&2
    exit 2
}
