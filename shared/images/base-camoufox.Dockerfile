# Shared base for the camoufox-using collectors (the private-market
# scrapers, schwab-web / schwab-api, fidelity-web, cointracking).
# Layers Xvfb/x11vnc,
# camoufox-pinned Playwright, and a pre-fetched Firefox bundle onto
# base-playwright, and bakes the shared entrypoint bootstrap
# (/opt/entrypoint-lib.sh) that each collector's entrypoint.sh sources.
# Collectors FROM this skip their own xvfb/camoufox setup; their
# requirements.txt only needs to add per-collector extras (pdfplumber,
# pypdfium2, pytest).
#
# Build context is shared/ (so collectorkit/ in the base layers is
# resolvable):
#   docker build -f images/base-camoufox.Dockerfile -t wealthdb/base-camoufox:latest .
FROM wealthdb/base-playwright:latest

# Xvfb gives Firefox a virtual X11 display so it can run headed
# inside the container with no real GPU/monitor. x11vnc serves
# that display over VNC so the browser can be driven interactively
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

# The browser build, camoufox-py, and Playwright are a matched set: the
# browser's juggler patches target a specific Playwright protocol, and
# base-playwright's bundled Playwright is much newer and crashes
# camoufox-launched Firefox — so Playwright is pinned back here for both
# collectors. Bump CAMOUFOX_BUILD deliberately and re-validate the
# collectors against their sources' bot detection; camoufox-py's own
# fetch constraint spans Firefox majors, which is why the build is
# pinned explicitly below (see fetch-camoufox.py).
ARG CAMOUFOX_BUILD=152.0.4-beta.26
RUN pip install 'camoufox[geoip]>=0.4.11,<0.5' 'playwright==1.49.0'

# Camoufox doesn't bundle the patched Firefox binary in the wheel —
# fetch the pinned build explicitly so the image is self-contained,
# reproducible, and first-run is fast. The fetch runs as root and lands
# under /root/.cache/camoufox/. The runtime user has HOME=/tmp, so a
# naive setup would force a 700MB re-download on every container start.
# Stage the fetched tree under a world-readable /opt/camoufox-cache;
# each collector's entrypoint.sh symlinks it into the runtime user's
# cache path.
COPY images/fetch-camoufox.py /tmp/fetch-camoufox.py
RUN python3 /tmp/fetch-camoufox.py "$CAMOUFOX_BUILD" && \
    rm /tmp/fetch-camoufox.py && \
    cp -a /root/.cache/camoufox /opt/camoufox-cache && \
    chmod -R go+rX /opt/camoufox-cache

# Shared Xvfb / x11vnc / camoufox-cache bootstrap, sourced by every
# collector's entrypoint.sh so the ~50-line setup lives in one place.
COPY images/entrypoint-lib.sh /opt/entrypoint-lib.sh
