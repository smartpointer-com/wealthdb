#!/usr/bin/env python3
"""Prune non-complete dumps from the raiffeisen_at bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested prune
engine) with this collector's configuration. It reclaims whole run dirs that
are not complete dumps — ``run.json`` is missing (a crashed walk) or its
``status`` is anything other than ``"complete"`` (an ``"in-progress"``
marker or a ``"dry-run"`` shell). ``load`` skips such dirs, so removing one
only surfaces on the next ``load --force`` rebuild.

A COMPLETE dump's load inputs (``accounts.json``, ``history/*.json``,
``balances/*.json``, ``statements/*/*.pdf``) are structurally protected: the
engine only ever touches whole non-complete run dirs and the configured
debug subdirs. This collector writes no in-bronze debug subdir — the
``explore``/``login`` diagnostics land in ``/debug`` (outside bronze) and
``prune --debug-dir`` reclaims that separately — so ``debug_subdirs`` is
empty.

Usage:
    prune.py [--bronze-dir /data] [--debug-dir DIR] [--dry-run] [--min-age-hours N]
"""
from __future__ import annotations

import sys

from collectorkit import prune


def _is_complete(run_dir, meta):
    return prune.status_classification(
        meta, run_dir=run_dir, legacy_complete=lambda rd, m: m is not None)


CONFIG = prune.PruneConfig(
    debug_subdirs=(),          # raiffeisen_at writes no in-bronze debug capture
    is_complete=_is_complete,
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


def validate_target(path, bronze_dir):
    return prune.validate_target(path, bronze_dir, CONFIG)


if __name__ == "__main__":
    sys.exit(main())
