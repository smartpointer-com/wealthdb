#!/usr/bin/env python3
"""
Recompress the fidelity-web bronze backlog to the .zst form.

Thin wrapper over :mod:`collectorkit.recompress` (the shared,
unit-tested backlog-recompression engine) with fidelity-web's
configuration. ``download`` compresses each HTML/CSV export as it lands
(``balances/balances.html.zst``, ``positions/positions_*.csv.zst``,
``activity/activity_*.csv.zst``); this verb converts the run dirs
written *before* that change, replacing every plain compressible file
inside a **complete** dump with a zstd-compressed twin. PDFs are never
touched — they are already internally compressed and are a load input
in raw form. The silver loader resolves either form, so silver comes
out byte-identical — verify with a ``load --force`` rebuild after the
sweep.

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
statusless-but-readable legacy rule), so the two verbs can never
disagree about a dump.

Usage:
    recompress.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import recompress

from prune import CONFIG as PRUNE_CONFIG

CONFIG = recompress.RecompressConfig(
    # The compressible bronze artefacts, and nothing else: HTML from the
    # balances / performance surfaces, the positions / activity /
    # statement-companion CSVs, and the lot step's index and response
    # bundle. documents/*.pdf (already compressed) and run.json (the
    # status-lifecycle handshake) never match a pattern, and the debug
    # screenshots/ tree lives under its own subdir.
    patterns=(
        "balances/*.html",
        "performance/*.html",
        "positions/*.csv",
        "activity/*.csv",
        "documents/*.csv",
        "lots/*.json",
        "lots/*.jsonl",
    ),
    # Literally prune's predicate — one completeness definition.
    is_complete=PRUNE_CONFIG.is_complete,
)


def main(argv=None):
    return recompress.main(CONFIG, argv, description=__doc__)


if __name__ == "__main__":
    sys.exit(main())
