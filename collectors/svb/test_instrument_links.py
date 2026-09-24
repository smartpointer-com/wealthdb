"""Tests for instrument_links — the quantity proof behind a trade's link.

Every fixture is synthetic: example tickers, round quantities, and
statement periods in 2099 so no date can coincide with a real one.
"""
from datetime import date

import pytest

import instrument_links as il

ACCOUNT = "SVM-000000"
EXAMPLE = "EXAMPLE COMPANY CL A"
OTHER = "OTHER EXAMPLE INC COM"


def _st(end, *, start=None, holdings=(), moves=(), opens_empty=False,
        account=ACCOUNT):
    """One statement. ``holdings=None`` is a statement that lost its table."""
    return il.Statement(
        account=account,
        start=date.fromisoformat(start) if start else None,
        end=date.fromisoformat(end),
        holdings=None if holdings is None else tuple(
            il.Holding(k, n, q) for k, n, q in holdings),
        opens_empty=opens_empty,
        movements=tuple(il.Movement(*m) for m in moves))


def _jan(holdings=(), moves=()):
    """A January statement that opens empty, as an account's first does."""
    return _st("2099-01-31", start="2099-01-01", holdings=holdings,
               moves=moves, opens_empty=True)


def _feb(holdings=(), moves=(), start="2099-02-01"):
    return _st("2099-02-28", start=start, holdings=holdings, moves=moves)


def test_a_name_and_a_closing_quantity_link():
    links = il.link([_jan(holdings=[("AAAA", EXAMPLE, 100)],
                          moves=[("buy", EXAMPLE, 100)])])
    assert links.keys == {"buy": "AAAA"}
    assert links.unlinked == {}


def test_trade_notes_and_kerning_do_not_stop_a_link():
    # The blotter kerns a name and appends its execution notes; both
    # survive the whitespace-free prefix comparison.
    links = il.link([_jan(
        holdings=[("AAAA", EXAMPLE, 30)],
        moves=[("a", "EXAM PLE COM PANY CL A @ 40.00", 10),
               ("b", "EXAMPLE COMPANY CL A AVERAGE PRICE TRADE", 20)])])
    assert links.keys == {"a": "AAAA", "b": "AAAA"}


def test_a_truncated_name_links_to_the_one_holding_it_prefixes():
    links = il.link([_jan(holdings=[("AAAA", EXAMPLE, 5)],
                          moves=[("a", "EXAMPLE COMP", 5)])])
    assert links.keys == {"a": "AAAA"}


def test_a_name_too_short_to_be_a_prefix_must_match_whole():
    links = il.link([_jan(holdings=[("AAAA", "EXA", 5), ("BBBB", EXAMPLE, 5)],
                          moves=[("a", "EXA", 5), ("b", "EXAMPLE", 5)])])
    assert links.keys == {"a": "AAAA", "b": "BBBB"}


def test_two_candidate_keys_are_told_apart_by_quantity():
    # A fund family's shared prefix matches both holdings; only one
    # assignment makes both quantities close.
    links = il.link([_jan(
        holdings=[("AAAA", "EXAMPLE FUNDS GROWTH ETF", 70),
                  ("BBBB", "EXAMPLE FUNDS VALUE ETF", 30)],
        moves=[("x", "EXAMPLE FUNDS", 70), ("y", "EXAMPLE FUNDS", 30)])])
    assert links.keys == {"x": "AAAA", "y": "BBBB"}


def test_two_candidate_keys_with_one_change_are_not_forced():
    links = il.link([_jan(
        holdings=[("AAAA", "EXAMPLE FUNDS GROWTH ETF", 50),
                  ("BBBB", "EXAMPLE FUNDS VALUE ETF", 50)],
        moves=[("x", "EXAMPLE FUNDS", 50), ("y", "EXAMPLE FUNDS", 50)])])
    assert links.keys == {}
    assert {r for r, _ in links.unlinked.values()} == {il.NOT_FORCED}


def test_rows_that_cancel_are_not_forced_but_the_rest_link():
    # Held throughout: +10 then -10 could as well be another security
    # bought and sold inside the month, so only the +25 is proved.
    links = il.link([
        _jan(holdings=[("AAAA", EXAMPLE, 100)], moves=[("open", EXAMPLE, 100)]),
        _feb(holdings=[("AAAA", EXAMPLE, 125)],
             moves=[("in", EXAMPLE, 10), ("out", EXAMPLE, -10),
                    ("add", EXAMPLE, 25)])])
    assert links.keys == {"open": "AAAA", "add": "AAAA"}
    assert links.unlinked == {"in": (il.NOT_FORCED, "EXAMPLECOMPANYCLA"),
                              "out": (il.NOT_FORCED, "EXAMPLECOMPANYCLA")}


