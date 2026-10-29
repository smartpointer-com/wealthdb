#!/usr/bin/env python3
"""
Prune debug artefacts and non-complete dumps from the schwab-web
bronze tree.

A run dir holds the load inputs (``statements/<suffix>/`` PDFs, the
``transactions/<suffix>/`` CSV/JSON/XML exports and
``more-details.json``) and the ``run.json`` manifest. None of them is
ever deleted from a complete dump.

Two kinds of path are debug-only and reclaimed from complete dumps:
``<run>/screenshots/`` (the tx-history landing-page HTML ``download
--debug`` writes) and ``<run>/transactions/*/page-*.html`` (landing-page
HTML beside the exports, never a load input). The glob can match no
load input: every sibling is ``more-details.json`` or a ``.csv`` /
``.json`` / ``.xml`` export. The login captures land in the debug dir
outside bronze, which the wrapper passes as ``--debug-dir``.

Completeness comes from the ``run.json`` status. ``run.json`` is
written incrementally, so a statusless one falls back to ``dry_run``: a
real dump (``dry_run`` false) is complete and keeps its load inputs, a
``--dry-run`` shell is not. ``load`` skips a dump whose status is not
``"complete"``, so removing one never changes silver. The safety
envelope (the age guard, ``--dry-run``, symlinks, an unreadable
manifest) is the shared engine's: see collectors/README.md.

Usage:
    prune.py [--bronze-dir /data] [--debug-dir DIR] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import debugcap, prune

# Per-account landing-page HTML beside the tx-history exports. `load`
# never reads it; this glob matches ONLY files named page-*.html two
# levels deep, never a load-input sibling (all of which are
# more-details.json or .csv/.json/.xml exports).
TX_PAGE_HTML_ORPHAN_GLOB = "transactions/*/page-*.html"


def _is_complete(run_dir, meta):
    # status_classification resolves any dump carrying a `status` key
    # (in-progress → complete/dry-run). The fallback below fires only
    # for a statusless manifest, which predates the field. load reads
    # every such dump, so it is kept, a legacy --dry-run shell included:
    # prune never deletes a load input.
    return prune.status_classification(
        run_dir=run_dir, meta=meta,
        legacy_complete=lambda rd, m: m is not None,
    )


CONFIG = prune.PruneConfig(
    debug_subdirs=(debugcap.SCREENSHOTS_DIR,),
    debug_globs=(TX_PAGE_HTML_ORPHAN_GLOB,),
    is_complete=_is_complete,
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


if __name__ == "__main__":
    sys.exit(main())
