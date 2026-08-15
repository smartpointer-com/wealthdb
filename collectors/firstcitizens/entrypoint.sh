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
#
# Xvfb is started directly (not via xvfb-run) — the Noble xvfb-run's SIGUSR1
# ready-signalling hangs under a non-root parent; socket-existence polling is
# version-independent.

set -euo pipefail

# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

case "${1:-help}" in
    explore)
        # Discovery harness: Camoufox over VNC + HAR / network log / click
        # log / DOM snapshots under /debug.
        start_xvfb
        start_x11vnc explore
        shift
        exec python3 /app/explore.py "$@"
        ;;
    login)
        # Interactive login (device registration). 2FA is driven from the
        # TERMINAL over stdin — headed Camoufox under Xvfb, no VNC.
        # `--check`/`--help` are headless-friendly but still launch Camoufox
        # to submit the form (the only way past Akamai), so Xvfb is started
        # for them too.
        start_xvfb
        shift
        exec python3 /app/login.py "$@"
        ;;
    vnc-login)
        # Fallback: expose VNC and complete Sign In + 2FA by hand in the
        # browser (use when the terminal 2FA drive can't find a control).
        start_xvfb
        start_x11vnc vnc-login
        shift
        exec python3 /app/login.py --no-cli-mfa "$@"
        ;;
    download)
        # Unattended: submit the form on the trusted device (no 2FA), then
        # fetch the deposit data over REST. Headed Camoufox under Xvfb, no
        # VNC. Fails loudly if device-trust has expired (run `login`).
        start_xvfb
        shift
        exec python3 /app/download.py --bronze-dir /data "$@"
        ;;
    load)
        # Silver loader — pure SQLite + Python, no browser.
        shift
        exec python3 /app/load.py --bronze-dir /data "$@"
        ;;
    prune)
        # Bronze reclaim — pure file walk. Also reachable host-side via the
        # wrapper (the usual path).
        shift
        exec python3 /app/prune.py "$@"
        ;;
    sh|bash)
        shift
        exec /bin/bash "$@"
        ;;
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
  prune       Delete non-complete dumps from the bronze tree. --dry-run
              prints the plan. Usually run host-side via the wrapper.
  sh|bash     Open an interactive shell.
  help        Show this message.

Run "<wrapper> <subcommand> --help" for subcommand flags.
EOF
        ;;
    *)
        exec "$@"
        ;;
esac
