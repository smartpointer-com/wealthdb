# Headed-browser (Camoufox) collector: builds on the shared
# base-camoufox image, which provides Firefox/Xvfb/x11vnc OS deps,
# camoufox + the matching Playwright pin, the pre-fetched Firefox
# bundle at /opt/camoufox-cache, and collectorkit. The only
# per-collector dep so far is pytest (for in-container tests once
# they exist); login.py / download.py will add their own deps as
# the implementation lands post-explore.
FROM wealthdb/base-camoufox:latest

WORKDIR /app

# Install per-collector deps only — camoufox/playwright are already
# pre-installed in the base.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# Vanilla Playwright Firefox for login.py + (future) download.py
# (headless, no stealth patches). Camoufox stays as the base for
# explore.py — defensive, in case cointracking ever adds bot
# detection we want stealth available for the discovery harness —
# but login + download don't need it and run on plain Firefox.
# `install-deps` is a near no-op on this base image because the
# Camoufox Firefox needs the same shared libraries, but it's cheap
# insurance against drift.
RUN playwright install firefox && playwright install-deps firefox

# Copy scripts last. Listed explicitly so accidentally-present host
# artefacts (.venv, real-credential JSONs, recorded Playwright
# traces) never make it into the image even if .dockerignore is
# incomplete.
COPY explore.py login.py download.py load.py prune.py recompress.py fetch_prices.py binance.py frankfurter.py /app/
COPY migrations /app/migrations
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes
# have host-correct ownership. Single-tenant single-origin
# (cointracking.info only); no sandbox-side concerns.

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
