#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand to the
# corresponding Python under /app/.
#
# The logon must run in the browser — Akamai Bot Manager gates
# preLogonUser/logonUser (DESIGN.md §3) — so `login` and `download` both
# launch Camoufox. `login` is interactive (registers the device, 2FA driven
# from the terminal); `download` is unattended (the trusted device skips
# 2FA). `vnc-login` is the by-hand fallback. `load` ingests bronze into the
# SQLite silver; `prune` is a pure file walk. `explore` is the retained
# discovery harness.

set -euo pipefail

# The verb dispatch (run, login_folds, entrypoint_common) and the Xvfb /
# x11vnc / camoufox-cache bootstrap, baked into the base image at
# /opt/entrypoint-lib.sh.
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

verb="${1:-help}"
[[ $# -gt 0 ]] && shift

case "$verb" in
    explore)   run vnc:explore explore.py -- "$@" ;;
    # Interactive: registers the device, 2FA driven from the terminal.
    login)     run xvfb login.py -- "$@" ;;
    vnc-login) run vnc:vnc-login login.py --no-cli-mfa -- "$@" ;;
    # Unattended: the trusted device skips 2FA.
    download)  run xvfb download.py --bronze-dir /data -- "$@" ;;
    load)      run plain load.py --bronze-dir /data -- "$@" ;;
    prune|sh|bash) entrypoint_common "$verb" "$@" ;;
    help|--help|-h)
        cat <<'EOF'
firstcitizens container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  explore     Discovery harness: Camoufox over VNC + HAR/network log/click
              log/DOM snapshots under /debug. See DESIGN.md §3.
  login       Interactive login — registers this device, 2FA driven from the
              TERMINAL. Leaves device-trust in the persistent profile so
              `download` runs unattended. 'login --check' reports whether the
              trusted-device logon still skips 2FA (sends no code).
  vnc-login   Fallback for login: expose VNC and complete Sign In + 2FA by
              hand. Pair with --fresh to force the untrusted-device 2FA
              flow (wipes the profile's device-trust first).
  download    Unattended logon (trusted device, no 2FA) + REST fetch of the
              deposit accounts (roster, history, exports, statement PDFs)
              into a UTC-stamped bronze run dir under /data. --format picks
              export formats; --no-documents skips statements; --dry-run
              enumerates without exporting.
  load        Ingest bronze into the SQLite silver.
  prune       Delete non-complete dumps from the bronze tree, and aged
              entries of the /debug cache. --dry-run prints the plan.
              Usually run host-side via the wrapper.
  sh|bash     Open an interactive shell.
  help        Show this message.

Run "<wrapper> <subcommand> --help" for subcommand flags.
EOF
        ;;
    *) exec "$verb" "$@" ;;
esac
