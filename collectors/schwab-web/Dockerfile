# Pin to a specific Playwright Python image tag for the underlying
# OS layer and bundled browsers. Playwright 1.59.0 ships Firefox
# at /ms-playwright/firefox-1511, which is what this build expects.
FROM mcr.microsoft.com/playwright/python:v1.59.0-noble

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

# Xvfb gives Firefox a virtual X11 display so it can run headed
# inside the container with no real GPU/monitor. x11vnc serves
# that display over VNC so the operator can drive the browser
# (`vnc-login` subcommand) from a host-side VNC client.
#
# Pre-create /tmp/.X11-unix as world-writable + sticky. Xvfb
# tries to mkdir it on startup; when the container runs as a
# non-root host user (which we do for volume-ownership reasons),
# that mkdir fails (_XSERVTransmkdir: ERROR: euid != 0) and Xvfb
# wedges. Provisioning the dir at image-build time sidesteps the
# race.
RUN apt-get update && \
    apt-get install -y --no-install-recommends xvfb x11vnc && \
    rm -rf /var/lib/apt/lists/* && \
    mkdir -p /tmp/.X11-unix && \
    chmod 1777 /tmp/.X11-unix

WORKDIR /app

# Install Python deps first so editing scripts doesn't bust the pip
# cache layer.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# Camoufox doesn't bundle the patched Firefox binary in the wheel
# — fetch it explicitly here so the image is self-contained and
# first-run is fast. The fetch runs as root, lands under
# /root/.cache/camoufox/. The runtime user has HOME=/tmp, so a
# naive setup would force a 700MB re-download on every container
# start. Stage the fetched tree under a world-readable
# /opt/camoufox-cache and symlink it into the runtime user's
# cache path at entrypoint time (see entrypoint.sh).
RUN python3 -m camoufox fetch && \
    cp -a /root/.cache/camoufox /opt/camoufox-cache && \
    chmod -R go+rX /opt/camoufox-cache

# Copy scripts last. Listed explicitly so accidentally-present
# host artefacts (.venv, real-credential JSONs, recorded Playwright
# traces) never make it into the image even if .dockerignore is
# incomplete.
COPY login.py download.py landmarks.py pdf_parsers.py load.py /app/
COPY migrations /app/migrations
COPY tests /app/tests
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes
# have host-correct ownership. Single-tenant single-origin
# (schwab.com only); no sandbox-side concerns to address.

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
