"""ishares, xtrackers and invesco on invented files in each issuer's
layout. Every fund, holding and identifier is invented."""

from datetime import date

import pytest

import fetcher
from providers.base import Context, ProviderError, Unavailable
from providers.invesco import Invesco, parse_docs
from providers.ishares import IShares, holdings_page, parse_csv, parse_grid
from providers.xtrackers import EXPORT, Xtrackers, parse_export
from tests.helpers import FakeClient, make_xlsx
from wanted import Fund

US_CSV = """Example Small Cap ETF
Fund Holdings as of,"Oct 12, 2026"
Inception Date,"Dec 10, 2007"
Shares Outstanding,"1,000,000.00"

Ticker,Name,Sector,Asset Class,Market Value,Weight (%),Notional Value,Quantity,Price,Location,Exchange,Currency,FX Rate,Market Currency,Accrual Date
"QAAA","ALPHA INC","Information Technology","Equity","6,000.00","60.00","6,000.00","100.00","60.00","United States","NASDAQ","USD","1.00","USD","-"
"QBBB","BETA SPA","Industrials","Equity","3,900.00","39.00","3,900.00","300.00","13.00","Italy","Borsa Italiana","USD","0.90","EUR","-"
"QCCC","GAMMA OYJ","Energy","Equity","50.00","0.50","50.00","10.00","5.00","Finland","Nasdaq Omx Helsinki Ltd.","USD","0.90","EUR","-"
"USD","USD CASH","Cash and/or Derivatives","Cash","50.00","0.50","50.00","50.00","100.00","United States","-","USD","1.00","USD","-"

"The content is for information only."
"""

PAGE = """<html><div id="c1495092304805"><!-- COMPONENT: PPV3/Holdings -->
<select class="date-dropdown"><option value="20261012" selected>12/Oct/2026</option>
<option value="20260930">30/Sept/2026</option><option value="20251231">31/Dec/2025</option></select>
<table id="allHoldingsTable"><thead><tr><th>Issuer <br>Ticker</th><th>Name</th><th>Sector</th>
<th>Asset Class</th><th>Market Value</th><th>Weight (%)</th><th>Notional Value</th><th>Nominal</th>
<th>ISIN</th><th>Price</th><th>Location</th><th>Exchange</th><th>Market Currency</th></tr></thead></table>
</div><!-- COMPONENT: next --></html>"""

GRID = {"aaData": [
    ["QLOC", "EXAMPLE PHARMA AG", "Health Care", "Equity", {"display": "CHF 700", "raw": 700},
     {"display": "70.00", "raw": 70.0}, {"display": "700", "raw": 700}, {"display": "7", "raw": 7},
     "XX0000000901", {"display": "100.00", "raw": 100}, "Switzerland", "SIX Swiss Exchange", "CHF"],
    ["QLOD", "EXAMPLE FOODS AG", "Consumer Staples", "Equity", {"display": "CHF 300", "raw": 300},
     {"display": "30.00", "raw": 30.0}, {"display": "300", "raw": 300}, {"display": "3", "raw": 3},
     "XX0000000902", {"display": "100.00", "raw": 100}, "Switzerland", "SIX Swiss Exchange", "CHF"]]}


def test_ishares_us_csv_keeps_us_tickers_only():
    p = parse_csv(US_CSV, "symbol:QSML", date(2026, 10, 12), provider="ishares")
    a, b, c, cash = p.lines
    assert (a["ticker"], a["weight"], a["currency"], a["country"], a["market_value"]) == \
        ("QAAA", "60", "USD", "United States", "6000")
    # Milan and Helsinki listings: their local tickers would read as US
    # symbols, so they move to the payload. The market currency wins
    # over the fund's base currency.
    assert (b["ticker"], b["payload"]["local_ticker"], b["currency"]) == (None, "QBBB", "EUR")
    assert c["ticker"] is None
    assert (cash["asset_type"], p.compositions[0]["weight_sum"]) == ("Cash", "100")


