# Headed-browser (Camoufox) collector: builds on the shared base-camoufox
# image, which provides Firefox/Xvfb/x11vnc OS deps, camoufox + the
# matching Playwright pin, the pre-fetched Firefox bundle at
# /opt/camoufox-cache, and collectorkit. Per-collector extras
# (pdfplumber for statement parsing, pytest for in-container tests)
# go into requirements.txt below.
ARG WEALTHDB_IMAGE_TAG=latest
FROM wealthdb/base-camoufox:${WEALTHDB_IMAGE_TAG}

WORKDIR /app

# Install per-collector deps only — camoufox/playwright are already
# pre-installed in the base.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# No source: every run and every test mounts the checkout over /app
# (HAS_APP_MOUNT in the wrapper), entrypoint.sh included, so the image
# holds only what the code runs on.

# Default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes
# have host-correct ownership. download.py drives Camoufox (a
# stealth-patched Firefox fork) headed under the entrypoint's Xvfb —
# there is no Chromium here. Single-tenant, single-trusted-origin
# (the collector loads only fidelity.com).

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
