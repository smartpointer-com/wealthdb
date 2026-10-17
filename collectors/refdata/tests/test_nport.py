"""The N-PORT parser on an invented filing in the SEC's layout."""

from datetime import date
from pathlib import Path

import pytest

import fetcher
from providers import sec_nport as m
from providers.base import Context, ProviderError, Unavailable
from providers.sec_edgar import SecEdgar
from providers.sec_nport import SecNport, _atom_accessions, _submission_filings, parse_nport
from tests.helpers import FakeClient
from wanted import Fund, Security

FIXTURE = Path(__file__).parent / "fixtures" / "nport_primary_doc.xml"


def parsed():
    return parse_nport(FIXTURE.read_bytes(), "symbol:EXMP", provider="sec-nport")


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


@pytest.mark.parametrize("read,error", [
    (lambda: parse_nport(b"<html></html>", "symbol:X", provider="sec-nport"), "has no genInfo"),
    (lambda: parse_nport(b"<edgarSubmission><genInfo>", "symbol:X", provider="sec-nport"), "document is not XML"),
    (lambda: parse_nport(b"<edgarSubmission><formData><genInfo><regName>Example Trust</regName></genInfo>"
                         b"</formData></edgarSubmission>", "symbol:X", provider="sec-nport"), "states no report date"),
    (lambda: _atom_accessions(b"<html><p>The feed is busy.</html>"), "filing feed is not XML"),
])
def test_a_document_or_feed_that_does_not_read_is_refused(read, error):
    with pytest.raises(ProviderError, match=error):
        read()


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


def test_a_refused_ticker_file_is_asked_for_once():
    client = FakeClient({"": 403})
    ctx = Context(client=client)
    p = SecNport()
    for sym in ("AAAA", "BBBB", "CCCC"):
        with pytest.raises(fetcher.FetchError):
            p.claim(Fund(ref=f"symbol:{sym}", symbol=sym), ctx)
    assert len(client.asked) == 1


RECENT = {"form": ["NPORT-P", "NPORT-P/A", "NPORT-P"],
          "accessionNumber": ["0000000001-26-000001", "0000000001-26-000002", "0000000001-26-000003"],
          "filingDate": ["2026-08-28", "2026-09-10", "2026-08-29"],
          "reportDate": ["2026-06-30", "2026-06-30", "2026-06-30"]}
OLDER = {"form": ["NPORT-P"], "accessionNumber": ["0000000001-25-000004"],
         "filingDate": ["2025-02-27"], "reportDate": ["2024-12-31"]}
# The series' own filings: not the third, another series' filing.
ATOM = b"""<feed xmlns="http://www.w3.org/2005/Atom">""" + b"".join(
    b"<entry><content><accession-number>%s</accession-number></content></entry>" % a
    for a in (b"0000000001-26-000001", b"0000000001-26-000002", b"0000000001-25-000004")) + b"</feed>"


def edgar(doc=None):
    """An invented corner of EDGAR: one fund series, one UIT, one company
    that files no N-PORT."""
    return FakeClient({
        m.BROWSE: ATOM,
        m.FUND_TICKERS: {"fields": ["cik", "seriesId", "classId", "symbol"],
                         "data": [[1, "S000000001", "C000000001", "QFND"]]},
        m.COMPANY_TICKERS: {"0": {"cik_str": 2, "ticker": "QUIT"}, "1": {"cik_str": 3, "ticker": "QCOM"}},
        m.SUBMISSIONS.format(cik=1): {"filings": {"recent": RECENT, "files": [
            {"name": "CIK0000000001-submissions-001.json", "filingFrom": "2023-01-02", "filingTo": "2025-06-30"}]}},
        m.SUBMISSIONS_PAGE.format(name="CIK0000000001-submissions-001.json"): OLDER,
        m.SUBMISSIONS.format(cik=2): {"filings": {"recent": {
            "form": ["NPORT-P"], "accessionNumber": ["0000000002-26-000001"],
            "filingDate": ["2026-05-28"], "reportDate": ["2026-03-31"]}}},
        m.SUBMISSIONS.format(cik=3): {"filings": {"recent": {
            "form": ["10-K"], "accessionNumber": ["0000000003-26-000001"],
            "filingDate": ["2026-02-28"], "reportDate": ["2025-12-31"]}}},
        "/primary_doc.xml": FIXTURE.read_bytes() if doc is None else doc,
    })


