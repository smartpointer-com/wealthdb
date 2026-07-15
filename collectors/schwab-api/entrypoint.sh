#!/bin/bash
# Container entrypoint for schwab-api's OAuth login flow.
#
# Only the browser-based `login` runs in the container; `download` and
# `load` run on the host venv (this is a hybrid collector — see the
# wrapper). `login` drives Schwab's OAuth authorize page in a headed
# Camoufox browser on an Xvfb display, and starts x11vnc so login / 2FA /
# consent can be completed over VNC. login.py captures the
# `?code=…` redirect and exchanges it for the token bundle. `--check`
# (inspect token) and `--manual` (paste-the-URL) need no browser, so they
# skip Xvfb/VNC.
#
# Xvfb is started directly (not via xvfb-run, whose SIGUSR1 ready-signal
# hangs under a non-root parent); socket-existence polling is the
# version-independent ready signal.

set -euo pipefail

# Xvfb / x11vnc / camoufox-cache bootstrap (VFB_DISPLAY, start_xvfb,
# start_x11vnc) is shared across the camoufox collectors, baked into
# base-camoufox at /opt/entrypoint-lib.sh.
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

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
        # Manual fallback: start x11vnc and run --no-cli-mfa so login /
        # 2FA / consent are driven by hand over a VNC client (use when
        # the automated `login` selectors drift). Account checkboxes
        # are still auto-ticked.
        shift
        start_xvfb
        start_x11vnc vnc-login
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
