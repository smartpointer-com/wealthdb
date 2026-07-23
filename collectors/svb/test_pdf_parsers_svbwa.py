"""
Unit tests for pdf_parsers_svbwa.py — text-level parsers only.

The PDF I/O entry-point (``parse_svbwa_statement_pdf``) is exercised
here through the same seam the supplied-statement parser tests use: the
``_extract_pdf_text`` extractor is monkeypatched to return synthetic
statement text, so no real PDF bytes are needed.

Every fixture below is **synthetic** (repo policy §4): placeholder
account numbers in the ``SV[MRT]-NNNNNN`` shape but all-zero serials,
a fabricated registration signature, example tickers that don't
collide with single-stock real-world symbols (``AAAA``/``BBBB`` and
the widely-held ``VOO``), synthetic 9-char CUSIPs, synthetic OCC
option codes, and round-number synthetic values. No real holdings,
balances, names, or account ids appear.
"""

from __future__ import annotations

import sys
from datetime import date
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import pdf_parsers_svbwa as ps  # noqa: E402


# Synthetic page-1 registration line used as the signature guard.
_SIG = "EXAMPLE HOLDER - Example Property"


# ============================================================
# Fixtures — synthetic statement text, no real values
# ============================================================

# A money-market + bond statement: cash (NET CASH POSITION),
# a money-market sweep row ($1.00 price), and a bond whose 9-char
# CUSIP sits inline at the end of the description followed by glued
# coupon / rating / accrued-interest continuation sub-lines.
_MM_BOND_TEXT = """\
ENV# EXAMPLEENVCODE
SVB WEALTH ADVISORY, INC.
EXAMPLE HOLDER
STATEMENT FOR THE PERIOD JANUARY 1, 2021 TO MARCH 31, 2021
EXAMPLE HOLDER - Example Property
Account Number: SVR-000000
Separate Acc't Manager: EXAMPLE ADVISORS
Account carried with National Financial Services LLC
Holdings
For additional information regarding your holdings, please refer to the footnotes.
CASH AND CASH EQUIVALENTS - 1.00% of Total Account Value
Symbol/Cusip Price on Current Estimated
Description Account Type Quantity 03/31/21 Market Value Annual Income
Money Markets
EXAMPLE GOVERNMENT MONEY MARKET BBBB 5,000.000 $1.00 $5,000.00
7 DAY YIELD 0.01% CASH
NET CASH POSITION $1,000.00
Total Cash and Cash Equivalents $6,000.00
HOLDINGS > FIXED INCOME - 99.00% of Total Account Value
Symbol/Cusip Price on Current Estimated
Description Account Type Quantity 03/31/21 Market Value Annual Income
Corporate Bonds
EXAMPLE CORP NOTE CALL MAKE WHOLE 11111AAA1 100,000 $95.000 $95,000.00 $3,000.00
3.00000% 06/30/2027 CASH
MOODY'S Aaa /S&P AAA
CPN PMT SEMI-ANNUAL
ON JUN 30, DEC 30
Next Interest Payable: 06/30/21
Accrued Interest $250.00
Total Fixed Income $95,000.00
Total Securities $101,000.00
TOTAL PORTFOLIO VALUE $101,000.00
Activity
"""

# An equity / ETP statement: one ETP row with a mid-line symbol and
# a wrapped description, with glued Estimated Yield / option-election
# continuation sub-lines.
_EQUITY_TEXT = """\
SVB WEALTH ADVISORY, INC.
STATEMENT FOR THE PERIOD DECEMBER 1, 2022 TO DECEMBER 31, 2022
EXAMPLE HOLDER - Example Property
Account Number: SVM-000000
Account carried with National Financial Services LLC
Holdings
HOLDINGS > EXCHANGE TRADED PRODUCTS - 100.00% of Total Account Value
Symbol/Cusip Price on Current Estimated
Description Account Type Quantity 12/31/22 Market Value Annual Income
Equity
EXAMPLE BROAD MARKET INDEX FUND VOO 1,000 $300.00 $300,000.00 $5,000.00
Estimated Yield 1.67% CASH
Dividend Option Cash
Capital Gain Option Cash
Total Exchange Traded Products $300,000.00
Total Securities $300,000.00
TOTAL PORTFOLIO VALUE $300,000.00
Activity
"""

