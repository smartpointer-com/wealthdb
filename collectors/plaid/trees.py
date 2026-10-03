"""
The data dir: one tree per Item, one directory per run of `download`.

    <data-dir>/<item>/<UTC-ts>/run.json, ...

A tree holds the runs of one Item. Every run.json names the tree it
belongs to (`item`) and carries the Item's `item_id` and `environment`
from its first write, so a run can always be traced to its Item.

The checks here keep a mistyped data dir from costing data. Pointed at
the data root instead of plaid's own dir, every other collector's dir
would pass for an Item tree. `download` would then write into it,
`prune` would delete its runs, and `load` would open its silver. All
three refuse a tree that holds a run that is not plaid's. `download` and
`load` also refuse one that holds another Item's runs.

This module also holds the vocabulary of run.json. `download` writes
run.json and `load` reads it, so both use these names.
"""

from __future__ import annotations

from pathlib import Path

from collectorkit import bronze

import items

RUN_FILE = "run.json"
ITEM_FILE = "item.json"

# A run's own status: in-progress until its last write; failed when its
# Item could not be read. Only a complete run is a load input.
IN_PROGRESS = "in-progress"
COMPLETE = "complete"
RUN_FAILED = "failed"

# What a product's entry in run.json says about it.
FETCHED = "fetched"          # read in full; its files are in the run
PARTIAL = "partial"          # read, but Plaid holds only part of the history
NOT_LINKED = "not_linked"    # the Item was not linked with it; not asked
ABSENT = "absent"            # Plaid says the Item has nothing for it
NOT_READY = "not_ready"      # Plaid was still assembling it
FAILED = "failed"            # asked, and the read did not succeed

# The statuses that leave nothing to do.
SETTLED = frozenset({FETCHED, NOT_LINKED, ABSENT})

# What run.json's `refresh` record says of a refresh `--refresh` asked
# for. From above: NOT_LINKED, an Item without investments, which is not
# asked; ABSENT, Plaid's word that the Item has no investment account;
# FAILED, Plaid's word that its fetch did not succeed.
REFRESHED = "refreshed"      # Plaid reports a fetch since the request
REFUSED = "refused"          # Plaid refused the request (a 4xx answer)
UNCONFIRMED = "unconfirmed"  # Plaid reported no fetch within the wait

# The refresh outcomes that leave nothing to do.
REFRESH_SETTLED = frozenset({REFRESHED, NOT_LINKED, ABSENT})

# The two ledgers, by product, with the id of a row.
LEDGER_IDS = {"investment_transactions": "investment_transaction_id",
              "transactions": "transaction_id"}


class TreeError(Exception):
    """A tree holds runs that are not this collector's runs of its Item,
    or the silver beside them is not that Item's."""


def listing(lines: list[str], limit: int = 5) -> str:
    """The first `limit` lines, indented, and a count of the rest."""
    text = "\n  ".join(lines[:limit])
    if len(lines) > limit:
        text += f"\n  ... and {len(lines) - limit} more"
    return text


def select(data_dir: Path, names: list[str] | None) -> list[Path]:
    """The Item trees a verb works on: every one, or the ones `names`
    lists. A name with no tree is refused."""
    found = item_trees(data_dir)
    if not names:
        return found
    missing = sorted(set(names) - {d.name for d in found})
    if missing:
        raise SystemExit(f"no Item tree under {data_dir}: "
                         f"{', '.join(missing)}")
    return [d for d in found if d.name in names]


def check_data_dir(data_dir: Path, outcome: str) -> None:
    """Exit unless every Item tree under `data_dir` holds plaid's runs
    only. Every tree is checked, not only the ones a verb works on. One
    tree that is not plaid's means the dir itself is the wrong one.
    `outcome` is what then did not happen, such as `loaded`."""
    found = [line for tree in item_trees(data_dir) for line in strangers(tree)]
    if found:
        raise SystemExit(
            f"{data_dir} holds runs that are not plaid's, so nothing was "
            f"{outcome}:\n  {listing(found)}\n--bronze-dir (the wrapper's "
            f"--data-dir) must be plaid's own data dir, <data-root>/plaid.")


def missing_files(run: Path, products: dict) -> list[str]:
    """The files a run's manifest lists, item.json included, that the run
    does not hold."""
    listed = [ITEM_FILE] + [name for entry in products.values()
                            for name in entry.get("files") or []]
    return [name for name in listed if not (run / name).is_file()]


def item_trees(data_dir: Path) -> list[Path]:
    """The directories under `data_dir` that can be Item trees: named
    like an Item, and not a link. No tree when `data_dir` is not a dir."""
    if not Path(data_dir).is_dir():
        return []
    return sorted(d for d in Path(data_dir).iterdir()
                  if d.is_dir() and not d.is_symlink()
                  and items.ITEM_NAME_RE.match(d.name))


def is_own(run: Path, meta: dict | None) -> bool:
    """Whether `meta` makes `run` a run of the tree that holds it."""
    return meta is not None and meta.get("item") == run.parent.name


def stopped_at_start(run: Path) -> bool:
    """A run that holds nothing but, at most, the temporary file of its
    first run.json: a download that stopped before its first write."""
    try:
        return {p.name for p in run.iterdir()} <= {RUN_FILE + ".tmp"}
    except OSError:
        return False


def strangers(tree: Path, item: items.Item | None = None) -> list[str]:
    """One line for each run in `tree` that is not plaid's run of the
    tree's Item. With `item`, a run of another Item of the same name (a
    name used again, or the other environment) counts as well.

    A run.json that exists and cannot be read proves nothing either way.
    It is not listed here, and `prune` keeps its run."""
    found = []
    for run in bronze.iter_run_dirs(tree):
        meta = bronze.read_manifest(run / RUN_FILE)
        if meta is None:
            if not (run / RUN_FILE).exists() and not stopped_at_start(run):
                found.append(f"{run}: holds files and no run.json")
        elif not is_own(run, meta):
            found.append(f"{run}: its run.json names no plaid run of "
                         f"{tree.name!r}")
        elif item is not None and meta.get("item_id") is not None and (
                meta.get("item_id") != item.item_id
                or meta.get("environment") != item.environment):
            found.append(f"{run}: a run of another Item named "
                         f"{tree.name!r} ({meta.get('environment')})")
    return found


def identity(tree: Path) -> tuple[str, str] | None:
    """The (item_id, environment) of the Item whose runs `tree` holds, or
    None when it holds no readable run yet. Raises TreeError when a run is
    not plaid's run of this tree, or when the runs name two Items."""
    held = {(meta.get("item_id"), meta.get("environment"))
            for run in bronze.iter_run_dirs(tree)
            if is_own(run, meta := bronze.read_manifest(run / RUN_FILE))
            and meta.get("item_id")}
    found = strangers(tree)
    if len(held) > 1:
        found.append(f"{tree}: runs of {len(held)} Items")
    if found:
        raise TreeError(f"{tree} holds runs that are not one Item's, so it "
                        f"is left as it is:\n  {listing(found)}")
    return next(iter(held), None)


def check(tree: Path, item: items.Item) -> None:
    """Raise TreeError unless every run in `tree` is a run of `item`."""
    found = strangers(tree, item)
    if found:
        raise TreeError(
            f"{item.name}: {tree} holds runs that are not this Item's, so "
            f"nothing is written there:\n  {listing(found)}\nThe data dir "
            f"must be plaid's own (<data-root>/plaid). A name used again "
            f"for another Item needs its old tree moved aside first.")
