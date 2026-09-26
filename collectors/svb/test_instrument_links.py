"""Tests for instrument_links — the quantity proof behind a trade's link,
and the name pass behind a dividend's.

Every fixture is synthetic: example tickers, round quantities, and
statement periods in 2099 so no date can coincide with a real one.
"""
from datetime import date, timedelta

import pytest

import instrument_links as il

ACCOUNT = "SVM-000000"
EXAMPLE = "EXAMPLE COMPANY CL A"
OTHER = "OTHER EXAMPLE INC COM"


def _st(end, *, start=None, holdings=(), moves=(), opens_empty=False,
        account=ACCOUNT, named=()):
    """One statement. ``holdings=None`` is a statement that lost its table.
    A named row is ``(ref, name)``, dated mid-period."""
    end = date.fromisoformat(end)
    return il.Statement(
        account=account,
        start=date.fromisoformat(start) if start else None,
        end=end,
        holdings=None if holdings is None else tuple(
            il.Holding(k, n, q) for k, n, q in holdings),
        opens_empty=opens_empty,
        movements=tuple(il.Movement(*m) for m in moves),
        named=tuple(il.Named(r, n, end.replace(day=15)) for r, n in named))


def _jan(holdings=(), moves=(), named=()):
    """A January statement that opens empty, as an account's first does."""
    return _st("2099-01-31", start="2099-01-01", holdings=holdings,
               moves=moves, opens_empty=True, named=named)


def _feb(holdings=(), moves=(), start="2099-02-01", named=()):
    return _st("2099-02-28", start=start, holdings=holdings, moves=moves,
               named=named)


def _month(month, holdings=(), moves=(), named=(), contiguous=True):
    """A 2099 statement of the given month, contiguous with the one before
    it unless it says otherwise (it then starts a day late)."""
    last = {2: 28, 4: 30, 6: 30, 9: 30, 11: 30}.get(month, 31)
    return _st(f"2099-{month:02d}-{last}",
               start=f"2099-{month:02d}-{'01' if contiguous else '02'}",
               holdings=holdings, moves=moves, named=named)


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


# ---- the name pass ---------------------------------------------------------

CLASS_A = "EXAMPLE COMPANY CL A"
CLASS_B = "EXAMPLE COMPANY CL B"
ISSUER = "EXAMPLE COMPANY"


def _day(iso):
    return date.fromisoformat(iso)


@pytest.mark.parametrize("printed", [
    EXAMPLE,                       # the holding's own name
    "EXAMPLE COMP",                # cut at the line width
    "EXAM PLE COMPANY CL A",       # kerned
    "EXAMPLE COMPANY CL A COM USD0.01",  # longer than the holding's
])
def test_a_dividend_links_to_the_one_holding_its_name_fits(printed):
    links = il.link([_jan(holdings=[("AAAA", EXAMPLE, 10)],
                          named=[("div", printed)])])
    assert links.keys == {"div": "AAAA"}
    assert links.named == {"div"}


def test_a_name_too_short_to_be_a_prefix_finds_no_holding():
    links = il.link([_jan(holdings=[("AAAA", "EXAMPLE CO", 10)],
                          named=[("div", "EXA")])])
    assert links.unlinked == {"div": (il.NO_CANDIDATE, "EXA")}


def test_a_security_sold_inside_the_period_is_found_at_its_opening():
    links = il.link([
        _jan(holdings=[("AAAA", EXAMPLE, 10)]),
        _feb(holdings=[], named=[("div", EXAMPLE)])])
    assert links.keys == {"div": "AAAA"}


def test_two_holdings_the_name_fits_leave_dividend_and_withholding_alike():
    links = il.link([_jan(
        holdings=[("AAAA", CLASS_A, 10), ("BBBB", CLASS_B, 10)],
        named=[("div", ISSUER), ("tax", ISSUER)])])
    assert links.keys == {}
    assert links.unlinked == {"div": (il.SEVERAL_FIT, "EXAMPLECOMPANY"),
                              "tax": (il.SEVERAL_FIT, "EXAMPLECOMPANY")}


def test_a_withholding_cut_shorter_lands_on_its_dividends_key():
    links = il.link([_jan(holdings=[("AAAA", CLASS_A, 10)],
                          named=[("div", CLASS_A),
                                 ("tax", "EXAMPLE COMPANY CL")])])
    assert links.keys == {"div": "AAAA", "tax": "AAAA"}


