#!/usr/bin/env python3
"""
Prune non-complete dumps from the carta bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with carta's configuration. Two categories are removed,
across every timestamped run dir under ``--bronze-dir``:

* whole run dirs that are not complete dumps: ``run.json`` is missing (a
  walk crashed before writing it) or its ``status`` is anything other than
  ``"complete"`` (an ``"in-progress"`` marker from a crashed walk). ``load`` skips such dirs; ``prune`` reclaims them once quiescent.
* ``screenshots/`` inside a complete dump: the landing-page DOM +
  screenshot ``download --debug`` writes. ``load`` never reads them, so
  reclaiming them cannot change silver. ``./carta explore``'s diagnostics
  (HAR, Playwright trace, click log) are a separate concern — they land
  outside bronze under ``/debug`` and prune never sees them.

Completeness signal: the ``run.json`` ``status`` field (``"in-progress"``
at run-dir creation, atomically overwritten with ``"complete"`` at the
end). A manifest with no ``status`` key counts as complete: a download
that writes no status writes its manifest only at the end. An unreadable
or corrupt ``run.json`` is UNKNOWN and never deleted.
``--dry-run`` never creates a run dir, so there is no dry-run shell to
prune.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir so a
long backfill is protected. ``--dry-run`` prints the plan without removing
anything. The only paths ever deleted are whole non-complete run dirs and
``screenshots/`` subdirs; a complete dump's load inputs (its entities/,
documents/ PDFs, bootstrap/ JSON, and the manifest), the side-loaded
``<eid>-valuations.csv`` / ``<eid>-transactions.csv`` overrides and the
silver DB (all at the bronze root, not under a run dir) are never touched.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import debugcap, prune

# `download --debug` writes the landing-page capture to <run>/screenshots/.
# Naming it here is what lets prune reclaim it from a dump that is otherwise
# worth keeping.
DEBUG_SUBDIRS: tuple[str, ...] = (debugcap.SCREENSHOTS_DIR,)


CONFIG = prune.PruneConfig(
    debug_subdirs=DEBUG_SUBDIRS,
    is_complete=prune.lenient_classification,
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


if __name__ == "__main__":
    sys.exit(main())
