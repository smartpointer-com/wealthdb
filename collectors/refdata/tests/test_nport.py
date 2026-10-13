"""The N-PORT parser on an invented filing in the SEC's layout."""

from datetime import date
from pathlib import Path

import pytest

import fetcher
from providers.base import Context, ProviderError
from providers.sec_nport import SecNport, _atom_accessions, _submission_filings, parse_nport
from wanted import Fund

FIXTURE = Path(__file__).parent / "fixtures" / "nport_primary_doc.xml"


def parsed():
    return parse_nport(FIXTURE.read_bytes(), "symbol:EXMP", provider="sec-nport", symbol="EXMP")


def test_composition_is_dated_by_the_report_period():
    p = parsed()
    (c,) = p.compositions
    assert c["as_of"] == date(2026, 6, 30)
    assert c["basis"] == "holdings" and c["source_kind"] == "filing"
    assert c["net_assets"] == "1000000" and c["line_count"] == 4
    assert c["weight_sum"] == "100" and c["fund_name"] == "Example Broad Market ETF"


def test_lines_keep_the_filed_labels():
    a, b, fwd, bond = parsed().lines
    assert (a["isin"], a["cusip"], a["weight"], a["currency"], a["country"], a["asset_type"]) == \
        ("XX0000000101", "000000AA1", "60", "USD", "US", "EC")
    assert a["payload"]["lei"] == "XXXXXXXXXXXXXXXXXX01"
    # The currency and asset category can sit in conditional elements.
    assert (b["currency"], b["asset_type"], b["ticker"], b["cusip"]) == ("EUR", "OTHER", "EXIA", None)
    assert a["sector"] is None


def test_a_forward_states_its_legs_and_counterparty():
    p = parsed()
    fwd = p.lines[2]
    assert (fwd["long_currency"], fwd["long_amount"], fwd["short_currency"], fwd["short_amount"]) == \
        ("EUR", "45000", "USD", "50000")
    (cp,) = p.counterparties
    assert (cp["role"], cp["name"], cp["lei"], cp["share"]) == \
        ("derivative", "Example Bank NA", "XXXXXXXXXXXXXXXXXX09", "0.1")


def test_a_bond_states_maturity_and_coupon():
    bond = parsed().lines[3]
    assert (bond["maturity"], bond["coupon"]) == ("2030-05-15", "2.5")


def test_not_an_nport_document():
    with pytest.raises(ProviderError):
        parse_nport(b"<html></html>", "symbol:X", provider="sec-nport")


def test_filing_lists():
    sub = {"filings": {"recent": {
        "form": ["NPORT-P", "10-K", "NPORT-P/A", "NPORT-P"],
        "accessionNumber": ["0000000001-26-000001", "x", "0000000001-26-000003", "0000000001-25-000009"],
        "filingDate": ["2026-08-28", "2026-03-01", "2026-09-10", "2025-11-27"],
        "reportDate": ["2026-06-30", "", "2026-06-30", "2025-09-30"]}}}
    got = _submission_filings(sub)
    assert [f["accession"] for f in got] == ["0000000001-26-000001", "0000000001-26-000003",
                                             "0000000001-25-000009"]
    atom = b"""<feed xmlns="http://www.w3.org/2005/Atom"><entry><content>
        <accession-number>0000000001-26-000001</accession-number></content></entry></feed>"""
    assert _atom_accessions(atom) == ["0000000001-26-000001"]


class _RefusingClient:
    """Answers every request with a 403, and counts them."""

    def __init__(self):
        self.urls = []

    def get(self, url, **_):
        self.urls.append(url)
        raise fetcher.FetchError(url, 403, "Forbidden")


def test_a_refused_ticker_file_is_asked_for_once():
    client = _RefusingClient()
    ctx = Context(client=client, contact="ops@example.org")
    p = SecNport()
    for sym in ("AAAA", "BBBB", "CCCC"):
        with pytest.raises(fetcher.FetchError):
            p.claim(Fund(ref=f"symbol:{sym}", symbol=sym), ctx)
    assert len(client.urls) == 1


