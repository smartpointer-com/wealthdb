#!/usr/bin/env python3
"""Fetch an exact, pinned Camoufox browser build into the local cache.

`python -m camoufox fetch` installs the newest GitHub release satisfying
camoufox-py's own constraint — which bounds only the `beta.N` release
suffix (`>=beta.19, <1`), not the Firefox major underneath. A routine
image rebuild can therefore silently swap the browser under every
camoufox collector at once: one rebuild jumped 135.0.1-beta.24 →
152.0.4-beta.26, and the new browser's first start on an existing
profile crashed mid-migration, killing a login with nothing but a
masked TargetClosedError.

This fetches one reviewed build instead, so rebuilds are reproducible.
Bumping the pin is a deliberate act: change the build arg in
base-camoufox.Dockerfile and re-validate the collectors against their
sources' bot detection (the browser build, camoufox-py, and Playwright
versions are a matched set — see the Dockerfile comments).

Usage: fetch-camoufox.py <version>-<release>   e.g. 152.0.4-beta.26
"""
from __future__ import annotations

import sys

from camoufox.pkgman import OS_NAME, CamoufoxFetcher, Version


def main(pin: str) -> int:
    version, sep, release = pin.partition("-")
    if not sep or not version or not release:
        print(f"fetch-camoufox: bad pin {pin!r}; expected "
              f"<version>-<release>, e.g. 152.0.4-beta.26",
              file=sys.stderr)
        return 2

    # The fetcher's constructor picks the newest supported release; when
    # that differs from the pin, re-point it at the pinned asset before
    # installing. The asset URL shape matches CamoufoxFetcher.check_asset
    # (camoufox-<version>-<release>-<os>.<arch>.zip under the v<pin> tag).
    fetcher = CamoufoxFetcher()
    if (fetcher.version, fetcher.release) != (version, release):
        print(f"fetch-camoufox: newest supported is "
              f"{fetcher.version}-{fetcher.release}; installing pinned "
              f"{pin} instead", file=sys.stderr)
        fetcher._version_obj = Version(release=release, version=version)
        fetcher._url = (
            "https://github.com/daijro/camoufox/releases/download/"
            f"v{pin}/camoufox-{pin}-{OS_NAME}.{fetcher.arch}.zip"
        )
    fetcher.install()

    installed = Version.from_path()
    if installed.full_string != pin:
        print(f"fetch-camoufox: installed {installed.full_string}, "
              f"wanted {pin}", file=sys.stderr)
        return 1
    print(f"fetch-camoufox: installed {installed.full_string}")
    return 0


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    sys.exit(main(sys.argv[1]))
