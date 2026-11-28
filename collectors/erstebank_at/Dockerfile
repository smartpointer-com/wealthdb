# Headed-browser (Camoufox) collector for George, the retail e-banking
# portal of Erste Bank und Sparkassen in Austria (george.sparkasse.at:
# checking accounts + credit cards). Builds on the shared base-camoufox
# image, which provides Firefox/Xvfb/x11vnc OS deps, camoufox + the
# matching Playwright pin, the pre-fetched Firefox bundle at
# /opt/camoufox-cache, and the collectorkit shared package.
#
# Camoufox signs in; the sign-in page carries a first-party sensor, the data
# API does not (DESIGN.md §A).
ARG WEALTHDB_IMAGE_TAG=latest
FROM wealthdb/base-camoufox:${WEALTHDB_IMAGE_TAG}

WORKDIR /app

# Install per-collector deps only — camoufox/playwright/collectorkit are
# already in the base.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# No source: every run and every test mounts the checkout over /app
# (HAS_APP_MOUNT in the wrapper), entrypoint.sh included, so the image
# holds only what the code runs on.

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes have
# host-correct ownership.

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
