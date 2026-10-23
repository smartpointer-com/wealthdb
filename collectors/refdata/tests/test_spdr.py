"""spdr: the fund finder claims a fund; the holdings workbook parses by
its header words; the fund page states the fund's figures. Every fund,
holding, identifier and figure is invented."""

import logging
import re
from datetime import date
from functools import partial
from pathlib import Path

import pytest

from providers.base import Context, ProviderError, read_meta
from providers.spdr import FILES, FINDER, HOST, Spdr, _plain, characteristics, fund_page, parse_holdings
from tests.helpers import FakeClient, characteristics_by_key, make_xlsx
from wanted import Fund

EMEA = [
    ["Fund Name:", "Example Europe Equity UCITS ETF"],
    ["ISIN:", "XX0000000501"],
    ["Ticker Symbol:", "QEUR GY"],
    ["Holdings As Of:", "12-Oct-2026"],
    [],
    ["ISIN", "SEDOL", "Security Name", "Currency", "Number of Shares", "Percent of Fund", "Trade Country Name",
     "Local Price", "Sector Classification", "Industry Classification", "Base Market Value"],
    ["XX0000000511", "0000001", "Alpha Industrie AG", "EUR", "1000", "60.5", "Germany", "10", "Industrials",
     "Machinery", "10000"],
    ["XX0000000512", "0000002", "Beta Energie SA", "EUR", "500", "39.5", "France", "13", "Energy", "Oil", "6500"],
    ["Unassigned", "Unassigned", "Euro", "EUR", "120", "-", "Eurozone", "1.0", "Unassigned", "Unassigned", "120"],
    [],
    ["Marketing communication. The whole or any part of this work may not be reproduced."],
]

US = [
    ["Fund Name:", "Example Equity Fund B"],
    ["Ticker Symbol:", "XMPEQB"],
    ["Holdings:", "As of 10-Oct-2026"],
    [],
    ["Name", "Ticker", "Identifier", "SEDOL", "Weight", "Sector", "Shares Held", "Local Currency"],
    ["GAMMA CORP", "QGAM", "000000AA9", "0000003", "100.0", "Financials", "200", "USD"],
]


def test_a_ucits_workbook():
    p = parse_holdings(make_xlsx(EMEA), "isin:XX0000000501", provider="spdr")
    (c,) = p.compositions
    assert (c["as_of"], c["line_count"], c["weight_sum"], c["fund_name"]) == \
        (date(2026, 10, 12), 3, "100", "Example Europe Equity UCITS ETF")
    a, _, cash = p.lines
    assert (a["isin"], a["weight"], a["country"], a["sector"], a["industry"], a["quantity"], a["market_value"]) == \
        ("XX0000000511", "60.5", "Germany", "Industrials", "Machinery", "1000", "10000")
    # A cash line states no weight; it falls into the remainder.
    assert (cash["isin"], cash["weight"], cash["name"]) == (None, None, "Euro")
    # SEDOL is never read.
    assert all("sedol" not in line["payload"] for line in p.lines)


def test_a_us_workbook_keys_its_lines_by_cusip():
    p = parse_holdings(make_xlsx(US), "symbol:XMPEQB", provider="spdr")
    (line,) = p.lines
    assert (p.compositions[0]["as_of"], line["cusip"], line["ticker"], line["weight"]) == \
        (date(2026, 10, 10), "000000AA9", "QGAM", "100")


def test_a_workbook_without_holdings_is_an_error():
    with pytest.raises(ProviderError, match="header"):
        parse_holdings(make_xlsx([["Fund Name:", "X"]]), "symbol:X", provider="spdr")


def _finder(url, params):
    datas = {"us": [{"fundUri": "/us/en/intermediary/etfs/example-equity-fund-b-xmpeqb",
                     "keywords": "Example Equity Fund B, XMPEQB, XX0000000502, 000000AB7"}],
             "ch": [{"fundUri": "/ch/en_gb/intermediary/etfs/example-europe-equity-ucits-etf-qeur-gy",
                     "keywords": "Example Europe Equity UCITS ETF, QEUR, XX0000000501, QEUR GY"},
                    {"fundUri": "/ch/en_gb/intermediary/etfs/example-bond-fund-c-xmpbdc-gy",
                     "keywords": "Example Bond Fund C, XMPBDC, XX0000000503, XMPBDC GY"}]}
    return {"data": {"funds": {"etfs": {"datas": datas[params["country"]]}}}}


