"""The N-MFP provider on invented filings in the SEC's layout, and its
dates from an invented corner of EDGAR."""

from datetime import date
from pathlib import Path

import pytest

import load
from providers import openfigi
from providers import sec_nport as nport
from providers.base import Context, ProviderError, Unavailable
from providers.sec_nmfp import SecNmfp, parse_nmfp
from providers.sec_nport import SecNport
from tests.helpers import FakeClient, figi, go_source, go_strings, run_ctx, silver_db
from wanted import Fund

FIXTURES = Path(__file__).parent / "fixtures"
MFP3 = (FIXTURES / "nmfp_mfp3_primary_doc.xml").read_bytes()
MFP2 = (FIXTURES / "nmfp_mfp2_primary_doc.xml").read_bytes()
REF = "symbol:EXMYY"


def figures(parsed):
    return [(r["as_of"], r["field"], r["value"]) for r in parsed.characteristics]


def test_an_nmfp3_states_each_days_yields_and_the_month_ends_maturities():
    p = parse_nmfp(MFP3, REF, "C000000002", provider="sec-nmfp")
    d26, d29, d30 = date(2026, 6, 26), date(2026, 6, 29), date(2026, 6, 30)
    assert figures(p) == [
        (d26, "seven_day_gross_yield", "5.31"), (d26, "seven_day_yield", "4.91"),
        (d29, "seven_day_gross_yield", "5.32"), (d29, "seven_day_yield", "4.92"),
        (d30, "seven_day_gross_yield", "5.3"), (d30, "seven_day_yield", "4.9"),
        (d30, "weighted_average_life", "61"), (d30, "weighted_average_maturity", "28"),
    ]
    assert {(r["fund_ref"], r["provider"], r["label"]) for r in p.characteristics} == {(REF, "sec-nmfp", "")}
    net = next(r for r in p.characteristics if r["field"] == "seven_day_yield")
    gross = next(r for r in p.characteristics if r["field"] == "seven_day_gross_yield")
    assert net["payload"] == {"series": "S000000001", "class": "C000000002"}
    assert gross["payload"] == {"series": "S000000001"}
    # Nothing else: no composition, no line, not the portfolio's yields.
    assert not p.compositions and not p.lines


def test_the_claimed_class_gives_the_net_yield():
    p = parse_nmfp(MFP3, "symbol:EXMXX", "C000000001", provider="sec-nmfp")
    assert [v for _, f, v in figures(p) if f == "seven_day_yield"] == ["5.11", "5.12", "5.1"]


def test_an_nmfp2_states_its_figures_for_the_report_date():
    p = parse_nmfp(MFP2, "symbol:EXMXX", "C000000001", provider="sec-nmfp")
    may = date(2024, 5, 31)
    assert figures(p) == [(may, "seven_day_gross_yield", "5.4"), (may, "seven_day_yield", "5.18"),
                          (may, "weighted_average_life", "72"), (may, "weighted_average_maturity", "35")]


@pytest.mark.parametrize("doc", [MFP3, MFP2])
def test_a_class_the_filing_does_not_list_gives_no_net_yield(doc):
    for class_id in ("C000000009", None):
        fields = {f for _, f, _ in figures(parse_nmfp(doc, REF, class_id, provider="sec-nmfp"))}
        assert fields == {"seven_day_gross_yield", "weighted_average_life", "weighted_average_maturity"}


def test_a_negative_yield_and_a_day_listed_twice():
    doc = MFP3.replace(b"<sevenDayGrossYieldValue>0.0531</sevenDayGrossYieldValue>",
                       b"<sevenDayGrossYieldValue>-0.0001</sevenDayGrossYieldValue>")
    doc = doc.replace(b"<sevenDayGrossYieldDate>2026-06-29</sevenDayGrossYieldDate>",
                      b"<sevenDayGrossYieldDate>2026-06-30</sevenDayGrossYieldDate>")
    gross = [(d, v) for d, f, v in figures(parse_nmfp(doc, REF, None, provider="sec-nmfp"))
             if f == "seven_day_gross_yield"]
    # The day's last entry stands.
    assert gross == [(date(2026, 6, 26), "-0.01"), (date(2026, 6, 30), "5.3")]


def test_the_fields_are_those_gold_reads_as_money_market_figures():
    rows = [r for doc in (MFP3, MFP2) for r in parse_nmfp(doc, REF, "C000000002", provider="sec-nmfp").characteristics]
    assert {r["field"] for r in rows} == set(go_strings(go_source("gold/refdata_wanted.go"), "moneyMarketFields"))


def test_the_rows_load_into_silver(tmp_path):
    conn = silver_db(tmp_path)
    assert load.write(conn, parse_nmfp(MFP3, REF, "C000000002", provider="sec-nmfp"), run_at=1) == 8
    assert conn.execute("SELECT COUNT(DISTINCT as_of) FROM characteristics").fetchone()[0] == 3


