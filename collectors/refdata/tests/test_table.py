"""The holdings table and the XLSX reader, on invented files."""

import io
import zipfile
from datetime import date

from providers import table, xlsx

DAY = date(2026, 10, 12)


def test_a_nameless_row_is_kept_or_skipped_and_a_footnote_ends_the_table():
    rows = [["Name", "ISIN", "Weight"],
            ["Alpha", "XX0000000801", "50"],
            ["", "", ""],                       # a blank row ends the table
            ]
    assert len(table.lines(rows, 0, ref="r", as_of=DAY)) == 1
    rows = [["Name", "ISIN", "Weight"],
            ["Alpha", "XX0000000801", "50"],
            ["", "XX0000000802", "-"],          # neither name nor weight: skipped
            ["", "XX0000000804", "10"],         # a weight without a name: kept
            ["Beta", "XX0000000803", "40"],
            ["Data as of the close. Not an offer."]]
    got = table.lines(rows, 0, ref="r", as_of=DAY)
    assert [(x["name"], x["weight"]) for x in got] == [("Alpha", "50"), (None, "10"), ("Beta", "40")]
    assert [x["line_no"] for x in got] == [1, 2, 3]


def test_issuer_days():
    assert table.parse_day("As of 10-Oct-2026") == date(2026, 10, 10)
    assert table.parse_day("Oct 10, 2026") == date(2026, 10, 10)
    assert table.parse_day("10/12/2026") == date(2026, 10, 12)
    assert table.parse_day("someday") is None


def _book(sheet_xml, shared=None, target="worksheets/sheet1.xml"):
    ns = 'xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"'
    rel = 'xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"'
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        z.writestr("xl/workbook.xml", f'<workbook {ns} {rel}><sheets><sheet name="S" sheetId="1" r:id="rId1"/>'
                                      '</sheets></workbook>')
        z.writestr("xl/_rels/workbook.xml.rels",
                   '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
                   f'<Relationship Id="rId1" Target="{target}" Type="worksheet"/></Relationships>')
        if shared is not None:
            z.writestr("xl/sharedStrings.xml", f"<sst {ns}>" + "".join(f"<si><t>{s}</t></si>" for s in shared)
                       + "</sst>")
        path = target.lstrip("/")
        z.writestr(path if path.startswith("xl/") else f"xl/{path}",
                   f"<worksheet {ns}><sheetData>{sheet_xml}</sheetData></worksheet>")
    return buf.getvalue()


def test_xlsx_shared_strings_numbers_sparse_cells_and_an_absolute_target():
    sheet = ('<row r="1"><c r="A1" t="s"><v>0</v></c><c r="C1" t="s"><v>1</v></c></row>'
             '<row r="3"><c t="s"><v>2</v></c><c><v>12.5</v></c></row>')
    got = xlsx.rows(_book(sheet, shared=["Name", "Weight", "Alpha"], target="/xl/worksheets/data.xml"))
    # Row 2 is omitted by the file; a skipped column reads as "".
    assert got == [["Name", "", "Weight"], ["Alpha", "12.5"]]


def test_a_dash_is_no_label():
    rows = [["Name", "Weight", "Sector", "Country"], ["Alpha", "-", "-", "n/a"], ["Beta", "100", "Energy", "France"],
            ["Gamma", "0", "unknown", "Unassigned"]]
    a, b, c = table.lines(rows, 0, ref="r", as_of=DAY)
    assert (a["weight"], a["sector"], a["country"]) == (None, None, None)
    assert (c["sector"], c["country"]) == (None, None)
    assert (b["sector"], b["country"]) == ("Energy", "France")


def test_a_ticker_names_a_us_listing_only():
    assert table.us_exchange("New York Stock Exchange Inc.") and table.us_exchange("NASDAQ")
    for venue in ("Cboe Canada", "Cboe Europe", "Nasdaq Omx Helsinki Ltd.", "Nyse Euronext - Euronext Paris",
                  "Nasdaq Baltic", "SIX Swiss Exchange", None):
        assert not table.us_exchange(venue), venue
    # Without an exchange column, the line's currency decides.
    rows = [["Name", "Ticker", "Weight", "Currency"], ["Alpha", "QAAA", "60", "USD"], ["Beta", "QBBB", "40", "CHF"],
            ["Gamma", "QCCC", "0", ""]]
    a, b, c = table.lines(rows, 0, ref="r", as_of=DAY)
    assert (a["ticker"], b["ticker"], b["payload"]["local_ticker"], c["ticker"]) == ("QAAA", None, "QBBB", "QCCC")