def test_ishares_page_names_the_grid_columns_and_the_dates():
    page = holdings_page(PAGE)
    assert page["dates"] == [date(2025, 12, 31), date(2026, 9, 30), date(2026, 10, 12)]
    assert page["columns"][0] == "Issuer Ticker" and page["columns"][8] == "ISIN"
    p = parse_grid(GRID, page["columns"], "isin:XX0000000900", date(2026, 10, 12), provider="ishares")
    a, b = p.lines
    assert (a["isin"], a["weight"], a["quantity"], a["currency"], a["country"], a["ticker"]) == \
        ("XX0000000901", "70", "7", "CHF", "Switzerland", None)
    assert a["payload"]["local_ticker"] == "QLOC"


def test_ishares_grid_without_columns_is_an_error():
    with pytest.raises(ProviderError, match="columns"):
        parse_grid(GRID, ["A", "B"], "isin:X", date(2026, 10, 12), provider="ishares")


def _products(url, params):
    us = "/us/" in url
    return {"1": {"isin": "XX0000000900" if not us else "US0000000909", "cusip": "000000AD3",
                  "localExchangeTicker": "QSML" if us else "QCHF",
                  "productPageUrl": "/us/products/1/example" if us else "/ch/individual/en/products/2/example",
                  "fundName": "Example"}}


def ishares_site(**routes):
    return FakeClient({".jsn": _products, "latest-holdings.csv": US_CSV, ".ajax": GRID, **routes, "/products/": PAGE})


def test_ishares_claims_both_sites_and_backfills_the_swiss_dates(tmp_path):
    client = ishares_site()
    ctx = Context(client=client)
    p = IShares()
    ch = p.claim(Fund(ref="isin:XX0000000900", isin="XX0000000900"), ctx)
    us = p.claim(Fund(ref="symbol:QSML", symbol="QSML"), ctx)
    assert (ch["site"], us["site"]) == ("ch", "us")
    # A bare ticker matches the US site only.
    assert p.claim(Fund(ref="symbol:QCHF", symbol="QCHF"), ctx) is None
    assert p.dates(ch, date(2026, 1, 1), date(2026, 12, 31), ctx) == [date(2026, 9, 30), date(2026, 10, 12)]
    assert p.dates(us, date(2026, 1, 1), date(2026, 12, 31), ctx) is None
    assert p.latest(ch, ctx) == date(2026, 10, 12)
    assert p.fetch(ch, date(2026, 9, 30), tmp_path / "ch", ctx) == date(2026, 9, 30)
    assert client.asked[-1][2]["asOfDate"] == "20260930"
    assert p.parse(tmp_path / "ch").lines
    assert p.fetch(us, None, tmp_path / "us", ctx) == date(2026, 10, 12)
    assert len(p.parse(tmp_path / "us").lines) == 4


XT = [["Although information in this document has been obtained from sources believed to be reliable."],
      ["", "Name", "ISIN", "Country", "Currency", "Exchange", "Type of Security", "Rating", "Primary Listing",
       "Industry Classification", "Weighting"],
      ["1", "ALPHA INC", "XX0000000911", "United States", "USD", "New York Stock Exchange", "Equities", "-",
       "Example Index", "Information Technology", "0.6"],
      ["2", "BETA AG", "XX0000000912", "Germany", "EUR", "Xetra", "Equities", "-", "-", "Industrials",
       "3.99E-01"],
      ["3", "SWISS FRANC", "_CURRENCYCHF", "Switzerland", "CHF", "-", "Cash", "-", "-", "unknown", "1.0E-03"]]


def test_xtrackers_reads_fractions_as_percent_and_its_industry_as_sector():
    p = parse_export(make_xlsx(XT), "isin:XX0000000910", date(2026, 10, 13), provider="xtrackers")
    a, b, cash = p.lines
    assert (a["weight"], a["sector"], a["industry"], a["isin"], b["weight"]) == \
        ("60", "Information Technology", None, "XX0000000911", "39.9")
    assert (cash["isin"], cash["weight"], cash["asset_type"]) == (None, "0.1", "Cash")
    assert p.compositions[0]["weight_sum"] == "100"


