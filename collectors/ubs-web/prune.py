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
* whole run dirs that are not complete dumps — a ``--dry-run`` shell
  (``status: "dry-run"``, only a manifest and no exports), or a walk
  that crashed before finalising (``status: "in-progress"``, or no
  ``run.json`` at all). ``load`` would otherwise keep re-ingesting
  whatever partial artefacts such a dir holds; after pruning one, the
  next ``load --force`` rebuild reflects the removal.

Completeness signal: the ``run.json`` ``status`` field the walk now
writes (``"in-progress"`` at run-dir creation, atomically overwritten
with ``"complete"`` / ``"dry-run"`` at the end). Dumps that predate
the ``status`` field carry a full manifest with no ``status`` key.
ubs-web's ``write_run_json`` runs only at the end of a real walk and
records a ``dry_run`` bool, so a statusless manifest is a pre-change
dump that is complete iff it is present **and** not a ``--dry-run``
shell — hence the legacy predicate checks ``not m.get("dry_run")`` on
top of ``m is not None`` (a legacy ``--dry-run`` shell has a
full-looking manifest and must not be classed complete). An
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


def _is_complete(run_dir, meta):
    # Forward convention: status_classification resolves "complete" →
    # COMPLETE and "in-progress"/"dry-run" → NON_COMPLETE before the
    # legacy fallback is consulted. The fallback fires only for a
    # statusless (pre-change) manifest: ubs-web's write_run_json ran
    # once, at the end of a real walk, and recorded a dry_run bool, so a
    # legacy dump is complete iff its manifest is present AND it was not
    # a --dry-run shell. The extra `not m.get("dry_run")` vs
    # fidelity-web's bare `m is not None` is load-bearing here: ubs-web's
    # run.json always carries dry_run, so a legacy --dry-run shell has a
    # full-looking manifest and must NOT be kept forever as "complete".
    return prune.status_classification(
        run_dir=run_dir,
        meta=meta,
        legacy_complete=lambda rd, m: m is not None
        and not m.get("dry_run", False),
    )


CONFIG = prune.PruneConfig(
    # Where `download --debug` puts its landmark captures.
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
