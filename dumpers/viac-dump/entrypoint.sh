#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand to
# the corresponding Python script under /app/. The Phase 1 verb
# (`vnc-explore`) starts an Xvfb virtual display + x11vnc + fluxbox
# so the operator can drive Chromium from a host-side VNC client;
# the Phase 2 / Phase 3 verbs (`login`, `download`) default to
# headless Chromium against the same image.

set -euo pipefail

VFB_DISPLAY=99

start_xvfb() {
    Xvfb ":$VFB_DISPLAY" -screen 0 1280x800x24 -nolisten tcp \
        >/tmp/xvfb.log 2>&1 &
    for _ in $(seq 1 50); do
        if [[ -S "/tmp/.X11-unix/X$VFB_DISPLAY" ]]; then
            export DISPLAY=":$VFB_DISPLAY"
            return 0
        fi
        sleep 0.1
    done
    echo "entrypoint: Xvfb failed to start within 5s; /tmp/xvfb.log:" >&2
    tail -20 /tmp/xvfb.log >&2 || true
    return 1
}

start_fluxbox() {
    # Background a minimal WM so window focus + drag works inside
    # the VNC session. fluxbox writes its own ~/.fluxbox files;
    # HOME=/tmp inside the container so they land in a tmpfs-backed
    # location and don't leak to /secrets.
    fluxbox >/tmp/fluxbox.log 2>&1 &
}

case "${1:-help}" in
    vnc-explore)
        # Phase 1: VNC-driven exploration. Starts Xvfb + x11vnc +
        # fluxbox so the operator can connect with a VNC client,
        # log in by hand, and walk the VIAC SPA while explore.py
        # records every network request/response, DOM snapshot,
        # download, and screenshot to the discovery dir. A fresh
        # single-use VNC password is generated each launch and
        # printed to stderr.
        start_xvfb
        start_fluxbox
        VNC_PASSWORD=$(openssl rand -hex 8)
        x11vnc -display ":$VFB_DISPLAY" -passwd "$VNC_PASSWORD" \
            -forever -shared -rfbport 5900 -bg \
            -o /tmp/x11vnc.log >/dev/null 2>&1
        echo "vnc-explore: VNC ready on 127.0.0.1:5900" >&2
        echo "vnc-explore: password (single-use):  $VNC_PASSWORD" >&2
        echo "vnc-explore: connect from this host with:" >&2
        echo "vnc-explore:   open vnc://localhost:5900" >&2
        echo "vnc-explore: (on a remote host, tunnel first:" >&2
        echo "vnc-explore:   ssh -L 5900:127.0.0.1:5900 <host>)" >&2
        shift
        exec python3 /app/explore.py "$@"
        ;;
    login)
        # Phase 2: mint or refresh the persistent session.
        # Headless by default; --check is a cheap liveness probe
        # that does NOT trigger an MFA challenge.
        shift
        exec python3 /app/login.py "$@"
        ;;
    download)
        # Phase 3: live scrape against an already-authenticated
        # session. Headless by default; --dry-run walks the UI
        # without exporting any artefacts.
        shift
        exec python3 /app/download.py "$@"
        ;;
    load)
        # Phase 4: parse bronze into the silver SQLite database.
        shift
        exec python3 /app/load.py "$@"
        ;;
    sh|bash)
        shift
        exec /bin/bash "$@"
        ;;
    help|--help|-h)
        cat <<'EOF'
viac-dump container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  vnc-explore   Phase 1: VNC-driven exploration. Starts Xvfb +
                x11vnc + fluxbox on display :99, exposes VNC on
                127.0.0.1:5900, and runs explore.py to record
                network / DOM / downloads while you drive the
                browser by hand.
  login         Phase 2: mint or refresh the persistent session.
                Use --check for a cheap liveness probe (no MFA).
  download      Phase 3: live scrape. Use --dry-run to walk the
                UI without exporting any artefacts.
  load          Phase 4: parse bronze into the silver SQLite DB.
  sh|bash       Open an interactive shell inside the container.
  help          Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *)
        # Pass-through for ad-hoc commands inside the container,
        # e.g. `docker run viac-dump python -c '...'`.
        exec "$@"
        ;;
esac
