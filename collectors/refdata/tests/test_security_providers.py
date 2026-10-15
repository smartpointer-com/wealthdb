"""The security providers: gleif, openfigi and sec-edgar, on invented
answers in each API's layout."""

from datetime import date

import pytest

import fetcher
import providers.gleif as gleif_mod
from providers.base import Context, ProviderError
from providers.gleif import Gleif, parse_records
from providers.openfigi import API as FIGI, OpenFigi, parse_mapping
from providers.sec_edgar import SecEdgar, latest_shares, names_agree, parse_company
from tests.helpers import FakeClient
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


def _mapping(url, jobs):
    return [{"warning": "No identifier found."} if job["idValue"] == "QNONE" else
            {"data": [{"figi": "BBG000000001", "compositeFIGI": "BBG000000002", "name": "EXAMPLE CO",
                       "ticker": job["idValue"], "exchCode": "US", "securityType": "Common Stock",
                       "marketSector": "Equity"}]}
            for job in jobs]


def batches(client):
    return [len(jobs) for _, _, jobs in client.asked]


def test_openfigi_asks_five_securities_a_request(tmp_path):
    client = FakeClient({FIGI: _mapping})
    ctx = Context(client=client)
    p = OpenFigi()
    claims = [p.claim_security(Security(key=f"symbol:Q{i}"), ctx) for i in range(7)]
    for i, c in enumerate(claims):
        p.fetch_security(c, tmp_path / str(i), ctx)
    assert batches(client) == [5, 2]
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


def tickers(doc):
    return FakeClient({"company_tickers.json": doc})


def test_sec_edgar_claims_a_listed_ticker():
    ctx = Context(client=tickers({"0": {"cik_str": 7, "ticker": "QEXI"}}))
    e = SecEdgar()
    assert e.claim_security(Security(key="symbol:QEXI"), ctx)["cik"] == 7
    assert e.claim_security(Security(key="symbol:QZZZ"), ctx) is None


def test_a_failed_openfigi_batch_fails_its_own_securities_only(tmp_path):
    client = FakeClient({FIGI: 400})
    ctx = Context(client=client)
    p = OpenFigi()
    claims = [p.claim_security(Security(key=f"symbol:Q{i}"), ctx) for i in range(7)]
    for i, c in enumerate(claims):
        with pytest.raises((ProviderError, fetcher.FetchError)):
            p.fetch_security(c, tmp_path / str(i), ctx)
    # One request per batch: the failed five are not asked again.
    assert batches(client) == [5, 2]


def test_openfigi_batches_the_jobs_still_waiting_whatever_the_order(tmp_path):
    client = FakeClient({FIGI: _mapping})
    ctx = Context(client=client)
    p = OpenFigi()
    claims = [p.claim_security(Security(key=f"symbol:Q{i}"), ctx) for i in range(8)]
    # The last first: it takes the first four waiting along.
    for i in (7, 0, 5, 6):
        p.fetch_security(claims[i], tmp_path / str(i), ctx)
    assert [[job["idValue"] for job in jobs] for _, _, jobs in client.asked] == [
        ["Q7", "Q0", "Q1", "Q2", "Q3"], ["Q5", "Q4", "Q6"]]


def test_an_openfigi_answer_of_the_wrong_length_is_an_error(tmp_path):
    ctx = Context(client=FakeClient({FIGI: []}))
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


CLAIM = {"key": "symbol:QEXI", "symbol": "QEXI", "cik": 7}


def test_sec_edgar_without_shares_is_no_error(tmp_path):
    ctx = Context(client=FakeClient({"companyconcept": 404, "submissions": SUB}))
    e = SecEdgar()
    e.fetch_security(CLAIM, tmp_path, ctx)
    got = e.parse(tmp_path)
    assert "shares_outstanding" not in {f["field"] for f in got.security_facts}


def test_sec_edgar_fails_on_a_submissions_file_that_is_not_json_or_shares_that_fail(tmp_path):
    with pytest.raises(fetcher.FetchError, match="not JSON"):
        SecEdgar().fetch_security(CLAIM, tmp_path / "a", Context(client=FakeClient({"submissions": "<html/>"})))
    with pytest.raises(fetcher.FetchError, match="503"):
        SecEdgar().fetch_security(CLAIM, tmp_path / "b",
                                  Context(client=FakeClient({"companyconcept": 503, "submissions": SUB})))
    assert not (tmp_path / "b" / "meta.json").exists()


