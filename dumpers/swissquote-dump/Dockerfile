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

# Copy scripts and migrations last. Listed explicitly so accidentally-
# present host artefacts (.venv, real-credential JSONs, recorded
# Playwright traces) never make it into the image even if
# .dockerignore is incomplete.
COPY login.py download.py load.py landmarks.py /app/
COPY migrations /app/migrations
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes
# have host-correct ownership; Chromium's user-namespace sandbox is
# disabled at launch time in code with `--no-sandbox` instead. This
# is acceptable in a single-tenant, single-trusted-origin scenario
# (we only ever load swissquote.ch).

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
