#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand to the
# corresponding Python under /app/.
#
# The sign-in must run in the browser (Akamai's script is cookie-borne and
# runs in the page — DESIGN.md §A), and `download` is the one verb that
# performs one: `login` folded into it (§L) because the sign-in budget is too
# small to spend one on a separate verb. Session cookies die with the browser
# while the device trust persists (§G), so a run on a trusted device signs in
# with no challenge and then fetches the data over REST. `vnc-login` is the
# same walk behind a by-hand sign-in. `load` ingests bronze into the SQLite
# silver — pure Python, no browser.
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
        # log / DOM snapshots under /debug. `explore --help` prints
        # argparse's usage and exits, so it starts neither Xvfb nor the VNC
        # server — reading the flags should cost no display and announce no
        # single-use VNC password.
        shift
        for a in "$@"; do
            case "$a" in
                --help|-h) exec python3 /app/explore.py "$@" ;;
            esac
        done
        start_xvfb
        start_x11vnc explore
        exec python3 /app/explore.py "$@"
        ;;
    login)
        # `login` folds into `download` (DESIGN.md §L). The host wrapper
        # already traps a bare one; this closes the direct-`docker run amex
        # login` path, which would otherwise reach the container's
        # util-linux /bin/login. `login --check` is a real verb: it reads
        # the profile's device-trust cookie straight out of the Firefox jar
        # on disk, so it needs no display, no browser and no network.
        # `--help` prints argparse's usage and likewise starts nothing.
        shift
        for a in "$@"; do
            case "$a" in
                --check|--help|-h) exec python3 /app/login.py "$@" ;;
            esac
        done
        echo "amex: 'login' folds into 'download' — the sign-in budget is" \
             "too small to spend one on a separate verb (DESIGN.md §L)." >&2
        echo "  Run 'download', or 'login --check' to report whether this" \
             "device is registered." >&2
        exit 0
        ;;
    vnc-login)
        # The same walk as `download`, behind a sign-in completed BY HAND in
        # the browser: the only way to answer a captcha, and the way to
        # register a device without a terminal. One sign-in, not two.
        start_xvfb
        start_x11vnc vnc-login
        shift
        exec python3 /app/download.py --vnc-mfa --bronze-dir /data "$@"
        ;;
    download)
        # Submit the form on the trusted device (normally no passcode), then
        # fetch the card data over REST. Headed Camoufox under Xvfb, no VNC.
        # A challenge is answered from the TERMINAL when the wrapper gave the
        # container a TTY, and is a loud failure when it did not — see
        # download.resolve_two_factor.
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
amex container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  explore     Discovery harness: Camoufox over VNC + HAR/network log/click
              log/DOM snapshots under /debug. DESIGN.md §3 records what
              discovery was pointed at, §A–§H what it answered.
  download    One-shot sign-in + REST fetch of the card accounts (roster,
              activity ledger, exports, statement PDFs) into a UTC-stamped
              bronze run dir under /data. A trusted device needs no passcode;
              a challenge is answered from the TERMINAL when there is one —
              registering the device, so the next run is unattended — and is
              a loud failure otherwise (--cli-mfa / --no-cli-mfa force
              either). --lookback bounds the window; --format picks export
              formats; --no-documents skips statements; --dry-run walks
              without exporting.
  vnc-login   Fallback for download: expose VNC and complete Sign In + the
              passcode by hand — the only way to answer a captcha. Pair with
              --fresh to force the untrusted-device flow (moves the profile
              aside first).
  login       Folds into download (the sign-in budget is too small to spend
              one on a separate verb, §L); a bare one is a no-op.
              'login --check' reports whether this device is registered, read
              from the profile's device-trust cookie — no sign-in, no network.
  load        Ingest bronze into the SQLite silver (accounts, the
              transaction ledger, statement balances, documents).
              --force rebuilds from all bronze.
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
