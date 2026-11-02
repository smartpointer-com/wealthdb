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

Completeness comes from the ``run.json`` status. A statusless
``run.json`` predates the field. ``load`` reads such a dump, so it is
complete and kept, a legacy ``--dry-run`` shell included. ``load``
skips a dump whose status is not ``"complete"``, so removing one never
changes silver. The safety
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


CONFIG = prune.PruneConfig(
    debug_subdirs=(debugcap.SCREENSHOTS_DIR,),
    debug_globs=(TX_PAGE_HTML_ORPHAN_GLOB,),
    # A status key decides. A statusless but readable run.json predates
    # the field; load reads such a dump, so it is kept.
    is_complete=prune.lenient_classification,
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


if __name__ == "__main__":
    sys.exit(main())
