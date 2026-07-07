#!/usr/bin/env python3
"""
Prune debug artefacts and non-complete dumps from the fidelity-web
bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with fidelity-web's configuration. Two categories are
removed, across every timestamped run dir under ``--bronze-dir``:

* ``<run>/screenshots/`` — debug captures (HTML DOM dumps, PNG
  screenshots, ``--explore`` DOM inventories). Written only when
  ``download`` runs with ``--debug`` / ``--explore``; never read by
  ``load``. Deleting them leaves silver byte-identical.

* whole run dirs that are not complete dumps: ``run.json`` is missing
  (the walk crashed before finalising) or its ``status`` is anything
  other than ``"complete"`` (an ``"in-progress"`` marker from a crashed
  walk, or a ``"dry-run"`` shell). ``load`` would otherwise keep
  re-ingesting whatever partial artefacts such a dir holds; after
  pruning one, the next ``load --force`` rebuild reflects the removal.

Completeness signal: the ``run.json`` ``status`` field the walk now
writes (``"in-progress"`` at start, atomically overwritten with
``"complete"`` / ``"dry-run"`` at the end). Dumps that predate the
``status`` field carry a full manifest with no ``status`` key — those
are pre-change complete dumps (the walk wrote ``run.json`` only once,
at the end), so a statusless-but-readable manifest is classified
COMPLETE and keeps its load inputs. An unreadable or corrupt
``run.json`` is UNKNOWN and never deleted.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir
so a long backfill is protected. ``--dry-run`` prints the plan without
removing anything. The only paths ever deleted are
``<run>/screenshots/`` subtrees and whole non-complete run dirs; load
inputs of complete dumps and non-run entries at the bronze root
(``manual/``, the silver DB) are never touched.

Classification is status-based and unaffected by bronze compression:
the HTML/CSV load inputs are now zstd-compressed (``balances.html.zst``,
``positions_*.csv.zst``, ``activity_*.csv.zst``; plain in
pre-compression dumps), but they are load inputs either way and stay
untouched. Only ``screenshots/`` is debug (``debug_subdirs``); PDFs and
``run.json`` are left raw. Converting the pre-compression backlog is the
separate manual ``recompress`` verb's job, not ``prune``'s.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import prune

SCREENSHOTS_DIR = "screenshots"


def _is_complete(run_dir, meta):
    # A statusless-but-readable run.json is a pre-`status` complete dump:
    # the walk historically wrote run.json only once, at the end, so its
    # presence means the walk finished. New walks always carry a status
    # key (in-progress → complete/dry-run), so status_classification
    # resolves those before the legacy fallback is consulted.
    return prune.status_classification(
        meta, run_dir=run_dir, legacy_complete=lambda rd, m: m is not None)


CONFIG = prune.PruneConfig(
    debug_subdirs=(SCREENSHOTS_DIR,),
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
