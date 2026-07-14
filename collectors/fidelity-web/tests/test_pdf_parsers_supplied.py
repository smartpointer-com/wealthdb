"""
Unit tests for pdf_parsers_supplied.py — text-level parsers only.

The PDF I/O entry-point (``parse_supplied_statement_pdf``) is
exercised indirectly by integration tests; the functions covered
here are pure functions of strings, so the fixtures are synthetic
statement text — no real PDF bytes, no real account numbers, no
real values. The placeholder account numbers follow the
``NNN-NNNNNN`` Fidelity shape; the placeholder tickers are
deliberate synthetic strings (``ABCFX``, ``DEFGX``) that don't
collide with widely-held real funds.
"""

from __future__ import annotations

import sys
from datetime import date
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import pdf_parsers_supplied as ppt  # noqa: E402


# ============================================================
# Fixtures — synthetic statement text, no real values
# ============================================================

# Monthly statement, two accounts, mixed equity-fund layout (7
# trailing numerics + ticker in parens) and Core Account layout
# (literal "not applicable not applicable" wedged in the numerics).
_MONTHLY_TEXT = """\
INVESTMENT REPORT
November 1, 2025 - November 30, 2025
Envelope # XXX
PLACEHOLDER WEALTH LLC
ID: G99999999

Account # 100-000001
PLACEHOLDER HOLDER - INDIVIDUAL
Separate Account Manager: . --

Holdings
Core Account
Price Total Total Unrealized Est. Annual Est.Yield
Description Quantity Per Unit Market Value Cost Basis Gain/Loss Income (EAI) (EY)
PLACEHOLDER GOVERNMENT CASH RESERVES 1,000.000 $1.0000 $1,000.00 not applicable not applicable $44.00 4.400%
(PLACEX)
Total Core Account (10% of account holdings) $1,000.00 $44.00

Mutual Funds
Price Total Total Unrealized Est. Annual Est.Yield
Description Quantity Per Unit Market Value Cost Basis Gain/Loss Income (EAI) (EY)
Stock Funds
PLACEHOLDER STOCK FUND CLASS A (ABCFX) 100.000 $50.0000 $5,000.00 $4,000.00 $1,000.00 $100.00 2.000%
PLACEHOLDER LONG NAME WRAPS ACROSS LINES 200.000 25.0000 5,000.00 4,500.00 500.00 50.00 1.000
(DEFGX)
Total Stock Funds (90% of account holdings) $10,000.00 $8,500.00 $1,500.00 $150.00

Activity

Account # 200-000002
PLACEHOLDER HOLDER - INDIVIDUAL
Separate Account Manager: . --

Holdings
Bonds
Price Total Market Value Total Unrealized Est. Annual Coupon
Description Maturity Quantity Per Unit Accrued Interest (AI) Cost Basis Gain/Loss Income (EAI) Rate
Municipal Bonds
PLACEHOLDER MUNI BOND 06/30/27 100,000.000 $99.5000 $99,500.00 $98,000.00 $1,500.00 $4,000.00 4.000%
FIXED COUPON MOODYS Aa1 SEMIANNUALLY CUSIP: ABC123456

Activity
"""

# Supplied statements re-stamp the per-account header on every page;
# this fixture demonstrates the gluing: two physical "pages" for
# the same account should produce one logical block, with the
# Holdings table living on the second page.
_RESTAMPED_TEXT = """\
INVESTMENT REPORT
November 1, 2025 - November 30, 2025

Account # 300-000003
PLACEHOLDER HOLDER - INDIVIDUAL
Account Summary
Account Value: $5,000.00

Account # 300-000003
PLACEHOLDER HOLDER - INDIVIDUAL
Holdings
Mutual Funds
Description Quantity Per Unit Market Value Cost Basis Gain/Loss Income (EAI) (EY)
Stock Funds
PLACEHOLDER FUND ALPHA (AAAAX) 1,000.000 $5.0000 $5,000.00 $4,500.00 $500.00 $50.00 1.000%

Activity
"""


# ============================================================
# parse_statement_period
# ============================================================

