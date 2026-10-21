"""ishares' per-line bond figures and the characteristics a fund page and
its product-list entry state, on invented files in the sites' layout.
Every fund, holding, identifier and figure is invented."""

import html
import json
import logging
from datetime import date
from functools import partial
from pathlib import Path

import pytest

import download

from providers.base import Context, read_meta
from providers.fundpage import BOND_FIGURES
from providers.ishares import IShares, characteristics, item_figures, page_items, parse_csv, point_figures
from tests.helpers import FakeClient, characteristics_by_key
from wanted import Fund

FIXTURES = Path(__file__).parent / "fixtures"
US_CSV = (FIXTURES / "ishares_us_bond.csv").read_bytes()
US_PAGE = (FIXTURES / "ishares_us_page.html").read_text()
US_EQUITY_PAGE = (FIXTURES / "ishares_us_equity_page.html").read_text()
US_PRODUCT = json.loads((FIXTURES / "ishares_us_product.json").read_text())
CH_PAGE = (FIXTURES / "ishares_ch_page.html").read_text()
CH_EQUITY_PAGE = (FIXTURES / "ishares_ch_equity_page.html").read_text()
DAY = date(2026, 10, 12)
CH_PRODUCT = {"isin": "XX0000000910", "fundName": "iShares Example Euro Bond UCITS ETF",
              "productPageUrl": "/ch/individual/en/products/900002/example-euro-bond"}
CH_GRID = {"aaData": [
    ["QEUR", "EXAMPLE REPUBLIC 2.5% 2034", "Treasuries", "Fixed Income", {"display": "EUR 600", "raw": 600},
     {"display": "60.00", "raw": 60.0}, {"display": "600", "raw": 600}, {"display": "590", "raw": 590},
     "XX0000000911", {"display": "101.69", "raw": 101.69}, "Germany", "-", "EUR",
     {"display": "6.20", "raw": 6.2}, {"display": "15/Feb/2034", "raw": 20340215},
     {"display": "2.50", "raw": 2.5}, {"display": "2.95", "raw": 2.95}, {"display": "15/Feb/2026", "raw": 20260215}],
    ["QBNK", "EXAMPLE BANK 3% 2029", "Banking", "Fixed Income", {"display": "EUR 400", "raw": 400},
     {"display": "40.00", "raw": 40.0}, {"display": "400", "raw": 400}, {"display": "395", "raw": 395},
     "XX0000000912", {"display": "101.27", "raw": 101.27}, "France", "-", "EUR",
     {"display": "2.90", "raw": 2.9}, {"display": "01/Mar/2029", "raw": 20290301},
     {"display": "3.00", "raw": 3.0}, {"display": "3.40", "raw": 3.4}, {"display": "-", "raw": "-"}]]}
BREAKDOWN_DAY = date(2026, 10, 9)


got = partial(characteristics_by_key, provider="ishares")


def figures(found: dict) -> dict:
    """field -> (value, day) of point_figures' or item_figures' answer."""
    return {field: (value, day) for field, value, day, _ in found.values()}


def test_a_us_bond_file_states_every_line_figure():
    p = parse_csv(US_CSV.decode(), "symbol:EXMP", DAY, provider="ishares")
    note, tips, corp, euro, cash = p.lines
    assert (note["duration"], note["modified_duration"], note["yield_to_maturity"], note["yield_to_worst"],
            note["yield_to_call"], note["exchange_rate"], note["maturity"], note["coupon"], note["accrual_date"]) == \
        ("6.12", "6.05", "4.31", "4.31", None, "1", "2034-03-09", "3.88", "2026-09-09")
    # The effective date stays in the payload; a SEDOL is never read.
    assert note["payload"]["effective date"] == "Mar 09, 2024" and "sedol" not in note["payload"]
    assert (note["real_duration"], note["real_yield"]) == (None, None)
    assert (tips["real_duration"], tips["real_yield"], tips["duration"]) == ("4.75", "1.85", "4.4")
    assert (corp["yield_to_call"], corp["yield_to_worst"]) == ("5.4", "5.12")
    assert (euro["exchange_rate"], euro["currency"], euro["isin"]) == ("1.0811", "EUR", "XX0000000004")
    assert (cash["maturity"], cash["accrual_date"], cash["duration"]) == (None, None, "0")


