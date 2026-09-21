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
# no-positions sentence, and the stated $0 portfolio total in both
# of the places the layout prints it.
_NO_POSITIONS_TEXT = """\
SVB WEALTH ADVISORY, INC.
STATEMENT FOR THE PERIOD JUNE 1, 2022 TO JUNE 30, 2022
EXAMPLE HOLDER - Example Property
Account Number: SVM-000000
For questions about your accounts: TOTAL VALUE OF YOUR PORTFOLIO $0.00
Account Overview
BEGINNING VALUE $1,000.00 $1,000.00
ENDING VALUE (AS OF 06/30/22) $0.00 $0.00
Account carried with National Financial Services LLC
Holdings
There were no positions in your account at the close of the statement period.
Activity
"""

# A statement carrying every Activity section, one row per known
# Transaction verb, in the sign each prints with. The section
# totals are the statement's own arithmetic, so the fixture also
# proves the reconciliation.
_ACTIVITY_TEXT = """\
SVB INVESTMENT SERVICES, INC.
STATEMENT FOR THE PERIOD JANUARY 1, 2022 TO JANUARY 31, 2022
EXAMPLE HOLDER - Example Property
Account Number: SVM-000000
For questions about your accounts: TOTAL VALUE OF YOUR PORTFOLIO $7,000.00
ENDING VALUE (AS OF 01/31/22) $7,000.00 $7,000.00
Holdings
HOLDINGS > EQUITIES - 100.00% of Total Account Value
Symbol/Cusip Price on Current Estimated
Description Account Type Quantity 01/31/22 Market Value Annual Income
EXAMPLE COMPANY CL A AAAA 175 $40.00 $7,000.00
Total Securities $7,000.00
Activity
ADDITIONS AND WITHDRAWALS > DEPOSITS
Account
Date Type Transaction Description Quantity Amount
Deposits
01/04/22 CASH DIRECT DEPOSIT EXAMPLE BROKERAGE MONEYLINK $500.00
DepositsDeposits
01/05/22 CASH WIRE TRANS FROM BANK WR00 000001 $1,000.00
Total Deposits $1,500.00
ACTIVITY >ADDITIONS AND WITHDRAWALS > OTHER ADDITIONS AND WITHDRAWALS
Account
Date Type Transaction Description Quantity Amount
Other Additions and Withdrawals
01/06/22 CASH TRANSFERRED FROM VS SV M-0000 0 0-1 $2,000.00
Other Additions and WithdrawalsOther Additions and Withdrawals
01/07/22 CASH TRANSFERRED TO VS SV R-0000 00 -1 ($3,000.00)
01/08/22 CASH WIRE TRANS TO BANK WD00 000002 1 ST PTY TRF TO EXAMPLE ($4,000.00)
EXAMPLE BANK AG *****000X
01/09/22 CASH DIRECT DEBIT EXAMPLE BROKERAGE MONEYLINK ($500.00)
Total Other Additions and Withdrawals ($5,500.00)
TOTAL ADDITIONS AND WITHDRAWALS ($4,000.00)
ACTIVITY >INCOME > TAXABLE INCOME
Settlement Account
Date Type Transaction Description Quantity Amount
Taxable Dividends
01/10/22 CASH DIVIDEND RECEIVED EXAMPLE COMPANY CL A $300.00
01/11/22 CASH INTEREST EXAMPLE CORP NOTE CALL MAKE WHOLE $200.00
Corporate Accrued Interest Earned $50.00
Total Taxable Income $550.00
ACTIVITY >INCOME > NON-TAXABLE INCOME
Settlement Account
Date Type Transaction Description Quantity Amount
Return of Capital
01/12/22 CASH RETURN OF CAPITAL EXAMPLE PARTNERS L P $100.00
COM
TOTAL INCOME $650.00
ACTIVITY >TAXES,FEES AND EXPENSES
Settlement Account
Date Type Transaction Description Quantity Amount
Non-Resident Alien Tax
01/10/22 CASH NON-RESIDENT TAX EXAMPLE COMPANY CL A ($90.00)
01/11/22 CASH FOREIGN TAX PAID EXAMPLE COMPANY CL A ($10.00)
01/13/22 CASH ADJ NON-RESIDENT TAX EXAMPLE COMPANY CL A $30.00
Account Fees
01/14/22 CASH FEE PAID EXAMPLE ACCOUNT FEE ($200.00)
01/15/22 CASH ADVISOR FEE DEDUCTED Advisor Fee ($400.00)
01/16/22 MARGIN MARGIN INTEREST @ 3.250% ($20.00)
01/17/22 CASH ADJUSTMENT FEE REVERSAL-EXAMPLE $60.00
TOTAL TAXES, FEES AND EXPENSES ($630.00)
ACTIVITY >MISC. & CORPORATE ACTIONS
This section includes miscellaneous and corporate action transactions.
Account
Date Type Transaction Description Quantity Amount
01/18/22 CASH TRANSFERRED TO EXAMPLE BROAD MARKET INDEX FUND VS (1,000) $0.00
SV R-0000 00-1
TRAN VALUE: ($5,000.00)
01/19/22 CASH DISTRIBUTION EXAMPLE PARTNERS L P 100 $1,200.00
01/20/22 MARGIN EXPIRED CALL (AAAA) EXAMPLE COMPANY CL A (2) $0.00
TRAN VALUE: $800.00
TOTAL MISC. & CORPORATE ACTIONS ($3,000.00)
ACTIVITY >CORE FUND ACTIVITY
Settlement Account
Date Type Transaction Description Quantity Amount
01/22/22 CASH YOU SOLD EXAMPLE GOVERNMENT MONEY MARKET (900.0) $900.00
@ 1
01/23/22 CASH REINVESTMENT EXAMPLE GOVERNMENT MONEY MARKET 100.0 ($100.00)
TOTAL CORE FUND ACTIVITY $800.00
ACTIVITY >OTHER ACTIVITY
Settlement Account
Date Type Transaction Description Quantity Amount
01/24/22 MARGIN JOURNALED MARGIN TO CASH A/C ($700.00)
01/24/22 CASH JOURNALED MARGIN TO CASH A/C $700.00
TOTAL OTHER ACTIVITY $0.00
PURCHASES, SALES, AND REDEMPTIONS
Settlement Account
Date Type Transaction Description Quantity Amount
Securities Purchased
01/25/22 CASH YOU BOUGHT EXAMPLE COMPANY CL A @ 40.00 100 ($4,000.00)
Securities Sold
01/26/22 CASH YOU SOLD EXAMPLE BROAD MARKET INDEX FUND 10 $3,000.00
Total Securities Sold $3,000.00
ACTIVITY > TRADES PENDING SETTLEMENT
These trades settle after the closing date of this statement.
Trade Settlement
Date Date Transaction Description Quantity Amount
01/28/22 02/01/22 BOUGHT EXAMPLE COMPANY CL A 20 ($800.00)
ACTIVITY >PENDING DISTRIBUTIONS 1
Symbol/Cusip Security Description Eligible Quantity Rate Payment Amount
Pending Accrued Dividends
AAAA EXAMPLE COMPANY CL A 175 $1.00 $175.00
Miscellaneous Footnotes
CHANGE IN VALUE reflects appreciation or depreciation of your holdings.
"""

