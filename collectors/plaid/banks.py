#!/usr/bin/env python3
"""
List the banks linked through Plaid: each Item's name, its institution,
when it was linked, its newest download, and whether that download is
loaded.

It reads only files on this computer: the Items' token files in the
secrets dir (never the token itself), the runs in the data dir and each
Item's silver database, opened read-only. It asks Plaid nothing, so it
needs no keys and costs nothing. `login --check` asks Plaid whether
each link is still healthy.

Usage:
    banks.py [--sandbox]
"""

from __future__ import annotations

import argparse
import datetime
import sqlite3
import sys
from pathlib import Path

from collectorkit import bronze, cli

import items
import trees
from link import say


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument(
        "--secrets-dir", type=Path, default=Path.home() / ".secrets",
        help="Where the Items' token files live (default ~/.secrets).")
    p.add_argument(
        "--bronze-dir", type=Path, required=True,
        help="The data dir: one tree per Item below it.")
    p.add_argument(
        "--sandbox", action="store_true",
        help="List the Items made in Plaid's Sandbox instead.")
    cli.add_common_args(p)
    return p.parse_args(argv)


def _day(stamp: str | None) -> str:
    """The day of an ISO time, as the run and token files write it."""
    return (stamp or "")[:10] or "-"


def _run_day(run: Path) -> str:
    when = datetime.datetime.fromtimestamp(bronze.parse_run_ts(run.name),
                                           datetime.timezone.utc)
    return when.strftime("%Y-%m-%d")


def _loaded(db_path: Path) -> set[int]:
    """The runs the Item's silver holds; empty when there is no silver.
    Opened read-only, so the listing never writes to it."""
    if not db_path.exists():
        return set()
    try:
        conn = sqlite3.connect(f"{db_path.resolve().as_uri()}?mode=ro",
                               uri=True)
        try:
            return {int(r[0]) for r in conn.execute(
                "SELECT snapshot_at FROM dump_runs")}
        finally:
            conn.close()
    except sqlite3.Error:
        return set()


def state(data_dir: Path, name: str) -> str:
    """Where one Item's data stands: no download, downloaded and not
    loaded, or loaded, with the day of the newest download."""
    tree = data_dir / name
    runs = [r for r in bronze.iter_run_dirs(tree)
            if (bronze.read_manifest(r / trees.RUN_FILE) or {}).get("status")
            == trees.COMPLETE]
    if not runs:
        return "not downloaded yet"
    newest = runs[-1]
    if bronze.parse_run_ts(newest.name) in _loaded(tree / f"{name}.db"):
        return f"downloaded {_run_day(newest)}, loaded"
    return f"downloaded {_run_day(newest)}, not loaded yet"


def report(secrets_dir: Path, data_dir: Path, environment: str) -> int:
    linked = items.of_environment(secrets_dir, environment)
    sandbox = " --sandbox" if environment == "sandbox" else ""
    if not linked:
        say(f"No bank is linked through Plaid yet ({environment}). Link one:")
        say()
        say(f"    wealthdb-collect plaid link --item <name>{sandbox}")
        say()
        say("<name> is a short name of your choice for the bank, such as "
            "chase.")
        return 0
    say(f"Banks linked through Plaid ({environment}):")
    say()
    width = max(len(i.name) for i in linked)
    bank = max(len(i.institution_name or "-") for i in linked)
    for i in linked:
        say(f"  {i.name:<{width}}  {i.institution_name or '-':<{bank}}  "
            f"linked {_day(i.linked_at)}  {state(data_dir, i.name)}")
    left, _ = items.open_sign_ins(secrets_dir, environment)
    if left:
        say()
        for p in left:
            say(f"A sign-in for {p.name} is left open, started "
                f"{_day(p.created_at)}. `wealthdb-collect plaid login"
                f"{sandbox}` settles it.")
    say()
    say(f"The name in the first column is the one the other commands take, "
        f"as in --item {linked[0].name}.")
    say()
    say("To link another bank:")
    say(f"    wealthdb-collect plaid link --item <name>{sandbox}")
    say("To check that each link still works at Plaid:")
    say(f"    wealthdb-collect plaid login --check{sandbox}")
    return 0


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)
    environment = "sandbox" if args.sandbox else "production"
    try:
        return report(args.secrets_dir, args.bronze_dir, environment)
    except items.ItemStoreError as e:
        say(str(e))
        return 1


if __name__ == "__main__":
    sys.exit(main())