WANTS = ("characteristics",)
PAGE_URL = "https://www.ishares.com/us/products/900001/ishares-example-bond-etf"


def us_site(page=US_PAGE, product=None):
    def products(url, params):
        return {"1": product or US_PRODUCT} if "/us/" in url else {"2": CH_PRODUCT}
    return FakeClient({".jsn": products, "latest-holdings.csv": US_CSV, "/us/products/": page})


def us_fetch(tmp_path, client, wants=WANTS):
    ctx = Context(client=client)
    p = IShares()
    claim = p.claim(Fund(ref="symbol:EXMP", symbol="EXMP", wants=wants), ctx)
    assert p.fetch(claim, None, tmp_path, ctx) == DAY
    return p.parse(tmp_path)


def test_a_us_fetch_asks_for_the_page_and_keeps_the_product_entry(tmp_path):
    client = us_site()
    parsed = us_fetch(tmp_path, client)
    assert [u for u in client.urls() if u.endswith("/ishares-example-bond-etf")] == [PAGE_URL]
    assert (tmp_path / "page.html").read_text() == US_PAGE
    assert json.loads((tmp_path / "product.json").read_text()) == US_PRODUCT
    assert "facts_missing" not in read_meta(tmp_path)
    assert len(parsed.lines) == 5 and parsed.lines[0]["duration"] == "6.12"
    assert got(parsed.characteristics) == {
        # The page's data points: each raw value, dated by its asOfDate.
        # A figure the page states twice is one row, with the day the
        # dated one states.
        ("sec_yield", ""): ("4.1234", date(2026, 10, 9)),
        ("trailing_yield", ""): ("3.7981", date(2026, 9, 30)),
        ("effective_duration", ""): ("5.84123", DAY),
        ("yield_to_maturity", ""): ("4.2512", DAY),
        ("weighted_average_coupon", ""): ("3.6704", DAY),
        ("weighted_average_maturity_years", ""): ("8.3791", DAY),
        ("option_adjusted_spread_bp", ""): ("34.87", DAY),
        ("convexity", ""): ("0.5213", DAY),
        # The page states no yield to worst: the product entry does.
        ("yield_to_worst", ""): ("4.2", date(2026, 10, 11)),
        # Neither states a distribution yield: the page's JSON-LD does,
        # without a day.
        ("distribution_yield", ""): ("3.5", DAY),
        # The exposure component's rating and maturity containers, not
        # its sector one; an empty container is no row.
        ("credit_quality", "AAA"): ("61.2", BREAKDOWN_DAY),
        ("credit_quality", "AA"): ("20.1", BREAKDOWN_DAY),
        ("credit_quality", "A"): ("8", BREAKDOWN_DAY),
        ("credit_quality", "BBB"): ("6.5", BREAKDOWN_DAY),
        ("credit_quality", "BB"): ("1.5", BREAKDOWN_DAY),
        ("credit_quality", "B"): ("0.9", BREAKDOWN_DAY),
        ("credit_quality", "Below B"): ("0.3", BREAKDOWN_DAY),
        ("credit_quality", "Not Rated"): ("0.4", BREAKDOWN_DAY),
        ("credit_quality", "Cash and Derivatives"): ("1.1", BREAKDOWN_DAY),
        ("maturity_breakdown", "0 - 1 Year"): ("3.1", BREAKDOWN_DAY),
        ("maturity_breakdown", "1 - 2 Years"): ("12.4", BREAKDOWN_DAY),
        ("maturity_breakdown", "2 - 3 Years"): ("14", BREAKDOWN_DAY),
        ("maturity_breakdown", "3 - 5 Years"): ("25.5", BREAKDOWN_DAY),
        ("maturity_breakdown", "5 - 10 Years"): ("30", BREAKDOWN_DAY),
        ("maturity_breakdown", "10+ Years"): ("15", BREAKDOWN_DAY),
    }
    by_field = {r["field"]: r["payload"] for r in parsed.characteristics if not r["label"]}
    assert by_field["yield_to_maturity"] == {"source": "page", "name": "fxHedgedYield"}
    assert by_field["yield_to_worst"] == {"source": "product", "name": "yieldToWorst"}
    assert by_field["distribution_yield"] == {"source": "json-ld", "name": "Distribution Yield"}