# The 2021 corporate-actions template: banner spelled out in full,
# no section total, and TRAN VALUE printed in the cash-equivalent
# convention — a receipt of shares parenthesised, a delivery plain.
_LEGACY_MISC_TEXT = """\
SVB WEALTH ADVISORY, INC.
STATEMENT FOR THE PERIOD JANUARY 1, 2021 TO MARCH 31, 2021
EXAMPLE HOLDER - Example Property
Account Number: SVT-000000
For questions about your accounts: TOTAL VALUE OF YOUR PORTFOLIO $1,000.00
ENDING VALUE (AS OF 03/31/21) $1,000.00 $1,000.00
Holdings
There were no positions in your account at the close of the statement period.
Activity
ACTIVITY >MISCELLANEOUS & CORPORATE ACTIONS
This section includes miscellaneous and certain corporate action transactions.
Account
Date Type Transaction Description Quantity Amount
03/08/21 CASH RECEIVED FROM YOU EXAMPLE COMPANY CL A 1,000 $0.00
TRAN VALUE: ($40,000.00)
03/08/21 CASH TRANSFERRED TO EXAMPLE COMPANY CL A VS (900) $0.00
SV M-0000 00-1
TRAN VALUE: $36,000.00
Miscellaneous Footnotes
"""

# A scanned deposit / mortgage statement: no text layer at all.
_IMAGE_ONLY_TEXT = "\n \n\n"


# ============================================================
# parse_statement_period — uppercase, "TO", quarterly span
# ============================================================

