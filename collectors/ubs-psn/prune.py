#!/usr/bin/env python3
"""
Prune debug artefacts and non-complete dumps from the ubs-psn bronze
tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested
prune engine) with ubs-psn's configuration. A run dir holds
``<ORDERTYPE>.zip`` files, a ``run.json`` status marker, and a
``listing.json`` recording what the server was offering pre-pull — so
this verb is deliberately narrow: it reclaims a legacy
``screenshots/sftp-listing.txt`` capture (the pre-``listing.json`` form
of that record) and a **zip-less shell** (a run dir minted before the
first file arrived and then abandoned, or a pull that found nothing
queued).

The scope is narrow because a queue zip is consumed by its own fetch:
UBS deletes ``download/<OT>/<OT>.zip`` from its server the moment it is
downloaded, its dated archive copy ages out after roughly two months
(``download --recover`` replays it inside that window), and ``load``
ingests whatever ``*.zip`` are present regardless of whether the dump
finished. A run dir holding *any* zip therefore holds genuinely
consumed data that no marker can distinguish from a clean dump, so it
is classified COMPLETE and never a deletion candidate — even when a
crash left ``run.json`` at ``"in-progress"`` or wrote no manifest at
all. The has-zip check short-circuits **before** the ``status`` field
is consulted; only a truly zip-less shell defers to the status
lifecycle. ``listing.json`` is provenance, not a debug artefact: from a
dump that holds zips it is never deleted; it goes only when a zip-less
shell is removed whole.

``debug_subdirs`` nominates ``screenshots/`` — the legacy listing
capture, never a ``load`` input, so reclaiming it from a complete dump
leaves silver byte-identical. The zips themselves are the load inputs
and are never touched. Beyond that the sole deletion path is a whole
zip-less run dir once it is quiescent: a crash shell, or the
``status="empty"`` dump any pull leaves when nothing was queued (kept
so its listing.json is inspectable until reclaimed here). ``--dry-run``
creates no run dir at all. The verb exists so a fleet-wide ``prune`` is
guaranteed never to delete a ubs-psn ``load`` input, and so its help
and behaviour match every other collector's.

An unreadable or corrupt ``run.json`` is UNKNOWN and never deleted. An
in-flight guard skips non-complete shells written within
``--min-age-hours`` (default 1), keyed on the newest mtime in the dir.
``--dry-run`` prints the plan without removing anything.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import debugcap, prune


def _is_complete(run_dir, meta):
    # A ubs-psn queue zip is consumed by its own fetch (UBS deletes it
    # server-side; the dated archive copy behind it ages out after ~2
    # months), and `load` ingests whatever *.zip are present regardless of
    # dump completeness. A run dir holding ANY zip therefore holds consumed
    # data and must NEVER be a whole-dir deletion candidate — even if a
    # crash left status="in-progress" or no manifest.
    #
    # This short-circuits to COMPLETE *before* status_classification is
    # consulted, because status_classification returns NON_COMPLETE for a
    # non-"complete" status (e.g. "in-progress") WITHOUT consulting
    # legacy_complete — so a zip-bearing crashed dump would be misclassified
    # NON_COMPLETE and rmtree'd if this delegated to it directly. Glob
    # "*.zip" (not "Z*.zip") so a dir holding only the non-Z admin zips
    # (HAC/PTK) — raw bronze data, though load never reads them — is kept too.
    if any(run_dir.glob("*.zip")):
        return prune.COMPLETE, "contains PSN zip(s) (consumed data; keep)"
    # Only a zip-less shell reaches here — a crash before the first file
    # landed, an abandoned in-progress dir, or the status="empty" shell a
    # pull leaves when nothing was queued. No load input is at stake, so
    # defer to the status lifecycle: a "complete" marker keeps it,
    # anything else (empty / in-progress / statusless / no manifest) is
    # NON_COMPLETE. The legacy_complete predicate echoes the has-zip
    # invariant above as a belt-and-braces guard (it is False here since
    # this branch is zip-less).
    return prune.status_classification(
        meta, run_dir=run_dir,
        legacy_complete=lambda rd, m: any(rd.glob("*.zip")))


CONFIG = prune.PruneConfig(
    # The legacy `download --debug` SFTP-listing capture, which debugcap
    # landed under this subdir fleet-wide (the record now lives in each
    # run's listing.json, which is provenance and never nominated here).
    # The zips beside it are the load inputs.
    debug_subdirs=(debugcap.SCREENSHOTS_DIR,),
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
