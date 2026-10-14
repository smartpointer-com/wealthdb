"""amundi: a fund's holdings from the product API, a swap fund's basket
read as collateral. Every fund, holding and figure is invented."""

import json
from datetime import date
from decimal import Decimal

import pytest

import fetcher
from providers.amundi import API, Amundi, parse_product
from providers.base import Context, ProviderError, Unavailable
from wanted import Fund

ISIN = "XX0000000301"


def product(replication, lines, isin=ISIN):
    return {"productId": isin, "currencies": {"CURRENCY": "USD"},
            "characteristics": {"FUND_REPLICATION_METHODOLOGY": replication,
                                "SHARE_MARKETING_NAME": "Amundi Qland UCITS ETF USD Acc"},
            "composition": {"totalNumberOfInstruments": len(lines), "compositionData": [
                {"compositionCharacteristics": {"date": d, "isin": i, "name": name, "type": kind, "weight": w,
                                                "quantity": 10.0, "currency": "USD", "sector": sector,
                                                "countryOfRisk": country}, "weight": w}
                for d, i, name, kind, w, sector, country in lines]}}


LINES = [("2026-10-12", "XX0000000302", "QORVANT MINING", "EQUITY_ORDINARY", 0.6, "Materials", "Brazil"),
         ("2026-10-11", "XX0000000303", "TALVERI BANK", "EQUITY_PREFERRED", 0.07, "Financials", "Mexico"),
         ("2026-10-12", None, None, "CASH", 0.33, None, None)]


def test_a_physical_fund_lists_its_holdings_in_percent():
    p = parse_product(product("Direct (Physical)", LINES), "isin:" + ISIN, ISIN, provider="amundi")
    (c,) = p.compositions
    # Dated by its newest line.
    assert (c["as_of"], c["basis"], c["line_count"], c["currency"]) == (date(2026, 10, 12), "holdings", 3, "USD")
    mine, bank, cash = p.lines
    assert (mine["isin"], mine["weight"], mine["sector"], mine["risk_country"], mine["asset_type"]) == \
        ("XX0000000302", "60", "Materials", "Brazil", "EQUITY_ORDINARY")
    # A fraction read as percent with no float noise.
    assert bank["weight"] == "7"
    assert (cash["asset_type"], cash["isin"], cash["name"]) == ("CASH", None, None)
    assert p.attributes == [] and p.funds[0]["isin"] == ISIN


def test_a_line_without_its_own_weight_takes_the_one_its_characteristics_state():
    doc = product("Direct (Physical)", LINES[:1])
    doc["composition"]["compositionData"][0]["weight"] = None
    assert Decimal(parse_product(doc, "isin:" + ISIN, ISIN, provider="amundi").lines[0]["weight"]) == 60


@pytest.mark.parametrize("replication", ["Indirect(Swap Based)", "Synthetic"])
def test_a_swap_funds_basket_is_collateral(replication):
    p = parse_product(product(replication, LINES), "isin:" + ISIN, ISIN, provider="amundi")
    assert {ln["basis"] for ln in p.lines} == {"collateral"} and p.compositions[0]["basis"] == "collateral"
    (a,) = p.attributes
    assert a["replication"] == "synthetic"


class Api:
    def __init__(self, *known):
        self.known, self.bodies = known, []

    def post_json(self, url, payload, headers=None):
        assert url == API
        self.bodies.append(payload)
        doc = {"products": list(self.known)}
        return fetcher.Response(url, 200, json.dumps(doc).encode(), "application/json")


def test_amundi_claims_an_etf_of_its_own_without_a_request():
    api = Api()
    ctx = Context(client=api)
    a = Amundi()
    assert a.claim(Fund(ref="isin:" + ISIN, isin=ISIN, name="AMUNDI QLAND UCITS ETF"), ctx) == \
        {"ref": "isin:" + ISIN, "isin": ISIN}
    assert a.claim(Fund(ref="isin:XX0000000306", isin="XX0000000306", name="Lyxor Qland ETF"), ctx) is not None
    # Another issuer's fund, or an Amundi fund that is no ETF, is not claimed.
    assert a.claim(Fund(ref="isin:XX0000000304", isin="XX0000000304", name="Qumbra Qland UCITS ETF"), ctx) is None
    assert a.claim(Fund(ref="isin:XX0000000305", isin="XX0000000305", name="Amundi Qland Bond Fund"), ctx) is None
    assert api.bodies == []


def test_amundi_fetches_the_holdings_of_the_fund_asked_for(tmp_path):
    other = product("Direct (Physical)", LINES, isin="XX0000000307")
    api = Api(other, product("Direct (Physical)", LINES))
    ctx = Context(client=api)
    a = Amundi()
    claim = a.claim(Fund(ref="isin:" + ISIN, isin=ISIN, name="Amundi Qland UCITS ETF"), ctx)
    assert a.fetch(claim, None, tmp_path / "f", ctx) == date(2026, 10, 12)
    # Switzerland's private investors are the context; the answer is
    # stored as served.
    assert api.bodies[0]["context"]["userProfileName"] == "RETAIL" and api.bodies[0]["productIds"] == [ISIN]
    assert json.loads((tmp_path / "f" / "products.json").read_text())["products"][0]["productId"] == "XX0000000307"
    assert len(a.parse(tmp_path / "f").lines) == 3
    with pytest.raises(Unavailable):
        a.fetch(claim, date(2026, 1, 30), tmp_path / "g", ctx)


def test_a_fund_the_api_does_not_know_or_lists_undated_is_refused(tmp_path):
    a = Amundi()
    claim = {"ref": "isin:" + ISIN, "isin": ISIN}
    with pytest.raises(ProviderError, match="does not know"):
        a.fetch(claim, None, tmp_path / "f", Context(client=Api()))
    undated = product("Direct (Physical)", LINES)
    undated["composition"] = {}
    with pytest.raises(ProviderError, match="no dated holdings"):
        a.fetch(claim, None, tmp_path / "f", Context(client=Api(undated)))
    assert not (tmp_path / "f").exists()