_SHADOWED_TEXT = """\
SVB WEALTH ADVISORY, INC.
STATEMENT FOR THE PERIOD DECEMBER 1, 2022 TO DECEMBER 31, 2022
EXAMPLE HOLDER - Example Property
Account Number: SVM-000000
Holdings
HOLDINGS > EQUITIES - 100.00% of Total Account Value
Symbol/Cusip Price on Current Estimated
Description Account Type Quantity 12/31/22 Market Value Annual Income
Equity
S&P EXAMPLE INDEX CO COM AAAA 10 $100.00 $1,000.00 $20.00
S&P A
ON EXAMPLE DEVICES CORP COM BBBB 200 $50.00 $10,000.00 $30.00
ON JAN 01, JUL 01
Total Return Fund TRFXX 100 $10.00 $1,000.00 $5.00
Total Securities $11,000.00
"""


def test_a_reinvestment_price_is_not_an_undated_amount():
    """An undated line inside a section is an amount the section's stated
    total already includes. A money-market dividend prints its reinvestment
    PRICE beneath it — "REINVEST @ $1.00" — and counting a dollar a share as
    an amount puts the section a dollar over its own total."""
    assert ps._UNDATED_AMOUNT_RE.match("REINVEST @ $1.00") is None
    assert ps._UNDATED_AMOUNT_RE.match("REINVEST @ $1.000") is None
    # The real undated amounts still land.
    m = ps._UNDATED_AMOUNT_RE.match("Corporate Accrued Interest Earned $50.00")
    assert m is not None and m["amt"] == "$50.00"


def test_a_cusip_keyed_row_is_not_refused_for_width():
    """A CUSIP is an identifier, not a figure — but an all-digit one reads as
    numeric, so a CUSIP-keyed row counts five trailing numerics where the
    layout allows four. Refusing it on width silently drops the holding."""
    line = ("EXAMPLE ADR EA REP 2 CL A  111111111  200  $50.00  $10,000.00  $400.00")
    row = ps._parse_security_row(line)
    assert row is not None, "CUSIP-keyed row refused on width"
    assert row.instrument_key == "111111111"
    assert row.description == "EXAMPLE ADR EA REP 2 CL A"
    assert row.quantity == 200.0 and row.market_value == 10000.0
    # A description ending in a numeral joins the run, so the CUSIP is not
    # the token the run opens on.
    row = ps._parse_security_row(
        "EXAMPLE ADR SPON ADS EACH REPR 2  222222222  460  $5.00  $2,300.00")
    assert row is not None, "CUSIP not the run's first token"
    assert row.instrument_key == "222222222"
    assert row.description == "EXAMPLE ADR SPON ADS EACH REPR 2"
    assert row.market_value == 2300.0
    # A genuinely over-wide row is still refused.
    assert ps._parse_security_row(
        "EXAMPLE CO COM EXCO 1 2 3 $4.00 $5.00 $6.00") is None


def test_a_security_named_like_boilerplate_is_still_a_holding():
    """Two boilerplate prefixes are short enough to collide with a real
    security's name. What they exist to catch — a credit rating, a bond's
    coupon-date line — never carries a symbol column before a numeric tail,
    so a line that parses as a holdings row wins over them."""
    rows = ps.parse_holdings_block(ps.parse_account_blocks(_SHADOWED_TEXT)[0].text)
    by_key = {r.instrument_key: r for r in rows}
    assert "AAAA" in by_key, "S&P-prefixed holding was dropped as boilerplate"
    assert "BBBB" in by_key, "ON-prefixed holding was dropped as boilerplate"
    assert by_key["AAAA"].market_value == 1000.0
    assert by_key["BBBB"].market_value == 10000.0
    assert by_key["AAAA"].description == "S&P EXAMPLE INDEX CO COM"


def test_a_structural_label_keeps_precedence_over_a_data_row():
    """The rest of the prefix list is structural, and a label CAN tokenise
    like a data row — admitting one would invent a holding out of a summary
    line, which is the failure the prefix list exists to prevent."""
    rows = ps.parse_holdings_block(ps.parse_account_blocks(_SHADOWED_TEXT)[0].text)
    # "Total Return Fund TRFXX 100 $10.00 $1,000.00 $5.00" tokenises exactly
    # like a holdings row — symbol column, four trailing numerics — and is
    # still a section label. Admitting it would invent a holding from a total.
    assert "TRFXX" not in {r.instrument_key for r in rows}
    # And the rating / coupon-date lines the two prefixes exist for stay out.
    assert len(rows) == 2


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
# classify_statement_text — family, off the text not the filename
# ============================================================