def ssga(workbook=None, **routes):
    """State Street: its fund finder, the answers `routes` gives by a part
    of their URL, and `workbook`, else the EMEA fund's holdings, for any
    other workbook."""
    return FakeClient({FINDER: _finder, **routes, ".xlsx": make_xlsx(EMEA) if workbook is None else workbook})


def test_claims_by_isin_and_fetches_the_ucits_file(tmp_path):
    client = ssga()
    ctx = Context(client=client)
    p = Spdr()
    claim = p.claim(Fund(ref="isin:XX0000000501", isin="XX0000000501"), ctx)
    assert (claim["region"], claim["ticker"]) == ("emea", "qeur-gy")
    assert p.claim(Fund(ref="symbol:XMPEQB", symbol="XMPEQB"), ctx)["ticker"] == "xmpeqb"
    assert p.claim(Fund(ref="symbol:NONE", symbol="NONE"), ctx) is None
    assert p.fetch(claim, None, tmp_path, ctx) == date(2026, 10, 12)
    assert client.urls()[-1] == FILES["emea"].format(ticker="qeur-gy")
    assert client.urls().count(FINDER) == 2
    assert p.parse(tmp_path).lines


def test_a_bare_ticker_matches_a_us_fund_only():
    ctx = Context(client=ssga())
    p = Spdr()
    # QEUR is a UCITS listing's ticker: a US broker's symbol:QEUR is not it.
    assert p.claim(Fund(ref="symbol:QEUR", symbol="QEUR"), ctx) is None


@pytest.mark.parametrize("workbook,error", [
    (b"<html>not here</html>", "not a workbook"),
    (make_xlsx([["Fund Name:", "X"]]), "no holdings header"),
    (make_xlsx([row for row in EMEA if row[:1] != ["Holdings As Of:"]]), "states no holdings date"),
])
def test_a_file_that_is_no_dated_holdings_workbook_is_not_stored(tmp_path, workbook, error):
    claim = {"ref": "isin:XX0000000501", "region": "emea", "ticker": "qeur-gy"}
    with pytest.raises(ProviderError, match=error):
        Spdr().fetch(claim, None, tmp_path / "f", Context(client=ssga(workbook)))
    assert not (tmp_path / "f").exists()


def test_the_probe_reads_both_fund_finders():
    client = ssga()
    assert Spdr().probe(Context(client=client)) == "SPDR fund finders: 12 identifiers"
    assert client.urls() == [FINDER, FINDER]


FIXTURES = Path(__file__).parent / "fixtures"
BOND_PAGE = (FIXTURES / "spdr_us_bond_page.html").read_text()
EQUITY_PAGE = (FIXTURES / "spdr_us_equity_page.html").read_text()
UCITS_PAGE = (FIXTURES / "spdr_emea_bond_page.html").read_text(encoding="utf-8")
DAY = date(2026, 10, 12)


got = partial(characteristics_by_key, provider="spdr")


def test_a_us_bond_page_states_the_funds_figures_and_breakdowns():
    rows = characteristics(BOND_PAGE, "symbol:XMPBDA", date(2026, 10, 13), provider="spdr")
    assert got(rows) == {
        # The "Fund Characteristics" table, dated by its heading; not the
        # index's, and not a convexity written as a dash.
        ("weighted_average_coupon", ""): ("3.85", DAY),
        ("weighted_average_maturity_years", ""): ("8.4", DAY),
        ("yield_to_worst", ""): ("4.71", DAY),
        ("effective_duration", ""): ("6.02", DAY),
        ("option_adjusted_spread_bp", ""): ("35.4", DAY),
        ("yield_to_maturity", ""): ("4.73", DAY),
        # The "Yields" table, by its own day; the unsubsidized yield is no
        # figure of the fund's.
        ("sec_yield", ""): ("4.12", date(2026, 10, 9)),
        ("distribution_yield", ""): ("3.95", date(2026, 10, 9)),
        # The fund's chart data, its raw share where it states one; a
        # part of no share is no row, and the index's chart is not read.
        ("credit_quality", "AAA"): ("62.5001", DAY),
        ("credit_quality", "AA1"): ("12.2499", DAY),
        ("credit_quality", "BAA2"): ("25", DAY),
        ("credit_quality", "Not Rated"): ("0.25", DAY),
        ("maturity_breakdown", "0 - 1 Year"): ("10", date(2026, 10, 11)),
        ("maturity_breakdown", "1 - 5 Years"): ("40", date(2026, 10, 11)),
        ("maturity_breakdown", "5 - 10 Years"): ("30", date(2026, 10, 11)),
        ("maturity_breakdown", "> 30 Years"): ("20", date(2026, 10, 11)),
    }
    # A label's tooltip is not part of it.
    assert {r["payload"]["name"] for r in rows if r["field"] == "yield_to_worst"} == {"Average Yield To Worst"}