def test_a_fund_that_wants_no_characteristics_asks_for_no_page(tmp_path):
    client = us_site()
    parsed = us_fetch(tmp_path, client, wants=())
    assert PAGE_URL not in client.urls()
    assert not (tmp_path / "page.html").exists() and not (tmp_path / "product.json").exists()
    assert "facts_missing" not in read_meta(tmp_path)
    assert len(parsed.lines) == 5 and parsed.characteristics == []


def test_a_us_page_that_does_not_come_leaves_the_holdings_and_the_characteristics_due(tmp_path, caplog):
    with caplog.at_level(logging.WARNING, logger="refdata.ishares"):
        parsed = us_fetch(tmp_path, us_site(page=500))
    assert "its fund page did not come" in caplog.text
    assert not (tmp_path / "page.html").exists()
    # The product entry's figures land; the characteristics stay due.
    assert read_meta(tmp_path)["facts_missing"] == ["characteristics"]
    assert len(parsed.lines) == 5
    assert got(parsed.characteristics) == {("effective_duration", ""): ("5.9", date(2026, 10, 11)),
                                           ("yield_to_worst", ""): ("4.2", date(2026, 10, 11)),
                                           ("weighted_average_coupon", ""): ("3.67", DAY)}


def test_files_that_state_no_characteristics_leave_them_due(tmp_path):
    product = {k: US_PRODUCT[k] for k in ("isin", "cusip", "localExchangeTicker", "productPageUrl", "fundName")}
    parsed = us_fetch(tmp_path, us_site(page=US_EQUITY_PAGE, product=product))
    assert read_meta(tmp_path)["facts_missing"] == ["characteristics"]
    assert parsed.characteristics == []


def ch_site(page=CH_PAGE):
    def products(url, params):
        return {} if "/us/" in url else {"2": CH_PRODUCT}
    return FakeClient({".jsn": products, ".ajax": CH_GRID, "/products/": page})


def test_a_swiss_fetch_keeps_the_page_it_reads_the_columns_from(tmp_path):
    client = ch_site()
    ctx = Context(client=client)
    p = IShares()
    claim = p.claim(Fund(ref="isin:XX0000000910", isin="XX0000000910", wants=WANTS), ctx)
    assert "product" not in claim
    assert p.fetch(claim, None, tmp_path / "latest", ctx) == DAY
    assert p.fetch(claim, date(2025, 12, 31), tmp_path / "year-end", ctx) == date(2025, 12, 31)
    # Two product lists, the page once, the grid twice.
    assert len(client.urls()) == 5 and len([u for u in client.urls() if u.endswith("/example-euro-bond")]) == 1
    assert (tmp_path / "year-end" / "page.html").read_text() == CH_PAGE
    latest = p.parse(tmp_path / "latest")
    a, b = latest.lines
    assert (a["duration"], a["yield_to_maturity"], a["maturity"], a["coupon"], a["accrual_date"]) == \
        ("6.2", "2.95", "2034-02-15", "2.5", "2026-02-15")
    assert (b["accrual_date"], b["yield_to_maturity"]) == (None, "3.4")
    expected = {
        ("trailing_yield", ""): ("2.34", date(2026, 9, 30)),
        ("effective_duration", ""): ("4.71", date(2026, 10, 9)),
        ("weighted_average_maturity_years", ""): ("5.3", date(2026, 10, 9)),
        # No day stated: the site's newest holdings day. A dash and an
        # item without data are no rows, nor are the fund's net assets.
        ("yield_to_maturity", ""): ("3.12", DAY),
    }
    assert got(latest.characteristics) == expected
    assert "facts_missing" not in read_meta(tmp_path / "latest")
    # A past day's holdings come with today's page: its figures keep
    # today's days.
    assert got(p.parse(tmp_path / "year-end").characteristics) == expected


@pytest.mark.parametrize("wants,missing", [(WANTS, ["characteristics"]), ((), None)])
def test_a_swiss_page_without_data_items_leaves_wanted_characteristics_due(tmp_path, wants, missing):
    client = ch_site(page=CH_PAGE[CH_PAGE.index('<div id="c1495092304805">'):])
    ctx = Context(client=client)
    p = IShares()
    claim = p.claim(Fund(ref="isin:XX0000000910", isin="XX0000000910", wants=wants), ctx)
    assert p.fetch(claim, None, tmp_path, ctx) == DAY
    assert read_meta(tmp_path).get("facts_missing") == missing
    assert (tmp_path / "page.html").exists() and p.parse(tmp_path).characteristics == []


