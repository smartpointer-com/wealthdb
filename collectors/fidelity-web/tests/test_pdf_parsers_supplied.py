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


# ============================================================
# Account-level activity
# ============================================================

# The three sections the feed does not carry, plus two it does — the
# per-security ADR fee that shares the Fees heading, and a corporate
# action under Other Activity Out — so the test can show what is read
# and what is left alone. Every value is invented.
_ACTIVITY_TEXT = """\
INVESTMENT REPORT
January 1, 2026 - January 31, 2026

Account # 100-000001
PLACEHOLDER HOLDER - INDIVIDUAL
Activity
Withdrawals
Date Reference Description Amount
01/06 Wire Tfr To Bank WD00000001 -$2,000.00
PLACEHOLDER PAYEE ONE
PLACEHOLDER BANK, N.A. ******0001
01/06 Wire Tfr To Bank WD00000002 -2,000.00
PLACEHOLDER PAYEE TWO
PLACEHOLDER BANK, N.A. ******0002
01/07 Check Issued CHECK PAID 000000001 -600.00
PLACEHOLDER PAYEE THREE
Total Withdrawals -$4,600.00
Deposits
Date Reference Description Amount
01/20 Wire Trans From Bank $1,500.00
Total Deposits $1,500.00
Fees and Charges
Date Description Amount
01/11 Account Fee -$4,000.00
01/13 Quarterly Fee -500.00
01/14 Placeholder Adr Each Rep 1 Ord -10.00
Total Fees and Charge -$4,510.00
Other Activity Out
Settlement Symbol/
Date Security Name CUSIP Description Quantity Price
01/27 PLACEHOLDER ADR 000000101 Reverse Split 40.000 -
Total Other Activity Out -
"""


# A Withdrawals section broken across a page. `parse_account_blocks` glues
# the two pages into one block, so the second page's masthead, its re-stamped
# account header, the registration line and the repeated column header all
# sit between the last row on page one and the first row on page two.
_PAGE_BREAK_TEXT = """
INVESTMENT REPORT
January 1, 2026 - January 31, 2026

Account # 100-000001
PLACEHOLDER HOLDER - INDIVIDUAL
Activity
Withdrawals
Date Reference Description Amount
01/06 Wire Tfr To Bank WD00000001 -$2,000.00
PLACEHOLDER PAYEE ONE
PLACEHOLDER BANK, N.A. ******0001

INVESTMENT REPORT
January 1, 2026 - January 31, 2026
Envelope # XXX
Account # 100-000001
PLACEHOLDER HOLDER - INDIVIDUAL
Date Reference Description Amount
01/22 Wire Tfr To Bank WD00000002 -$3,000.00
PLACEHOLDER PAYEE TWO
PLACEHOLDER BANK, N.A. ******0002
Total Withdrawals -$5,000.00
"""


def _activity(text=_ACTIVITY_TEXT, signature=None):
    period = ppt.parse_statement_period(text)
    block = ppt.parse_account_blocks(text)[0]
    return ppt.parse_activity_block(
        block.text, period=period, expected_signature=signature)


def test_a_page_break_does_not_fold_the_masthead_into_the_row():
    # The last money row on a page is followed by the next page's frame:
    # masthead, re-stamped account header, registration line, repeated
    # column header. None of it is part of the payment, and the description
    # is what gold reads as the narrative.
    rows = _activity(_PAGE_BREAK_TEXT, signature="PLACEHOLDER HOLDER")
    # Two rows, not one: the guard ends the row at the furniture without
    # ending the SECTION, which continues on the next page.
    assert len(rows) == 2
    assert rows[0].description == (
        "Wire Tfr To Bank WD00000001 PLACEHOLDER PAYEE ONE "
        "PLACEHOLDER BANK, N.A. ******0001")
    # The far side of the break still folds its own continuations.
    assert rows[1].description.endswith("******0002")


def test_activity_reads_the_three_account_level_sections():
    rows = _activity()
    assert [(r.date.isoformat(), r.section, r.amount) for r in rows] == [
        ("2026-01-06", "WITHDRAWAL", -2000.00),
        ("2026-01-06", "WITHDRAWAL", -2000.00),
        ("2026-01-07", "WITHDRAWAL", -600.00),
        ("2026-01-20", "DEPOSIT", 1500.00),
        ("2026-01-11", "FEE", -4000.00),
        ("2026-01-13", "FEE", -500.00),
        ("2026-01-14", "FEE", -10.00),
    ]