def test_a_key_once_held_beside_a_fitting_sibling_is_refused():
    # Class B left the account in February; a March dividend printed under
    # the issuer's name alone could be its late payment as well as class A's.
    links = il.link([
        _month(1, holdings=[("AAAA", CLASS_A, 10), ("BBBB", CLASS_B, 10)]),
        _month(2, holdings=[("AAAA", CLASS_A, 10)]),
        _month(3, holdings=[("AAAA", CLASS_A, 10)], named=[("div", ISSUER)])])
    assert links.unlinked == {"div": (il.SIBLING_HELD, "EXAMPLECOMPANY")}


def test_a_fitting_key_held_only_in_another_era_is_no_sibling():
    # A ticker change: never held at the same statement end as the key the
    # window holds, so it cannot be the dividend's source.
    links = il.link([
        _month(1, holdings=[("BBBB", ISSUER, 10)]),
        _month(2, holdings=[]),
        _month(3, holdings=[("AAAA", ISSUER, 10)]),
        _month(4, holdings=[("AAAA", ISSUER, 10)], named=[("div", ISSUER)])])
    assert links.keys == {"div": "AAAA"}


_FEBRUARY_SALE = _day("2099-02-10")


def _bought_and_sold(key="AAAA", sale=_FEBRUARY_SALE):
    """``key`` bought in January and sold on ``sale`` in February under
    EXAMPLE's name, both proved."""
    return [
        _jan(holdings=[(key, EXAMPLE, 10)],
             moves=[("buy", EXAMPLE, 10, None, None, _day("2099-01-05"))]),
        _feb(holdings=[], moves=[("sell", EXAMPLE, -10, None, None, sale)])]


def test_a_dividend_paid_after_the_sale_finds_the_proven_trades_key():
    links = il.link([*_bought_and_sold(),
                     _month(3, holdings=[], named=[("div", EXAMPLE)])])
    assert links.keys == {"buy": "AAAA", "sell": "AAAA", "div": "AAAA"}


def test_a_proven_trade_past_the_pay_lag_does_not_decide():
    links = il.link([*_bought_and_sold(),
                     _month(12, holdings=[], named=[("div", EXAMPLE)],
                            contiguous=False)])
    assert links.unlinked == {"div": (il.TOO_OLD, "EXAMPLECOMPANYCLA")}


def test_two_proven_keys_the_name_fits_decide_nothing():
    links = il.link([
        _jan(holdings=[("AAAA", CLASS_A, 10), ("BBBB", CLASS_B, 10)],
             moves=[("a", CLASS_A, 10, None, None, _day("2099-01-05")),
                    ("b", CLASS_B, 10, None, None, _day("2099-01-05"))]),
        _feb(holdings=[],
             moves=[("sa", CLASS_A, -10, None, None, _day("2099-02-10")),
                    ("sb", CLASS_B, -10, None, None, _day("2099-02-10"))]),
        _month(3, holdings=[], named=[("div", ISSUER)])])
    assert links.unlinked["div"] == (il.SEVERAL_FIT, "EXAMPLECOMPANY")


def test_proven_trades_that_moved_only_another_key_contradict_the_holding():
    # One security under a CUSIP until February and a ticker from March,
    # the ticker never traded: the statements disagree with themselves.
    links = il.link([
        *_bought_and_sold("000000AA0"),
        _month(3, holdings=[("AAAA", EXAMPLE, 10)], named=[("div", EXAMPLE)])])
    assert links.unlinked["div"] == (il.NAMES_CONTRADICT, "EXAMPLECOMPANYCLA")


def test_a_held_key_among_the_proven_ones_links_through_a_rename():
    links = il.link([
        *_bought_and_sold("000000AA0"),
        _month(3, holdings=[("AAAA", EXAMPLE, 10)],
               moves=[("rebuy", EXAMPLE, 10, None, None, _day("2099-03-05"))],
               named=[("div", EXAMPLE)])])
    assert links.keys["rebuy"] == "AAAA"
    assert links.keys["div"] == "AAAA"


def test_one_name_under_two_keys_in_two_eras_links_each_to_its_own():
    links = il.link([
        _jan(holdings=[("AAAA", EXAMPLE, 10)], named=[("jan", EXAMPLE)]),
        _month(7, holdings=[("BBBB", EXAMPLE, 10)], named=[("jul", EXAMPLE)],
               contiguous=False)])
    assert links.keys == {"jan": "AAAA", "jul": "BBBB"}


def test_either_known_end_of_a_window_is_enough():
    unknown_opening = _month(3, holdings=[("AAAA", EXAMPLE, 10)],
                             named=[("mar", EXAMPLE)], contiguous=False)
    lost_closing = [_jan(holdings=[("AAAA", EXAMPLE, 10)]),
                    _feb(holdings=None, named=[("feb", EXAMPLE)])]
    links = il.link([unknown_opening, *lost_closing])
    assert links.keys == {"mar": "AAAA", "feb": "AAAA"}


