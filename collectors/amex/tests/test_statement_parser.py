"""Tests for the statement-PDF parser, on synthetic extracted text.

The fixtures below are written from scratch to the LAYOUT the real statements
use — the interleaved summary column, the bare section headings, the
`MM/DD/YY*` rows — with invented merchants and round amounts. No real
statement text, merchant or figure appears here.
"""
from __future__ import annotations

import sys
from datetime import date
from decimal import Decimal
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import statement_parser as sp  # noqa: E402


def statement(*, previous="100.00", payments="-40.00", charges="+60.00",
              fees="+0.00", interest="+0.00", new="120.00",
              payment_rows=(), credit_rows=(), charge_rows=(), fee_rows=(),
              interest_rows=(), closing="03/12/26", card_ending=False,
              trailer=True) -> str:
    """Render a synthetic statement in the real layout.

    The summary is deliberately interleaved with legal prose on the same
    lines, because that is what the real document does and what the blob-based
    summary reader exists for.
    """
    def rows(items):
        return "\n".join(
            f" {d}{'*' if star else ''}     {desc:<40s}"
            f"{'CA':>12s}          {amt}"
            for d, star, desc, amt in items)

    out = [
        "                                       Account Ending 1-01234",
        f" Closing Date {closing}                             TTY: 1-800-000",
        "",
        f" New Balance ${new} as of {closing}",
        " Minimum Payment Due $35.00",
        f" Previous Balance ${previous}",
        f" Late Payment Warning: If we do not receive your   Payments/Credits {payments.replace('-', '-$')}",
        f" Minimum Payment Due by the Payment Due Date of    New Charges {charges.replace('+', '+$')}",
        f" 04/05/26, you may have to pay a late fee.         Fees {fees.replace('+', '+$')}",
        f"                                                   Interest Charged {interest.replace('+', '+$')}",
        " Credit Limit $10,000.00",
        " Available Credit $9,880.00",
        "",
        " Payments and Credits",
        f" Payments -${abs(float(payments)):.2f}" if payment_rows else " Payments $0.00",
        f" Total Payments and Credits {payments.replace('-', '-$')}",
        "",
        " Detail *Indicates posting date",
        " Payments Amount",
        rows(payment_rows),
    ]
    if credit_rows:
        out += [" Credits", rows(credit_rows)]
    out += [
        "",
        " New Charges",
        f" Total New Charges ${abs(float(charges)):.2f}",
        " Detail",
    ]
    if card_ending:
        out += [" Card Ending 2-05678", " Detail Continued"]
    out += [rows(charge_rows)]
    if fee_rows:
        out += ["", " Fees", rows(fee_rows),
                f" Total Fees for this Period ${abs(float(fees)):.2f}"]
    if interest_rows:
        out += ["", " Interest Charged", rows(interest_rows),
                " Total Interest Charged for this Period "
                f"${abs(float(interest)):.2f}"]
    if trailer:
        # The year-to-date block that follows the activity and must not be
        # read as part of it.
        out += ["", " Total Fees in 2026 $0.00", " Total Interest in 2026 $0.00",
                " 03/01/26  NOT A TRANSACTION                        $999.00"]
    return "\n".join(out) + "\n"


BUY = [("02/20/26", False, "EXAMPLE STORE", "$60.00")]
PAY = [("03/05/26", True, "EXAMPLE PAYMENT RECEIVED", "-$40.00")]


# ============================================================
# Period + summary
# ============================================================

def test_the_closing_date_is_the_only_period_date():
    p = sp.parse_card_statement_text(statement(payment_rows=PAY,
                                               charge_rows=BUY))
    assert p.period_end == date(2026, 3, 12)


def test_the_summary_is_read_out_of_the_interleaved_column():
    # Every figure sits on a line that also carries unrelated legal prose.
    p = sp.parse_card_statement_text(statement(payment_rows=PAY,
                                               charge_rows=BUY))
    assert p.previous_balance == Decimal("100.00")
    assert p.payments_credits == Decimal("-40.00")
    assert p.new_charges == Decimal("60.00")
    assert p.fees == Decimal("0.00")
    assert p.interest_charged == Decimal("0.00")
    assert p.new_balance == Decimal("120.00")


def test_each_summary_label_resolves_to_its_own_amount():
    # "New Charges" and "New Balance" share their first word, and each
    # pattern carries the whole label, so the two resolve separately.
    p = sp.parse_card_statement_text(statement(payment_rows=PAY,
                                               charge_rows=BUY))
    assert p.new_balance != p.new_charges


@pytest.mark.parametrize("raw,want", [
    ("$100.00", Decimal("100.00")),
    ("-$40.00", Decimal("-40.00")),
    ("$-40.00", Decimal("-40.00")),
    ("+$60.00", Decimal("60.00")),
    ("$1,234.56", Decimal("1234.56")),
    ("nonsense", None),
])
def test_amount_parsing(raw, want):
    assert sp._dec(raw) == want


# ============================================================
# Rows and sections
# ============================================================

def test_rows_are_stamped_with_their_section():
    p = sp.parse_card_statement_text(statement(payment_rows=PAY,
                                               charge_rows=BUY))
    assert [(t.kind, str(t.amount)) for t in p.transactions] == [
        ("STMT_PAYMENT", "-40.00"), ("STMT_PURCHASE", "60.00")]


def test_the_document_sign_convention_is_preserved():
    # A charge is POSITIVE and a payment NEGATIVE, as printed; the loader
    # converts to silver's inverse convention.
    p = sp.parse_card_statement_text(statement(payment_rows=PAY,
                                               charge_rows=BUY))
    charge = next(t for t in p.transactions if t.kind == "STMT_PURCHASE")
    payment = next(t for t in p.transactions if t.kind == "STMT_PAYMENT")
    assert charge.amount > 0 and payment.amount < 0


