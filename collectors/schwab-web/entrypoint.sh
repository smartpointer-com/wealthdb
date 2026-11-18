#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand
# to the corresponding Python script under /app/. The browser-
# driving subcommands (download, vnc-login) start an Xvfb virtual
# X11 display first so Firefox can run headed (Schwab's anti-bot
# detection flags headless markers, so running without a display
# is not an option).

set -euo pipefail

# The verb engine, baked into the base image (shared/images/entrypoint-lib.sh).
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

verb="${1:-help}"
[[ $# -gt 0 ]] && shift

case "$verb" in
    # One-shot CLI-MFA login + scrape, headed under Xvfb.
    download)
        run xvfb login.py --profile-dir /secrets/schwab-web-profile \
            --cli-mfa --bronze-dir /data -- "$@"
        ;;
    vnc-login)
        run vnc:vnc-login login.py --profile-dir /secrets/schwab-web-profile \
            --no-cli-mfa --bronze-dir /data -- "$@"
        ;;
    load)                run plain load.py -- "$@" ;;
    collapse-statements) run plain dedup.py -- "$@" ;;
    login)
        cat >&2 <<'EOF'
schwab-web: 'login' folds into 'download' — nothing to persist.
  This one-shot scraper mints its session inside 'download'
  (CLI-MFA prompt on stdin). Run 'download' directly, or
  'vnc-login' when the CLI-MFA selectors drift.
EOF
        ;;
    prune|sh|bash) entrypoint_common "$verb" "$@" ;;
    help|--help|-h)
        cat <<'EOF'
schwab-web container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  download    One-shot CLI-MFA login + scrape. Pre-fills the
              form from SCHWAB_LOGIN_ID / SCHWAB_PASSWORD,
              auto-submits, prompts for the 2FA code on stdin,
              runs the statements + tx-history download in the
              same Firefox session, exits. Default range: 3
              months (widen with --lookback; --lookback all for a
              full backfill, capped at Schwab's ~10 years). Stdin
              must be a TTY.
  load        Parse bronze into the silver SQLite database.
  collapse-statements
              Collapse re-rendered statement PDFs that parse
              identically onto the oldest copy (reclaims the
              per-download byte churn the fleet `wealthdb-collect
              dedup` byte-identical sweep can't). --dry-run prints
              the evidence report first.
  prune       Delete <run>/screenshots/ and stray
              transactions/*/page-*.html from complete dumps, whole
              non-complete dumps, and idle entries of the /debug
              cache. --dry-run prints the plan first.
  vnc-login   Fallback to a VNC-driven login + scrape when the
              CLI-MFA selectors drift or a non-code challenge is
              required. Starts x11vnc; the wrapper publishes it on
              the first free 127.0.0.1 port in 5900-6000 (printed at
              start). Tunnel to it and connect a local VNC client.
  sh|bash     Open an interactive shell inside the container.
  help        Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *) exec "$verb" "$@" ;;
esac
