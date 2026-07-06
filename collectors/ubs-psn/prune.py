#!/usr/bin/env python3
"""
Prune non-complete dumps from the ubs-psn bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with ubs-psn's configuration. Unlike a browser collector,
ubs-psn writes **no** bronze-resident debug artefact, and its run dirs
hold only ``<ORDERTYPE>.zip`` files — so this verb is deliberately
narrow: the only thing it can ever reclaim is a **zip-less crash shell**
(a run dir minted before the first file arrived, then abandoned).

The scope is narrow because ubs-psn's zips are **irreplaceable**: UBS
deletes each per-order-type zip from its server the moment it is
downloaded, so a re-run cannot re-fetch it, and ``load`` ingests whatever
``*.zip`` are present regardless of whether the dump finished. A run dir
holding *any* zip therefore holds genuinely consumed data that no marker
can distinguish from a clean dump, so it is classified COMPLETE and never
a deletion candidate — even when a crash left ``run.json`` at
``"in-progress"`` or wrote no manifest at all. The has-zip check
short-circuits **before** the ``status`` field is consulted; only a truly
zip-less shell defers to the status lifecycle.

``debug_subdirs`` is empty (nothing to reclaim from a complete dump), so
the sole deletion path is a whole zip-less run dir once it is quiescent.
In practice this is a safety-first near-no-op — ``download.py`` already
removes a run dir that received zero files, and ``--dry-run`` creates no
run dir — but the verb exists so a fleet-wide ``prune`` is guaranteed
never to delete a ubs-psn ``load`` input, and so its help and behaviour
match every other collector's.

An unreadable or corrupt ``run.json`` is UNKNOWN and never deleted. An
in-flight guard skips non-complete shells written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir.
``--dry-run`` prints the plan without removing anything.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import prune


def _is_complete(run_dir, meta):
    # ubs-psn PSN zips are IRREPLACEABLE: UBS deletes each file server-side
    # on a successful download, and `load` ingests whatever *.zip are
    # present regardless of dump completeness. A run dir holding ANY zip
    # therefore holds consumed data and must NEVER be a whole-dir deletion
    # candidate — even if a crash left status="in-progress" or no manifest.
    #
    # This short-circuits to COMPLETE *before* status_classification is
    # consulted, because status_classification returns NON_COMPLETE for a
    # non-"complete" status (e.g. "in-progress") WITHOUT consulting
    # legacy_complete — so a zip-bearing crashed dump would be misclassified
    # NON_COMPLETE and rmtree'd if this delegated to it directly. Glob
    # "*.zip" (not "Z*.zip") so a dir holding only the non-Z admin zips
    # (HAC/PTK) — raw bronze data, though load never reads them — is kept too.
    if any(run_dir.glob("*.zip")):
        return prune.COMPLETE, "contains PSN zip(s) (irreplaceable; keep)"
    # Only a zip-less shell reaches here — a crash before the first file
    # landed, or an abandoned in-progress dir. There is nothing to keep, so
    # defer to the status lifecycle: a "complete" marker keeps it, anything
    # else (in-progress / statusless / no manifest) is NON_COMPLETE. The
    # legacy_complete predicate echoes the has-zip invariant above as a
    # belt-and-braces guard (it is False here since this branch is zip-less).
    return prune.status_classification(
        meta, run_dir=run_dir,
        legacy_complete=lambda rd, m: any(rd.glob("*.zip")))


CONFIG = prune.PruneConfig(
    debug_subdirs=(),                 # no bronze-resident debug artefacts
    is_complete=_is_complete,
    manifest_name="run.json",         # read for status + UNKNOWN-on-corrupt
    dump_label="dump",
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


def validate_target(path, bronze_dir):
    """Back-compat shim for the local test suite: bind the shared
    validator to this collector's config."""
    return prune.validate_target(path, bronze_dir, CONFIG)


if __name__ == "__main__":
    sys.exit(main())
