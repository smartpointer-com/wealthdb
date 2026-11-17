"""Two channels that state one ledger: where each one's rows belong.

A card or bank account is often read twice: a structured export (or the
site's activity feed) for the recent past, and statement PDFs that reach
further back. The export is the richer record, so it owns the span it
reaches, and a statement fills in only outside it.

* :func:`seams` is, per account, the oldest day the export reaches, and
  :func:`reach` the newest.
* :func:`period_decision` says what one statement period is to the
  export: rows to import, a span the export covers, a straddle that
  belongs to neither, or unbounded because no export is loaded yet.
* :func:`pair_one_to_one` matches the rows two channels both carry, when
  they share no id: the same account, the same amount, a few days apart,
  each row claimed at most once.

Each loader passes its own sources, tolerance and window.
"""
from __future__ import annotations

import re
import sqlite3
from collections.abc import Callable, Iterable, Sequence
from typing import NamedTuple

IMPORT = "import"
COVERED = "covered"
STRADDLE = "straddle"
UNBOUNDED = "unbounded"

_IDENTIFIER = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")


def _bound(conn: sqlite3.Connection, func: str, table: str, date_col: str,
           sources: Sequence[str], account_col: str,
           source_col: str) -> dict[str, int]:
    for name in (table, date_col, account_col, source_col):
        if not _IDENTIFIER.match(name):
            raise ValueError(f"not an SQL identifier: {name!r}")
    marks = ",".join("?" * len(sources))
    return {account: day for account, day in conn.execute(
        f"SELECT {account_col}, {func}({date_col}) FROM {table} "
        f"WHERE {source_col} IN ({marks}) GROUP BY {account_col}",
        tuple(sources)) if day is not None}


def seams(conn: sqlite3.Connection, table: str, date_col: str,
          primary_sources: Sequence[str], *,
          account_col: str = "account_external_id",
          source_col: str = "source") -> dict[str, int]:
    """Each account's oldest ``date_col`` among the rows of the primary
    channel (``source_col`` in ``primary_sources``): the seam below which
    the other channel owns the ledger.

    Anchored to the primary channel's rows only, so importing the other
    channel never moves a seam. Per account, since export depth differs by
    account. An account with no primary row is absent."""
    return _bound(conn, "MIN", table, date_col, primary_sources,
                  account_col, source_col)


def reach(conn: sqlite3.Connection, table: str, date_col: str,
          primary_sources: Sequence[str], *,
          account_col: str = "account_external_id",
          source_col: str = "source") -> dict[str, int]:
    """Each account's newest ``date_col`` among the primary channel's rows:
    the seam's upper twin. The primary channel proves coverage only up to
    its newest row."""
    return _bound(conn, "MAX", table, date_col, primary_sources,
                  account_col, source_col)


def period_decision(start, end, seam, reach, oldest) -> str:
    """What one statement period ``start`` to ``end`` is to the primary
    channel:

    * ``"unbounded"``: there is no seam, since the account has no primary
      row yet. Importing would put a second copy of every row the first
      export lands beside it.
    * ``"import"``: the period closes before the seam. Only the statement
      holds its rows.
    * ``"covered"``: the period opens at or after the seam and closes at or
      before ``reach``, so the primary channel carries every row of it. A
      ``reach`` of None bounds nothing above. The ``oldest`` period is
      never covered, because its start is not known. A caller that chains
      each start from the period before passes the oldest period's end;
      None is no such period.
    * ``"straddle"``: anything else. The period opens before the seam and
      closes after it, runs past the reach, or is the oldest. Its rows
      belong to neither channel.

    The gate is on the whole period, not each row: a statement dates a row
    by transaction date and the cycle bills it by post date, so a row gate
    could land one charge from both channels."""
    if seam is None:
        return UNBOUNDED
    if end < seam:
        return IMPORT
    if start >= seam and (reach is None or end <= reach) and end != oldest:
        return COVERED
    return STRADDLE


class Leg(NamedTuple):
    """One row offered for pairing: its id, its account, when it happened
    (epoch seconds), its signed amount, and what kind of row it is."""
    id: str
    account: str
    at: int
    amount: float
    kind: str | None = None


def pair_one_to_one(left: Iterable[Leg], right: Iterable[Leg],
                    same_amount: Callable[[float, float], bool],
                    window_days: float | Callable[[Leg], float],
                    claims: dict[str, str] | None = None) -> dict[str, str]:
    """Pair each left row with the right row that carries the same
    movement, and return ``{left id: right id}`` for the rows paired.

    A right row is a candidate when it has the left row's account, an
    amount ``same_amount`` accepts, and lies within ``window_days`` (a
    number, or a function of the left row). Candidates are taken
    closest in time first, then by id, and the first one no left row has
    claimed wins. A left row seen again (one payment printed on two
    statements, under one id) keeps the claim it made.

    One-to-one, since a resemblance is not an identity: two payments can
    share an account, a day and an amount, and then the one right row that
    carries one of them must not absorb both.

    ``claims`` continues an earlier call's pairing, and is updated in
    place: a pass that pairs statement by statement threads one through."""
    claims = {} if claims is None else claims
    taken = set(claims.values())
    by_account: dict[str, list[Leg]] = {}
    for leg in right:
        by_account.setdefault(leg.account, []).append(leg)
    for leg in left:
        if leg.id in claims:
            continue
        days = window_days(leg) if callable(window_days) else window_days
        window = days * 86400
        candidates = sorted(
            (r for r in by_account.get(leg.account, ())
             if same_amount(leg.amount, r.amount)
             and abs(r.at - leg.at) <= window),
            key=lambda r: (abs(r.at - leg.at), r.id))
        for r in candidates:
            if r.id not in taken:
                claims[leg.id] = r.id
                taken.add(r.id)
                break
    return claims

