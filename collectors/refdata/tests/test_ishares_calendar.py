"""ishares' dividend calendar: a US fund page's distributions table and the
Swiss site's distributions answer, on invented files in the sites'
layout. Every fund, day and amount is invented."""

import json
import logging
from datetime import date
from functools import partial

import pytest

from providers.base import Context, read_meta
from providers.ishares import (CH_DISTRIBUTIONS, HOST, IShares, ch_events, ch_grid, distribution_rows,
                               distributions_url, page_state, us_events)
from tests import test_ishares_characteristics as chars
from tests.helpers import events_by_id
from tests.test_ishares_characteristics import DAY, PAGE_URL, US_EQUITY_PAGE, US_PAGE, us_fetch, us_site
from wanted import Fund

# The Swiss site's distributions answer: per row the record, ex and
# payable days and the amount, each cell a display text and a raw value.
CH_DISTRIBUTIONS_ANSWER = {"table": {"aaData": [
    [{"display": "11/Sept/2026", "raw": 20260911}, {"display": "10/Sept/2026", "raw": 20260910},
     {"display": "24/Sept/2026", "raw": 20260924}, {"display": "EUR 0.1712", "raw": 0.1712}],
    [{"display": "19 Jun 2026", "raw": ""}, {"display": "18 Jun 2026", "raw": ""},
     {"display": "30 Jun 2026", "raw": ""}, {"display": "EUR 0.1523", "raw": "EUR 0.1523"}],
    [{"display": "-", "raw": "-"}, {"display": "-", "raw": "-"}, {"display": "-", "raw": "-"},
     {"display": "-", "raw": "-"}]]}}
DISTRIBUTIONS_URL = (f"{HOST}/ch/individual/en/products/900002/example-euro-bond/{CH_DISTRIBUTIONS}.ajax"
                     "?tab=distributions&fileType=json&subtab=table")


# The page's three distributions, one column object per field (its
# `value` list beside its `formattedValue` list).
BOND_EVENTS = {
    "dividend|2026-10-01|2026-10-01|2026-10-06": ("dividend", date(2026, 10, 1), date(2026, 10, 1),
                                                  date(2026, 10, 6), "0.251234", "USD"),
    # December pays income and both gains: one dividend, one capital gain.
    "dividend|2025-12-17|2025-12-17|2025-12-22": ("dividend", date(2025, 12, 17), date(2025, 12, 17),
                                                  date(2025, 12, 22), "0.23", "USD"),
    "capital_gain|2025-12-17|2025-12-17|2025-12-22": ("capital_gain", date(2025, 12, 17), date(2025, 12, 17),
                                                      date(2025, 12, 22), "0.180512", "USD"),
    "dividend|2025-08-01|2025-08-01|2025-08-06": ("dividend", date(2025, 8, 1), date(2025, 8, 1),
                                                  date(2025, 8, 6), "0.15", "USD"),
    "return_of_capital|2025-08-01|2025-08-01|2025-08-06": ("return_of_capital", date(2025, 8, 1), date(2025, 8, 1),
                                                           date(2025, 8, 6), "0.05", "USD"),
}


def test_a_us_page_states_every_distribution_as_events_per_kind(tmp_path):
    client = us_site()
    parsed = us_fetch(tmp_path, client, ("characteristics", "dividend_calendar"))
    # The page asked for characteristics answers the calendar too: no
    # request more.
    assert [u for u in client.urls() if u.endswith("/ishares-example-bond-etf")] == [PAGE_URL]
    assert "facts_missing" not in read_meta(tmp_path)
    assert events_by_id(parsed.dividend_events) == BOND_EVENTS
    assert {(r["ref"], r["provider"], r["as_of"], r["status"]) for r in parsed.dividend_events} == {
        ("symbol:EXMP", "ishares", DAY, None)}
    gain = next(r for r in parsed.dividend_events if r["kind"] == "capital_gain")
    assert gain["payload"] == {"total": "0.410512", "short_term": "0.030512", "long_term": "0.15"}
    assert parsed.characteristics and len(parsed.lines) == 5
    # The table is the fund's whole history.
    assert parsed.events_complete


def test_a_fund_that_wants_its_calendar_alone_asks_for_the_page_and_no_product_entry(tmp_path):
    client = us_site()
    parsed = us_fetch(tmp_path, client, ("dividend_calendar",))
    assert PAGE_URL in client.urls() and (tmp_path / "page.html").exists()
    assert not (tmp_path / "product.json").exists() and "facts_missing" not in read_meta(tmp_path)
    assert events_by_id(parsed.dividend_events) == BOND_EVENTS


def test_a_us_page_that_does_not_come_leaves_the_calendar_due(tmp_path):
    parsed = us_fetch(tmp_path, us_site(page=500), ("characteristics", "dividend_calendar"))
    assert "dividend_calendar" in read_meta(tmp_path)["facts_missing"]
    assert parsed.dividend_events == [] and not parsed.events_complete


