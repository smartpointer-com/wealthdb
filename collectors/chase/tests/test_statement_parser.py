"""Unit tests for statement_parser — the pure text parse, for both layouts:
the deposit statement (OpenText markers, per-product segments, section signs)
and the card statement (one card per document, printed row signs, a summary
identity and a section-by-section row identity). Synthetic statement text
only (mirrors the `pdftotext -layout` shape); no real statement data.
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
                Ref 00000000000
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
    assert "Ref 00000000000" in zelle.description
    assert all("not a transaction" not in t.description for t in txns)


def test_reconciles_and_running_balance():
    s = sp.parse_statement_text(STATEMENT)
    assert sp.segment_reconciles(s.segments[0]) is True
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
    assert sp.segment_reconciles(seg) is True


def test_reconcile_flags_a_missed_row():
    # Drop a row's amount from the totals → beginning+Σ != ending.
    bad = STATEMENT.replace("  04/05    Check                                250.00\n", "")
    s = sp.parse_statement_text(bad)
    assert sp.segment_reconciles(s.segments[0]) is False


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
    # break one segment → only that segment fails
    bad = sp.parse_statement_text(
        COMBINED.replace("  06/10   Withdrawal B           100.00\n", ""))
    assert sp.segment_reconciles(bad.segments[0]) is True
    assert sp.segment_reconciles(bad.segments[1]) is False


def test_segment_reconciles_on_hand_built_segment():
    seg = sp.StatementSegment(
        beginning_balance=Decimal("10.00"), ending_balance=Decimal("15.00"),
        transactions=[sp.StatementTxn(date(2023, 1, 2), Decimal("5.00"), "x")])
    assert sp.segment_reconciles(seg) is True
    seg.ending_balance = Decimal("16.00")
    assert sp.segment_reconciles(seg) is False


def test_a_segment_with_no_balance_pair_does_not_reconcile():
    # A missing balance is not an absent question, it is an unanswerable
    # one — and it means the parse is already degraded, which is exactly when
    # the row sum must not be waved through. The card path answers the same
    # way (`card_summary_reconciles`).
    seg = sp.StatementSegment(
        beginning_balance=None, ending_balance=Decimal("15.00"),
        transactions=[sp.StatementTxn(date(2023, 1, 2), Decimal("5.00"), "x")])
    assert sp.segment_reconciles(seg) is False
    seg.beginning_balance, seg.ending_balance = Decimal("10.00"), None
    assert sp.segment_reconciles(seg) is False


# ============================================================
# Card statements
# ============================================================
#
# One synthetic card statement carrying every hazard the layout imposes, in
# the places a real render puts them:
#
#   * the period as `Opening/Closing Date  MM/DD/YY - MM/DD/YY` (two-digit
#     years, a hyphen) — the shape the deposit period regex cannot read;
#   * a summary label split across two lines around a stray backtick
#     (`Balance` / `` ` Transfers ``), and a whole-dollar credit-line figure;
#   * right-column text bleeding onto summary lines, as the two-column page
#     layout produces;
#   * a purchase transacted the day BEFORE the period opens (the printed date
#     is the transaction date, which the post date can outrun into the next
#     cycle);
#   * a page break that repeats `ACCOUNT ACTIVITY (CONTINUED)` and the column
#     header but not the section heading, with an account-message block and
#     page furniture between the two halves of the same section;
#   * an FX continuation that STARTS with MM/DD, and a foreign-fee
#     continuation that ENDS in a bare amount — each a phantom row for a
#     parser that requires only one of the two;
#   * a sub-dollar amount printed without its leading zero;
#   * `TOTAL FEES FOR THIS PERIOD` / `TOTAL INTEREST FOR THIS PERIOD` inside
#     their own transaction sections;
#   * the `Table Summary` / `empty cell` artifacts one render generation
#     sprinkles through the page;
#   * the blocks that must be skipped whole — the minimum-payment
#     amortisation table, the rewards summary, the legal pages, the
#     year-to-date totals, the APR table and the promotional block.
#
# Figures: previous 100.00, payments -40.00, purchases +79.99, fees +3.00,
# interest +2.00 → new 144.99, and the printed rows sum to the same 44.99.
CARD_STATEMENT = """\
                                              Manage your account online at:
                                              www.chase.com/cardhelp

                                                    New Balance
                     August 2026                                        EXAMPLE CARD REWARDS SUMMARY
                                                    $144.99
      S    M    T    W    T    F    S                                   Previous points balance          400
                                                    Minimum Payment Due
     26   27   28   29   30   31    1                                   + 1 point per $1 on purchases     80
                                                    $25.00
      2    3    4    5    6    7    8                                   Total points available         480

    Minimum Payment Warning: If you make only the minimum payment each
    period, you will pay more in interest and it will take you longer to
    pay off your balance. For example:

        If you make no      You will pay off the And you will end up
       additional charges balance shown on this paying an estimated
      using this card and  statement in about...      total of...
     each month you pay...

         Only the minimum                  12 months              $160
             payment

    Table Summary

    ACCOUNT SUMMARY
    Account Number: XXXX XXXX XXXX 0000
    Previous Balance                                     $100.00        EXAMPLE CARD REWARDS SUMMARY
    Payment, Credits                                     -$40.00
    Purchases                                            +$79.99        Previous points balance      400
    Cash Advances                                          $0.00
    Balance
     `      Transfers                                      $0.00
    Fees Charged                                           $3.00
    Interest Charged                                       $2.00
    New Balance                                          $144.99
    Opening/Closing Date                          06/29/26 - 07/28/26
    Credit Access Line
     `                                                     $5,000
    Available Credit                                       $4,855

    YOUR ACCOUNT MESSAGES

    An example message that mentions 07/09 and an amount 55.55 in prose.

  Information About Your Account
  Making Your Payments: the amount of your payment should be at least your
  minimum payment due. We add transactions and fees to your daily balance no
  earlier than 1. the date of the transaction 07/10 for new purchases 12.00

 ACCOUNT ACTIVITY

    Date of
  Transaction                Merchant Name or Transaction Description        $ Amount
 PAYMENTS AND OTHER CREDITS

  07/05                 EXAMPLE PAYMENT THANK YOU                              -40.00

 PURCHASE

  06/28                 EXAMPLE MERCHANT ONE PLACETOWN ZZ                       25.00
  07/02                 EXAMPLE MERCHANT TWO PLACETOWN ZZ                       12.34
  07/03                 EXAMPLE FOREIGN SHOP CITYNAME XX                         9.66
                         07/04 EXAMPLE CURRENCY
                            80.00 X 0.120750000 (EXCHG RATE)


