"""collectorkit — shared utilities for wealthdb silver collectors.

Bundles the infrastructure every collector repeated by hand: bash-sourced
env-file loading + credential resolution, the SQLite migration runner and
connection setup, bronze-artifact writing, and CLI/logging helpers.
"""
from collectorkit import (  # noqa: F401
    bronze, cli, compress, docdedup, envfile, parse, prune, recompress,
    session, silver,
)

# `dedup` is intentionally NOT eagerly imported here: it is the module run
# as `python -m collectorkit.dedup` (the shared sweep), and eagerly
# importing a module that is also executed as __main__ triggers a runpy
# double-import RuntimeWarning. It stays importable on demand
# (`from collectorkit import dedup` / `import collectorkit.dedup`).
# `docdedup` has no __main__ (it runs inside download.py, not as a verb), so
# it is eagerly imported like the other library modules.
__all__ = ["bronze", "cli", "compress", "dedup", "docdedup", "envfile",
           "parse", "prune", "recompress", "session", "silver"]
