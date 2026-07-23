# Headed-browser (Camoufox) collector for the Carta holder web UI. Builds on
# the shared base-camoufox image, which provides Firefox/Xvfb/x11vnc OS deps,
# camoufox + the matching Playwright pin, the pre-fetched Firefox bundle at
# /opt/camoufox-cache, and the collectorkit shared package. Per-collector
# Python deps come from requirements.txt; poppler-utils (below) is the one
# system dep.
FROM wealthdb/base-camoufox:latest

WORKDIR /app

# Install per-collector deps only — camoufox/playwright/collectorkit are
# already in the base.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# poppler-utils provides `pdftotext`, used by load.py to parse the fund's
# capital-account-statement PDFs into a quarterly NAV time series (DESIGN.md
# §5.1). The exercise-detail xlsx are parsed with the stdlib zipfile, so no
# spreadsheet dep is needed.
RUN apt-get update && apt-get install -y --no-install-recommends poppler-utils \
    && rm -rf /var/lib/apt/lists/*

# Vanilla Playwright Firefox, installed as a fallback engine. Carta's
# login is fronted by Cloudflare Turnstile bot detection, so login.py
# and download.py run on Camoufox (the base image's stealth-patched
# Firefox), which clears it. This plain-Firefox install is kept as
# cheap insurance and mirrors the cointracking image.
RUN playwright install firefox && playwright install-deps firefox

# Copy scripts last. Listed explicitly so accidentally-present host
# artefacts (.venv, real-credential JSONs, recorded traces, sample
# holder-UI HTML) never make it into the image even if .dockerignore is
# incomplete.
COPY explore.py login.py download.py load.py prune.py /app/
COPY migrations /app/migrations
COPY tests /app/tests
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes have
# host-correct ownership. Single-tenant single-origin (carta.com only);
# no sandbox-side concerns.

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
