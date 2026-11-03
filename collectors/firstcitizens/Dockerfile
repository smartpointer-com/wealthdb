# Headed-browser (Camoufox) collector for the First Citizens Bank retail
# deposit accounts (checking + savings). Builds on the shared
# base-camoufox image, which provides Firefox/Xvfb/x11vnc OS deps,
# camoufox + the matching Playwright pin, the pre-fetched Firefox bundle
# at /opt/camoufox-cache, and the collectorkit shared package.
#
# Camoufox, not vanilla Playwright: Akamai Bot Manager gates the logon
# (DESIGN.md §3), as it does for every US sibling (chase, fidelity-web,
# schwab-web). No fallback engine is installed.
ARG WEALTHDB_IMAGE_TAG=latest
FROM wealthdb/base-camoufox:${WEALTHDB_IMAGE_TAG}

WORKDIR /app

# poppler's pdftotext extracts the statement-PDF text the loader reads each
# statement's interest rate from (statement_parser.py).
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
# host-correct ownership. Single-tenant single-origin
# (firstcitizens.com only).

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
