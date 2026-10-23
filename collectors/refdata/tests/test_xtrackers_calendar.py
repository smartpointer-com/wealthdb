"""xtrackers' dividend calendar: the dividends table of the page data DWS's
site serves, on invented answers in its layout. Every fund, identifier,
day and amount is invented."""

import copy
import logging
from datetime import date

import pytest

from providers.base import read_meta
from providers.xtrackers import EXPORT, PAGE, _cell_day, dividend_events, dividend_rows
from tests import test_xtrackers_characteristics as chars
from tests.helpers import events_by_id
from tests.test_xtrackers_characteristics import DAY, EQUITY, ISIN, NO_FUND_PAGE, site

CAL = "dividend_calendar"


def cell(text, sort):
    return {"value": text, "sortValue": sort}


# The table of a distributing class's dividends: per row its year,
# dividend per share, dividend yield, record day, ex day and pay day.
DIVIDENDS = {
    "id": "fundinformation-dividends", "type": "Table", "headlineText": "Dividends",
    "table": {"headers": ["Year", "Dividend per Share", "Dividend yield", "Record date", "Ex Day", "Paydate"],
              "values": [
                  {"column_0": "2026", "column_1": cell("0.5274", 0.5274), "column_2": cell("1.21%", 1.21),
                   "column_3": cell("20/08/2026", "2026-08-20"), "column_4": cell("19/08/2026", "2026-08-19"),
                   "column_5": cell("03/09/2026", "2026-09-03")},
                  # Days as text alone, day first, and an amount that names
                  # its currency.
                  {"column_0": "2026", "column_1": "0.4810 EUR", "column_2": "1.12%", "column_3": "19/02/2026",
                   "column_4": "18/02/2026", "column_5": "04/03/2026"},
                  # The record day before the ex day, kept as stated.
                  {"column_0": "2016", "column_1": cell("0.1001", 0.1001), "column_2": cell("0.40%", 0.4),
                   "column_3": cell("15/06/2016", "2016-06-15"), "column_4": cell("16/06/2016", "2016-06-16"),
                   "column_5": cell("30/06/2016", "2016-06-30")},
                  {"column_0": "2015", "column_1": cell("-", None), "column_2": cell("-", None),
                   "column_3": cell("-", None), "column_4": cell("-", None), "column_5": cell("-", None)}]}}


def distributing(income="Distribution", table=True):
    """The equity page of a distributing class in US dollars, with its
    dividends table among its key facts."""
    doc = copy.deepcopy(EQUITY)
    for item in doc["pdpResult"]["pageSections"]["keyFacts"]["accordionItems"][0]["listBundle"][0]["items"]:
        item["value"] = {"Share class currency": "USD", "Income treatment": income}.get(item["key"], item["value"])
    if table:
        doc["pdpResult"]["pageSections"]["keyFacts"]["accordionItems"].append(copy.deepcopy(DIVIDENDS))
    return doc


def fetch(tmp_path, monkeypatch, page, wants=(CAL,)):
    """The site's answers and what a fetch of the fund that wants `wants`
    parses from them."""
    client = site(page)
    return client, chars.fetch(tmp_path, client, monkeypatch, wants)


def test_a_fund_that_wants_its_calendar_alone_asks_for_the_page_data(tmp_path, monkeypatch):
    client, parsed = fetch(tmp_path, monkeypatch, distributing())
    assert client.urls() == [EXPORT.format(isin=ISIN), PAGE.format(isin=ISIN)]
    assert "facts_missing" not in read_meta(tmp_path)
    assert events_by_id(parsed.dividend_events) == {
        # The ISO sort value, in the class currency of the key facts.
        "dividend|2026-08-19|2026-08-20|2026-09-03": ("dividend", date(2026, 8, 19), date(2026, 8, 20),
                                                      date(2026, 9, 3), "0.5274", "USD"),
        # Text alone reads day first; the amount names its currency.
        "dividend|2026-02-18|2026-02-19|2026-03-04": ("dividend", date(2026, 2, 18), date(2026, 2, 19),
                                                      date(2026, 3, 4), "0.481", "EUR"),
        "dividend|2016-06-16|2016-06-15|2016-06-30": ("dividend", date(2016, 6, 16), date(2016, 6, 15),
                                                      date(2016, 6, 30), "0.1001", "USD")}
    assert {(r["ref"], r["provider"], r["as_of"], r["status"]) for r in parsed.dividend_events} == {
        ("isin:" + ISIN, "xtrackers", DAY, None)}
    assert parsed.dividend_events[0]["payload"] == {"year": "2026", "dividend_yield": "1.21%"}
    assert parsed.events_complete
    # The page states no figure of an equity fund's own.
    assert parsed.characteristics == [] and parsed.lines