def test_a_series_takes_its_own_filings_and_an_amendment_wins():
    sec = edgar()
    ctx = Context(client=sec)
    p = SecNport()
    claim = p.claim(Fund(ref="symbol:QFND", symbol="QFND"), ctx)
    assert claim["series"] == "S000000001"
    filings = p._filings(claim, ctx)
    assert list(filings) == [date(2026, 6, 30)]
    assert filings[date(2026, 6, 30)]["accession"] == "0000000001-26-000002"


def test_a_fund_is_found_by_its_us_ticker_only():
    p, ctx = SecNport(), Context(client=edgar())
    # Named by a US ISIN, the fund is found by its ticker and keeps its
    # reference; a fund with another country's ISIN files no N-PORT, so
    # a US fund's ticker on it finds nothing.
    claim = p.claim(Fund(ref="isin:US0000000901", isin="US0000000901", symbol="QFND"), ctx)
    assert (claim["ref"], claim["series"]) == ("isin:US0000000901", "S000000001")
    assert p.claim(Fund(ref="isin:XX0000000901", isin="XX0000000901", symbol="QFND"), ctx) is None


def test_backfill_reads_older_pages_only_as_far_back_as_asked():
    sec = edgar()
    p, ctx = SecNport(), Context(client=sec)
    claim = p.claim(Fund(ref="symbol:QFND", symbol="QFND"), ctx)
    assert p.dates(claim, date(2025, 7, 1), date(2026, 12, 31), ctx) == [date(2026, 6, 30)]
    assert not any("submissions-001" in u for u in sec.urls())
    assert p.dates(claim, date(2024, 1, 1), date(2026, 12, 31), ctx) == [date(2024, 12, 31), date(2026, 6, 30)]


def test_a_uit_is_claimed_and_a_company_that_files_no_nport_is_not():
    p, ctx = SecNport(), Context(client=edgar())
    uit = p.claim(Fund(ref="symbol:QUIT", symbol="QUIT"), ctx)
    assert uit["cik"] == 2 and uit["series"] is None
    assert p.latest(uit, ctx) == date(2026, 3, 31)
    assert p.claim(Fund(ref="symbol:QCOM", symbol="QCOM"), ctx) is None


def test_a_filing_is_kept_compressed_and_parses_back(tmp_path):
    p, ctx = SecNport(), Context(client=edgar())
    claim = p.claim(Fund(ref="symbol:QFND", symbol="QFND"), ctx)
    assert p.fetch(claim, None, tmp_path, ctx) == date(2026, 6, 30)
    assert (tmp_path / "primary_doc.xml.zst").is_file() and not (tmp_path / "primary_doc.xml").exists()
    got = p.parse(tmp_path)
    assert got.compositions[0]["fund_ref"] == "symbol:QFND" and got.lines


def test_a_body_that_is_no_nport_document_is_an_error(tmp_path):
    p, ctx = SecNport(), Context(client=edgar(doc=b"<html>maintenance</html>"))
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


def test_a_period_with_no_filing_is_unavailable_and_a_folder_without_its_document_does_not_parse(tmp_path):
    p, ctx = SecNport(), Context(client=edgar())
    claim = p.claim(Fund(ref="symbol:QFND", symbol="QFND"), ctx)
    with pytest.raises(Unavailable, match="2026-03-31"):
        p.fetch(claim, date(2026, 3, 31), tmp_path / "f", ctx)
    with pytest.raises(Unavailable, match="no NPORT-P filing for QCOM"):
        p.fetch({"ref": "symbol:QCOM", "symbol": "QCOM", "cik": 3, "series": None}, None, tmp_path / "g", ctx)
    (tmp_path / "h").mkdir()
    (tmp_path / "h" / "meta.json").write_text('{"ref": "symbol:QFND"}')
    with pytest.raises(ProviderError, match="no primary_doc.xml"):
        p.parse(tmp_path / "h")


def test_both_sec_providers_read_one_company_ticker_file():
    sec = edgar()
    ctx = Context(client=sec)
    assert SecNport().claim(Fund(ref="symbol:QUIT", symbol="QUIT"), ctx)["cik"] == 2
    assert SecEdgar().claim_security(Security(key="symbol:QCOM"), ctx)["cik"] == 3
    assert sec.urls().count(m.COMPANY_TICKERS) == 1


def test_the_probe_reads_the_fund_ticker_file():
    assert SecNport().probe(Context(client=edgar())) == "SEC fund ticker file: 1 tickers"