def test_classify_brokerage_across_both_mastheads():
    # The masthead changed mid-archive; the period + account headers
    # are what identify the family, so both spellings classify alike.
    assert ps.classify_statement_text(_MM_BOND_TEXT) == ps.FAMILY_BROKERAGE
    assert ps.classify_statement_text(_ACTIVITY_TEXT) == ps.FAMILY_BROKERAGE
    assert "SVB INVESTMENT SERVICES" in _ACTIVITY_TEXT
    assert "SVB WEALTH ADVISORY" in _MM_BOND_TEXT


def test_classify_image_only_has_no_text_layer():
    assert ps.classify_statement_text(_IMAGE_ONLY_TEXT) == ps.FAMILY_IMAGE_ONLY
    assert ps.classify_statement_text("") == ps.FAMILY_IMAGE_ONLY


def test_classify_unknown_when_text_is_not_a_brokerage_statement():
    other = "A LETTER ABOUT SOMETHING ELSE ENTIRELY, WITH PLENTY OF TEXT ON IT."
    assert ps.classify_statement_text(other) == ps.FAMILY_UNKNOWN


# ============================================================
# parse_statement_total — the STATED closing value
# ============================================================

def test_stated_total_read_from_both_printings():
    assert ps.parse_statement_total(_ACTIVITY_TEXT) == 7000.0


def test_stated_zero_is_a_value_not_an_absence():
    assert ps.parse_statement_total(_NO_POSITIONS_TEXT) == 0.0


def test_stated_total_none_when_the_two_printings_disagree():
    # Either printing may be misread; requiring agreement means a
    # misread yields "the statement did not say" rather than a value.
    text = _NO_POSITIONS_TEXT.replace(
        "ENDING VALUE (AS OF 06/30/22) $0.00 $0.00",
        "ENDING VALUE (AS OF 06/30/22) $12.00 $12.00")
    assert ps.parse_statement_total(text) is None


def test_stated_total_none_when_absent():
    assert ps.parse_statement_total("no totals printed here") is None


# ============================================================
# parse_activity_block — sections, signs, totals
# ============================================================

def _activity():
    block = ps.parse_account_blocks(_ACTIVITY_TEXT)[0]
    return ps.parse_activity_block(block.text)


def _by_verb(rows):
    return {r.verb: r for r in rows if r.verb}


def test_activity_sign_per_verb():
    # The make-or-break for the flow legs: gold reads a wire's
    # direction off the sign alone, so an inverted parse reverses a
    # transfer instead of failing. One assertion per verb the
    # statements print, in the direction each prints with.
    rows, _ = _activity()
    got = {v: r.amount for v, r in _by_verb(rows).items()}
    assert got == {
        "DIRECT DEPOSIT": 500.0,
        "WIRE TRANS FROM BANK": 1000.0,
        "TRANSFERRED FROM": 2000.0,
        "TRANSFERRED TO": -5000.0,      # the misc row's TRAN VALUE wins
        "WIRE TRANS TO BANK": -4000.0,
        "DIRECT DEBIT": -500.0,
        "DIVIDEND RECEIVED": 300.0,
        "INTEREST": 200.0,
        "RETURN OF CAPITAL": 100.0,
        "NON-RESIDENT TAX": -90.0,
        "FOREIGN TAX PAID": -10.0,
        "ADJ NON-RESIDENT TAX": 30.0,   # a withholding REVERSAL: a credit
        "FEE PAID": -200.0,
        "ADVISOR FEE DEDUCTED": -400.0,
        "MARGIN INTEREST": -20.0,
        "ADJUSTMENT": 60.0,
        "DISTRIBUTION": 1200.0,
        "EXPIRED": 800.0,
        "YOU SOLD": 3000.0,             # the blotter row, after core fund
        "REINVESTMENT": -100.0,
        "JOURNALED": 700.0,             # the cash leg, after the margin leg
        "YOU BOUGHT": -4000.0,
    }


def test_activity_sections_reconcile_to_stated_totals():
    rows, totals = _activity()
    sums = {}
    for r in rows:
        sums[r.section] = round(sums.get(r.section, 0.0) + (r.amount or 0.0), 2)
    assert totals == {
        ps.SECTION_ADDITIONS: -4000.0,
        ps.SECTION_INCOME: 650.0,
        ps.SECTION_TAXES_FEES: -630.0,
        ps.SECTION_MISC: -3000.0,
        ps.SECTION_CORE_FUND: 800.0,
        ps.SECTION_OTHER: 0.0,
    }
    for section, stated in totals.items():
        assert sums[section] == stated, section


