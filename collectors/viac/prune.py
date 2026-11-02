#!/usr/bin/env python3
"""
Prune debug artefacts and non-complete dumps from the viac bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with viac's configuration. Two categories are reclaimed:

* ``<run>/screenshots/`` — the HTTP trace ``download --debug`` writes
  (``http-trace.jsonl``: one metadata line per VIAC request, retries
  included). viac drives no browser, so there are no DOM dumps or
  Playwright traces beside it; the request trace is its whole debug
  surface. Written only under ``--debug`` and never read by ``load``, so
  deleting it leaves silver byte-identical. Every other file in a
  complete dump is a bronze capture and is never touched.

* whole run dirs that are not complete dumps — ``run.json`` is missing
  (the walk crashed before writing even the in-progress marker), or its
  ``status`` is anything other than ``"complete"`` (an ``"in-progress"``
  marker from a crashed walk, or a legacy ``"dry-run"`` shell).
  ``load`` skips such dirs, so removing one never changes silver.

Completeness signal: the ``run.json`` ``status`` field (``"in-progress"``
at run-dir creation, atomically overwritten with ``"complete"`` /
``"dry-run"`` at the end). A statusless-but-readable manifest predates
the ``status`` field. ``load`` reads such a dump, so it is COMPLETE and
kept, a legacy ``dry_run: true`` shell included. An unreadable or
corrupt ``run.json`` is UNKNOWN and never deleted.

Note on hard-linked PDFs: ``download.py`` deduplicates document PDFs
across run dirs with ``os.link``. Deleting a whole NON-complete run dir
that holds such a link is safe — the inode survives as long as any
complete dump holds another link to it, so no ``load`` input (the
``documents/<docid>.pdf`` parsed cross-dump by the historical-reports
phase) is lost.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir
so a long backfill is protected. ``--dry-run`` prints the plan without
removing anything. The only paths ever deleted are
``<run>/screenshots/`` subtrees and whole non-complete run dirs;
complete dumps' load inputs and non-run entries at the bronze root (the
silver ``viac.db``) are never touched.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import debugcap, prune


CONFIG = prune.PruneConfig(
    # `download --debug`'s HTTP trace, which debugcap lands under this
    # subdir fleet-wide. Everything else in a run dir is a bronze capture.
    debug_subdirs=(debugcap.SCREENSHOTS_DIR,),
    # A status key decides. A statusless but readable run.json predates
    # the field; load reads such a dump, so it is kept.
    is_complete=prune.lenient_classification,
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


if __name__ == "__main__":
    sys.exit(main())
