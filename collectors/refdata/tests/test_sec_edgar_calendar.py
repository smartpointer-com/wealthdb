"""sec-edgar's dividend calendar: the dividends a company's 10-Q and 10-K
filings tag in inline XBRL, which filings a request reads, when a company
is asked again, and the events a run loads, on invented filings in
EDGAR's layout."""

import io
import zipfile
from datetime import date, timedelta
from pathlib import Path

import pytest

import download
import load
import wanted as wantedmod
from collectorkit import compress
from providers import REGISTRY, sec_calendar, sec_edgar
from providers.base import Context, read_meta, write_meta
from providers.sec_calendar import Seen, due, months_back, seen
from providers.sec_edgar import SecEdgar
from tests.helpers import FakeClient, kept_run, make_run, rows, run_entries, wanted_doc

FIXTURE = Path(__file__).parent / "fixtures" / "edgar_10q_dividends.htm"
CAL = "dividend_calendar"
KEY = "symbol:EXMP"
TODAY = date(2026, 11, 2)
TICKERS = {"0": {"cik_str": 7, "ticker": "EXMP", "title": "EXAMPLE INDUSTRIES INC"}}
SUB = {"cik": "0000000007", "name": "EXAMPLE INDUSTRIES INC", "entityType": "operating", "sic": "3990",
       "tickers": ["EXMP"], "exchanges": ["NYSE"], "stateOfIncorporation": "DE",
       "addresses": {"business": {"stateOrCountry": "DE"}}}
# The company's recent filings as its submissions file lists them: an
# 8-K and an insider's form 4 besides its 10-Qs and 10-K, the oldest
# 10-Q filed more than fifteen months before TODAY.
FILINGS = (
    ("0000000007-26-000012", "2026-10-01", "", "8-K", 0, 0, "exmp-8k.htm"),
    ("0000000007-26-000011", "2026-10-28", "2026-09-30", "10-Q", 1, 1, "exmp-20260930.htm"),
    ("0000000007-26-000009", "2026-09-01", "", "4", 0, 0, "xslF345X05/form4.xml"),
    ("0000000007-26-000010", "2026-08-05", "2026-06-30", "10-Q", 1, 1, "exmp-20260630.htm"),
    ("0000000007-26-000006", "2026-05-06", "2026-03-31", "10-Q", 1, 1, "exmp-20260331.htm"),
    ("0000000007-26-000003", "2026-02-20", "2025-12-31", "10-K", 1, 1, "exmp-20251231.htm"),
    ("0000000007-25-000020", "2025-10-29", "2025-09-30", "10-Q", 1, 1, "exmp-20250930.htm"),
    ("0000000007-25-000014", "2025-07-30", "2025-06-30", "10-Q", 1, 1, "exmp-20250630.htm"),
)
ARCHIVES = "https://www.sec.gov/Archives/edgar/data/7/"


def submissions(*filings):
    keys = ("accessionNumber", "filingDate", "reportDate", "form", "isXBRL", "isInlineXBRL", "primaryDocument")
    return {**SUB, "filings": {"recent": {k: [f[i] for f in filings] for i, k in enumerate(keys)}, "files": []}}


def archive(accession):
    return f"{ARCHIVES}{accession.replace('-', '')}/{accession}-xbrl.zip"


def xbrl_zip(doc: bytes) -> bytes:
    """A filing's XBRL archive: its inline document, an exhibit in plain
    HTML and its schema."""
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as z:
        z.writestr("exmp-20260930.htm", doc)
        z.writestr("exmp-ex311.htm", EXHIBIT)
        z.writestr("exmp-20260930.xsd", b"<schema/>")
    return buf.getvalue()


EXHIBIT = b"<html><p>Certification<br></p></html>"  # a plain-HTML exhibit: no inline XBRL


def late(doc: bytes) -> bytes:
    """An inline document that declares its namespace past the bytes
    sniffed: only its name as the primary document tells it."""
    return b"<!--" + b" " * sec_calendar.SNIFF + b"-->" + doc.partition(b"?>")[2]


# ---- an inline XBRL document, built ------------------------------------

NS = ('xmlns="http://www.w3.org/1999/xhtml" xmlns:ix="http://www.xbrl.org/2013/inlineXBRL" '
      'xmlns:ixt="http://www.xbrl.org/inlineXBRL/transformation/2020-02-12" '
      'xmlns:xbrli="http://www.xbrl.org/2003/instance" xmlns:xbrldi="http://xbrl.org/2006/xbrldi" '
      'xmlns:iso4217="http://www.xbrl.org/2003/iso4217" xmlns:gaap="http://fasb.org/us-gaap/2024" '
      'xmlns:exmp="http://www.example.com/20260930"')
