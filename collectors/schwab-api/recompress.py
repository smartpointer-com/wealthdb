#!/usr/bin/env python3
"""
Recompress the schwab-api bronze backlog to the .json.zst form.

Thin wrapper over :mod:`collectorkit.recompress` (the shared,
unit-tested backlog-recompression engine) with schwab-api's
configuration. ``download`` compresses each bronze DATA artefact as it
lands (``accounts_positions.json.zst``, ``transactions_NNN.json.zst``,
etc.); this verb converts the run dirs written *before* that change,
replacing every plain data JSON inside a **complete** dump with a
zstd-compressed twin. The silver loader resolves either form and parses
the decompressed bytes, so silver comes out byte-identical — verify with
a ``load --force`` rebuild after the sweep.

``run.json`` is NEVER compressed: it is excluded from the patterns
below (the run dir is flat, so a top-level ``*.json`` glob would wrongly
match it — the artefacts are therefore enumerated exactly). It is the
status-lifecycle manifest ``prune`` and ``load`` read directly and must
stay greppable.

Unlike ``prune``, this verb rewrites load inputs, so it is strictly
manual: never wire it into a schedule; review ``--dry-run`` first.
Safety envelope (shared engine): complete quiescent dumps only, an
unreadable/corrupt run.json is never touched, symlinks are refused,
and each original is unlinked only after the compressed twin has been
decompressed and sha256-verified against it. Interrupted sweeps are
safe to re-run — a verified twin left next to its original is adopted,
a corrupt one is redone.

Completeness classification is byte-identical to ``prune``'s (it reuses
prune's own ``is_complete`` predicate — same ``status`` lifecycle, same
``open_orders.json`` fallback for a dump carrying no status), so the two
verbs can never disagree about a dump.

Usage:
    recompress.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import recompress

from prune import CONFIG as PRUNE_CONFIG

CONFIG = recompress.RecompressConfig(
    # The six data artefacts, enumerated exactly — a top-level *.json
    # glob would also match run.json (the status-lifecycle manifest that
    # must never be compressed), and compressed twins don't match a
    # *.json pattern, so already-converted runs drop out naturally.
    patterns=(
        "account_numbers.json",
        "user_preference.json",
        "accounts_positions.json",
        "transactions_*.json",
        "open_orders.json",
        "instruments.json",
    ),
    # Literally prune's predicate — one completeness definition.
    is_complete=PRUNE_CONFIG.is_complete,
)


def main(argv=None):
    return recompress.main(CONFIG, argv, description=__doc__)


if __name__ == "__main__":
    sys.exit(main())
