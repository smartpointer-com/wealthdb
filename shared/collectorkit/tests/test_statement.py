"""Tests for collectorkit.statement, each against the parser it is
lifted from: chase's card parser and amex's, called on invented
statements."""
from __future__ import annotations

import dataclasses
from datetime import date
from decimal import Decimal

import collector_modules
import pytest

from collectorkit.statement import (identity_problems, labelled_amounts,
                                    row_date, section_problems)

chase = collector_modules.load("chase", "statement_parser")
amex = collector_modules.load("amex", "statement_parser")

PERIODS = [
    (date(2025, 6, 29), date(2025, 7, 28)),     # inside one year
    (date(2024, 12, 20), date(2025, 1, 19)),    # across the new year
    (date(2023, 12, 31), date(2024, 1, 30)),    # a leap year ahead
    (date(2024, 2, 1), date(2024, 2, 29)),      # a leap day inside
    (date(2025, 1, 1), date(2025, 1, 1)),       # a one-day period
    (date(2025, 3, 1), date(2025, 9, 30)),      # half a year apart: ties
]


@pytest.mark.parametrize("start,end", PERIODS)
def test_row_date_is_chases_card_row_date(start, end):
    for mm in range(0, 14):
        for dd in range(0, 33):
            assert row_date(mm, dd, start, end) == \
                chase._card_row_date(mm, dd, start, end), (mm, dd)


def test_row_date_branches():
    start, end = date(2024, 12, 20), date(2025, 1, 19)
    assert row_date(12, 28, start, end) == date(2024, 12, 28)   # inside
    assert row_date(12, 15, start, end) == date(2024, 12, 15)   # just before
    assert row_date(1, 25, start, end) == date(2025, 1, 25)     # just after
    assert row_date(13, 2, start, end) is None                  # no date
    assert row_date(2, 29, date(2025, 2, 1), date(2025, 2, 28)) == \
        date(2024, 2, 29)                                       # leap year back


CHASE_BLOB = ("ACCOUNT SUMMARY Previous Balance $1,234.56 Payment, Credits "
              "-$40.00 Purchases +$79.99 Cash Advances $0.00 Balance "
              "Transfers $0.00 Fees Charged $0.00 Interest Charged $1.10 "
              "New Balance $1,275.65 Credit Access Line $5,000 Opening/"
              "Closing Date 06/29/25 - 07/28/25")
AMEX_TEXT = ("Previous Balance $100.00\nLate Payment Warning: legal text "
             "Payments/Credits -$100.00\nNew Charges +$60.00 more text\n"
             "Fees +$0.00\nInterest Charged $-1.00\nNew Balance $59.00\n")


def test_labelled_amounts_is_chases_card_labelled():
    found = labelled_amounts(CHASE_BLOB, chase.CARD_SUMMARY_LABELS,
                             chase._CARD_SUMMARY_AMOUNT)
    for name, label in chase.CARD_SUMMARY_LABELS.items():
        raw = found[name]
        mine = None if raw is None else chase._dec(raw.replace("+", ""))
        assert mine == chase._card_labelled(CHASE_BLOB, label), name
    assert labelled_amounts("Purchases none", {"p": "Purchases"},
                            chase._CARD_SUMMARY_AMOUNT) == {"p": None}


def test_labelled_amounts_is_amexs_summary():
    found = labelled_amounts(amex._norm(AMEX_TEXT), amex.CARD_SUMMARY_LABELS,
                             amex._SUMMARY_AMOUNT)
    mine = {k: None if v is None else amex._dec(v) for k, v in found.items()}
    assert mine == amex._summary(AMEX_TEXT)


def _chase_card(**over):
    """An invented chase card statement that passes both gates."""
    txn = chase.StatementTxn
    base = dict(
        period_start=date(2025, 6, 29), period_end=date(2025, 7, 28),
        previous_balance=Decimal("100.00"), payments_credits=Decimal("-40.00"),
        purchases=Decimal("79.99"), cash_advances=Decimal("0.00"),
        balance_transfers=Decimal("0"), fees_charged=Decimal("0.00"),
        interest_charged=Decimal("1.10"), new_balance=Decimal("141.09"),
        transactions=[
            txn(date(2025, 7, 1), Decimal("-40.00"), "PAYMENT",
                kind="STMT_PAYMENT"),
            txn(date(2025, 7, 2), Decimal("80.00"), "SHOP",
                kind="STMT_PURCHASE"),
            txn(date(2025, 7, 3), Decimal("-0.01"), "SHOP REFUND",
                kind="STMT_PURCHASE"),
            txn(date(2025, 7, 28), Decimal("1.10"), "INTEREST",
                kind="STMT_INTEREST"),
        ])
    base.update(over)
    return chase.ParsedCardStatement(**base)


