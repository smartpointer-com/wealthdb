#!/bin/bash
# Container entrypoint for schwab-api's OAuth login flow.
#
# Only the browser-based `login` runs in the container; `download` and
# `load` run on the host venv (this is a hybrid collector — see the
# wrapper). `login` drives Schwab's OAuth authorize page in a headed
# Camoufox browser on an Xvfb display. `vnc-login` also starts x11vnc, so
# login / 2FA / consent can be completed by hand over VNC. login.py
# captures the `?code=…` redirect and exchanges it for the token bundle.
# `--check` (probe the token) and `--manual` (paste-the-URL) need no
# browser, so they skip Xvfb/VNC.

set -euo pipefail

# The verb dispatch (run, login_folds, entrypoint_common) and the Xvfb /
# x11vnc / camoufox-cache bootstrap, baked into the base image at
# /opt/entrypoint-lib.sh.
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

verb="${1:-help}"
[[ $# -gt 0 ]] && shift

case "$verb" in
    login)
        # `--check` (probe the token) and `--manual` (paste the URL) need
        # no browser.
        for a in "$@"; do
            case "$a" in
                --check|--manual) run plain login.py -- "$@" ;;
            esac
        done
        run xvfb login.py -- "$@"
        ;;
    vnc-login) run vnc:vnc-login login.py --no-cli-mfa -- "$@" ;;
    # `prune` runs on the host venv; of the common verbs the image
    # answers only the shell.
    sh|bash) entrypoint_common "$verb" "$@" ;;
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
    *) exec "$verb" "$@" ;;
esac
