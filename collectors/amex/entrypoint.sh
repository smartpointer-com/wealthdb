#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand to the
# corresponding Python under /app/.
#
# The sign-in must run in the browser (Akamai's script is cookie-borne and
# runs in the page — DESIGN.md §A), and `download` is the one verb that
# performs one (§L). Session cookies die with the browser
# while the device trust persists (§G), so a run on a trusted device signs in
# with no challenge and then fetches the data over REST. `vnc-login` is the
# same walk behind a by-hand sign-in. `load` ingests bronze into the SQLite
# silver — pure Python, no browser.

set -euo pipefail

# The verb engine, baked into the base image (shared/images/entrypoint-lib.sh).
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

verb="${1:-help}"
[[ $# -gt 0 ]] && shift

case "$verb" in
    explore)   run vnc:explore explore.py -- "$@" ;;
    login)
        # Folds into `download` (§L). `login --check` reads the profile's
        # device-trust cookie: no browser, no network.
        login_folds plain login.py -- "$@"
        cat >&2 <<'EOF'
amex: 'login' folds into 'download' — the sign-in budget is too small to spend one on a separate verb (DESIGN.md §L).
  Run 'download', or 'login --check' to report whether this device is registered.
EOF
        ;;
    # The sign-in walk behind a by-hand sign-in over VNC, the only way to
    # answer a captcha.
    vnc-login) run vnc:vnc-login download.py --vnc-mfa --bronze-dir /data -- "$@" ;;
    # Headed Camoufox under Xvfb, no VNC: a challenge is answered from the
    # terminal.
    download)  run xvfb download.py --bronze-dir /data -- "$@" ;;
    load)      run plain load.py --bronze-dir /data -- "$@" ;;
    prune|sh|bash) entrypoint_common "$verb" "$@" ;;
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
    *) exec "$verb" "$@" ;;
esac