EXAMPLE CARDHOLDER                                  Page 1 of 2
0000001   AAA00000 D 3        Y  9  28  26/07/28    Page 1 of 2    00000

 YOUR ACCOUNT MESSAGES                              (CONTINUED)
 A second example message, also carrying 07/11 and 66.66 in prose.

     ACCOUNT ACTIVITY                         (CONTINUED)
        Date of
      Transaction            Merchant Name or Transaction Description      $ Amount

     07/20                EXAMPLE MERCHANT THREE PLACETOWN ZZ                 32.00
     07/22                EXAMPLE SUBSCRIPTION PLACETOWN ZZ                     .99

     FEES CHARGED

     07/03                FOREIGN TRANSACTION FEE                              3.00
                          EXAMPLE FOREIGN SHOP CITYNAME XX                    80.00
                                  TOTAL FEES FOR THIS PERIOD                  $3.00

     INTEREST CHARGED

     07/28                PURCHASE INTEREST CHARGE                             2.00
                                  TOTAL INTEREST FOR THIS PERIOD              $2.00


                                     2026 Totals Year-to-Date
                                 Total fees charged in 2026        $3.00
                                 Total interest charged in 2026    $2.00

    Table Summary

 INTEREST CHARGES

 Balance Type                    Annual Percentage Rate   Balance Subject   Interest
 PURCHASES                       empty cell               empty cell        empty cell
   Purchases                     19.99%(v)(d)             100.00            2.00
 CASH ADVANCES                   empty cell               empty cell        empty cell
   Cash Advances                 24.99%(v)(d)             -0-               -0-
 BALANCE TRANSFERS
   Balance Transfers             19.99%(v)(d)             -0-               -0-

 IMPORTANT NEWS

               An example promotional block, valid 07/01/26 - 09/30/26.