FIXTURE = Path(__file__).parent / "fixtures" / "nport_primary_doc.xml"


class _FakeSec:
    """An invented corner of EDGAR: one fund series, one UIT, one company
    that files no N-PORT."""

    def __init__(self, doc=None):
        from providers import sec_nport as m
        self.m, self.urls = m, []
        recent = {"form": ["NPORT-P", "NPORT-P/A", "NPORT-P"],
                  "accessionNumber": ["0000000001-26-000001", "0000000001-26-000002", "0000000001-26-000003"],
                  "filingDate": ["2026-08-28", "2026-09-10", "2026-08-29"],
                  "reportDate": ["2026-06-30", "2026-06-30", "2026-06-30"]}
        older = {"form": ["NPORT-P"], "accessionNumber": ["0000000001-25-000004"],
                 "filingDate": ["2025-02-27"], "reportDate": ["2024-12-31"]}
        self.json = {
            m.FUND_TICKERS: {"fields": ["cik", "seriesId", "classId", "symbol"],
                             "data": [[1, "S000000001", "C000000001", "QFND"]]},
            m.COMPANY_TICKERS: {"0": {"cik_str": 2, "ticker": "QUIT"}, "1": {"cik_str": 3, "ticker": "QCOM"}},
            m.SUBMISSIONS.format(cik=1): {"filings": {"recent": recent, "files": [
                {"name": "CIK0000000001-submissions-001.json", "filingFrom": "2023-01-02", "filingTo": "2025-06-30"}]}},
            m.SUBMISSIONS_PAGE.format(name="CIK0000000001-submissions-001.json"): older,
            m.SUBMISSIONS.format(cik=2): {"filings": {"recent": {
                "form": ["NPORT-P"], "accessionNumber": ["0000000002-26-000001"],
                "filingDate": ["2026-05-28"], "reportDate": ["2026-03-31"]}}},
            m.SUBMISSIONS.format(cik=3): {"filings": {"recent": {
                "form": ["10-K"], "accessionNumber": ["0000000003-26-000001"],
                "filingDate": ["2026-02-28"], "reportDate": ["2025-12-31"]}}},
        }
        # The series' own filings: not the third, another series' filing.
        self.atom = b"""<feed xmlns="http://www.w3.org/2005/Atom">""" + b"".join(
            b"<entry><content><accession-number>%s</accession-number></content></entry>" % a
            for a in (b"0000000001-26-000001", b"0000000001-26-000002", b"0000000001-25-000004")) + b"</feed>"
        self.doc = doc if doc is not None else FIXTURE.read_bytes()

    def get(self, url, params=None):
        self.urls.append(url)
        if url == self.m.BROWSE:
            return fetcher.Response(url, 200, self.atom, "application/atom+xml")
        if url in self.json:
            import json
            return fetcher.Response(url, 200, json.dumps(self.json[url]).encode(), "application/json")
        if url.endswith("/primary_doc.xml"):
            return fetcher.Response(url, 200, self.doc, "text/xml")
        raise fetcher.FetchError(url, 404, "Not Found")


def _ctx(client):
    return Context(client=client, contact="ops@example.org")


def test_a_series_takes_its_own_filings_and_an_amendment_wins():
    sec = _FakeSec()
    p = SecNport()
    claim = p.claim(Fund(ref="symbol:QFND", symbol="QFND"), _ctx(sec))
    assert claim["series"] == "S000000001"
    filings = p._filings(claim, _ctx(sec))
    assert list(filings) == [date(2026, 6, 30)]
    assert filings[date(2026, 6, 30)]["accession"] == "0000000001-26-000002"


def test_backfill_reads_older_pages_only_as_far_back_as_asked():
    sec = _FakeSec()
    p, ctx = SecNport(), _ctx(sec)
    claim = p.claim(Fund(ref="symbol:QFND", symbol="QFND"), ctx)
    assert p.dates(claim, date(2025, 7, 1), date(2026, 12, 31), ctx) == [date(2026, 6, 30)]
    assert not any("submissions-001" in u for u in sec.urls)
    assert p.dates(claim, date(2024, 1, 1), date(2026, 12, 31), ctx) == [date(2024, 12, 31), date(2026, 6, 30)]


