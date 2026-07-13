# shellcheck shell=bash
# Shared Camoufox bootstrap for the base-camoufox collector entrypoints.
#
# Baked into wealthdb/base-camoufox at /opt/entrypoint-lib.sh and SOURCED
# (not executed) from each collector's /app/entrypoint.sh, so every
# camoufox collector shares one copy of the Xvfb + x11vnc + camoufox-cache
# setup instead of pasting it into seven entrypoints. The caller runs under
# `set -euo pipefail`; keep this file source-safe.

VFB_DISPLAY=99

# Symlink the pre-staged camoufox cache (populated at image build time by
# `python3 -m camoufox fetch` and copied to /opt) into the runtime user's
# $HOME/.cache/camoufox so camoufox skips its first-run download. HOME is
# /tmp inside the container (set by the wrapper for the non-root user); we
# always overwrite the symlink so a stale /tmp/.cache/camoufox from a
# previous container with a different runtime uid doesn't shadow it.
mkdir -p /tmp/.cache
if [[ -d /opt/camoufox-cache && ! -e /tmp/.cache/camoufox ]]; then
    ln -snf /opt/camoufox-cache /tmp/.cache/camoufox
fi

# start_xvfb — bring up the virtual X11 display Firefox needs to run headed
# in the container, then export DISPLAY. Polls for the socket (a
# version-independent ready signal); returns non-zero if Xvfb never comes up.
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
    # The wrapper picks the host-side port (it knows which are free); we
    # publish it through VNC_HOST_PORT so the messages below print the
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