def test_an_equity_funds_page_states_its_pe_ratio_and_yield():
    assert got(characteristics(CH_EQUITY_PAGE, {}, "isin:XX0000000920", DAY, provider="ishares")) == {
        ("price_earnings", ""): ("18.52", date(2026, 10, 9)), ("trailing_yield", ""): ("1.85", date(2026, 9, 30))}
    # A US equity page: no figure of a bond fund's, a distribution yield
    # without a value, and no rating or maturity parts.
    assert characteristics(US_EQUITY_PAGE, {}, "symbol:QEQF", DAY, provider="ishares") == []


def test_a_page_that_states_nothing():
    assert characteristics("", {}, "symbol:QEQF", DAY, provider="ishares") == []
    assert characteristics("<html><body>Maintenance</body></html>", {"fundName": "Example"}, "symbol:QEQF", DAY,
                           provider="ishares") == []


def point(name, label, value, formatted, as_of=None):
    return {"name": name, "label": label, "value": value, "formattedValue": formatted, "asOfDate": as_of}


@pytest.mark.parametrize("p,field,value", [
    pytest.param(point("ytwFund", "Yield to Worst", "4.2012", "4.20%"), "yield_to_worst", "4.2012", id="by-label"),
    pytest.param(point("yieldToWorst", "Yield to Worst", None, "4.20%"), "yield_to_worst", "4.2",
                 id="no-raw-value"),
    pytest.param(point("weightedAvgLife", "Weighted Avg Maturity", "35", "35 days"),
                 "weighted_average_maturity_years", "0.0958", id="a-maturity-in-days"),
    pytest.param(point("weightedAvgLife", "Weighted Avg Maturity", None, "1,826 days"),
                 "weighted_average_maturity_years", "4.9993", id="a-maturity-in-days-formatted"),
    pytest.param(point("weightedAvgLife", "Weighted Avg Maturity", "8.38", "8.38 yrs"),
                 "weighted_average_maturity_years", "8.38", id="a-maturity-in-years"),
    pytest.param(point("optionAdjustedSpread", "Option Adjusted Spread", "0.35", "0.35%"),
                 "option_adjusted_spread_bp", "35", id="a-spread-in-percent"),
    pytest.param(point("priceEarnings", "P/E Ratio", "1’050.25", "1’050.25"), "price_earnings", "1050.25",
                 id="a-thousands-mark"),
])
def test_a_data_point(p, field, value):
    assert figures(point_figures([p])) == {field: (value, None)}


def test_a_data_point_without_a_value_or_another_figure_is_none():
    assert point_figures([point("modelOad", "Effective Duration", "-", "-"),
                          point("beta3Yr", "Beta (3y)", "1.02", "1.02"),
                          point("expr", "Expense Ratio", "0.05", "0.05%")]) == {}


def test_a_data_point_in_a_script_and_in_an_attribute():
    state = json.dumps({"points": [point("convexity", "Convexity", "0.61", "0.61", 20261009)]})
    for page in (f"<script>window.state = {state};</script>", f'<div data-state="{html.escape(state)}"></div>'):
        assert got(characteristics(page, {}, "symbol:EXMP", DAY, provider="ishares")) == {
            ("convexity", ""): ("0.61", BREAKDOWN_DAY)}


def test_a_point_named_for_a_figure_wins_over_one_only_labelled_so():
    benchmark = point("benchmarkOad", "Effective Duration", "6.50", "6.50 yrs", 20261012)
    fund = point("modelOad", "Effective Duration", "5.84", "5.84 yrs", 20261012)
    assert figures(point_figures([benchmark, fund])) == {"effective_duration": ("5.84", DAY)}
    # A label still answers a figure no named point states.
    assert figures(point_figures([benchmark])) == {"effective_duration": ("6.5", DAY)}


def test_a_data_item_by_its_caption():
    page = ('<div class="product-data-item col-earningsMultiple "> <div class="caption"> P/E Ratio </div>'
            ' <div class="data"> 1’050.25 </div> </div>')
    assert figures(item_figures(page_items(page))) == {"price_earnings": ("1050.25", None)}