"""


def _card():
    return sp.parse_card_statement_text(CARD_STATEMENT)


def test_card_period_is_the_opening_closing_line():
    s = _card()
    assert (s.period_start, s.period_end) == (date(2026, 6, 29), date(2026, 7, 28))


def test_card_summary_survives_the_split_label_and_column_bleed():
    s = _card()
    assert s.previous_balance == Decimal("100.00")
    assert s.payments_credits == Decimal("-40.00")
    assert s.purchases == Decimal("79.99")
    assert s.cash_advances == Decimal("0.00")
    assert s.balance_transfers == Decimal("0.00")   # label split by a backtick
    assert s.fees_charged == Decimal("3.00")
    assert s.interest_charged == Decimal("2.00")
    assert s.new_balance == Decimal("144.99")
    assert sp.card_summary_reconciles(s) is True


def test_card_rows_are_signed_dated_and_sectioned():
    s = _card()
    got = [(t.posted_at.isoformat(), str(t.amount), t.kind)
           for t in s.transactions]
    assert got == [
        ("2026-07-05", "-40.00", "STMT_PAYMENT"),
        # transacted the day before the period opens — dated by proximity,
        # not by falling inside the period
        ("2026-06-28", "25.00", "STMT_PURCHASE"),
        ("2026-07-02", "12.34", "STMT_PURCHASE"),
        ("2026-07-03", "9.66", "STMT_PURCHASE"),
        # the continued page resumes PURCHASE without reprinting the heading
        ("2026-07-20", "32.00", "STMT_PURCHASE"),
        ("2026-07-22", "0.99", "STMT_PURCHASE"),    # printed as ".99"
        ("2026-07-03", "3.00", "STMT_FEE"),
        ("2026-07-28", "2.00", "STMT_INTEREST"),
    ]


def test_card_rows_carry_their_own_sign_not_a_section_sign():
    # The credits section prints its rows negative already; applying the
    # deposit `_section_sign` on top would flip them back to positive.
    s = _card()
    payments = [t for t in s.transactions if t.kind == "STMT_PAYMENT"]
    assert payments and all(t.amount < 0 for t in payments)
    assert all(t.amount > 0 for t in s.transactions
               if t.kind != "STMT_PAYMENT")


def test_card_parse_invents_no_phantom_rows():
    s = _card()
    descriptions = " | ".join(t.description for t in s.transactions)
    for phantom in ("EXCHG RATE", "EXAMPLE CURRENCY", "TOTAL FEES",
                    "TOTAL INTEREST", "example message", "Information About",
                    "Table Summary", "empty cell", "promotional",
                    "Totals Year-to-Date", "19.99"):
        assert phantom not in descriptions
    # the foreign-fee continuation ends in a bare amount but has no date
    assert not any(t.amount == Decimal("80.00") for t in s.transactions)
    assert len(s.transactions) == 8


def test_card_rows_reconcile_with_the_balance_delta():
    s = _card()
    assert sp.card_rows_reconcile(s) is True
    # The figures the fixture's header states, written out rather than
    # recomputed from the parse: 144.99 − 100.00 over eight rows.
    assert sum(t.amount for t in s.transactions) == Decimal("44.99")
    assert (s.previous_balance, s.new_balance) == (Decimal("100.00"),
                                                   Decimal("144.99"))


def test_card_rows_reconcile_checks_each_section_against_its_own_total():
    # The whole-period sum is blind to rows moving BETWEEN sections, which is
    # exactly what a dropped heading does (the rows keep the previous
    # section's kind). Re-stamp the fee row as a purchase and the period total
    # is untouched; only the per-section identities move.
    s = _card()
    fee = next(t for t in s.transactions if t.kind == "STMT_FEE")
    fee.kind = "STMT_PURCHASE"
    assert sum(t.amount for t in s.transactions) == Decimal("44.99")  # unmoved
    assert sp.card_summary_reconciles(s) is True
    assert sp.card_rows_reconcile(s) is False


def test_card_rows_reconcile_refuses_an_unparsed_section():
    # Cash advances and balance transfers have no section in the map, so a
    # non-zero figure means the statement is printing an activity section
    # nothing here reads. The rows would then be short by that amount — but
    # if it were a section that nets to zero, no total would move at all,
    # which is why the figures are asserted zero rather than merely summed.
    s = _card()
    s.cash_advances = Decimal("50.00")
    assert sp.card_rows_reconcile(s) is False
    s.cash_advances = Decimal("0.00")
    s.balance_transfers = Decimal("-0.01")
    assert sp.card_rows_reconcile(s) is False


def test_every_summary_addend_is_either_a_mapped_section_or_asserted_zero():
    # The structural invariant behind the reconciliation: a summary figure
    # that is neither checked against a section nor required to be zero would
    # be a hole in both gates. Adding a section heading without its total —
    # or a total without its section — must fail here.
    assert set(sp.CARD_SECTION_KINDS.values()) == set(sp.CARD_SECTION_TOTALS)
    # `previous_balance` is the period's opening carry, not an activity
    # total, so it is the one addend that maps to no section by construction.
    assert (set(sp.CARD_SECTION_TOTALS.values())
            | set(sp._CARD_UNMAPPED_SUMMARY_FIELDS)
            | {"previous_balance"}) == set(sp._CARD_SUMMARY_ADDENDS)


def test_card_reconcile_flags_a_dropped_row():
    bad = CARD_STATEMENT.replace(
        "     07/20                EXAMPLE MERCHANT THREE PLACETOWN ZZ                 32.00\n", "")
    s = sp.parse_card_statement_text(bad)
    assert len(s.transactions) == 7
    assert sp.card_summary_reconciles(s) is True     # the printed summary still adds up
    assert sp.card_rows_reconcile(s) is False        # but the rows no longer do


def test_card_row_with_an_impossible_date_is_dropped():
    # A row-shaped line whose MM/DD is no date in any candidate year is not a
    # row — it never becomes a transaction, and the row identity still holds
    # because the printed section totals never counted it either.
    bad = CARD_STATEMENT.replace(
        "  07/02                 EXAMPLE MERCHANT TWO PLACETOWN ZZ                       12.34\n",
        "  07/02                 EXAMPLE MERCHANT TWO PLACETOWN ZZ                       12.34\n"
        "  99/99                 EXAMPLE UNDATABLE ROW                                    1.00\n")
    s = sp.parse_card_statement_text(bad)
    assert len(s.transactions) == 8
    assert not any(t.amount == Decimal("1.00") for t in s.transactions)
    assert sp.card_rows_reconcile(s) is True


def test_card_reconcile_accepts_a_statement_with_no_activity():
    # A period with no transactions at all is a real, reconciling statement,
    # not a mis-parse: every figure is zero and the balance does not move.
    text = CARD_YEAR_WRAP.replace("$30.00", "$0.00").replace("$40.00", "$10.00")
    text = text[:text.index(" ACCOUNT ACTIVITY")]
    s = sp.parse_card_statement_text(text)
    assert s.transactions == []
    assert sp.card_summary_reconciles(s) is True
    assert sp.card_rows_reconcile(s) is True


def test_card_summary_reconcile_flags_a_wrong_figure():
    bad = CARD_STATEMENT.replace("Purchases                                            +$79.99",
                                 "Purchases                                            +$78.99")
    s = sp.parse_card_statement_text(bad)
    assert sp.card_summary_reconciles(s) is False


def test_card_summary_reconcile_is_false_when_a_figure_is_missing():
    s = sp.parse_card_statement_text("ACCOUNT SUMMARY\n  New Balance  $10.00\n")
    assert sp.card_summary_reconciles(s) is False
    assert sp.card_rows_reconcile(s) is False


CARD_YEAR_WRAP = """\
    ACCOUNT SUMMARY
    Previous Balance                                      $10.00
    Payment, Credits                                       $0.00
    Purchases                                             +$30.00
    Cash Advances                                          $0.00
    Balance Transfers                                      $0.00
    Fees Charged                                           $0.00
    Interest Charged                                       $0.00
    New Balance                                           $40.00
    Opening/Closing Date                          12/18/25 - 01/21/26

 ACCOUNT ACTIVITY
 PURCHASE

  12/16                 EXAMPLE MERCHANT BEFORE THE PERIOD                     5.00
  12/20                 EXAMPLE MERCHANT IN DECEMBER                          10.00
  01/15                 EXAMPLE MERCHANT IN JANUARY                           15.00

 INTEREST CHARGES
