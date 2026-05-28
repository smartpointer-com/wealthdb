# Slim Python base. The toolkit is REST-only — no browser, no
# Xvfb, no VNC — so we don't need Microsoft's Playwright image.
# Phase 1 discovery (now complete) used Playwright; the bronze
# fetch + silver loader replay VIAC's JSON API with pure httpx.
# Pin the python minor + the bookworm tag so reproducibility
# doesn't depend on Docker Hub's `latest` retag policy.
FROM python:3.12-slim-bookworm

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

WORKDIR /app

# Install Python deps first so editing scripts doesn't bust the
# pip cache layer.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# Copy scripts last. Listed explicitly so accidentally-present
# host artefacts (.venv, real-credential env files, bronze
# downloads) never make it into the image even if .dockerignore
# is incomplete.
COPY login.py download.py load.py viac_client.py /app/
COPY migrations /app/migrations
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes
# have host-correct ownership. Single-tenant single-trusted-origin
# (app.viac.ch only); no sandbox-side concerns to address.

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
