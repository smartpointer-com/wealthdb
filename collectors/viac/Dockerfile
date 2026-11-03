# REST-only collector: build on the shared slim-Python base, which
# provides the Python/pip env settings and a pre-installed collectorkit.
ARG WEALTHDB_IMAGE_TAG=latest
FROM wealthdb/base-python:${WEALTHDB_IMAGE_TAG}

# Install collector-specific deps first so editing scripts doesn't
# bust the pip cache layer.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# No source: every run and every test mounts the checkout over /app
# (HAS_APP_MOUNT in the wrapper), entrypoint.sh included, so the image
# holds only what the code runs on.

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes
# have host-correct ownership. Single-tenant single-trusted-origin
# (app.viac.ch only); no sandbox-side concerns to address.

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