def _chase_variants():
    good = _chase_card()
    yield good
    for name in ("previous_balance", "new_balance", "purchases",
                 "cash_advances", "fees_charged", "interest_charged"):
        yield dataclasses.replace(good, **{name: None})
    yield dataclasses.replace(good, new_balance=Decimal("141.10"))
    yield dataclasses.replace(good, cash_advances=Decimal("5.00"),
                              new_balance=Decimal("146.09"))
    moved = list(good.transactions)
    moved[1] = dataclasses.replace(moved[1], kind="STMT_PAYMENT")
    yield dataclasses.replace(good, transactions=moved)
    yield dataclasses.replace(good, transactions=good.transactions[:-1])
    yield dataclasses.replace(good, transactions=[])


def _chase_summary(stmt):
    terms = [getattr(stmt, f) for f in chase._CARD_SUMMARY_ADDENDS]
    return not identity_problems("summary", terms, stmt.new_balance)


def _chase_rows(stmt):
    stated = {f: getattr(stmt, f) for f in (
        *chase.CARD_SECTION_TOTALS.values(),
        *chase._CARD_UNMAPPED_SUMMARY_FIELDS)}
    period = identity_problems(
        "period", [stmt.previous_balance, *(t.amount for t in stmt.transactions)],
        stmt.new_balance)
    sections = section_problems(stmt.transactions, chase.CARD_SECTION_TOTALS,
                                stated, chase._CARD_UNMAPPED_SUMMARY_FIELDS)
    return not period and not sections


def test_identity_and_sections_are_chases_gates():
    variants = list(_chase_variants())
    assert chase.card_summary_reconciles(variants[0])
    assert chase.card_rows_reconcile(variants[0])
    for stmt in variants:
        assert _chase_summary(stmt) == chase.card_summary_reconciles(stmt)
        assert _chase_rows(stmt) == chase.card_rows_reconcile(stmt)


def _amex_card(**over):
    txn = amex.StatementTxn
    base = dict(
        period_end=date(2025, 2, 28), previous_balance=Decimal("100.00"),
        payments_credits=Decimal("-100.00"), new_charges=Decimal("60.00"),
        fees=Decimal("0.00"), interest_charged=Decimal("-1.00"),
        new_balance=Decimal("59.00"),
        transactions=[
            txn(date(2025, 2, 1), "PAYMENT", Decimal("-90.00"), "STMT_PAYMENT"),
            txn(date(2025, 2, 2), "REFUND", Decimal("-10.00"), "STMT_CREDIT"),
            txn(date(2025, 2, 3), "SHOP", Decimal("60.00"), "STMT_PURCHASE"),
            txn(date(2025, 2, 28), "INTEREST", Decimal("-1.00"),
                "STMT_INTEREST"),
        ])
    base.update(over)
    return amex.ParsedCardStatement(**base)


def _amex_variants():
    good = _amex_card()
    yield good
    for name in ("previous_balance", "new_balance", "payments_credits",
                 "new_charges", "fees", "interest_charged"):
        yield dataclasses.replace(good, **{name: None})
    yield dataclasses.replace(good, new_balance=Decimal("59.01"))
    odd = list(good.transactions) + [
        amex.StatementTxn(date(2025, 2, 4), "X", Decimal("0.00"), "STMT_ODD")]
    yield dataclasses.replace(good, transactions=odd)
    swapped = list(good.transactions)
    swapped[2] = dataclasses.replace(swapped[2], kind="STMT_FEE")
    yield dataclasses.replace(good, transactions=swapped)
    # Payments and credits share a figure: moving a row between them holds.
    shared = list(good.transactions)
    shared[1] = dataclasses.replace(shared[1], kind="STMT_PAYMENT")
    yield dataclasses.replace(good, transactions=shared)


def _amex_summary(stmt):
    terms = [stmt.previous_balance,
             *(getattr(stmt, f) for f in amex.CARD_SUMMARY_ADDENDS)]
    return not identity_problems("summary", terms, stmt.new_balance)


def _amex_rows(stmt):
    stated = {f: getattr(stmt, f) for f in amex.CARD_SECTION_TOTALS.values()}
    return _amex_summary(stmt) and not section_problems(
        stmt.transactions, amex.CARD_SECTION_TOTALS, stated)


def test_identity_and_sections_are_amexs_gates():
    variants = list(_amex_variants())
    assert amex.rows_reconcile(variants[0])
    assert amex.rows_reconcile(variants[-1])
    for stmt in variants:
        assert _amex_summary(stmt) == amex.summary_reconciles(stmt)
        assert _amex_rows(stmt) == amex.rows_reconcile(stmt)


def test_problem_messages_name_fields_not_amounts():
    stmt = dataclasses.replace(_chase_card(), purchases=Decimal("79.98"))
    problems = section_problems(
        stmt.transactions, chase.CARD_SECTION_TOTALS,
        {f: getattr(stmt, f) for f in chase.CARD_SECTION_TOTALS.values()})
    assert problems == ["purchases: its rows do not add up to it"]
    assert identity_problems("summary", [Decimal(1)], Decimal(2)) == \
        ["summary: does not add up"]
    assert identity_problems("summary", [None], Decimal(2)) == \
        ["summary: a figure is missing"]