def test_a_us_page_without_a_distributions_table_leaves_only_the_calendar_due(tmp_path, caplog):
    page = US_PAGE.replace("performance.distributions.table.", "performance.payouts.table.")
    with caplog.at_level(logging.WARNING, logger="refdata.ishares"):
        parsed = us_fetch(tmp_path, us_site(page=page), ("characteristics", "dividend_calendar"))
    assert "states no distributions table" in caplog.text
    assert read_meta(tmp_path)["facts_missing"] == ["dividend_calendar"]
    assert parsed.characteristics and parsed.dividend_events == []


def _column(field, values, texts):
    return {"name": field, "fullName": f"performance.distributions.table.{field}", "visible": True,
            "formattedValue": texts, "label": field, "prefix": None, "value": values}


@pytest.mark.parametrize("nest", [
    pytest.param(lambda container, columns: {"performance": {"distributions": container, **columns}},
                 id="beside-the-container"),
    pytest.param(lambda container, columns: {"performance": {"distributions": {**container, "table": columns}}},
                 id="inside-the-container"),
])
def test_the_columns_read_as_rows_wherever_the_state_holds_them(nest):
    container = {"name": "distributions", "fullName": "performance.distributions", "label": "Distributions"}
    columns = {"exDate": _column("exDate", [20260617, 20260316], ["Jun 17, 2026", "Mar 16, 2026"]),
               "payableDate": _column("payableDate", [20260622, 20260320], ["Jun 22, 2026", "Mar 20, 2026"]),
               "incomeAmount": _column("incomeAmount", ["0.172340", "0.000000"], ["$0.172340", "$0.000000"]),
               "shortTermCapitalGain": _column("shortTermCapitalGain", ["0.000000", "0.012000"],
                                               ["$0.000000", "$0.012000"])}
    state = json.dumps(nest(container, columns)).replace('"', "&quot;")
    got = page_state(f'<div data-component-state="{state}"></div>')
    assert events_by_id(us_events(distribution_rows(got["distributions"]), "symbol:EXMP", DAY, provider="ishares")) == {
        "dividend|2026-06-17||2026-06-22": ("dividend", date(2026, 6, 17), None, date(2026, 6, 22), "0.17234", "USD"),
        "capital_gain|2026-03-16||2026-03-20": ("capital_gain", date(2026, 3, 16), None, date(2026, 3, 20), "0.012",
                                                "USD")}
    # The columns are no data points of the fund's figures.
    assert not [p for p in got["points"] if str(p.get("fullName", "")).startswith("performance.distributions.")]


def test_a_distributions_component_of_one_row_of_data_points_per_distribution():
    rows = distribution_rows(page_state(US_EQUITY_PAGE)["distributions"])
    assert events_by_id(us_events(rows, "symbol:QEQF", DAY, provider="ishares")) == {
        "dividend|2026-09-24|2026-09-24|2026-09-29": ("dividend", date(2026, 9, 24), date(2026, 9, 24),
                                                      date(2026, 9, 29), "0.4321", "USD"),
        "dividend|2026-06-24|2026-06-24|2026-06-29": ("dividend", date(2026, 6, 24), date(2026, 6, 24),
                                                      date(2026, 6, 29), "0.4012", "USD")}


def test_a_table_of_one_list_of_named_cells_per_column_reads_as_rows():
    def cell(name, value):
        return {"name": name, "value": value, "formattedValue": str(value)}
    table = [[cell("exDate", 20260901), cell("exDate", 20260801)],
             [cell("incomeAmount", "0.25"), cell("incomeAmount", "0.24")]]
    assert [(r["exDate"]["value"], r["incomeAmount"]["value"]) for r in distribution_rows(table)] == [
        (20260901, "0.25"), (20260801, "0.24")]
    # One distribution and one column reads the same either way.
    assert distribution_rows([[cell("exDate", 20260901)]]) == [{"exDate": cell("exDate", 20260901)}]


@pytest.mark.parametrize("table,rows", [
    pytest.param({}, [], id="an-empty-table"),
    pytest.param([], [], id="an-empty-list"),
    pytest.param(None, None, id="no-table"),
    pytest.param({"title": "Distributions"}, None, id="no-column"),
    pytest.param(["Oct 01, 2026"], None, id="a-list-of-text"),
])
def test_a_table_in_no_layout_is_none_and_an_empty_one_has_no_rows(table, rows):
    assert distribution_rows(table) == rows


def test_a_row_that_pays_no_part_is_one_event_of_its_total_and_one_without_a_day_is_none():
    rows = [{"exDate": {"raw": 20260301}, "totalDistribution": {"raw": "0.31", "formatted": "$0.31"},
             "incomeAmount": {"raw": 0}},
            {"exDate": {"formatted": "Feb 02, 2026"}, "totalDistribution": {"formatted": "TBA"}},
            {"totalDistribution": {"raw": "0.30"}}]
    assert events_by_id(us_events(rows, "symbol:EXMP", DAY, provider="ishares")) == {
        "other|2026-03-01||": ("other", date(2026, 3, 1), None, None, "0.31", "USD"),
        "other|2026-02-02||": ("other", date(2026, 2, 2), None, None, None, None)}