# An options statement (long and short legs). Each option spans
# three physical lines. Short legs print parenthesised — those must
# come out NEGATIVE for BOTH quantity and market value. One long leg
# (the put) prints plain (positive). The equity leg is a plain long.
_OPTIONS_TEXT = """\
SVB WEALTH ADVISORY, INC.
STATEMENT FOR THE PERIOD DECEMBER 1, 2021 TO DECEMBER 31, 2021
EXAMPLE HOLDER - Example Property
Account Number: SVM-000000
Account carried with National Financial Services LLC
Holdings
CASH AND CASH EQUIVALENTS - 0.00% of Total Account Value
Symbol/Cusip Price on Current Estimated
Description Account Type Quantity 12/31/21 Market Value Annual Income
Cash
NET CASH POSITION ($2,000.00)
Total Cash and Cash Equivalents ($2,000.00)
HOLDINGS > EQUITIES - 100.00% of Total Account Value
Symbol/Cusip Price on Current Estimated
Description Account Type Quantity 12/31/21 Market Value Annual Income
Equity
EXAMPLE COMPANY CL A AAAA 300 $40.00 $12,000.00
Dividend Option Reinvest MARGIN
Capital Gain Option Reinvest
Total Equities $12,000.00
HOLDINGS > OPTIONS - 0.00% of Total Account Value
Price on Current Estimated
Description Symbol/Cusip Account Type Quantity 12/31/21 Market Value Annual Income
Equity
CALL (AAAA) EXAMPLE COMPANY CL A JAN 18 30 (4) $20.00 ($8,000.00)
$100 (100 SHS) MARGIN
AAAA300118C100
PUT (AAAA) EXAMPLE COMPANY CL A JAN 18 30 5 $3.00 $1,500.00
$90 (100 SHS) MARGIN
AAAA300118P90
Total Options ($6,500.00)
Total Securities $3,500.00
TOTAL PORTFOLIO VALUE $3,500.00
Activity
"""

# A closing / $0 statement: no holdings table, just the
# no-positions sentence.
_NO_POSITIONS_TEXT = """\
SVB WEALTH ADVISORY, INC.
STATEMENT FOR THE PERIOD JUNE 1, 2022 TO JUNE 30, 2022
EXAMPLE HOLDER - Example Property
Account Number: SVM-000000
Account carried with National Financial Services LLC
Holdings
There were no positions in your account at the close of the statement period.
Activity
"""


# ============================================================
# parse_statement_period — uppercase, "TO", quarterly span
# ============================================================

def test_parse_statement_period_quarterly_uppercase_to():
    p = ps.parse_statement_period(_MM_BOND_TEXT)
    assert p == (date(2021, 1, 1), date(2021, 3, 31))


def test_parse_statement_period_monthly():
    p = ps.parse_statement_period(_EQUITY_TEXT)
    assert p == (date(2022, 12, 1), date(2022, 12, 31))


def test_parse_statement_period_returns_none_on_missing():
    assert ps.parse_statement_period("no period header here") is None


# ============================================================
# parse_account_blocks — literal SV[MRT]-NNNNNN, restamp glue
# ============================================================

def test_parse_account_blocks_keeps_literal_id():
    blocks = ps.parse_account_blocks(_MM_BOND_TEXT)
    assert len(blocks) == 1
    assert blocks[0].account_external_id == "SVR-000000"


def test_parse_account_blocks_glues_restamped_single_account():
    # The header is re-stamped on every page; consecutive same-id
    # headers must collapse to one logical block.
    restamped = (
        "Account Number: SVM-000000\nAccount Summary\n"
        "Account Number: SVM-000000\nHoldings\n"
        "HOLDINGS > EQUITIES - 100.00% of Total Account Value\n"
        "Symbol/Cusip Price on Current Estimated\n"
        "Description Account Type Quantity 12/31/22 Market Value Annual Income\n"
        "EXAMPLE FUND VOO 10 $100.00 $1,000.00\n"
        "Activity\n"
    )
    blocks = ps.parse_account_blocks(restamped)
    assert len(blocks) == 1
    assert blocks[0].account_external_id == "SVM-000000"


# ============================================================
# parse_holdings_block — money market + NET CASH POSITION
# ============================================================

