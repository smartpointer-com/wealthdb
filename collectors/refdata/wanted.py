"""
The wanted list: what gold asks this collector for.

The engine prints it (`wealthdb -r refdata-wanted`, a hidden read-only
subcommand); `--wanted FILE` reads a saved copy instead, for tests and
for a run on another machine. It is a versioned JSON contract
(wealthdb/docs/adapters/refdata.md §5): this module refuses a contract
or a version it does not know rather than guess at its fields.
"""

from __future__ import annotations

import json
import subprocess
from dataclasses import dataclass, field
from datetime import date
from pathlib import Path

CONTRACT = "refdata-wanted"
VERSIONS = (1,)


class WantedError(Exception):
    """The wanted list cannot be had or read."""


@dataclass(frozen=True)
class Fund:
    ref: str
    isin: str | None = None
    cusip: str | None = None
    symbol: str | None = None
    name: str | None = None
    currency: str | None = None          # in the contract (adapters/refdata.md §5); no provider reads it
    roles: tuple = ()
    held_from: date | None = None
    held_to: date | None = None          # None: still held
    compositions: tuple = ()             # dates gold has, ascending


@dataclass(frozen=True)
class Security:
    key: str
    isin: str | None = None
    cusip: str | None = None
    symbol: str | None = None
    name: str | None = None
    currency: str | None = None
    scope: str = "direct"                 # direct | constituent
    share_pct: float | None = None        # a constituent's share of net worth through funds
    asset_class: str | None = None
    missing: tuple = ()
    lookups: dict = field(default_factory=dict)   # provider -> date of its last answer


@dataclass(frozen=True)
class Wanted:
    as_of: date
    max_gap_days: int
    funds: tuple
    securities: tuple


def _date(v):
    return date.fromisoformat(v) if v else None


def parse(doc) -> Wanted:
    if not isinstance(doc, dict) or doc.get("contract") != CONTRACT:
        raise WantedError(f"not a {CONTRACT} document")
    if doc.get("version") not in VERSIONS:
        raise WantedError(f"{CONTRACT} version {doc.get('version')!r} is not one this collector reads "
                          f"({', '.join(map(str, VERSIONS))}); update the collector or the engine so they agree")
    try:
        funds = tuple(Fund(
            ref=f["ref"], isin=f.get("isin"), cusip=f.get("cusip"), symbol=f.get("symbol"),
            name=f.get("name"), currency=f.get("currency"), roles=tuple(f.get("roles") or ()),
            held_from=_date(f.get("held_from")), held_to=_date(f.get("held_to")),
            compositions=tuple(date.fromisoformat(d) for d in f.get("compositions") or ()))
            for f in doc.get("funds") or ())
        securities = tuple(Security(
            key=s["key"], isin=s.get("isin"), cusip=s.get("cusip"), symbol=s.get("symbol"),
            name=s.get("name"), currency=s.get("currency"), scope=s.get("scope") or "direct",
            share_pct=float(s["share_pct"]) if s.get("share_pct") is not None else None,
            asset_class=s.get("asset_class"),
            missing=tuple(s.get("missing") or ()),
            lookups={k: date.fromisoformat(v) for k, v in (s.get("lookups") or {}).items()})
            for s in doc.get("securities") or ())
        return Wanted(as_of=date.fromisoformat(doc["as_of"]),
                      max_gap_days=int(doc.get("settings", {}).get("max_gap_days", 400)),
                      funds=funds, securities=securities)
    except (KeyError, TypeError, ValueError) as e:
        raise WantedError(f"{CONTRACT} document is malformed: {e}") from e


def from_file(path: Path) -> Wanted:
    try:
        return parse(json.loads(Path(path).read_text(encoding="utf-8")))
    except OSError as e:
        raise WantedError(f"{path}: {e}") from e
    except ValueError as e:
        raise WantedError(f"{path} is not JSON: {e}") from e


def from_engine(engine: str, timeout: float = 600) -> Wanted:
    """Ask the engine: `<engine> -r refdata-wanted`. The engine finds its
    config and data root the way it always does, from the environment.
    An engine that cannot run, or runs past `timeout` seconds, is a
    WantedError like any other."""
    try:
        out = subprocess.run([engine, "-r", "refdata-wanted"], capture_output=True, text=True,
                             timeout=timeout, check=False)
    except FileNotFoundError as e:
        raise WantedError(f"engine {engine!r} not found; set REFDATA_WEALTHDB or `engine` in "
                          f"refdata.cfg, or pass --wanted FILE") from e
    except (OSError, subprocess.TimeoutExpired) as e:
        raise WantedError(f"engine {engine!r} could not run: {e}") from e
    if out.returncode != 0:
        # An engine without the subcommand prints its whole usage after
        # the line that says so.
        err = (out.stderr or out.stdout).strip()
        if "unknown subcommand" in err:
            raise WantedError(f"engine {engine!r} has no refdata-wanted subcommand; it is older than "
                              f"this collector")
        tail = err.splitlines()[-3:]
        raise WantedError(f"engine {engine!r} failed ({out.returncode}): {' | '.join(tail)}")
    try:
        return parse(json.loads(out.stdout))
    except ValueError as e:
        raise WantedError(f"engine {engine!r} printed no JSON: {e}") from e