UNIT = ('<xbrli:unit id="usdPerShare"><xbrli:divide><xbrli:unitNumerator><xbrli:measure>iso4217:USD</xbrli:measure>'
        '</xbrli:unitNumerator><xbrli:unitDenominator><xbrli:measure>xbrli:shares</xbrli:measure>'
        '</xbrli:unitDenominator></xbrli:divide></xbrli:unit>')


def context(cid, start, end=None, **dims):
    """An xbrli:context: an instant, or the period start to end; each
    dimension a US GAAP axis and its member."""
    period = (f"<xbrli:instant>{start}</xbrli:instant>" if end is None
              else f"<xbrli:startDate>{start}</xbrli:startDate><xbrli:endDate>{end}</xbrli:endDate>")
    members = "".join(f'<xbrldi:explicitMember dimension="gaap:{axis}">{member}</xbrldi:explicitMember>'
                      for axis, member in dims.items())
    segment = f"<xbrli:segment>{members}</xbrli:segment>" if members else ""
    return (f'<xbrli:context id="{cid}"><xbrli:entity><xbrli:identifier scheme="http://www.sec.gov/CIK">0000000007'
            f'</xbrli:identifier>{segment}</xbrli:entity><xbrli:period>{period}</xbrli:period></xbrli:context>')


def day(concept, ctx, text, fmt="ixt:date-monthname-day-year-en"):
    return f'<ix:nonNumeric name="gaap:{concept}" contextRef="{ctx}" format="{fmt}">{text}</ix:nonNumeric>'


def amount(concept, ctx, value):
    return (f'<ix:nonFraction name="gaap:{concept}" contextRef="{ctx}" unitRef="usdPerShare" decimals="2" '
            f'format="ixt:num-dot-decimal">{value}</ix:nonFraction>')


RECORD, PAY, DECLARED = ("DividendsPayableDateOfRecordDayMonthAndYear", "DividendPayableDateToBePaidDayMonthAndYear",
                         "DividendsPayableDateDeclaredDayMonthAndYear")
DECLARED_AMOUNT, PAYABLE_AMOUNT = "CommonStockDividendsPerShareDeclared", "DividendsPayableAmountPerShare"


def ixbrl(contexts, facts) -> bytes:
    """An inline XBRL document: its contexts and a unit in its header, its
    facts in a paragraph each. The us-gaap namespace has a prefix of its
    own: a fact is read by its namespace."""
    paragraphs = "".join(f"<p>{f}</p>" for f in facts)
    return (f'<?xml version="1.0" encoding="utf-8"?><html {NS}><body><div style="display:none"><ix:header>'
            f'<ix:resources>{"".join(contexts)}{UNIT}</ix:resources></ix:header></div>'
            f"<div>{paragraphs}</div></body></html>").encode()


# The 10-Q for the third quarter of 2026: the dividend it declared, its
# amount tagged apart for the quarter, and the second quarter's again,
# under a member of another name and without its amount.
Q3 = ixbrl([context("q3", "2026-07-01", "2026-09-30"),
            context("d3", "2026-07-01", "2026-09-30", DividendsAxis="exmp:Q3Dividend2026Member"),
            context("d2", "2026-04-01", "2026-06-30", DividendsAxis="exmp:Q2Dividend2026Member")],
           [amount(DECLARED_AMOUNT, "q3", "0.44"), day(DECLARED, "d3", "August 11, 2026"),
            day(RECORD, "d3", "October 13, 2026"), day(PAY, "d3", "October 30, 2026"),
            day(RECORD, "d2", "July 14, 2026"), day(PAY, "d2", "July 31, 2026")])
# A filing that tags no dividend's day: only an amount for the quarter.
UNTAGGED = ixbrl([context("q", "2026-07-01", "2026-09-30")], [amount(DECLARED_AMOUNT, "q", "0.27")])


def by_record(dividends):
    return {d["record_date"].isoformat(): d for d in dividends}


# ---- reading inline XBRL -----------------------------------------------