def test_activity_undated_accrual_row_counts_but_is_not_a_movement():
    rows, _ = _activity()
    undated = [r for r in rows
               if r.date is None and r.section == ps.SECTION_INCOME]
    assert [(r.description, r.amount) for r in undated] == [
        ("Corporate Accrued Interest Earned", 50.0)]
    # It carries no Transaction column, which is what marks it as not
    # a movement — the income section's stated total still includes it,
    # so without the row the section would not reconcile.
    assert undated[0].verb == ""


def test_activity_quantity_only_where_the_column_exists():
    # A counterparty account id kerns into fragments whose tail
    # ("-1") parses as a number. Reading a Quantity outside the
    # sections that print one is what would harvest it.
    rows, _ = _activity()
    additions = [r for r in rows if r.section == ps.SECTION_ADDITIONS]
    assert all(r.quantity is None for r in additions)
    misc = {r.verb: r.quantity for r in rows if r.section == ps.SECTION_MISC}
    assert misc == {"TRANSFERRED TO": -1000.0, "DISTRIBUTION": 100.0,
                    "EXPIRED": -2.0}


def test_activity_kerned_counterparty_stays_in_the_description():
    rows, _ = _activity()
    row = next(r for r in rows
               if r.section == ps.SECTION_ADDITIONS and r.verb == "TRANSFERRED TO")
    assert row.description == "VS SV R-0000 00 -1"
    assert row.amount == -3000.0


def test_activity_ordinals_are_unique_and_in_document_order():
    rows, _ = _activity()
    assert [r.ordinal for r in rows] == list(range(len(rows)))


def test_activity_same_day_duplicates_stay_distinct():
    # Two identical same-day transfers from one counterparty differ
    # only by their position on the page.
    text = _ACTIVITY_TEXT.replace(
        "01/06/22 CASH TRANSFERRED FROM VS SV M-0000 0 0-1 $2,000.00",
        "01/06/22 CASH TRANSFERRED FROM VS SV M-0000 0 0-1 $2,000.00\n"
        "01/06/22 CASH TRANSFERRED FROM VS SV M-0000 0 0-1 $2,000.00")
    rows, _ = ps.parse_activity_block(ps.parse_account_blocks(text)[0].text)
    dupes = [r for r in rows if r.verb == "TRANSFERRED FROM"]
    assert len(dupes) == 2
    assert dupes[0].ordinal != dupes[1].ordinal


def test_activity_pending_and_blotter_sections_are_labelled_not_dropped():
    rows, totals = _activity()
    sections = {r.section for r in rows}
    assert ps.SECTION_TRADES in sections
    assert ps.SECTION_PENDING_DISTRIBUTIONS in sections
    # Neither strikes a section total, so neither joins a reconciliation.
    assert ps.SECTION_TRADES not in totals
    assert ps.SECTION_PENDING_DISTRIBUTIONS not in totals


def test_activity_trades_pending_settlement_rows_are_not_movements():
    # They carry two dates and no account-type column, and settle
    # into the NEXT statement's blotter — booking them double-counts.
    rows, _ = _activity()
    assert not any(r.date == "2022-01-28" for r in rows)


def test_activity_dates_span_the_statement_period():
    rows, _ = _activity()
    dated = [r.date for r in rows if r.date]
    assert min(dated) == "2022-01-04"
    assert max(dated) == "2022-01-26"


def test_activity_row_whose_date_names_no_real_day_is_refused():
    # Kept with a null date, the row would read as one of the undated
    # components a section prints: counted in the stated total, never
    # booked. The section would still reconcile and the movement would be
    # gone. Refused, its amount leaves the sum and the section stops
    # adding up, which is the only signal the statement offers.
    text = _ACTIVITY_TEXT.replace("01/08/22 CASH WIRE TRANS TO BANK",
                                  "01/32/22 CASH WIRE TRANS TO BANK")
    rows, totals = ps.parse_activity_block(
        ps.parse_account_blocks(text)[0].text)
    assert not any(r.verb == "WIRE TRANS TO BANK" for r in rows)
    assert not any(r.description.startswith("WD00") for r in rows)
    parsed = sum(r.amount or 0.0 for r in rows
                 if r.section == ps.SECTION_ADDITIONS)
    assert parsed != totals[ps.SECTION_ADDITIONS]


