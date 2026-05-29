# Headless-browser collector: build on the shared Playwright base
# (collectorkit pre-installed; the Playwright image tag is pinned there
# and must stay in lockstep with the `playwright` pin in requirements.txt).
FROM wealthdb/base-playwright:latest

# Install collector-specific deps first so editing scripts doesn't bust
# the pip cache layer.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# Copy scripts and migrations last. Listed explicitly so accidentally-
# present host artefacts (.venv, real-credential JSONs, recorded
# Playwright traces) never make it into the image even if
# .dockerignore is incomplete.
COPY login.py download.py load.py landmarks.py /app/
COPY migrations /app/migrations
COPY tests /app/tests
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
