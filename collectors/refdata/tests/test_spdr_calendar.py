"""spdr's dividend calendar: State Street's US and EMEA distributions
workbooks, one request each a run, on invented workbooks in their layout.
Every fund, identifier, day and amount is invented."""

import io
import json
import logging
import zipfile
from datetime import date

import pytest

from providers import xlsx
from providers.base import Context, read_meta
from providers.spdr import DISTRIBUTIONS, Spdr, _day, distribution_events, fund_rows
from tests.helpers import FakeClient, events_by_id, make_xlsx
from tests.test_spdr import US, ssga
from wanted import Fund

CAL = "dividend_calendar"
US_URL, EMEA_URL = DISTRIBUTIONS["us"][0], DISTRIBUTIONS["emea"][0]
US_HEADER = ["FUND NAME", "TICKER", "CUSIP", "EX-DATE", "RECORD DATE", "PAYABLE DATE", "DIVIDEND ($)",
             "SHORT TERM CAPITAL GAIN ($)", "LONG TERM CAPITAL GAIN ($)", "FREQUENCY"]
EMEA_HEADER = ["FUND UMBRELLA", "FUND NAME", "TICKER", "ISIN", "CURRENCY", "Announcement Date", "EX-DATE",
               "EX-DATE NAV", "RECORD DATE", "PAYMENT DATE", "DISTRIBUTION", "FREQUENCY"]
# The US sheet: dates mm/dd/yyyy, or a date cell's serial (46191 is
# 2026-06-18); another fund's rows, and a fund that reuses the ticker
# under another CUSIP.
US_SHEET = [
    ["SPDR ETF Historical Distributions"],
    US_HEADER,
    ["Example Equity Fund B", "XMPEQB", "000000AB7", "09/18/2026", "09/18/2026", "10/30/2026", "1.888834", "", "",
     "Quarterly"],
    ["Example Equity Fund B", "XMPEQB", "000000AB7", 46191, 46191, "07/31/2026", "1.7012", "0", "0", "Quarterly"],
    ["Example Equity Fund B", "XMPEQB", "000000AB7", "12/19/2025", "12/19/2025", "01/30/2026", "2.1", "0.05",
     "0.2512", "Quarterly"],
    ["Example Equity Fund B", "XMPEQB", "000000AB7", "03/20/2025", "03/20/2025", "04/30/2025", "", "", "",
     "Quarterly"],
    ["Example Equity Fund B", "XMPEQB", "", "09/19/1997", "09/19/1997", "10/31/1997", "$0.38", "", "", "Quarterly"],
    ["Example Equity Fund B (old)", "XMPEQB", "000000ZZ1", "06/19/1997", "06/19/1997", "07/31/1997", "0.4", "", "",
     "Quarterly"],
    ["Example Value Fund", "XMPVAL", "000000AC5", "09/18/2026", "09/18/2026", "09/23/2026", "0.31", "", "",
     "Quarterly"],
    [],
    ["Distributions are subject to change."],
]
# The EMEA sheets: dates dd/mm/yyyy, the currency apart.
OPEN_SHEET = [
    EMEA_HEADER,
    ["Example Umbrella I plc", "Example Europe Equity UCITS ETF", "QEUR", "XX0000000501", "EUR", "14/09/2026",
     "21/09/2026", "61.2345", "22/09/2026", "29/09/2026", "1.6714", "Semi-Annually"],
    ["Example Umbrella I plc", "Example Europe Equity UCITS ETF", "QEUR", "XX0000000501", "EUR", "13/03/2026",
     "20/03/2026", "59.87", "23/03/2026", "30/03/2026", "1.4402", "Semi-Annually"],
    ["Example Umbrella I plc", "Example Bond Fund C", "XMPBDC", "XX0000000503", "USD", "14/09/2026",
     "21/09/2026", "28.1", "22/09/2026", "29/09/2026", "0.4012", "Semi-Annually"],
]
CLOSED_SHEET = [
    EMEA_HEADER,
    ["Example Umbrella I plc", "Example Europe Equity UCITS ETF", "QEUR", "XX0000000501", "EUR", "15/09/2014",
     "22/09/2014", "40.5", "23/09/2014", "30/09/2014", "0.912", "Semi-Annually"],
]


US_BOOK = make_xlsx(sheets={"Cover": [["SPDR"]], "dividend": US_SHEET})
EMEA_BOOK = make_xlsx(sheets={"Open Fund": OPEN_SHEET, "Closed Fund": CLOSED_SHEET})