@pytest.mark.parametrize("text, fmt, want", [
    ("November 20, 2025", "ixt:date-monthname-day-year-en", date(2025, 11, 20)),
    ("Nov. 20, 2025", "ixt:date-monthname-day-year-en", date(2025, 11, 20)),
    ("Sept 9th, 2025", "ixt:date-monthname-day-year-en", date(2025, 9, 9)),
    ("20 November 2025", "ixt:date-day-monthname-year-en", date(2025, 11, 20)),
    ("11/20/2025", "ixt:date-month-day-year", date(2025, 11, 20)),
    ("11/20/25", "ixt:dateslashus", date(2025, 11, 20)),
    ("20.11.2025", "ixt:datedoteu", date(2025, 11, 20)),
    ("November 20, 2025", "ixt:datelongus", date(2025, 11, 20)),
    ("November 20, 2025", "ixt:datemonthdayyearen", date(2025, 11, 20)),
    ("2025-11-20", "ixt:date-year-month-day", date(2025, 11, 20)),
    ("2025-11-20", None, date(2025, 11, 20)),
    # No whole day, no day that exists, or no day at all.
    ("November 2025", "ixt:date-monthname-year-en", None),
    ("November 20", "ixt:date-monthname-day-en", None),
    ("February 30, 2026", "ixt:date-monthname-day-year-en", None),
    ("first quarter 2026", "ixt-sec:datequarterend", None),
    ("November or December 20, 2025", "ixt:date-monthname-day-year-en", None),
    ("to be announced", None, None),
])
def test_a_day_is_read_by_its_date_transform(text, fmt, want):
    assert sec_calendar._day(text, fmt) == want


@pytest.mark.parametrize("text, fmt, scale, sign, want", [
    ("0.44", "ixt:num-dot-decimal", "0", None, "0.44"),
    ("1,234.5", "ixt:numdotdecimal", None, None, "1234.5"),
    ("1.234,5", "ixt:num-comma-decimal", None, None, "1234.5"),
    ("0,25", "ixt:numdotcomma", None, None, "0.25"),
    ("—", "ixt:fixed-zero", None, None, "0"),
    ("1,250", "ixt:num-dot-decimal", "6", None, "1250000000"),
    ("0.40", None, None, "-", "-0.4"),
    ("none", "ixt-sec:numwordsen", None, None, None),
])
def test_a_number_is_read_by_its_number_transform(text, fmt, scale, sign, want):
    assert sec_calendar._number(text, fmt, scale, sign) == want


def test_a_number_of_a_scale_that_is_no_number_is_none():
    assert sec_calendar._number("1", None, "x", None) is None


def test_two_amounts_of_one_concept_are_none():
    assert sec_calendar._amount({DECLARED_AMOUNT: {("0.44", "USD")}}) == ("0.44", "USD", DECLARED_AMOUNT)
    assert sec_calendar._amount({DECLARED_AMOUNT: {("0.44", "USD"), ("0.48", "USD")}}) is None


def test_a_10q_states_one_dividend_per_member_of_its_dividend_axis():
    got = by_record(sec_calendar.dividends(FIXTURE.read_bytes()))
    # Every fact is nested in a text block; the preferred stock's dividend
    # is none of the share's.
    assert sorted(got) == ["2025-04-08", "2025-07-15", "2026-04-07", "2026-07-14"]
    q2 = got["2026-07-14"]
    assert (q2["pay_date"], q2["declared_date"], q2["member"]) == (
        date(2026, 7, 31), date(2026, 5, 12), "exmp:DividendDeclaredMay2026Member")
    # Its amount is tagged for its quarter, without the axis.
    assert q2["amount"] == ("0.44", "USD", DECLARED_AMOUNT)
    assert got["2025-07-15"]["amount"] == ("0.4", "USD", DECLARED_AMOUNT)
    # A member's own amount; a declared day the header's hidden facts state
    # without a transform.
    q1 = got["2026-04-07"]
    assert (q1["declared_date"], q1["amount"]) == (date(2026, 2, 10), ("0.44", "USD", DECLARED_AMOUNT))


def test_dividends_without_an_axis_join_by_their_context():
    doc = ixbrl([context("q", "2026-07-01", "2026-09-30"), context("y", "2026-01-01", "2026-12-31"),
                 context("i", "2026-08-25", SubsequentEventTypeAxis="gaap:SubsequentEventMember")],
                [day(RECORD, "q", "July 14, 2026"), day(PAY, "q", "July 31, 2026"),
                 amount(DECLARED_AMOUNT, "q", "0.44"),
                 # Two dividends in one context: no telling which day is whose.
                 day(RECORD, "y", "April 7, 2026"), day(RECORD, "y", "October 13, 2026"),
                 day(PAY, "y", "April 24, 2026"),
                 # A dividend declared after the quarter, its amount its own.
                 day(RECORD, "i", "October 20, 2026"), amount(PAYABLE_AMOUNT, "i", "0.48"),
                 day(DECLARED, "i", "August 25, 2026")])
    got = by_record(sec_calendar.dividends(doc))
    assert sorted(got) == ["2026-07-14", "2026-10-20"]
    assert (got["2026-07-14"]["period"], got["2026-07-14"]["amount"]) == (
        ("2026-07-01", "2026-09-30"), ("0.44", "USD", DECLARED_AMOUNT))
    late = got["2026-10-20"]
    assert (late["pay_date"], late["declared_date"], late["amount"]) == (
        None, date(2026, 8, 25), ("0.48", "USD", PAYABLE_AMOUNT))


