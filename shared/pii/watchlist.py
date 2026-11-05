#!/usr/bin/env python3
"""watchlist — the private terms pii_check.py searches for, read from the
local gold database.

A real account number in a fixture looks like a synthetic one. Only the
local data tells them apart, so this script lists that data's own
identifiers and names, and pii_check.py refuses any change that holds
one.

It reads every account and portfolio the engine reports, today and on
the last day of each earlier year back to --since, through `wealthdb
holdings <view> -f json`. The years end at the first one with no
accounts. It keeps:

- every account, portfolio and relationship id with three or more
  digits;
- every account and portfolio name, and every nickname, that holds a
  word beyond the generic ones. "Joint savings" and a lone ordinary word
  are left out, since they would match the documentation;
- the host name and $WEALTHDB_DATA_ROOT.

It writes them to ${XDG_CONFIG_HOME:-~/.config}/wealthdb-pii/
watchlist.txt, mode 0600 in a 0700 directory, and prints only counts.
The file never enters the repository. People, entities, places and
holdings that no account name carries go into curated.txt beside it, by
hand.

usage: watchlist.py [--wealthdb CMD] [-c CONFIG] [--since YEAR] [--out FILE]
"""
from __future__ import annotations

import argparse
import datetime
import json
import os
import re
import socket
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
sys.path.insert(0, str(HERE))
from pii_check import CONFIG_DIR  # noqa: E402

VIEWS = {
    "accounts": ("account", "account_id", "relationship_id",
                 "account_nickname", "portfolio_external_id"),
    "portfolios": ("portfolio", "portfolio_id", "relationship_id",
                   "portfolio_nickname"),
}
ID_KEYS = frozenset({"account_id", "relationship_id", "portfolio_id",
                     "portfolio_external_id"})

# Words that name a kind of account, a currency or a tax wrapper rather
# than an account. A name of these words alone is left out.
GENERIC = frozenset("""
a an and the of for to my our his her their own new old main primary
secondary other account accounts acct checking current savings saving
joint individual personal private business household family card cards
credit debit rewards prepaid charge loan loans home mortgage term deposit
deposits fixed notice month months year years day days brokerage custody
cash money market portfolio portfolios mandate discretionary advisory
managed self directed investment investments wealth trading margin
retirement pension pillar vested benefits ira roth sep simple traditional
rollover hsa esa utma ugma trust estate education college plan plans
fund funds giro privatkonto sparkonto konto depot sparen vorsorge
usd chf eur gbp jpy cad aud bank union association building society
crypto bitcoin wallet exchange digital securities services investor
ventures venture property residence house travel holiday health
emergency reserve spending everyday bills income salary tax taxes
""".split())

# A one-word name that is an ordinary word would match prose everywhere.
DICTIONARY = Path("/usr/share/dict/words")


def institution_words(repo: Path) -> frozenset[str]:
    """The collectors' directory names: the institutions a name may
    carry without saying anything private."""
    root = repo / "collectors"
    if not root.is_dir():
        return frozenset()
    return frozenset(w for d in root.iterdir() if d.is_dir()
                     for w in re.split(r"[-_]", d.name.lower()) if w)


def common_words() -> frozenset[str]:
    """The system dictionary's lower-case entries, or none without one.
    A capitalised entry is a proper name and stays out."""
    if not DICTIONARY.is_file():
        return frozenset()
    return frozenset(w for w in DICTIONARY.read_text(errors="replace").split()
                     if w.islower())


def generic(name: str, institutions: frozenset[str],
            common: frozenset[str] = frozenset()) -> bool:
    """Whether the name says nothing beyond kinds, currencies and
    institutions: every word of three or more letters is generic, or the
    name is one ordinary word."""
    words = [w for w in re.split(r"[^a-z]+", name.lower()) if len(w) > 2]
    if len(words) == 1 and words[0] in common:
        return True
    return all(w in GENERIC or w in institutions for w in words)


def holdings(cmd: list[str], view: str, as_of: str) -> list[dict]:
    run = subprocess.run(
        [*cmd, "holdings", view, "-d", as_of, "-f", "json",
         "-C", ",".join(VIEWS[view])],
        capture_output=True, text=True)
    if run.returncode != 0:
        raise SystemExit(f"watchlist: wealthdb holdings {view} -d {as_of} "
                         f"failed ({run.returncode}): {run.stderr.strip()}")
    rows = json.loads(run.stdout or "[]")
    return rows if isinstance(rows, list) else []


def collect(cmd: list[str], since: int, today: datetime.date,
            institutions: frozenset[str],
            common: frozenset[str] = frozenset()) -> tuple[set[str], set[str]]:
    """The ids and names of every account and portfolio as of today and
    each earlier year end, back to since or the first empty year."""
    ids: set[str] = set()
    names: set[str] = set()
    dates = [today.isoformat()] + [f"{y}-12-31"
                                   for y in range(today.year - 1, since - 1, -1)]
    for i, as_of in enumerate(dates):
        rows = [r for view in VIEWS for r in holdings(cmd, view, as_of)]
        if i and not any(k in r for r in rows for k in ID_KEYS):
            break
        for row in rows:
            for key, value in row.items():
                if value is None:
                    continue
                value = str(value).strip()
                if key in ID_KEYS:
                    if sum(c.isdigit() for c in value) >= 3:
                        ids.add(value)
                elif value and not value.startswith("(") \
                        and not generic(value, institutions, common):
                    names.add(value)
    return ids, names


def environment() -> set[str]:
    terms = {socket.gethostname().split(".")[0]}
    if os.environ.get("WEALTHDB_DATA_ROOT"):
        terms.add(os.environ["WEALTHDB_DATA_ROOT"])
    return {t for t in terms if len(t) >= 4 and t.lower() != "localhost"}


def write(out: Path, terms: list[str], today: datetime.date) -> None:
    fresh = not out.parent.exists()
    out.parent.mkdir(parents=True, exist_ok=True)
    if fresh or out.parent == CONFIG_DIR:
        os.chmod(out.parent, 0o700)
    fd = os.open(out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(f"# Written by watchlist.py on {today}; rerun it rather than"
                " editing.\n# Hand-picked terms go into curated.txt.\n")
        f.writelines(t + "\n" for t in terms)
    os.chmod(out, 0o600)


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        prog="watchlist.py",
        description="Write the private watchlist from the local gold database.")
    ap.add_argument("--wealthdb", default=str(REPO / "wealthdb" / "wealthdb"),
                    metavar="CMD", help="the engine wrapper (default: this checkout's)")
    ap.add_argument("-c", "--config", metavar="CONFIG",
                    help="the engine config (default: the engine's own)")
    ap.add_argument("--since", type=int, default=2000, metavar="YEAR",
                    help="the earliest year end to read (default 2000)")
    ap.add_argument("--out", type=Path, default=CONFIG_DIR / "watchlist.txt",
                    metavar="FILE", help="the file to write")
    args = ap.parse_args(argv)

    cmd = [args.wealthdb, "-r", *(["-c", args.config] if args.config else [])]
    today = datetime.date.today()
    ids, names = collect(cmd, args.since, today, institution_words(REPO),
                         common_words())
    env = environment()
    write(args.out, sorted(ids | names | env), today)
    print(f"watchlist: {len(ids)} ids, {len(names)} names, {len(env)} host and"
          f" path terms written to {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