"""


def test_card_year_wrap_across_january():
    s = sp.parse_card_statement_text(CARD_YEAR_WRAP)
    assert [t.posted_at.isoformat() for t in s.transactions] == [
        "2025-12-16",       # transacted before the period opened, still 2025
        "2025-12-20", "2026-01-15"]
    assert sp.card_rows_reconcile(s) is True


def test_card_statement_without_a_period_yields_no_transactions():
    # Undatable MM/DD rows are not guessed at; the summary still reports.
    text = CARD_STATEMENT.replace(
        "Opening/Closing Date                          06/29/26 - 07/28/26", "")
    s = sp.parse_card_statement_text(text)
    assert (s.period_start, s.period_end) == (None, None)
    assert s.transactions == []
    assert s.new_balance == Decimal("144.99")


def test_card_period_is_found_without_an_account_summary_heading():
    # The summary blob is anchored on ACCOUNT SUMMARY; a render that loses
    # the heading still has to yield a period, or the whole statement is
    # undatable.
    text = CARD_STATEMENT.replace("    ACCOUNT SUMMARY\n", "")
    s = sp.parse_card_statement_text(text)
    assert (s.period_start, s.period_end) == (date(2026, 6, 29), date(2026, 7, 28))
    assert len(s.transactions) == 8