def test_xtrackers_claims_a_fund_its_name_names():
    x = Xtrackers()
    ctx = Context(client=None)
    assert x.claim(Fund(ref="isin:XX0000000910", isin="XX0000000910", name="Xtrackers Example UCITS ETF"), ctx)
    assert x.claim(Fund(ref="isin:XX0000000910", isin="XX0000000910", name="Example Index Fund"), ctx) is None


def test_invesco_physical_fund_holdings_and_index():
    docs = {"fund": {"effectiveDate": "2026-10-12", "holdings": [
                {"name": "Alpha Inc", "isin": "XX0000000921", "weight": 60.0, "currency": "USD"},
                {"name": "Beta Inc", "isin": "XX0000000922", "weight": 40.0, "currency": "USD"}]},
            "index": {"effectiveDate": "2026-10-10", "holdings": [
                {"name": "Alpha Inc", "isin": "XX0000000921", "cusip": "000000AE1", "weight": 59.0}]},
            "swaps": {"isin": "XX0000000920", "swapsCounterParties": []}}
    p = parse_docs(docs, "isin:XX0000000920", provider="invesco")
    assert [(c["basis"], c["as_of"]) for c in p.compositions] == [("holdings", date(2026, 10, 12)),
                                                                   ("index", date(2026, 10, 10))]
    assert not p.counterparties and not p.attributes


def test_invesco_swap_fund_holds_a_basket():
    docs = {"fund": {"effectiveDate": "2026-10-12", "holdings": [
                {"name": "Basket Co", "isin": "XX0000000931", "weight": 100.0, "currency": "EUR"}]},
            "index": {"effectiveDate": "2026-10-12", "holdings": [
                {"name": "Index Co", "isin": "XX0000000932", "cusip": None, "weight": 100.0}]},
            "swaps": {"swapsCounterParties": [{"counterPartyName": "Example Bank A", "weight": 60.5},
                                              {"counterPartyName": "Example Bank B", "weight": 39.5}]}}
    p = parse_docs(docs, "isin:XX0000000930", provider="invesco")
    assert [c["basis"] for c in p.compositions] == ["collateral", "index"]
    assert [(c["role"], c["name"], c["share"]) for c in p.counterparties] == [
        ("swap", "Example Bank A", "60.5"), ("swap", "Example Bank B", "39.5")]
    assert p.attributes[0]["replication"] == "synthetic"


def test_invesco_us_fund_by_cusip():
    docs = {"fund": {"cusip": "000000AF8", "effectiveBusinessDate": "2026-10-12", "holdings": [
        {"issuerName": "ALPHA INC", "ticker": "QAAA", "cusip": "000000AG6", "percentageOfTotalNetAssets": 99.5,
         "securityTypeName": "Common Stock", "units": 10, "marketValueBase": 995.0, "currency": "USD"},
        {"issuerName": "US DOLLAR", "ticker": None, "cusip": None, "percentageOfTotalNetAssets": 0.5,
         "securityTypeName": "Currency", "currency": "USD"}]}}
    p = parse_docs(docs, "symbol:QZQX", provider="invesco")
    a, cash = p.lines
    assert (a["ticker"], a["cusip"], a["weight"], a["asset_type"], a["quantity"]) == \
        ("QAAA", "000000AG6", "99.5", "Common Stock", "10")
    assert cash["asset_type"] == "Currency"
    inv = Invesco()
    ctx = Context(client=None)
    claim = inv.claim(Fund(ref="isin:US000000AF84", isin="US000000AF84", name="Invesco Example ETF"), ctx)
    assert (claim["site"], claim["id"]) == ("us", "000000AF8")
    assert inv.claim(Fund(ref="symbol:QZQX", symbol="QZQX"), ctx) is None