def test_money_market_sweep_row():
    blocks = ps.parse_account_blocks(_MM_BOND_TEXT)
    rows = ps.parse_holdings_block(blocks[0].text)
    mm = [r for r in rows if r.instrument_key == "BBBB"]
    assert len(mm) == 1
    assert mm[0].quantity == 5000.0
    assert mm[0].price == 1.0
    assert mm[0].market_value == 5000.0
    assert mm[0].cost_basis is None
    assert mm[0].unrealized_gain is None


def test_net_cash_position_row():
    blocks = ps.parse_account_blocks(_MM_BOND_TEXT)
    rows = ps.parse_holdings_block(blocks[0].text)
    cash = [r for r in rows if r.description == "NET CASH POSITION"]
    assert len(cash) == 1
    assert cash[0].instrument_key is None
    assert cash[0].quantity is None
    assert cash[0].price is None
    assert cash[0].market_value == 1000.0


def test_net_cash_position_letter_spaced():
    # pdfplumber sometimes letter-spaces the label as a kerning
    # artefact ("N E T C A S H P O S I T I O N").
    text = (
        "Account Number: SVM-000000\nHoldings\n"
        "CASH AND CASH EQUIVALENTS - 0.00% of Total Account Value\n"
        "Cash\nN E T C A S H P O S I T I O N ($3,000.00)\n"
        "Total Cash and Cash Equivalents ($3,000.00)\nActivity\n"
    )
    blocks = ps.parse_account_blocks(text)
    rows = ps.parse_holdings_block(blocks[0].text)
    cash = [r for r in rows if r.description == "NET CASH POSITION"]
    assert len(cash) == 1
    assert cash[0].market_value == -3000.0


# ============================================================
# parse_holdings_block — equity row (mid-line symbol)
# ============================================================

def test_equity_row_midline_symbol():
    blocks = ps.parse_account_blocks(_EQUITY_TEXT)
    rows = ps.parse_holdings_block(blocks[0].text)
    etp = [r for r in rows if r.instrument_key == "VOO"]
    assert len(etp) == 1
    assert etp[0].description == "EXAMPLE BROAD MARKET INDEX FUND"
    assert etp[0].quantity == 1000.0
    assert etp[0].price == 300.0
    assert etp[0].market_value == 300000.0
    # SVB-WA statements never print cost / unrealized columns.
    assert etp[0].cost_basis is None
    assert etp[0].unrealized_gain is None


def test_equity_glued_continuations_not_emitted():
    # "Estimated Yield", "Dividend Option", "Capital Gain Option"
    # sub-lines must not be emitted as their own rows.
    blocks = ps.parse_account_blocks(_EQUITY_TEXT)
    rows = ps.parse_holdings_block(blocks[0].text)
    assert len(rows) == 1


# ============================================================
# parse_holdings_block — bond with inline CUSIP + glue
# ============================================================

def test_bond_inline_cusip_and_glued_continuations():
    blocks = ps.parse_account_blocks(_MM_BOND_TEXT)
    rows = ps.parse_holdings_block(blocks[0].text)
    bond = [r for r in rows if r.instrument_key == "11111AAA1"]
    assert len(bond) == 1
    assert bond[0].quantity == 100000.0
    assert bond[0].price == 95.0
    assert bond[0].market_value == 95000.0
    # The CUSIP is stripped from the description tail.
    assert "11111AAA1" not in bond[0].description
    assert bond[0].description.startswith("EXAMPLE CORP NOTE")
    # Coupon / rating / accrued-interest sub-lines aren't rows.
    descs = [r.description for r in rows]
    assert not any(d.startswith("Accrued Interest") for d in descs)
    assert not any(d.startswith("MOODY") for d in descs)


def test_bond_glued_cusip_recovery():
    # pdfplumber sometimes glues the last description word onto the
    # CUSIP and mis-splits the run on a kerning boundary
    # ("WHOLE11 111AAA1"); the parser must still recover the CUSIP.
    text = (
        "Account Number: SVR-000000\nHoldings\n"
        "HOLDINGS > FIXED INCOME - 100.00% of Total Account Value\n"
        "Symbol/Cusip Price on Current Estimated\n"
        "Description Account Type Quantity 03/31/21 Market Value Annual Income\n"
        "Corporate Bonds\n"
        "EXAMPLE CORP NOTE CALL MAKE WHOLE11 111AAA1 100,000 $95.000 $95,000.00 $3,000.00\n"
        "3.00000% 06/30/2027 CASH\n"
        "Total Fixed Income $95,000.00\nActivity\n"
    )
    blocks = ps.parse_account_blocks(text)
    rows = ps.parse_holdings_block(blocks[0].text)
    bond = [r for r in rows if r.instrument_key == "11111AAA1"]
    assert len(bond) == 1
    assert bond[0].market_value == 95000.0
    # The spilled-over "WHOLE" word is restored to the description.
    assert bond[0].description.endswith("WHOLE")