def site(us=US_BOOK, emea=EMEA_BOOK):
    """State Street with its US and EMEA distributions workbooks, and the
    holdings of a US and an EMEA fund."""
    return ssga(**{"spdr-etf-historical-distributions.xlsx": us,
                   "spdr-etf-historical-distributions-emea-en.xlsx": emea, "holdings-daily-us": make_xlsx(US)})


def fetch(tmp_path, client, fund, ctx=None):
    ctx = ctx or Context(client=client)
    p = Spdr()
    claim = p.claim(fund, ctx)
    p.fetch(claim, None, tmp_path, ctx)
    return p.parse(tmp_path)


US_FUND = Fund(ref="symbol:XMPEQB", symbol="XMPEQB", wants=(CAL,))
EMEA_FUND = Fund(ref="isin:XX0000000501", isin="XX0000000501", wants=(CAL,))


def test_a_us_fund_reads_its_rows_of_the_us_workbook(tmp_path):
    client = site()
    parsed = fetch(tmp_path, client, US_FUND)
    assert client.urls().count(US_URL) == 1 and EMEA_URL not in client.urls()
    assert "facts_missing" not in read_meta(tmp_path)
    assert events_by_id(parsed.dividend_events, declared=True) == {
        "dividend|2026-09-18|2026-09-18|2026-10-30": ("dividend", date(2026, 9, 18), date(2026, 9, 18),
                                                      date(2026, 10, 30), None, "1.888834", "USD"),
        # A day stored as a date cell, and gains of zero that pay none.
        "dividend|2026-06-18|2026-06-18|2026-07-31": ("dividend", date(2026, 6, 18), date(2026, 6, 18),
                                                      date(2026, 7, 31), None, "1.7012", "USD"),
        # December pays a dividend and both gains: the gains are one event.
        "dividend|2025-12-19|2025-12-19|2026-01-30": ("dividend", date(2025, 12, 19), date(2025, 12, 19),
                                                      date(2026, 1, 30), None, "2.1", "USD"),
        "capital_gain|2025-12-19|2025-12-19|2026-01-30": ("capital_gain", date(2025, 12, 19), date(2025, 12, 19),
                                                          date(2026, 1, 30), None, "0.3012", "USD"),
        # A row that pays no amount is one event of kind other.
        "other|2025-03-20|2025-03-20|2025-04-30": ("other", date(2025, 3, 20), date(2025, 3, 20), date(2025, 4, 30),
                                                   None, None, None),
        # A row without its CUSIP is the fund's by its ticker; the oldest
        # row is kept too.
        "dividend|1997-09-19|1997-09-19|1997-10-31": ("dividend", date(1997, 9, 19), date(1997, 9, 19),
                                                      date(1997, 10, 31), None, "0.38", "USD")}
    # Dated by the holdings file's day; the workbook is the fund's whole
    # history.
    assert {(r["ref"], r["provider"], r["as_of"], r["status"]) for r in parsed.dividend_events} == {
        ("symbol:XMPEQB", "spdr", date(2026, 10, 10), None)}
    assert parsed.events_complete
    gain = next(r for r in parsed.dividend_events if r["kind"] == "capital_gain")
    assert gain["payload"] == {"sheet": "dividend", "fund_name": "Example Equity Fund B", "ticker": "XMPEQB",
                               "cusip": "000000AB7", "frequency": "Quarterly", "short_term": "0.05",
                               "long_term": "0.2512"}
    # The fund's rows only, with their sheet's header.
    kept = json.loads((tmp_path / "distributions.json").read_text())
    assert kept["url"] == US_URL
    ((sheet),) = kept["sheets"]
    assert (sheet["sheet"], sheet["header"], len(sheet["rows"])) == ("dividend", US_HEADER, 5)