def test_a_constituent_is_asked_only_what_a_provider_answers():
    def c(key, missing, **kw):
        return Security(key=key, scope="constituent", missing=missing, asset_class=kw.pop("asset_class", "public_equity"),
                        **kw)
    us_stock = c("isin:US0000000081", ("sector",))
    no_country = c("isin:XX0000000082", ("country",))
    with_symbol = c("isin:US0000000083", ("sector",), symbol="QSYM")
    us_bond = c("isin:US0000000084", ("sector",), asset_class="fixed_income")
    cins = c("cusip:Q00000008", ("sector",))
    us_cusip = c("cusip:000000AH4", ("sector",))
    no_sector_abroad = c("isin:XX0000000085", ("sector",))
    g, f, e = Gleif(), OpenFigi(), SecEdgar()
    # GLEIF states a country, and leads a stock under a US ISIN (a
    # receipt's) to its issuer's share; not a bond or a share abroad.
    assert g.wants_constituent(no_country) and g.wants_constituent(us_stock)
    assert not g.wants_constituent(us_bond) and not g.wants_constituent(no_sector_abroad)
    # OpenFIGI finds the US ticker of a US stock lacking a sector; not for a
    # bond, a known ticker, a country gap or a CINS code.
    assert f.wants_constituent(us_stock) and f.wants_constituent(us_cusip)
    for s in (no_country, with_symbol, us_bond, cins):
        assert not f.wants_constituent(s), s.key
    assert e.wants_constituent(with_symbol)


def test_sec_edgar_matches_a_share_class_whatever_its_punctuation():
    ctx = Context(client=tickers({"0": {"cik_str": 9, "ticker": "QBR-B"}, "1": {"cik_str": 10, "ticker": "QTA"},
                                  "2": {"cik_str": 11, "ticker": "QT-A"}}))
    e = SecEdgar()
    for written in ("QBRB", "QBR.B", "QBR/B"):
        assert e.claim_security(Security(key=f"symbol:{written}", symbol=written), ctx)["cik"] == 9
    # A plain ticker is itself; a share class never takes a plain ticker's place.
    assert e.claim_security(Security(key="symbol:QTA", symbol="QTA"), ctx)["cik"] == 10
    assert e.claim_security(Security(key="symbol:QT.A", symbol="QT.A"), ctx)["cik"] == 11


def test_sec_edgar_claims_a_ticker_abroad_only_where_the_names_agree():
    ctx = Context(client=tickers({"0": {"cik_str": 21, "ticker": "QEB", "title": "Quillmore Biologics Inc."},
                                  "1": {"cik_str": 22, "ticker": "QLB", "title": "QUELLBRIDGE LTD"}}))
    e = SecEdgar()
    # A bank abroad whose local ticker is a US biotech's: not claimed.
    bank = Security(key="isin:XA0000000611", isin="XA0000000611", symbol="QEB", name="Quarzberg Bank AG")
    assert e.claim_security(bank, ctx) is None
    # A company abroad listed in New York under its own name: claimed.
    listed = Security(key="isin:XB0000000612", isin="XB0000000612", symbol="QLB", name="Quellbridge Limited")
    assert e.claim_security(listed, ctx)["cik"] == 22
    # A US ISIN, or no name to compare, keeps the ticker's answer.
    assert e.claim_security(Security(key="isin:US0000000613", symbol="QEB", name="Other"), ctx)["cik"] == 21
    assert e.claim_security(Security(key="isin:XA0000000614", symbol="QEB"), ctx)["cik"] == 21
    # A local ticker with no ISIN, priced abroad: the names must agree too.
    local = Security(key="symbol:QEB", symbol="QEB", name="Quarzberg Bank AG", currency="EUR")
    assert e.claim_security(local, ctx) is None
    assert e.claim_security(Security(key="symbol:QEB", symbol="QEB", name="Other", currency="USD"), ctx)["cik"] == 21


def test_names_agree_on_the_first_telling_word():
    assert names_agree("Quellmark Inc", "QUELLMARK HOLDINGS LTD")
    assert names_agree("QUELLMARK", "Quellmarkets Corp")
    assert not names_agree("Quarzberg Bank AG", "Quillmore Biologics Inc.")
    # A word of two letters agrees only with itself.
    assert names_agree("QX Corp", "QX HOLDINGS") and not names_agree("QX Corp", "QXANTA Inc")


def test_sec_edgar_reads_an_address_abroad_by_its_country_code():
    sub = {**SUB, "stateOfIncorporation": "",
           "addresses": {"business": {"city": "TORONTO", "stateOrCountry": None, "countryCode": "A6",
                                      "isForeignLocation": 1}}}
    p = parse_company(sub, None, "symbol:QEXI", DAY, provider="sec-edgar")
    assert {f["field"]: f["value"] for f in p.security_facts}["hq_country"] == "A6"