def test_a_uit_is_claimed_and_a_company_that_files_no_nport_is_not():
    sec = _FakeSec()
    p, ctx = SecNport(), _ctx(sec)
    uit = p.claim(Fund(ref="symbol:QUIT", symbol="QUIT"), ctx)
    assert uit["cik"] == 2 and uit["series"] is None
    assert p.latest(uit, ctx) == date(2026, 3, 31)
    assert p.claim(Fund(ref="symbol:QCOM", symbol="QCOM"), ctx) is None


def test_a_filing_is_kept_compressed_and_parses_back(tmp_path):
    sec = _FakeSec()
    p, ctx = SecNport(), _ctx(sec)
    claim = p.claim(Fund(ref="symbol:QFND", symbol="QFND"), ctx)
    assert p.fetch(claim, None, tmp_path, ctx) == date(2026, 6, 30)
    assert (tmp_path / "primary_doc.xml.zst").is_file() and not (tmp_path / "primary_doc.xml").exists()
    got = p.parse(tmp_path)
    assert got.compositions[0]["fund_ref"] == "symbol:QFND" and got.lines


def test_a_body_that_is_no_nport_document_is_an_error(tmp_path):
    sec = _FakeSec(doc=b"<html>maintenance</html>")
    p, ctx = SecNport(), _ctx(sec)
    claim = p.claim(Fund(ref="symbol:QFND", symbol="QFND"), ctx)
    with pytest.raises(ProviderError, match="not an N-PORT document"):
        p.fetch(claim, None, tmp_path, ctx)


SWAP_DOC = b"""<edgarSubmission xmlns="http://www.sec.gov/edgar/nport"><formData>
  <genInfo><seriesName>Example Swap ETF</seriesName><repPdDate>2026-06-30</repPdDate></genInfo>
  <fundInfo><netAssets>1000.00</netAssets></fundInfo>
  <invstOrSecs>
    <invstOrSec><name>Total return swap</name><lei>N/A</lei><curCd>USD</curCd><pctVal>60</pctVal>
      <assetCat>DE</assetCat><derivativeInfo><swapDeriv derivCat="SWP">
        <counterparties><counterpartyName>Example Bank A</counterpartyName><counterpartyLei>N/A</counterpartyLei></counterparties>
      </swapDeriv></derivativeInfo></invstOrSec>
    <invstOrSec><name>Total return swap</name><curCd>USD</curCd><pctVal>-40</pctVal>
      <assetCat>DE</assetCat><derivativeInfo><swapDeriv derivCat="SWP">
        <counterparties><counterpartyName>Example Bank A</counterpartyName></counterparties>
        <counterparties><counterpartyName>Example Bank B</counterpartyName><counterpartyLei>XXXXXXXXXXXXXXXXXX11</counterpartyLei></counterparties>
      </swapDeriv></derivativeInfo></invstOrSec>
  </invstOrSecs></formData></edgarSubmission>"""


def test_swap_counterparties_add_up_and_na_is_no_lei():
    p = parse_nport(SWAP_DOC, "symbol:QSWP", provider="sec-nport")
    assert [(c["role"], c["name"], c["lei"], c["share"]) for c in p.counterparties] == [
        ("swap", "Example Bank A", None, "100"), ("swap", "Example Bank B", "XXXXXXXXXXXXXXXXXX11", "40")]
    assert "lei" not in p.lines[0]["payload"]


def test_an_unknown_country_is_no_country():
    doc = SWAP_DOC.replace(b"<assetCat>DE</assetCat>", b"<assetCat>DE</assetCat><invCountry>XX</invCountry>", 1)
    p = parse_nport(doc, "symbol:QSWP", provider="sec-nport")
    assert p.lines[0]["country"] is None
