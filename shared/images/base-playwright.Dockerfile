# Shared base for headless-browser (Playwright) collectors: Microsoft's
# version-matched Playwright Python image with collectorkit pre-installed.
# Bump the tag in lockstep with the collectors' pinned `playwright` pip
# version. Build context is shared/:
#   docker build -f images/base-playwright.Dockerfile -t wealthdb/base-playwright:latest .
FROM mcr.microsoft.com/playwright/python:v1.59.0-noble

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

WORKDIR /app

# Bake in the shared collector library.
COPY collectorkit /opt/collectorkit
RUN pip install /opt/collectorkit
