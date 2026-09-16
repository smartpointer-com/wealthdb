"""collectorkit — shared utilities for wealthdb silver collectors.

Bundles the infrastructure every collector repeated by hand: bash-sourced
env-file loading + credential resolution, the SQLite migration runner and
connection setup, bronze-artifact writing, browser launch hardening, and
CLI/logging helpers.
"""
import os as _os

# Everything a collector writes is the source's own financial record —
# bronze blobs, the silver DB and its sidecars, the parse caches — so the
# whole tree is created owner-only rather than at the login shell's umask.
#
# Set here, at the package import, because it is the only hook every
# collector actually passes through: there is no shared entrypoint (four
# collectors are host-venv with no Dockerfile at all), a `umask` in the
# wrappers would not cross into a container, and the alternative is a chmod
# beside each of the ~90 write sites — which is what produced a guarantee
# covering the silver DBs and nothing around them. A library mutating
# process state on import is a real cost; it buys every future write site
# too, including files written by libraries the collectors never call
# through (a browser saving a download, DuckDB creating a sidecar), which
# no per-call-site mode argument can reach.
#
# Creation-time only: an explicit chmod still wins, and `silver.own_only`
# stays as the repair path for files already on disk at a wider mode.
_os.umask(0o077)

from collectorkit import (  # noqa: E402,F401
    bronze, cli, compress, docdedup, envfile, launch, parse, prune,
    recompress, session, silver,
)

# `dedup` is intentionally NOT eagerly imported here: it is the module run
# as `python -m collectorkit.dedup` (the shared sweep), and eagerly
# importing a module that is also executed as __main__ triggers a runpy
# double-import RuntimeWarning. It stays importable on demand
# (`from collectorkit import dedup` / `import collectorkit.dedup`).
# `docdedup` has no __main__ (it runs inside download.py, not as a verb), so
# it is eagerly imported like the other library modules.
__all__ = ["bronze", "cli", "compress", "dedup", "docdedup", "envfile",
           "launch", "parse", "prune", "recompress", "session", "silver"]
