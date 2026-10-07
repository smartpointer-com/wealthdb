"""
Unit tests for pdf_parsers.py.

Hand-curated synthetic text mimics extracted statement text (both
the pdfplumber-tight and pypdfium2-spaced shapes seen on real
Schwab brokerage statements). No real PDFs touched — these tests are safe to ship
and run in CI without any account data.

A separate test module (test_pdf_parsers_fixture.py) handles the
fixture-driven case against a real local PDF; that one
auto-skips when the fixture isn't present.
"""

from __future__ import annotations

import sys
from datetime import date
from pathlib import Path

import pytest

# Import target under test. tests/ is a sibling of pdf_parsers.py
# in the repo root.
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import pdf_parsers as pp  # noqa: E402


# ============================================================
# parse_statement_period
# ============================================================

class TestStatementPeriod:
    def test_single_month_period(self):
        text = (
            "Schwab One International Account of AccountNickname\n"
            "AccountNickname\n"
            "StatementPeriod\n"
            "February 1-28, 2026\n"
        )
        result = pp.parse_statement_period(text)
        assert result == (date(2026, 2, 1), date(2026, 2, 28))

    def test_cross_month_period(self):
        text = "Period: January 30-February 5, 2025\n"
        result = pp.parse_statement_period(text)
        assert result == (date(2025, 1, 30), date(2025, 2, 5))

    def test_period_at_year_end(self):
        text = "December 1-31, 2023 footer noise\n"
        result = pp.parse_statement_period(text)
        assert result == (date(2023, 12, 1), date(2023, 12, 31))

    def test_no_period_returns_none(self):
        text = "Some statement without a parseable period header"
        assert pp.parse_statement_period(text) is None

    def test_period_with_spaces_after_hyphen(self):
        # Schwab is inconsistent about whitespace around hyphens.
        text = "March 1 - 31, 2024"
        assert pp.parse_statement_period(text) == (date(2024, 3, 1), date(2024, 3, 31))


# ============================================================
# parse_transactions row parsing
# ============================================================

# Minimal valid wrapper: "Transaction Details" header + rows +
# "TotalTransactions" terminator. parse_transactions ignores
# everything outside this section.
PERIOD_HEADER = "February 1-28, 2026\n"


def _wrap(rows_text: str) -> str:
    """Wrap synthetic transaction rows in the minimal headers
    parse_transactions expects."""
    return (
        PERIOD_HEADER
        + "Transaction Details\n"
        + "Symbol/ Price/Rate Charges/ Realized\n"
        + "Date Category Action CUSIP Description Quantity perShare($) Interest($) Amount($) Gain/(Loss)($)\n"
        + rows_text
        + "TotalTransactions ($1.00) $2.00\n"
    )


class TestAccountRegistration:
    """The header line we surface as silver's
    `accounts.account_registration` — verbatim from the
    statement-PDF header so wealthdb's gold adapter can map to
    its `tax_wrapper` enum. Three layout eras, three anchors."""

    def test_2025_plus_format(self):
        # 2025+: a single line "<LABEL> of Account Nickname".
        text = (
            "1 of 8\n"
            "Statement Period\n"
            "PLACEHOLDER NAME February 1-28, 2026\n"
            "Account Number\n"
            "0000-0000\n"
            "Schwab One International® Account of Account Nickname\n"
            "Nickname Goes Here\n"
        )
        assert pp.parse_account_registration(text) == (
            "Schwab One International® Account"
        )

    def test_2025_plus_ira(self):
        text = (
            "1 of 6\n"
            "Statement Period\n"
            "PLACEHOLDER NAME February 1-28, 2026\n"
            "Account Number\n"
            "0000-0000\n"
            "Contributory IRA of Account Nickname\n"
            "Nickname Goes Here\n"
        )
        assert pp.parse_account_registration(text) == "Contributory IRA"

    def test_2020_2024_format(self):
        # 2020-2024: "<LABEL> of" alone, followed by the
        # holder's name on the next line.
        text = (
            "Schwab One® International Account of\n"
            "PLACEHOLDER NAME\n"
            "Manage Your Account\n"
            "Account Number\n"
            "0000-0000\n"
            "Statement Period\n"
            "June 1-30, 2024\n"
        )
        assert pp.parse_account_registration(text) == (
            "Schwab One® International Account"
        )

    def test_2020_2024_custodial_utma(self):
        # The custodial header line is the same string for both
        # UTMA and UGMA accounts, so the parser also scans the
        # holder block immediately below it for a "<state>UTMA"
        # / "<state>UGMA" marker (UCAUTMA = California UTMA,
        # NYUTMA = New York UTMA, etc.) and promotes the label to
        # "<label> (UTMA)" / "<label> (UGMA)". Silver resolves
        # this so the wealthdb gold adapter doesn't have to
        # re-read bronze.
        text = (
            "Schwab One® Custodial Account of\n"
            "PLACEHOLDER CUST FOR\n"
            "PLACEHOLDER UCAUTMA\n"
            "UNTIL AGE 18\n"
            "Account Number\n"
            "0000-0000\n"
        )
        assert pp.parse_account_registration(text) == (
            "Schwab One® Custodial Account (UTMA)"
        )

    def test_2020_2024_custodial_ugma(self):
        text = (
            "Schwab One® Custodial Account of\n"
            "PLACEHOLDER CUST FOR\n"
            "PLACEHOLDER NYUGMA\n"
            "UNTIL AGE 18\n"
            "Account Number\n"
            "0000-0000\n"
        )
        assert pp.parse_account_registration(text) == (
            "Schwab One® Custodial Account (UGMA)"
        )

    def test_custodial_without_marker_stays_unaugmented(self):
        # Defensive: if neither UTMA nor UGMA appears in the
        # holder block (no real statement we've observed), the
        # raw header survives unaugmented.
        text = (
            "Schwab One® Custodial Account of\n"
            "PLACEHOLDER CUST FOR\n"
            "PLACEHOLDER\n"
            "Account Number\n"
            "0000-0000\n"
        )
        assert pp.parse_account_registration(text) == (
            "Schwab One® Custodial Account"
        )

    def test_non_custodial_not_augmented(self):
        # The augmentation only fires for custodial labels —
        # other registrations are returned verbatim even if the
        # surrounding text happens to contain "UTMA" / "UGMA"
        # (e.g. a fund name).
        text = (
            "Contributory IRA of\n"
            "PLACEHOLDER UTMA FUND HOLDINGS\n"
            "Account Number\n"
            "0000-0000\n"
        )
        assert pp.parse_account_registration(text) == "Contributory IRA"

    def test_2017_2019_format(self):
        # 2017-2019: bare label immediately above
        # "Account Number: <NNNN-NNNN>".
        text = (
            "Mail To\n"
            "Account Value Summary\n"
            "Total Account Value $ 0.00\n"
            "Schwab One® Account\n"
            "Account Number: 0000-0000\n"
            "Statement Period: June 1, 2018 to June 30, 2018\n"
        )
        assert pp.parse_account_registration(text) == "Schwab One® Account"

    def test_returns_none_when_no_header(self):
        # Random prose that doesn't contain any registration
        # anchor must return None.
        text = (
            "This statement was provided by Schwab.\n"
            "Cost basis information is not a guarantee.\n"
            "Please see the disclosures section.\n"
        )
        assert pp.parse_account_registration(text) is None

    def test_internal_whitespace_normalised(self):
        # Multiple spaces inside the label collapse to one —
        # so comparisons across statements are robust to
        # incidental spacing drift in pypdfium2's output.
        text = (
            "Schwab    One®   International   Account of Account Nickname\n"
        )
        assert pp.parse_account_registration(text) == (
            "Schwab One® International Account"
        )


class TestAccountNumber:
    """The page-1 header account number we surface into
    `accounts.payload.account_number_full` — the api↔web bridge
    key (INTEROP.md §1). Returned exactly as printed (dash kept);
    the gold join normalises to digits-only on both sides. Two
    anchor shapes: 2017-2019 inline colon form, 2020+ label line
    with the value on the following line."""

    def test_2025_plus_two_line_form(self):
        text = (
            "1 of 8\n"
            "Statement Period\n"
            "PLACEHOLDER NAME February 1-28, 2026\n"
            "Account Number\n"
            "1234-5678\n"
            "Schwab One International® Account of Account Nickname\n"
        )
        assert pp.parse_account_number(text) == "1234-5678"

    def test_2020_2024_two_line_form(self):
        text = (
            "Schwab One® International Account of\n"
            "PLACEHOLDER NAME\n"
            "Manage Your Account\n"
            "Account Number\n"
            "0000-0000\n"
            "Statement Period\n"
            "June 1-30, 2024\n"
        )
        assert pp.parse_account_number(text) == "0000-0000"

    def test_2017_2019_inline_form(self):
        text = (
            "Mail To\n"
            "Schwab One® Account\n"
            "Account Number: 1234-5678\n"
            "Statement Period: June 1, 2018 to June 30, 2018\n"
        )
        assert pp.parse_account_number(text) == "1234-5678"

    def test_inline_form_without_colon(self):
        text = "Account Number 1234-5678\n"
        assert pp.parse_account_number(text) == "1234-5678"

    def test_label_case_and_whitespace_tolerated(self):
        text = "  ACCOUNT  NUMBER:   1234-5678\n"
        assert pp.parse_account_number(text) == "1234-5678"

    def test_value_after_one_intervening_line(self):
        # pypdfium2's line ordering occasionally interleaves an
        # adjacent header cell between label and value; one
        # intervening line is tolerated.
        text = (
            "Account Number\n"
            "Statement Period\n"
            "1234-5678\n"
        )
        assert pp.parse_account_number(text) == "1234-5678"

    def test_value_too_far_from_label_is_ignored(self):
        text = (
            "Account Number\n"
            "Statement Period\n"
            "February 1-28, 2026\n"
            "1234-5678\n"
        )
        assert pp.parse_account_number(text) is None

    def test_undashed_value_not_matched(self):
        # Only the printed NNNN-NNNN form counts — a drifted
        # layout yields None rather than a guessed value.
        text = (
            "Account Number: 12345678\n"
            "Account Number\n"
            "12345678\n"
        )
        assert pp.parse_account_number(text) is None

    def test_no_header_returns_none(self):
        text = (
            "This statement was provided by Schwab.\n"
            "Please see the disclosures section.\n"
        )
        assert pp.parse_account_number(text) is None

    def test_bare_number_without_label_not_matched(self):
        # A NNNN-NNNN token elsewhere on the page (CUSIP fragment,
        # phone extension) must not be picked up without the
        # "Account Number" anchor.
        text = "Reference 1234-5678\n"
        assert pp.parse_account_number(text) is None

    def test_header_beyond_first_80_lines_ignored(self):
        text = "\n" * 100 + "Account Number: 1234-5678\n"
        assert pp.parse_account_number(text) is None


