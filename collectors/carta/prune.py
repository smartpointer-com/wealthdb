#!/usr/bin/env python3
"""
Prune non-complete dumps from the carta bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with carta's configuration. carta writes no bronze-resident
debug artefact — its browser diagnostics (HAR, Playwright trace, click
log) are captured externally by ``./carta explore`` under ``/debug``, not
in a run dir — so ``debug_subdirs`` is empty and only one category is ever
removed, across every timestamped run dir under ``--bronze-dir``:

* whole run dirs that are not complete dumps: ``run.json`` is missing (a
  pre-status walk crashed before finalising) or its ``status`` is anything
  other than ``"complete"`` (an ``"in-progress"`` marker from a crashed
  walk). ``load`` skips such dirs; ``prune`` reclaims them once quiescent.

Completeness signal: the ``run.json`` ``status`` field the walk now writes
(``"in-progress"`` at run-dir creation, atomically overwritten with
``"complete"`` at the end). Dumps that predate the ``status`` field carry a
full manifest with no ``status`` key — carta wrote ``run.json`` only once,
as the last step of a successful walk, so a statusless-but-readable
manifest is classified COMPLETE and kept. An unreadable or corrupt
``run.json`` is UNKNOWN and never deleted. ``--dry-run`` never creates a
run dir, so there is no dry-run shell to prune.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir so a
long backfill is protected. ``--dry-run`` prints the plan without removing
anything. The only paths ever deleted are whole non-complete run dirs;
complete dumps (their entities/, documents/ PDFs, bootstrap/ JSON, and the
manifest), the side-loaded ``<eid>-valuations.csv`` / ``<eid>-transactions.csv``
overrides and the silver DB (all at the bronze root, not under a run dir)
are never touched.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import prune

# carta writes no bronze-resident debug artefact (diagnostics are external,
# under /debug via `./carta explore`), so there is nothing to reclaim from a
# complete dump — prune's only category for carta is whole non-complete dumps.
DEBUG_SUBDIRS: tuple[str, ...] = ()


def _is_complete(run_dir, meta):
    # A statusless-but-readable run.json is a pre-`status` complete dump:
    # carta historically wrote run.json only once, as the last step of a
    # successful walk, so its presence means the walk finished. New walks
    # always carry a status key (in-progress → complete), so
    # status_classification resolves those before the legacy fallback runs.
    # A missing run.json (meta is None) means a crash before finalising →
    # NON_COMPLETE.
    return prune.status_classification(
        meta, run_dir=run_dir, legacy_complete=lambda rd, m: m is not None)


CONFIG = prune.PruneConfig(
    debug_subdirs=DEBUG_SUBDIRS,
    is_complete=_is_complete,
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


def validate_target(path, bronze_dir):
    """Back-compat shim for the local test suite: bind the shared
    validator to this collector's config."""
    return prune.validate_target(path, bronze_dir, CONFIG)


if __name__ == "__main__":
    sys.exit(main())