def test_an_amount_joins_no_dividend_that_shares_its_period_with_another():
    doc = ixbrl([context("q", "2026-07-01", "2026-09-30"),
                 context("a", "2026-07-01", "2026-09-30", DividendsAxis="exmp:RegularMember"),
                 context("b", "2026-07-01", "2026-09-30", DividendsAxis="exmp:SpecialMember")],
                [amount(DECLARED_AMOUNT, "q", "1.44"), day(RECORD, "a", "July 14, 2026"),
                 day(RECORD, "b", "September 15, 2026"),
                 # Only a declared day: no dividend a payment can be matched to.
                 day(DECLARED, "q", "July 21, 2026")])
    got = by_record(sec_calendar.dividends(doc))
    assert {k: d["amount"] for k, d in got.items()} == {"2026-07-14": None, "2026-09-15": None}


def test_a_partnership_tags_its_distributions_to_limited_partners():
    doc = ixbrl([context("d", "2026-07-01", "2026-09-30",
                         DistributionMadeToLimitedPartnerByDistributionAxis="exmp:SecondQuarterDistributionMember")],
                [day("DistributionMadeToLimitedPartnerDeclarationDate", "d", "July 21, 2026"),
                 day("DistributionMadeToLimitedPartnerDateOfRecord", "d", "August 3, 2026"),
                 day("DistributionMadeToLimitedPartnerDistributionDate", "d", "August 14, 2026"),
                 amount("DistributionMadeToLimitedPartnerDistributionsDeclaredPerUnit", "d", "0.6250")])
    (d,) = sec_calendar.dividends(doc)
    assert (d["declared_date"], d["record_date"], d["pay_date"], d["amount"]) == (
        date(2026, 7, 21), date(2026, 8, 3), date(2026, 8, 14),
        ("0.625", "USD", "DistributionMadeToLimitedPartnerDistributionsDeclaredPerUnit"))


def test_a_document_reads_named_characters_and_leaves_out_what_it_excludes(caplog):
    doc = ixbrl([context("d", "2026-07-01", "2026-09-30", DividendsAxis="exmp:Q3Member")],
                [day(RECORD, "d", "October&nbsp;13,<ix:exclude>(1)</ix:exclude> 2026"),
                 '<ix:nonNumeric name="gaap:DividendPayableDateToBePaidDayMonthAndYear" contextRef="d" '
                 'xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:nil="true"/>',
                 # A concept of another taxonomy by the same name is none.
                 '<ix:nonNumeric name="exmp:DividendPayableDateToBePaidDayMonthAndYear" contextRef="d" '
                 'format="ixt:date-monthname-day-year-en">October 30, 2026</ix:nonNumeric>'])
    (d,) = sec_calendar.dividends(doc)
    assert (d["record_date"], d["pay_date"]) == (date(2026, 10, 13), None)
    # A document that is no XML states none, and a warning names it.
    assert sec_calendar.filing_dividends(b"<html><p>unclosed</html>", "x.htm", None) == []
    assert sec_calendar.filing_dividends(b"not a zip", "x-xbrl.zip", None) == []
    assert "x.htm: not XML" in caplog.text and "x-xbrl.zip" in caplog.text


def test_an_archive_reads_its_inline_documents_and_skips_its_exhibits(caplog):
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as z:
        z.writestr("exmp-20260930.htm", Q3)
        # Two exhibits, plain HTML: neither is parsed, so neither warns.
        z.writestr("exmp-ex311.htm", EXHIBIT)
        z.writestr("exmp-ex32.htm", b"<html><p>Section 906 &mdash; certification</html>")
        z.writestr("exmp-20260930.xsd", b"<schema/>")
    assert sorted(by_record(sec_calendar.filing_dividends(buf.getvalue(), "a-xbrl.zip", None))) == [
        "2026-07-14", "2026-10-13"]
    assert "not XML" not in caplog.text
    assert sec_calendar.READS.line() == "calendar: archives read 1, documents parsed 1, members skipped 2"


def late_archive() -> bytes:
    """An archive whose primary document, in a folder, declares its
    namespace late, beside two inline documents that do not read: one
    of inline XBRL 1.1, one of 1.0."""
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        z.writestr("filing/exmp-20260930.htm", late(Q3))
        z.writestr("exmp-10q2.htm", b'<html xmlns:ix="http://www.xbrl.org/2013/inlineXBRL"><p>cut')
        z.writestr("exmp-10q3.htm", b'<html xmlns:ix="http://www.xbrl.org/2008/inlineXBRL"><p>cut')
    return buf.getvalue()


