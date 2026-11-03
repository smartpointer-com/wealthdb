# Headed-browser (Camoufox) collector for Mein ELBA, the Austrian
# Raiffeisen retail e-banking portal (mein.elba.raiffeisen.at: checking +
# savings). Builds on the shared base-camoufox image, which provides
# Firefox/Xvfb/x11vnc OS deps, camoufox + the matching Playwright pin, the
# pre-fetched Firefox bundle at /opt/camoufox-cache, and the collectorkit
# shared package.
#
# Camoufox, the fleet's standard browser; ELBA shows no bot defense
# (DESIGN.md §1).
ARG WEALTHDB_IMAGE_TAG=latest
FROM wealthdb/base-camoufox:${WEALTHDB_IMAGE_TAG}

WORKDIR /app

# poppler's pdftotext extracts the text of the transaction listings the
# loader stitches in from supplied/ (collectorkit.pdftotext, DESIGN.md §I).
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
# host-correct ownership. Single-tenant single-origin (raiffeisen.at
# only).

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