def test_invesco_fetch_skips_a_missing_index_and_swaps(tmp_path):
    api = FakeClient({"holdings/fund": {"effectiveDate": "2026-10-12", "holdings": [
        {"name": "Alpha Inc", "isin": "XX0000000941", "weight": 100.0, "currency": "USD"}]},
        "holdings/index": 500, "/swaps": 404})
    inv = Invesco()
    ctx = Context(client=api)
    claim = inv.claim(Fund(ref="isin:XX0000000940", isin="XX0000000940", name="Invesco Example UCITS ETF"), ctx)
    assert claim == {"ref": "isin:XX0000000940", "site": "ucits", "id": "XX0000000940"}
    assert inv.fetch(claim, None, tmp_path, ctx) == date(2026, 10, 12)
    got = inv.parse(tmp_path)
    assert [c["basis"] for c in got.compositions] == ["holdings"]


def test_invesco_fetch_fails_without_the_fund_holdings(tmp_path):
    inv = Invesco()
    ctx = Context(client=FakeClient({"holdings/fund": 500}))
    claim = {"ref": "isin:XX0000000940", "site": "ucits", "id": "XX0000000940"}
    with pytest.raises(fetcher.FetchError):
        inv.fetch(claim, None, tmp_path, ctx)


def test_invesco_claims_etfs_only():
    inv = Invesco()
    ctx = Context(client=None)
    assert inv.claim(Fund(ref="isin:US000000AH46", isin="US000000AH46", name="Invesco Example Fund Class A"),
                     ctx) is None


def test_xtrackers_fetch_and_claims(tmp_path):
    book = make_xlsx(XT)
    api = FakeClient({"/XX0000000910/": book})
    x = Xtrackers()
    ctx = Context(client=api)
    claim = x.claim(Fund(ref="isin:XX0000000910", isin="XX0000000910", name="Xtrackers Example UCITS ETF"), ctx)
    x.fetch(claim, None, tmp_path, ctx)
    assert api.urls() == [EXPORT.format(isin="XX0000000910")]
    assert len(x.parse(tmp_path).lines) == 3
    # A US Xtrackers ETF is no UCITS export's.
    assert x.claim(Fund(ref="isin:US0000000951", isin="US0000000951", name="Xtrackers Example ETF"), ctx) is None
    with pytest.raises(ProviderError, match="workbook"):
        x.fetch({"ref": "isin:XX0000000919", "isin": "XX0000000919"}, None, tmp_path / "bad",
                Context(client=FakeClient({"/XX0000000919/": b"<html>not here</html>"})))


def test_ishares_does_not_match_a_ucits_fund_by_a_us_ticker():
    p = IShares()
    ctx = Context(client=ishares_site())
    # The fund's ISIN is in neither list; its ticker is a US fund's.
    assert p.claim(Fund(ref="isin:XX0000000999", isin="XX0000000999", symbol="QSML"), ctx) is None


def test_ishares_fetch_refuses_a_day_the_site_does_not_serve_and_an_answer_that_is_not_json(tmp_path):
    p = IShares()
    ctx = Context(client=ishares_site())
    ch = p.claim(Fund(ref="isin:XX0000000900", isin="XX0000000900"), ctx)
    with pytest.raises(Unavailable, match="2026-08-31"):
        p.fetch(ch, date(2026, 8, 31), tmp_path / "a", ctx)
    ctx = Context(client=ishares_site(**{".ajax": "<html>maintenance</html>"}))
    with pytest.raises(fetcher.FetchError, match="not JSON"):
        p.fetch(p.claim(Fund(ref="isin:XX0000000900", isin="XX0000000900"), ctx), None, tmp_path / "b", ctx)
    assert not (tmp_path / "b").exists()


def test_ishares_probe_reads_both_product_lists():
    client = ishares_site()
    assert IShares().probe(Context(client=client)) == "iShares product lists: 4 identifiers"
    assert len(client.urls()) == 2


def test_xtrackers_and_invesco_have_nothing_cheap_to_ask():
    for p in (Xtrackers(), Invesco()):
        assert p.probe(Context(client=None)) == "nothing to probe"


def test_invesco_fetch_refuses_an_answer_that_is_not_json(tmp_path):
    ctx = Context(client=FakeClient({"holdings/fund": "<html>busy</html>"}))
    with pytest.raises(fetcher.FetchError, match="not JSON"):
        Invesco().fetch({"ref": "isin:XX0000000940", "site": "ucits", "id": "XX0000000940"}, None, tmp_path, ctx)
