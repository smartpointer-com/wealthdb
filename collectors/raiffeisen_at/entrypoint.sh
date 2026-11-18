#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand to the
# corresponding Python under /app/.
#
# Mein ELBA fires a pushTAN at every sign-in and keeps no session across
# browser restarts (DESIGN.md §F), so — like chase — `download` is a
# one-shot: login + REST fetch in one Firefox lifetime, the pushTAN approved
# on the phone (no code to type, no VNC). `login` has nothing durable to
# persist, so it folds into `download` (except `login --check`, a read-only
# session probe). `vnc-login` is the by-hand fallback. `load` builds the
# silver. `prune` is a pure file walk.

set -euo pipefail

# The verb engine, baked into the base image (shared/images/entrypoint-lib.sh).
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

verb="${1:-help}"
[[ $# -gt 0 ]] && shift

case "$verb" in
    explore)   run vnc:explore explore.py -- "$@" ;;
    # One-shot: fill or confirm the login, pushTAN approved on the phone,
    # REST fetch. Headed Camoufox under Xvfb, no VNC.
    download)  run xvfb login.py --cli-mfa --bronze-dir /data -- "$@" ;;
    # Fallback: the whole login (form/card + pushTAN) by hand over VNC.
    vnc-login) run vnc:vnc-login login.py --no-cli-mfa --bronze-dir /data -- "$@" ;;
    login)
        # `login --check` is a read-only session probe under Xvfb.
        login_folds xvfb login.py -- "$@"
        cat >&2 <<'EOF'
raiffeisen_at: 'login' folds into 'download' — nothing to persist (the pushTAN fires every sign-in; §F).
  Run 'download' (one-shot login + fetch), or 'login --check' to probe a session.
EOF
        ;;
    load)      run plain load.py --bronze-dir /data -- "$@" ;;
    prune|sh|bash) entrypoint_common "$verb" "$@" ;;
    help|--help|-h)
        cat <<'EOF'
raiffeisen_at container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  explore     Discovery harness: Camoufox over VNC + HAR/network log/click
              log/DOM snapshots under /debug. See DESIGN.md §3-Surface.
  download    One-shot login + REST fetch (deposit accounts), the pushTAN
              approved on your phone — no VNC, nothing to type. Fills the
              region/Verfüger/PIN form (or clicks the saved-user card),
              then fetches each account's history + daily balances +
              statement PDFs into a UTC-stamped bronze run dir under /data.
              --lookback bounds the window; --no-documents skips statements;
              --dry-run enumerates without fetching; --fresh forces the cold
              form.
  vnc-login   Fallback for download: expose VNC and complete the login by
              hand (form/card + pushTAN), then fetch.
  login       Folds into download (nothing to persist). 'login --check'
              probes a session read-only (exit 0 alive / 1 dead; dead
              between runs is expected).
  load        Ingest bronze into the SQLite silver (accounts, transactions,
              daily balances, statements). --force rebuilds from all bronze.
  prune       Delete non-complete dumps from the bronze tree. --dry-run
              prints the plan. Usually run host-side via the wrapper.
  sh|bash     Open an interactive shell.
  help        Show this message.

Run "<wrapper> <subcommand> --help" for subcommand flags.
EOF
        ;;
    *) exec "$verb" "$@" ;;
esac
