# Headed-browser (Camoufox) collector: builds on the shared
# base-camoufox image, which provides Firefox/Xvfb/x11vnc OS deps,
# camoufox + the matching Playwright pin, the pre-fetched Firefox
# bundle at /opt/camoufox-cache, and collectorkit. Per-collector deps
# come from requirements.txt: duckdb (the DuckDB silver layer),
# zstandard (in-place .csv.zst compression at download time), requests
# (Binance price backfill), and pytest (in-container tests).
ARG WEALTHDB_IMAGE_TAG=latest
FROM wealthdb/base-camoufox:${WEALTHDB_IMAGE_TAG}

WORKDIR /app

# Install per-collector deps only — camoufox/playwright are already
# pre-installed in the base.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# Vanilla Playwright Firefox for login.py + download.py
# (headless, no stealth patches). Camoufox stays as the base for
# explore.py, so the discovery harness has stealth should
# cointracking ever add bot detection. login + download don't need
# it and run on plain Firefox.
# `install-deps` is a near no-op on this base image because the
# Camoufox Firefox needs the same shared libraries, but it's cheap
# insurance against drift.
RUN playwright install firefox && playwright install-deps firefox

# No source: every run and every test mounts the checkout over /app
# (HAS_APP_MOUNT in the wrapper), entrypoint.sh included, so the image
# holds only what the code runs on.

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes
# have host-correct ownership. Single-tenant single-origin
# (cointracking.info only); no sandbox-side concerns.

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
