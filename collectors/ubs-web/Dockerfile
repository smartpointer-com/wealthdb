# Headless-browser collector: build on the shared Playwright base
# (collectorkit pre-installed; the Playwright image tag is pinned there,
# and `make update` moves it and the `playwright` pin in requirements.txt
# together).
ARG WEALTHDB_IMAGE_TAG=latest
FROM wealthdb/base-playwright:${WEALTHDB_IMAGE_TAG}

COPY requirements.txt /app/
RUN pip install -r requirements.txt

# No source: every run and every test mounts the checkout over /app,
# entrypoint.sh included, so the image holds only what the code runs on.

# Default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes
# have host-correct ownership; Chromium's user-namespace sandbox is
# disabled at launch time in code with `--no-sandbox` instead. This
# is acceptable in a single-tenant, single-trusted-origin scenario
# (the collector loads only ubs.com).

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
