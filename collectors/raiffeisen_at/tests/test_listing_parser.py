"""Unit tests for listing_parser — synthetic listings only (listing_fixtures):
the per-page column ruler, the BUTAG carry-forward, continuation lines across
page breaks and banners, the split-label repair, money and sign, the value-date
year, and every check `problems` makes."""
from __future__ import annotations

import sys
from datetime import date, datetime
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import listing_parser as lp  # noqa: E402
from listing_fixtures import Posting, ledger, money, render  # noqa: E402

D = date


def _parse(postings, **kw):
    return lp.parse_listing(render(postings, **kw))


# ============================================================
# Layout
# ============================================================

def test_each_page_is_read_against_its_own_column_ruler():
    L = ledger(days=60)
    text = L.listing_text(D(2024, 1, 1), D(2024, 3, 1),
                          money_shifts={2: 2, 3: -1}, code_shifts={3: 1, 4: 1})
    lst = lp.parse_listing(text)
    assert lp.problems(lst) == []
    assert [p.amount_cents for p in lst.postings] == [
        a for d in sorted(L.postings) for a in L.postings[d]]


def test_is_listing_needs_the_header_block():
    assert lp.is_listing(render([Posting(D(2024, 1, 2), -1_000)]))
    assert not lp.is_listing("Kontoauszug\nSome other statement\n")


def test_booking_day_carries_forward_across_a_page_break():
    postings = [Posting(D(2024, 1, 2), -1_000), Posting(D(2024, 1, 2), -2_000),
                Posting(D(2024, 1, 2), 3_000), Posting(D(2024, 1, 5), -500)]
    lst = _parse(postings, per_page=2)
    assert [p.butag for p in lst.postings] == [D(2024, 1, 2)] * 3 + [D(2024, 1, 5)]
    assert [p.saldo_cents for p in lst.postings] == [None, None, 100_000, 99_500]
    assert lst.postings[2].page == 2


def test_continuation_lines_survive_page_breaks_and_the_units_banner():
    lines = [(7, "Zahlungsempfänger: ACME GmbH"), (26, "TESTSTRASSE 1"),
             (7, "Verwendungszweck: RENT JANUARY"), (7, "Mandat: M0000 vom 01.01.20")]
    postings = [Posting(D(2024, 1, 2), -1_000, lines=list(lines)),
                Posting(D(2024, 1, 3), -2_000, lines=list(lines))]
    lst = _parse(postings, per_page=1, split_blocks=True, units_mid_block=True)
    for p in lst.postings:
        assert p.counterparty == "ACME GmbH"
        assert p.description == "RENT JANUARY"
        assert p.labelled("Zahlungsempfänger") == ("ACME GmbH", "TESTSTRASSE 1")
        assert p.labelled("Mandat") == ("M0000 vom 01.01.20",)


def test_a_label_split_across_two_lines_is_rejoined():
    lines = [(7, "DAUERAUFTRAG"), (7, "Dauerauftrag(E) zu Lasten vom 01.01. EUR 10,00Auftrag"),
             (7, "geber: TEST PAYER")]
    p = _parse([Posting(D(2024, 1, 2), -1_000, lines=lines, herk="DT")]).postings[0]
    assert p.counterparty == "TEST PAYER"
    assert p.labelled("geber") is None
    assert p.lines[-1] == "Auftraggeber: TEST PAYER"
    assert p.lines[1].endswith("EUR 10,00")


def test_description_prefers_purpose_then_reference_then_booking_text():
    rows = [
        Posting(D(2024, 1, 2), -100, herk="IZVE",
                lines=[(7, "Kundendaten: Kartenentgelt 2024"), (7, "Verwendungszweck: CARD 1")]),
        Posting(D(2024, 1, 3), -200, herk="SEUA",
                lines=[(7, "Online Banking vom 03.01 um 10:00"), (7, "Empfänger: TEST PAYEE"),
                       (7, "Zahlungsreferenz: INVOICE 7")]),
        Posting(D(2024, 1, 4), -300, herk="ABS", lines=[(7, "Kontoführung")]),
    ]
    card, transfer, closing = _parse(rows).postings
    assert card.description == "Kartenentgelt 2024 CARD 1" and card.counterparty is None
    assert transfer.description == "INVOICE 7" and transfer.counterparty == "TEST PAYEE"
    assert closing.description == "Kontoführung"


