#!/usr/bin/env python3
"""
Prune debug artefacts and non-complete dumps from the fidelity-web
bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with fidelity-web's configuration. Three categories are
removed:

* ``<run>/screenshots/`` — debug captures (HTML DOM dumps, PNG
  screenshots, ``--explore`` DOM inventories). Written only when
  ``download`` runs with ``--debug`` / ``--explore``; never read by
  ``load``. Deleting them leaves silver byte-identical.

* whole run dirs that are not complete dumps: ``run.json`` is missing
  (the walk crashed before its first manifest write) or its ``status``
  is anything other than ``"complete"`` (an ``"in-progress"`` marker
  from a crashed walk, or a ``"dry-run"`` shell). ``load`` skips such dirs, so
  removing one never changes silver.

* aged entries of the host ``/debug`` cache (``--debug-dir``, which the
  wrapper passes): the ``--screenshot-dir`` / ``--trace`` output.

Completeness signal: the ``run.json`` ``status`` field
(``"in-progress"`` at start, atomically overwritten with
``"complete"`` / ``"dry-run"`` at the end). A manifest with no
``status`` key predates the field and is classified COMPLETE, keeping
its load inputs: such a dump only ever got a ``run.json`` at the end,
so its presence alone means the walk finished. An unreadable or
corrupt ``run.json`` is UNKNOWN and never deleted.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir
so a long backfill is protected. ``--dry-run`` prints the plan without
removing anything. The only paths ever deleted are
``<run>/screenshots/`` subtrees and whole non-complete run dirs; load
inputs of complete dumps and non-run entries at the bronze root
(``manual/``, the silver DB) are never touched.

Classification is status-based and unaffected by bronze compression:
the HTML/CSV load inputs are zstd-compressed (``balances.html.zst``,
``positions_*.csv.zst``, ``activity_*.csv.zst``; plain in
pre-compression dumps), but they are load inputs either way and stay
untouched. Only ``screenshots/`` is debug (``debug_subdirs``); PDFs and
``run.json`` are left raw. Converting the pre-compression backlog is the
separate manual ``recompress`` verb's job, not ``prune``'s.

Usage:
    prune.py [--bronze-dir /data] [--debug-dir DIR] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import debugcap, prune


CONFIG = prune.PruneConfig(
    debug_subdirs=(debugcap.SCREENSHOTS_DIR,),
    # A status key decides; a statusless but readable run.json is
    # COMPLETE (see the module docstring).
    is_complete=prune.lenient_classification,
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


if __name__ == "__main__":
    sys.exit(main())
