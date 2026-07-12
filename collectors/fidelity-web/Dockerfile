# Headed-browser (Camoufox) collector: builds on the shared base-camoufox
# image, which provides Firefox/Xvfb/x11vnc OS deps, camoufox + the
# matching Playwright pin, the pre-fetched Firefox bundle at
# /opt/camoufox-cache, and collectorkit. Per-collector extras
# (pdfplumber for statement parsing, pytest for in-container tests)
# go into requirements.txt below.
FROM wealthdb/base-camoufox:latest

WORKDIR /app

# Install per-collector deps only — camoufox/playwright are already
# pre-installed in the base.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# Copy scripts last. Listed explicitly so accidentally-present
# host artefacts (.venv, real-credential JSONs, recorded Playwright
# traces) never make it into the image even if .dockerignore is
# incomplete. download.py is the one-shot CLI (login → walk →
# logout); load.py is the silver loader, prune.py the bronze
# debug-artefact/incomplete-dump cleaner, and recompress.py the
# one-time HTML/CSV → .zst backlog sweep (all also reachable
# host-side via the wrapper).
COPY download.py load.py prune.py recompress.py pdf_parsers.py pdf_parsers_supplied.py pdf_common.py /app/
COPY migrations /app/migrations
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes
# have host-correct ownership. download.py drives Camoufox (a
# stealth-patched Firefox fork) headed under the entrypoint's Xvfb —
# there is no Chromium here. Single-tenant, single-trusted-origin
# (we only ever load fidelity.com).

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
