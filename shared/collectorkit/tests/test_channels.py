"""Tests for collectorkit.channels over invented rows: the seams of the
primary channel, the decision per statement period, and the one-to-one
pairing."""
from __future__ import annotations

import sqlite3

import pytest

from collectorkit.channels import (COVERED, IMPORT, STRADDLE, UNBOUNDED, Leg,
                                   pair_one_to_one, period_decision, reach,
                                   seams)


# ------------------------------------------------------------------
# seams and reach
# ------------------------------------------------------------------

@pytest.fixture
def ledger():
    conn = sqlite3.connect(":memory:")
    conn.execute("CREATE TABLE transactions (account_external_id TEXT, "
                 "source TEXT, posted_at INTEGER, acct TEXT, origin TEXT)")
    conn.executemany(
        "INSERT INTO transactions VALUES (?, ?, ?, ?, ?)",
        [("a", "export", 50, "a", "export"), ("a", "export", 90, "a", "export"),
         ("a", "api", 40, "a", "api"), ("a", "statement", 10, "a", "statement"),
         ("b", "statement", 5, "b", "statement"),
         ("c", "api", 70, "c", "api")])
    return conn


def test_seams_and_reach_read_the_primary_channel_only(ledger):
    primary = ("export", "api")
    assert seams(ledger, "transactions", "posted_at", primary) == \
        {"a": 40, "c": 70}
    assert reach(ledger, "transactions", "posted_at", primary) == \
        {"a": 90, "c": 70}
    assert seams(ledger, "transactions", "posted_at", ("export",)) == \
        {"a": 50}
    assert seams(ledger, "transactions", "posted_at", ("none",)) == {}
    assert seams(ledger, "transactions", "posted_at", primary,
                 account_col="acct", source_col="origin") == \
        {"a": 40, "c": 70}


def test_seams_refuse_what_is_no_identifier(ledger):
    with pytest.raises(ValueError):
        seams(ledger, "transactions; DROP TABLE x", "posted_at", ("api",))


# ------------------------------------------------------------------
# period_decision
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
# pair_one_to_one
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


def test_pair_one_to_one_by_window_tolerance_distance_and_id():
    left = [Leg(sid, acct, T0 + int(d * DAY), amount, section)
            for sid, acct, d, amount, section in STATEMENT]
    right = [Leg(i, a, T0 + int(d * DAY), amt) for i, a, d, amt in FEED
             if not i.startswith("stmt_")]
    windows = {"FEE": 14}
    pairs = pair_one_to_one(
        left, right, lambda a, b: abs(b - a) < 0.005,
        lambda leg: windows.get(leg.kind, 3))
    assert pairs == {"s1": "feed-1", "s2": "feed-2", "s4": "feed-3",
                     "s7": "feed-6", "s8": "feed-0"}


def test_pair_one_to_one_continues_a_pass():
    right = [Leg("f1", "a", 0, 1.0), Leg("f2", "a", 0, 1.0)]
    claims = pair_one_to_one([Leg("s1", "a", 0, 1.0)], right,
                             lambda a, b: a == b, 3)
    assert claims == {"s1": "f1"}
    pair_one_to_one([Leg("s2", "a", 0, 1.0), Leg("s1", "a", 0, 1.0)], right,
                    lambda a, b: a == b, 3, claims)
    assert claims == {"s1": "f1", "s2": "f2"}
