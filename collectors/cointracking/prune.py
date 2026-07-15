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
* ``screenshots/`` inside a complete dump — the DOM + screenshot
  captures ``download --debug`` writes for the portfolio-discovery page
  and for any portfolio that failed. Everything else in a run dir is a
  ``load`` input (``run.json`` plus one
  ``cu_<id>/{trades,balance,overview}.csv.zst`` set per portfolio; plain
  ``.csv`` in pre-compression dumps), so ``screenshots/`` is the only
  thing a complete dump ever gives up, and losing it cannot change
  silver. (The discovery harness ``explore.py`` writes its traces to an
  external ``/debug`` mount, never into a bronze run dir.)

Completeness signal: the ``run.json`` ``status`` field (``"in-progress"``
at run-dir creation, atomically overwritten with ``"complete"`` at the
end). ``--dry-run`` materialises no run dir, so no ``"dry-run"`` shell is
ever produced. A manifest with no ``status`` key predates the field and
is classified COMPLETE: such a dump only ever got a ``run.json`` at the
very end, so its presence alone means the walk finished. An unreadable or
corrupt ``run.json`` is UNKNOWN and never deleted.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir so
a long multi-portfolio walk is protected. ``--dry-run`` prints the plan
without removing anything. The only paths ever deleted are whole
non-complete run dirs and ``screenshots/`` subdirs; complete dumps' load
inputs (``run.json`` + ``cu_<id>/{trades,balance,overview}.csv[.zst]``)
and non-run entries at the bronze root (``known_portfolios.json``, the
silver ``cointracking.duckdb``) are never touched.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import debugcap, prune


def _is_complete(run_dir, meta):
    # Statusless-but-readable run.json → COMPLETE (see module docstring).
    # A status key, when present, resolves in status_classification before
    # this fallback is consulted: a crashed walk leaves
    # status="in-progress" (NON_COMPLETE); a crash before the marker
    # leaves no run.json at all (meta is None → NON_COMPLETE).
    return prune.status_classification(
        meta, run_dir=run_dir, legacy_complete=lambda rd, m: m is not None)


CONFIG = prune.PruneConfig(
    # The one non-load-input a run dir can hold: where `download --debug`
    # puts its captures. Every other file is a load input.
    debug_subdirs=(debugcap.SCREENSHOTS_DIR,),
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