def test_a_row_the_quantities_leave_out_is_not_forced():
    # 100 of the 150 bought reached the closing, and only the 100-row
    # sums to it: the 50-row is equally some other security's.
    links = il.link([_jan(holdings=[("AAAA", EXAMPLE, 100)],
                          moves=[("a", EXAMPLE, 100), ("b", EXAMPLE, 50)])])
    assert links.keys == {"a": "AAAA"}
    assert links.unlinked["b"][0] == il.NOT_FORCED


def test_a_security_never_held_has_no_candidate_and_states_its_name():
    links = il.link([_jan(holdings=[("AAAA", EXAMPLE, 10)],
                          moves=[("a", EXAMPLE, 10),
                                 ("rt1", OTHER, 40), ("rt2", OTHER, -40)])])
    assert links.keys == {"a": "AAAA"}
    assert links.unlinked == {"rt1": (il.NO_CANDIDATE, "OTHEREXAMPLEINCCOM"),
                              "rt2": (il.NO_CANDIDATE, "OTHEREXAMPLEINCCOM")}


def test_a_key_short_of_a_row_links_none_of_its_rows():
    links = il.link([_jan(holdings=[("AAAA", EXAMPLE, 100)],
                          moves=[("a", EXAMPLE, 60), ("b", EXAMPLE, 30)])])
    assert links.keys == {}
    assert {r for r, _ in links.unlinked.values()} == {il.DOES_NOT_CLOSE}


def test_a_missing_month_leaves_the_window_without_an_opening():
    links = il.link([
        _jan(holdings=[("AAAA", EXAMPLE, 10)], moves=[("a", EXAMPLE, 10)]),
        _st("2099-03-31", start="2099-03-01", holdings=[("AAAA", EXAMPLE, 15)],
            moves=[("b", EXAMPLE, 5)])])
    assert links.keys == {"a": "AAAA"}
    assert links.unlinked == {"b": (il.NO_BRACKET, "EXAMPLECOMPANYCLA")}


def test_a_lost_holdings_table_leaves_both_neighbouring_windows_unproved():
    links = il.link([
        _jan(holdings=[("AAAA", EXAMPLE, 10)], moves=[("a", EXAMPLE, 10)]),
        _feb(holdings=None, moves=[("b", EXAMPLE, 5)]),
        _st("2099-03-31", start="2099-03-01", holdings=[("AAAA", EXAMPLE, 20)],
            moves=[("c", EXAMPLE, 5)])])
    assert links.keys == {"a": "AAAA"}
    assert {k: r for k, (r, _) in links.unlinked.items()} == {
        "b": il.NO_BRACKET, "c": il.NO_BRACKET}


def test_a_stated_zero_opening_needs_no_predecessor():
    stated = _st("2099-03-31", start="2099-03-01", opens_empty=True,
                 holdings=[("AAAA", EXAMPLE, 5)], moves=[("a", EXAMPLE, 5)])
    unstated = _st("2099-03-31", start="2099-03-01", account="SVM-000001",
                   holdings=[("AAAA", EXAMPLE, 5)], moves=[("b", EXAMPLE, 5)])
    links = il.link([stated, unstated])
    assert links.keys == {"a": "AAAA"}
    assert links.unlinked["b"][0] == il.NO_BRACKET


def test_a_contiguous_predecessor_is_the_opening_before_a_stated_zero():
    links = il.link([
        _jan(holdings=[("AAAA", EXAMPLE, 10)], moves=[("a", EXAMPLE, 10)]),
        _st("2099-02-28", start="2099-02-01", opens_empty=True,
            holdings=[("AAAA", EXAMPLE, 15)], moves=[("b", EXAMPLE, 5)])])
    assert links.keys == {"a": "AAAA", "b": "AAAA"}


def test_option_legs_sharing_one_name_link_by_their_stated_code():
    name = "CALL (AAAA) EXAMPLE COMPANY CL A"
    links = il.link([_jan(
        holdings=[("AAAA990115C50", name, -2), ("AAAA990115C60", name, -2)],
        moves=[("c50", name, -2, "AAAA990115C50"),
               ("c60", name, -2, "AAAA990115C60")])])
    assert links.keys == {"c50": "AAAA990115C50", "c60": "AAAA990115C60"}


