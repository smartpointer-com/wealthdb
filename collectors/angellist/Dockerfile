# Headed-browser (Camoufox) collector for the AngelList
# Investor Portal. Builds on the shared base-camoufox image, which
# provides Firefox/Xvfb/x11vnc OS deps, camoufox + the matching
# Playwright pin, the pre-fetched Firefox bundle at /opt/camoufox-cache,
# and the collectorkit shared package. Per-collector deps (pytest for
# now) come from requirements.txt.
FROM wealthdb/base-camoufox:latest

WORKDIR /app

# Install per-collector deps only — camoufox/playwright/collectorkit are
# already in the base.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# Vanilla Playwright Firefox, kept as a possible fallback engine (headless,
# no stealth). `download.py` uses Camoufox (the Investor Portal flags the
# automation fingerprint — see DESIGN.md "Why download is browser-based"),
# so this is currently unused; installing it is cheap insurance and mirrors
# the cointracking image.
RUN playwright install firefox && playwright install-deps firefox

# Stock Mozilla Firefox (arm64/amd64 from Mozilla's own APT repo) for the
# BYO-session login: a genuine, un-instrumented browser, driven by hand
# over VNC to clear AngelList's invisible anti-bot login challenge, which
# flags the Camoufox/Playwright automation stack (see DESIGN.md). We then
# read its plaintext cookies.sqlite to lift the session. libdbus-glib-1-2
# is the one runtime lib Firefox needs that the Playwright/Camoufox deps
# don't already pull in. poppler-utils provides `pdftotext`, used by
# statements.py to parse the fund financial-report PDFs (load step).
RUN apt-get update \
 && apt-get install -y --no-install-recommends wget ca-certificates \
 && install -d -m 0755 /etc/apt/keyrings \
 && wget -qO /etc/apt/keyrings/packages.mozilla.org.asc \
      https://packages.mozilla.org/apt/repo-signing-key.gpg \
 && echo "deb [signed-by=/etc/apt/keyrings/packages.mozilla.org.asc] https://packages.mozilla.org/apt mozilla main" \
      > /etc/apt/sources.list.d/mozilla.list \
 && printf 'Package: *\nPin: origin packages.mozilla.org\nPin-Priority: 1000\n' \
      > /etc/apt/preferences.d/mozilla \
 && apt-get update \
 && apt-get install -y --no-install-recommends firefox libdbus-glib-1-2 poppler-utils \
 && rm -rf /var/lib/apt/lists/*

# Copy scripts last. Listed explicitly so accidentally-present host
# artefacts (.venv, real-credential JSONs, recorded traces, sample
# Investor-Portal HTML) never make it into the image even if
# .dockerignore is incomplete.
COPY explore.py download.py load.py extract_cookies.py fxprofile.py prune.py /app/
COPY migrations /app/migrations
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to mounted volumes have
# host-correct ownership. Single-tenant single-origin (angellist.com
# only); no sandbox-side concerns.

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
