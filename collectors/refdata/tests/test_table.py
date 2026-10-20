"""The holdings table and the XLSX reader, on invented files."""

from datetime import date

import pytest

from providers import table, xlsx
from tests.helpers import make_xlsx

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
    assert table.parse_day("30/Sept/2026") == table.parse_day("30/Sep/2026") == date(2026, 9, 30)
    assert table.parse_day("20261012") == date(2026, 10, 12)
    for nothing in ("someday", "2026101", "20261312", None):
        assert table.parse_day(nothing) is None, nothing


def test_xlsx_shared_strings_numbers_sparse_cells_and_an_absolute_target():
    sheet = ('<row r="1"><c r="A1" t="s"><v>0</v></c><c r="C1" t="s"><v>1</v></c></row>'
             '<row r="3"><c t="s"><v>2</v></c><c><v>12.5</v></c></row>')
    got = xlsx.rows(make_xlsx(sheet=sheet, shared=["Name", "Weight", "Alpha"], target="/xl/worksheets/data.xml"))
    # Row 2 is omitted by the file; a skipped column reads as "".
    assert got == [["Name", "", "Weight"], ["Alpha", "12.5"]]


@pytest.mark.parametrize("data", [b"<html>not here</html>", make_xlsx([["Name"]])[:200]])
def test_bytes_that_are_no_workbook_are_refused(data):
    with pytest.raises(xlsx.XlsxError, match="not a readable workbook"):
        xlsx.rows(data)


def test_csv_rows_and_a_composition_of_its_lines():
    rows = table.csv_rows('Name,Weight\n"Alpha, Inc",33.3\nBeta,33.3\nGamma,33.4\nDelta,-\n')
    lns = table.lines(rows, 0, ref="r", as_of=DAY)
    assert lns[0]["name"] == "Alpha, Inc"
    c = table.composition("r", DAY, lns, provider="p", payload={"sub_fund": "Alpha"})
    # The weights stated add up exactly; a line without one counts all the same.
    assert (c["weight_sum"], c["line_count"], c["payload"]) == ("100", 4, {"sub_fund": "Alpha"})
    assert table.composition("r", DAY, lns[3:], provider="p")["weight_sum"] is None


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


def test_a_debt_lines_figures_fill_their_columns_by_any_of_their_header_words():
    rows = [["Name", "Weight (%)", "Duration", "YTM (%)", "FX Rate", "Mod. Duration", "Yield to Call (%)",
             "Yield to Worst (%)", "Real Duration", "Real YTM (%)", "Accrual Date", "Effective Date"],
            ["Alpha 4% 2034", "60", "6.12", "4.31", "1.00", "6.05", "-", "4.31", "-", "-", "Aug 15, 2026",
             "Feb 15, 2024"],
            ["Beta Linker 2031", "40", "4.40", "4.02", "1.0811", "4.38", "4.10", "4.02", "4.75", "1.85", "-", "-"]]
    a, b = table.lines(rows, 0, ref="r", as_of=DAY)
    assert (a["duration"], a["yield_to_maturity"], a["exchange_rate"], a["modified_duration"], a["yield_to_call"],
            a["yield_to_worst"], a["real_duration"], a["real_yield"], a["accrual_date"]) == \
        ("6.12", "4.31", "1", "6.05", None, "4.31", None, None, "2026-08-15")
    # The effective date has no column of its own.
    assert a["payload"] == {"effective date": "Feb 15, 2024"}
    assert (b["real_duration"], b["real_yield"], b["yield_to_call"], b["exchange_rate"], b["accrual_date"]) == \
        ("4.75", "1.85", "4.1", "1.0811", None)
    rows = [["Holding", "Weight", "Effective Duration", "Modified Duration", "Yield to Maturity", "Yield to Worst",
             "Exchange Rate", "Real Yield", "Accrual Date"],
            ["Gamma 2% 2030", "100", "3.5", "3.4", "2.9", "2.8", "0.92", "0.7", "01.03.2026"]]
    (c,) = table.lines(rows, 0, ref="r", as_of=DAY)
    assert (c["duration"], c["modified_duration"], c["yield_to_maturity"], c["yield_to_worst"], c["exchange_rate"],
            c["real_yield"], c["accrual_date"], c["payload"]) == \
        ("3.5", "3.4", "2.9", "2.8", "0.92", "0.7", "2026-03-01", {})


def test_a_file_without_debt_figures_leaves_their_columns_empty():
    (a,) = table.lines([["Name", "Weight"], ["Alpha", "100"]], 0, ref="r", as_of=DAY)
    assert {a[f] for f in (*table.FIGURES, "accrual_date")} == {None}


def test_a_file_of_fractions_states_its_yields_as_fractions_too():
    rows = [["Name", "Weight", "YTM (%)", "Yield to Worst", "Yield to Call", "Real Yield", "Duration", "FX Rate"],
            ["Alpha 4% 2034", "0.6", "0.0431", "0.0425", "0.0440", "0.0185", "6.12", "1.0811"]]
    (a,) = table.lines(rows, 0, ref="r", as_of=DAY, fractions=True)
    assert (a["weight"], a["yield_to_maturity"], a["yield_to_worst"], a["yield_to_call"], a["real_yield"]) == \
        ("60", "4.31", "4.25", "4.4", "1.85")
    # A duration and a rate are no shares.
    assert (a["duration"], a["exchange_rate"]) == ("6.12", "1.0811")
