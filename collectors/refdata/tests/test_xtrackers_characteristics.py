"""xtrackers: the figures a fund page shows, from the page data DWS's
site serves, on invented answers in its layout. Every fund, identifier
and figure is invented."""

import copy
import json
import logging
from datetime import date
from functools import partial
from pathlib import Path

import pytest

from providers.base import Context, read_meta
from providers.xtrackers import EXPORT, PAGE, Xtrackers, characteristics, fund_page
from tests.helpers import FakeClient, characteristics_by_key, make_xlsx
from wanted import Fund

FIXTURES = Path(__file__).parent / "fixtures"
BOND = json.loads((FIXTURES / "xtrackers_bond_page.json").read_text())
EQUITY = json.loads((FIXTURES / "xtrackers_equity_page.json").read_text())
DAY = date(2026, 10, 13)
ISIN = "XX0000000960"
EXPORT_ROWS = [["Although information in this document has been obtained from sources believed to be reliable."],
               ["", "Name", "ISIN", "Country", "Currency", "Exchange", "Type of Security", "Rating",
                "Primary Listing", "Industry Classification", "Weighting"],
               ["1", "EXAMPLE REPUBLIC 2.5% 2034", "XX0000000911", "Germany", "EUR", "-", "Bonds", "AAA", "-",
                "Government", "1.0"]]


got = partial(characteristics_by_key, provider="xtrackers")
NO_FUND_PAGE = {"culture": "en-GB", "main": {}}


def portfolio(doc) -> list:
    """The items of a page's portfolio characteristics."""
    accordions = doc["pdpResult"]["pageSections"]["keyFacts"]["accordionItems"]
    (item,) = [a for a in accordions if a["id"] == "fundinformation-portfoliocharacteristics"]
    return item["listBundle"][0]["items"]


def test_a_bond_funds_key_facts():
    rows = characteristics(BOND, "isin:XX0000000960", DAY, provider="xtrackers")
    # Dated day first, as the page writes it; the index's figures and the
    # modified duration are none.
    assert got(rows) == {("effective_duration", ""): ("6.45", date(2026, 10, 9)),
                         ("yield_to_worst", ""): ("3.21", date(2026, 10, 9)),
                         ("weighted_average_coupon", ""): ("2.48", date(2026, 10, 9))}
    assert {r["payload"]["name"] for r in rows} == {"Effective duration", "Yield to worst", "Coupon"}


def test_an_equity_funds_page_states_no_figure_of_its_own():
    """Its P/E and dividend yield are its index's."""
    assert characteristics(EQUITY, "isin:XX0000000970", DAY, provider="xtrackers") == []


def test_a_bond_figure_of_an_equity_fund_is_none():
    doc = copy.deepcopy(BOND)
    doc["tealium"]["attributes"]["product_category"] = ["Equities"]
    assert characteristics(doc, "isin:XX0000000960", DAY, provider="xtrackers") == []


def test_a_category_of_several_names_is_no_asset_class():
    doc = copy.deepcopy(BOND)
    doc["tealium"]["attributes"]["product_category"] = ["Equities", "Fixed Income"]
    assert len(characteristics(doc, "isin:XX0000000960", DAY, provider="xtrackers")) == 3


def test_a_dash_a_zero_n_a_or_a_number_without_its_text_is_none():
    doc = copy.deepcopy(BOND)
    for item in portfolio(doc):
        item["value"] = {"Coupon": "0.00%", "Yield to worst": "N/A", "Effective duration": "-"}.get(item["key"])
    assert characteristics(doc, "isin:XX0000000960", DAY, provider="xtrackers") == []
    for item in portfolio(doc):
        item["value"] = {"Effective duration": 6.45, "Coupon": True}.get(item["key"])
    assert characteristics(doc, "isin:XX0000000960", DAY, provider="xtrackers") == []


def test_a_figure_of_no_stated_day_takes_the_fetch_day():
    doc = copy.deepcopy(BOND)
    accordions = doc["pdpResult"]["pageSections"]["keyFacts"]["accordionItems"]
    next(a for a in accordions if a["id"] == "fundinformation-portfoliocharacteristics")["disclaimers"] = []
    assert got(characteristics(doc, "isin:XX0000000960", DAY, provider="xtrackers"))[("yield_to_worst", "")] == \
        ("3.21", DAY)


@pytest.mark.parametrize("doc", [{}, NO_FUND_PAGE, {"pdpResult": {"pageSections": []}}, []])
def test_page_data_without_key_facts_is_no_fund_page(doc):
    assert characteristics(doc, "isin:XX0000000960", DAY, provider="xtrackers") == []
    assert not fund_page(doc)
    assert fund_page(BOND) and fund_page(EQUITY)


def site(page=BOND):
    return FakeClient({"/constituent/": make_xlsx(EXPORT_ROWS), "/api/pdp/": page})


def fetch(tmp_path, client, monkeypatch, wants=("characteristics",)):
    monkeypatch.setattr("providers.xtrackers.today_utc", lambda: DAY)
    ctx = Context(client=client)
    x = Xtrackers()
    claim = x.claim(Fund(ref="isin:XX0000000960", isin=ISIN, name="Xtrackers Example UCITS ETF", wants=wants), ctx)
    assert x.fetch(claim, None, tmp_path, ctx) == DAY
    return x.parse(tmp_path)


def test_a_fetch_for_characteristics_keeps_the_page_data(tmp_path, monkeypatch):
    client = site()
    parsed = fetch(tmp_path, client, monkeypatch)
    assert client.urls() == [EXPORT.format(isin=ISIN), PAGE.format(isin=ISIN)]
    assert json.loads((tmp_path / "page.json").read_text()) == BOND
    assert "facts_missing" not in read_meta(tmp_path)
    assert len(parsed.lines) == 1
    assert ("effective_duration", "") in got(parsed.characteristics)


def test_a_fund_that_wants_no_characteristics_asks_for_no_page(tmp_path, monkeypatch):
    client = site()
    parsed = fetch(tmp_path, client, monkeypatch, wants=())
    assert client.urls() == [EXPORT.format(isin=ISIN)] and not (tmp_path / "page.json").exists()
    assert "facts_missing" not in read_meta(tmp_path)
    assert parsed.lines and parsed.characteristics == []


@pytest.mark.parametrize("page,kept,warning", [
    (500, False, "its fund page did not come"),
    ("<html>maintenance</html>", False, "its fund page did not come"),
    (NO_FUND_PAGE, True, "its fund page matched no known layout"),
])
def test_a_page_that_does_not_come_or_is_no_fund_page_leaves_the_characteristics_due(tmp_path, monkeypatch, caplog,
                                                                                   page, kept, warning):
    with caplog.at_level(logging.WARNING, logger="refdata.xtrackers"):
        parsed = fetch(tmp_path, site(page), monkeypatch)
    assert warning in caplog.text
    assert (tmp_path / "page.json").exists() is kept
    assert read_meta(tmp_path)["facts_missing"] == ["characteristics"]
    assert parsed.lines and parsed.characteristics == []


def test_an_equity_funds_page_answers_the_characteristics(tmp_path, monkeypatch, caplog):
    """It states no figure of the fund's own: none stay due, so the export
    is not asked for again before max_age."""
    with caplog.at_level(logging.WARNING, logger="refdata.xtrackers"):
        parsed = fetch(tmp_path, site(EQUITY), monkeypatch)
    assert not caplog.text and (tmp_path / "page.json").exists()
    assert "facts_missing" not in read_meta(tmp_path)
    assert parsed.lines and parsed.characteristics == []
