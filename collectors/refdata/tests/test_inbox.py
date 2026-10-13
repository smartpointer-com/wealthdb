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
    with pytest.raises(ProviderError, match="name and weight"):
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