def test_the_page_asked_for_characteristics_answers_the_calendar_too(tmp_path, monkeypatch):
    client, parsed = fetch(tmp_path, monkeypatch, distributing(), wants=("characteristics", CAL))
    assert client.urls().count(PAGE.format(isin=ISIN)) == 1
    assert "facts_missing" not in read_meta(tmp_path) and len(parsed.dividend_events) == 3


def test_an_accumulating_class_without_the_table_answers_the_calendar(tmp_path, monkeypatch, caplog):
    with caplog.at_level(logging.WARNING, logger="refdata.xtrackers"):
        _, parsed = fetch(tmp_path, monkeypatch, EQUITY)
    assert not caplog.text and "facts_missing" not in read_meta(tmp_path) and parsed.dividend_events == []


def test_a_distributing_class_without_the_table_leaves_the_calendar_due(tmp_path, monkeypatch, caplog):
    with caplog.at_level(logging.WARNING, logger="refdata.xtrackers"):
        _, parsed = fetch(tmp_path, monkeypatch, distributing(table=False), wants=("characteristics", CAL))
    assert "its page data states no dividends table" in caplog.text
    assert read_meta(tmp_path)["facts_missing"] == [CAL]
    assert parsed.dividend_events == [] and not parsed.events_complete


@pytest.mark.parametrize("page", [500, NO_FUND_PAGE])
def test_a_page_that_does_not_come_or_is_no_fund_page_leaves_the_calendar_due(tmp_path, monkeypatch, page):
    _, parsed = fetch(tmp_path, monkeypatch, page, wants=("characteristics", CAL))
    assert CAL in read_meta(tmp_path)["facts_missing"] and parsed.dividend_events == []


def test_no_fund_page_warns_where_only_the_calendar_is_due(tmp_path, monkeypatch, caplog):
    with caplog.at_level(logging.WARNING, logger="refdata.xtrackers"):
        fetch(tmp_path, monkeypatch, NO_FUND_PAGE)
    assert "its fund page matched no known layout" in caplog.text
    assert read_meta(tmp_path)["facts_missing"] == [CAL]


def test_the_table_reads_wherever_the_page_data_holds_it():
    doc = distributing(table=False)
    doc["pdpResult"]["pageSections"]["distributions"] = {"tables": [copy.deepcopy(DIVIDENDS)]}
    assert len(dividend_rows(doc)) == 4
    assert len(dividend_events(doc, "isin:" + ISIN, DAY, provider="xtrackers")) == 3
    # An item of the id without a table of values is none.
    doc["pdpResult"]["pageSections"]["distributions"] = {"tables": [{"id": "fundinformation-dividends"}]}
    assert dividend_rows(doc) is None and dividend_rows([]) is None and dividend_rows(None) is None


@pytest.mark.parametrize("cell,day", [
    ({"value": "19/08/2026", "sortValue": "2026-08-19"}, date(2026, 8, 19)), ("19/08/2026", date(2026, 8, 19)),
    ({"value": "2026-08-19"}, date(2026, 8, 19)), ("31/02/2026", None), ({"value": "-", "sortValue": None}, None)])
def test_a_cells_day_is_its_sort_value_else_its_text_day_first(cell, day):
    assert _cell_day(cell) == day