def test_a_window_with_neither_end_known_and_no_proven_trade_finds_nothing():
    links = il.link([_st("2099-03-31", holdings=None,
                         named=[("div", EXAMPLE)])])
    assert links.unlinked == {"div": (il.NO_CANDIDATE, "EXAMPLECOMPANYCLA")}


def test_a_statement_filed_twice_keeps_the_first_copys_named_outcomes():
    # Its second copy has no contiguous predecessor, so its window is the
    # closing alone — narrower, and without the key that made it ambiguous.
    feb = _feb(holdings=[("AAAA", CLASS_A, 10)], named=[("div", ISSUER)])
    links = il.link([_jan(holdings=[("BBBB", CLASS_B, 10)]), feb, feb])
    assert links.keys == {}
    assert links.unlinked == {"div": (il.SEVERAL_FIT, "EXAMPLECOMPANY")}


def test_named_rows_leave_the_proof_alone():
    statements = [
        _jan(holdings=[("AAAA", EXAMPLE, 100)],
             moves=[("open", EXAMPLE, 100)]),
        _feb(holdings=[("AAAA", EXAMPLE, 125)],
             moves=[("in", EXAMPLE, 10), ("out", EXAMPLE, -10),
                    ("add", EXAMPLE, 25)])]
    alone = il.link(statements)
    beside = il.link([statements[0],
                      _feb(holdings=[("AAAA", EXAMPLE, 125)],
                           moves=[("in", EXAMPLE, 10), ("out", EXAMPLE, -10),
                                  ("add", EXAMPLE, 25)],
                           named=[("div", EXAMPLE)])])
    assert {k: v for k, v in beside.keys.items() if k != "div"} == alone.keys
    assert beside.unlinked == alone.unlinked
    assert beside.keys["div"] == "AAAA"


def test_the_money_fund_is_a_holding_like_any_other():
    fund = "EXAMPLE GOVERNMENT MONEY MARKET"
    links = il.link([_jan(
        holdings=[("XXXXX", fund, 1000), ("AAAA", EXAMPLE, 1)],
        named=[("div", fund)])])
    assert links.keys == {"div": "XXXXX"}


def test_the_census_reads_each_pass_apart():
    links = il.link([_jan(
        holdings=[("AAAA", EXAMPLE, 10)],
        moves=[("buy", EXAMPLE, 10), ("rt", OTHER, 5), ("rt2", OTHER, -5)],
        named=[("div", EXAMPLE), ("odd", OTHER)])])
    assert (links.linked(), links.linked(named=True)) == (1, 1)
    assert links.census() == {il.NO_CANDIDATE: 2}
    assert links.census(named=True) == {il.NO_CANDIDATE: 1}


def test_a_withholding_cut_shorter_is_judged_on_its_own():
    # Cut to the class boundary, it fits both classes the dividend's full
    # name tells apart, so it is refused where the dividend links.
    links = il.link([_jan(
        holdings=[("AAAA", CLASS_A, 10), ("BBBB", CLASS_B, 10)],
        named=[("div", CLASS_A), ("tax", "EXAMPLE COMPANY CL")])])
    assert links.keys == {"div": "AAAA"}
    assert links.unlinked == {"tax": (il.SEVERAL_FIT, "EXAMPLECOMPANYCL")}


def test_a_sibling_counts_only_under_a_name_it_held_beside_the_key():
    # Class B stood beside class A only under its own class's name; the
    # issuer's bare name it carried later, alone, makes it no sibling.
    links = il.link([
        _month(1, holdings=[("AAAA", CLASS_A, 10), ("BBBB", CLASS_B, 10)]),
        _month(2, holdings=[("BBBB", ISSUER, 10)]),
        _month(4, holdings=[("AAAA", CLASS_A, 10)], named=[("div", CLASS_A)],
               contiguous=False)])
    assert links.keys == {"div": "AAAA"}


def test_a_proven_key_once_held_beside_a_fitting_sibling_is_refused():
    links = il.link([
        _jan(holdings=[("AAAA", CLASS_A, 10), ("BBBB", CLASS_B, 10)],
             moves=[("a", CLASS_A, 10, None, None, _day("2099-01-05")),
                    ("b", CLASS_B, 10, None, None, _day("2099-01-05"))]),
        _feb(holdings=[("AAAA", CLASS_A, 10)],
             moves=[("sb", CLASS_B, -10, None, None, _day("2099-02-10"))]),
        _month(9, holdings=[("AAAA", CLASS_A, 10)], contiguous=False),
        _month(10, holdings=[],
               moves=[("sa", CLASS_A, -10, None, None, _day("2099-10-10"))]),
        _month(11, holdings=[], named=[("div", ISSUER)])])
    assert links.keys["sa"] == "AAAA"
    assert links.unlinked == {"div": (il.SIBLING_HELD, "EXAMPLECOMPANY")}


