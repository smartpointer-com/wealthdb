"""collectorkit — shared utilities for wealthdb silver collectors.

Bundles the infrastructure every collector repeated by hand: bash-sourced
env-file loading + credential resolution, the SQLite migration runner and
connection setup, bronze-artifact writing, and CLI/logging helpers.
"""
from collectorkit import (  # noqa: F401
    bronze, cli, compress, envfile, parse, prune, recompress, session, silver,
)

# `dedup` is intentionally NOT eagerly imported here: it is the module run
# as `python -m collectorkit.dedup` (the shared sweep), and eagerly
# importing a module that is also executed as __main__ triggers a runpy
# double-import RuntimeWarning. It stays importable on demand
# (`from collectorkit import dedup` / `import collectorkit.dedup`).
__all__ = ["bronze", "cli", "compress", "dedup", "envfile", "parse", "prune",
           "recompress", "session", "silver"]
