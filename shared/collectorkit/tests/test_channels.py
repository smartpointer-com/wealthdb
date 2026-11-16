"""Tests for collectorkit.channels, each against the loader code it is
lifted from, run on invented rows: chase's card statement gate, amex's
statement import, fidelity-web's statement-to-feed match."""
from __future__ import annotations

import sqlite3
from datetime import date, timedelta
from decimal import Decimal
from pathlib import Path

import collector_modules
import pytest

from collectorkit import silver
from collectorkit.channels import (COVERED, IMPORT, STRADDLE, UNBOUNDED, Leg,
                                   pair_one_to_one, period_decision, reach,
                                   seams)
from collectorkit.parse import epoch_day

COLLECTORS = collector_modules.COLLECTORS


def _silver(collector: str, tmp_path: Path) -> sqlite3.Connection:
    conn = sqlite3.connect(str(tmp_path / f"{collector}.db"))
    silver.apply_migrations(conn, COLLECTORS / collector / "migrations")
    return conn


# ------------------------------------------------------------------
# period_decision on its own
# ------------------------------------------------------------------

def test_period_decision_outcomes():
    assert period_decision(1, 5, None, None, None) == UNBOUNDED
    assert period_decision(1, 5, 6, None, None) == IMPORT
    assert period_decision(1, 6, 6, None, None) == STRADDLE
    assert period_decision(6, 9, 6, None, None) == COVERED
    assert period_decision(6, 9, 6, 8, None) == STRADDLE      # past the reach
    assert period_decision(6, 8, 6, 8, None) == COVERED
    assert period_decision(6, 8, 6, 8, 8) == STRADDLE         # the oldest
    assert period_decision(1, 5, 6, 8, 5) == IMPORT           # oldest, below


# ------------------------------------------------------------------
# chase: the card statement pass
# ------------------------------------------------------------------

def _chase_card(chase_sp, start: date, end: date):
    """A card statement passing both gates: one purchase inside it."""
    amount = Decimal("12.34")
    zero = Decimal("0.00")
    return chase_sp.ParsedCardStatement(
        period_start=start, period_end=end, previous_balance=zero,
        payments_credits=zero, purchases=amount, cash_advances=zero,
        balance_transfers=zero, fees_charged=zero, interest_charged=zero,
        new_balance=amount,
        transactions=[chase_sp.StatementTxn(start + timedelta(days=1), amount,
                                            "SHOP", kind="STMT_PURCHASE")])


def test_chase_card_gate_is_period_decision(tmp_path, monkeypatch):
    chase = collector_modules.load("chase", "load")
    conn = _silver("chase", tmp_path)
    seam_day = date(2025, 3, 10)
    for i, day in enumerate((seam_day, date(2025, 6, 1))):
        chase._insert_transaction(conn, "card-a", {
            "fitid": f"export-{i}", "posted_at": epoch_day(day),
            "amount": -1.0, "kind": None, "description": "X",
            "check_number": None, "balance": None,
            "source": chase.SOURCE_CSV, "payload": {}})
    periods = [(date(2025, 1, 1), date(2025, 1, 31)),     # below the seam
               (date(2025, 2, 1), date(2025, 3, 9)),      # closes the day before
               (date(2025, 3, 1), date(2025, 3, 10)),     # closes on the seam
               (date(2025, 3, 10), date(2025, 4, 9)),     # opens on the seam
               (date(2025, 7, 1), date(2025, 7, 31))]     # past every export row
    by_acct = {acct: {end: (1700000000, _chase_card(chase.statement_parser,
                                                    start, end))
                      for start, end in periods}
               for acct in ("card-a", "card-b")}
    monkeypatch.setattr(chase, "_card_statements_in_tree",
                        lambda bronze_dir, card_ids: by_acct)
    chase.load_card_statements(conn, tmp_path, {"card-a", "card-b"})

    found = seams(conn, "transactions", "posted_at", chase.EXPORT_SOURCES)
    assert found == chase._export_seams(conn) == {"card-a": epoch_day(seam_day)}
    for acct in ("card-a", "card-b"):
        for start, end in periods:
            decision = period_decision(epoch_day(start), epoch_day(end),
                                       found.get(acct), None, None)
            (covered,) = conn.execute(
                "SELECT transactions_covered FROM statement_balances "
                "WHERE account_external_id=? AND period_end=?",
                (acct, epoch_day(end))).fetchone()
            (rows,) = conn.execute(
                "SELECT COUNT(*) FROM transactions WHERE source=? AND "
                "account_external_id=? AND fitid LIKE 'stmt_%' AND "
                "posted_at=?", (chase.SOURCE_STATEMENT, acct,
                                epoch_day(start + timedelta(days=1)))
            ).fetchone()
            assert (covered, rows) == {IMPORT: (1, 1), COVERED: (1, 0),
                                       STRADDLE: (0, 0),
                                       UNBOUNDED: (0, 0)}[decision], \
                (acct, start, end, decision)
    assert period_decision(0, 1, None, None, None) == UNBOUNDED