def test_an_archive_reads_its_primary_document_by_name(caplog):
    # The primary is read whatever its start declares, its name matched
    # without its folder; an inline document that does not read keeps
    # its warning.
    got = sec_calendar.filing_dividends(late_archive(), "b-xbrl.zip", "docs/exmp-20260930.htm")
    assert sorted(by_record(got)) == ["2026-07-14", "2026-10-13"]
    assert caplog.text.count("b-xbrl.zip: not XML") == 2
    assert sec_calendar.READS.line() == "calendar: archives read 1, documents parsed 3, members skipped 0"


def test_an_archive_without_the_primarys_name_misses_a_late_declaration():
    assert sec_calendar.filing_dividends(late_archive(), "b-xbrl.zip", None) == []
    assert sec_calendar.READS.line() == "calendar: archives read 1, documents parsed 2, members skipped 1"


def test_a_primary_document_stored_on_its_own_is_read_whole():
    assert sec_calendar.READS.line() is None  # nothing read yet
    assert sorted(by_record(sec_calendar.filing_dividends(late(Q3), "c.htm", None))) == ["2026-07-14", "2026-10-13"]
    assert sec_calendar.READS.line() == "calendar: archives read 0, documents parsed 1, members skipped 0"


# ---- fetch and parse ---------------------------------------------------


def routes(**extra):
    return {"company_tickers.json": TICKERS, "submissions": submissions(*FILINGS), **extra}


@pytest.fixture
def today(monkeypatch):
    """sec-edgar's today: TODAY, or the day a test sets."""
    days = [TODAY]
    monkeypatch.setattr(sec_edgar, "today_utc", lambda: days[0])
    return days


def test_a_share_reads_the_filings_of_fifteen_months_that_no_earlier_run_read(tmp_path, today):
    client = FakeClient(routes(**{
        archive("0000000007-26-000006"): b"<html><body>Not here</body></html>",
        "000000000726000006/exmp-20260331.htm": UNTAGGED,
        archive("0000000007-26-000010"): xbrl_zip(FIXTURE.read_bytes()),
        "000000000726000011/exmp-20260930.htm": Q3}))
    claim = {"key": KEY, "symbol": "EXMP", "cik": 7, "families": [], "facts": [CAL],
             "read": ["0000000007-25-000020", "0000000007-26-000003"]}
    e = SecEdgar()
    e.fetch_security(claim, tmp_path, Context(client=client))
    assert months_back(TODAY, 15) == date(2025, 8, 2)
    # The filings of the window not read before, oldest first: an answer
    # that is no archive, or none, leads to the primary document.
    assert [u for u in client.urls() if "Archives" in u] == [
        archive("0000000007-26-000006"), f"{ARCHIVES}000000000726000006/exmp-20260331.htm",
        archive("0000000007-26-000010"), archive("0000000007-26-000011"),
        f"{ARCHIVES}000000000726000011/exmp-20260930.htm"]
    meta = read_meta(tmp_path)
    assert "facts_missing" not in meta
    assert [(f["accession"], f["file"], f["tagged"]) for f in meta["calendar"]["filings"]] == [
        ("0000000007-26-000006", "0000000007-26-000006.htm", False),
        ("0000000007-26-000010", "0000000007-26-000010-xbrl.zip", True),
        ("0000000007-26-000011", "0000000007-26-000011.htm", True)]
    assert compress.resolve_variant(tmp_path / "0000000007-26-000011.htm") is not None
    events = {r["event_id"]: r for r in e.parse(tmp_path).dividend_events}
    assert sorted(events) == ["dividend||2025-04-08|2025-04-25", "dividend||2025-07-15|2025-08-01",
                              "dividend||2026-04-07|2026-04-24", "dividend||2026-07-14|2026-07-31",
                              "dividend||2026-10-13|2026-10-30"]
    # Stated again by the later 10-Q, without its amount or declared day:
    # those of the earlier one stay, and the later one dates it.
    q2 = events["dividend||2026-07-14|2026-07-31"]
    assert (q2["ref"], q2["provider"], q2["kind"], q2["status"], q2["as_of"]) == (
        KEY, "sec-edgar", "dividend", "final", date(2026, 10, 28))
    assert (q2["ex_date"], q2["record_date"], q2["pay_date"], q2["declared_date"]) == (
        None, date(2026, 7, 14), date(2026, 7, 31), date(2026, 5, 12))
    assert (q2["amount"], q2["currency"]) == ("0.44", "USD")
    assert q2["payload"] == {"accn": "0000000007-26-000011", "form": "10-Q", "member": "exmp:Q2Dividend2026Member",
                             "amount_concept": "us-gaap:CommonStockDividendsPerShareDeclared"}
    q3 = events["dividend||2026-10-13|2026-10-30"]
    assert (q3["declared_date"], q3["amount"], q3["as_of"]) == (date(2026, 8, 11), "0.44", date(2026, 10, 28))
    assert events["dividend||2025-04-08|2025-04-25"]["as_of"] == date(2026, 8, 5)


