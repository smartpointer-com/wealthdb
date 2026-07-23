# REST-only collector: build on the shared slim-Python base, which
# provides the Python/pip env settings and a pre-installed collectorkit.
FROM wealthdb/base-python:latest

# Install collector-specific deps first so editing scripts doesn't
# bust the pip cache layer.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# Copy scripts last. Listed explicitly so accidentally-present
# host artefacts (.venv, real-credential env files, bronze
# downloads) never make it into the image even if .dockerignore
# is incomplete.
COPY login.py download.py load.py prune.py viac_client.py pdf_parsers.py /app/
COPY migrations /app/migrations
COPY tests /app/tests
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes
# have host-correct ownership. Single-tenant single-trusted-origin
# (app.viac.ch only); no sandbox-side concerns to address.

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
