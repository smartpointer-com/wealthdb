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
  ``"in-progress"`` marker left by a crashed walk). ``load`` would
  otherwise keep re-ingesting whatever partial ``investments.json`` /
  ``offerings/`` such a dir holds; after pruning one, the next
  ``load --force`` rebuild reflects the removal. ``--dry-run`` never
  creates a run dir, so there is no dry-run shell to reclaim.

Completeness signal: the ``run.json`` ``status`` field the walk now
writes (``"in-progress"`` at run-dir creation, atomically overwritten
with ``"complete"`` at the end). Dumps that predate the ``status`` field
carry a full manifest with no ``status`` key — those are pre-change
complete dumps (the walk historically wrote ``run.json`` only once, at the
very end, after every artefact), so a statusless-but-readable manifest is
classified COMPLETE and keeps its load inputs. An unreadable or corrupt
``run.json`` is UNKNOWN and never deleted.

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


def _is_complete(run_dir, meta):
    # A statusless-but-readable run.json is a pre-`status` complete dump:
    # the walk historically wrote run.json only once, at the very end
    # (after investments.json + every offering + every document blob), so
    # its mere presence means the walk finished. New walks always carry a
    # status key (in-progress → complete), so status_classification
    # resolves those before this legacy fallback is consulted.
    return prune.status_classification(
        meta, run_dir=run_dir, legacy_complete=lambda rd, m: m is not None)


CONFIG = prune.PruneConfig(
    # Where `download --debug` puts its captures.
    debug_subdirs=(debugcap.SCREENSHOTS_DIR,),
    is_complete=_is_complete,  # manifest_name defaults to "run.json"
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


def validate_target(path, bronze_dir):
    """Back-compat shim for the local test suite: bind the shared
    validator to this collector's config."""
    return prune.validate_target(path, bronze_dir, CONFIG)


if __name__ == "__main__":
    sys.exit(main())