def test_a_wire_keeps_the_beneficiary_lines():
    # Two wires of the same amount on the same day: the reference and
    # the beneficiary are the only things that tell them apart, and
    # both live on the continuation lines.
    a, b = _activity()[:2]
    assert a.description == (
        "Wire Tfr To Bank WD00000001 PLACEHOLDER PAYEE ONE "
        "PLACEHOLDER BANK, N.A. ******0001")
    assert b.description.endswith("******0002")
    assert a.description != b.description


def test_a_fee_row_never_folds_the_line_below_it():
    # Fee rows are one line by construction. A section whose total
    # fell on the far side of a page break is followed by page
    # furniture, which must not land in the last fee's description.
    text = _ACTIVITY_TEXT.replace("Total Fees and Charge -$4,510.00\n", "")
    fees = [r for r in _activity(text) if r.section == "FEE"]
    assert fees[-1].description == "Placeholder Adr Each Rep 1 Ord"


def test_security_level_sections_are_left_to_the_scraped_feed():
    # Other Activity Out carries corporate actions, which arrive
    # through the activity feed; reading them here would double them.
    assert all(r.section in ("WITHDRAWAL", "DEPOSIT", "FEE") for r in _activity())
    assert not any("Reverse Split" in r.description for r in _activity())


def test_a_month_row_is_dated_inside_the_statement_period():
    # The row carries MM/DD only; the year comes from the period, and
    # a year-end statement spans twelve months of them.
    text = _ACTIVITY_TEXT.replace(
        "January 1, 2026 - January 31, 2026", "January 1, 2026 - December 31, 2026")
    assert _activity(text)[0].date == date(2026, 1, 6)


def test_an_arrears_row_resolves_backwards_not_into_the_future():
    # A fee charged for an earlier dividend can print on a January
    # statement with its own date, outside the period. It
    # belongs to the November before, not the November after.
    text = _ACTIVITY_TEXT.replace(
        "01/14 Placeholder Adr Each Rep 1 Ord -10.00",
        "11/12 Placeholder Adr Each Rep 1 Ord -10.00")
    row = next(r for r in _activity(text) if r.amount == -10.00)
    assert row.date == date(2025, 11, 12)


# ============================================================
# Securities Bought & Sold — the sales
# ============================================================

# One account's Bought & Sold listing across a page break: a purchase,
# a Specific Share sale with its gain on a wrapped note line, a sale
# whose lots span both terms, a sale with an unknown basis, a sale and
# its cancellation, two four-cell rows (one blank charge, one blank
# basis), and a bond redemption. Every value is invented.
_SALES_TEXT = """\
INVESTMENT REPORT
March 1, 2026 - March 31, 2026
Account # 100-000001
PLACEHOLDER HOLDER - INDIVIDUAL
Activity
Securities Bought & Sold
Settlement Symbol/ Total Transaction
Date Security Name CUSIP Description Quantity Price Cost Basis Cost Amount
03/03 EXAMPLE CORP COM 000000AA0 You Bought 10.000 $50.00000 - -$500.00
s03/04 SAMPLE INDS INC 000000BB0 You Sold -4.000 25.00000 80.00 -0.02 99.98
AVERAGE PRICE TRADE DETAILS ON Short-term gain: $19.98
REQUEST refer to confirm for Lot detail
s03/05 PLACEHOLDER HLDGS COM 000000CC0 You Sold -10.000 12.00000 150.00 -0.05 119.95
CL A Short-term gain: $4.95
Long-term loss: $35.00
refer to confirm for Lot detail
03/06 MYSTERY CO 000000DD0 You Sold -5.000 20.00000 unknown -0.03 99.97
7 of 20
INVESTMENT REPORT
March 1, 2026 - March 31, 2026
Account # 100-000001
PLACEHOLDER HOLDER - INDIVIDUAL
Activity
Securities Bought & Sold (continued)
Settlement Symbol/ Total Transaction
Date Security Name CUSIP Description Quantity Price Cost Basis Cost Amount
03/09 EXAMPLE CORP COM 000000AA0 You Sold -2.000 50.00000 - 100.00
03/09 EXAMPLE CORP COM 000000AA0 Cancelled Sell 2.000 50.00000 - -100.00
CXL PLACEHOLDER NOTE
s03/10 SAMPLE INDS INC 000000BB0 You Sold -1.000 30.00000 -0.01 29.99
s03/11 SAMPLE INDS INC 000000BB0 You Sold -1.000 30.00000 21.00 30.00
Long-term gain: $9.00
s03/12 EXAMPLE STATE BOND 000000EE0 Redeemed -1,000.000 - $1,000.00 - $1,000.00
Long-term loss: $12.50
Total Securities Bought -$500.00
Total Securities Sold $1,479.89 -$0.11
Dividends, Interest & Other Income
03/13 EXAMPLE CORP COM 000000AA0 You Sold -9.000 1.00000 9.00 - 9.00
"""


