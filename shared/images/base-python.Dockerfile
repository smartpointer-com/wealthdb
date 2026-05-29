# Shared base image for REST-only (no-browser) collectors: slim Python
# with collectorkit pre-installed, so collectors that `FROM` it can
# `import collectorkit` with no per-collector plumbing.
#
# Build context is shared/ (so collectorkit/ is reachable):
#   docker build -f images/base-python.Dockerfile -t wealthdb/base-python:latest .
FROM python:3.12-slim-bookworm

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

WORKDIR /app

# Bake in the shared collector library.
COPY collectorkit /opt/collectorkit
RUN pip install /opt/collectorkit
