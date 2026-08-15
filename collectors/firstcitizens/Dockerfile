# Headed-browser (Camoufox) discovery harness for the First Citizens Bank
# retail web UI (firstcitizens.com: checking + savings). Builds on the
# shared base-camoufox image, which provides Firefox/Xvfb/x11vnc OS deps,
# camoufox + the matching Playwright pin, the pre-fetched Firefox bundle
# at /opt/camoufox-cache, and the collectorkit shared package.
#
# Camoufox from the start, not vanilla Playwright: the bot-defense stack
# is unmapped, but every US sibling (chase, fidelity-web, schwab-web)
# measured Akamai-class defense blocking vanilla engines outright — the
# vanilla rungs are skipped rather than re-measured, and Camoufox costs
# nothing if First Citizens turns out lenient. No fallback engine is
# installed until a real capture shows one is needed.
FROM wealthdb/base-camoufox:latest

WORKDIR /app

# Install per-collector deps only — camoufox/playwright/collectorkit are
# already in the base.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# Copy scripts last, listed explicitly so accidentally-present host
# artefacts (.venv, recorded traces, downloaded statements) never make it
# into the image even if .dockerignore is incomplete.
#   explore.py      discovery harness
#   q2client.py     Q2 mobilews wire contract (pure: endpoints, classify, parse)
#   auth_dialog.py  the access-code 2FA challenge as a tested CLI dialog
#   login.py        interactive login (device registration) + shared authenticate()
#   download.py     unattended logon + REST fetch → bronze
#   load.py         bronze → SQLite silver (Phase 3 stub)
#   prune.py        bronze reclaim
COPY explore.py q2client.py auth_dialog.py login.py download.py \
     load.py prune.py /app/
COPY tests /app/tests
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes have
# host-correct ownership. Single-tenant single-origin
# (firstcitizens.com only).

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
