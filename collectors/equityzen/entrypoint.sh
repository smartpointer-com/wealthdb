#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand to the
# corresponding Python script under /app/.
#
# - `explore`: drives Camoufox over VNC for discovery — needs Xvfb +
#   x11vnc started below.
# - `login`:   drives the SPA login form + TOTP via headed Camoufox in the
#   Xvfb display (no VNC) — the proven stealth fingerprint.
# - `download`: same per-surface browse pattern as explore (headed
#   Camoufox under Xvfb, no VNC).
# - `load`:    pure SQLite + Python (+ pdftotext for PDF parsing), no browser.
#
# When Xvfb IS started, it's started directly rather than via `xvfb-run`:
# the Ubuntu Noble xvfb-run script's SIGUSR1 ready-signaling hangs under
# a non-root parent. Socket-existence polling is a version-independent
# ready signal.

set -euo pipefail

VFB_DISPLAY=99

# Symlink the pre-staged camoufox cache (populated at image build time by
# `python3 -m camoufox fetch` and copied to /opt) into the runtime user's
# $HOME/.cache/camoufox so camoufox skips its first-run download. HOME is
# /tmp inside the container (set by the wrapper for the non-root user); we
# always overwrite the symlink so a stale /tmp/.cache/camoufox from a
# previous container with a different runtime uid doesn't shadow it.
mkdir -p /tmp/.cache
if [[ -d /opt/camoufox-cache && ! -e /tmp/.cache/camoufox ]]; then
    ln -snf /opt/camoufox-cache /tmp/.cache/camoufox
fi

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

start_x11vnc() {
    # Single-use password. openssl rand -hex 8 is a single command, so
    # `set -euo pipefail` does not trip on SIGPIPE the way a piped
    # `tr -dc ... | head -c 16` would.
    VNC_PASSWORD=$(openssl rand -hex 8)
    x11vnc -display ":$VFB_DISPLAY" -passwd "$VNC_PASSWORD" \
        -forever -shared -rfbport 5900 -bg \
        -o /tmp/x11vnc.log >/dev/null 2>&1
    # The wrapper picks the host-side port (it knows which are free); we
    # publish it through VNC_HOST_PORT so the messages below print the
    # real port to tunnel through. Defaults to 5900 for direct
    # `docker run` invocations that skip the wrapper.
    local label="$1"
    local host_port="${VNC_HOST_PORT:-5900}"
    echo "$label: VNC ready on 127.0.0.1:${host_port}" >&2
    echo "$label: password (single-use):  $VNC_PASSWORD" >&2
    echo "$label: tunnel from your laptop with" >&2
    echo "$label:   ssh -L ${host_port}:127.0.0.1:${host_port} <host>" >&2
    echo "$label: then on the laptop:" >&2
    echo "$label:   open vnc://localhost:${host_port}" >&2
}

case "${1:-help}" in
    explore)
        # Discovery harness. Drives Camoufox over VNC, records every
        # click + every network fetch into /debug/<ts>/ so login.py +
        # download.py can be written from real traces.
        start_xvfb
        start_x11vnc explore
        shift
        exec python3 /app/explore.py "$@"
        ;;
    login)
        # SPA login form + TOTP, reading the 2FA code from stdin. Runs
        # headed Camoufox in the Xvfb virtual display (the proven stealth
        # fingerprint) — no VNC. Xvfb is needed for the headed browser; the
        # operator interacts only via the CLI TOTP prompt.
        start_xvfb
        shift
        exec python3 /app/login.py "$@"
        ;;
    download)
        # Per-surface browse + GraphQL-capture loop. Headed Camoufox in the
        # Xvfb display (same engine/fingerprint as login), no VNC. Uses the
        # same persistent profile as login.py so authentication carries over.
        start_xvfb
        shift
        exec python3 /app/download.py "$@"
        ;;
    load)
        # SQLite silver loader. Ingests bronze (offering/position/
        # cash-flow JSON, document PDFs) into the source-shaped silver
        # tables. Pure Python, no browser, no display.
        shift
        exec python3 /app/load.py "$@"
        ;;
    sh|bash)
        shift
        exec /bin/bash "$@"
        ;;
    help|--help|-h)
        cat <<'EOF'
equityzen container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  explore     Launch Camoufox in the container's Xvfb display and record
              every action taken in the VNC session (HAR + Playwright
              trace + click log under /debug). Use during the discovery
              phase. Starts x11vnc on the first free host port in
              127.0.0.1:5900-6000 (printed at handoff).
  login       Mint an EquityZen session (headed Camoufox under Xvfb, no
              VNC). Prompts for the 6-digit TOTP code on stdin; persists to
              the profile dir at /secrets/equityzen-profile and renews
              silently if still valid. Pass --check to probe the existing
              session without firing a 2FA push.
  download    Browse the investor surfaces and capture bronze: offerings +
              positions + cash flows (getBuyerInvestments per stage +
              getMyInvestmentDetails per offering). Pass --dry-run for a
              read-only smoke test (writes nothing); --documents to also
              record doc/tax-centre metadata.
  load        Ingest bronze snapshots into the SQLite silver: offerings,
              positions, cash_flows, tax_documents. Pass --force to
              re-load snapshots already recorded in dump_runs.
  sh|bash     Open an interactive shell inside the container.
  help        Show this message.

NOTE: all four verbs (explore / login / download / load) are
implemented. See DESIGN.md.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *)
        # Pass-through for ad-hoc commands inside the container.
        exec "$@"
        ;;
esac
