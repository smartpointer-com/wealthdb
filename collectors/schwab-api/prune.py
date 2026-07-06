#!/usr/bin/env python3
"""
Prune non-complete dumps from the schwab-api bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with schwab-api's configuration. schwab-api is a pure
REST collector: a bronze run dir is a flat set of JSON artefacts
(``account_numbers.json``, ``user_preference.json``,
``accounts_positions.json``, ``transactions_NNN.json``,
``open_orders.json``, and optionally ``instruments.json``) plus the
``run.json`` manifest — every one of those is a ``load`` input or the
manifest, and there are no bronze-resident debug artefacts (the
browser-flow page captures / Playwright traces belong to ``login.py``
and land in a separate ``/debug`` dir, never in bronze). So
``debug_subdirs`` is empty and the verb's sole effect is reclaiming
whole run dirs that are not complete dumps:

* ``run.json`` is missing (a pre-manifest dump that crashed before
  ``open_orders.json`` was written) or its ``status`` is anything other
  than ``"complete"`` (an ``"in-progress"`` marker from a crashed walk,
  or a ``"dry-run"`` shell — though ``--dry-run`` never creates a run
  dir here). ``load`` would otherwise keep re-ingesting whatever partial
  artefacts such a dir holds (a crashed dump with a present-but-partial
  ``account_numbers.json`` still loads a truncated snapshot); after
  pruning one, the next ``load --force`` rebuild reflects the removal.

Completeness signal: the ``run.json`` ``status`` field ``download.py``
now writes (``"in-progress"`` at run-dir creation, atomically
overwritten with ``"complete"`` at the end). Dumps that predate the
manifest carry no ``run.json`` at all (``meta is None``); for those the
legacy terminal signal is the presence of ``open_orders.json`` — the
last unconditional artefact a complete run writes (only the optional
``instruments.json`` may follow it, and ``load`` treats that as
optional), so a run dir with ``open_orders.json`` but no manifest is a
pre-change complete dump and keeps its load inputs. An unreadable or
corrupt ``run.json`` is UNKNOWN and never deleted.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir so
a long transaction backfill (which keeps writing ``transactions_NNN.json``)
is protected. ``--dry-run`` prints the plan without removing anything.
The only paths ever deleted are whole non-complete run dirs; load inputs
of complete dumps and non-run entries at the bronze root (the silver
``schwab-api.db``) are never touched.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import prune

# The terminal artefact of a complete pre-manifest run: the last file
# `download.py` writes unconditionally (before the optional, load-optional
# instruments.json). Its presence is the legacy completeness signal.
TERMINAL_ARTEFACT = "open_orders.json"


def _is_complete(run_dir, meta):
    # Forward: run.json status ("in-progress" -> "complete") is
    # authoritative and resolved by status_classification before the
    # legacy fallback. Legacy (no run.json at all, so meta is None —
    # every pre-change schwab-api dump): the terminal artefact
    # open_orders.json. It is checked with .exists() only, never
    # deleted, so a complete dump loses nothing (debug_subdirs is empty).
    return prune.status_classification(
        meta, run_dir=run_dir,
        legacy_complete=lambda rd, m: (rd / TERMINAL_ARTEFACT).exists())


CONFIG = prune.PruneConfig(
    debug_subdirs=(),            # no bronze-resident debug artefacts
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
