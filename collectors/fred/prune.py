#!/usr/bin/env python3
"""
Prune non-complete dumps from the fred bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with fred's configuration. fred is a pure REST/JSON
collector: a complete run dir holds only ``load`` inputs — the
``run.json`` manifest and one ``<series_id>.json`` observations document
per fetched FX series — and it writes no bronze-resident debug artefacts
(no screenshots, traces, or DOM dumps). So ``debug_subdirs`` is empty and
prune's only effect is removing **whole run dirs that are not complete
dumps**, across every timestamped run dir under ``--bronze-dir``:

* ``run.json`` is missing (the walk crashed before writing any manifest),
  or its ``status`` is anything other than ``"complete"`` (an
  ``"in-progress"`` marker from a crashed or still-running walk). ``load``
  already skips such a dump; prune reclaims its disk. After pruning one,
  the next ``load --force`` rebuild reflects the removal.

Completeness signal: the ``run.json`` ``status`` field the walk now
writes (``"in-progress"`` at run-dir creation, atomically overwritten
with ``"complete"`` at the end). Dumps that predate the ``status`` field
carry a full manifest with no ``status`` key — fred historically wrote
``run.json`` exactly once, at the end of the walk, so a statusless-but-
readable manifest means the walk finished: it is classified COMPLETE and
kept. An unreadable or corrupt ``run.json`` is UNKNOWN and never deleted.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir so a
long ``--lookback all`` backfill (a sequence of series fetches) is
protected while it runs. ``--dry-run`` prints the plan without removing
anything. The only paths ever deleted are whole non-complete run dirs;
load inputs of complete dumps, the silver ``fred.db`` and any other
non-run entries at the bronze root are never touched.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import prune


def _is_complete(run_dir, meta):
    # A statusless-but-readable run.json is a pre-`status` complete dump:
    # fred historically wrote run.json only once, at the end of the walk,
    # so its presence means the walk finished. New walks always carry a
    # status key (in-progress → complete), so status_classification
    # resolves those before the legacy fallback is consulted. A missing
    # manifest (meta is None) stays NON_COMPLETE — a crashed download.
    return prune.status_classification(
        meta, run_dir=run_dir, legacy_complete=lambda rd, m: m is not None)


CONFIG = prune.PruneConfig(
    # fred writes NO bronze-resident debug artefacts, and every file in a
    # complete run dir (run.json + each <series_id>.json) is a load input,
    # so nothing inside a complete dump is ever pruned. Must stay empty:
    # adding run.json or any *.json here would delete a load input.
    debug_subdirs=(),
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
