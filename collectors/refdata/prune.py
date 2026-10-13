#!/usr/bin/env python3
"""
Prune the refdata bronze tree.

Thin wrapper over :mod:`collectorkit.prune` with refdata's configuration.
It removes, across every timestamped run dir under ``--bronze-dir``:

* ``<run>/screenshots/``: the HTTP trace ``download --debug`` writes. Never
  read by ``load``.
* whole run dirs that are not complete dumps: no ``run.json``, or a
  ``status`` other than ``complete`` (a crashed or running download).

The raw provider files of a complete dump are ``load`` inputs and are
never touched; neither is the ``inbox/`` folder nor the silver DB at the
bronze root. ``--min-age-hours`` (default 1) protects a running backfill;
``--dry-run`` prints the plan.

Usage:
    prune.py [--bronze-dir DIR] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import debugcap, prune

CONFIG = prune.PruneConfig(
    debug_subdirs=(debugcap.SCREENSHOTS_DIR,),
    is_complete=prune.lenient_classification,
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


if __name__ == "__main__":
    sys.exit(main())