ch_site = partial(chars.ch_site, distributions=CH_DISTRIBUTIONS_ANSWER)


def ch_fetch(tmp_path, client, wants=("dividend_calendar",), as_of=None):
    ctx = Context(client=client)
    p = IShares()
    claim = p.claim(Fund(ref="isin:XX0000000910", isin="XX0000000910", wants=wants), ctx)
    p.fetch(claim, as_of, tmp_path, ctx)
    return p.parse(tmp_path)


def test_a_swiss_fetch_asks_for_the_distributions_its_page_names(tmp_path):
    client = ch_site()
    parsed = ch_fetch(tmp_path, client)
    assert [u for u in client.urls() if "tab=distributions" in u] == [DISTRIBUTIONS_URL]
    assert "facts_missing" not in read_meta(tmp_path)
    # Dated by the site's newest holdings day, as the characteristics are;
    # a row that states no day is none.
    assert events_by_id(parsed.dividend_events) == {
        "dividend|2026-09-10|2026-09-11|2026-09-24": ("dividend", date(2026, 9, 10), date(2026, 9, 11),
                                                      date(2026, 9, 24), "0.1712", "EUR"),
        "dividend|2026-06-18|2026-06-19|2026-06-30": ("dividend", date(2026, 6, 18), date(2026, 6, 19),
                                                      date(2026, 6, 30), "0.1523", "EUR")}
    assert {r["as_of"] for r in parsed.dividend_events} == {DAY} and parsed.events_complete


def test_an_accumulating_class_answers_null_and_the_calendar_is_answered(tmp_path):
    parsed = ch_fetch(tmp_path, ch_site(distributions=b"null"))
    assert (tmp_path / "distributions.json").read_bytes() == b"null"
    assert "facts_missing" not in read_meta(tmp_path) and parsed.dividend_events == []


@pytest.mark.parametrize("answer,warns", [(500, False), ({"error": "busy"}, True), (b"<html>busy</html>", False)])
def test_distributions_that_do_not_come_leave_the_calendar_due_and_the_holdings_stand(tmp_path, caplog, answer,
                                                                                     warns):
    with caplog.at_level(logging.WARNING, logger="refdata.ishares"):
        parsed = ch_fetch(tmp_path, ch_site(distributions=answer))
    assert read_meta(tmp_path)["facts_missing"] == ["dividend_calendar"]
    assert ("its distributions matched no known layout" in caplog.text) == warns
    assert parsed.dividend_events == [] and len(parsed.lines) == 2 and not parsed.events_complete


def test_a_past_days_fetch_and_a_fund_that_wants_no_calendar_ask_for_no_distributions(tmp_path):
    client = ch_site()
    assert ch_fetch(tmp_path / "past", client, as_of=date(2025, 12, 31)).dividend_events == []
    assert ch_fetch(tmp_path / "none", client, wants=("characteristics",)).dividend_events == []
    assert not [u for u in client.urls() if "tab=distributions" in u]
    assert "facts_missing" not in read_meta(tmp_path / "past")


def test_the_distributions_url_is_the_pages_on_the_sites_host_else_the_components():
    path = "/ch/individual/en/products/900003/example"
    named = f'<div data-ajaxuri="{path}/1495000000001.ajax?tab=distributions&amp;fileType=json"></div>'
    assert distributions_url(named, path) == f"{HOST}{path}/1495000000001.ajax?tab=distributions&fileType=json"
    relative = '<div data-ajaxuri="1495000000002.ajax?tab=distributions"></div>'
    assert distributions_url(relative, path) == f"{HOST}{path}/1495000000002.ajax?tab=distributions"
    default = f"{HOST}{path}/{CH_DISTRIBUTIONS}.ajax?tab=distributions&fileType=json&subtab=table"
    elsewhere = '<div data-ajaxuri="https://example.org/x.ajax?tab=distributions"></div>'
    holdings = f'<div data-ajaxuri="{path}/1495092304805.ajax?tab=all"></div>'
    assert distributions_url(elsewhere, path) == distributions_url(holdings, path) == distributions_url("", path) \
        == default


def test_a_swiss_answer_in_no_layout_is_none():
    assert ch_grid(None) == [] and ch_grid({"table": {"aaData": []}}) == []
    assert ch_grid({"table": None}) is ch_grid({"aaData": []}) is ch_grid([]) is None
    # A short row reads what it states.
    assert events_by_id(ch_events([[{"display": "11/Sept/2026", "raw": 20260911}]], "isin:XX0000000910", DAY,
                            provider="ishares")) == {
        "dividend||2026-09-11|": ("dividend", None, date(2026, 9, 11), None, None, None)}
