"""The security providers: gleif, openfigi and sec-edgar, on invented
answers in each API's layout."""

import json
from datetime import date

import fetcher
from providers.base import Context
from providers.gleif import Gleif, parse_records
from providers.openfigi import OpenFigi, parse_mapping
from providers.sec_edgar import SecEdgar, latest_shares, parse_company
from wanted import Security

DAY = date(2026, 10, 13)


def test_gleif_states_the_issuer_and_its_countries():
    doc = {"data": [{"id": "XXXXXXXXXXXXXXXXXX21", "attributes": {"lei": "XXXXXXXXXXXXXXXXXX21", "entity": {
        "legalName": {"name": "Example Holdings Ltd"}, "jurisdiction": "KY", "category": "GENERAL",
        "legalAddress": {"country": "KY"}, "headquartersAddress": {"country": "HK"}}}}]}
    p = parse_records(doc, "isin:XX0000000601", "XX0000000601", DAY, provider="gleif")
    (s,) = p.securities
    assert (s["lei"], s["name"], s["payload"]["category"]) == ("XXXXXXXXXXXXXXXXXX21", "Example Holdings Ltd",
                                                              "GENERAL")
    assert {(f["field"], f["value"]) for f in p.security_facts} == {("legal_country", "KY"), ("hq_country", "HK")}


def test_gleif_reads_a_subdivision_as_its_country_and_records_a_miss():
    doc = {"data": [{"attributes": {"lei": "XXXXXXXXXXXXXXXXXX22", "entity": {"jurisdiction": "US-DE"}}}]}
    p = parse_records(doc, "isin:XX0000000602", "XX0000000602", DAY, provider="gleif")
    assert [(f["field"], f["value"]) for f in p.security_facts] == [("legal_country", "US")]
    miss = parse_records({"data": []}, "isin:XX0000000603", "XX0000000603", DAY, provider="gleif")
    assert miss.securities[0]["payload"] == {"found": False} and not miss.security_facts


def test_gleif_claims_a_security_with_an_isin():
    g = Gleif()
    assert g.claim_security(Security(key="isin:XX0000000604"), Context(client=None))["isin"] == "XX0000000604"
    assert g.claim_security(Security(key="symbol:QAAA"), Context(client=None)) is None


class _FakeFigi:
    def __init__(self):
        self.posts = []

    def post_json(self, url, payload):
        self.posts.append(payload)
        out = []
        for job in payload:
            if job["idValue"] == "QNONE":
                out.append({"warning": "No identifier found."})
            else:
                out.append({"data": [{"figi": "BBG000000001", "compositeFIGI": "BBG000000002", "name": "EXAMPLE CO",
                                      "ticker": job["idValue"], "exchCode": "US", "securityType": "Common Stock",
                                      "marketSector": "Equity"}]})
        return fetcher.Response(url, 200, json.dumps(out).encode(), "application/json")


def test_openfigi_asks_five_securities_a_request(tmp_path):
    client = _FakeFigi()
    ctx = Context(client=client)
    p = OpenFigi()
    claims = [p.claim_security(Security(key=f"symbol:Q{i}"), ctx) for i in range(7)]
    for i, c in enumerate(claims):
        p.fetch_security(c, tmp_path / str(i), ctx)
    assert [len(b) for b in client.posts] == [5, 2]
    (s,) = p.parse(tmp_path / "6").securities
    assert (s["figi"], s["ticker"], s["exchange"], s["security_type"]) == ("BBG000000002", "Q6", "US", "Common Stock")


def test_openfigi_prefers_the_isin_and_records_a_miss():
    p = OpenFigi()
    ctx = Context(client=None)
    c = p.claim_security(Security(key="isin:XX0000000605", symbol="QAAA"), ctx)
    assert c["job"] == {"idType": "ID_ISIN", "idValue": "XX0000000605"}
    miss = parse_mapping({"job": {"idType": "TICKER", "idValue": "QNONE"}, "result": {"warning": "No identifier found."}},
                         "symbol:QNONE", DAY, provider="openfigi")
    assert miss.securities[0]["figi"] is None and miss.securities[0]["payload"]["found"] is False


SUB = {"cik": "0000000007", "name": "EXAMPLE INDUSTRIES INC", "entityType": "operating", "sic": "3711",
       "sicDescription": "Motor Vehicles", "tickers": ["QEXI"], "exchanges": ["NYSE"], "stateOfIncorporation": "DE",
       "addresses": {"business": {"stateOrCountry": "CA"}}}
