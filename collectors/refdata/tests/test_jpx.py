"""jpx: the Japan Exchange Group's daily ex-rights lists, read from the
index page while a Japanese share is held, every list no earlier run
fetched, and every dividend row of each kept, held or not."""

from datetime import date
from pathlib import Path

import pytest

import download
import load
import wanted as wantedmod
from providers import REGISTRY, jpx
from providers.base import Context, ProviderError, read_meta
from tests.helpers import FakeClient, make_run, rows, run_entries, wanted_doc
from tests.xls import workbook

FIX = Path(__file__).parent / "fixtures"
CAL = "dividend_calendar"
DAY = date(2026, 10, 23)
LISTS = "https://www.jpx.co.jp/english/listing/others/ex-rights/tvdivq000000xxxx-att/"
HEADER = ["Record Date", "Ex-rights Date (Other Transactions)", "Code", "Issue Name", "Market Segment", "Remarks",
          "Update"]


def index_page():
    return (FIX / "jpx_ex_rights_index.html").read_text()


def jp_share(key, symbol=None, **kw):
    return wantedmod.Security(key=key, isin=f"JP{key[-10:]}", symbol=symbol, asset_class="public_equity",
                              wants=(CAL,), **kw)


def ex_rights(*rows, title=True):
    """An invented list: a title and a note above the header, as JPX
    writes them, the rows, and a note below."""
    head = [["Ex-rights/Ex-dividend Information"], ["Updated around 13:00 every business day"], []] if title else []
    return workbook([*head, HEADER, *rows, [], ["* The list is for reference only."]])


LIST = ex_rights(
    [date(2026, 10, 30), date(2026, 10, 29), "9Z0A", "Example Kogyo", "Prime", "Ex-Dividend", "New"],
    ["2026/10/30", "2026/10/29", "9Z9A", "Sample Trading", "Standard", "Ex-Dividend/Split", ""],
    [date(2026, 10, 31), date(2026, 10, 30), "9Z1A", "Specimen REIT", "REIT", "Ex-Distribution", ""],
    [date(2026, 10, 31), date(2026, 10, 30), "9Z2A", "Model Seiki", "Growth", "Split", ""],
    ["", "", "", "", "", "Ex-Dividend", ""])


def test_the_index_links_the_newest_five_lists_on_jpxs_own_host():
    assert jpx.files(index_page()) == [(date(2026, 10, d), f"{LISTS}202610{d}EN.xls") for d in (19, 20, 21, 22, 23)]
    assert jpx.files("<p>No list today.</p>") == []


def test_a_japanese_share_plans_one_request_per_list_no_earlier_run_fetched(caplog):
    client = FakeClient({"/ex-rights/index.html": index_page()})
    ctx = Context(client=client)
    secs = [jp_share("isin:JP0000000010", "9Z0A"), jp_share("isin:JP0000009Z9A", "9Z9A.T"),
            jp_share("isin:JP0000000013"), jp_share("isin:JP0000000014", "9Z4A", receipt=True),
            wantedmod.Security(key="isin:CH0000000001", isin="CH0000000001", asset_class="public_equity",
                               wants=(CAL,))]
    planned = REGISTRY["jpx"].plan_bulk(secs, ctx, DAY, {}, 30)
    assert [ref for ref, _ in planned] == [f"ex-rights:2026-10-{d}" for d in (19, 20, 21, 22, 23)]
    ref, claim = planned[-1]
    assert claim == {"ref": ref, "url": f"{LISTS}20261023EN.xls", "day": "2026-10-23",
                     "keys": ["isin:JP0000000010", "isin:JP0000009Z9A", "isin:JP0000000013"],
                     "codes": {"9Z0A": ["isin:JP0000000010"], "9Z9A": ["isin:JP0000009Z9A"]}, "facts": [CAL]}
    # A list an earlier run fetched is not asked for again; the index is
    # read once a run.
    fetched = {f"ex-rights:2026-10-{d}": DAY for d in (19, 20, 21)}
    assert [ref for ref, _ in REGISTRY["jpx"].plan_bulk(secs, ctx, DAY, fetched, 30)] == \
        ["ex-rights:2026-10-22", "ex-rights:2026-10-23"]
    assert client.urls() == ["https://www.jpx.co.jp/english/listing/others/ex-rights/index.html"]
    # No Japanese share: JPX is not asked.
    quiet = FakeClient()
    assert REGISTRY["jpx"].plan_bulk(secs[3:], Context(client=quiet), DAY, {}, 30) == [] and quiet.asked == []
    # An index that links no list plans nothing, and says so.
    empty = Context(client=FakeClient({"/ex-rights/index.html": "<html></html>"}))
    assert REGISTRY["jpx"].plan_bulk(secs, empty, DAY, {}, 30) == []
    assert "links no list" in caplog.text


