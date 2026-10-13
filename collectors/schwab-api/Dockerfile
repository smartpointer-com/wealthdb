# Hybrid collector: only the OAuth `login` runs in this image — it drives
# Schwab's authorize page in headed Camoufox (VNC) and exchanges the
# captured redirect for the token bundle. `download` and `load` run on
# the host venv (see the wrapper + the .host-venv marker). Builds on the
# shared base-camoufox image (Firefox/Xvfb/x11vnc + camoufox + the
# matching Playwright pin + collectorkit).
ARG WEALTHDB_IMAGE_TAG=latest
FROM wealthdb/base-camoufox:${WEALTHDB_IMAGE_TAG}

WORKDIR /app

# Per-collector deps only — camoufox/playwright/collectorkit are in the
# base. schwab-py drives the OAuth context + token exchange.
COPY requirements.txt /app/
RUN pip install -r requirements.txt

# Copy only the login surface, plus the token-file client that
# `login --check` shares with download.py. download.py / load.py / tests
# run on the host venv, so they're deliberately NOT baked into this
# image. Listed explicitly so stray host artefacts never leak in.
COPY login.py oauth_landmarks.py _tokenclient.py /app/
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Image default user is root. The wrapper runs the container with
# `--user $(id -u):$(id -g)` so files written to /secrets have
# host-correct ownership. Single-tenant single-origin (schwab.com /
# api.schwabapi.com); no sandbox-side concerns to address.

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["help"]
