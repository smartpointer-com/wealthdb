# Pin to a specific Playwright Python image tag for reproducibility.
# Microsoft publishes one image per Playwright version; the Python
# bindings and Chromium binaries inside are version-matched. Bump
# this tag when upgrading Playwright; do NOT mix it with a different
# `playwright` pip version in requirements.txt.
FROM mcr.microsoft.com/playwright/python:v1.59.0-noble

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

# Phase 1 (VNC-driven discovery) needs a real X11 display so
# Chromium can run headed and the operator can drive the login
# flow from a host-side VNC client. Phase 2/3 (login.py, download.py)
# run headless against the same image.
#
# Xvfb supplies the virtual display; x11vnc serves it; fluxbox is a
# minimal window manager so the operator can move/focus windows
# during exploration (mirrors the VNC plumbing in schwab-web-dump,
# which omits the WM — but VIAC's SPA navigation benefits from
# proper window focus on the operator side).
#
# Pre-create /tmp/.X11-unix world-writable + sticky. Xvfb tries to
# mkdir it on startup; when the container runs as a non-root host
# user (which we do for volume-ownership reasons), that mkdir
# fails with `_XSERVTransmkdir: ERROR: euid != 0` and Xvfb wedges.
# Provisioning the dir at image-build time sidesteps the race.
RUN apt-get update && \
    apt-get install -y --no-install-recommends xvfb x11vnc fluxbox && \
    rm -rf /var/lib/apt/lists/* && \
    mkdir -p /tmp/.X11-unix && \
    chmod 1777 /tmp/.X11-unix

WORKDIR /app

# Install Python deps first so editing scripts doesn't bust the
# pip cache layer.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# Copy scripts last. Listed explicitly so accidentally-present host
# artefacts (.venv, real-credential env files, recorded Playwright
# traces) never make it into the image even if .dockerignore is
# incomplete. Scripts are STUBS in the initial commit; phased
# implementations land in subsequent commits.
COPY explore.py login.py download.py load.py viac_client.py /app/
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Default user is root. The host wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes
# have host-correct ownership; Chromium's user-namespace sandbox is
# disabled at launch time in code with `--no-sandbox` instead. This
# is acceptable in a single-tenant, single-trusted-origin scenario
# (we only ever load app.viac.ch).

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
