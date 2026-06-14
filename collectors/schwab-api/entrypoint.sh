#!/bin/bash
# Container entrypoint for schwab-api's OAuth login flow.
#
# Only the browser-based `login` runs in the container; `download` and
# `load` run on the host venv (this is a hybrid collector — see the
# wrapper). `login` drives Schwab's OAuth authorize page in a headed
# Camoufox browser on an Xvfb display, and starts x11vnc so the operator
# can complete login / 2FA / consent over VNC. login.py captures the
# `?code=…` redirect and exchanges it for the token bundle. `--check`
# (inspect token) and `--manual` (paste-the-URL) need no browser, so they
# skip Xvfb/VNC.
#
# Xvfb is started directly (not via xvfb-run, whose SIGUSR1 ready-signal
# hangs under a non-root parent); socket-existence polling is the
# version-independent ready signal.

set -euo pipefail

VFB_DISPLAY=99

# Symlink the pre-staged camoufox cache (baked at image-build time) into
# the runtime user's $HOME/.cache so camoufox skips its ~700MB download.
mkdir -p /tmp/.cache
if [[ -d /opt/camoufox-cache && ! -e /tmp/.cache/camoufox ]]; then
    ln -snf /opt/camoufox-cache /tmp/.cache/camoufox
fi

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

# Start x11vnc on the Xvfb display + print the tunnel instructions. Used
# by `vnc-login` (the manual fallback).
start_x11vnc() {
    local pw host_port
    pw=$(openssl rand -hex 8)
    x11vnc -display ":$VFB_DISPLAY" -passwd "$pw" \
        -forever -shared -rfbport 5900 -bg \
        -o /tmp/x11vnc.log >/dev/null 2>&1
    host_port="${VNC_HOST_PORT:-5900}"
    echo "vnc-login: VNC ready on 127.0.0.1:${host_port}" >&2
    echo "vnc-login: password (single-use):  $pw" >&2
    echo "vnc-login: tunnel from your laptop with" >&2
    echo "vnc-login:   ssh -L ${host_port}:127.0.0.1:${host_port} <mbp-host>" >&2
    echo "vnc-login: then on the laptop:" >&2
    echo "vnc-login:   open vnc://localhost:${host_port}" >&2
}

case "${1:-help}" in
    login)
        # Automated (default): auto-submit login, prompt 2FA on stdin,
        # drive the consent pages. Camoufox needs the Xvfb display; no VNC.
        shift
        # --check / --manual / --help are headless; skip the browser.
        for a in "$@"; do
            case "$a" in
                --check|--manual|-h|--help)
                    exec python3 /app/login.py "$@"
                    ;;
            esac
        done
        start_xvfb
        # Trace + capture to /debug by default (later args win).
        exec python3 /app/login.py --screenshot-dir /debug --trace "$@"
        ;;
    vnc-login)
        # Manual fallback: start x11vnc and run --no-cli-mfa so the
        # operator drives login / 2FA / consent over a VNC client (use
        # when the automated `login` selectors drift). Account checkboxes
        # are still auto-ticked.
        shift
        start_xvfb
        start_x11vnc
        exec python3 /app/login.py --no-cli-mfa \
            --screenshot-dir /debug --trace "$@"
        ;;
    sh|bash)
        shift
        exec /bin/bash "$@"
        ;;
    help|--help|-h)
        cat <<'EOF'
schwab-api login container

Usage:
  <wrapper> {login|vnc-login} [args...]

  login          Automated OAuth refresh in headed Camoufox: pre-fills the
                 Schwab login from SCHWAB_LOGIN_ID / SCHWAB_PASSWORD
                 (schwab-web.env), auto-submits, prompts for the 2FA code
                 on stdin, drives the consent / account-link pages (ticking
                 every account), captures the redirect, writes the token to
                 /secrets. Traces to /debug. Stdin must be a TTY.
  login --check  Inspect the stored token's age (no browser).
  login --manual Print the auth URL, paste the redirected URL back.
  vnc-login      Manual fallback: drive login / 2FA / consent yourself over
                 VNC (starts x11vnc, prints the tunnel). Use if `login`'s
                 selectors drift.
  sh|bash        Open an interactive shell inside the container.
  help           Show this message.
EOF
        ;;
    *)
        exec "$@"
        ;;
esac
