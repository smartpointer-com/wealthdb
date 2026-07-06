# Headed-browser (Camoufox) collector: builds on the shared base-camoufox
# image, which provides Firefox/Xvfb/x11vnc OS deps, camoufox + the
# matching Playwright pin, the pre-fetched Firefox bundle at
# /opt/camoufox-cache, and collectorkit. Per-collector extras
# (pypdfium2 for fast statement-PDF text extraction, pytest for
# in-container tests) go into requirements.txt below.
FROM wealthdb/base-camoufox:latest

WORKDIR /app

# Install per-collector deps only — camoufox/playwright are already
# pre-installed in the base.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# Copy scripts last. Listed explicitly so accidentally-present
# host artefacts (.venv, real-credential JSONs, recorded Playwright
# traces) never make it into the image even if .dockerignore is
# incomplete.
COPY login.py download.py landmarks.py pdf_parsers.py tax_form_parsers.py load.py prune.py /app/
COPY migrations /app/migrations
COPY tests /app/tests
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes
# have host-correct ownership. Single-tenant single-origin
# (schwab.com only); no sandbox-side concerns to address.

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