# ============================================================
# parse_holdings_block — option 3-line shape, parens -> NEGATIVE
# ============================================================

def test_option_short_leg_is_negative():
    blocks = ps.parse_account_blocks(_OPTIONS_TEXT)
    rows = ps.parse_holdings_block(blocks[0].text)
    short_call = [r for r in rows if r.instrument_key == "AAAA300118C100"]
    assert len(short_call) == 1
    # Parenthesised qty AND market value => NEGATIVE for both.
    assert short_call[0].quantity == -4.0
    assert short_call[0].market_value == -8000.0
    assert short_call[0].price == 20.0


def test_option_long_leg_is_positive():
    blocks = ps.parse_account_blocks(_OPTIONS_TEXT)
    rows = ps.parse_holdings_block(blocks[0].text)
    long_put = [r for r in rows if r.instrument_key == "AAAA300118P90"]
    assert len(long_put) == 1
    assert long_put[0].quantity == 5.0
    assert long_put[0].market_value == 1500.0


def test_options_statement_reconciles_signed():
    # The make-or-break: short options must net negative so the
    # account total reconciles. Sum of signed market values:
    #   cash -2,000 + equity +12,000
    #   + short call -8,000 + long put +1,500 = 3,500.
    blocks = ps.parse_account_blocks(_OPTIONS_TEXT)
    rows = ps.parse_holdings_block(blocks[0].text)
    total = sum((r.market_value or 0.0) for r in rows)
    assert total == 3500.0


# ============================================================
# parse_holdings_block — no positions / $0 statement
# ============================================================

def test_no_positions_yields_empty_holdings():
    blocks = ps.parse_account_blocks(_NO_POSITIONS_TEXT)
    assert len(blocks) == 1
    rows = ps.parse_holdings_block(blocks[0].text)
    assert rows == []


# ============================================================
# parse_svbwa_statement_pdf — orchestration + signature gate
# ============================================================

def test_pdf_signature_mismatch(monkeypatch):
    monkeypatch.setattr(
        ps, "_extract_pdf_text",
        lambda path: "SVB WEALTH ADVISORY, INC. — UNRELATED ACCOUNT",
    )
    out = ps.parse_svbwa_statement_pdf(
        "/fake/path.pdf", expected_signature=_SIG,
    )
    assert out["_error"] == "signature-mismatch"
    assert out["expected_signature"] == _SIG


def test_pdf_accepts_when_signature_present(monkeypatch):
    monkeypatch.setattr(ps, "_extract_pdf_text", lambda path: _MM_BOND_TEXT)
    out = ps.parse_svbwa_statement_pdf(
        "/fake/path.pdf", expected_signature=_SIG,
    )
    assert "_error" not in out
    assert out["period_start"] == "2021-01-01"
    assert out["period_end"] == "2021-03-31"
    assert len(out["accounts"]) == 1
    acct = out["accounts"][0]
    assert acct["account_external_id"] == "SVR-000000"
    # Holdings dicts carry exactly the loader-consumed keys.
    h = acct["holdings"][0]
    assert set(h) == {
        "description", "instrument_key", "quantity", "price",
        "market_value", "cost_basis", "unrealized_gain",
    }


def test_pdf_no_positions_returns_account_with_period(monkeypatch):
    # A $0 closing statement returns the account (empty holdings)
    # plus the correct period_end so a terminal snapshot is recorded.
    monkeypatch.setattr(
        ps, "_extract_pdf_text", lambda path: _NO_POSITIONS_TEXT,
    )
    out = ps.parse_svbwa_statement_pdf("/fake/path.pdf")
    assert "_error" not in out
    assert out["period_end"] == "2022-06-30"
    assert len(out["accounts"]) == 1
    assert out["accounts"][0]["account_external_id"] == "SVM-000000"
    assert out["accounts"][0]["holdings"] == []
