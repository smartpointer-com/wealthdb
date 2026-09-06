# Shared base for the camoufox-using collectors (the private-market
# scrapers, schwab-web / schwab-api, fidelity-web, cointracking).
# Layers camoufox-pinned Playwright and a pre-fetched Firefox bundle
# onto base-playwright, which already carries Xvfb/x11vnc and the shared
# entrypoint bootstrap (/opt/entrypoint-lib.sh) that each collector's
# entrypoint.sh sources.
# Collectors FROM this skip their own camoufox setup; their
# requirements.txt only needs to add per-collector extras (pdfplumber,
# pypdfium2, pytest).
#
# Build context is shared/ (so collectorkit/ in the base layers is
# resolvable):
#   docker build -f images/base-camoufox.Dockerfile -t wealthdb/base-camoufox:latest .
FROM wealthdb/base-playwright:latest

# Xvfb, x11vnc and the shared entrypoint bootstrap come from
# base-playwright — running a browser headed in a container is not a
# camoufox-specific need, so it lives one layer down.

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

