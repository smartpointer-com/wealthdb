"""
The data dir: one tree per Item, one directory per run of `download`.

    <data-dir>/<item>/<UTC-ts>/run.json, ...

A tree holds the runs of one Item. Every run.json names the tree it
belongs to (`item`) and carries the Item's `item_id` and `environment`
from its first write, so a run can always be traced to its Item.

The checks here keep a mistyped data dir from costing data. Pointed at
the data root instead of plaid's own dir, every other collector's dir
would pass for an Item tree. `download` would then write into it, and
`prune` would delete its runs. Both refuse a tree that holds a run that
is not plaid's run of that tree's Item.
"""

from __future__ import annotations

import json
from pathlib import Path

from collectorkit import bronze

import items

RUN_FILE = "run.json"


class TreeError(Exception):
    """A tree holds runs that are not this collector's runs of its Item."""


def item_trees(data_dir: Path) -> list[Path]:
    """The directories under `data_dir` that can be Item trees: named
    like an Item, and not a link."""
    return sorted(d for d in Path(data_dir).iterdir()
                  if d.is_dir() and not d.is_symlink()
                  and items.ITEM_NAME_RE.match(d.name))


def manifest(run: Path) -> dict | None:
    """The run's run.json, or None when it is missing or unreadable."""
    try:
        doc = json.loads((run / RUN_FILE).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return doc if isinstance(doc, dict) else None


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
        meta = manifest(run)
        if meta is None:
            if not (run / RUN_FILE).exists() and not stopped_at_start(run):
                found.append(f"{run}: holds files and no run.json")
        elif not is_own(run, meta):
            found.append(f"{run}: its run.json names no plaid run of "
                         f"{tree.name!r}")
        elif item is not None and (
                meta.get("item_id") != item.item_id
                or meta.get("environment") != item.environment):
            found.append(f"{run}: a run of another Item named "
                         f"{tree.name!r} ({meta.get('environment')})")
    return found


def check(tree: Path, item: items.Item) -> None:
    """Raise TreeError unless every run in `tree` is a run of `item`."""
    found = strangers(tree, item)
    if found:
        raise TreeError(
            f"{item.name}: {tree} holds runs that are not this Item's, so "
            f"nothing is written there:\n  " + "\n  ".join(found[:5])
            + (f"\n  ... and {len(found) - 5} more" if len(found) > 5 else "")
            + "\nThe data dir must be plaid's own (<data-root>/plaid). A "
              "name used again for another Item needs its old tree moved "
              "aside first.")
