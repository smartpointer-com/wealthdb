#!/usr/bin/env python3
"""
Prune non-complete dumps from the equityzen bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with equityzen's configuration. Two categories are
reclaimed:

* ``screenshots/`` from a complete dump — the portfolio-list and
  detail-less-offering captures ``download --debug`` writes. ``load``
  never reads them, so reclaiming them cannot change silver. (The other
  diagnostics — ``login --screenshot-dir`` screenshots and the ``explore``
  verb's ``/debug/<UTC-ts>/`` HAR/trace/click log — live outside bronze,
  and prune never sees them.)
* whole run dirs that are not complete dumps: ``run.json`` is missing
  (the walk crashed before writing the in-progress marker or the terminal
  manifest, or ``--debug`` captured a page and the walk then failed the
  auth gate) or its ``status`` is anything other than ``"complete"`` (an
  ``"in-progress"`` marker left by a crashed walk). ``load`` skips such
  dirs; ``prune`` reclaims them once quiescent. ``--dry-run`` never
  creates a run dir, so there is no dry-run shell to reclaim.

Completeness signal: the ``run.json`` ``status`` field (``"in-progress"``
at run-dir creation, atomically overwritten with ``"complete"`` at the
end). A manifest with no ``status`` key counts as complete, keeping its
load inputs: a download that writes no status writes its manifest only at
the end. An unreadable or corrupt ``run.json`` is UNKNOWN and never
deleted.

A COMPLETE dump's load inputs (``investments.json``,
``offerings/*/detail.json``, and the ``documents/<deal-slug>/*.pdf`` /
``.zip`` blobs parsed by statements.py) are structurally protected: the
engine only ever touches the configured debug subdirs of complete dumps —
``screenshots/`` alone — and whole non-complete run dirs.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir so a
long backfill is protected. ``--dry-run`` prints the plan without removing
anything. Non-run entries at the bronze root (the silver ``equityzen.db``)
are never touched.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import debugcap, prune


CONFIG = prune.PruneConfig(
    # Where `download --debug` puts its captures.
    debug_subdirs=(debugcap.SCREENSHOTS_DIR,),
    # manifest_name defaults to "run.json".
    is_complete=prune.lenient_classification,
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


if __name__ == "__main__":
    sys.exit(main())
