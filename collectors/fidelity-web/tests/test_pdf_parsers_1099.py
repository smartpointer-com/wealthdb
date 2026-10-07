"""
Unit tests for pdf_parsers_1099.py — the Form 1099-B lot parser.

The parser reads placed words, so the fixtures build lines of
``Word``s by hand at the x-positions a Consolidated 1099 prints its
columns at. Every account number, security, date and amount is
invented.
"""

from __future__ import annotations

import sys
from datetime import date
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import pdf_parsers_1099 as P  # noqa: E402


# ============================================================
# Fixture builders
# ============================================================

def _prose(text, x0=22.0):
    """A line of running text: words left to right from ``x0``."""
    out = []
    for token in text.split():
        out.append(P.Word(token, x0, x0 + 5 * len(token)))
        x0 += 5 * len(token) + 3
    return out


# Where each box label starts, and where its right-aligned figures end.
_LABEL_X0 = {"1d": 288, "1e": 368, "1f": 438, "1g": 490,
             "Gain/Loss": 557, "4": 640, "14": 706}
_FIGURE_X1 = {"proceeds": 333, "cost": 405, "discount": 477, "wash": 540,
              "gain": 603, "federal": 675, "state": 736}


def _header():
    """The first column-header line, with every box label placed."""
    line = [P.Word("Action", 29, 51), P.Word("Quantity", 114, 144)]
    line += [P.Word(label, x0, x0 + 9) for label, x0 in _LABEL_X0.items()]
    return sorted(line, key=lambda w: w.x0)


def _row(action, qty, acquired, sold, **figures):
    """One lot row. ``figures`` maps a column (proceeds, cost, discount,
    wash, gain, federal, state) to the text printed in it; a column left
    out prints nothing, as on the form."""
    line = _prose(action, 31)
    line.append(P.Word(qty, 144 - 5 * len(qty), 144))
    line.append(P.Word(acquired, 158, 189))
    line.append(P.Word(sold, 212, 243))
    for column, text in figures.items():
        x1 = _FIGURE_X1[column]
        line.append(P.Word(text, x1 - 5 * len(text), x1))
    return line


def _section(text):
    return _prose(text)


_BOX_A = ("Short-term transactions for which basis is reported to the IRS "
          "--report on Form 8949 with Box A checked and/or Schedule D, Part I")
_BOX_B = ("Short-term transactions for which basis is not reported to the IRS "
          "--report on Form 8949 with Box B checked and/or Schedule D, Part I")
_BOX_D = ("Long-term transactions for which basis is reported to the IRS "
          "--report on Form 8949 with Box D checked and/or Schedule D, Part II")
_BOX_E = ("Long-term transactions for which basis is not reported to the IRS "
          "--report on Form 8949 with Box E checked and/or Schedule D, Part II")
_UNKNOWN = ("Transactions for which basisis not reported to the IRS and Term "
            "is Unknown--report on Form 8949 with Box B or E checked")


def _page(section, *body, page_no=2):
    """One 1099-B page: masthead, form title, section, column header,
    then ``body``, then the footer."""
    return [
        _prose("2025 TAX REPORTING STATEMENT", 321),
        _prose("PLACEHOLDER HOLDER Account No. 100-000001 Customer Service:", 322),
        _prose("FORM 1099-B* Copy B for Recipient OMB No. 1545-0715"),
        _section(section),
        _prose("1a Description of property, Stock or Other Symbol, CUSIP", 29),
        _header(),
        _prose("Acquired or Disposed Other Basis (b) Market Loss", 148),
        *body,
        _prose(f"02/15/2026 9000000000 Pages {page_no} of 9"),
    ]


def _summary_page():
    return [
        _prose("2025 TAX REPORTING STATEMENT", 321),
        _prose("PLACEHOLDER HOLDER Account No. 100-000001 Customer Service:", 322),
        _prose("Form 1099-DIV 2025 Dividends and Distributions"),
        _prose("1a Total Ordinary Dividends..........0.00"),
        _prose("02/15/2026 9000000000 Pages 1 of 9"),
    ]