def test_a_posting_date_star_is_recorded():
    p = sp.parse_card_statement_text(statement(payment_rows=PAY,
                                               charge_rows=BUY))
    assert next(t for t in p.transactions if t.kind == "STMT_PAYMENT").posting_date
    assert not next(t for t in p.transactions
                    if t.kind == "STMT_PURCHASE").posting_date


def test_a_credits_subsection_is_its_own_kind():
    # A statement credit is not a bill payment, and the deep era has no
    # category to tell them apart — only the section.
    credits = [("03/02/26", False, "CREDIT ADJUSTMENT", "-$15.00")]
    p = sp.parse_card_statement_text(statement(
        payments="-55.00", new="105.00",
        payment_rows=PAY, credit_rows=credits, charge_rows=BUY))
    kinds = {t.kind for t in p.transactions}
    assert "STMT_CREDIT" in kinds and "STMT_PAYMENT" in kinds
    assert sp.rows_reconcile(p)


def test_a_card_ending_heading_does_not_reset_the_section():
    # It sub-divides New Charges by card member; its rows are still charges,
    # and they all belong to the one account.
    two = BUY + [("02/25/26", False, "OTHER STORE", "$40.00")]
    p = sp.parse_card_statement_text(statement(
        charges="+100.00", new="160.00", payment_rows=PAY, charge_rows=two,
        card_ending=True))
    assert [t.kind for t in p.transactions] == [
        "STMT_PAYMENT", "STMT_PURCHASE", "STMT_PURCHASE"]
    assert sp.rows_reconcile(p)


def test_the_year_to_date_trailer_is_not_read_as_activity():
    # A date-shaped line after the activity block would otherwise be imported
    # as a phantom transaction.
    p = sp.parse_card_statement_text(statement(payment_rows=PAY,
                                               charge_rows=BUY))
    assert all("NOT A TRANSACTION" not in t.description
               for t in p.transactions)


def test_the_trailing_columns_are_dropped_from_the_description():
    p = sp.parse_card_statement_text(statement(payment_rows=PAY,
                                               charge_rows=BUY))
    charge = next(t for t in p.transactions if t.kind == "STMT_PURCHASE")
    assert charge.description == "EXAMPLE STORE"


def test_fees_and_interest_are_their_own_sections():
    fees = [("02/28/26", False, "ANNUAL MEMBERSHIP FEE", "$12.00")]
    interest = [("03/12/26", False, "INTEREST CHARGE ON PURCHASES", "$3.00")]
    p = sp.parse_card_statement_text(statement(
        fees="+12.00", interest="+3.00", new="135.00",
        payment_rows=PAY, charge_rows=BUY, fee_rows=fees,
        interest_rows=interest))
    kinds = [t.kind for t in p.transactions]
    assert "STMT_FEE" in kinds and "STMT_INTEREST" in kinds
    assert sp.rows_reconcile(p)


# ============================================================
# The reconciliation gates
# ============================================================

def test_a_reconciling_statement_passes_both_gates():
    p = sp.parse_card_statement_text(statement(payment_rows=PAY,
                                               charge_rows=BUY))
    assert sp.summary_reconciles(p) and sp.rows_reconcile(p)


def test_a_summary_that_does_not_add_up_is_refused():
    p = sp.parse_card_statement_text(statement(new="999.00",
                                               payment_rows=PAY,
                                               charge_rows=BUY))
    assert not sp.summary_reconciles(p)
    assert not sp.rows_reconcile(p)


def test_rows_that_do_not_sum_to_their_section_total_are_refused():
    # The summary still adds up; only the ROWS are short. A whole-period check
    # would miss this when another section absorbs the difference.
    short = [("02/20/26", False, "EXAMPLE STORE", "$50.00")]
    p = sp.parse_card_statement_text(statement(payment_rows=PAY,
                                               charge_rows=short))
    assert sp.summary_reconciles(p)
    assert not sp.rows_reconcile(p)


def test_a_document_with_no_summary_is_refused():
    # The accessible-PDF variant renders no summary this parser can read; it
    # must be skipped, never imported short.
    assert not sp.rows_reconcile(sp.parse_card_statement_text("nothing here\n"))


def test_a_missing_section_is_caught_by_the_per_section_check():
    # Rows under an unknown heading inherit the preceding section's kind,
    # which leaves the whole-period total untouched — so the check has to be
    # section by section.
    text = statement(payment_rows=PAY, charge_rows=BUY).replace(
        " New Charges\n", " Some Section This Parser Does Not Know\n", 1)
    p = sp.parse_card_statement_text(text)
    assert not sp.rows_reconcile(p)


# ============================================================
# Period chaining (the document states no opening date)
# ============================================================

def test_period_starts_chain_from_the_previous_close():
    import load
    ends = [load.epoch_day(date(2026, 1, 12)),
            load.epoch_day(date(2026, 2, 12)),
            load.epoch_day(date(2026, 3, 12))]
    starts = load.statement_period_starts(ends)
    assert starts[ends[1]] == ends[0] + 86400
    assert starts[ends[2]] == ends[1] + 86400
    # The oldest period has no predecessor; it anchors a balance and nothing
    # reads its start.
    assert starts[ends[0]] == ends[0]


def test_period_chaining_is_order_independent():
    import load
    ends = [load.epoch_day(date(2026, 3, 12)),
            load.epoch_day(date(2026, 1, 12))]
    starts = load.statement_period_starts(ends)
    assert starts[ends[0]] == ends[1] + 86400