def test_an_item_named_for_a_figure_wins_over_one_only_captioned_so():
    page = ('<div class="product-data-item col-benchmarkOad "> <div class="caption"> Effective Duration </div>'
            ' <div class="data"> 6.50 </div> </div>'
            '<div class="product-data-item col-modelOad "> <div class="caption"> Effective Duration </div>'
            ' <div class="data"> 5.84 </div> </div>')
    assert figures(item_figures(page_items(page))) == {"effective_duration": ("5.84", None)}


def test_line_durations_come_only_with_an_ishares_composition():
    """The planner names iShares as the provider whose compositions state
    a fund's line durations."""
    assert download._unanswered("line_durations", []) == "comes only with a composition from: ishares"


def test_a_zero_or_a_dash_is_no_figure():
    """A site writes a zero or a dash for a figure it does not compute."""
    assert point_figures([point("modelOad", "Effective Duration", "0", "0.00 yrs"),
                          point("twelveMonTrlYld", "12m Trailing Yield", 0, "0.00%"),
                          point("optionAdjustedSpread", "Option Adjusted Spread", None, "0.00"),
                          point("convexity", "Convexity", None, "-")]) == {}
    # A small figure is one.
    assert figures(point_figures([point("modelOad", "Effective Duration", 0.00616, "0.01 yrs")])) == {
        "effective_duration": ("0.00616", None)}


def item(name, caption, data):
    return (f'<div class="product-data-item col-{name} "> <div class="caption"> {caption} </div>'
            f' <div class="data"> {data} </div> </div>')


def us_state(points) -> str:
    """A US page whose script state holds `points`."""
    return f'<div data-state="{html.escape(json.dumps({"points": points}))}"></div>'


EQUITY_PRODUCT = {"aladdinAssetClass": "Equity", "optionAdjustedSpread": {"d": "0.04", "r": 0.0412},
                  "thirtyDaySecYield": {"d": "1.18", "r": 1.18}, "effectiveDuration": "-"}


@pytest.mark.parametrize("page,product", [
    pytest.param(item("assetClass", "Asset Class", "Equity") + item("priceEarnings", "P/E Ratio", "18.52")
                 + item("modelOad", "Effective Duration", "0.12"), {}, id="swiss-page"),
    pytest.param(us_state([point("assetClass", "Asset Class", None, "Equity"),
                           point("priceEarnings", "P/E Ratio", "18.52", "18.52"),
                           point("modelOad", "Effective Duration", "0.12", "0.12 yrs")]), {}, id="us-page"),
    pytest.param(item("priceEarnings", "P/E Ratio", "18.52"), EQUITY_PRODUCT, id="product-entry"),
])
def test_a_bond_figure_of_an_equity_fund_is_none(page, product):
    """A product list states an equity fund's option-adjusted spread, and
    a page could state its duration: neither applies to the fund."""
    rows = got(characteristics(page, product, "symbol:QEQF", DAY, provider="ishares"))
    assert ("price_earnings", "") in rows and not {f for f, _ in rows} & BOND_FIGURES
    if product:
        assert rows[("sec_yield", "")] == ("1.18", DAY)


def test_a_stock_figure_of_a_bond_fund_is_none_and_a_fund_of_no_stated_class_keeps_both():
    page = item("priceEarnings", "P/E Ratio", "14.2") + item("modelOad", "Effective Duration", "5.84")
    bond = got(characteristics(page + item("assetClass", "Asset Class", "Fixed Income"), {}, "isin:XX0000000910", DAY,
                               provider="ishares"))
    assert set(bond) == {("effective_duration", "")}
    assert set(got(characteristics(page, {}, "isin:XX0000000910", DAY, provider="ishares"))) == {
        ("effective_duration", ""), ("price_earnings", "")}


def test_a_breakdown_part_of_no_share_is_no_row():
    exposure = {"componentId": "exposureBreakdowns", "containersByNameMap": {"rating": {"dataPointsByNameMap": {
        "type": {"name": "type", "value": ["AAA", "BB"]}, "fund": {"name": "fund", "value": [99.5, 0.0]}}}}}
    page = f'<div data-state="{html.escape(json.dumps(exposure))}"></div>'
    assert got(characteristics(page, {}, "symbol:EXMP", DAY, provider="ishares")) == {
        ("credit_quality", "AAA"): ("99.5", DAY)}
