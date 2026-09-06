# Shared base for headless-browser (Playwright) collectors: Microsoft's
# version-matched Playwright Python image with collectorkit pre-installed.
# Bump the tag in lockstep with the collectors' pinned `playwright` pip
# version. Build context is shared/:
#   docker build -f images/base-playwright.Dockerfile -t wealthdb/base-playwright:latest .
FROM mcr.microsoft.com/playwright/python:v1.62.0-noble

# Fetch Ubuntu's archives over HTTPS instead of the image default of HTTP.
# Port 80 to the Ubuntu archive hosts is unreachable from some networks —
# the connection times out rather than being refused, so apt burns minutes
# of retries per index before failing the build — while port 443 to the very
# same hosts answers normally. Every apt-get in this image's descendants
# (base-camoufox, and carta / angellist / equityzen on top of it) inherits
# these sources, so the switch belongs here rather than at each call site.
# apt >= 1.5 speaks HTTPS natively and the image already ships
# ca-certificates, so this needs no extra package and no extra layer cost.
# ports.* serves arm64/ppc64el; archive.*/security.* serve amd64 — rewrite
# all three so the image builds the same way on either host arch.
RUN sed -i -E 's#http://(ports|archive|security)\.ubuntu\.com#https://\1.ubuntu.com#g' \
        /etc/apt/sources.list.d/ubuntu.sources

# Xvfb gives a browser a virtual X11 display so it can run HEADED inside
# the container with no real GPU/monitor; x11vnc serves that display over
# VNC so the browser can be driven by hand from a host-side VNC client
# (the `explore` discovery harnesses, and the vnc-login fallbacks).
#
# This lives here rather than in base-camoufox because it is a property of
# running a browser in a container, not of any one browser: the Chromium
# collectors need the same display to be driven interactively. Descendants
# that never run headed simply carry two unused packages.
#
# Pre-create /tmp/.X11-unix world-writable + sticky. Xvfb tries to mkdir it
# on startup; when the container runs as a non-root host user (we always do
# for volume-ownership reasons), that mkdir fails
# (_XSERVTransmkdir: ERROR: euid != 0) and Xvfb wedges. Provisioning the dir
# at image-build time sidesteps the race.
RUN apt-get update && \
    apt-get install -y --no-install-recommends xvfb x11vnc && \
    rm -rf /var/lib/apt/lists/* && \
    mkdir -p /tmp/.X11-unix && \
    chmod 1777 /tmp/.X11-unix

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

WORKDIR /app

# Bake in the shared collector library.
COPY collectorkit /opt/collectorkit
RUN pip install /opt/collectorkit

# Shared Xvfb / x11vnc / browser-cache bootstrap, sourced by the entrypoint
# of every collector that runs a browser headed. One copy here rather than
# the same ~50 lines pasted into each entrypoint.
COPY images/entrypoint-lib.sh /opt/entrypoint-lib.sh