def test_a_tse_code_is_a_symbol_of_four_characters_or_a_ticker_of_one():
    for symbol, code in (("9Z0A", "9Z0A"), ("9Z9A.T", "9Z9A"), ("9Z5A JP", "9Z5A"), ("9Z0A:TYO", "9Z0A"),
                         ("QABC", ""), ("99901", ""), (None, "")):
        assert jpx.tse_code(jp_share("isin:JP0000000010", symbol)) == code


def _fetch(tmp_path, body, claim=None):
    p = REGISTRY["jpx"]
    claim = claim or {"ref": "ex-rights:2026-10-23", "url": f"{LISTS}20261023EN.xls", "day": "2026-10-23",
                      "keys": ["isin:JP0000000010", "isin:JP0000000012"],
                      "codes": {"9Z0A": ["isin:JP0000000010"], "9Z2A": ["isin:JP0000000012"]}, "facts": [CAL]}
    out = tmp_path / "list"
    p.fetch_bulk(claim, out, Context(client=FakeClient({claim["url"]: body})))
    return p, out


def test_a_list_keeps_every_dividend_row_held_or_not(tmp_path):
    p, out = _fetch(tmp_path, LIST)
    assert (out / jpx.LIST_DOC).read_bytes() == LIST
    # The share split alone (9Z2A) states no dividend.
    assert read_meta(out)["answered"] == ["isin:JP0000000010"] and read_meta(out)["as_of"] == "2026-10-23"
    parsed = p.parse(out)
    events = {e["ref"]: e for e in parsed.dividend_events}
    assert sorted(events) == ["jpx:9Z0A", "jpx:9Z1A", "jpx:9Z9A"]
    # A list of coming days: what silver holds stands beside it.
    assert not parsed.events_complete
    first = events["jpx:9Z0A"]
    assert (first["provider"], first["event_id"], first["as_of"], first["kind"], first["record_date"],
            first["ex_date"], first["pay_date"], first["amount"], first["currency"], first["status"]) == \
        ("jpx", "dividend|2026-10-29|2026-10-30|", DAY, "dividend", date(2026, 10, 30), date(2026, 10, 29), None,
         None, None, None)
    assert first["payload"] == {"name": "Example Kogyo", "segment": "Prime", "remarks": "Ex-Dividend", "update": "New"}
    # A day written as text reads as one; an ETF's or a REIT's
    # distribution is a dividend too.
    assert (events["jpx:9Z9A"]["record_date"], events["jpx:9Z9A"]["payload"]["remarks"]) == \
        (date(2026, 10, 30), "Ex-Dividend/Split")
    assert events["jpx:9Z1A"]["ex_date"] == date(2026, 10, 30)


def test_a_column_no_field_names_goes_to_the_payload_under_its_header():
    rows = [["Record Date", "Code", "Remarks", "Ex-rights Date", "Notes"],
            ["2026/10/30", "9Z0A", "Ex-Dividend", "2026/10/29", "Commemorative"]]
    (event,) = jpx.events(jpx.list_rows(rows), DAY, provider="jpx")
    assert (event["ex_date"], event["payload"]) == (date(2026, 10, 29), {"remarks": "Ex-Dividend",
                                                                          "Notes": "Commemorative"})


@pytest.mark.parametrize("body,refusal", [
    (b"<html>Not found</html>", "no workbook xlrd reads"),
    (ex_rights(title=False)[:600], "no workbook xlrd reads"),
    (workbook([["Code", "Issue Name"], ["9Z0A", "Example Kogyo"]]), "holds no ex-rights list")])
