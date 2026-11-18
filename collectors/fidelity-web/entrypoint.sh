#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand
# to the corresponding Python script under /app/. The browser-
# driving subcommands (download, vnc-login) start an Xvfb virtual
# X11 display first so Camoufox can run headed.

set -euo pipefail

# The verb dispatch (run, login_folds, entrypoint_common) and the Xvfb /
# x11vnc / camoufox-cache bootstrap, baked into the base image at
# /opt/entrypoint-lib.sh.
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

verb="${1:-help}"
[[ $# -gt 0 ]] && shift

case "$verb" in
    explore)    run vnc:explore explore.py -- "$@" ;;
    download)   run xvfb download.py -- "$@" ;;
    vnc-login)  run vnc:vnc-login download.py --vnc -- "$@" ;;
    load)       run plain load.py --bronze-dir /data --silver-db /data/fidelity-web.db -- "$@" ;;
    recompress) run plain recompress.py -- "$@" ;;
    login)
        login_folds -- "$@" <<'EOF'
fidelity-web: 'login' folds into 'download' — nothing to persist.
  The Fidelity session is browser-lifetime only, so
  there's nothing to persist from a login-and-exit.
  Use instead:
    vnc-login   first-time / re-auth via VNC-assisted MFA
    download    one-shot login -> walk (login is folded in)
EOF
        ;;
    prune|sh|bash) entrypoint_common "$verb" "$@" ;;
    help|--help|-h)
        cat <<'EOF'
fidelity-web container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  explore     Discovery harness (DAF surface): Camoufox over VNC +
              HAR/network log/click log/DOM snapshots under /debug.
              See DESIGN.md §12.
  download    One-shot login → walk → logout → exit. Prompts on stdin
              for the Fidelity 2FA code; auto-skips MFA when the
              device-trust cookie in --profile-dir is still valid.
              Pass --check to only validate the session (no walk).
  vnc-login   First-time profile-dir seed: pre-fills credentials,
              hands off to a VNC client for the human-driven click +
              2FA, then continues with the walk. Starts x11vnc on
              the first free host port in 127.0.0.1:5900-6000 (the
              wrapper picks; printed at handoff). Pass `--mode none`
              to skip the walk and just seed cookies.
  load        Parse bronze into the silver SQLite database.
  prune       Delete debug artefacts and non-complete dumps from
              the bronze tree, and aged entries of the /debug cache.
              --dry-run prints the plan first.
  recompress  Convert the pre-compression bronze backlog: replace
              plain HTML/CSV inside complete dumps with sha256-
              verified .zst twins. Manual one-time sweep; --dry-run
              prints the plan first.
  sh|bash     Open an interactive shell inside the container.
  help        Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *) exec "$verb" "$@" ;;
esac
