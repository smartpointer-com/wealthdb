# Headed-browser (Camoufox) collector for the EquityZen investor portal.
# Builds on the shared base-camoufox image, which provides
# Firefox/Xvfb/x11vnc OS deps, camoufox + the matching Playwright pin, the
# pre-fetched Firefox bundle at /opt/camoufox-cache, and the collectorkit
# shared package. Per-collector pip deps come from requirements.txt
# (pytest for the in-container tests; silver is stdlib SQLite, so no
# driver dep). poppler-utils (below) is the one system dep.
ARG WEALTHDB_IMAGE_TAG=latest
FROM wealthdb/base-camoufox:${WEALTHDB_IMAGE_TAG}

WORKDIR /app

# poppler-utils provides `pdftotext`, used by statements.py to parse the
# capital-account-statement + K-1 PDFs (load step). Single apt layer.
RUN apt-get update \
    && apt-get install -y --no-install-recommends poppler-utils \
    && rm -rf /var/lib/apt/lists/*

# Install per-collector deps only — camoufox/playwright/collectorkit are
# already in the base.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# No source: every run and every test mounts the checkout over /app
# (HAS_APP_MOUNT in the wrapper), entrypoint.sh included, so the image
# holds only what the code runs on.

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes have
# host-correct ownership. Single-tenant single-origin (equityzen.com
# only); no sandbox-side concerns.

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