# ------------------------------------------------------------------
# amex: the statement import
# ------------------------------------------------------------------

def _amex_card(amex_sp, end: date):
    amount = Decimal("10.00")
    zero = Decimal("0.00")
    return amex_sp.ParsedCardStatement(
        period_end=end, previous_balance=zero, payments_credits=zero,
        new_charges=amount, fees=zero, interest_charged=zero,
        new_balance=amount,
        transactions=[amex_sp.StatementTxn(end - timedelta(days=3), "SHOP",
                                           amount, "STMT_PURCHASE")])


@pytest.mark.parametrize("activity", [
    (date(2025, 3, 15), date(2025, 6, 20)),
    (date(2025, 1, 31), date(2025, 7, 31)),
    (date(2025, 2, 1), date(2025, 2, 27)),
    None,
])
def test_amex_statement_import_is_period_decision(tmp_path, activity):
    amex = collector_modules.load("amex", "load")
    conn = _silver("amex", tmp_path)
    acct = "card-a"
    for i, day in enumerate(activity or ()):
        amex._insert_transaction(conn, acct, {
            "txn_id": f"act-{i}", "posted_at": epoch_day(day), "amount": -1.0,
            "kind": None, "description": "X", "merchant": None,
            "category": None, "category_code": None, "txn_date": None,
            "statement_end_at": None, "currency": None, "is_pending": 0,
            "source": amex.SOURCE_ACTIVITY, "payload": {}})
    ends = [date(2025, 1, 31), date(2025, 2, 28), date(2025, 3, 31),
            date(2025, 4, 30), date(2025, 5, 31), date(2025, 6, 30),
            date(2025, 7, 31)]
    parsed = {epoch_day(end): (1700000000, _amex_card(amex.statement_parser,
                                                      end))
              for end in ends}
    amex._import_statements(conn, acct, parsed)

    seam = seams(conn, "transactions", "posted_at",
                 [amex.SOURCE_ACTIVITY]).get(acct)
    top = reach(conn, "transactions", "posted_at",
                [amex.SOURCE_ACTIVITY]).get(acct)
    assert seam == amex.export_seam(conn, acct)
    assert top == amex.activity_reach(conn, acct)
    starts = amex.statement_period_starts(list(parsed))
    oldest = min(parsed)
    seen = set()
    for end in parsed:
        decision = period_decision(starts[end], end, seam, top, oldest)
        seen.add(decision)
        (covered,) = conn.execute(
            "SELECT transactions_covered FROM statement_balances "
            "WHERE account_external_id=? AND period_end=? AND source=?",
            (acct, end, amex.SOURCE_STATEMENT)).fetchone()
        (rows,) = conn.execute(
            "SELECT COUNT(*) FROM transactions WHERE source=? AND "
            "statement_end_at=?", (amex.SOURCE_STATEMENT, end)).fetchone()
        assert (covered, rows) == {IMPORT: (1, 1), COVERED: (1, 0),
                                   STRADDLE: (0, 0),
                                   UNBOUNDED: (0, 0)}[decision], (end, decision)
    if activity is None:
        assert seen == {UNBOUNDED}
    elif activity[0] == date(2025, 3, 15):
        assert seen == {IMPORT, COVERED, STRADDLE}


# ------------------------------------------------------------------
# fidelity-web: the statement-to-feed match
# ------------------------------------------------------------------

