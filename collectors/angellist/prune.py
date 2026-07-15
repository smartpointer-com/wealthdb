#!/usr/bin/env python3
"""
Prune non-complete dumps from the angellist bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with angellist's configuration. Two categories are removed:

* ``screenshots/`` from a complete dump — the per-route DOM + screenshot
  captures ``download --debug`` writes. ``load`` reads only
  ``captures.jsonl`` and ``run.json``, so reclaiming them cannot change
  silver. (The ``explore`` verb's diagnostics — HAR, Playwright trace,
  click log, saved blobs — are a separate concern: they land OUTSIDE
  bronze, and prune never sees them.)
* whole run dirs that are not complete dumps — a crashed or interrupted
  ``download`` whose ``run.json`` ``status`` is ``"in-progress"`` (the
  marker the walk drops at run-dir creation) rather than ``"complete"``,
  or which has no ``run.json`` at all (the walk crashed before minting
  it, or ``--debug`` created the dir for captures and the walk then found
  no session to capture). ``load`` ingests any dir that carries a
  ``captures.jsonl``, regardless of manifest, so such a partial dir would
  otherwise keep seeding silver; after pruning it, the next
  ``load --force`` rebuild reflects the removal.

Completeness signal: the ``run.json`` ``status`` field (``"in-progress"``
at run-dir creation, atomically overwritten with ``"complete"`` at the
end). A manifest with no ``status`` key predates the field and is
classified COMPLETE: such a dump only ever got a ``run.json`` as the
walk's final step, so its presence alone means the walk finished. No
``run.json`` at all (``meta is None``) → NON_COMPLETE. An unreadable or
corrupt ``run.json`` is UNKNOWN and never deleted.

The load inputs a complete dump holds — ``captures.jsonl`` (the primary
input) and ``run.json`` itself (stored into ``dump_runs.payload``) — are
never touched: ``screenshots/`` is the only thing prune removes from a
complete dump. The K-1 CSV/PDF documents live at
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

from collectorkit import debugcap, prune


def _is_complete(run_dir, meta):
    # Statusless-but-readable run.json → COMPLETE (see module docstring);
    # no run.json at all → NON_COMPLETE. A status key, when present,
    # resolves in status_classification before this fallback is consulted.
    return prune.status_classification(
        meta, run_dir=run_dir, legacy_complete=lambda rd, m: m is not None)


CONFIG = prune.PruneConfig(
    # Where `download --debug` puts its per-route captures.
    debug_subdirs=(debugcap.SCREENSHOTS_DIR,),
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
