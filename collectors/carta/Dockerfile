# Headed-browser (Camoufox) collector for the Carta holder web UI. Builds on
# the shared base-camoufox image, which provides Firefox/Xvfb/x11vnc OS deps,
# camoufox + the matching Playwright pin, the pre-fetched Firefox bundle at
# /opt/camoufox-cache, and the collectorkit shared package. Per-collector
# Python deps come from requirements.txt; poppler-utils (below) is the one
# system dep.
ARG WEALTHDB_IMAGE_TAG=latest
FROM wealthdb/base-camoufox:${WEALTHDB_IMAGE_TAG}

WORKDIR /app

# Install per-collector deps only — camoufox/playwright/collectorkit are
# already in the base.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# poppler-utils provides `pdftotext`, used by load.py to parse the fund's
# capital-account-statement PDFs into a quarterly NAV time series (DESIGN.md
# §5.1) and the K-1 face pages (§5.3). The exercise-detail xlsx are parsed
# with the stdlib zipfile, so no spreadsheet dep is needed.
RUN apt-get update && apt-get install -y --no-install-recommends poppler-utils \
    && rm -rf /var/lib/apt/lists/*

# Copy scripts last, by name, so stray host artefacts (.venv,
# real-credential JSONs, traces, sample holder-UI HTML) never enter the
# image.
COPY explore.py login.py download.py load.py k1.py prune.py /app/
COPY migrations /app/migrations
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes have
# host-correct ownership. Single-tenant single-origin (carta.com only);
# no sandbox-side concerns.

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