def test_a_filing_that_cannot_be_had_leaves_the_calendar_due(tmp_path, today, caplog):
    client = FakeClient(routes(**{archive("0000000007-26-000010"): 503, "000000000726000006/": 503,
                                  archive("0000000007-26-000011"): xbrl_zip(Q3)}))
    e = SecEdgar()
    e.fetch_security({"key": KEY, "symbol": "EXMP", "cik": 7, "facts": [CAL], "read": []}, tmp_path,
                     Context(client=client))
    meta = read_meta(tmp_path)
    assert meta["facts_missing"] == [CAL]
    # Two failed, and of the 10-K and the oldest 10-Q neither the archive
    # nor the primary document is served: only the last is read.
    assert [f["accession"] for f in meta["calendar"]["filings"]] == ["0000000007-26-000011"]
    assert "filing 0000000007-26-000010: " in caplog.text
    assert {r["event_id"] for r in e.parse(tmp_path).dividend_events} == {
        "dividend||2026-07-14|2026-07-31", "dividend||2026-10-13|2026-10-30"}


def test_a_filing_without_an_archive_that_names_no_primary_document_leaves_the_calendar_due(tmp_path, today,
                                                                                            caplog):
    bare = ("0000000007-26-000011", "2026-10-28", "2026-09-30", "10-Q", 1, 1, "")
    client = FakeClient({"company_tickers.json": TICKERS, "submissions": submissions(bare)})
    SecEdgar().fetch_security({"key": KEY, "symbol": "EXMP", "cik": 7, "facts": [CAL], "read": []}, tmp_path,
                              Context(client=client))
    meta = read_meta(tmp_path)
    assert meta["facts_missing"] == [CAL] and meta["calendar"]["filings"] == []
    assert "filing 0000000007-26-000011: no XBRL archive and no primary document" in caplog.text
    assert [u for u in client.urls() if "Archives" in u] == [archive("0000000007-26-000011")]


def test_only_a_10q_or_10k_in_inline_xbrl_is_read():
    listed = submissions(
        ("0000000007-26-000011", "2026-10-28", "2026-09-30", "10-Q", 1, 1, "exmp-20260930.htm"),
        ("0000000007-26-000008", "2026-08-20", "2026-06-30", "10-Q/A", 1, 1, "exmp-20260630a.htm"),
        ("0000000007-26-000007", "2026-08-10", "2026-06-30", "10-Q", 1, 0, "exmp-20260630.htm"),
        ("../../x", "2026-08-09", "2026-06-30", "10-Q", 1, 1, "exmp.htm"),
        ("0000000007-26-000003", "2026-02-20", "2025-12-31", "10-K", 1, 1, ""))
    got = sec_calendar.filings(listed, date(2026, 1, 1), {"0000000007-26-000011"})
    assert got == [{"accession": "0000000007-26-000003", "form": "10-K", "filed": "2026-02-20",
                    "report": "2025-12-31", "document": None}]
    assert sec_calendar.filings({}, date(2026, 1, 1), set()) == []


def test_a_lookup_that_asks_for_no_calendar_reads_no_filing(tmp_path, today):
    client = FakeClient(routes())
    e = SecEdgar()
    e.fetch_security({"key": KEY, "symbol": "EXMP", "cik": 7, "families": []}, tmp_path, Context(client=client))
    assert not [u for u in client.urls() if "Archives" in u]
    assert "calendar" not in read_meta(tmp_path) and e.parse(tmp_path).dividend_events == []


# ---- the claim, and when a company is asked again ----------------------


def share(**kw):
    return wantedmod.Security(key=KEY, symbol="EXMP", wants=(CAL,), **kw)


