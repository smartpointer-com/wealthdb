#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand
# to the corresponding Python script under /app/.
#
# - `explore`: drives Camoufox over VNC for discovery — needs Xvfb
#   + x11vnc started below.
# - `login`: pure-HTTP CLI-MFA, no browser. Skips Xvfb entirely.
# - `download` (TBD): same SPA blob-download pattern as `explore`,
#   needs Xvfb but not VNC.
# - `load`: pure DuckDB + Python, no browser.
#
# When Xvfb IS started (explore + future download), it's started
# directly rather than via `xvfb-run`. The Ubuntu
# Noble xvfb-run script's SIGUSR1 ready-signaling hangs when the
# parent shell is non-root: it sets the parent SIGUSR1 handler to
# `:` rather than SIG_IGN, so Xvfb declines to send the ready
# signal and the parent waits forever. Socket-existence polling
# is a version-independent ready signal.

set -euo pipefail

VFB_DISPLAY=99

# Symlink the pre-staged camoufox cache (populated at image build
# time by `python3 -m camoufox fetch` and copied to /opt) into the
# runtime user's $HOME/.cache/camoufox so camoufox skips its 700MB
# first-run download. HOME is /tmp inside the container (set by
# the wrapper for the non-root user); we always overwrite the
# symlink so a stale /tmp/.cache/camoufox from a previous container
# with a different runtime uid doesn't shadow the staged tree.
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
    # Single-use password. openssl rand -hex 8 is a single command —
    # `set -euo pipefail` does not trip on SIGPIPE the way a piped
    # `tr -dc ... | head -c 16` would.
    VNC_PASSWORD=$(openssl rand -hex 8)
    x11vnc -display ":$VFB_DISPLAY" -passwd "$VNC_PASSWORD" \
        -forever -shared -rfbport 5900 -bg \
        -o /tmp/x11vnc.log >/dev/null 2>&1
    # The wrapper picks the host-side port (it knows which ones are
    # free); we publish it through this env var so the messages below
    # print the actual port the operator needs to tunnel. Defaults
    # to 5900 for direct `docker run` invocations that skip the
    # wrapper.
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
        # Discovery harness. Drives Camoufox over VNC, records the
        # operator's clicks + every network fetch into /debug/<ts>/
        # so login.py + download.py can be written from real traces.
        start_xvfb
        start_x11vnc explore
        shift
        exec python3 /app/explore.py "$@"
        ;;
    login)
        # Headless Playwright Firefox driving the SPA login form.
        # No Xvfb, no VNC. Reads the 2FA code from stdin.
        shift
        exec python3 /app/login.py "$@"
        ;;
    download)
        # Headless Playwright Firefox per-portfolio loop.
        # No Xvfb / no VNC — Playwright handles its own headless
        # X (Firefox runs against an in-process Xwayland/Xvfb
        # equivalent that Playwright manages). Uses the same
        # persistent profile as login.py so authentication carries
        # over for free.
        shift
        exec python3 /app/download.py "$@"
        ;;
    load)
        # The loader is a pure-stdlib stub for now. It applies the
        # placeholder schema migration and exits — keeps the bronze
        # tree + dump_runs gate honest until real artefacts exist.
        shift
        exec python3 /app/load.py "$@"
        ;;
    sh|bash)
        shift
        exec /bin/bash "$@"
        ;;
    help|--help|-h)
        cat <<'EOF'
cointracking container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  explore     Launch Camoufox in the container's Xvfb display and
              record everything the operator does via VNC (HAR +
              Playwright trace + click log under /debug). Use during
              the discovery phase. Starts x11vnc on the first free
              host port in 127.0.0.1:5900-6000 (printed at handoff).
  login       Mint a long-lived cointracking session via headless
              Playwright Firefox. Prompts for 2FA code on stdin;
              ticks "Don't ask again" so the device-trust cookie
              persists for years. Persists to the same Firefox
              profile dir as explore (/secrets/cointracking-profile).
              Pass --check to probe the existing session without
              firing a 2FA push.
  download    Headless Firefox per-portfolio loop: trade history
              (Extended-with-additional-columns blob CSV) + balance
              by exchange CSV per portfolio. Writes a UTC-timestamped
              bronze subdir + run.json manifest. Pass --dry-run to
              walk the navigation without firing exports.
  load        Ingest bronze snapshots into DuckDB silver: refresh
              transactions table, incremental upsert of
              positions_daily (replays from genesis; rewrites
              only from the first deviation day), reconcile final
              balances vs the balance.csv. Pass --replay-only to
              re-run the holdings replay without re-ingesting
              bronze; --force to re-load already-processed
              snapshots.
  sh|bash     Open an interactive shell inside the container.
  help        Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *)
        # Pass-through for ad-hoc commands inside the container.
        exec "$@"
        ;;
esac