_DOC = [
    *_summary_page(),
    *_page(
        _BOX_A,
        _prose("EXAMPLE CORP COM USD0.01,EXMP,000000AA0", 27),
        _row("Sale", "10.000", "01/02/25", "03/04/25",
             proceeds="1,100.00", cost="1,000.00", gain="100.00"),
        _row("Sale", "5.000", "02/03/25", "03/05/25",
             proceeds="400.00", cost="500.00", wash="25.00", gain="-100.00"),
        _prose("Subtotals 1,500.00 1,500.00", 26),
        _prose("- - - - - - - - - -", 18),
        _prose("SAMPLE TREASURY NOTE,000000BB0", 27),
        _row("Sale", "1,000.000", "05/15/24", "02/01/25",
             proceeds="990.00", cost="980.00", discount="4.50", gain="10.00"),
    ),
    *_page(
        _BOX_A,
        # The security runs on past the page break without its line.
        _row("!C Cash In Lieu", "0.250", "05/15/24", "02/02/25",
             proceeds="2.10", cost="2.05(f)", gain="0.05"),
        page_no=3,
    ),
    *_page(
        _BOX_B,
        _prose("PLACEHOLDER HLDGS INC,PLHD,000000CC0", 27),
        _row("Sale", "3.000", "VARIOUS", "04/05/25",
             proceeds="300.00", cost="210.00", gain="90.00"),
        page_no=4,
    ),
    *_page(
        _BOX_D,
        _prose("PLACEHOLDER HLDGS INC,PLHD,000000CC0", 27),
        _row("Sale", "2.000", "07/08/98", "05/06/25",
             proceeds="200.00", cost="20.00", gain="180.00", federal="4.00"),
        page_no=5,
    ),
    *_page(
        _BOX_E,
        _prose("MYSTERY CO COM,000000DD0", 27),
        _row("Merger", "4.000", "01/01/20", "06/07/25",
             proceeds="80.00", gain="80.00"),
        page_no=6,
    ),
    *_page(
        _UNKNOWN,
        _prose("ODD ONE PLC ORD,ODD,000000EE0", 27),
        _row("Sale", "6.000", "Unknown", "07/08/25",
             proceeds="60.00", cost="Unknown"),
        _prose("TOTALS 60.00 Unknown 0.00 0.00 0.00", 26),
        page_no=7,
    ),
    _prose("Form 1099-OID 2025 Original Issue Discount"),
    # Off the 1099-B pages a row-shaped line is not a lot.
    _row("Sale", "1.000", "01/01/25", "02/02/25", proceeds="1.00"),
]


def _lots():
    return P.parse_1099b_lines(_DOC, tax_year=2025)


# ============================================================
# Identity
# ============================================================

def test_identity_reads_year_account_and_prepared_date():
    year, acct, prepared, corrected = P.parse_identity(_DOC)
    assert (year, acct) == (2025, "100000001")
    assert prepared == date(2026, 2, 15)
    assert corrected is None


def test_identity_reads_the_corrected_stamp():
    lines = [_prose("02/20/2026 9000000000 Pages 2 of 2"), *_DOC]
    lines.insert(3, _prose("CORRECTED 02/21/2026"))
    *_, prepared, corrected = P.parse_identity(lines)
    # The cover's footer comes first: it is the date the form was prepared.
    assert prepared == date(2026, 2, 20)
    assert corrected == date(2026, 2, 21)


# ============================================================
# Lots
# ============================================================

def test_every_lot_on_the_1099b_pages_is_read():
    lots = _lots()
    assert len(lots) == 8
    assert [lot.action for lot in lots] == [
        "Sale", "Sale", "Sale", "Cash In Lieu", "Sale", "Sale", "Merger",
        "Sale"]


def test_a_lot_carries_its_security_dates_and_figures():
    lot = _lots()[0]
    assert (lot.description, lot.symbol, lot.cusip) == (
        "EXAMPLE CORP COM USD0.01", "EXMP", "000000AA0")
    assert (lot.quantity, lot.acquired_date, lot.date_sold) == (
        10.0, "2025-01-02", "2025-03-04")
    assert (lot.proceeds, lot.cost_basis, lot.gain_loss) == (1100.0, 1000.0, 100.0)
    assert lot.wash_sale_disallowed is None
    assert lot.accrued_market_discount is None


def test_a_wash_sale_and_a_market_discount_land_in_their_own_columns():
    # Both rows print four figures; only the position tells 1f from 1g.
    wash, bond = _lots()[1], _lots()[2]
    assert (wash.wash_sale_disallowed, wash.accrued_market_discount) == (25.0, None)
    assert (bond.accrued_market_discount, bond.wash_sale_disallowed) == (4.5, None)


def test_a_security_without_a_symbol_keys_on_its_cusip():
    bond = _lots()[2]
    assert (bond.description, bond.symbol, bond.cusip) == (
        "SAMPLE TREASURY NOTE", None, "000000BB0")


def test_a_security_runs_on_across_a_page_break():
    cil = _lots()[3]
    assert cil.cusip == "000000BB0"
    assert cil.corrected is True
    assert cil.cost_basis == 2.05
    assert cil.footnotes == {"cost_basis": ["f"]}


def test_the_section_gives_term_covered_and_box():
    lots = _lots()
    assert [(lot.form_8949_box, lot.term, lot.covered) for lot in lots] == [
        ("A", "short", True)] * 4 + [
        ("B", "short", False),
        ("D", "long", True),
        ("E", "long", False),
        (None, None, False),
    ]


def test_various_and_unknown_acquired_dates_stay_as_printed():
    lots = _lots()
    assert lots[4].acquired_date == "Various"
    assert lots[7].acquired_date == "Unknown"
    assert lots[7].cost_basis is None


def test_a_two_digit_year_after_the_tax_year_is_last_century():
    assert _lots()[5].acquired_date == "1998-07-08"


def test_withholding_is_read_and_a_blank_basis_is_null():
    long_covered, noncovered = _lots()[5], _lots()[6]
    assert long_covered.federal_tax_withheld == 4.0
    assert noncovered.cost_basis is None
    assert noncovered.proceeds == 80.0


def test_a_document_with_no_1099b_pages_has_no_lots():
    assert P.parse_1099b_lines(_summary_page(), tax_year=2025) == []
