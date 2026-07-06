#!/usr/bin/env python3
"""
Prune debug artefacts and non-complete dumps from the schwab-web
bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with schwab-web's configuration. Two categories are
removed, across every timestamped run dir under ``--bronze-dir``:

* ``<run>/screenshots/`` — debug captures (the per-account
  tx-history landing-page HTML baseline). Written only when
  ``download`` runs with ``--debug``; never read by ``load``.
  Deleting them leaves silver byte-identical.

* whole run dirs that are not complete dumps: ``run.json`` is
  missing (the walk crashed before its first manifest write) or its
  ``status`` is anything other than ``"complete"`` (an
  ``"in-progress"`` marker from a crashed walk, or a ``"dry-run"``
  shell). ``load`` skips such dumps too; after pruning one, the next
  ``load --force`` rebuild reflects the removal.

Completeness signal: the ``run.json`` ``status`` field the walk now
writes (``"in-progress"`` at start, atomically overwritten with
``"complete"`` / ``"dry-run"`` at the end). Dumps that predate the
``status`` field carry a statusless manifest; schwab-web wrote
``run.json`` INCREMENTALLY (present from the first account onward),
so presence alone can't prove completion. The legacy fallback
therefore keys on ``dry_run``: a statusless real dump
(``dry_run=false``) keeps its load inputs, while a statusless
``--dry-run`` shell (``dry_run=true``) is a non-complete dump. A
crashed statusless non-dry dump keeps a partial manifest and is
conservatively classified COMPLETE (kept) — the safe direction
(never delete a load input); such legacy crash-dumps simply are not
reclaimed. An unreadable or corrupt ``run.json`` is UNKNOWN and
never deleted.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir
so a long backfill is protected. ``--dry-run`` prints the plan
without removing anything. The only paths ever deleted are
``<run>/screenshots/`` subtrees and whole non-complete run dirs;
load inputs of complete dumps (``statements/``, ``transactions/``,
``run.json``) and non-run entries at the bronze root (the silver DB)
are never touched.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import prune

SCREENSHOTS_DIR = "screenshots"


def _is_complete(run_dir, meta):
    # New walks always carry a `status` key (in-progress →
    # complete/dry-run), which status_classification resolves before
    # the legacy fallback is consulted. The fallback fires only for a
    # statusless (pre-`status`) manifest: schwab-web wrote run.json
    # incrementally, so mere presence is NOT proof of completion —
    # split a real dump (dry_run=false → keep its load inputs) from a
    # --dry-run shell (dry_run=true → prunable). A crashed statusless
    # non-dry dump keeps a partial manifest and is treated COMPLETE
    # (kept) — never deletes a load input.
    return prune.status_classification(
        run_dir=run_dir, meta=meta,
        legacy_complete=lambda rd, m: m is not None and not m.get(
            "dry_run", False),
    )


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