class TestSaleRows:
    def test_sale_with_realized_gain_short_term(self):
        text = _wrap(
            "02/02 Sale SYN1 SYNTHETICONEFUND (100.0000) 100.0000 0.10 10,000.00 500.00,(ST)\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.date == date(2026, 2, 2)
        assert r.category == "Sale"
        assert r.symbol == "SYN1"
        assert "SYNTHETICONEFUND" in r.description
        assert r.quantity == -100.0
        assert r.price == 100.0
        assert r.charges == 0.10
        assert r.amount == 10000.0
        assert r.realized_gain_loss == pytest.approx(500.00)
        assert r.term == "ST"

    def test_sale_with_realized_loss(self):
        text = _wrap(
            "03/15 Sale ABCD SOMECOMPANYINC (100.0000) 50.0000 0.05 5,000.00 (250.50),(LT)\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.realized_gain_loss == pytest.approx(-250.50)
        assert r.term == "LT"

    def test_sale_of_cusip_with_continuation(self):
        # Treasury sales render with a CUSIP and the description
        # wraps onto a second line ("NOTE DUE12/31/99"). Values
        # below are synthetic placeholders.
        text = _wrap(
            "02/06 Sale 000000AA0 SYNTHETICBONDNT (100,000.0000) 95.0000 50.00 95,000.00 100.00,(ST)\n"
            "NOTE DUE12/31/99\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.symbol == "000000AA0"  # CUSIP
        assert "SYNTHETICBONDNT" in r.description
        assert "DUE12/31/99" in r.description  # continuation merged in
        assert r.quantity == -100000.0
        assert r.realized_gain_loss == pytest.approx(100.00)


class TestPurchaseRows:
    def test_simple_purchase(self):
        text = _wrap(
            "02/05 Purchase SYN1 SYNTHETICONEFUND 100.0000 100.0000 (10,000.00)\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.category == "Purchase"
        assert r.symbol == "SYN1"
        assert r.quantity == 100.0
        assert r.price == 100.0
        assert r.charges is None  # no charge column in this row
        assert r.amount == pytest.approx(-10000.0)
        assert r.realized_gain_loss is None

    def test_purchase_inherits_date_from_previous_row(self):
        # Schwab condenses multi-row dates: rows after the first
        # for a date start with the category, not the date.
        text = _wrap(
            "02/05 Purchase SYN1 SYNTHETICONEFUND 100.0000 100.0000 (10,000.00)\n"
            "Purchase SYN2 SYNTHETICTWOFUND 500.0000 50.0000 (25,000.00)\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 2
        assert rows[0].date == date(2026, 2, 5)
        assert rows[1].date == date(2026, 2, 5)  # inherited
        assert rows[1].symbol == "SYN2"


class TestWithdrawalsAndDeposits:
    def test_withdrawal_with_money_link(self):
        text = _wrap(
            "02/02 Withdrawal MoneyLinkTxn TfrSYNTHETIC-BANKNA,N/A (10,000.00)\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.category == "Withdrawal"
        assert r.action == "MoneyLinkTxn"
        assert r.symbol is None
        assert r.amount == -10000.0
        assert r.quantity is None
        assert r.price is None

    def test_deposit_wired_funds(self):
        text = _wrap(
            "02/04 Deposit FundsReceived WIREDFUNDSRECEIVED 1,000,000.00\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.category == "Deposit"
        assert r.action == "FundsReceived"
        assert r.amount == 1000000.0


class TestDividendsAndInterest:
    def test_cash_dividend(self):
        text = _wrap(
            "02/05 Dividend CashDividend SYN3 SYNTHETICTHREEFUND 300.00\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.category == "Dividend"
        assert r.action == "CashDividend"
        assert r.symbol == "SYN3"
        assert r.amount == 300.00

    def test_nra_tax_withholding(self):
        text = _wrap(
            "02/05 Dividend NRATax SYN3 SYNTHETICTHREEFUND (45.00)\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.category == "Dividend"
        assert r.action == "NRATax"
        assert r.amount == -45.00

    def test_credit_interest(self):
        text = _wrap(
            "02/27 Interest CreditInterest SCHWAB1INT01/01-01/31 1.00\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.category == "Interest"
        assert r.action == "CreditInterest"
        assert r.amount == 1.00


class TestSectionBoundaries:
    def test_ignores_text_before_section(self):
        text = (
            PERIOD_HEADER
            + "Some preamble about positions etc.\n"
            + "02/01 Purchase XXXX FAKE 1.0 1.0 (1.00)\n"  # outside section, must NOT be parsed
            + "Transaction Details\n"
            + "Symbol/ Price/Rate\n"
            + "02/02 Sale SYN1 SYNTHETICONE (100.0000) 100.0000 0.01 10,000.00 50.00,(ST)\n"
            + "TotalTransactions ($1) $2\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        assert rows[0].symbol == "SYN1"

    def test_ignores_text_after_terminator(self):
        text = _wrap(
            "02/02 Sale SYN1 SYNTHETICONE (100.0000) 100.0000 0.01 10,000.00 50.00,(ST)\n"
        ) + "02/28 Purchase XXXX SHOULDNOTAPPEAR 1.0 1.0 (1.00)\n"
        rows = pp.parse_transactions(text)
        assert len(rows) == 1

    def test_pending_open_activity_terminates(self):
        text = (
            PERIOD_HEADER
            + "Transaction Details\n"
            + "Symbol/ Price/Rate\n"
            + "02/02 Sale SYN1 SYNTHETICONE (100.0000) 100.0000 0.01 10,000.00 50.00,(ST)\n"
            + "Pending / Open Activity\n"
            + "Pending 02/27 Purchase SYN9 SYNTHETICULTRA 5.0 2.00 (10.00)\n"  # after terminator
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        assert rows[0].symbol == "SYN1"

    def test_total_transactions_spaced_form_terminates(self):
        # pypdfium2 emits "Total Transactions" with a space
        # (pdfplumber's tight output gave us "TotalTransactions"
        # — the original anchor). Both must terminate the
        # section; without the spaced-form support, a statement
        # with no Pending block would leave the section open and
        # sweep the trailing Endnotes / disclosure paragraphs
        # (which carry the custodian name + account number) into
        # the last row's description.
        text = (
            PERIOD_HEADER
            + "Transaction Details\n"
            + "Symbol/ Price/Rate\n"
            + "02/02 Sale SYN1 SYNTHETICONE (100.0000) 100.0000 0.01 10,000.00 50.00,(ST)\n"
            + "Total Transactions $50.00 $50.00\n"
            + "Endnotes For Your Account\n"
            + "Interest: For the Schwab One Interest, Bank Sweep ...\n"
            + "paid for a period that may differ from the Statement Period.\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        assert rows[0].symbol == "SYN1"
        assert "Interest" not in rows[0].description
        assert "Bank Sweep" not in rows[0].description

    def test_terms_and_conditions_terminates(self):
        # Belt-and-suspenders: even without Total Transactions
        # or Endnotes, "Terms and Conditions" is a clean break.
        text = (
            PERIOD_HEADER
            + "Transaction Details\n"
            + "Symbol/ Price/Rate\n"
            + "02/02 Sale SYN1 SYNTHETICONE (100.0000) 100.0000 0.01 10,000.00 50.00,(ST)\n"
            + "Terms and Conditions\n"
            + "Interest: For the Schwab One Interest, Bank Sweep ...\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        assert "Bank Sweep" not in rows[0].description


class TestNumberParsing:
    @pytest.mark.parametrize("s,expected", [
        ("1.00", 1.0),
        ("-1.00", -1.0),
        ("(1.00)", -1.0),
        ("1,234.56", 1234.56),
        ("(1,234.56)", -1234.56),
        ("0.00", 0.0),
        ("100,000.0000", 100000.0),
        ("", None),
        ("notanumber", None),
        (None, None),
    ])
    def test_parse_number(self, s, expected):
        assert pp._parse_number(s) == expected


class TestRawLinesPreserved:
    def test_raw_lines_contains_source(self):
        text = _wrap(
            "02/02 Sale SYN1 SYNTHETICONE (100.0000) 100.0000 0.01 10,000.00 50.00,(ST)\n"
            "IndustryFee$0.01\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        # raw_lines preserves the source for debugging / diff
        # against future parser changes.
        assert len(r.raw_lines) >= 1
        assert any("Sale" in ln for ln in r.raw_lines)


class TestToDict:
    def test_to_dict_serializes_date(self):
        text = _wrap(
            "02/02 Sale SYN1 SYNTHETICONE (100.0000) 100.0000 0.01 10,000.00 50.00,(ST)\n"
        )
        rows = pp.parse_transactions(text)
        d = rows[0].to_dict()
        assert d["date"] == "2026-02-02"  # ISO string, not date object
        assert d["category"] == "Sale"
        assert "raw_lines" in d  # raw_lines kept for diff / debug

    def test_to_dict_handles_none_fields(self):
        text = _wrap(
            "02/04 Deposit FundsReceived WIREDFUNDSRECEIVED 1,000,000.00\n"
        )
        rows = pp.parse_transactions(text)
        d = rows[0].to_dict()
        assert d["quantity"] is None
        assert d["realized_gain_loss"] is None
        assert d["term"] is None


class TestStatementYearOverride:
    def test_explicit_year_overrides_period_header(self):
        text = _wrap(
            "02/02 Sale SYN1 SYNTHETICONE (100.0000) 100.0000 0.01 10,000.00 50.00,(ST)\n"
        )
        rows = pp.parse_transactions(text, statement_year=2099)
        assert rows[0].date == date(2099, 2, 2)

    def test_missing_period_raises_without_override(self):
        text = (
            "Transaction Details\n"
            "Symbol/ Price/Rate\n"
            "02/02 Sale SYN1 SYNTHETICONE (100.0000) 100.0000 0.01 10,000.00 50.00,(ST)\n"
            "TotalTransactions ($1) $2\n"
        )
        with pytest.raises(ValueError, match="statement period"):
            pp.parse_transactions(text)


# ============================================================
# parse_positions
# ============================================================
#
# Synthetic position-block wrappers. Tickers SYN1..SYNn are
# made-up placeholders; no real Schwab account data appears here.

def _wrap_equities_block(rows_text: str) -> str:
    return (
        "Positions - Equities\n"
        "Unrealized Est. Est.Annual %of\n"
        "Symbol Description Quantity Price($) Market Value($) "
        "CostBasis($) Gain/(Loss)($) Yield Income($) Acct\n"
        + rows_text
        + "TotalEquities $0.00 $0.00 $0.00 N/A 0%\n"
    )


class TestParsePositions:
    def test_full_row_with_pct_int_suffix(self):
        # Trailing "1%" (integer-percent, no decimal) must not break
        # the trailing-column scan.
        text = _wrap_equities_block(
            "SYN1 SyntheticOneInc(M) 100.0000 50.00000 5,000.00 4,000.00 1,000.00 N/A N/A 1%\n"
        )
        rows = pp.parse_positions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r["instrument_key"] == "SYN1"
        assert r["section"] == "Equities"
        assert r["quantity"] == 100.0
        assert r["market_price"] == 50.0
        assert r["market_value"] == 5000.0
        assert r["cost_basis"] == 4000.0
        assert r["unrealized_gain_loss"] == 1000.0
        assert r["pct_of_acct"] == "1%"
        assert r["est_yield"] == "N/A"  # raw string preserved

    def test_negative_unrealized_in_parens(self):
        text = _wrap_equities_block(
            "SYN2 SyntheticTwoCorp(M) 400.0000 10.00000 4,000.00 9,000.00 (5,000.00) N/A N/A <1%\n"
        )
        rows = pp.parse_positions(text)
        r = rows[0]
        assert r["unrealized_gain_loss"] == -5000.0
        assert r["pct_of_acct"] == "<1%"

    def test_two_consecutive_rows_dont_merge(self):
        text = _wrap_equities_block(
            "SYN1 SyntheticOne(M) 100.0000 50.00000 5,000.00 4,000.00 1,000.00 N/A N/A 1%\n"
            "SYN2 SyntheticTwo(M) 200.0000 25.00000 5,000.00 3,000.00 2,000.00 N/A N/A 1%\n"
        )
        rows = pp.parse_positions(text)
        assert [r["instrument_key"] for r in rows] == ["SYN1", "SYN2"]
        assert rows[0]["quantity"] == 100.0
        assert rows[1]["quantity"] == 200.0

    def test_description_continuation_attaches(self):
        # Multi-line description (Schwab does this for ADRs etc.).
        text = _wrap_equities_block(
            "SYN3 SyntheticThreeAg F 5,000.0000 1.00000 5,000.00 4,000.00 1,000.00 N/A N/A <1%\n"
            "SPONSOREDADR\n"
            "1ADRREPS 0.2 ORDSHS\n"
        )
        rows = pp.parse_positions(text)
        assert len(rows) == 1
        assert "SPONSOREDADR" in rows[0]["description"]
        assert "1ADRREPS" in rows[0]["description"]

    def test_etf_section_label_preserved(self):
        text = (
            "Positions - Exchange Traded Funds\n"
            "Symbol Description Quantity Price($) Market Value($) "
            "CostBasis($) Gain/(Loss)($) Yield Income($) Acct\n"
            "SYN4 SyntheticETF(M), 1,000.0000 100.00000 100,000.00 95,000.00 5,000.00 1.00% 1,000.00 50%\n"
            "TotalExchangeTradedFunds $100,000.00 $95,000.00 $5,000.00 $1,000.00 50%\n"
        )
        rows = pp.parse_positions(text)
        assert len(rows) == 1
        assert rows[0]["section"] == "Exchange Traded Funds"
        assert rows[0]["est_yield"] == "1.00%"
        assert rows[0]["est_annual_income"] == 1000.0

    def test_summary_section_is_excluded(self):
        # The one-line "Positions - Summary" roll-up should NOT
        # yield any per-instrument rows.
        text = (
            "Positions - Summary\n"
            "BeginningValue Transfer Reinvested Activity EndingValue\n"
            "$1,000,000.00 $0.00 $0.00 $0.00 $1,000,000.00\n"
            "Cash and Cash Investments\n"
            "Cash 100.00 100.00 0.00 0.00 <1%\n"
        )
        rows = pp.parse_positions(text)
        assert rows == []

    def test_cash_section_is_excluded(self):
        # "Cash and Cash Investments" is its own section; its rows
        # don't go into position_snapshots. The parser should
        # only emit rows for sections matching "Positions - X"
        # AND not "Summary".
        text = (
            "Cash and Cash Investments\n"
            "Type Symbol Description Quantity Price Beginning Ending\n"
            "Cash 1,000.00 800.00 (200.00) 0.00 <1%\n"
            "TotalCashandCashInvestments $1,000.00 $800.00 ($200.00) <1%\n"
        )
        rows = pp.parse_positions(text)
        assert rows == []

    def test_missing_cost_basis_stays_none(self):
        # Edge: cost_basis column is blank — should be None, NOT
        # coerced to 0.0.
        text = _wrap_equities_block(
            "SYN5 SyntheticFive(M) 100.0000 50.00000 5,000.00\n"
        )
        rows = pp.parse_positions(text)
        # The 3-column row should give us quantity / price /
        # market_value with cost_basis None.
        assert len(rows) == 1
        r = rows[0]
        assert r["quantity"] == 100.0
        assert r["market_price"] == 50.0
        assert r["market_value"] == 5000.0
        assert r["cost_basis"] is None
        assert r["unrealized_gain_loss"] is None

    def test_empty_input_returns_empty_list(self):
        assert pp.parse_positions("") == []

    def test_no_positions_section_returns_empty(self):
        text = "Transaction Details\nsomething\nTotalTransactions $1 $2\n"
        assert pp.parse_positions(text) == []


# ============================================================
# parse_cash_summary
# ============================================================

class TestParseCashSummary:
    DATA_LINE = (
        "$1,000.00 $500.00 ($800.00) $0.00 $400.00 "
        "$10.00 $0.00 $1,110.00\n"
    )

    def _wrap_cash_summary(self, data_line: str | None = None) -> str:
        return (
            "Transactions - Summary\n"
            "BeginningCash*asof01/01 + Deposits + Withdrawals + Purchases "
            "+ Sales/Redemptions + Dividends/Interest + Expenses = "
            "EndingCash*asof01/31\n"
            + (data_line or self.DATA_LINE)
            + "OtherActivity $0.00 Other activity includes...\n"
            + "Transaction Details\n"
        )

    def test_extracts_all_eight_columns(self):
        cash = pp.parse_cash_summary(self._wrap_cash_summary())
        assert cash is not None
        assert cash["opening_balance"] == 1000.00
        assert cash["closing_balance"] == 1110.00
        assert cash["deposits"] == 500.00
        assert cash["withdrawals"] == -800.00
        assert cash["purchases"] == 0.0
        assert cash["sales_redemptions"] == 400.00
        assert cash["dividends_interest"] == 10.00
        assert cash["expenses"] == 0.0
        assert cash["currency_iso"] == "USD"

    def test_total_credits_and_debits_derived(self):
        cash = pp.parse_cash_summary(self._wrap_cash_summary())
        # credits = deposits + sales_redemptions + dividends_interest
        assert cash["total_credits"] == pytest.approx(500 + 400 + 10)
        # debits = abs(withdrawals + purchases + expenses)
        assert cash["total_debits"] == pytest.approx(800.00)

    def test_other_activity_captured(self):
        cash = pp.parse_cash_summary(self._wrap_cash_summary())
        assert cash["other_activity"] == 0.0

    def test_raw_line_preserved(self):
        cash = pp.parse_cash_summary(self._wrap_cash_summary())
        assert "$1,000.00" in cash["raw_line"]
        assert "$1,110.00" in cash["raw_line"]

    def test_no_section_returns_none(self):
        text = "Account Summary\nSomething else\n"
        assert pp.parse_cash_summary(text) is None

    def test_data_line_with_seven_amounts_still_parses(self):
        # Edge: if Schwab ever ships a missing column, we should
        # still capture what's there. Seven $-amounts (no
        # Expenses column).
        seven = "$100.00 $50.00 ($25.00) $0.00 $30.00 $5.00 $160.00\n"
        cash = pp.parse_cash_summary(self._wrap_cash_summary(seven))
        assert cash is not None
        assert cash["opening_balance"] == 100.00
        # The 8th slot is missing; closing_balance ends up None
        # rather than being silently assigned a wrong column.
        assert cash["closing_balance"] is None

    def test_nulls_preserved_when_input_missing(self):
        # If a column was blank in the row (e.g. parser gives us
        # only six numbers), the derived totals should be None
        # rather than 0.0.
        six_line = "$100.00 $50.00 ($25.00) $0.00 $30.00 $5.00\n"
        text = (
            "Transactions - Summary\n"
            + "BeginningCash + Deposits ...\n"
            + six_line
            + "Transaction Details\n"
        )
        cash = pp.parse_cash_summary(text)
        # Below the 7-amount threshold — parser returns None.
        assert cash is None


class TestParsePositionsFootnoteMarker:
    """An Endnote reference letter printed inline among a holding's
    numeric columns (e.g. 'e' = "edited or provided by the account
    holder" on an SPV / alternative interest, 't' = "edited by a third
    party") must not shift the column mapping. Without the fix the
    scan stopped at the marker and the columns slid left — quantity
    read the unrealized gain, market value read blank. Wholly synthetic
    fixtures (invented tickers, round made-up dollar values)."""

    def test_inline_marker_single_row_does_not_shift_columns(self):
        # 'e' sits between Cost Basis and Unrealized Gain — the exact
        # shape that mis-parsed (qty=unrealized, mv=blank) before the fix.
        text = _wrap_equities_block(
            "SYN1 SyntheticSpvInc(M) 300.0000 50.00000 15,000.00 "
            "12,000.00 e 3,000.00 N/A 0.00 2%\n"
        )
        rows = pp.parse_positions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r["instrument_key"] == "SYN1"
        assert r["quantity"] == 300.0           # NOT the unrealized gain
        assert r["market_price"] == 50.0
        assert r["market_value"] == 15000.0     # NOT blank/0
        assert r["cost_basis"] == 12000.0
        assert r["unrealized_gain_loss"] == 3000.0
        assert r["pct_of_acct"] == "2%"
        assert r["footnotes"] == ["e"]          # marker preserved

    def test_inline_marker_multiline_block(self):
        # Multi-line block: ticker + name on one line, "(M)" on the
        # next, the numbers (with the 'e' marker) on a third.
        text = (
            "Positions - Equities\n"
            "Unrealized Est. Est.Annual %of\n"
            "Symbol Description Quantity Price($) Market Value($) "
            "CostBasis($) Gain/(Loss)($) Yield Income($) Acct\n"
            "SYN1 SyntheticSpvInc\n"
            "(M)\n"
            "300.0000 50.00000 15,000.00 12,000.00 e 3,000.00 N/A 0.00 2%\n"
            "TotalEquities $0.00 $0.00 $0.00 N/A 0%\n"
        )
        rows = pp.parse_positions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r["instrument_key"] == "SYN1"
        assert r["quantity"] == 300.0
        assert r["market_value"] == 15000.0
        assert r["cost_basis"] == 12000.0
        assert r["footnotes"] == ["e"]

    def test_third_party_marker_t(self):
        text = _wrap_equities_block(
            "SYN2 SyntheticTrust 100.0000 40.00000 4,000.00 3,500.00 "
            "t 500.00 N/A 0.00 <1%\n"
        )
        r = pp.parse_positions(text)[0]
        assert r["quantity"] == 100.0
        assert r["market_value"] == 4000.0
        assert r["footnotes"] == ["t"]

    def test_description_ending_in_lone_letter_not_eaten(self):
        # A description that ends in a single letter ("CLASS A") must
        # NOT be mistaken for a footnote marker — the lone 'A' has a
        # non-column token to its left, so the scan keeps it in the
        # description.
        text = _wrap_equities_block(
            "SYN3 Synthetic Holding Co CLASS A 100.0000 50.00000 "
            "5,000.00 4,000.00 1,000.00 N/A N/A 1%\n"
        )
        r = pp.parse_positions(text)[0]
        assert r["quantity"] == 100.0
        assert r["market_value"] == 5000.0
        assert "CLASS A" in r["description"]
        assert r["footnotes"] is None

    def test_unmarked_row_has_null_footnotes(self):
        text = _wrap_equities_block(
            "SYN4 SyntheticPlain(M) 100.0000 50.00000 5,000.00 "
            "4,000.00 1,000.00 N/A N/A 1%\n"
        )
        assert pp.parse_positions(text)[0]["footnotes"] is None

    def test_marker_glued_to_parenthesised_negative(self):
        # pypdfium2 glues the marker to a parenthesised-negative column,
        # e.g. "t(1,000.00)" — the unrealized loss on a third-party-
        # edited holding. Must split into the column + the marker.
        text = _wrap_equities_block(
            "SYN5 SyntheticTrust 200.0000 30.00000 6,000.00 7,000.00 "
            "t(1,000.00) N/A 5.00 3%\n"
        )
        r = pp.parse_positions(text)[0]
        assert r["quantity"] == 200.0
        assert r["market_value"] == 6000.0
        assert r["cost_basis"] == 7000.0
        assert r["unrealized_gain_loss"] == -1000.0
        assert r["footnotes"] == ["t"]


class TestSponsoredAdrSegmentation:
    """An ADR's "SPONSORED ADR" description-continuation line must not
    be mistaken for a new position-row header — "SPONSORED" is nine
    uppercase chars and so matches the ticker shape. Before the fix it
    split the ADR's block and stole the real ticker's numbers (dropping
    the ADR, emitting a spurious "SPONSORED" holding). Synthetic data."""

    def test_sponsored_adr_does_not_steal_numbers(self):
        text = (
            "Positions - Equities\n"
            "Unrealized Est. Est.Annual %of\n"
            "Symbol Description Quantity Price($) Market Value($) "
            "CostBasis($) Gain/(Loss)($) Yield Income($) Acct\n"
            "SYN1 Synthetic Holdings Ltd F\n"
            "SPONSORED ADR\n"
            "1 ADR REPS 1 ORD SHS\n"
            "20.0000 75.00000 1,500.00 1,200.00 300.00 N/A 0.00 <1%\n"
            "TotalEquities $0.00 $0.00 $0.00 N/A 0%\n"
        )
        rows = pp.parse_positions(text)
        keys = [r["instrument_key"] for r in rows]
        assert "SPONSORED" not in keys
        assert keys == ["SYN1"]
        assert rows[0]["quantity"] == 20.0
        assert rows[0]["market_value"] == 1500.0
        assert "SPONSORED ADR" in rows[0]["description"]


class TestParsePositionsWrappedColumns:
    """Options and Fixed Income holdings wrap their columns over
    several lines (2025+ layout, pypdfium2). Wholly synthetic fixtures
    in the statement's shape: invented tickers and CUSIPs, round
    made-up values."""

    OPTIONS_HEAD = (
        "Positions - Options\n"
        "Symbol Description Quantity Price($) Market Value($) Cost Basis($)\n"
        "Unrealized\n"
        "Gain/(Loss)($) Est. Yield\n"
        "Est. Annual\n"
        "Income($)\n"
        "% of\n"
        "Acct\n"
    )
    LONG_CALL = (
        "XMPL\n"
        "01/16/20\n"
        "26 50.00\n"
        "C\n"
        "CALL EXAMPLE CORP\n"
        ",\n"
        "$50 EXP 01/16/26\n"
        "2.0000 3.00000 600.00 500.00 100.00 <1%\n"
    )
    SHORT_CALL = (
        "SYNX\n"
        "06/18/20\n"
        "27\n"
        "120.00 C\n"
        "CALL SYNTHETIC INDUSTRIES\n"
        ",\n"
        "$120 EXP 06/18/27\n"
        "(1.0000)\n"
        "S\n"
        "4.00000 (400.00) (600.00) 200.00\n"
    )
    TRAILER = (
        "Option Customers: Be aware of the following: 1) Commissions\n"
        "Transactions - Summary\n"
        "Transaction Details\n"
        "07/01 Sale XMPL EXAMPLE CORP (10.0000) 25.0000 0.01 249.99 (10.00),\n"
        "TotalTransactions $0.00\n"
    )

    def test_long_and_short_calls(self):
        text = (self.OPTIONS_HEAD + self.LONG_CALL + self.SHORT_CALL
                + "Total Options $200.00 ($100.00) $300.00 $0.00 <1%\n")
        rows = pp.parse_positions(text)
        by_key = {r["instrument_key"]: r for r in rows}
        assert sorted(by_key) == ["SYNX 06/18/2027 120.00 C",
                                  "XMPL 01/16/2026 50.00 C"]
        long = by_key["XMPL 01/16/2026 50.00 C"]
        assert (long["quantity"], long["market_price"], long["market_value"],
                long["cost_basis"], long["unrealized_gain_loss"]) == (
            2.0, 3.0, 600.0, 500.0, 100.0)
        assert long["pct_of_acct"] == "<1%"
        assert long["est_yield"] is None
        assert long["description"] == "CALL EXAMPLE CORP $50 EXP 01/16/26"
        short = by_key["SYNX 06/18/2027 120.00 C"]
        assert (short["quantity"], short["market_price"], short["market_value"],
                short["cost_basis"], short["unrealized_gain_loss"]) == (
            -1.0, 4.0, -400.0, -600.0, 200.0)
        assert short["footnotes"] == ["S"]
        assert short["pct_of_acct"] is None

    def test_negative_options_total_closes_the_section(self):
        # A short option makes the section total negative. The footer
        # must still close the section, or the transaction lines after
        # it parse as holdings.
        text = (self.OPTIONS_HEAD + self.SHORT_CALL
                + "Total Options ($400.00) ($600.00) $200.00 $0.00A\n"
                + self.TRAILER)
        rows = pp.parse_positions(text)
        assert [r["instrument_key"] for r in rows] == ["SYNX 06/18/2027 120.00 C"]

    def test_fixed_income_columns_after_the_numbers_line(self):
        text = (
            "Positions - Fixed Income\n"
            "Symbol/\n"
            "CUSIP Description Coupon\n"
            "Maturity\n"
            "Date Quantity/Par Price($)\n"
            "Accrued\n"
            "Interest$)\n"
            "% of\n"
            "Acct\n"
            "000000AA0 US TREASURY NT\n"
            "(M)\n"
            "2.000% 05/15/30 10,000.0000 99.00000 9,900.00 9,800.00\n"
            "9,750.00\n"
            "100.00 2.20% 200.00 50.00 1%\n"
            "000000BB0 US TREASURY BD\n"
            "(M)\n"
            "1.50% 02/15/40 5,000.0000 80.00000 4,000.00 4,500.00\n"
            "4,600.00\n"
            "(500.00) 3.10% 75.00 10.00 <1%\n"
            "Total Fixed Income 15,000.0000 $13,900.00 $14,300.00 ($400.00) $60.00 1%\n"
            "Total Adj Cost Basis $14,300.00\n"
        )
        rows = pp.parse_positions(text)
        by_key = {r["instrument_key"]: r for r in rows}
        assert sorted(by_key) == ["000000AA0", "000000BB0"]
        a = by_key["000000AA0"]
        assert (a["quantity"], a["market_price"], a["market_value"],
                a["cost_basis"], a["unrealized_gain_loss"]) == (
            10000.0, 99.0, 9900.0, 9800.0, 100.0)
        assert a["est_yield"] == "2.20%"
        assert a["est_annual_income"] == 200.0
        assert a["accrued_interest"] == 50.0
        assert a["pct_of_acct"] == "1%"
        assert a["description"] == "US TREASURY NT 2.000% 05/15/30"
        b = by_key["000000BB0"]
        assert b["cost_basis"] == 4500.0
        assert b["unrealized_gain_loss"] == -500.0
        assert b["accrued_interest"] == 10.0

    def test_fixed_income_without_pct_column(self):
        text = (
            "Positions - Fixed Income\n"
            "000000AA0 US TREASURY NT\n"
            "(M)\n"
            "2.000% 05/15/30 10,000.0000 99.00000 9,900.00 9,800.00\n"
            "9,750.00\n"
            "100.00 2.20% 200.00 50.00\n"
            "Total Fixed Income 10,000.0000 $9,900.00 $9,800.00 $100.00 $50.00\n"
        )
        (row,) = pp.parse_positions(text)
        assert row["unrealized_gain_loss"] == 100.0
        assert row["accrued_interest"] == 50.0
        assert row["pct_of_acct"] is None


# ============================================================
# parse_statement_pdf — return-dict shape
# ============================================================

class TestParsePositionsLegacy:
    """Tests for the 2020-2024 'Investment Detail - <Section>'
    statement layout. Synthetic-text fixtures only — no real
    Schwab data."""

    def _wrap(self, body: str) -> str:
        return (
            "Investment Detail - Equities\n"
            "Quantity Market Price Market Value\n"
            "% of\n"
            "Account\n"
            "Assets\n"
            "Unrealized\n"
            "Gain or (Loss)\n"
            "Estimated\n"
            "Yield\n"
            "Estimated\n"
            "Annual Income\n"
            "Equities Units Purchased Cost Per Share Cost Basis Acquired\n"
            + body
            + "Total Investment Detail $999,999.99\n"
        )

    def test_main_row_with_symbol_and_cost_basis(self):
        text = self._wrap(
            "ALPHACORP INC (M) 100.0000 50.00000 5,000.00 2% 1,000.00 N/A N/A\n"
            "CLASS A 50.0000 35.0000 1,750.00 01/15/22 875.00 800 Long-Term\n"
            "SYMBOL: ALPH 50.0000 45.0000 2,250.00 02/20/22 125.00 760 Long-Term\n"
            "Cost Basis 4,000.00\n"
        )
        rows = pp.parse_positions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r["instrument_key"] == "ALPH"          # from SYMBOL: line
        assert r["quantity"] == 100.0
        assert r["market_price"] == 50.0
        assert r["market_value"] == 5000.0
        assert r["pct_of_acct"] == "2%"
        assert r["unrealized_gain_loss"] == 1000.0
        assert r["cost_basis"] == 4000.0              # from Cost Basis line
        assert r["est_yield"] == "N/A"
        assert r["est_annual_income"] is None         # N/A → None

    def test_accrued_dividend_captured(self):
        text = self._wrap(
            "BETACORP HLDG (M) 150.0000 72.00000 10,800.00 1% (2,700.00) N/A N/A\n"
            "SYMBOL: BETA\n"
            "Cost Basis 13,500.00 Accrued Dividend: 250.00\n"
        )
        rows = pp.parse_positions(text)
        assert len(rows) == 1
        assert rows[0]["cost_basis"] == 13500.0
        assert rows[0]["accrued_interest"] == 250.0   # dividend → accrued_interest

    def test_negative_unrealized_in_parens(self):
        text = self._wrap(
            "GAMMA CORP 25.0000 6.00000 150.00 <1% (5.00) N/A N/A\n"
            "SYMBOL: GAMM\n"
        )
        rows = pp.parse_positions(text)
        assert rows[0]["unrealized_gain_loss"] == -5.0
        assert rows[0]["pct_of_acct"] == "<1%"

    def test_multiple_positions_dont_merge(self):
        text = self._wrap(
            "ALPHACORP INC (M) 100.0000 50.00000 5,000.00 2% 1,000.00 N/A N/A\n"
            "SYMBOL: ALPH\n"
            "Cost Basis 4,000.00\n"
            "BETACORP HLDG 50.0000 100.00000 5,000.00 2% 500.00 0.50% 25.00\n"
            "SYMBOL: BETA\n"
            "Cost Basis 4,500.00\n"
        )
        rows = pp.parse_positions(text)
        assert [r["instrument_key"] for r in rows] == ["ALPH", "BETA"]
        assert rows[0]["cost_basis"] == 4000.0
        assert rows[1]["cost_basis"] == 4500.0
        assert rows[1]["est_yield"] == "0.50%"
        assert rows[1]["est_annual_income"] == 25.0

    def test_tax_lot_rows_dont_become_positions(self):
        # Lot rows have a MM/DD/YY date token; the main-row
        # detector requires a leading uppercase token and 7
        # trailing trailing-col tokens. Lot rows fail both.
        text = self._wrap(
            "ALPHACORP INC 100.0000 50.00000 5,000.00 2% 1,000.00 N/A N/A\n"
            "CLASS A 25.0000 120.0000 3,000.00 01/03/22 700.00 783 Long-Term\n"
            "SYMBOL: ALPH 50.0000 45.0000 2,250.00 02/20/22 125.00 760 Long-Term\n"
            "Cost Basis 4,000.00\n"
        )
        rows = pp.parse_positions(text)
        assert len(rows) == 1
        assert rows[0]["instrument_key"] == "ALPH"

    def test_cash_section_excluded(self):
        # "Investment Detail - Cash" / "Bank Sweep" / "Cash and
        # Bank Sweep" are cash positions, not security holdings;
        # the parser must skip them entirely.
        text = (
            "Investment Detail - Cash\n"
            "Cash Starting Balance Ending Balance\n"
            "Cash $1,000.00 $2,000.00\n"
            "Investment Detail - Bank Sweep\n"
            "Bank Sweep $500.00 $750.00\n"
            "Investment Detail - Equities\n"
            "ALPHACORP INC 100.0000 50.00000 5,000.00 2% 1,000.00 N/A N/A\n"
            "SYMBOL: ALPH\n"
            "Cost Basis 4,000.00\n"
            "Total Investment Detail $5,000.00\n"
        )
        rows = pp.parse_positions(text)
        assert [r["instrument_key"] for r in rows] == ["ALPH"]

    def test_continued_section_header_keeps_section(self):
        # "Investment Detail - Equities (continued)" must stay in
        # the same section, not flush + re-open and lose state.
        text = (
            "Investment Detail - Equities\n"
            "ALPHACORP INC 100.0000 50.00000 5,000.00 2% 1,000.00 N/A N/A\n"
            "SYMBOL: ALPH\n"
            "Cost Basis 4,000.00\n"
            "Investment Detail - Equities (continued)\n"
            "BETACORP HLDG 50.0000 100.00000 5,000.00 2% 500.00 N/A N/A\n"
            "SYMBOL: BETA\n"
            "Cost Basis 4,500.00\n"
            "Total Investment Detail $10,000.00\n"
        )
        rows = pp.parse_positions(text)
        assert [r["instrument_key"] for r in rows] == ["ALPH", "BETA"]

    def test_legacy_falls_back_only_when_new_format_absent(self):
        # If a text has the NEW "Positions - Equities" anchor and
        # produces rows, the legacy parser must NOT be invoked.
        # We assert that by including BOTH an old-style block and
        # a new-style block; the new-style rows win.
        text = (
            "Positions - Equities\n"
            "Symbol Description Quantity Price MV CB Gain Yield Income Acct\n"
            "NEW1 NewFormat(M) 10.0000 1.00000 10.00 8.00 2.00 N/A N/A 1%\n"
            "TotalEquities $10.00 $8.00 $2.00 N/A 1%\n"
            "Investment Detail - Equities\n"
            "OLD1 OldFormat 5.0000 2.00000 10.00 1% 1.00 N/A N/A\n"
            "SYMBOL: OLDX\n"
            "Cost Basis 9.00\n"
            "Total Investment Detail $20.00\n"
        )
        rows = pp.parse_positions(text)
        # New-format wins; legacy is not consulted.
        assert [r["instrument_key"] for r in rows] == ["NEW1"]


class TestParseCashSummaryLegacy:
    def test_extracts_all_known_lines(self):
        # All values are synthetic; structure mirrors what Schwab
        # emits but the amounts are made-up.
        text = (
            "Cash Transactions Summary This Period Year to Date\n"
            "Starting Cash* $ 1,000.00 $ 500.00\n"
            "Deposits and other Cash Credits 5,000.00 10,000.00\n"
            "Investments Sold 500.00 1,000.00\n"
            "Dividends and Interest 50.00 100.00\n"
            "Withdrawals and other Debits (2,000.00) (4,000.00)\n"
            "Investments Purchased (3,000.00) (6,000.00)\n"
            "Fees and Charges (10.00) (20.00)\n"
            "Total Cash Transaction Detail 540.00 1,080.00\n"
            "Ending Cash* $ 1,540.00 $ 1,580.00\n"
            "*Cash (includes any cash debit balance) ...\n"
            "Investment Detail - Cash\n"
        )
        cash = pp.parse_cash_summary(text)
        assert cash is not None
        assert cash["opening_balance"] == 1000.0
        assert cash["closing_balance"] == 1540.0
        assert cash["deposits"] == 5000.0
        assert cash["sales_redemptions"] == 500.0
        assert cash["dividends_interest"] == 50.0
        assert cash["withdrawals"] == -2000.0
        assert cash["purchases"] == -3000.0
        assert cash["expenses"] == -10.0
        assert cash["other_activity"] is None         # not in legacy
        assert cash["currency_iso"] == "USD"

    def test_derived_totals(self):
        text = (
            "Cash Transactions Summary\n"
            "Starting Cash $ 100.00 $ 100.00\n"
            "Deposits and other Cash Credits 1,000.00 1,000.00\n"
            "Investments Sold 500.00 500.00\n"
            "Dividends and Interest 25.00 25.00\n"
            "Withdrawals and other Debits (200.00) (200.00)\n"
            "Investments Purchased (300.00) (300.00)\n"
            "Fees and Charges (10.00) (10.00)\n"
            "Ending Cash $ 1,115.00 $ 1,115.00\n"
            "Investment Detail - Equities\n"
        )
        cash = pp.parse_cash_summary(text)
        # credits = 1000 + 500 + 25 = 1525
        assert cash["total_credits"] == pytest.approx(1525.0)
        # debits = abs(-200 + -300 + -10) = 510
        assert cash["total_debits"] == pytest.approx(510.0)

    def test_missing_label_stays_null(self):
        # Schwab statements sometimes omit Fees row entirely.
        text = (
            "Cash Transactions Summary\n"
            "Starting Cash $ 100.00 $ 100.00\n"
            "Deposits and other Cash Credits 1,000.00 1,000.00\n"
            "Investments Sold 500.00 500.00\n"
            "Dividends and Interest 25.00 25.00\n"
            "Withdrawals and other Debits (200.00) (200.00)\n"
            "Investments Purchased (300.00) (300.00)\n"
            "Ending Cash $ 1,125.00 $ 1,125.00\n"
            "Investment Detail - Equities\n"
        )
        cash = pp.parse_cash_summary(text)
        assert cash["expenses"] is None
        # debits chain with a None input → None (NULL preservation)
        assert cash["total_debits"] is None

    def test_no_section_returns_none(self):
        text = "Some other content with no cash block\n"
        assert pp.parse_cash_summary(text) is None

    def test_new_format_takes_precedence(self):
        # Both anchors present → new format wins.
        text = (
            "Transactions - Summary\n"
            "header\n"
            "$1.00 $2.00 ($3.00) $0.00 $5.00 $0.50 $0.00 $5.50\n"
            "Cash Transactions Summary\n"
            "Starting Cash $ 100.00 $ 100.00\n"
            "Ending Cash $ 200.00 $ 200.00\n"
            "Investment Detail - Equities\n"
        )
        cash = pp.parse_cash_summary(text)
        # New format: opening=1.00, closing=5.50
        assert cash["opening_balance"] == 1.0
        assert cash["closing_balance"] == 5.5


class TestParsePositionsLegacyRowShapes:
    """Legacy (2020-2024) rows that print other than the seven Equities
    columns: Options and Mutual Funds layouts, rows that stop after % of
    account, short holdings, and endnote markers among the columns.
    Synthetic-text fixtures only."""

    @staticmethod
    def _section(name: str, body: str) -> str:
        return (
            f"Investment Detail - {name}\n"
            "Quantity Market Price Market Value\n"
            "% of\n"
            "Account\n"
            + body
            + "Total Investment Detail $999,999.99\n"
        )

    def test_long_and_short_options_key_on_their_contract(self):
        text = self._section("Options", (
            "CALL EXAMPLE CORP 2.0000 3.00000 600.00 <1% 100.00\n"
            "$50 EXP 01/16/26 2.0000 2.5000 500.00 12/01/25 100.00\n"
            "SYMBOL: XMPL 01/16/2026 50.00 C\n"
            "PUT SYNTHETIC INDS 1000 1.0000 S4.00000 (400.00) 200.00\n"
            "$120 EXP 06/18/27 1.0000 S 6.0000 (600.00) 01/02/26 200.00\n"
            "SYMBOL: SYNX 06/18/2027 1.0000 S 6.0000 (600.00) 01/02/26 200.00\n"
            "120.00 P\n"
            "Cost Basis (600.00)\n"
            "Total Options 1.0000 200.00 <1% 300.00\n"))
        rows = {r["instrument_key"]: r for r in pp.parse_positions(text)}
        assert sorted(rows) == ["SYNX 06/18/2027 120.00 P", "XMPL 01/16/2026 50.00 C"]
        long = rows["XMPL 01/16/2026 50.00 C"]
        assert (long["quantity"], long["market_price"], long["market_value"],
                long["pct_of_acct"], long["unrealized_gain_loss"]) == (
            2.0, 3.0, 600.0, "<1%", 100.0)
        short = rows["SYNX 06/18/2027 120.00 P"]
        assert (short["quantity"], short["market_price"], short["market_value"],
                short["unrealized_gain_loss"], short["cost_basis"]) == (
            -1.0, 4.0, -400.0, 200.0, -600.0)
        assert short["description"] == "PUT SYNTHETIC INDS 1000"

    def test_mutual_fund_row_prints_cost_basis_in_the_row(self):
        text = self._section("Mutual Funds", (
            "Bond Funds Quantity\n"
            "EXAMPLE BOND FUND (M) 100.0000 10.00000 1,000.00 1,100.00 (100.00) 1%\n"
            "FUND\n"
            "SYMBOL: XBND\n"
            "SYNTHETIC INCOME FUND (M) 50.0000 20.00000 1,000.00 <1%\n"
            "SYMBOL: XMUN\n"
            "Total Bond Funds 150.0000 2,000.00 1,100.00 (100.00) 2%\n"))
        rows = {r["instrument_key"]: r for r in pp.parse_positions(text)}
        assert (rows["XBND"]["market_value"], rows["XBND"]["cost_basis"],
                rows["XBND"]["unrealized_gain_loss"], rows["XBND"]["pct_of_acct"]) == (
            1000.0, 1100.0, -100.0, "1%")
        # A fund with no cost stops after % of account.
        assert (rows["XMUN"]["quantity"], rows["XMUN"]["market_value"],
                rows["XMUN"]["cost_basis"], rows["XMUN"]["pct_of_acct"]) == (
            50.0, 1000.0, None, "<1%")

    def test_equity_rows_that_the_seven_column_rule_missed(self):
        text = self._section("Equities", (
            "EXAMPLE.COM INC (M) 10.0000 100.00000 1,000.00 1% 200.00 N/A N/A\n"
            "SYMBOL: XCOM\n"
            "SYNTHETIC TRUST XTR 300.0000 10.00000 3,000.00 2%\n"
            "SYMBOL: XTRU\n"
            "EXAMPLE 8 CORP (M) 40.0000 5.00000 200.00 <1% N/A iN/A N/A\n"
            "SYMBOL: XEIG\n"
            "SYNTHETIC SHORT CO (M) 100.0000 S 2.50000 (250.00) (10.00) N/A N/A\n"
            "SYMBOL: XSHO\n"))
        rows = {r["instrument_key"]: r for r in pp.parse_positions(text)}
        assert sorted(rows) == ["XCOM", "XEIG", "XSHO", "XTRU"]
        assert rows["XCOM"]["market_value"] == 1000.0      # punctuated name
        assert (rows["XTRU"]["market_value"], rows["XTRU"]["unrealized_gain_loss"]) == (
            3000.0, None)                                  # stops after % of account
        assert rows["XEIG"]["market_value"] == 200.0       # glued endnote on N/A
        assert (rows["XSHO"]["quantity"], rows["XSHO"]["market_value"],
                rows["XSHO"]["unrealized_gain_loss"]) == (-100.0, -250.0, -10.0)


class TestParseAccountValue:
    def test_legacy_total(self):
        assert pp.parse_account_value(
            "Total Assets Long $ 1,250.00\nTotal Account Value $ 1,000.00 100%\n") == 1000.0

    def test_2025_ending_value_is_this_periods(self):
        assert pp.parse_account_value(
            "Beginning Account Value $900.00 $800.00\n"
            "Ending Account Value $1,000.00 $1,000.00\n") == 1000.0

    def test_absent(self):
        assert pp.parse_account_value("no summary here") is None


class TestParseCashSummaryEarliest:
    def test_deposit_accounts_print_the_ending_balance_alone(self):
        text = (
            "Investment Detail\n"
            "Description Symbol Quantity Price Market Value\n"
            "Cash, Money Market, and Deposit Accounts\n"
            "DEPOSIT ACCOUNTS X,Z 5,000.00\n"
            "Investments\n"
            "ALPHACORP INC ALPH 100.0000 50.00000 5,000.00\n"
            "Total Account Value 10,000.00\n"
        )
        cash = pp.parse_cash_summary(text)
        assert cash["closing_balance"] == 5000.0
        assert cash["opening_balance"] is None


class TestParsePositionsVeryOld:
    """2017-2019 layout: bare 'Investment Detail' header,
    'Investments' sub-header, rows of the shape
    NAME [DESCRIPTORS] TICKER QUANTITY PRICE MARKET_VALUE."""

    def _wrap(self, body: str) -> str:
        return (
            "Investment Detail\n"
            "Description Starting Balance Ending Balance\n"
            "Cash and Bank Sweep\n"
            "BANK SWEEP X,Z 100.00 200.00\n"
            "Description Symbol Quantity Price Market Value\n"
            "Investments\n"
            + body
            + "Total Account Value 1,000.00\n"
        )

    def test_basic_position_row(self):
        text = self._wrap("ALPHACORP INC ALPH 100.0000 50.00000 5,000.00\n")
        rows = pp.parse_positions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r["instrument_key"] == "ALPH"
        assert r["quantity"] == 100.0
        assert r["market_price"] == 50.0
        assert r["market_value"] == 5000.0
        # Pre-2020 columns the source doesn't carry:
        assert r["cost_basis"] is None
        assert r["unrealized_gain_loss"] is None
        assert r["accrued_interest"] is None
        assert r["section"] == "Investments"

    def test_description_continuation_attaches(self):
        text = self._wrap(
            "ALPHACORP INC ALPH 100.0000 50.00000 5,000.00\n"
            "CLASS A\n"
            "BETACORP HLDG BETA 50.0000 100.00000 5,000.00\n"
        )
        rows = pp.parse_positions(text)
        assert [r["instrument_key"] for r in rows] == ["ALPH", "BETA"]
        assert "CLASS A" in rows[0]["description"]

    def test_cash_row_is_excluded(self):
        # The "BANK SWEEP" row appears under "Cash and Bank Sweep",
        # NOT under "Investments". The position parser must not
        # see it as a position.
        text = self._wrap("ALPHACORP INC ALPH 100.0000 50.0 5,000.00\n")
        rows = pp.parse_positions(text)
        assert [r["instrument_key"] for r in rows] == ["ALPH"]
        # BANK is not a security ticker in this output.
        assert all(r["instrument_key"] != "BANK" for r in rows)


class TestParseCashSummaryVeryOld:
    def test_extracts_opening_and_closing(self):
        text = (
            "Investment Detail\n"
            "Description Starting Balance Ending Balance\n"
            "Cash and Bank Sweep\n"
            "BANK SWEEP X,Z 1,000.00 2,000.00\n"
            "CASH 50.00 100.00\n"
            "Description Symbol Quantity Price Market Value\n"
            "Investments\n"
            "ALPHACORP INC ALPH 10.0000 1.00 10.00\n"
            "Total Account Value 2,110.00\n"
        )
        cash = pp.parse_cash_summary(text)
        assert cash is not None
        # Multiple cash rows sum.
        assert cash["opening_balance"] == 1050.0
        assert cash["closing_balance"] == 2100.0
        # Pre-2020 statements don't itemise flows.
        assert cash["deposits"] is None
        assert cash["withdrawals"] is None
        assert cash["purchases"] is None
        assert cash["sales_redemptions"] is None
        assert cash["total_credits"] is None
        assert cash["total_debits"] is None
        assert cash["currency_iso"] == "USD"

    def test_no_section_returns_none(self):
        assert pp.parse_cash_summary("nothing relevant\n") is None


class TestParseTransactionsLegacy:
    """Legacy (2017-2024) "Transaction Detail" / "Transaction
    Detail - <Category>" sections. Synthetic-text rows only."""

    def _wrap(self, body: str, *, with_category: bool = False) -> str:
        header = (
            "Transaction Detail - Purchases & Sales\n" if with_category
            else "Transaction Detail\n"
        )
        return (
            header
            + "Settle\nDate\nTrade\nDate Transaction Description Quantity Price Total\n"
            + body
            + "Total Account Value 0.00\n"
        )

    def test_2017_2019_one_numeric_row(self):
        text = self._wrap(
            "Cash, Bank Sweep, and Money Market Funds Activity\n"
            "12/20 12/20 Qualified Dividend ALPHACORP INC: ALPH 5.60\n"
        )
        # No statement_year inferable — pass explicitly.
        rows = pp.parse_transactions(text, statement_year=2019)
        assert len(rows) == 1
        r = rows[0]
        assert r.date == date(2019, 12, 20)
        assert r.symbol == "ALPH"
        assert r.amount == 5.60
        assert r.category == "Dividend"

    def test_2017_2019_three_numeric_row_with_negative_amount(self):
        text = self._wrap(
            "Investments Activity\n"
            "12/18 12/16 Bought ALPHACORP INC 100.0000 40.0000 (4,000.00)\n"
            "CLASS A: ALPH\n"
        )
        rows = pp.parse_transactions(text, statement_year=2019)
        assert len(rows) == 1
        r = rows[0]
        assert r.date == date(2019, 12, 18)
        assert r.quantity == 100.0
        assert r.price == 40.0
        assert r.amount == -4000.00
        assert r.category == "Purchase"
        assert r.symbol == "ALPH"  # picked up from continuation

    def test_2020_2024_section_header_with_category(self):
        text = self._wrap(
            "Equities Activity\n"
            "06/10/24 06/07/24 Bought BETACORP HLDG: BETA 6.0000 200.0000 0.00 (1,200.00)\n",
            with_category=True,
        )
        rows = pp.parse_transactions(text, statement_year=2024)
        assert len(rows) == 1
        r = rows[0]
        assert r.date == date(2024, 6, 10)
        assert r.quantity == 6.0
        assert r.price == 200.0
        assert r.charges == 0.00
        assert r.amount == -1200.00
        assert r.symbol == "BETA"
        assert r.category == "Purchase"

    def test_category_phrase_priority(self):
        # "Reinvested Shares" must beat the single-token
        # "Reinvest" / "Shares" matches that could compete.
        text = self._wrap(
            "Investments Activity\n"
            "12/30 12/30 Reinvested Shares ALPHACORP INC: ALPH 0.1033 100.0 (10.33)\n"
        )
        rows = pp.parse_transactions(text, statement_year=2019)
        assert len(rows) == 1
        assert rows[0].category == "Reinvest"

    def test_footnote_marker_glued_to_kind_phrase(self):
        # Schwab statements glue footnote-marker letters (X for
        # margin, Z for FDIC bank sweep, etc.) to the
        # transaction-type column with no separating whitespace,
        # so what the eye reads as "Bank Interest" arrives as
        # "Bank InterestX,Z". The category match has to tolerate
        # the suffix or the row falls through as kind=Unknown.
        text = self._wrap(
            "Cash, Bank Sweep, and Money Market Funds Activity\n"
            "12/15 12/16 Bank InterestX,Z BANK INT 111621-121521 1.15\n"
        )
        rows = pp.parse_transactions(text, statement_year=2021)
        assert len(rows) == 1
        assert rows[0].category == "Interest"
        assert rows[0].amount == 1.15

    def test_footnote_marker_single_letter(self):
        # Single-letter footnote (e.g. just "X" without a Z
        # paired) must also match — typical for Margin Interest.
        text = self._wrap(
            "Investments Activity\n"
            "04/01 04/01 Margin InterestX INTEREST 03/01THRU 04/01 (4321.00)\n"
        )
        rows = pp.parse_transactions(text, statement_year=2021)
        assert len(rows) == 1
        assert rows[0].category == "Interest"
        assert rows[0].amount == -4321.00

    def test_moneylink_deposit_is_a_deposit(self):
        # An inbound MoneyLink booked under its own phrase rather
        # than "MoneyLink Txn". Left unmapped it fell through as
        # kind=Unknown, and the far side of the movement sat
        # unpaired in the bank that sent it.
        text = self._wrap(
            "Cash, Bank Sweep, and Money Market Funds Activity\n"
            "06/15 06/15 MoneyLink Deposit SYNTHETIC HOLDER 60,000.00\n"
        )
        rows = pp.parse_transactions(text, statement_year=2021)
        assert len(rows) == 1
        assert rows[0].category == "Deposit"
        assert rows[0].amount == 60000.00

    def test_moneylink_return_takes_its_side_from_the_sign(self):
        # A returned MoneyLink is a transfer whose direction only
        # the amount states, like the undirected "MoneyLink Txn".
        text = self._wrap(
            "Cash, Bank Sweep, and Money Market Funds Activity\n"
            "01/03 01/03 MoneyLink Return Tfr SYNTHETIC-BANK, N/A (2,000.00)\n"
        )
        rows = pp.parse_transactions(text, statement_year=2023)
        assert len(rows) == 1
        assert rows[0].category == "Transfer"
        assert rows[0].amount == -2000.00

    def test_auto_transfer_categorised(self):
        # Bank-sweep transfer rows ("Auto TransferX,Z BANK
        # CREDIT FROM BROKERAGE ...") were also slipping past
        # the kind dispatch — now explicitly mapped to Transfer.
        text = self._wrap(
            "Cash, Bank Sweep, and Money Market Funds Activity\n"
            "12/03 12/03 Auto TransferX BANK CREDIT FROM BROKERAGE 800.00 1,000.00\n"
        )
        rows = pp.parse_transactions(text, statement_year=2021)
        assert len(rows) == 1
        assert rows[0].category == "Transfer"

    def test_no_footnote_still_matches(self):
        # The footnote group is optional — phrases that appear
        # without any glued letters must continue to match.
        text = self._wrap(
            "Investments Activity\n"
            "12/20 12/20 Qualified Dividend ALPHACORP INC: ALPH 5.60\n"
        )
        rows = pp.parse_transactions(text, statement_year=2019)
        assert rows[0].category == "Dividend"

    def test_description_does_not_absorb_page_chrome(self):
        # Pre-2024 layout: an Interest row sitting at the end of
        # its sub-section had the parser sweep up everything
        # until the next "Total Account Value", which included
        # the page-footer repeat (Schwab page header + the
        # account-holder's name + account number) and any
        # following section. Description must STOP at the
        # page-chrome boundary.
        text = self._wrap(
            "Cash, Bank Sweep, and Money Market Funds Activity\n"
            "12/15 12/16 Bank InterestX,Z BANK INT 111621-121521 1.15\n"
            "Bank Sweep: Interest Rate as of 12/31/21 was 0.01%.\n"
            "Schwab One International Account of\n"
            "PLACEHOLDER CUST FOR PLACEHOLDER\n"
            "Account Number\n"
            "0000-0000\n"
            "Statement Period\n"
            "December 1-31, 2021\n"
            "Page 17 of 18\n"
        )
        rows = pp.parse_transactions(text, statement_year=2021)
        assert len(rows) == 1
        # Bank Sweep disclosure starts with "Bank Sweep:" which
        # is a row-stop marker; everything from there must NOT
        # be in the description.
        assert "Account Number" not in rows[0].description
        assert "PLACEHOLDER" not in rows[0].description
        assert "0000" not in rows[0].description
        assert "Page 17 of 18" not in rows[0].description
        # The legitimate "BANK INT 111621-121521" content stays.
        assert "BANK INT" in rows[0].description

    def test_description_stops_at_the_sub_section_summary(self):
        # The last row of a sub-section sat right above its closing
        # summary and the next banner, and absorbed both: the period's
        # totals then travelled in the row's description, where they
        # read as part of the payee.
        text = self._wrap(
            "07/21 07/21 Deposit Funds Received EXAMPLE CHECK 321.00\n"
            "The total deposits activity for the statement period was $321.00. "
            "The total withdrawals activity for the statement period was $0.00.\n"
            "Transaction Detail - Purchases & Sales (continued)\n"
        )
        rows = pp.parse_transactions(text, statement_year=2022)
        assert len(rows) == 1
        assert "total" not in rows[0].description.lower()
        assert "Transaction Detail" not in rows[0].description
        assert "EXAMPLE CHECK" in rows[0].description

    def test_description_stops_at_the_margin_disclosures(self):
        text = self._wrap(
            "10/15 10/15 ADR Pass Thru Fee EXAMPLE HLDGS FSPONSORED ADR (1.50)\n"
            "1 ADR REPS\n"
            "Margin interest charged to your Account during the statement "
            "period is included in this section of the statement.\n"
            "10/29 10/29 Margin Interest INTEREST 09/30THRU 10/29 (0.25)\n"
            "The opening margin loan balance on 10/01 was $0.00.\n"
        )
        rows = pp.parse_transactions(text, statement_year=2021)
        assert len(rows) == 2
        assert rows[0].description.endswith("1 ADR REPS")
        assert "opening margin" not in rows[1].description

    def test_description_capped_at_max_chars(self):
        # Belt-and-suspenders: if a row picks up several short
        # continuation lines that AREN'T row-stop markers (no
        # leading keyword), the total description still has a
        # ceiling so a future Schwab layout quirk can't smuggle
        # in another page of chrome.
        long_continuation = "FAKEFRAGMENT" * 30  # 360 chars
        text = self._wrap(
            "Investments Activity\n"
            "12/30 12/30 Reinvested Shares ALPHACORP INC: ALPH 0.1 100.0 (10.00)\n"
            + long_continuation + "\n"
        )
        rows = pp.parse_transactions(text, statement_year=2019)
        assert len(rows) == 1
        # Cap is _LEGACY_TX_DESC_MAX_CHARS (200).
        assert len(rows[0].description) <= 200

    def test_new_format_wins_over_legacy(self):
        # If the text has the 2025+ "Transaction Details" anchor
        # (plural) the new parser handles it and the legacy
        # path stays unused. Both anchors here.
        text = (
            "February 1-28, 2026\n"
            "Transaction Details\n"
            "Symbol/ Price/Rate\n"
            "02/05 Deposit FundsReceived WIRE 1,000.00\n"
            "TotalTransactions $1 $2\n"
            # Legacy section in the same blob — must be ignored
            # because the new parser already returned rows.
            "Transaction Detail\n"
            "12/20 12/20 Qualified Dividend FOO: FOOX 5.60\n"
            "Total Account Value 0.00\n"
        )
        rows = pp.parse_transactions(text)
        # New-format rows only.
        assert all(t.symbol != "FOOX" for t in rows)
        assert any(t.category == "Deposit" for t in rows)


class TestStatementPdfReturnShape:
    def test_dict_carries_positions_and_cash_keys(self):
        # Build a tiny synthetic full-text that has every section
        # the high-level function looks at. We then call the
        # public functions individually since parse_statement_pdf
        # itself opens a real PDF (covered separately by
        # test_pdf_parsers_fixture).
        text = (
            "February 1-28, 2026\n"
            "Transactions - Summary\n"
            "header\n"
            "$1.00 $2.00 ($3.00) $0.00 $5.00 $0.50 $0.00 $5.50\n"
            "Transaction Details\n"
            "Symbol/ Price/Rate\n"
            "02/02 Deposit FundsReceived WIRE 100.00\n"
            "TotalTransactions $1 $2\n"
            "Positions - Equities\n"
            "Symbol Description Quantity Price MV CB Gain Yield Income Acct\n"
            "SYN1 SyntheticOne(M) 10.0000 1.00000 10.00 8.00 2.00 N/A N/A 1%\n"
            "TotalEquities $10.00 $8.00 $2.00 N/A 1%\n"
        )
        # Each parser is callable in isolation.
        period = pp.parse_statement_period(text)
        assert period is not None
        txs = pp.parse_transactions(text)
        assert any(t.category == "Deposit" for t in txs)
        positions = pp.parse_positions(text)
        assert positions and positions[0]["instrument_key"] == "SYN1"
        cash = pp.parse_cash_summary(text)
        assert cash and cash["closing_balance"] == 5.50


# ============================================================
# 3rd-Party-Distribution letters
# ============================================================
#
# Synthetic text mimicking pypdfium2's one-row-per-line extraction of
# the three observed layout families. Wholly invented counterparties /
# amounts; VTI is a widely-held example ticker (AGENTS.md §4).

class TestParseDistributionText:
    _SECURITIES = (
        "Account(s) ending: 999\n"
        "January 23, 2024\n"
        "Confirmation: We've moved funds as requested.\n"
        "account noted above. We've transferred these funds as described below.\n"
        "Transfer(s) to Schwab accounts of third parties\n"
        "To account ending in: 321\n"
        "Account name: SYNTH FAMILY TRUST\n"
        "Security(ies) transferred:\n"
        "Symbol Quantity Market Value\n"
        "VTI 12.50000 $1,250.00\n"
        "Total market value: $1,250.00\n"
        "Please note that the transaction amounts above do not reflect transaction fees.\n"
    )
    _WIRE = (
        "Account(s) ending: 999\n"
        "October 9, 2025\n"
        "We've transferred these funds as described below.\n"
        "Wire transfer(s)\n"
        "Reference: SYNTHWIREREF0001\n"
        "To the account of SYNTH OPPORTUNITY FUND LP at Some Synthetic Bank and\n"
        "Account ending in: 654\n"
        "Cash transfer amount requested: $25,000.00\n"
    )
    _CASH_SCHWAB = (
        "Account(s) ending: 999\n"
        "June 17, 2024\n"
        "We've transferred these funds as described below.\n"
        "Transfer(s) to Schwab accounts of third parties\n"
        "To account ending in: 777\n"
        "Account name: SYNTH BENEFICIARY\n"
        "Cash transfer amount requested: $3,141.59\n"
    )

    def test_securities_transfer(self):
        rows = pp.parse_distribution_text(
            self._SECURITIES, filename="3rd-Party-Distribution_2024-01-23_999.PDF")
        assert len(rows) == 1
        r = rows[0]
        assert r["transfer_kind"] == "securities"
        assert r["direction"] == "out"
        assert r["kind"] == "Transfer Out"
        assert r["method"] == "schwab_third_party"
        assert r["symbol"] == "VTI"
        assert r["instrument_key"] == "VTI"
        assert r["quantity"] == 12.5
        assert r["market_value"] == 1250.0
        assert r["amount"] == 1250.0
        assert r["counterparty"] == "SYNTH FAMILY TRUST"
        assert r["counterparty_account_suffix"] == "321"
        assert r["date"] == "2024-01-23"

    def test_wire_cash_transfer(self):
        rows = pp.parse_distribution_text(
            self._WIRE, filename="3rd-Party-Distribution_2025-10-09_999.PDF")
        assert len(rows) == 1
        r = rows[0]
        assert r["transfer_kind"] == "cash"
        assert r["method"] == "wire"
        assert r["cash_amount"] == 25000.0
        assert r["amount"] == 25000.0
        assert r["symbol"] is None
        assert r["instrument_key"] is None
        assert r["counterparty"] == "SYNTH OPPORTUNITY FUND LP"
        assert r["counterparty_bank"] == "Some Synthetic Bank"
        assert r["counterparty_account_suffix"] == "654"
        assert r["date"] == "2025-10-09"

    def test_cash_to_schwab_third_party(self):
        rows = pp.parse_distribution_text(
            self._CASH_SCHWAB, filename="3rd-Party-Distribution_2024-06-17_999.PDF")
        assert len(rows) == 1
        r = rows[0]
        assert r["transfer_kind"] == "cash"
        assert r["method"] == "schwab_third_party"
        assert r["cash_amount"] == 3141.59
        assert r["counterparty"] == "SYNTH BENEFICIARY"
        assert r["counterparty_account_suffix"] == "777"

    def test_date_falls_back_to_filename(self):
        # Body with no "Month D, YYYY" line → use the filename date.
        text = ("Wire transfer(s)\n"
                "To the account of SYNTH LP at Bank\n"
                "Cash transfer amount requested: $1.00\n")
        rows = pp.parse_distribution_text(
            text, filename="3rd-Party-Distribution_2022-03-04_999.PDF")
        assert rows[0]["date"] == "2022-03-04"

    def test_unrecognised_layout_emits_no_rows(self):
        rows = pp.parse_distribution_text(
            "Some unrelated letter with no transfer block.\n",
            filename="3rd-Party-Distribution_2024-01-01_999.PDF")
        assert rows == []

    def test_multi_security_table(self):
        text = (
            "March 1, 2024\n"
            "Transfer(s) to Schwab accounts of third parties\n"
            "To account ending in: 100\n"
            "Account name: SYNTH DAF\n"
            "Security(ies) transferred:\n"
            "Symbol Quantity Market Value\n"
            "VTI 1.00000 $100.00\n"
            "SPY 2.00000 $200.00\n"
            "Total market value: $300.00\n"
        )
        rows = pp.parse_distribution_text(text, filename="x_2024-03-01_100.PDF")
        assert len(rows) == 2
        assert {r["symbol"] for r in rows} == {"VTI", "SPY"}
        assert all(r["transfer_kind"] == "securities" for r in rows)
