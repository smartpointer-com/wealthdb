#!/usr/bin/env python3
"""
Prune non-complete dumps from the ubs-web bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with ubs-web's configuration. Two things are reclaimed:

* ``screenshots/`` inside a complete dump — the landmark DOM +
  screenshot captures ``download --debug`` writes. ``load`` never reads
  them, so reclaiming them cannot change silver. (The rest of ubs-web's
  troubleshooting output — per-landmark screenshots, the Playwright
  trace bundle, the login QR PNG, and the whole ``explore`` capture —
  stays outside bronze, in the external ``--screenshot-dir`` /
  ``--trace`` / ``--qr-png`` / ``--debug-dir`` outputs under the
  ``/debug`` mount. ``prune`` reclaims that dir separately, via the
  shared engine's own ``--debug-dir``, which ages out each entry
  directly under it — an ``explore`` capture is one such entry.)
* whole run dirs that ``load`` does not read — a ``--dry-run`` shell
  (``status: "dry-run"``, only a manifest and no exports), or a walk
  that crashed before finalising (``status: "in-progress"``, or no
  ``run.json`` at all). ``load`` skips such dirs, so pruning one only
  reclaims disk.

Completeness signal: the ``run.json`` ``status`` field the walk
writes (``"in-progress"`` at run-dir creation, atomically overwritten
with the terminal status at the end). ``"complete"`` and ``"partial"``
(a ``download --only`` pass, which ``load`` reads) are kept. Legacy
dumps carry a full manifest with no ``status`` key and a ``dry_run``
bool, so a statusless manifest is kept when it does not record a
``--dry-run`` shell (``load.legacy_manifest_is_complete``). An
unreadable or corrupt ``run.json`` is UNKNOWN and never deleted.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir
so a long multi-window backfill is protected. ``--dry-run`` prints
the plan without removing anything. The only paths ever deleted are
whole non-complete run dirs and ``screenshots/`` subdirs; load inputs
of complete dumps (``run.json``, ``positions/``, ``transactions/``,
``documents/``) and non-run entries at the bronze root (the silver DB)
are never touched.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import debugcap, prune

import load  # local module: the loader owns which dumps are load inputs


def _is_complete(run_dir, meta):
    # A `partial` dump (a `download --only` pass) is one `load` reads, so
    # it is kept like a complete one. Every other present status resolves
    # in status_classification. The legacy fallback fires only for a
    # statusless manifest, and it is `load`'s own rule: present and not a
    # --dry-run shell. ubs-web's run.json always carries `dry_run`, so a
    # legacy --dry-run shell has a full-looking manifest and must NOT be
    # kept forever as complete.
    if meta is not None and meta.get("status") == load.PARTIAL_STATUS:
        return prune.COMPLETE, "partial (a download --only pass)"
    return prune.status_classification(
        meta, run_dir=run_dir,
        legacy_complete=lambda rd, m: load.legacy_manifest_is_complete(m))


CONFIG = prune.PruneConfig(
    # Where `download --debug` puts its landmark captures.
    debug_subdirs=(debugcap.SCREENSHOTS_DIR,),
    is_complete=_is_complete,
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


if __name__ == "__main__":
    sys.exit(main())