def test_an_emea_fund_reads_both_sheets_by_its_isin(tmp_path):
    parsed = fetch(tmp_path, site(), EMEA_FUND)
    assert events_by_id(parsed.dividend_events, declared=True) == {
        "dividend|2026-09-21|2026-09-22|2026-09-29": ("dividend", date(2026, 9, 21), date(2026, 9, 22),
                                                      date(2026, 9, 29), date(2026, 9, 14), "1.6714", "EUR"),
        "dividend|2026-03-20|2026-03-23|2026-03-30": ("dividend", date(2026, 3, 20), date(2026, 3, 23),
                                                      date(2026, 3, 30), date(2026, 3, 13), "1.4402", "EUR"),
        "dividend|2014-09-22|2014-09-23|2014-09-30": ("dividend", date(2014, 9, 22), date(2014, 9, 23),
                                                      date(2014, 9, 30), date(2014, 9, 15), "0.912", "EUR")}
    assert {r["as_of"] for r in parsed.dividend_events} == {date(2026, 10, 12)}
    closed = next(r for r in parsed.dividend_events if r["ex_date"] == date(2014, 9, 22))
    assert closed["payload"] == {"sheet": "Closed Fund", "fund_umbrella": "Example Umbrella I plc",
                                 "fund_name": "Example Europe Equity UCITS ETF", "ticker": "QEUR",
                                 "isin": "XX0000000501", "ex_date_nav": "40.5", "frequency": "Semi-Annually"}


def test_an_emea_workbook_of_one_of_its_sheets_reads_that_one(tmp_path):
    parsed = fetch(tmp_path, site(emea=make_xlsx(sheets={"Open Fund": OPEN_SHEET})), EMEA_FUND)
    assert "facts_missing" not in read_meta(tmp_path)
    assert sorted(e["ex_date"] for e in parsed.dividend_events) == [date(2026, 3, 20), date(2026, 9, 21)]


def test_an_emea_row_without_its_currency_takes_the_one_its_amount_names():
    header = OPEN_SHEET[0]
    rows = [[*r[:4], "", *r[5:10], amount, r[11]] for r, amount in zip(OPEN_SHEET[1:3], ("USD 1.6714", "1.4402"),
                                                                         strict=True)]
    doc = {"sheets": [{"sheet": "Open Fund", "header": header, "rows": rows}]}
    got = distribution_events(doc, "isin:XX0000000501", date(2026, 10, 12), emea=True, provider="spdr")
    assert [(e["amount"], e["currency"]) for e in got] == [("1.6714", "USD"), ("1.4402", None)]


def test_a_row_both_sheets_list_is_one_distribution_of_its_amount():
    header, row = OPEN_SHEET[0], OPEN_SHEET[1]
    doc = {"sheets": [{"sheet": "Open Fund", "header": header, "rows": [row]},
                      {"sheet": "Closed Fund", "header": header, "rows": [row]}]}
    (event,) = distribution_events(doc, "isin:XX0000000501", date(2026, 10, 12), emea=True, provider="spdr")
    assert (event["amount"], event["currency"], event["payload"]["sheet"]) == ("1.6714", "EUR", "Open Fund")


def test_one_request_per_workbook_serves_every_fund_of_the_run(tmp_path):
    client = site()
    ctx = Context(client=client)
    fetch(tmp_path / "a", client, US_FUND, ctx)
    fetch(tmp_path / "b", client, EMEA_FUND, ctx)
    fetch(tmp_path / "c", client, Fund(ref="isin:XX0000000503", isin="XX0000000503", wants=(CAL,)), ctx)
    assert client.urls().count(US_URL) == 1 and client.urls().count(EMEA_URL) == 1
    rows = json.loads((tmp_path / "c" / "distributions.json").read_text())["sheets"]
    assert [len(s["rows"]) for s in rows] == [1, 0]


@pytest.mark.parametrize("answer,warning", [
    (500, "Internal Server Error"),
    (b"<html>busy</html>", "is not a workbook"),
    (make_xlsx(sheets={"dividend": [["FUND NAME", "TICKER"]]}), "has no distributions header"),
    (make_xlsx(sheets={"Sheet1": US_SHEET}), "the workbook has no sheet 'dividend'"),
    (b"PK not a zip", "not a readable workbook"),
])
def test_a_workbook_that_does_not_come_leaves_the_calendar_due_and_is_asked_for_once(tmp_path, caplog, answer,
                                                                                     warning):
    client = site(us=answer)
    ctx = Context(client=client)
    with caplog.at_level(logging.WARNING, logger="refdata.spdr"):
        parsed = fetch(tmp_path / "a", client, US_FUND, ctx)
        fetch(tmp_path / "b", client, Fund(ref="isin:XX0000000502", isin="XX0000000502", wants=(CAL,)), ctx)
    assert warning in caplog.text and "its distributions workbook did not come" in caplog.text
    assert client.urls().count(US_URL) == 1
    for d in ("a", "b"):
        assert read_meta(tmp_path / d)["facts_missing"] == [CAL]
        assert not (tmp_path / d / "distributions.json").exists()
    assert parsed.dividend_events == [] and parsed.lines and not parsed.events_complete