@pytest.mark.parametrize("doc,error", [
    (b"<edgarSubmission><formData>", "document is not XML"),
    (b"<html></html>", "has no generalInfo or seriesLevelInfo"),
    (b"<edgarSubmission><formData><generalInfo><seriesId>S000000001</seriesId></generalInfo>"
     b"<seriesLevelInfo/></formData></edgarSubmission>", "states no report date"),
])
def test_a_document_that_does_not_read_is_refused(doc, error):
    with pytest.raises(ProviderError, match=error):
        parse_nmfp(doc, REF, "C000000002", provider="sec-nmfp")


RECENT = {"form": ["N-MFP3", "N-MFP3/A", "N-MFP3", "NPORT-P", "N-MFP3"],
          "accessionNumber": ["0000000001-26-000010", "0000000001-26-000011", "0000000001-26-000012",
                              "0000000001-26-000013", "0000000001-26-000008"],
          "filingDate": ["2026-07-08", "2026-07-20", "2026-07-08", "2026-05-28", "2026-06-05"],
          "reportDate": ["2026-06-30", "2026-06-30", "2026-06-30", "2026-03-31", "2026-05-29"]}
OLDER = {"form": ["N-MFP2", "N-MFP2"], "accessionNumber": ["0000000001-24-000005", "0000000001-24-000006"],
         "filingDate": ["2024-06-07", "2024-06-07"], "reportDate": ["2024-05-31", "2024-05-31"]}
# Each series' own filings, per form: the third of RECENT and the second
# of OLDER are another series' of the same registrant.
FEEDS = {("S000000001", "N-MFP3"): ("0000000001-26-000010", "0000000001-26-000011", "0000000001-26-000008"),
         ("S000000001", "N-MFP2"): ("0000000001-24-000005",)}


def feed(url, params):
    return b"""<feed xmlns="http://www.w3.org/2005/Atom">""" + b"".join(
        b"<entry><content><accession-number>%s</accession-number></content></entry>" % a.encode()
        for a in FEEDS.get((params["CIK"], params["type"]), ())) + b"</feed>"


TICKERS = {"fields": ["cik", "seriesId", "classId", "symbol"],
           "data": [[1, "S000000001", "C000000001", "EXMXX"], [1, "S000000001", "C000000002", "EXMYY"],
                    [1, "S000000002", "C000000003", "EXBND"], [5, None, None, "EXNOS"]]}


def edgar(doc=None):
    """An invented corner of EDGAR: a trust with a money-market series of
    two classes and a bond series that files no N-MFP, and a ticker with
    no series."""
    return FakeClient({
        nport.BROWSE: feed,
        nport.FUND_TICKERS: TICKERS,
        nport.SUBMISSIONS.format(cik=1): {"filings": {"recent": RECENT, "files": [
            {"name": "CIK0000000001-submissions-001.json", "filingFrom": "2023-01-02", "filingTo": "2024-07-31"}]}},
        nport.SUBMISSIONS_PAGE.format(name="CIK0000000001-submissions-001.json"): OLDER,
        "/000000000124000005/primary_doc.xml": MFP2,
        "/primary_doc.xml": MFP3 if doc is None else doc,
    })


def wanting(symbol, isin=None, wants=("characteristics", "money_market")):
    return Fund(ref=f"symbol:{symbol}", symbol=symbol, isin=isin, wants=wants)


def test_a_fund_is_claimed_only_when_it_wants_money_market_figures():
    sec = edgar()
    p, ctx = SecNmfp(), Context(client=sec)
    assert p.claim(wanting("EXMYY", wants=("characteristics",)), ctx) is None
    assert p.claim(Fund(ref="symbol:EXMYY", symbol="EXMYY"), ctx) is None
    assert sec.asked == []
    claim = p.claim(wanting("EXMYY"), ctx)
    assert (claim["ref"], claim["cik"], claim["series"], claim["class"]) == (REF, 1, "S000000001", "C000000002")


def test_a_fund_is_found_by_its_us_ticker_in_a_series_that_files_nmfp():
    p, ctx = SecNmfp(), Context(client=edgar())
    assert p.claim(wanting("EXMXX", isin="US0000000001"), ctx)["class"] == "C000000001"
    # Another country's ISIN, a series that files no N-MFP, a ticker with
    # no series, and a ticker the SEC does not list are not claimed.
    assert p.claim(wanting("EXMXX", isin="XX0000000001"), ctx) is None
    assert p.claim(wanting("EXBND"), ctx) is None
    assert p.claim(wanting("EXNOS"), ctx) is None
    assert p.claim(wanting("EXNONE"), ctx) is None


def test_the_series_filings_and_an_amendment_wins():
    sec = edgar()
    p, ctx = SecNmfp(), Context(client=sec)
    claim = p.claim(wanting("EXMYY"), ctx)
    assert p.latest(claim, ctx) == date(2026, 6, 30)
    filings = p._filings(claim, ctx)
    assert list(filings) == [date(2026, 5, 29), date(2026, 6, 30)]
    assert (filings[date(2026, 6, 30)]["accession"], filings[date(2026, 6, 30)]["form"]) == \
        ("0000000001-26-000011", "N-MFP3/A")
    # Both forms' feeds are asked, once a run.
    p.latest(claim, ctx)
    assert sorted(d["type"] for _, u, d in sec.asked if u == nport.BROWSE) == ["N-MFP2", "N-MFP3"]