def test_a_stated_code_outside_the_window_is_its_hint():
    name = "PUT (AAAA) EXAMPLE COMPANY CL A"
    links = il.link([_jan(holdings=[("AAAA", EXAMPLE, 1)],
                          moves=[("s", EXAMPLE, 1),
                                 ("p", name, 3, "AAAA990115P40"),
                                 ("q", name, -3, "AAAA990115P40")])])
    assert links.unlinked == {"p": (il.NO_CANDIDATE, "AAAA990115P40"),
                              "q": (il.NO_CANDIDATE, "AAAA990115P40")}


def test_a_stated_code_is_linked_though_its_rows_cancel():
    # A name alone could not be forced through a buy and sell that
    # cancel; a row that names its contract is its own evidence.
    name = "CALL (AAAA) EXAMPLE COMPANY CL A"
    links = il.link([_jan(
        holdings=[("AAAA990115C50", name, -2)],
        moves=[("open", name, -2, "AAAA990115C50"),
               ("in", name, -1, "AAAA990115C50"),
               ("out", name, 1, "AAAA990115C50")])])
    assert set(links.keys) == {"open", "in", "out"}


def test_a_reversal_leaves_the_proof_with_the_row_it_reverses():
    # A booking, its cancellation and the fill booked again: by the
    # quantities alone either buy could be the one that stands.
    links = il.link([_jan(
        holdings=[("AAAA", EXAMPLE, 100)],
        moves=[("buy", EXAMPLE, 100), ("cancel", EXAMPLE, -100, None, "buy"),
               ("rebuy", EXAMPLE, 100)])])
    assert links.keys == {"rebuy": "AAAA"}
    assert links.unlinked == {}


def test_a_later_reversal_carries_its_rows_key_into_a_window_without_it():
    # Sold in February and held by no statement after; the sale is
    # cancelled and booked again in March, whose holdings never name it.
    links = il.link([
        _jan(holdings=[("AAAA", EXAMPLE, 100)], moves=[("buy", EXAMPLE, 100)]),
        _feb(holdings=[], moves=[("sell", EXAMPLE, -100)]),
        _st("2099-03-31", start="2099-03-01", holdings=[],
            moves=[("cancel", "EXAM PLE COMPANY", 100, None, "sell"),
                   ("resell", "EXAMPLE COMPANY CL A @ 12.00", -100)])])
    assert links.keys == {"buy": "AAAA", "sell": "AAAA", "cancel": "AAAA",
                          "resell": "AAAA"}


def test_a_reversal_of_an_unlinked_row_proves_nothing():
    links = il.link([
        _st("2099-01-31", start="2099-01-01",
            holdings=[("AAAA", EXAMPLE, 100)], moves=[("buy", EXAMPLE, 100)]),
        _feb(holdings=[("AAAA", EXAMPLE, 100)],
             moves=[("cancel", EXAMPLE, -100, None, "buy"),
                    ("rebuy", EXAMPLE, 100)])])
    assert links.keys == {}
    assert links.unlinked["rebuy"] == (il.NOT_FORCED, "EXAMPLECOMPANYCLA")


def test_a_search_over_budget_links_nothing(monkeypatch):
    monkeypatch.setattr(il, "_MAX_NODES", 1)
    links = il.link([_jan(
        holdings=[("AAAA", "EXAMPLE FUNDS GROWTH ETF", 70),
                  ("BBBB", "EXAMPLE FUNDS VALUE ETF", 30)],
        moves=[("x", "EXAMPLE FUNDS", 70), ("y", "EXAMPLE FUNDS", 30)])])
    assert links.keys == {}
    assert {r for r, _ in links.unlinked.values()} == {il.TOO_MANY}


def test_a_statement_filed_twice_keeps_the_first_copys_links():
    jan = _jan(holdings=[("AAAA", EXAMPLE, 10)], moves=[("a", EXAMPLE, 10)])
    links = il.link([jan, jan])
    assert links.keys == {"a": "AAAA"}
    assert links.unlinked == {}


@pytest.mark.parametrize("quantities", [(1.5, 2.25), (0.001, 0.002)])
def test_fractional_quantities_close_exactly(quantities):
    a, b = quantities
    links = il.link([_jan(holdings=[("BBBB", "EXAMPLE INDEX ETF", a + b)],
                          moves=[("a", "EXAMPLE INDEX ETF", a),
                                 ("b", "EXAMPLE INDEX ETF", b)])])
    assert links.keys == {"a": "BBBB", "b": "BBBB"}
