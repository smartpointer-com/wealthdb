# Headed-browser (Camoufox) collector: build on the shared Playwright
# base for its OS deps (libnss, libdrm, fonts that Firefox needs), the
# Python/pip env, and a pre-installed collectorkit. The base image's
# bundled Playwright browsers are unused — camoufox brings its own
# patched Firefox and requirements.txt pins the `playwright` package
# back to 1.49.0 to match camoufox's juggler-protocol revision. The
# Xvfb/x11vnc setup and Camoufox fetch below stay here for now; the
# base-camoufox consolidation will absorb them later.
FROM wealthdb/base-playwright:latest

# Xvfb gives Chromium a virtual X11 display so it can run headed
# inside the container with no real GPU/monitor. x11vnc serves
# that display over VNC so the operator can drive the browser
# (`vnc-login` subcommand) from a host-side VNC client.
#
# Pre-create /tmp/.X11-unix world-writable + sticky. Xvfb tries
# to mkdir it on startup; when the container runs as a non-root
# host user (which we do for volume-ownership reasons), that
# mkdir fails (_XSERVTransmkdir: ERROR: euid != 0) and Xvfb wedges.
# Provisioning the dir at image-build time sidesteps the race.
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

# Camoufox doesn't bundle the patched Firefox binary in the wheel —
# fetch it explicitly here so the image is self-contained and first-
# run is fast. The fetch runs as root, lands under
# /root/.cache/camoufox/. The runtime user has HOME=/tmp, so a naive
# setup would force a 700MB re-download on every container start.
# Stage the fetched tree under a world-readable /opt/camoufox-cache
# and symlink it into the runtime user's cache path at entrypoint
# time (see entrypoint.sh).
RUN python3 -m camoufox fetch && \
    cp -a /root/.cache/camoufox /opt/camoufox-cache && \
    chmod -R go+rX /opt/camoufox-cache

# Copy scripts last. Listed explicitly so accidentally-present
# host artefacts (.venv, real-credential JSONs, recorded Playwright
# traces) never make it into the image even if .dockerignore is
# incomplete. download.py is the one-shot CLI (login → walk →
# logout); load.py is the silver loader (also reachable host-side
# via the wrapper).
COPY download.py load.py /app/
COPY migrations /app/migrations
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes
# have host-correct ownership; Chromium's user-namespace sandbox is
# disabled at launch time in code with `--no-sandbox` instead. This
# is acceptable in a single-tenant, single-trusted-origin scenario
# (we only ever load fidelity.com).

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
