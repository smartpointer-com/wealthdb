#!/usr/bin/env python3
"""
Prune non-complete dumps from the swissquote bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with swissquote's configuration. Two categories are
removed:

* ``screenshots/`` inside a complete dump — the landmark DOM +
  screenshot captures ``download --debug`` writes. ``load`` never reads
  them, so reclaiming them cannot change silver. (The other
  diagnostics — landmark screenshots, the MFA page's API-call list
  ``mfa_api_calls.txt``, and Playwright trace bundles — stay opt-in
  under ``--screenshot-dir`` / ``--trace`` and land OUTSIDE bronze in
  the ``/debug`` mount, whose stale entries prune reclaims through
  ``--debug-dir``.)
* whole run dirs that are not complete dumps: run.json is missing (a
  hard-kill / OOM / power-loss that bypassed download.py's
  crash-cleanup trap before it could finalise) or its ``status`` is
  anything other than ``"complete"`` (an ``"in-progress"`` marker from
  a crashed walk). ``load`` skips such dirs, so removing one never
  changes silver. ``download --debug`` deliberately leaves a crashed dump
  in place rather than letting its own trap remove it — the captures
  are the point — so under that flag this category is the backstop that
  reclaims it.

Completeness signal: the ``run.json`` ``status`` field (``"in-progress"``
at run-dir creation, atomically overwritten with ``"complete"`` at the
end). A statusless-but-readable manifest predates the ``status`` field,
where the manifest was written only at the end so its presence alone
marked completion; it is classified COMPLETE and its load inputs are
kept. An unreadable or corrupt ``run.json`` is UNKNOWN and never
deleted.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir
so a long backfill is protected. ``--dry-run`` prints the plan without
removing anything. The only paths ever deleted are whole non-complete
run dirs, ``screenshots/`` subdirs, and stale ``--debug-dir`` entries;
the load inputs of complete dumps
(run.json, accounts.json, positions.xls, position_details.json,
list_of_assets.xls, the transactions CSVs, and documents/) and the
non-run entries at the bronze root (``manual/``, the silver
``swissquote.db``) are never touched.

Usage:
    prune.py [--bronze-dir /data] [--debug-dir DIR] [--dry-run]
             [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import debugcap, prune


CONFIG = prune.PruneConfig(
    # Where `download --debug` puts its landmark captures.
    debug_subdirs=(debugcap.SCREENSHOTS_DIR,),
    # A statusless-but-readable run.json is a pre-`status` complete dump:
    # the manifest was written only at the end, so its presence means the
    # walk finished.
    is_complete=prune.lenient_classification,
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


if __name__ == "__main__":
    sys.exit(main())