DAY = 86400
T0 = 1735689600            # an invented midnight


FEED = [  # (id, account, day offset, amount)
    ("feed-1", "acct-a", 0, -450.00),
    ("feed-2", "acct-a", 1, -450.00),
    ("feed-3", "acct-a", 10, -25.00),
    ("feed-4", "acct-b", 0, -450.00),
    ("feed-5", "acct-a", 2, 450.00),
    ("feed-6", "acct-a", -2, -99.995),
    ("feed-7", "acct-a", 30, -7.00),
    ("feed-0", "acct-a", 31, -7.00),
    ("stmt_old", "acct-a", 0, -450.00),     # a derived row: never a candidate
]

STATEMENT = [  # (id, account, day offset, amount, section)
    ("s1", "acct-a", 0, -450.00, "OTHER"),
    ("s2", "acct-a", 0, -450.00, "OTHER"),
    ("s3", "acct-a", 0, -450.00, "OTHER"),     # a third: nothing left
    ("s1", "acct-a", 0, -450.00, "OTHER"),     # seen again: keeps its claim
    ("s4", "acct-a", 0, -25.00, "FEE"),        # 10 days: only a fee's window
    ("s5", "acct-a", 0, -25.00, "OTHER"),
    ("s6", "acct-a", 0, -100.01, "OTHER"),     # 0.015 off: no match
    ("s7", "acct-a", 0, -99.999, "OTHER"),     # 0.004 off: a match
    ("s8", "acct-a", 30.5, -7.00, "OTHER"),    # equal distance: by id
    ("s9", "acct-c", 0, -450.00, "OTHER"),
]


def test_pair_one_to_one_is_fidelitys_feed_match(tmp_path):
    fidelity = collector_modules.load("fidelity-web", "load", ("pdfplumber",))
    conn = sqlite3.connect(":memory:")
    conn.execute("CREATE TABLE transactions (activity_id TEXT PRIMARY KEY, "
                 "account_external_id TEXT, amount REAL, timestamp INTEGER)")
    conn.executemany("INSERT INTO transactions VALUES (?,?,?,?)",
                     [(i, a, amt, T0 + int(d * DAY)) for i, a, d, amt in FEED])

    claims = fidelity._FeedClaims()
    theirs = {}
    for sid, acct, d, amount, section in STATEMENT:
        absorbed = claims.claimed_for(sid)
        if absorbed is None:
            window = (fidelity._FEE_MATCH_WINDOW if section == "FEE"
                      else fidelity._ACTIVITY_MATCH_WINDOW)
            absorbed = fidelity._unclaimed_feed_match(
                conn, acct, amount, T0 + int(d * DAY), claims, window)
            if absorbed is not None:
                claims.claim(sid, absorbed)
        if absorbed is not None:
            theirs[sid] = absorbed

    left = [Leg(sid, acct, T0 + int(d * DAY), amount, section)
            for sid, acct, d, amount, section in STATEMENT]
    right = [Leg(i, a, T0 + int(d * DAY), amt) for i, a, d, amt in FEED
             if not i.startswith("stmt_")]
    windows = {"FEE": fidelity._FEE_MATCH_WINDOW // DAY}
    ours = pair_one_to_one(
        left, right, lambda a, b: abs(b - a) < 0.005,
        lambda leg: windows.get(leg.kind,
                                fidelity._ACTIVITY_MATCH_WINDOW // DAY))
    assert ours == theirs
    assert ours == {"s1": "feed-1", "s2": "feed-2", "s4": "feed-3",
                    "s7": "feed-6", "s8": "feed-0"}


def test_pair_one_to_one_continues_a_pass():
    right = [Leg("f1", "a", 0, 1.0), Leg("f2", "a", 0, 1.0)]
    claims = pair_one_to_one([Leg("s1", "a", 0, 1.0)], right,
                             lambda a, b: a == b, 3)
    assert claims == {"s1": "f1"}
    pair_one_to_one([Leg("s2", "a", 0, 1.0), Leg("s1", "a", 0, 1.0)], right,
                    lambda a, b: a == b, 3, claims)
    assert claims == {"s1": "f1", "s2": "f2"}