def earlier(bronze_dir, slug, *, filings=(), missing=False, status="complete", calendar=True):
    """An earlier run in bronze_dir/slug whose sec-edgar request for EXMP
    read `filings` ((accession, filed, tagged)); with `missing`, it
    named the calendar missing."""
    folder = kept_run(bronze_dir, slug, "security", "sec-edgar", KEY, status=status)
    meta = {"provider": "sec-edgar", "key": KEY, "cik": 7, "families": [], "as_of": slug_day(slug)}
    if calendar:
        meta["calendar"] = {"filings": [{"accession": a, "filed": f, "tagged": t} for a, f, t in filings]}
    if missing:
        meta["facts_missing"] = [CAL]
    write_meta(folder, **meta)


def slug_day(slug):
    return f"{slug[:4]}-{slug[4:6]}-{slug[6:8]}"


def claim(data_dir, sec=None):
    ctx = Context(client=FakeClient({"company_tickers.json": TICKERS}), data_dir=data_dir)
    return SecEdgar().claim_security(sec or share(), ctx)


def test_the_claim_asks_for_the_calendar_of_a_share_that_wants_it(tmp_path, today):
    assert SecEdgar().answers >= {CAL}
    got = claim(None)
    assert (got["facts"], got["read"]) == ([CAL], [])
    # A depositary receipt, or a share whose calendar is not due this run.
    for sec in (share(receipt=True), wantedmod.Security(key=KEY, symbol="EXMP")):
        assert "facts" not in claim(None, sec) and "read" not in claim(None, sec)


def test_a_company_that_tags_its_dividends_is_asked_for_the_filings_not_read(tmp_path, today):
    earlier(tmp_path, "20260801T010000Z", filings=[("0000000007-26-000006", "2026-05-06", False),
                                                   ("0000000007-26-000003", "2026-02-20", True)])
    got = claim(tmp_path)
    assert (got["facts"], got["read"]) == ([CAL], ["0000000007-26-000003", "0000000007-26-000006"])


def test_a_company_that_tags_none_is_asked_again_a_year_after_it_was_asked(tmp_path, today):
    earlier(tmp_path, "20251201T010000Z", filings=[("0000000007-25-000020", "2025-10-29", True)])
    earlier(tmp_path, "20260801T010000Z", filings=[("0000000007-26-000006", "2026-05-06", False)])
    # Its only filing that tagged one is more than a year old.
    assert "facts" not in claim(tmp_path)
    today[0] = date(2027, 8, 1)
    assert claim(tmp_path)["facts"] == [CAL]


def test_only_a_run_that_read_every_filing_it_asked_for_counts_as_asked(tmp_path, today):
    earlier(tmp_path, "20260801T010000Z", filings=[("0000000007-26-000006", "2026-05-06", False)], missing=True)
    earlier(tmp_path, "20260901T010000Z", filings=[("0000000007-26-000010", "2026-08-05", False)],
            status="in-progress")
    # A lookup that asked for no calendar says nothing of it.
    earlier(tmp_path, "20261001T010000Z", calendar=False)
    got = claim(tmp_path)
    assert (got["facts"], got["read"]) == ([CAL], ["0000000007-26-000006"])
    s = seen(Context(client=None, data_dir=tmp_path), "sec-edgar")[7]
    assert s.checked is None


@pytest.mark.parametrize("day, n, want", [
    (date(2026, 11, 2), 15, date(2025, 8, 2)), (date(2026, 5, 31), 3, date(2026, 2, 28)),
    (date(2028, 5, 31), 3, date(2028, 2, 29)), (date(2026, 1, 31), 2, date(2025, 11, 30))])
def test_months_back_keeps_the_day_or_the_months_last(day, n, want):
    assert months_back(day, n) == want


def test_due():
    assert due(None, TODAY) and due(Seen(), TODAY)
    quiet = Seen(read={"a": (date(2026, 5, 6), False)}, checked=date(2026, 8, 1))
    assert not due(quiet, TODAY) and due(quiet, date(2027, 8, 1))
    tags = Seen(read={"a": (date(2025, 11, 3), True)}, checked=date(2026, 10, 1))
    assert due(tags, TODAY) and not due(tags, TODAY + timedelta(days=1))


# ---- a run, and the next ones ------------------------------------------


def doc():
    return wanted_doc(securities=[{"key": KEY, "symbol": "EXMP", "scope": "direct", "asset_class": "public_equity",
                                   "missing": [], "lookups": {}, "periods": {}, "receipt": False,
                                   "wants": [CAL], "calendar": None}], version=3)


def run(bronze_dir, day, client, today):
    """One download on `day`: plan and fetch, as download.main does, with
    sec-edgar alone; the run's manifest entries and coverage."""
    today[0] = day
    ctx = Context(client=client, data_dir=bronze_dir, providers=frozenset({"sec-edgar"}))
    reqs, _, cover = download.plan(wantedmod.parse(doc()), [REGISTRY["sec-edgar"]], ctx, today=day, max_age=30,
                                   backfill=False, earlier=download.fetched_before(bronze_dir))
    return run_entries(make_run(bronze_dir, f"{day:%Y%m%d}T010000Z", reqs, ctx)), cover


