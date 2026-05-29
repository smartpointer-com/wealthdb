# Shared base for camoufox-using collectors (fidelity-web, schwab-web).
# Layers Xvfb/x11vnc, camoufox-pinned Playwright, and a pre-fetched
# Firefox bundle onto base-playwright. Collectors FROM this skip their
# own xvfb/camoufox setup; their requirements.txt only needs to add
# per-collector extras (pdfplumber, pypdfium2, pytest).
#
# Build context is shared/ (so collectorkit/ in the base layers is
# resolvable):
#   docker build -f images/base-camoufox.Dockerfile -t wealthdb/base-camoufox:latest .
FROM wealthdb/base-playwright:latest

# Xvfb gives Firefox a virtual X11 display so it can run headed
# inside the container with no real GPU/monitor. x11vnc serves
# that display over VNC so the operator can drive the browser
# (vnc-login subcommand) from a host-side VNC client.
#
# Pre-create /tmp/.X11-unix world-writable + sticky. Xvfb tries
# to mkdir it on startup; when the container runs as a non-root
# host user (we always do for volume-ownership reasons), that
# mkdir fails (_XSERVTransmkdir: ERROR: euid != 0) and Xvfb wedges.
# Provisioning the dir at image-build time sidesteps the race.
RUN apt-get update && \
    apt-get install -y --no-install-recommends xvfb x11vnc && \
    rm -rf /var/lib/apt/lists/* && \
    mkdir -p /tmp/.X11-unix && \
    chmod 1777 /tmp/.X11-unix

# camoufox-py 0.4.11 downloads Firefox 135.0.1-beta.24 whose juggler
# patches target Playwright 1.49.x; base-playwright's bundled Playwright
# is much newer and crashes camoufox-launched Firefox. Pinning Playwright
# back here overrides the upstream version for both collectors.
RUN pip install 'camoufox[geoip]>=0.4.11,<0.5' 'playwright==1.49.0'

# Camoufox doesn't bundle the patched Firefox binary in the wheel —
# fetch it explicitly so the image is self-contained and first-run
# is fast. The fetch runs as root and lands under /root/.cache/camoufox/.
# The runtime user has HOME=/tmp, so a naive setup would force a 700MB
# re-download on every container start. Stage the fetched tree under a
# world-readable /opt/camoufox-cache; each collector's entrypoint.sh
# symlinks it into the runtime user's cache path.
RUN python3 -m camoufox fetch && \
    cp -a /root/.cache/camoufox /opt/camoufox-cache && \
    chmod -R go+rX /opt/camoufox-cache
