# Shared base for the camoufox-using collectors.
# Layers camoufox-pinned Playwright and a pre-fetched Firefox bundle
# onto base-playwright, which already carries Xvfb/x11vnc and the shared
# entrypoint bootstrap (/opt/entrypoint-lib.sh) that each collector's
# entrypoint.sh sources.
# Collectors FROM this skip their own camoufox setup; their
# requirements.txt only needs to add per-collector extras (pdfplumber,
# pypdfium2, pytest).
#
# Build context is shared/ (so collectorkit/ in the base layers is
# resolvable):
#   docker build -f images/base-camoufox.Dockerfile -t wealthdb/base-camoufox:latest .
#
# That command builds on whatever wealthdb/base-playwright:latest is
# already local (the WEALTHDB_IMAGE_TAG build argument picks another
# tag), so it must be preceded by a base-playwright rebuild —
# Xvfb/x11vnc, /opt/entrypoint-lib.sh and the baked-in collectorkit all
# live one layer down, and a stale parent yields a camoufox image
# whose collectors start without them or redact with an old collectorkit
# while their unit tests still pass. `make base-images` (or `make all`,
# or any `make build-<name>`) builds the pair in order and is the way in.
# It also passes the two inputs below that the command above leaves out:
# the browser mode and the local archive.
ARG WEALTHDB_IMAGE_TAG=latest
FROM wealthdb/base-playwright:${WEALTHDB_IMAGE_TAG} AS base

# Xvfb, x11vnc and the shared entrypoint bootstrap come from
# base-playwright — running a browser headed in a container is not a
# camoufox-specific need, so it lives one layer down.

# The browser build, camoufox-py, and Playwright are a matched set: the
# browser's juggler patches target a specific Playwright protocol, and
# base-playwright's bundled Playwright is much newer and crashes
# camoufox-launched Firefox — so Playwright is pinned back here for every
# collector built on this image. Bump CAMOUFOX_BUILD deliberately and re-validate the
# collectors against their sources' bot detection; camoufox-py's own
# fetch constraint spans Firefox majors, which is why the build is
# pinned explicitly below (see fetch-camoufox.py).
RUN pip install 'camoufox[geoip]>=0.4.11,<0.5' 'playwright==1.49.0'

# The directory of the local browser archive, as the named build context
# camoufox-archive (docker build --build-context camoufox-archive=DIR).
# Without one, this empty stage stands in, and the build downloads.
FROM scratch AS camoufox-archive

FROM base

# The pinned build, and the checksum its local archive must have
# (camoufox-archive.py), per CPU architecture. An empty checksum: no
# archive of that architecture is known, and one present fails the build.
ARG CAMOUFOX_BUILD=152.0.4-beta.26
ARG CAMOUFOX_SUM_ARM64=4f11de50b1ecc0cc4527a6d4af0b22f8ad33f494cd94d8b4049d62f830d440d4
ARG CAMOUFOX_SUM_AMD64=

# CAMOUFOX_BROWSER=bundle (the default) bakes in the browser; none
# leaves it out, for the images the unit tests run in, which never start
# one. The mode stays in the image's environment, and
# collectorkit.launch.require_browser stops a launch in a none image.
ARG CAMOUFOX_BROWSER=bundle
ARG TARGETARCH
ENV WEALTHDB_CAMOUFOX_BROWSER=${CAMOUFOX_BROWSER}

# Camoufox doesn't bundle the patched Firefox binary in the wheel, so
# the image carries the pinned build itself: self-contained,
# reproducible, and fast on first run. The build comes from the local
# archive camoufox-cache-<build>-lin.<arch>.tgz when the archive context
# holds it. The archive is checked against the pinned checksum and
# unpacked into the cache, and fetch-camoufox.py then keeps it. Without
# the archive, fetch-camoufox.py downloads the build. Either way it
# lands under /root/.cache/camoufox/. The runtime user has HOME=/tmp,
# so a naive setup would force a 700MB re-download on every container
# start. The tree moves to a world-readable /opt/camoufox-cache, and
# /opt/entrypoint-lib.sh links it into the runtime user's cache path.
# A link from root's cache path keeps a run as root on the same tree.
COPY images/fetch-camoufox.py images/camoufox-archive.py /tmp/
RUN --mount=type=bind,from=camoufox-archive,target=/archive \
    set -e; \
    case "$CAMOUFOX_BROWSER" in \
    none) ;; \
    bundle) \
        archive="/archive/camoufox-cache-$CAMOUFOX_BUILD-lin.$TARGETARCH.tgz"; \
        case "$TARGETARCH" in \
            arm64) sum="$CAMOUFOX_SUM_ARM64" ;; \
            amd64) sum="$CAMOUFOX_SUM_AMD64" ;; \
            *) sum= ;; \
        esac; \
        mkdir -p /root/.cache; \
        if [ -e "$archive" ]; then \
            python3 /tmp/camoufox-archive.py unpack "$archive" "$sum" /root/.cache/camoufox; \
        fi; \
        python3 /tmp/fetch-camoufox.py "$CAMOUFOX_BUILD"; \
        mv /root/.cache/camoufox /opt/camoufox-cache; \
        ln -s /opt/camoufox-cache /root/.cache/camoufox; \
        chmod -R go+rX /opt/camoufox-cache ;; \
    *) echo "base-camoufox: CAMOUFOX_BROWSER is bundle or none, not '$CAMOUFOX_BROWSER'" >&2; \
       exit 1 ;; \
    esac; \
    rm /tmp/fetch-camoufox.py /tmp/camoufox-archive.py