def filings_read(client):
    return [u.rsplit("/", 1)[1] for u in client.urls() if "Archives" in u]


def test_a_run_loads_the_events_and_the_next_asks_only_what_is_new(tmp_path, today, caplog, monkeypatch):
    data = tmp_path / "data"
    data.mkdir()
    # The latest 10-Q declares its namespace late: its name as the
    # primary document is what has it read, at the fetch and at the load.
    docs = {"0000000007-26-000010": FIXTURE.read_bytes(), "0000000007-26-000011": late(Q3)}
    client = FakeClient(routes(**{archive(a): xbrl_zip(docs.get(a, UNTAGGED)) for a, *_ in FILINGS}))
    (entry,), cover = run(data, TODAY, client, today)
    assert (entry["status"], entry["facts"]) == ("ok", [CAL]) and cover[CAL][KEY].requests
    assert len(filings_read(client)) == 5
    assert [f["tagged"] for f in read_meta(data / "20261102T010000Z" / entry["dir"])["calendar"]["filings"]] == [
        False, False, False, True, True]
    assert sec_calendar.READS.line() == "calendar: archives read 5, documents parsed 5, members skipped 5"
    # The load counts afresh.
    monkeypatch.setattr(sec_calendar, "READS", sec_calendar.Reads())
    caplog.set_level("INFO", logger="refdata.load")
    db = tmp_path / "refdata.db"
    assert load.main(["--silver-db", str(db), "--bronze-dir", str(data)]) == 0
    assert "sec-edgar: calendar: archives read 5, documents parsed 5, members skipped 5" in caplog.text
    events = rows(db, "SELECT ref, provider, event_id, ex_date, record_date, pay_date, declared_date, amount, "
                      "currency, status FROM dividend_events ORDER BY event_id")
    assert rows(db, "SELECT COUNT(*) FROM securities") == [(1,)]
    assert len(events) == 5 and events[-1] == (KEY, "sec-edgar", "dividend||2026-10-13|2026-10-30", None, "2026-10-13",
                                               "2026-10-30", "2026-08-11", "0.44", "USD", "final")
    # A month on, a company that tags is asked again: its submissions
    # file, and only the filing that is new.
    client.asked.clear()
    assert [e.get("facts") for e in run(data, TODAY + timedelta(days=31), client, today)[0]] == [[CAL]]
    assert filings_read(client) == []
    new = ("0000000007-26-000014", "2026-11-20", "2026-09-30", "10-Q/A", 1, 1, "exmp-20260930a.htm")
    client.routes["submissions"] = submissions(new, *FILINGS)
    client.asked.clear()
    run(data, TODAY + timedelta(days=62), client, today)
    assert filings_read(client) == []
    new = ("0000000007-26-000015", "2026-12-20", "2026-09-30", "10-Q", 1, 1, "exmp-20260930b.htm")
    client.routes["submissions"] = submissions(new, *FILINGS)
    client.routes[archive(new[0])] = xbrl_zip(UNTAGGED)
    client.asked.clear()
    run(data, TODAY + timedelta(days=93), client, today)
    assert filings_read(client) == ["0000000007-26-000015-xbrl.zip"]


def test_a_company_that_tags_none_is_left_a_year_and_then_read_from_where_it_was(tmp_path, today):
    data = tmp_path / "data"
    data.mkdir()
    client = FakeClient(routes(**{archive(a): xbrl_zip(UNTAGGED) for a, *_ in FILINGS}))
    run(data, TODAY, client, today)
    assert len(filings_read(client)) == 5
    # A month on, the lookup is asked for and the calendar is not.
    client.asked.clear()
    (entry,), cover = run(data, TODAY + timedelta(days=31), client, today)
    assert "facts" not in entry and filings_read(client) == []
    assert cover[CAL][KEY].reason == "no provider claims it"
    # A year on, the filings filed since, and only those.
    new = ("0000000007-27-000004", "2027-05-05", "2027-03-31", "10-Q", 1, 1, "exmp-20270331.htm")
    client.routes["submissions"] = submissions(new, *FILINGS)
    client.routes[archive(new[0])] = xbrl_zip(UNTAGGED)
    client.asked.clear()
    (entry,), _ = run(data, TODAY + timedelta(days=366), client, today)
    assert entry["facts"] == [CAL] and filings_read(client) == ["0000000007-27-000004-xbrl.zip"]
