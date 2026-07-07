#!/usr/bin/env python3
"""
Recompress the cointracking bronze backlog to the .csv.zst form.

Thin wrapper over :mod:`collectorkit.recompress` (the shared,
unit-tested backlog-recompression engine) with cointracking's
configuration. ``download`` compresses each CSV export as it lands
(``cu_<id>/trades.csv.zst`` etc.); this verb converts the run dirs
written *before* that change, replacing every plain
``cu_<id>/*.csv`` inside a **complete** dump with a zstd-compressed
twin. The silver loader resolves either form, and DuckDB streams the
compressed CSVs natively, so silver comes out byte-identical — verify
with a ``load --force`` rebuild after the sweep.

Unlike ``prune``, this verb rewrites load inputs, so it is strictly
manual: never wire it into a schedule; review ``--dry-run`` first.
Safety envelope (shared engine): complete quiescent dumps only, an
unreadable/corrupt run.json is never touched, symlinks are refused,
and each original is unlinked only after the compressed twin has been
decompressed and sha256-verified against it. Interrupted sweeps are
safe to re-run — a verified twin left next to its original is adopted,
a corrupt one is redone.

Completeness classification is byte-identical to ``prune``'s (same
``status`` lifecycle, same statusless-but-readable legacy rule), so
the two verbs can never disagree about a dump.

Usage:
    recompress.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import recompress

from prune import CONFIG as PRUNE_CONFIG

CONFIG = recompress.RecompressConfig(
    # Every per-portfolio CSV export is a candidate; run.json and the
    # bronze-root files (known_portfolios.json, the silver DuckDB)
    # never match the pattern.
    patterns=("cu_*/*.csv",),
    # Literally prune's predicate — one completeness definition.
    is_complete=PRUNE_CONFIG.is_complete,
)


def main(argv=None):
    return recompress.main(CONFIG, argv, description=__doc__)


if __name__ == "__main__":
    sys.exit(main())
