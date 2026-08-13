"""Unit tests for statement_parser — the pure text parse. Synthetic statement
text only (mirrors the `pdftotext -layout` shape); no real statement data.
"""
from __future__ import annotations

import sys
from datetime import date
from decimal import Decimal
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import statement_parser as sp  # noqa: E402

# A synthetic one-account statement: begin 1000.00, two deposits (+500, +200),
# a withdrawal (-300) and a check (-250) → end 1150.00. Includes a continuation
# line and a non-transaction "post fees message" block that must be ignored.
STATEMENT = """\
                       March 19, 2026 through April 17, 2026

CHECKING SUMMARY
  Beginning Balance                         $1,000.00
  Deposits and Additions                       700.00
  Electronic Withdrawals                      -300.00
  Ending Balance                            $1,150.00

*start*deposits and additions*
  DATE     DESCRIPTION                          AMOUNT
  03/20    Payroll Direct Deposit               500.00
  04/01    Zelle payment from EXAMPLE PARTY      200.00
                Ref 28701518282
  Total Deposits and Additions                 700.00
*end*deposits and additions*

*start*electronic withdrawal*
  03/25    Online Transfer to savings           300.00
*end*electronic withdrawal*

*start*checks paid section*
  04/05    Check                                250.00
*end*checks paid section*

*start*post fees message*
  There were no fees on this account. 04/09 is not a transaction 12.34
*end*post fees message*
"""


def test_period_and_balances():
    s = sp.parse_statement_text(STATEMENT)
    assert s.period_start == date(2026, 3, 19)
    assert s.period_end == date(2026, 4, 17)
    assert len(s.segments) == 1
    assert s.segments[0].beginning_balance == Decimal("1000.00")
    assert s.segments[0].ending_balance == Decimal("1150.00")


def test_transactions_signed_and_dated():
    s = sp.parse_statement_text(STATEMENT)
    txns = s.segments[0].transactions
    got = [(t.posted_at.isoformat(), str(t.amount)) for t in txns]
    assert got == [
        ("2026-03-20", "500.00"),
        ("2026-04-01", "200.00"),
        ("2026-03-25", "-300.00"),
        ("2026-04-05", "-250.00"),
    ]
    # continuation line folded into the description; message block ignored.
    zelle = next(t for t in txns if t.posted_at == date(2026, 4, 1))
    assert "Ref 28701518282" in zelle.description
    assert all("not a transaction" not in t.description for t in txns)


def test_reconciles_and_running_balance():
    s = sp.parse_statement_text(STATEMENT)
    assert sp.balance_reconciles(s) is True
    rb = sp.running_balances(s.segments[0])
    # posted-date order: +500 -300 +200 -250 over 1000 → 1500,1200,1400,1150
    assert [str(b) for _, b in rb] == ["1500.00", "1200.00", "1400.00", "1150.00"]
    assert rb[-1][1] == s.segments[0].ending_balance


def test_year_wrap_across_january():
    text = """\
December 18, 2025 through January 21, 2026
  Beginning Balance   $10.00
  Ending Balance      $30.00
*start*deposits and additions*
  12/20   Deposit in December    5.00
  01/15   Deposit in January    15.00
*end*deposits and additions*
"""
    s = sp.parse_statement_text(text)
    assert [t.posted_at.isoformat() for t in s.segments[0].transactions] == \
        ["2025-12-20", "2026-01-15"]


def test_section_sign_classifies_by_keyword():
    assert sp._section_sign("deposits and additions") == 1
    assert sp._section_sign("electronic withdrawal") == -1
    assert sp._section_sign("checks paid section") == -1
    assert sp._section_sign("atm & debit card withdrawals") == -1
    assert sp._section_sign("fees and other withdrawals") == -1
    # prose blocks that merely mention a keyword are not transaction sections
    assert sp._section_sign("post fees message") is None
    assert sp._section_sign("consolidated balance summary") is None