def test_an_equity_page_states_its_yields_and_no_forecast_pe():
    """The fund's P/E is the next fiscal year's forecast; the trailing one
    is the index's. A distribution yield of zero is none."""
    assert got(characteristics(EQUITY_PAGE, "symbol:XMPEQB", DAY, provider="spdr")) == {
        ("sec_yield", ""): ("1.85", date(2026, 10, 9))}


def test_a_bond_figure_of_an_equity_fund_is_none():
    page = BOND_PAGE.replace("&#34;Fixed Income&#34;", "&#34;Equity&#34;")
    assert set(got(characteristics(page, "symbol:XMPBDA", DAY, provider="spdr"))) == {
        ("sec_yield", ""), ("distribution_yield", "")}


def test_a_ucits_page_reads_its_figures_through_its_decimal_comma():
    assert got(characteristics(UCITS_PAGE, "isin:XX0000000503", DAY, provider="spdr")) == {
        ("convexity", ""): ("0.58", DAY),
        ("effective_duration", ""): ("6.35", DAY),
        ("yield_to_maturity", ""): ("3.02", DAY),
        ("weighted_average_maturity_years", ""): ("7.45", DAY),
        ("distribution_yield", ""): ("2.4", DAY),
        ("credit_quality", "AAA"): ("40.5", DAY),
        # No raw share: the share the chart shows, in the page's format.
        ("credit_quality", "A2"): ("59.5", DAY),
    }


@pytest.mark.parametrize("text,point,mark,plain", [
    ("1 007,50", ",", " ", "1007.50"),
    ("1.007,50 %", ",", ".", "1007.50 %"),
    ("8,802", ".", ",", "8802"),
    ("6,35", ",", ",", "6.35"),
    ("6,35", None, None, "6,35"),
    ("1,234.5 %", None, None, "1,234.5 %"),
    ("1,234.5", ".", None, "1,234.5"),
])
def test_a_figure_reads_through_the_pages_number_format(text, point, mark, plain):
    assert _plain(text, point, mark) == plain


def _no_number_format(page: str) -> str:
    return re.sub(r'<input type="hidden" id="(?:decimal-point|thousands-sep)"[^>]*>', "", page)


def test_a_page_that_names_no_number_format_reads_its_figures_as_written():
    """A decimal comma is not read as a thousands mark: 6,35 is no figure,
    never 635. A raw share and a US page's figures still read."""
    assert got(characteristics(_no_number_format(UCITS_PAGE), "isin:XX0000000503", DAY, provider="spdr")) == {
        ("credit_quality", "AAA"): ("40.5", DAY)}
    assert got(characteristics(_no_number_format(BOND_PAGE), "symbol:XMPBDA", DAY, provider="spdr")) == \
        got(characteristics(BOND_PAGE, "symbol:XMPBDA", DAY, provider="spdr"))


def test_a_figure_whose_heading_states_no_day_takes_the_holdings_day():
    page = BOND_PAGE.replace('<span class="date">as of Oct 9 2026</span>', "")
    assert got(characteristics(page, "symbol:XMPBDA", date(2026, 10, 13), provider="spdr"))[("sec_yield", "")] == \
        ("4.12", date(2026, 10, 13))


MAINTENANCE = "<html><body>Maintenance</body></html>"


def test_a_page_that_states_nothing():
    assert characteristics("", "symbol:XMPBDA", DAY, provider="spdr") == []
    assert characteristics(MAINTENANCE, "symbol:XMPBDA", DAY, provider="spdr") == []


def test_a_fund_page_is_one_with_a_table_of_the_funds_own_figures():
    assert fund_page(BOND_PAGE) and fund_page(EQUITY_PAGE) and fund_page(UCITS_PAGE)
    assert not fund_page(MAINTENANCE) and not fund_page("")


