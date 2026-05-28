# Pin to a specific Playwright Python image tag for reproducibility.
# Microsoft publishes one image per Playwright version; the Python
# bindings and Chromium binaries inside are version-matched. Bump this
# tag when upgrading Playwright; do NOT mix it with a different
# `playwright` pip version in requirements.txt.
FROM mcr.microsoft.com/playwright/python:v1.59.0-noble

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

WORKDIR /app

# Install Python deps first so editing scripts doesn't bust the pip
# cache layer.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# Copy scripts last. Listed explicitly so accidentally-present
# host artefacts (.venv, real-credential JSONs, recorded Playwright
# traces) never make it into the image even if .dockerignore is
# incomplete.
#
# Only login.py + landmarks.py exist today; download.py / load.py /
# migrations/ will be added in subsequent commits. Until then the
# entrypoint dispatches `download` / `load` subcommands to scripts
# that do not exist, which is fine — running them produces a clear
# "no such file" error rather than a silent failure.
COPY login.py download.py load.py landmarks.py pdf_parsers.py /app/
COPY migrations /app/migrations
COPY tests /app/tests
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes
# have host-correct ownership; Chromium's user-namespace sandbox is
# disabled at launch time in code with `--no-sandbox` instead. This
# is acceptable in a single-tenant, single-trusted-origin scenario
# (we only ever load ubs.com).

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