def test_selects_reconciling_balance_pair():
    # A combined statement carries a CONSOLIDATED pair (checking + cards) before
    # the CHECKING pair; only the checking pair reconciles with the net.
    text = """\
January 22, 2026 through February 19, 2026
CONSOLIDATED BALANCE SUMMARY
  Beginning Balance   $5,000.00
  Ending Balance      $4,800.00
CHECKING SUMMARY
  Beginning Balance   $1,000.00
  Ending Balance      $1,200.00
*start*deposits and additions*
  01/25   Deposit    500.00
*end*deposits and additions*
*start*electronic withdrawal*
  02/10   Transfer   300.00
*end*electronic withdrawal*
"""
    s = sp.parse_statement_text(text)
    seg = s.segments[0]
    assert seg.beginning_balance == Decimal("1000.00")   # checking, not consolidated
    assert seg.ending_balance == Decimal("1200.00")
    assert sp.balance_reconciles(s) is True


def test_reconcile_flags_a_missed_row():
    # Drop a row's amount from the totals → beginning+Σ != ending.
    bad = STATEMENT.replace("  04/05    Check                                250.00\n", "")
    s = sp.parse_statement_text(bad)
    assert sp.balance_reconciles(s) is False


# A synthetic combined statement: two products, each with its own
# `*start*global product*` header, summary pair, and same-named transaction
# sections. Segment A: 100 → 130 (+50, -20); segment B: 1000 → 1400 (+500,
# -100). Pooled parsing can never reconcile this; per-segment parsing must.
COMBINED = """\
                       May 18, 2023 through June 20, 2023

*start*consolidated balance summary2*
  ACCOUNT                    BEGINNING BALANCE   ENDING BALANCE
  Example Product One             $100.00            $130.00
  Example Product Two           $1,000.00          $1,400.00
*end*consolidated balance summary2*

*start*global product*
Example Product One
*end*global product*
*start*summary*
  Beginning Balance   $100.00
  Ending Balance      $130.00
*end*summary*
*start*deposits and additions*
  05/20   Deposit A                50.00
*end*deposits and additions*
*start*electronic withdrawal*
  06/01   Withdrawal A            20.00
*end*electronic withdrawal*

*start*global product*
Example Product Two
*end*global product*
*start*summary*
  Beginning Balance   $1,000.00
  Ending Balance      $1,400.00
*end*summary*
*start*deposits and additions*
  05/25   Deposit B               500.00
*end*deposits and additions*
*start*electronic withdrawal*
  06/10   Withdrawal B           100.00
*end*electronic withdrawal*
"""


def test_combined_statement_splits_into_segments():
    s = sp.parse_statement_text(COMBINED)
    assert len(s.segments) == 2
    a, b = s.segments
    assert (a.beginning_balance, a.ending_balance) == \
        (Decimal("100.00"), Decimal("130.00"))
    assert (b.beginning_balance, b.ending_balance) == \
        (Decimal("1000.00"), Decimal("1400.00"))
    assert [str(t.amount) for t in a.transactions] == ["50.00", "-20.00"]
    assert [str(t.amount) for t in b.transactions] == ["500.00", "-100.00"]


def test_combined_segments_reconcile_independently():
    s = sp.parse_statement_text(COMBINED)
    assert all(sp.segment_reconciles(seg) for seg in s.segments)
    assert sp.balance_reconciles(s) is True
    # break one segment → only that segment fails, and the statement gate too
    bad = sp.parse_statement_text(
        COMBINED.replace("  06/10   Withdrawal B           100.00\n", ""))
    assert sp.segment_reconciles(bad.segments[0]) is True
    assert sp.segment_reconciles(bad.segments[1]) is False
    assert sp.balance_reconciles(bad) is False


def test_segment_reconciles_on_hand_built_segment():
    seg = sp.StatementSegment(
        beginning_balance=Decimal("10.00"), ending_balance=Decimal("15.00"),
        transactions=[sp.StatementTxn(date(2023, 1, 2), Decimal("5.00"), "x")])
    assert sp.segment_reconciles(seg) is True
    seg.ending_balance = Decimal("16.00")
    assert sp.segment_reconciles(seg) is False