def test_a_sibling_found_under_any_name_it_carried_beside_the_key():
    # Renamed at the second end, class B still stood beside class A under
    # a fitting name at the first.
    links = il.link([
        _month(1, holdings=[("AAAA", CLASS_A, 10), ("BBBB", CLASS_B, 10)]),
        _month(2, holdings=[("AAAA", CLASS_A, 10),
                            ("BBBB", "RENAMED HOLDING", 10)]),
        _month(3, holdings=[("AAAA", CLASS_A, 10)]),
        _month(4, holdings=[("AAAA", CLASS_A, 10)], named=[("div", ISSUER)])])
    assert links.unlinked == {"div": (il.SIBLING_HELD, "EXAMPLECOMPANY")}


def test_a_key_is_found_under_any_name_either_end_prints():
    links = il.link([
        _jan(holdings=[("AAAA", EXAMPLE, 10)]),
        _feb(holdings=[("AAAA", "RENAMED EXAMPLE HOLDING", 10)],
             named=[("div", EXAMPLE)])])
    assert links.keys == {"div": "AAAA"}


def test_a_key_first_seen_after_the_dividend_does_not_decide_it():
    # Evidence that postdates the row is no pay lag, and not "too old"
    # either: nothing was ever seen before it.
    links = il.link([
        _jan(holdings=[], named=[("div", EXAMPLE)]),
        _month(2, holdings=[]),
        _month(3, holdings=[("AAAA", EXAMPLE, 10)],
               moves=[("buy", EXAMPLE, 10, None, None, _day("2099-03-05"))])])
    assert links.keys == {"buy": "AAAA"}
    assert links.unlinked == {"div": (il.NO_CANDIDATE, "EXAMPLECOMPANYCLA")}


def test_proven_trades_past_the_pay_lag_do_not_contradict_the_holding():
    links = il.link([
        *_bought_and_sold("BBBB"),
        _month(12, holdings=[("AAAA", EXAMPLE, 10)], named=[("div", EXAMPLE)],
               contiguous=False)])
    assert links.keys["div"] == "AAAA"


def test_a_holding_at_a_statement_end_dates_the_proven_key():
    # Proved in January, then held at July's end with no proven exit: July
    # is what puts the key within the pay lag of a September dividend.
    links = il.link([
        _jan(holdings=[("AAAA", EXAMPLE, 10)],
             moves=[("buy", EXAMPLE, 10, None, None, _day("2099-01-05"))]),
        _month(7, holdings=[("AAAA", EXAMPLE, 10)], contiguous=False),
        _month(9, holdings=[], named=[("div", EXAMPLE)], contiguous=False)])
    assert links.keys["div"] == "AAAA"


@pytest.mark.parametrize("lag, linked", [(183, True), (184, False)])
def test_the_pay_lag_is_a_half_year(lag, linked):
    sale = _day("2099-02-10")
    paid = sale + timedelta(days=lag)
    links = il.link([
        *_bought_and_sold(sale=sale),
        il.Statement(account=ACCOUNT, start=None, end=paid + timedelta(days=5),
                     holdings=(), opens_empty=False, movements=(),
                     named=(il.Named("div", EXAMPLE, paid),))])
    assert ("div" in links.keys) is linked
    if not linked:
        assert links.unlinked["div"][0] == il.TOO_OLD


def test_neither_end_known_leaves_the_proven_trades_to_decide():
    links = il.link([
        *_bought_and_sold(),
        _st("2099-04-30", start="2099-04-02", holdings=None,
            named=[("div", EXAMPLE)])])
    assert links.keys["div"] == "AAAA"


def test_a_trade_proven_in_a_later_window_names_an_earlier_dividends_key():
    links = il.link([
        _jan(holdings=[("AAAA", EXAMPLE, 10)]),
        _month(2, holdings=[], named=[("div", EXAMPLE)], contiguous=False),
        _st("2099-04-30", start="2099-04-01", opens_empty=True,
            holdings=[("AAAA", EXAMPLE, 10)],
            moves=[("buy", EXAMPLE, 10, None, None, _day("2099-04-05"))])])
    assert links.keys == {"buy": "AAAA", "div": "AAAA"}


def test_a_statement_filed_twice_keeps_a_named_link_its_first_copy_made():
    # The first copy opens on January's holdings; the second, with no
    # contiguous predecessor, sees an empty closing alone.
    feb = _feb(holdings=[], named=[("div", EXAMPLE)])
    links = il.link([_jan(holdings=[("AAAA", EXAMPLE, 10)]), feb, feb])
    assert links.keys == {"div": "AAAA"}
    assert links.unlinked == {}
