#!/usr/bin/env python3
"""
Prune non-complete dumps from the swissquote bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with swissquote's configuration. Swissquote writes no
bronze-resident debug artefact — its screenshots, HTML/DOM dumps, the
SmartL3 feedback log, and Playwright trace bundles are all opt-in
(``--screenshot-dir`` / ``--trace``) and land OUTSIDE bronze, in the
``/debug`` mount. So ``debug_subdirs`` is empty and only the second
prune category applies:

* whole run dirs that are not complete dumps: run.json is missing (a
  hard-kill / OOM / power-loss that bypassed download.py's
  crash-cleanup trap before it could finalise) or its ``status`` is
  anything other than ``"complete"`` (an ``"in-progress"`` marker from
  a crashed walk). ``load`` skips such dirs, so pruning one only
  reclaims disk; after removal the next ``load --force`` rebuild
  reflects it.

Completeness signal: the ``run.json`` ``status`` field the walk now
writes (``"in-progress"`` at run-dir creation, atomically overwritten
with ``"complete"`` at the end). Dumps that predate the ``status``
field carry a full manifest with no ``status`` key — those are
pre-change complete dumps (download.py wrote run.json only once, at
the very end), so a statusless-but-readable manifest is classified
COMPLETE and its load inputs are kept. An unreadable or corrupt
``run.json`` is UNKNOWN and never deleted.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir
so a long backfill is protected. ``--dry-run`` prints the plan without
removing anything. The only paths ever deleted are whole non-complete
run dirs; the load inputs of complete dumps (run.json, accounts.json,
positions.xls, position_details.json, list_of_assets.xls, the
transactions CSVs, and documents/) and the non-run entries at the
bronze root (``manual/``, the silver ``swissquote.db``) are never
touched.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import prune


def _is_complete(run_dir, meta):
    # A statusless-but-readable run.json is a pre-`status` complete
    # dump: download.py historically wrote run.json only once, at the
    # end, so its presence means the walk finished. New walks always
    # carry a status key (in-progress -> complete), which
    # status_classification resolves before the legacy fallback is
    # consulted. Identical shape to fidelity-web because both use
    # run.json-presence as the legacy completeness signal.
    return prune.status_classification(
        meta, run_dir=run_dir, legacy_complete=lambda rd, m: m is not None)


CONFIG = prune.PruneConfig(
    debug_subdirs=(),           # nothing debug-y ever lands in bronze
    is_complete=_is_complete,   # manifest_name defaults to 'run.json'
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


def validate_target(path, bronze_dir):
    """Back-compat shim for the local test suite: bind the shared
    validator to this collector's config."""
    return prune.validate_target(path, bronze_dir, CONFIG)


if __name__ == "__main__":
    sys.exit(main())
