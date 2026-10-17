#!/bin/bash
# Container entrypoint for schwab-api's OAuth login flow.
#
# Only the browser-based `login` runs in the container; `download` and
# `load` run on the host venv (this is a hybrid collector — see the
# wrapper). `login` drives Schwab's OAuth authorize page in a headed
# Camoufox browser on an Xvfb display, and starts x11vnc so login / 2FA /
# consent can be completed over VNC. login.py captures the
# `?code=…` redirect and exchanges it for the token bundle. `--check`
# (probe the token) and `--manual` (paste-the-URL) need no browser, so they
# skip Xvfb/VNC.

set -euo pipefail

# Xvfb / x11vnc / camoufox-cache bootstrap (VFB_DISPLAY, start_xvfb,
# start_x11vnc), shared by the browser collectors and baked into
# base-playwright at /opt/entrypoint-lib.sh.
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
        # Diagnostics are opt-in (fleet convention), not injected here.
        # Pass --screenshot-dir /debug through the wrapper to capture
        # pages; --trace adds a Playwright trace, but the base image's
        # Firefox crashes on tracing.start(), which ends the login.
        exec python3 /app/login.py "$@"
        ;;
    vnc-login)
        # Manual fallback: start x11vnc and run --no-cli-mfa so login /
        # 2FA / consent are driven by hand over a VNC client (use when
        # the automated `login` selectors drift). Account checkboxes
        # are still auto-ticked.
        shift
        exec_if_help login.py --no-cli-mfa "$@"
        start_xvfb
        start_x11vnc vnc-login
        exec python3 /app/login.py --no-cli-mfa "$@"
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
                 /secrets. Stdin must be a TTY. Add --screenshot-dir /debug
                 for page captures (off by default).
  login --check  Show the stored token's age and prove it works (no
                 browser, no 2FA).
  login --manual Print the auth URL, paste the redirected URL back.
  vnc-login      Manual fallback: login / 2FA / consent are driven by hand
                 over VNC (starts x11vnc, prints the tunnel). Use if
                 `login`'s selectors drift.
  sh|bash        Open an interactive shell inside the container.
  help           Show this message.
EOF
        ;;
    *)
        exec "$@"
        ;;
esac
