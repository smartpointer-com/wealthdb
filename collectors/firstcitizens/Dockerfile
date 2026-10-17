# Headed-browser (Camoufox) collector for the First Citizens Bank retail
# deposit accounts (checking + savings). Builds on the shared
# base-camoufox image, which provides Firefox/Xvfb/x11vnc OS deps,
# camoufox + the matching Playwright pin, the pre-fetched Firefox bundle
# at /opt/camoufox-cache, and the collectorkit shared package.
#
# Camoufox, not vanilla Playwright: Akamai Bot Manager gates the logon
# (DESIGN.md §3), as it does for every US sibling (chase, fidelity-web,
# schwab-web). No fallback engine is installed.
ARG WEALTHDB_IMAGE_TAG=latest
FROM wealthdb/base-camoufox:${WEALTHDB_IMAGE_TAG}

WORKDIR /app

# Install per-collector deps only — camoufox/playwright/collectorkit are
# already in the base.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# Copy scripts last, by name, so stray host artefacts (.venv, traces,
# captures, downloaded statements) never enter the image.
#   explore.py      discovery harness
#   q2client.py     Q2 mobilews wire contract (pure: endpoints, classify, parse)
#   auth_dialog.py  the access-code 2FA challenge as a tested CLI dialog
#   login.py        interactive login (device registration) + shared authenticate()
#   download.py     unattended logon + REST fetch → bronze
#   load.py         bronze → SQLite silver
#   prune.py        bronze reclaim
COPY explore.py q2client.py auth_dialog.py login.py download.py \
     load.py prune.py /app/
COPY migrations /app/migrations
COPY tests /app/tests
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes have
# host-correct ownership. Single-tenant single-origin
# (firstcitizens.com only).

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
