# Headed-browser (Camoufox) collector image for the Chase retail web UI
# (chase.com: checking + savings, plus credit-card reads). Builds on the
# shared base-camoufox image, which provides Firefox/Xvfb/x11vnc OS deps,
# camoufox + the matching Playwright pin, the pre-fetched Firefox bundle at
# /opt/camoufox-cache, and the collectorkit shared package.
#
# Camoufox from the start, not vanilla Playwright: both US siblings
# (fidelity-web, schwab-web) measured Akamai-class bot defense blocking
# vanilla Chromium outright, and Chase fronts the same class of defense —
# the vanilla rungs are skipped rather than re-measured. No fallback
# engine is installed until a real capture shows one is needed.
ARG WEALTHDB_IMAGE_TAG=latest
FROM wealthdb/base-camoufox:${WEALTHDB_IMAGE_TAG}

WORKDIR /app

# poppler's pdftotext extracts the statement-PDF text the loader parses for
# transactions older than Chase's 24-month export cap (statement_parser.py).
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
# host-correct ownership. Single-tenant single-origin (chase.com only).

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