def _sales(text=_SALES_TEXT):
    period = ppt.parse_statement_period(text)
    blocks = ppt.parse_account_blocks(text)
    assert len(blocks) == 1
    return ppt.parse_sales_block(blocks[0].text, period=period)


def test_sales_are_read_and_purchases_are_not():
    rows = _sales()
    assert [r.settlement_date.day for r in rows] == [4, 5, 6, 10, 11, 12]
    assert {r.action for r in rows} == {"You Sold", "Redeemed"}


def test_a_sale_carries_its_printed_basis_charges_and_term():
    sale = _sales()[0]
    assert sale.settlement_date == date(2026, 3, 4)
    assert (sale.description, sale.symbol) == ("SAMPLE INDS INC", "000000BB0")
    assert sale.specific_share_id is True
    assert (sale.quantity, sale.price, sale.cost_basis) == (4.0, 25.0, 80.0)
    assert (sale.transaction_cost, sale.amount) == (-0.02, 99.98)
    assert (sale.term, sale.gain_loss) == ("short", 19.98)


def test_a_sale_over_both_terms_names_no_single_term():
    sale = _sales()[1]
    assert sale.term is None
    assert sale.gain_loss == -30.05
    assert sale.terms == [("short", 4.95), ("long", -35.0)]


def test_an_unknown_basis_is_null_and_so_is_a_missing_term():
    sale = _sales()[2]
    assert sale.cost_basis is None
    assert sale.cells[2] == "unknown"
    assert sale.specific_share_id is False
    assert (sale.term, sale.gain_loss) == (None, None)


def test_a_sale_and_its_cancellation_drop_out_together():
    assert not [r for r in _sales() if r.settlement_date.day == 9]


def test_a_cancellation_with_no_sale_on_the_statement_stays():
    text = _SALES_TEXT.replace(
        "03/09 EXAMPLE CORP COM 000000AA0 You Sold -2.000 50.00000 - 100.00\n", "")
    cancel = [r for r in _sales(text) if r.settlement_date.day == 9]
    assert [r.action for r in cancel] == ["Cancelled Sell"]


def test_a_blank_cell_is_told_apart_by_the_row_arithmetic():
    by_day = {r.settlement_date.day: r for r in _sales()}
    # 1 × 30.00 − 0.01 = 29.99: the middle figure is the charge.
    assert (by_day[10].cost_basis, by_day[10].transaction_cost) == (None, -0.01)
    # 1 × 30.00 + 21.00 ≠ 30.00: the middle figure is the basis.
    assert (by_day[11].cost_basis, by_day[11].transaction_cost) == (21.0, None)


def test_a_redemption_reads_its_basis_without_a_price():
    bond = _sales()[-1]
    assert (bond.action, bond.price, bond.cost_basis) == ("Redeemed", None, 1000.0)
    assert (bond.quantity, bond.amount) == (1000.0, 1000.0)
    assert (bond.term, bond.gain_loss) == ("long", -12.5)


def test_the_listing_ends_at_its_totals():
    # A dated row after the totals belongs to another section.
    assert all(r.settlement_date.day != 13 for r in _sales())


def test_the_pdf_entry_point_returns_the_sales(monkeypatch):
    monkeypatch.setattr(ppt, "_extract_pdf_text", lambda path: _SALES_TEXT)
    out = ppt.parse_supplied_statement_pdf("/fake/path.pdf")
    (account,) = out["accounts"]
    first = account["sales"][0]
    assert first["settlement_date"] == "2026-03-04"
    assert first["specific_share_id"] is True
    assert first["terms"] == [["short", 19.98]]
