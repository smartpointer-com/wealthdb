#!/usr/bin/env python3
"""
Prune non-complete dumps from the fred bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with fred's configuration. Two categories are removed,
across every timestamped run dir under ``--bronze-dir``:

* ``<run>/screenshots/`` — the HTTP trace ``download --debug`` writes
  (``http-trace.jsonl``: one metadata line per FRED request). Written
  only under ``--debug``; never read by ``load``, so deleting it leaves
  silver byte-identical.

* whole run dirs that are not complete dumps: ``run.json`` is missing
  (the walk crashed before writing any manifest), or its ``status`` is
  anything other than ``"complete"`` (an ``"in-progress"`` marker from a
  crashed or still-running walk). ``load`` skips such dirs, so removing
  one never changes silver.

Every other file in a complete run dir is a ``load`` input — the
``run.json`` manifest and one ``<series_id>.json`` observations document
per fetched FX series — and is never touched.

Completeness signal: the ``run.json`` ``status`` field (``"in-progress"``
at run-dir creation, atomically overwritten with ``"complete"`` at the
end). A readable manifest with no ``status`` key is COMPLETE and kept —
such a dump only ever got its manifest at the end of the walk, so its
presence proves the walk finished. An unreadable or corrupt ``run.json``
is UNKNOWN and never deleted.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir so a
long ``--lookback all`` backfill (a sequence of series fetches) is
protected while it runs. ``--dry-run`` prints the plan without removing
anything. The only paths ever deleted are ``<run>/screenshots/`` subtrees
and whole non-complete run dirs; load inputs of complete dumps, the silver
``fred.db`` and any other non-run entries at the bronze root are never
touched.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import debugcap, prune


CONFIG = prune.PruneConfig(
    # The only debug artefact fred writes: `download --debug`'s HTTP trace,
    # which debugcap lands under this subdir fleet-wide. Everything else in
    # a complete run dir (run.json + each <series_id>.json) is a load input —
    # adding either here would delete one.
    debug_subdirs=(debugcap.SCREENSHOTS_DIR,),
    is_complete=prune.lenient_classification,
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


if __name__ == "__main__":
    sys.exit(main())
