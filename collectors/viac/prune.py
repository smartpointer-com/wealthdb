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
the ``status`` field, where the manifest was written only at the end, so
it is a finished dump — classified COMPLETE and kept — UNLESS it also
carries ``dry_run: true``, the shape an older ``--dry-run`` left behind,
which stays NON_COMPLETE to match the forward ``status="dry-run"``
behaviour. An unreadable or corrupt ``run.json`` is UNKNOWN and never
deleted.

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


def _is_complete(run_dir, meta):
    # Legacy fallback for statusless dumps (and the missing-manifest
    # branch): a run.json with no `status` key predates the status
    # lifecycle. load reads every such dump, so it is kept, a legacy
    # dry-run shell included. A missing run.json (meta is None) fails the
    # `m is not None` check -> a crashed download with no manifest is
    # NON_COMPLETE. Current walks always carry a status key, so
    # status_classification resolves those before this fallback.
    return prune.status_classification(
        meta, run_dir=run_dir, legacy_complete=lambda rd, m: m is not None)


CONFIG = prune.PruneConfig(
    # `download --debug`'s HTTP trace, which debugcap lands under this
    # subdir fleet-wide. Everything else in a run dir is a bronze capture.
    debug_subdirs=(debugcap.SCREENSHOTS_DIR,),
    is_complete=_is_complete,
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


if __name__ == "__main__":
    sys.exit(main())
