#!/usr/bin/env python3
"""
Prune debug artefacts and non-complete dumps from the schwab-api bronze
tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with schwab-api's configuration. A bronze run dir is a flat
set of JSON artefacts (``account_numbers.json``, ``user_preference.json``,
``accounts_positions.json``, ``transactions_NNN.json``,
``open_orders.json``, and optionally ``instruments.json``) plus the
``run.json`` manifest — every one of those is a ``load`` input or the
manifest, and is never deleted. The one artefact that is not:
``<run>/screenshots/``, holding the HTTP trace ``download --debug``
writes (``http-trace.jsonl``: one metadata line per Schwab request,
retries included). It is written only under ``--debug`` and never read by
``load``, so deleting it leaves silver byte-identical. The browser-flow
page captures / Playwright traces are a different thing again — they
belong to ``login.py`` and land in a separate ``/debug`` dir, never in
bronze.

That separate ``/debug`` dir is reclaimed too — the wrapper passes it as
the engine's ``--debug-dir``, since every ``login --trace`` leaves a
bundle there and nothing else ever clears them out.

The data artefacts are now zstd-compressed as they land
(``accounts_positions.json.zst`` etc.; plain ``.json`` in
pre-compression dumps — both forms are ``load`` inputs), while
``run.json`` stays uncompressed (it is the ``status`` manifest this
classification keys on). For forward (manifest-bearing) dumps
compression is inert to prune — classification is purely status-based.
The one place it bites: a *legacy* (no-``run.json``) dump's completeness
signal is the terminal DATA artefact ``open_orders.json``, which the
``recompress`` sweep may leave as ``open_orders.json.zst``; so
``_is_complete`` resolves the on-disk variant (plain **or** ``.zst``)
rather than probing the plain name — otherwise a recompressed legacy
dump would flip to NON_COMPLETE and be pruned. Converting the
pre-compression backlog is the separate manual ``recompress`` verb's
job, not prune's. The categories:

* ``<run>/screenshots/`` — the ``--debug`` HTTP trace, reclaimed from
  complete dumps (see above).

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
legacy terminal signal is the presence of ``open_orders.json`` (in
either the plain or the ``.zst`` form — see above) — the last
unconditional artefact a complete run writes (only the optional
``instruments.json`` may follow it, and ``load`` treats that as
optional), so a run dir with ``open_orders.json`` but no manifest is a
pre-change complete dump and keeps its load inputs. An unreadable or
corrupt ``run.json`` is UNKNOWN and never deleted.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir so
a long transaction backfill (which keeps writing ``transactions_NNN.json``)
is protected. ``--dry-run`` prints the plan without removing anything.
The only paths ever deleted are ``<run>/screenshots/`` subtrees and whole
non-complete run dirs; load inputs of complete dumps and non-run entries
at the bronze root (the silver ``schwab-api.db``) are never touched.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import compress, debugcap, prune

# The terminal artefact of a complete pre-manifest run: the last file
# `download.py` writes unconditionally (before the optional, load-optional
# instruments.json). Its presence is the legacy completeness signal.
TERMINAL_ARTEFACT = "open_orders.json"


def _is_complete(run_dir, meta):
    # Forward: run.json status ("in-progress" -> "complete") is
    # authoritative and resolved by status_classification before the
    # legacy fallback. Legacy (no run.json at all, so meta is None —
    # every pre-change schwab-api dump): the terminal artefact
    # open_orders.json. It is a compressible DATA artefact, so the
    # `recompress` sweep may leave it as open_orders.json.zst — resolve
    # the on-disk variant rather than probing the plain name, or a
    # recompressed legacy dump would flip to NON_COMPLETE and get pruned.
    # It is only inspected, never deleted (it is a load input, not a debug
    # subdir), so a complete dump keeps it either way.
    return prune.status_classification(
        meta, run_dir=run_dir,
        legacy_complete=lambda rd, m: (
            compress.resolve_variant(rd / TERMINAL_ARTEFACT) is not None))


CONFIG = prune.PruneConfig(
    # `download --debug`'s HTTP trace, which debugcap lands under this
    # subdir fleet-wide. Every other file in a run dir is a load input.
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