SHARES = {"units": {"shares": [
    {"end": "2026-03-31", "val": 1000, "accn": "0000000007-26-000001", "filed": "2026-05-01"},
    {"end": "2026-06-30", "val": 1100, "accn": "0000000007-26-000002", "filed": "2026-08-01"},
    {"end": "2026-06-30", "val": 1150, "accn": "0000000007-26-000003", "filed": "2026-08-20"}]}}


def test_sec_edgar_states_sic_incorporation_and_shares():
    p = parse_company(SUB, SHARES, "symbol:QEXI", DAY, provider="sec-edgar")
    (s,) = p.securities
    assert (s["name"], s["ticker"], s["exchange"]) == ("EXAMPLE INDUSTRIES INC", "QEXI", "NYSE")
    facts = {f["field"]: (f["value"], f["scheme"]) for f in p.security_facts}
    assert facts == {"sic": ("3711", "sic"), "incorporation": ("DE", "edgar"), "hq_country": ("CA", "edgar"),
                     "shares_outstanding": ("1150", "count")}
    assert latest_shares(None) == (None, None)


def test_sec_edgar_claims_a_listed_ticker():
    class Tickers:
        def get(self, url, params=None):
            body = json.dumps({"0": {"cik_str": 7, "ticker": "QEXI"}}).encode()
            return fetcher.Response(url, 200, body, "application/json")
    ctx = Context(client=Tickers(), contact="ops@example.org")
    e = SecEdgar()
    assert e.claim_security(Security(key="symbol:QEXI"), ctx)["cik"] == 7
    assert e.claim_security(Security(key="symbol:QZZZ"), ctx) is None


class _FailingFigi:
    def __init__(self, body=None):
        self.posts, self.body = [], body

    def post_json(self, url, payload):
        self.posts.append(payload)
        if self.body is None:
            raise fetcher.FetchError(url, 400, "Bad Request")
        return fetcher.Response(url, 200, self.body, "application/json")


def test_a_failed_openfigi_batch_fails_its_own_securities_only(tmp_path):
    import pytest
    from providers.base import ProviderError
    client = _FailingFigi()
    ctx = Context(client=client)
    p = OpenFigi()
    claims = [p.claim_security(Security(key=f"symbol:Q{i}"), ctx) for i in range(7)]
    for i, c in enumerate(claims):
        with pytest.raises((ProviderError, fetcher.FetchError)):
            p.fetch_security(c, tmp_path / str(i), ctx)
    # One request per batch: the failed five are not asked again.
    assert [len(b) for b in client.posts] == [5, 2]


def test_an_openfigi_answer_of_the_wrong_length_is_an_error(tmp_path):
    import pytest
    from providers.base import ProviderError
    ctx = Context(client=_FailingFigi(body=b"[]"))
    p = OpenFigi()
    c = p.claim_security(Security(key="symbol:QAAA"), ctx)
    with pytest.raises(ProviderError, match="something else"):
        p.fetch_security(c, tmp_path, ctx)


def test_openfigi_keeps_a_ticker_from_a_us_listing_only():
    listings = [{"figi": "BBG000000011", "ticker": "QLOC", "exchCode": "SW", "name": "EXAMPLE AG"},
                {"figi": "BBG000000012", "ticker": "QUSA", "exchCode": "US", "name": "EXAMPLE AG"}]
    us = parse_mapping({"job": {"idType": "ID_ISIN", "idValue": "XX0000000606"}, "result": {"data": listings}},
                       "isin:XX0000000606", DAY, provider="openfigi").securities[0]
    assert (us["ticker"], us["exchange"], us["figi"]) == ("QUSA", "US", "BBG000000012")
    local = parse_mapping({"job": {"idType": "ID_CUSIP", "idValue": "000000AC5"}, "result": {"data": listings[:1]}},
                          "cusip:000000AC5", DAY, provider="openfigi").securities[0]
    assert (local["ticker"], local["payload"]["local_ticker"], local["cusip"]) == (None, "QLOC", "000000AC5")


def test_sec_edgar_without_shares_is_no_error(tmp_path):
    class Edgar:
        def get(self, url, params=None):
            if "companyconcept" in url:
                raise fetcher.FetchError(url, 404, "Not Found")
            return fetcher.Response(url, 200, json.dumps(SUB).encode(), "application/json")
    ctx = Context(client=Edgar(), contact="ops@example.org")
    e = SecEdgar()
    e.fetch_security({"key": "symbol:QEXI", "symbol": "QEXI", "cik": 7}, tmp_path, ctx)
    got = e.parse(tmp_path)
    assert "shares_outstanding" not in {f["field"] for f in got.security_facts}
