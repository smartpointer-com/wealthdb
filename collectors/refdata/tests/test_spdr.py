"""spdr: the fund finder claims a fund; the holdings workbook parses by
its header words. Every fund, holding and identifier is invented."""

import json
from datetime import date

import pytest

import fetcher
from providers.base import Context, ProviderError
from providers.spdr import FILES, FINDER, Spdr, parse_holdings
from tests.helpers import make_xlsx
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
    ["Fund Name:", "Example US Value ETF"],
    ["Ticker Symbol:", "QVAL"],
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
    assert p.funds[0]["isin"] == "XX0000000501"


def test_a_us_workbook_keys_its_lines_by_cusip():
    p = parse_holdings(make_xlsx(US), "symbol:QVAL", provider="spdr")
    (line,) = p.lines
    assert (p.compositions[0]["as_of"], line["cusip"], line["ticker"], line["weight"]) == \
        (date(2026, 10, 10), "000000AA9", "QGAM", "100")


def test_a_workbook_without_holdings_is_an_error():
    with pytest.raises(ProviderError, match="header"):
        parse_holdings(make_xlsx([["Fund Name:", "X"]]), "symbol:X", provider="spdr")


class _FakeSsga:
    def __init__(self):
        self.urls = []

    def get(self, url, params=None):
        self.urls.append((url, (params or {}).get("country")))
        if url == FINDER:
            datas = {"us": [{"fundUri": "/us/en/intermediary/etfs/example-us-value-etf-qval",
                             "keywords": "Example US Value ETF, QVAL, XX0000000502, 000000AB7"}],
                     "ch": [{"fundUri": "/ch/en_gb/intermediary/etfs/example-europe-equity-ucits-etf-qeur-gy",
                             "keywords": "Example Europe Equity UCITS ETF, QEUR, XX0000000501, QEUR GY"}]}
            doc = {"data": {"funds": {"etfs": {"datas": datas[params["country"]]}}}}
            return fetcher.Response(url, 200, json.dumps(doc).encode(), "application/json")
        return fetcher.Response(url, 200, make_xlsx(EMEA), "application/vnd.ms-excel")


def test_claims_by_isin_and_fetches_the_ucits_file(tmp_path):
    client = _FakeSsga()
    ctx = Context(client=client)
    p = Spdr()
    claim = p.claim(Fund(ref="isin:XX0000000501", isin="XX0000000501"), ctx)
    assert (claim["region"], claim["ticker"]) == ("emea", "qeur-gy")
    assert p.claim(Fund(ref="symbol:QVAL", symbol="QVAL"), ctx)["ticker"] == "qval"
    assert p.claim(Fund(ref="symbol:NONE", symbol="NONE"), ctx) is None
    assert p.fetch(claim, None, tmp_path, ctx) == date(2026, 10, 12)
    assert client.urls[-1][0] == FILES["emea"].format(ticker="qeur-gy")
    assert len([u for u in client.urls if u[0] == FINDER]) == 2
    assert p.parse(tmp_path).lines


def test_a_bare_ticker_matches_a_us_fund_only():
    client = _FakeSsga()
    ctx = Context(client=client)
    p = Spdr()
    # QEUR is a UCITS listing's ticker: a US broker's symbol:QEUR is not it.
    assert p.claim(Fund(ref="symbol:QEUR", symbol="QEUR"), ctx) is None
