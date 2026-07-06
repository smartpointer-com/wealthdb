#!/usr/bin/env python3
"""
Prune non-complete dumps from the cointracking bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with cointracking's configuration. Across every
timestamped run dir under ``--bronze-dir``, ``prune`` removes:

* whole run dirs that are not complete dumps: ``run.json`` is missing
  (the walk crashed before creating the marker) or its ``status`` is
  anything other than ``"complete"`` (an ``"in-progress"`` marker left
  by a crashed walk). ``load`` already skips such a dir, but its partial
  ``cu_<id>/`` CSVs otherwise linger on disk; after pruning one, the
  next ``load --force`` rebuild reflects the removal.

cointracking's download writes **no** bronze-resident debug artefacts —
only ``run.json`` and one ``cu_<id>/{trades,balance,overview}.csv`` set
per portfolio, all of which are ``load`` inputs — so ``debug_subdirs``
is empty and only the whole-non-complete-dump category applies. (The
discovery harness ``explore.py`` writes traces to an external ``/debug``
mount, never into a bronze run dir; a ``download --debug`` opt-in
reserves the same discipline for any future capture.)

Completeness signal: the ``run.json`` ``status`` field the walk now
writes (``"in-progress"`` at run-dir creation, atomically overwritten
with ``"complete"`` at the end). ``--dry-run`` materialises no run dir,
so no ``"dry-run"`` shell is ever produced. Dumps that predate the
``status`` field carry a full manifest with no ``status`` key — the walk
historically wrote ``run.json`` only once, at the very end, so a
statusless-but-readable manifest means the dump finished: it is
classified COMPLETE and kept. An unreadable or corrupt ``run.json`` is
UNKNOWN and never deleted.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir so
a long multi-portfolio walk is protected. ``--dry-run`` prints the plan
without removing anything. The only paths ever deleted are whole
non-complete run dirs; complete dumps' load inputs
(``run.json`` + ``cu_<id>/{trades,balance,overview}.csv``) and non-run
entries at the bronze root (``known_portfolios.json``, the silver
``cointracking.duckdb``) are never touched.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import prune


def _is_complete(run_dir, meta):
    # A statusless-but-readable run.json is a pre-`status` complete
    # dump: the walk historically wrote run.json only once, at the end,
    # so its presence means the walk finished. New walks always carry a
    # status key (in-progress → complete), which status_classification
    # resolves before this legacy fallback is consulted. A crashed new
    # walk leaves status="in-progress" (NON_COMPLETE); a crash before
    # the marker leaves no run.json at all (meta is None → NON_COMPLETE).
    return prune.status_classification(
        meta, run_dir=run_dir, legacy_complete=lambda rd, m: m is not None)


CONFIG = prune.PruneConfig(
    # cointracking writes no bronze-resident debug artefact — every file
    # in a run dir is a load input — so there is nothing to reclaim from
    # a complete dump; only whole non-complete dumps are prunable.
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
