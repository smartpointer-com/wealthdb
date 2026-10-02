#!/usr/bin/env python3
"""
Prune the plaid bronze tree: one pass over each Item's runs.

Thin wrapper over :mod:`collectorkit.prune`, the shared and unit-tested
prune engine, run once per Item tree under ``--bronze-dir``. Two kinds of
path are removed from an Item's runs:

* ``<run>/screenshots/`` — the HTTP trace ``download --debug`` writes.
  It is not a load input, so removing it leaves silver unchanged.

* whole runs that did not complete: their ``run.json`` holds a
  ``status`` other than ``"complete"``, or the run stopped before its
  first ``run.json``. That is a download that stopped early or is still
  running (``"in-progress"``), or one whose Item could not be read
  (``"failed"``). Only a complete run is a load input.

Every other file of a complete run is a load input and is never touched,
nor is anything in an Item tree that is not a run directory. A run whose
``run.json`` cannot be read is left alone. A run that did not complete
and was written within ``--min-age-hours`` (default 1) is left alone too.
A download writes each product's files once it has read them, and its
longest wait on Plaid is minutes. A download in flight is therefore safe.
Should one be removed all the same, the download notices the files it
lost and does not mark its run complete. ``--dry-run`` prints the plan
and removes nothing.

``--bronze-dir`` is plaid's own data dir, ``<data-root>/plaid``. Every
tree is checked before anything is removed. A tree that holds a run that
is not plaid's run of that tree means the dir is a wrong one, such as the
data root itself, and then nothing is removed anywhere.

Usage:
    prune.py --bronze-dir DIR [--item NAME ...] [--dry-run]
             [--min-age-hours N]
"""

from __future__ import annotations

import sys

from collectorkit import cli, debugcap, prune

import trees


def _is_complete(run_dir, meta):
    # The engine has already set aside a run.json that cannot be read.
    if meta is None:
        if trees.stopped_at_start(run_dir):
            return prune.NON_COMPLETE, "stopped before its first run.json"
        return prune.UNKNOWN, "no plaid run.json"
    if not trees.is_own(run_dir, meta):
        return prune.UNKNOWN, "not a plaid run of this tree"
    return prune.status_classification(meta)


CONFIG = prune.PruneConfig(
    # The one debug artefact a run writes: `download --debug`'s HTTP trace.
    # Every other file in a complete run is a load input.
    debug_subdirs=(debugcap.SCREENSHOTS_DIR,),
    is_complete=_is_complete,
)


def main(argv=None) -> int:
    # plaid writes no debug output outside its runs, so the shared flag
    # that reclaims such output does not exist here.
    parser = prune.build_parser(__doc__, debug_dir=False)
    parser.add_argument(
        "--item", metavar="NAME", action="append",
        help="Prune only this Item's runs. Repeat for more. Default: every "
             "Item tree under --bronze-dir.")
    args = parser.parse_args(argv)
    cli.configure_logging(args.verbose)
    if not args.bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {args.bronze_dir}")
    found = trees.select(args.bronze_dir, args.item)
    trees.check_data_dir(args.bronze_dir, "removed")
    if not found:
        print("nothing to prune")
        return 0
    status = 0
    for tree in found:
        print(f"== {tree.name}")
        status = max(status, prune.run(
            CONFIG, tree, dry_run=args.dry_run,
            min_age_hours=args.min_age_hours))
    return status


def validate_target(path, bronze_dir):
    """The shared validator, bound to this collector's config, for the
    tests."""
    return prune.validate_target(path, bronze_dir, CONFIG)


if __name__ == "__main__":
    sys.exit(main())