def test_a_fund_without_a_row_has_paid_none_and_its_calendar_is_answered(tmp_path):
    us = make_xlsx(sheets={"dividend": [r for r in US_SHEET if "XMPEQB" not in r]})
    parsed = fetch(tmp_path, site(us=us), US_FUND)
    assert "facts_missing" not in read_meta(tmp_path) and parsed.dividend_events == []
    assert json.loads((tmp_path / "distributions.json").read_text())["sheets"][0]["rows"] == []


def test_a_fund_that_wants_no_calendar_asks_for_no_workbook(tmp_path):
    client = site()
    parsed = fetch(tmp_path, client, Fund(ref="symbol:XMPEQB", symbol="XMPEQB"))
    assert US_URL not in client.urls() and not (tmp_path / "distributions.json").exists()
    assert "facts_missing" not in read_meta(tmp_path) and parsed.dividend_events == []


def test_both_facts_missing_are_named(tmp_path):
    client = FakeClient({**site(us=500).routes, "/intermediary/etfs/": 500})
    fetch(tmp_path, client, Fund(ref="symbol:XMPEQB", symbol="XMPEQB", wants=("characteristics", CAL)))
    assert read_meta(tmp_path)["facts_missing"] == ["characteristics", CAL]


def test_a_claim_finds_the_funds_identifiers_in_its_finder_entry():
    ctx = Context(client=site())
    claim = Spdr().claim(US_FUND, ctx)
    assert (claim["isin"], claim["cusip"], claim["calendar"]) == ("XX0000000502", "000000AB7", True)
    # A US ISIN names the CUSIP.
    claim = Spdr().claim(Fund(ref="isin:US000000AB75", isin="US000000AB75", symbol="XMPEQB"), ctx)
    assert (claim["cusip"], claim["calendar"]) == ("000000AB7", False)


def test_a_us_row_is_the_funds_by_its_cusip_and_by_its_ticker_only_where_a_cusip_is_unknown():
    sheet = {"sheet": "dividend", "header": US_HEADER, "rows": US_SHEET[2:9]}
    claim = {"region": "us", "ticker": "xmpeqb", "cusip": "", "isin": ""}
    # Without the fund's CUSIP, every row of its ticker.
    assert len(fund_rows(sheet, claim)) == 6
    assert len(fund_rows(sheet, {**claim, "cusip": "000000AB7"})) == 5


def test_a_kept_file_of_no_sheet_states_no_event():
    assert distribution_events({"sheets": []}, "symbol:XMPEQB", date(2026, 10, 10), emea=False,
                               provider="spdr") == []


@pytest.mark.parametrize("text,day_first,day", [
    ("09/18/2026", False, date(2026, 9, 18)),
    ("21/09/2026", True, date(2026, 9, 21)),
    ("09/18/2026", True, None),
    ("46283", False, date(2026, 9, 18)),
    ("46283.0", True, date(2026, 9, 18)),
    ("2026-09-18", True, date(2026, 9, 18)),
    ("", False, None),
    ("TBA", False, None),
])
def test_a_day_reads_as_its_sheet_writes_it(text, day_first, day):
    assert _day(text, day_first=day_first) == day


def test_a_named_sheet_reads_in_any_case_and_a_missing_one_is_an_error():
    assert xlsx.rows(US_BOOK, sheet="Dividend")[1] == US_HEADER
    assert xlsx.rows(US_BOOK) == [["SPDR"]]
    with pytest.raises(xlsx.XlsxError, match="no sheet 'Closed Fund'"):
        xlsx.rows(US_BOOK, sheet="Closed Fund")
    # A workbook whose relationships do not name the sheet's part.
    book = io.BytesIO()
    with zipfile.ZipFile(io.BytesIO(US_BOOK)) as src, zipfile.ZipFile(book, "w") as z:
        for item in src.infolist():
            data = src.read(item)
            z.writestr(item, data.replace(b'Id="rId2"', b'Id="rId9"') if item.filename.endswith(".rels") else data)
    with pytest.raises(xlsx.XlsxError, match="does not say where its sheet 'dividend' lies"):
        xlsx.rows(book.getvalue(), sheet="dividend")
