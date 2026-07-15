#!/usr/bin/env python3
"""
Prune debug artefacts and non-complete dumps from the relevate bronze
tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with relevate's configuration. Two categories are removed,
across every timestamped run dir under ``--bronze-dir``.

The first is ``<run>/screenshots/`` — the HTTP trace ``download --debug``
writes (``http-trace.jsonl``: one metadata line per Relevate request).
relevate drives no browser, so there are no DOM dumps or Playwright
traces beside it; the request trace is its whole debug surface. It is
written only under ``--debug`` and never read by ``load``, so deleting it
leaves silver byte-identical. Every other artefact a complete dump holds
(the JSON payloads, the document PDFs, the manifest) is a faithful bronze
capture, several are ``load`` inputs read cross-dump, and none is ever
removed.

The second is whole run dirs that are **not complete dumps**:

* a crashed / interrupted walk — ``run.json`` is absent, or present
  but its ``status`` is ``"in-progress"`` / ``"incomplete"`` (the
  marker the walk drops at run-dir creation and only overwrites with a
  terminal status at the end), or (for a dump predating the ``status``
  field) a manifest whose ``ended_at`` never got stamped;
* a ``--dry-run`` shell — ``status`` ``"dry-run"``, or (pre-``status``)
  a manifest with ``dry_run: true``.

``load`` would otherwise re-ingest whatever partial artefacts such a
dir holds; after pruning one, the next ``load --force`` rebuild
reflects the removal.

Completeness signal: the ``run.json`` ``status`` field the walk now
writes (``"in-progress"`` at run-dir creation, atomically overwritten
with ``"complete"`` / ``"dry-run"`` / ``"incomplete"`` at the end).
Dumps predating the field carry a manifest with no ``status`` key —
and because relevate's manifest is written *incrementally* from the
start (flushed after every fetch), the mere presence of ``run.json``
does NOT mean the walk finished. So the legacy fallback classifies a
statusless dump COMPLETE only when its original terminal signal is
genuinely present: ``ended_at`` stamped (``finish()`` ran) on a real,
non-``dry_run`` run. A crashed pre-``status`` walk (``run.json``
present, ``ended_at: null``) is correctly NON_COMPLETE. An unreadable
or corrupt ``run.json`` is UNKNOWN and never deleted.

An in-flight guard skips non-complete dumps written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir
so a long backfill is protected. ``--dry-run`` prints the plan without
removing anything. The only paths ever deleted are
``<run>/screenshots/`` subtrees and whole non-complete run dirs; load
inputs of complete dumps (``accounts/``, ``portfolios/``, ``documents/``
— the last read cross-dump for historical-snapshot and credit-note PDF
parsing) and non-run entries at the bronze root (``manual/``, the silver
DB) are never touched.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import debugcap, prune


def _legacy_complete(run_dir, meta) -> bool:
    # A statusless-but-readable run.json is a pre-`status` dump. Unlike
    # fidelity-web (whose walk wrote run.json ONLY at the end, so mere
    # presence meant complete), relevate flushes run.json after every
    # fetch from run-dir creation — so presence proves nothing. The
    # original terminal signal was `finish()` stamping `ended_at` on a
    # real (non-dry) run; a crashed walk leaves `ended_at: null`. New
    # walks always carry a `status` key, so status_classification
    # resolves in-progress/dry-run/complete before this fallback runs.
    return (
        meta is not None
        and meta.get("ended_at") is not None
        and not meta.get("dry_run")
    )


def _is_complete(run_dir, meta):
    return prune.status_classification(
        meta, run_dir=run_dir, legacy_complete=_legacy_complete)


CONFIG = prune.PruneConfig(
    # `download --debug`'s HTTP trace, which debugcap lands under this
    # subdir fleet-wide. Everything else in a run dir is a bronze capture.
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