def test_money_sign_and_the_two_money_columns():
    assert lp.parse_money("1.234,56-") == -123_456
    assert lp.parse_money("0,05") == 5
    lst = _parse([Posting(D(2024, 1, 2), -123_456), Posting(D(2024, 1, 2), 5)],
                 opening=1_000_000_00)
    assert [p.amount_cents for p in lst.postings] == [-123_456, 5]
    assert lst.postings[1].saldo_cents == 1_000_000_00 - 123_451


def test_a_negative_balance_printed_against_the_statement_date():
    p = _parse([Posting(D(2024, 1, 2), -200_000, druck=D(2024, 1, 31))],
               opening=100_000).postings[0]
    assert p.saldo_cents == -100_000 and p.druckdatum == D(2024, 1, 31)


def test_value_date_takes_its_year_from_the_booking_day():
    assert lp.value_date(D(2023, 12, 30), "02.01") == D(2024, 1, 2)
    assert lp.value_date(D(2024, 1, 2), "29.12") == D(2023, 12, 29)
    assert lp.value_date(D(2024, 5, 31), "03.06") == D(2024, 6, 3)
    p = _parse([Posting(D(2023, 12, 30), -1_000, val="02.01")]).postings[0]
    assert p.valuta == D(2024, 1, 2)


def test_statement_print_day_is_blank_until_printed():
    lst = _parse([Posting(D(2024, 1, 2), -1_000, druck=D(2024, 1, 31)),
                  Posting(D(2024, 2, 20), -1_000)])
    assert [p.druckdatum for p in lst.postings] == [D(2024, 1, 31), None]


def test_header_fields():
    lst = _parse([Posting(D(2024, 1, 2), -1_000)], coverage_start=D(2024, 1, 1),
                 printed=datetime(2024, 2, 1, 14, 5))
    assert (lst.account_number, lst.currency, lst.coverage_start) == ("1234", "EUR", D(2024, 1, 1))
    assert lst.printed_at == datetime(2024, 2, 1, 14, 5) and lst.print_day == D(2024, 2, 1)
    assert (lst.selection, lst.key_text, lst.shows_balance, lst.shows_text) == (
        "A", "alle Umsätze", True, True)
    assert (lst.stated_debits_cents, lst.stated_credits_cents) == (1_000, 0)


def test_opening_balance_is_the_first_balance_less_that_days_postings():
    lst = _parse([Posting(D(2024, 1, 2), -1_000), Posting(D(2024, 1, 2), 300)],
                 opening=50_000)
    assert lst.opening_balance() == 50_000
    assert lst.closing_balances() == {D(2024, 1, 2): 49_300}


# ============================================================
# Layout errors
# ============================================================

def test_pages_that_name_different_accounts_are_unreadable():
    postings = [Posting(D(2024, 1, 2 + i), -100) for i in range(4)]
    with pytest.raises(lp.ListingError, match="account_number"):
        _parse(postings, per_page=2, accounts_per_page={2: "9999"})


def test_missing_pages_are_unreadable():
    postings = [Posting(D(2024, 1, 2 + i), -100) for i in range(4)]
    with pytest.raises(lp.ListingError, match="pages missing"):
        _parse(postings, per_page=2, page_numbers=[1, 3, 4])


def test_a_continuation_line_is_never_mistaken_for_a_row():
    lines = [(7, "Verwendungszweck: PAYMENT"), (21, "REF 01.02 12,34")]
    p = _parse([Posting(D(2024, 1, 2), -1_000, lines=lines)]).postings
    assert len(p) == 1 and p[0].description == "PAYMENT REF 01.02 12,34"


