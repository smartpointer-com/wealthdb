#!/usr/bin/env python3
"""
Prune non-complete dumps from the angellist bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with angellist's configuration. angellist writes NO
bronze-resident debug artefact — the browser-diagnostic capture (HAR,
Playwright trace, click log, saved blobs) lives in the separate
``explore`` verb, which writes OUTSIDE bronze — so ``debug_subdirs`` is
empty and the first prune category (debug artefacts from complete dumps)
never fires. That leaves the second category, which is real cleanup here:

* whole run dirs that are not complete dumps — a crashed or interrupted
  ``download`` whose ``run.json`` ``status`` is ``"in-progress"`` (the
  marker the walk drops at run-dir creation) rather than ``"complete"``,
  or which has no ``run.json`` at all (the walk crashed before minting
  it). ``load`` ingests any dir that carries a ``captures.jsonl``,
  regardless of manifest, so such a partial dir would otherwise keep
  seeding silver; after pruning it, the next ``load --force`` rebuild
  reflects the removal.

Completeness signal: the ``run.json`` ``status`` field the walk now
writes (``"in-progress"`` at run-dir creation, atomically overwritten
with ``"complete"`` at the end). Dumps that predate the ``status`` field
carry a manifest with no ``status`` key — ``download`` historically wrote
``run.json`` only once, as its final step (after viewer.json +
captures.jsonl), so a present-but-statusless manifest means the walk
finished: it is classified COMPLETE and kept whole. A crashed pre-change
walk left ``captures.jsonl`` with no ``run.json`` (``meta is None``) →
NON_COMPLETE. An unreadable or corrupt ``run.json`` is UNKNOWN and never
deleted.

The load inputs a complete dump holds — ``captures.jsonl`` (the primary
input) and ``run.json`` itself (stored into ``dump_runs.payload``) — are
never touched: with ``debug_subdirs`` empty, prune never removes anything
from a complete dump. The K-1 CSV/PDF documents live at
``<bronze>/angellist-documents/`` — a bronze-ROOT sibling of the run
dirs, not a run-dir child — and the silver DB at
``<bronze>/angellist.db``; neither matches the timestamped-run-dir slug,
so the engine (which iterates only ``bronze.iter_run_dirs``) never
touches them. Point ``--bronze-dir`` at the bronze ROOT, never at the
documents dir.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir so
a long backfill is protected. ``--dry-run`` prints the plan without
removing anything.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import prune


def _is_complete(run_dir, meta):
    # A statusless-but-readable run.json is a pre-`status` complete dump:
    # download.py historically wrote run.json only once, at the end (after
    # viewer.json + captures.jsonl), so its presence means the walk
    # finished. A crashed walk has no run.json (meta is None) → the
    # legacy fallback returns False → NON_COMPLETE. New walks always carry
    # a status key (in-progress → complete), which status_classification
    # resolves before the legacy fallback is consulted. Identical to
    # fidelity-web's predicate.
    return prune.status_classification(
        meta, run_dir=run_dir, legacy_complete=lambda rd, m: m is not None)


CONFIG = prune.PruneConfig(
    debug_subdirs=(),          # angellist writes no bronze-resident debug artefact
    is_complete=_is_complete,
    manifest_name="run.json",
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


def validate_target(path, bronze_dir):
    """Back-compat shim for the local test suite: bind the shared
    validator to this collector's config."""
    return prune.validate_target(path, bronze_dir, CONFIG)


if __name__ == "__main__":
    sys.exit(main())