def test_parse_statement_period_monthly():
    p = ppt.parse_statement_period(_MONTHLY_TEXT)
    assert p == (date(2025, 11, 1), date(2025, 11, 30))


def test_parse_statement_period_returns_none_on_missing():
    assert ppt.parse_statement_period("no period here") is None


# ============================================================
# parse_account_blocks
# ============================================================

def test_parse_account_blocks_finds_two_accounts():
    blocks = ppt.parse_account_blocks(_MONTHLY_TEXT)
    assert [b.account_external_id for b in blocks] == ["100000001", "200000002"]


def test_parse_account_blocks_glues_restamped_headers():
    blocks = ppt.parse_account_blocks(_RESTAMPED_TEXT)
    assert len(blocks) == 1
    assert blocks[0].account_external_id == "300000003"
    assert "Holdings" in blocks[0].text
    assert "(AAAAX)" in blocks[0].text


# ============================================================
# parse_holdings_block — equity / fund layout
# ============================================================

def test_parse_holdings_core_account_row():
    blocks = ppt.parse_account_blocks(_MONTHLY_TEXT)
    rows = ppt.parse_holdings_block(blocks[0].text)
    cores = [r for r in rows if r.instrument_key == "PLACEX"]
    assert len(cores) == 1
    assert cores[0].quantity == 1000.0
    assert cores[0].price == 1.0
    assert cores[0].market_value == 1000.0
    assert cores[0].cost_basis is None  # core has no cost basis
    assert cores[0].unrealized_gain is None


def test_parse_holdings_standard_stock_fund():
    blocks = ppt.parse_account_blocks(_MONTHLY_TEXT)
    rows = ppt.parse_holdings_block(blocks[0].text)
    abc = [r for r in rows if r.instrument_key == "ABCFX"]
    assert len(abc) == 1
    assert abc[0].quantity == 100.0
    assert abc[0].price == 50.0
    assert abc[0].market_value == 5000.0
    assert abc[0].cost_basis == 4000.0
    assert abc[0].unrealized_gain == 1000.0


def test_parse_holdings_glues_continuation_line_for_ticker():
    blocks = ppt.parse_account_blocks(_MONTHLY_TEXT)
    rows = ppt.parse_holdings_block(blocks[0].text)
    defgx = [r for r in rows if r.instrument_key == "DEFGX"]
    assert len(defgx) == 1
    assert "WRAPS ACROSS LINES" in defgx[0].description
    assert defgx[0].market_value == 5000.0


# ============================================================
# parse_holdings_block — bond layout (CUSIP fallback)
# ============================================================

def test_parse_holdings_bond_uses_cusip_fallback():
    blocks = ppt.parse_account_blocks(_MONTHLY_TEXT)
    rows = ppt.parse_holdings_block(blocks[1].text)
    bonds = [r for r in rows if r.instrument_key == "ABC123456"]
    assert len(bonds) == 1
    assert bonds[0].quantity == 100000.0
    assert bonds[0].price == 99.5
    assert bonds[0].market_value == 99500.0


# ============================================================
# parse_supplied_statement_pdf — signature gate
# ============================================================

def test_parse_returns_signature_mismatch_when_substring_absent(monkeypatch):
    # Stub the PDF text extractor so we don't need a real PDF on
    # disk; the function under test should bail out before
    # reaching any text parsing when the signature is absent.
    monkeypatch.setattr(
        ppt, "_extract_pdf_text",
        lambda path: "INVESTMENT REPORT — UNRELATED ACCOUNT",
    )
    out = ppt.parse_supplied_statement_pdf(
        "/fake/path.pdf", expected_signature="PLACEHOLDER HOLDER",
    )
    assert out["_error"] == "signature-mismatch"
    assert out["expected_signature"] == "PLACEHOLDER HOLDER"


def test_parse_accepts_when_signature_present(monkeypatch):
    monkeypatch.setattr(ppt, "_extract_pdf_text", lambda path: _MONTHLY_TEXT)
    out = ppt.parse_supplied_statement_pdf(
        "/fake/path.pdf", expected_signature="PLACEHOLDER HOLDER",
    )
    assert "_error" not in out
    assert out["period_end"] == "2025-11-30"
    assert len(out["accounts"]) == 2
