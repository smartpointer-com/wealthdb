"""The inbox: files placed by hand, claimed by their folder."""

from datetime import date

import pytest

import wanted
from providers.base import Context, ProviderError
from providers.inbox import Inbox, parse_csv


def test_claims_a_fund_by_its_folder_in_either_spelling(tmp_path):
    (tmp_path / "inbox" / "isin_XX0000000401" / "2026-03-31").mkdir(parents=True)
    (tmp_path / "inbox" / "symbol:ABC" / "2026-04-30").mkdir(parents=True)
    (tmp_path / "inbox" / "symbol:ABC" / "notes").mkdir()
    ctx = Context(client=None, data_dir=tmp_path)
    inbox = Inbox()
    a = inbox.claim(wanted.Fund(ref="isin:XX0000000401"), ctx)
    b = inbox.claim(wanted.Fund(ref="symbol:ABC"), ctx)
    assert a and b and inbox.claim(wanted.Fund(ref="symbol:NONE"), ctx) is None
    assert inbox.dates(b, date(2026, 1, 1), date(2026, 12, 31), ctx) == [date(2026, 4, 30)]


def test_csv_needs_name_and_weight():
    with pytest.raises(ProviderError, match="a name and a weight"):
        parse_csv(b"isin,country\nXX0000000402,US\n", "isin:XX", date(2026, 1, 31))


def test_csv_reads_optional_columns_and_keeps_the_rest():
    p = parse_csv(b"\xef\xbb\xbfName,Weight,Ticker,Note\nExample Corp,\"1,250.5\",EXC,first\n",
                  "symbol:X", date(2026, 1, 31))
    (line,) = p.lines
    assert (line["name"], line["weight"], line["ticker"], line["payload"]) == \
        ("Example Corp", "1250.5", "EXC", {"note": "first"})


def _placed(tmp_path, *files):
    folder = tmp_path / "inbox" / "symbol_ABC" / "2026-03-31"
    folder.mkdir(parents=True)
    for name, body in files:
        (folder / name).write_bytes(body)
    ctx = Context(client=None, data_dir=tmp_path)
    inbox = Inbox()
    claim = inbox.claim(wanted.Fund(ref="symbol:ABC"), ctx)
    out = tmp_path / "bronze"
    inbox.fetch(claim, date(2026, 3, 31), out, ctx)
    return inbox, out


def test_a_folder_holds_one_composition_file(tmp_path):
    inbox, out = _placed(tmp_path, ("a.csv", b"name,weight\nA,60\n"), ("b.csv", b"name,weight\nB,40\n"))
    with pytest.raises(ProviderError, match="one composition file"):
        inbox.parse(out)


def test_an_nport_document_in_the_inbox(tmp_path):
    from pathlib import Path
    xml = (Path(__file__).parent / "fixtures" / "nport_primary_doc.xml").read_bytes()
    inbox, out = _placed(tmp_path, ("primary_doc.xml", xml), ("notes.txt", b"from the fund"))
    got = inbox.parse(out)
    assert got.compositions[0]["source_kind"] == "inbox" and got.lines


def test_an_issuer_download_with_preamble_and_notes(tmp_path):
    csv_text = (b'Example Fund ETF\r\nFund Holdings as of,"Oct 10, 2026"\r\n\r\n'
                b'Ticker,Name,Sector,Asset Class,Market Value,Weight (%),Location,Exchange,Currency\r\n'
                b'QAAA,ALPHA INC,Information Technology,Equity,"1,200.50",61.20,United States,NASDAQ,USD\r\n'
                b'QBBB,BETA INC,Health Care,Equity,760.00,38.80,United States,NYSE,USD\r\n'
                b'\r\n"The content is for personal use only."\r\n')
    inbox, out = _placed(tmp_path, ("holdings.csv", csv_text))
    got = inbox.parse(out)
    a, b = got.lines
    assert (a["ticker"], a["weight"], a["market_value"], a["country"], a["asset_type"]) == \
        ("QAAA", "61.2", "1200.5", "United States", "Equity")
    assert got.compositions[0]["weight_sum"] == "100" and got.compositions[0]["as_of"] == date(2026, 3, 31)


def test_an_xlsx_in_the_inbox(tmp_path):
    from tests.helpers import make_xlsx
    book = make_xlsx([["Name", "ISIN", "Weight"], ["Alpha", "XX0000000701", "100"]])
    inbox, out = _placed(tmp_path, ("holdings.xlsx", book))
    (line,) = inbox.parse(out).lines
    assert (line["isin"], line["weight"]) == ("XX0000000701", "100")
