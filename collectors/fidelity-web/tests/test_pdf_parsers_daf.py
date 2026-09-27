"""
Unit tests for pdf_parsers_daf.py — text-level parsers only.

The PDF I/O entry-point (``parse_daf_statement_pdf``) is a thin
pdfplumber wrapper over these pure string functions, so the fixtures
are synthetic statement text mirroring the observed layout — no real
PDF bytes or real values (root AGENTS.md §4).
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import pdf_parsers_daf as pd  # noqa: E402


# Synthetic statement text mirroring the observed page-1 layout:
# account summary, then the pool-holdings table. Values are round and
# obviously fake; they reconcile by construction (2 pools summing to
# the totals).
STATEMENT_TEXT = """\
Account Summary:
Beginning Value as of April 1, 2025 $ 90,000.00
Contributions Settled After Previous Period End $ 0.00
Grant Distributions $ ( 5,000.00 )
Miscellaneous Transactions $ 0.00
Change in Account Value $ 15,000.00
Ending Value as of June 30, 2025 $ 100,000.00
GivingAccount Pool Holdings:
Units Price per Unit Total Value Total Value
June 30, 2025 June 30, 2025 April 1, 2025 June 30, 2025
Example Growth 1,000.000 $ 60.00 $ 54,000.00 $ 60,000.00
Example Allocation 42% 2,000.000 $ 20.00 $ 36,000.00 $ 40,000.00
Equity
Total Value $ 90,000.00 $ 100,000.00
Transaction Detail:
"""


def test_parse_ending_value():
    as_of, ending = pd.parse_ending_value(STATEMENT_TEXT)
    assert as_of == "2025-06-30"
    assert ending == 100000.00


def test_parse_pool_rows_with_wrapped_name():
    rows, total_begin, total_end = pd.parse_pool_rows(STATEMENT_TEXT)
    assert [r.description for r in rows] == [
        "Example Growth",
        "Example Allocation 42% Equity",  # wrapped tail re-joined
    ]
    assert rows[0].units == 1000.0
    assert rows[0].unit_price == 60.0
    assert rows[0].begin_value == 54000.0
    assert rows[0].end_value == 60000.0
    assert (total_begin, total_end) == (90000.0, 100000.0)


def test_reconcile_passes_on_consistent_statement():
    as_of, ending = pd.parse_ending_value(STATEMENT_TEXT)
    rows, _, total_end = pd.parse_pool_rows(STATEMENT_TEXT)
    ok, reason = pd.reconcile(ending, rows, total_end)
    assert ok and reason is None


def test_reconcile_fails_on_pool_sum_mismatch():
    rows, _, total_end = pd.parse_pool_rows(STATEMENT_TEXT)
    rows[0].end_value += 100.0  # corrupt one pool
    ok, reason = pd.reconcile(100000.0, rows, total_end)
    assert not ok and "pool sum" in reason


def test_reconcile_fails_on_ending_value_mismatch():
    rows, _, total_end = pd.parse_pool_rows(STATEMENT_TEXT)
    ok, reason = pd.reconcile(99000.0, rows, total_end)
    assert not ok and "ending value" in reason


def test_parse_amount_parenthesized_negative():
    assert pd._parse_amount("( 5,000.00 )") == -5000.0
    assert pd._parse_amount("1,234.56") == 1234.56


def test_missing_holdings_block_yields_nothing():
    rows, tb, te = pd.parse_pool_rows("no table here\nEnding Value\n")
    assert rows == [] and tb is None and te is None
    ok, reason = pd.reconcile(100.0, rows, te)
    assert not ok and "Total Value" in reason
