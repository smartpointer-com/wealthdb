# Headless-browser collector: build on the shared Playwright base
# (collectorkit pre-installed; the Playwright image tag is pinned there
# and must stay in lockstep with the `playwright` pin in requirements.txt).
FROM wealthdb/base-playwright:latest

# Install collector-specific deps first so editing scripts doesn't bust
# the pip cache layer.
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
