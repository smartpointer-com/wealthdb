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

# Copy scripts last, by name, so stray host artefacts (.venv, traces,
# captures, downloaded statements) never enter the image.
#   explore.py      discovery harness
#   elba_client.py  Mein ELBA wire contract (pure: endpoints, bodies, parsers)
#   login.py        one-shot login (pushTAN) + shared authenticate()
#   download.py     REST fetch → bronze (history, balances, statements)
#   load.py         bronze → SQLite silver
#   listing_parser.py  supplied transaction listings → postings (pure)
#   stitch.py       listings + live history → one ledger (pure)
#   prune.py        bronze reclaim
COPY explore.py elba_client.py login.py download.py load.py listing_parser.py \
     stitch.py prune.py /app/
COPY migrations /app/migrations
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes have
# host-correct ownership. Single-tenant single-origin (raiffeisen.at
# only).

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