# ============================================================
# problems()
# ============================================================

def test_a_clean_listing_has_no_problems():
    assert lp.problems(ledger().listing(D(2024, 1, 1), D(2024, 6, 1))) == []


@pytest.mark.parametrize("kw, expect", [
    ({"selection": "S"}, "all postings"),
    ({"key_text": "Gutschriften"}, "text key"),
    ({"show_balance": False}, "without closing balances"),
])
def test_an_incomplete_selection_is_refused(kw, expect):
    lst = _parse([Posting(D(2024, 1, 2), -1_000)], **kw)
    assert any(expect in p for p in lp.problems(lst))


def test_a_broken_balance_chain_names_its_day():
    postings = [Posting(D(2024, 1, 2 + i), -1_000) for i in range(5)]
    lst = _parse(postings, saldo_offsets={D(2024, 1, 4): 1})
    assert lp.problems(lst) == ["the balance chain breaks on 2024-01-04 (page 1)"]


def test_totals_must_net_exactly():
    postings = [Posting(D(2024, 1, 2), -1_000), Posting(D(2024, 1, 3), 500)]
    assert lp.problems(_parse(postings, totals=(1_000, 501))) == [
        "postings do not net to the stated totals"]
    assert "truncated" in lp.problems(_parse(postings, totals=None))[0]


def test_a_same_day_reversal_explains_a_gross_residual():
    # debit, its reversal and the re-booking: the bank's totals leave the pair out
    postings = [Posting(D(2024, 1, 2), -9_000), Posting(D(2024, 1, 2), 9_000),
                Posting(D(2024, 1, 2), -9_000), Posting(D(2024, 1, 3), 1_000)]
    assert lp.problems(_parse(postings, totals=(9_000, 1_000))) == []
    # the same residual with no equal-and-opposite pair behind it
    postings = [Posting(D(2024, 1, 2), -9_000), Posting(D(2024, 1, 3), 1_000),
                Posting(D(2024, 1, 4), -1_000), Posting(D(2024, 1, 5), 1_000)]
    assert "reversals" in lp.problems(_parse(postings, totals=(9_000, 1_000)))[0]


# ============================================================
# Ids
# ============================================================

def test_ids_ignore_the_statement_print_day_and_count_identical_postings():
    twice = [Posting(D(2024, 1, 2), -1_000), Posting(D(2024, 1, 2), -1_000)]
    early = _parse(twice).postings
    later = _parse([Posting(D(2024, 1, 2), -1_000, druck=D(2024, 1, 31)),
                    Posting(D(2024, 1, 2), -1_000, druck=D(2024, 1, 31))]).postings
    assert lp.occurrences(early) == [0, 1]
    ids = [lp.txn_id("X", p, n) for p, n in zip(early, lp.occurrences(early))]
    assert len(set(ids)) == 2 and all(i.startswith("doc_") for i in ids)
    assert ids == [lp.txn_id("X", p, n) for p, n in zip(later, lp.occurrences(later))]


def test_ids_ignore_the_parsed_text():
    a = _parse([Posting(D(2024, 1, 2), -1_000, lines=[(7, "Verwendungszweck: A")])]).postings[0]
    b = _parse([Posting(D(2024, 1, 2), -1_000, lines=[(7, "Verwendungszweck: B")])]).postings[0]
    assert lp.txn_id("X", a, 0) == lp.txn_id("X", b, 0)


def test_money_fixture_matches_the_parser():
    for cents in (0, 5, 100, -123_456, 1_234_567_89):
        assert lp.parse_money(money(cents)) == cents


def test_a_date_that_names_no_day_is_a_layout_error():
    text = render([Posting(D(2024, 1, 2), -1_000)], printed=datetime(2024, 2, 28, 9))
    with pytest.raises(lp.ListingError):
        lp.parse_listing(text.replace("28.02.2024/", "30.02.2024/"))
