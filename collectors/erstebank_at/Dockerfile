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

# Copy scripts last, by name, so stray host artefacts (.venv, traces,
# captures, downloaded statements) never enter the image.
#   explore.py        discovery harness
#   george_client.py  George wire contract (pure: endpoints, parsers)
#   login.py          one-shot sign-in (push) + the REST helper
#   download.py       REST fetch → bronze
#   prune.py          bronze reclaim
COPY explore.py george_client.py login.py download.py prune.py /app/
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes have
# host-correct ownership.

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