def test_a_file_that_is_no_workbook_or_holds_no_list_is_refused(tmp_path, body, refusal):
    with pytest.raises(ProviderError, match=refusal):
        _fetch(tmp_path, body)
    assert not (tmp_path / "list").exists()


def test_a_number_cell_reads_as_its_text_and_a_date_cell_as_its_day():
    assert jpx.sheet_rows(workbook([[1234, 2.5, date(2026, 10, 30), "  Example  "]])) == [
        ["1234", "2.5", date(2026, 10, 30), "Example"]]


@pytest.mark.parametrize("text,day", [
    ("2026/10/30", date(2026, 10, 30)), ("2026.10.30", date(2026, 10, 30)), ("Oct. 30, 2026", date(2026, 10, 30)),
    ("30-Oct-2026", date(2026, 10, 30)), ("", None), ("x", None), ("2026/02/30", None)])
def test_a_lists_day_reads_as_jpx_writes_it(text, day):
    assert jpx._day(text) == day


def test_a_dividend_row_without_a_day_is_none_and_rows_of_one_code_and_days_are_one_event():
    rows = [["Record Date", "Ex-rights Date", "Code", "Remarks", "Update"],
            ["", "", "9Z0A", "Ex-Dividend", ""],
            ["2026/10/30", "2026/10/29", "9Z1A", "Ex-Dividend", "New"],
            ["2026/10/30", "2026/10/29", "9Z1A", "Ex-Distribution", "Revised"]]
    (event,) = jpx.events(jpx.list_rows(rows), DAY, provider="jpx")
    assert (event["ref"], event["event_id"]) == ("jpx:9Z1A", "dividend|2026-10-29|2026-10-30|")
    assert [part["remarks"] for part in event["payload"]["parts"]] == ["Ex-Dividend", "Ex-Distribution"]


def _execute(data, slug, reqs, client):
    return run_entries(make_run(data, slug, reqs, Context(client=client, data_dir=data)))


def test_a_run_fetches_each_list_once_and_the_newer_lists_row_wins(tmp_path):
    doc = wanted_doc(securities=[{"key": "isin:JP0000000010", "isin": "JP0000000010", "symbol": "9Z0A",
                                  "scope": "direct", "asset_class": "public_equity", "wants": [CAL]}], version=3)
    want, data, db = wantedmod.parse(doc), tmp_path / "data", tmp_path / "refdata.db"
    older = ex_rights([date(2026, 10, 30), date(2026, 10, 29), "9Z0A", "Example Kogyo", "Prime", "Ex-Dividend", "New"])
    client = FakeClient({"/ex-rights/index.html": index_page(), "20261023EN.xls": LIST, "EN.xls": older})
    ctx = Context(client=client)
    reqs, _, cover = download.plan(want, [REGISTRY["jpx"]], ctx, today=DAY, max_age=30, backfill=False)
    assert [(r.kind, r.ref, r.facts) for r in reqs] == [("bulk", f"ex-rights:2026-10-{d}", (CAL,))
                                                        for d in (19, 20, 21, 22, 23)]
    assert cover[CAL]["isin:JP0000000010"].requests == reqs
    entries = _execute(data, "20261023T050000Z", reqs, client)
    assert [e["status"] for e in entries] == ["ok"] * 5
    assert load.main(["--silver-db", str(db), "--bronze-dir", str(data)]) == 0
    assert rows(db, "SELECT ref, as_of FROM dividend_events WHERE ref = 'jpx:9Z0A'") == [("jpx:9Z0A", 1792713600)]
    assert rows(db, "SELECT COUNT(*) FROM dividend_events") == [(3,)]
    # The next run reads the index again and asks for no list it has.
    earlier = download.fetched_before(data)
    assert download.plan(want, [REGISTRY["jpx"]], Context(client=client), today=DAY, max_age=30, backfill=False,
                         earlier=earlier)[0] == []
    assert earlier.wants == {("jpx", "isin:JP0000000010", CAL): DAY}


def test_probe_counts_the_lists_the_index_links():
    client = FakeClient({"/ex-rights/index.html": index_page()})
    assert REGISTRY["jpx"].probe(Context(client=client)) == "JPX ex-rights index: 5 lists"
