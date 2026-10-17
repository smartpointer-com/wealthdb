#!/usr/bin/env python3
"""
Prune debug artefacts and non-complete dumps from the schwab-api bronze
tree.

A run dir holds the JSON load inputs (``account_numbers``,
``user_preference``, ``accounts_positions``, ``transactions_NNN``,
``open_orders`` and the optional ``instruments``, each ``.json`` or
``.json.zst``) and the uncompressed ``run.json`` manifest. None of them
is ever deleted from a complete dump.

The one debug-only path in bronze is ``<run>/screenshots/``: the HTTP
trace ``download --debug`` writes, which ``load`` never reads. The login
captures land in the debug dir outside bronze, which the wrapper passes
as ``--debug-dir``; its idle entries are reclaimed too.

Completeness comes from the ``run.json`` status. A dump carrying no
status, including one with no ``run.json`` at all, is complete when it
holds ``open_orders.json`` (plain or ``.zst``), the last file a finished
run always writes. ``load`` applies the same rule, so removing a
non-complete dump never changes silver. The safety envelope (the age
guard, ``--dry-run``, symlinks, an unreadable manifest) is the shared
engine's: see collectors/README.md.

Usage:
    prune.py [--bronze-dir /data] [--debug-dir DIR] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import compress, debugcap, prune

# The terminal artefact of a complete run: the last file `download.py`
# writes unconditionally (before the optional, load-optional
# instruments.json). Its presence is the statusless completeness signal.
TERMINAL_ARTEFACT = "open_orders.json"


def _is_complete(run_dir, meta):
    # run.json status ("in-progress" -> "complete") is authoritative and
    # resolved by status_classification first. The fallback below fires
    # only for a dump carrying no status: the terminal artefact
    # open_orders.json. It is a compressible DATA artefact, so the
    # `recompress` sweep may leave it as open_orders.json.zst — resolve
    # the on-disk variant rather than probing the plain name, or a
    # recompressed dump would flip to NON_COMPLETE and get pruned. It is
    # only inspected, never deleted (it is a load input, not a debug
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


if __name__ == "__main__":
    sys.exit(main())
