# Hybrid collector: only the OAuth `login` runs in this image — it drives
# Schwab's authorize page in headed Camoufox (over VNC for vnc-login) and
# exchanges the captured redirect for the token bundle. `download` and
# `load` run on the host venv (see the wrapper + the .host-venv marker).
# Builds on the shared base-camoufox image (Firefox/Xvfb/x11vnc + camoufox
# + the matching Playwright pin + collectorkit).
ARG WEALTHDB_IMAGE_TAG=latest
FROM wealthdb/base-camoufox:${WEALTHDB_IMAGE_TAG}

WORKDIR /app

# Per-collector deps only — camoufox/playwright/collectorkit are in the
# base. schwab-py drives the OAuth context + token exchange.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# No source: every run of the login container mounts the checkout
# over /app, entrypoint.sh included, so the image holds only what the
# code runs on.

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to /secrets have
# host-correct ownership. Single-tenant single-origin (schwab.com /
# api.schwabapi.com); no sandbox-side concerns to address.

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
