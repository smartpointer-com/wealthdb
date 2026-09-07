# Headed-browser (Camoufox) collector image for the American Express card
# UI (americanexpress.com: credit and charge cards). Builds on the shared
# base-camoufox image, which provides Firefox/Xvfb/x11vnc OS deps, camoufox
# + the matching Playwright pin, the pre-fetched Firefox bundle at
# /opt/camoufox-cache, and the collectorkit shared package.
#
# Camoufox from the start, not vanilla Playwright: it was picked ahead of the
# first capture on the US siblings' evidence (chase, fidelity-web,
# schwab-web all measured Akamai-class defense blocking vanilla engines
# outright at credential submit), and the captures then found the same stack
# here — cookie-borne Akamai Bot Manager, which Camoufox cleared with no
# visible challenge (DESIGN.md §A). The stack does escalate, but on the
# sign-in budget rather than the engine: too many sign-ins draw a captcha
# that is answered by hand over VNC (§K), which no other engine would help
# with. No fallback is installed.
FROM wealthdb/base-camoufox:latest

WORKDIR /app

# poppler's pdftotext extracts the statement-PDF text the loader parses for
# the transactions older than Amex's 24-month structured horizon
# (statement_parser.py).
RUN apt-get update \
    && apt-get install -y --no-install-recommends poppler-utils \
    && rm -rf /var/lib/apt/lists/*

# Install per-collector deps only — camoufox/playwright/collectorkit are
# already in the base.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# Copy scripts last, listed explicitly so accidentally-present host
# artefacts (.venv, recorded traces, downloaded statements) never make it
# into the image even if .dockerignore is incomplete.
#   explore.py      discovery harness
#   amexclient.py   the wire contract (pure: endpoints, classify, parse)
#   auth_dialog.py  the passcode challenge as a tested CLI dialog
#   login.py        the sign-in driver download.py signs in with, plus
#                   the `login --check` device-trust probe
#   download.py     the whole run: sign-in + REST fetch → bronze
#   statement_parser.py  statement PDF → transactions + balances
#   load.py         bronze → SQLite silver
#   prune.py        bronze reclaim
COPY explore.py amexclient.py auth_dialog.py login.py download.py \
     statement_parser.py load.py prune.py /app/
COPY migrations /app/migrations
COPY tests /app/tests
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes have
# host-correct ownership. Single-tenant single-origin
# (americanexpress.com only).

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