def test_sec_edgar_states_no_address_for_a_depositary_receipt():
    office = {"business": {"city": "NEW YORK", "stateOrCountry": "NY"}}
    sub = {**SUB, "sic": "8880", "stateOfIncorporation": "M0", "addresses": office}
    p = parse_company(sub, None, "symbol:QEXIY", DAY, provider="sec-edgar")
    assert {f["field"]: f["value"] for f in p.security_facts} == {"sic": "8880", "incorporation": "M0"}
    # EDGAR names a registrant with no SIC code COMPANY/ADR.
    sub = {**SUB, "name": "Example Industries Corporation/ADR", "sic": "", "stateOfIncorporation": "M0",
           "addresses": office}
    p = parse_company(sub, None, "symbol:QEXIY", DAY, provider="sec-edgar")
    assert {f["field"]: f["value"] for f in p.security_facts} == {"incorporation": "M0"}


def test_gleif_fetches_the_issuer_isins_of_a_receipt(tmp_path):
    record = {"data": [{"id": "XXXXXXXXXXXXXXXXXX31", "attributes": {"lei": "XXXXXXXXXXXXXXXXXX31", "entity": {
        "legalName": {"name": "Qorvant Holdings"}, "jurisdiction": "KR", "legalAddress": {"country": "KR"}}}}]}
    isins = {"data": [{"attributes": {"isin": "US0000000641"}}, {"attributes": {"isin": "XK0000000642"}}]}
    g, api = Gleif(), FakeClient({"/isins": isins, "lei-records": record})
    ctx = Context(client=api)
    receipt = Security(key="isin:US0000000641", isin="US0000000641", asset_class="public_equity")
    g.fetch_security(g.claim_security(receipt, ctx), tmp_path / "r", ctx)
    assert any(u.endswith("XXXXXXXXXXXXXXXXXX31/isins") for u in api.urls())
    facts = [(f["field"], f["value"]) for f in g.parse(tmp_path / "r").security_facts]
    assert ("issuer_isins", "XK0000000642") in facts
    # A bond, or a share under an ISIN from abroad, asks for no ISIN list.
    for sec in (Security(key="isin:US0000000643", isin="US0000000643", asset_class="fixed_income"),
                Security(key="isin:XK0000000644", isin="XK0000000644", asset_class="public_equity")):
        api.asked.clear()
        g.fetch_security(g.claim_security(sec, ctx), tmp_path / sec.key[5:], ctx)
        assert not any(u.endswith("/isins") for u in api.urls()), sec.key


def test_gleif_pages_the_issuer_isins_up_to_its_cap_and_keeps_the_issuer_without_them(tmp_path, monkeypatch):
    monkeypatch.setattr(gleif_mod, "ISIN_PAGE_SIZE", 2)
    monkeypatch.setattr(gleif_mod, "ISIN_PAGES", 3)
    record = {"data": [{"id": "XXXXXXXXXXXXXXXXXX32", "attributes": {"lei": "XXXXXXXXXXXXXXXXXX32", "entity": {
        "legalName": {"name": "Qorvant Holdings"}, "jurisdiction": "KR", "legalAddress": {"country": "KR"}}}}]}

    def page(url, params):
        n = int(params["page[number]"])
        return {"data": [{"attributes": {"isin": f"XK00000006{n}{i}"}} for i in range(2)]}
    g = Gleif()
    receipt = Security(key="isin:US0000000651", isin="US0000000651", asset_class="public_equity")
    api = FakeClient({"/isins": page, "lei-records": record})
    ctx = Context(client=api)
    g.fetch_security(g.claim_security(receipt, ctx), tmp_path / "full", ctx)
    # Every page full: the cap stops it.
    assert [d["page[number]"] for _, u, d in api.asked if u.endswith("/isins")] == ["1", "2", "3"]
    (fact,) = [f for f in g.parse(tmp_path / "full").security_facts if f["field"] == "issuer_isins"]
    assert len(fact["value"].split()) == 6
    # The list failing leaves the issuer's own answer.
    ctx = Context(client=FakeClient({"/isins": 503, "lei-records": record}))
    g.fetch_security(g.claim_security(receipt, ctx), tmp_path / "failed", ctx)
    fields = {f["field"] for f in g.parse(tmp_path / "failed").security_facts}
    assert "legal_country" in fields and "issuer_isins" not in fields


def test_gleif_fails_on_an_answer_that_is_not_json(tmp_path):
    ctx = Context(client=FakeClient({"lei-records": "<html>busy</html>"}))
    with pytest.raises(fetcher.FetchError, match="not JSON"):
        Gleif().fetch_security({"key": "isin:XX0000000607", "isin": "XX0000000607", "equity": True}, tmp_path, ctx)


def test_the_security_probes_ask_one_cheap_question_each():
    client = FakeClient({FIGI: [{"data": []}], "lei-records": {"data": []},
                         "company_tickers.json": {"0": {"cik_str": 7, "ticker": "QEXI"}}})
    ctx = Context(client=client)
    assert OpenFigi().probe(ctx) == "OpenFIGI mapping answered 200"
    assert Gleif().probe(ctx) == "GLEIF API answered 200"
    assert SecEdgar().probe(ctx) == "SEC company ticker file: 1 tickers"
    assert len(client.asked) == 3