def test_backfill_reads_older_pages_only_as_far_back_as_asked():
    sec = edgar()
    p, ctx = SecNmfp(), Context(client=sec)
    claim = p.claim(wanting("EXMYY"), ctx)
    assert p.dates(claim, date(2025, 1, 1), date(2026, 12, 31), ctx) == [date(2026, 5, 29), date(2026, 6, 30)]
    assert not any("submissions-001" in u for u in sec.urls())
    assert p.dates(claim, date(2024, 1, 1), date(2026, 5, 31), ctx) == [date(2024, 5, 31), date(2026, 5, 29)]


def test_a_filing_is_kept_compressed_and_parses_back(tmp_path):
    sec = edgar()
    p, ctx = SecNmfp(), Context(client=sec)
    claim = p.claim(wanting("EXMYY"), ctx)
    assert p.fetch(claim, None, tmp_path, ctx) == date(2026, 6, 30)
    assert sec.urls()[-1] == "https://www.sec.gov/Archives/edgar/data/1/000000000126000011/primary_doc.xml"
    assert (tmp_path / "primary_doc.xml.zst").is_file() and not (tmp_path / "primary_doc.xml").exists()
    meta = (tmp_path / "meta.json").read_text()
    for value in ('"series": "S000000001"', '"class": "C000000002"', '"accession": "0000000001-26-000011"',
                  '"form": "N-MFP3/A"', '"period": "2026-06-30"'):
        assert value in meta
    got = p.parse(tmp_path)
    assert {r["fund_ref"] for r in got.characteristics} == {REF}
    assert [r["value"] for r in got.characteristics if r["field"] == "seven_day_yield"] == ["4.91", "4.92", "4.9"]


def test_a_past_nmfp2_filing_is_fetched_from_an_older_page(tmp_path):
    p, ctx = SecNmfp(), Context(client=edgar())
    claim = p.claim(wanting("EXMYY"), ctx)
    assert p.fetch(claim, date(2024, 5, 31), tmp_path, ctx) == date(2024, 5, 31)
    assert '"form": "N-MFP2"' in (tmp_path / "meta.json").read_text()
    assert figures(p.parse(tmp_path))[1] == (date(2024, 5, 31), "seven_day_yield", "4.98")


def test_a_body_that_is_no_nmfp_document_is_an_error(tmp_path):
    p, ctx = SecNmfp(), Context(client=edgar(doc=b"<html>maintenance</html>"))
    claim = p.claim(wanting("EXMYY"), ctx)
    with pytest.raises(ProviderError, match="not an N-MFP document"):
        p.fetch(claim, None, tmp_path, ctx)


def test_a_period_with_no_filing_is_unavailable_and_a_folder_without_its_document_does_not_parse(tmp_path):
    p, ctx = SecNmfp(), Context(client=edgar())
    claim = p.claim(wanting("EXMYY"), ctx)
    with pytest.raises(Unavailable, match="no N-MFP filing for EXMYY for 2026-04-30"):
        p.fetch(claim, date(2026, 4, 30), tmp_path / "f", ctx)
    (tmp_path / "h").mkdir()
    (tmp_path / "h" / "meta.json").write_text('{"ref": "symbol:EXMYY", "class": "C000000002"}')
    with pytest.raises(ProviderError, match="no primary_doc.xml"):
        p.parse(tmp_path / "h")


def test_both_sec_fund_providers_read_one_ticker_file():
    sec = edgar()
    ctx = Context(client=sec)
    assert SecNport().claim(Fund(ref="symbol:EXMXX", symbol="EXMXX"), ctx)["series"] == "S000000001"
    assert SecNmfp().claim(wanting("EXMXX"), ctx)["series"] == "S000000001"
    assert sec.urls().count(nport.FUND_TICKERS) == 1


def test_the_probe_reads_the_fund_ticker_file():
    assert SecNmfp().probe(Context(client=edgar())) == "SEC fund ticker file: 4 tickers"


def test_a_fund_named_by_a_us_isin_alone_is_found_through_openfigi_once_for_both_providers():
    sec = figi(edgar(), {"US0000000001": [
        {"figi": "BBG000000201", "ticker": "EXMYY", "exchCode": "US", "name": "EXAMPLE MONEY FUND"}]})
    ctx = run_ctx(sec)
    fund = Fund(ref="isin:US0000000001", isin="US0000000001", wants=("money_market",))
    claim = SecNmfp().claim(fund, ctx)
    assert (claim["ref"], claim["symbol"], claim["class"]) == ("isin:US0000000001", "EXMYY", "C000000002")
    assert SecNport().claim(fund, ctx)["series"] == "S000000001"
    assert sec.urls().count(openfigi.API) == 1
    # A fund that wants no money-market figures is not looked up for them.
    assert SecNmfp().claim(Fund(ref="isin:US0000000002", isin="US0000000002"), ctx) is None
    assert sec.urls().count(openfigi.API) == 1