def test_a_breakdown_reads_its_first_chart_and_no_other():
    """A page with both credit-quality inputs gives each part once."""
    other = ('<input type="hidden" id="fund-quality-breakdown" value="{&#34;asOfDateSimple&#34;:&#34;Oct 12 2026&#34;,'
             '&#34;attrArray&#34;:[{&#34;name&#34;:{&#34;value&#34;:&#34;AAA&#34;},&#34;weight&#34;:{&#34;value&#34;:'
             '&#34;100.00%&#34;}}]}"/>')
    rows = got(characteristics(BOND_PAGE.replace("</body>", other + "</body>"), "symbol:XMPBDA", DAY,
                               provider="spdr"))
    assert {label: value for (field, label), (value, _) in rows.items() if field == "credit_quality"} == {
        "AAA": "62.5001", "AA1": "12.2499", "BAA2": "25", "Not Rated": "0.25"}


US_PAGE_URL = HOST + "/us/en/intermediary/etfs/example-equity-fund-b-xmpeqb"


def page_fetch(tmp_path, page=BOND_PAGE, wants=("characteristics",), fund=None, workbook=US, day=date(2026, 10, 10)):
    client = FakeClient({FINDER: _finder, ".xlsx": make_xlsx(workbook), "/intermediary/etfs/": page})
    ctx = Context(client=client)
    p = Spdr()
    claim = p.claim(fund or Fund(ref="symbol:XMPEQB", symbol="XMPEQB", wants=wants), ctx)
    assert p.fetch(claim, None, tmp_path, ctx) == day
    return client, p.parse(tmp_path)


def test_a_fetch_for_characteristics_keeps_the_fund_page(tmp_path):
    client, parsed = page_fetch(tmp_path)
    assert client.urls().count(US_PAGE_URL) == 1
    assert (tmp_path / "page.html").read_text() == BOND_PAGE
    assert "facts_missing" not in read_meta(tmp_path)
    assert len(parsed.lines) == 1
    # The holdings day dates no figure: the page states each one's.
    assert got(parsed.characteristics)[("yield_to_maturity", "")] == ("4.73", DAY)


def test_a_fund_that_wants_no_characteristics_asks_for_no_page(tmp_path):
    client, parsed = page_fetch(tmp_path, wants=())
    assert US_PAGE_URL not in client.urls() and not (tmp_path / "page.html").exists()
    assert "facts_missing" not in read_meta(tmp_path)
    assert parsed.lines and parsed.characteristics == []


def test_a_ucits_fetch_reads_the_page_its_finder_entry_names(tmp_path):
    fund = Fund(ref="isin:XX0000000503", isin="XX0000000503", wants=("characteristics",))
    client, parsed = page_fetch(tmp_path, page=UCITS_PAGE, fund=fund, workbook=EMEA, day=DAY)
    assert client.urls()[-1] == HOST + "/ch/en_gb/intermediary/etfs/example-bond-fund-c-xmpbdc-gy"
    assert client.urls()[-2] == FILES["emea"].format(ticker="xmpbdc-gy")
    # Read through the page's decimal comma.
    assert got(parsed.characteristics)[("effective_duration", "")] == ("6.35", DAY)


@pytest.mark.parametrize("page,kept,warning", [(500, False, "its fund page did not come"),
                                               (MAINTENANCE, True, "its fund page matched no known layout")])
def test_a_page_that_does_not_come_or_is_no_fund_page_leaves_the_characteristics_due(tmp_path, caplog, page, kept,
                                                                                   warning):
    with caplog.at_level(logging.WARNING, logger="refdata.spdr"):
        _, parsed = page_fetch(tmp_path, page=page)
    assert warning in caplog.text
    assert (tmp_path / "page.html").exists() is kept
    assert read_meta(tmp_path)["facts_missing"] == ["characteristics"]
    assert parsed.lines and parsed.characteristics == []


def test_a_fund_page_that_states_no_figure_answers_the_characteristics(tmp_path, caplog):
    """An accumulating equity fund's page: no P/E is read, and it pays no
    distribution. The fund has none of the figures, so none stay due."""
    page = EQUITY_PAGE.replace("1.85%", "-")
    with caplog.at_level(logging.WARNING, logger="refdata.spdr"):
        _, parsed = page_fetch(tmp_path, page=page)
    assert not caplog.text and (tmp_path / "page.html").read_text() == page
    assert "facts_missing" not in read_meta(tmp_path)
    assert parsed.lines and parsed.characteristics == []