def test_activity_empty_when_no_region():
    assert ps.parse_activity_block("Holdings\nnothing here\n") == ([], {})


def test_legacy_corporate_actions_tran_value_is_normalised():
    # The 2021 template prints TRAN VALUE cash-equivalent: a receipt
    # of shares parenthesised, a delivery plain. Normalised to the
    # later template's value-flow convention, a receipt reads
    # positive and the two legs of a move cancel.
    block = ps.parse_account_blocks(_LEGACY_MISC_TEXT)[0]
    rows, totals = ps.parse_activity_block(block.text)
    got = {r.verb: r.amount for r in rows if r.verb}
    assert got == {"RECEIVED FROM YOU": 40000.0, "TRANSFERRED TO": -36000.0}
    # The legacy template strikes no section total, so there is
    # nothing to reconcile against.
    assert totals == {}


# ============================================================
# parse_svbwa_statement_pdf — orchestration + signature gate
# ============================================================

def test_pdf_signature_mismatch(monkeypatch):
    other_registration = _MM_BOND_TEXT.replace(
        _SIG, "EXAMPLE COMPANY LLC - Example Business")
    monkeypatch.setattr(
        ps, "_extract_pdf_text", lambda path: other_registration)
    out = ps.parse_svbwa_statement_pdf(
        "/fake/path.pdf", expected_signatures=(_SIG,),
    )
    assert out["_error"] == "signature-mismatch"


def test_pdf_accepts_any_configured_registration(monkeypatch):
    # An archive can span several registrations; a statement signed
    # with any one of the configured ones is in scope.
    second = _MM_BOND_TEXT.replace(
        _SIG, "EXAMPLE COMPANY LLC - Example Business")
    monkeypatch.setattr(ps, "_extract_pdf_text", lambda path: second)
    out = ps.parse_svbwa_statement_pdf(
        "/fake/path.pdf",
        expected_signatures=(_SIG, "EXAMPLE COMPANY LLC"),
    )
    assert "_error" not in out


def test_pdf_accepts_when_signature_present(monkeypatch):
    monkeypatch.setattr(ps, "_extract_pdf_text", lambda path: _MM_BOND_TEXT)
    out = ps.parse_svbwa_statement_pdf(
        "/fake/path.pdf", expected_signatures=(_SIG,),
    )
    assert "_error" not in out
    assert out["family"] == ps.FAMILY_BROKERAGE
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


def test_pdf_activity_dicts_carry_the_loader_consumed_keys(monkeypatch):
    monkeypatch.setattr(ps, "_extract_pdf_text", lambda path: _ACTIVITY_TEXT)
    out = ps.parse_svbwa_statement_pdf("/fake/path.pdf")
    acct = out["accounts"][0]
    assert set(acct["activity"][0]) == {
        "date", "section", "account_type", "verb", "description",
        "quantity", "amount", "ordinal",
    }
    assert acct["activity_totals"][ps.SECTION_ADDITIONS] == -4000.0
    assert out["stated_total"] == 7000.0


def test_pdf_non_brokerage_never_reaches_the_row_parsers(monkeypatch):
    monkeypatch.setattr(ps, "_extract_pdf_text", lambda path: _IMAGE_ONLY_TEXT)
    out = ps.parse_svbwa_statement_pdf(
        "/fake/path.pdf", expected_signatures=(_SIG,))
    # Reported as its family, not as a signature failure: an
    # image-only document has no text for the guard to read.
    assert out["family"] == ps.FAMILY_IMAGE_ONLY
    assert out["accounts"] == []
    assert "_error" not in out


def test_pdf_no_positions_returns_account_with_period(monkeypatch):
    # A $0 closing statement returns the account (empty holdings),
    # the correct period_end, and the STATED zero — which is what
    # lets a real $0 snapshot be recorded without inferring one.
    monkeypatch.setattr(
        ps, "_extract_pdf_text", lambda path: _NO_POSITIONS_TEXT,
    )
    out = ps.parse_svbwa_statement_pdf("/fake/path.pdf")
    assert "_error" not in out
    assert out["period_end"] == "2022-06-30"
    assert out["stated_total"] == 0.0
    assert len(out["accounts"]) == 1
    assert out["accounts"][0]["account_external_id"] == "SVM-000000"
    assert out["accounts"][0]["holdings"] == []
