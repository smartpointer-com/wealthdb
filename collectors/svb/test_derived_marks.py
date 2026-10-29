"""
Unit tests for derived_marks.py — the performance workbook of month-end
account values that fills the months the statement archive misses.

`read_workbook` and `read_stated_periods` are exercised against a
workbook this module builds, so the fixture is synthetic end to end:
all-zero account serials in the `SV[MRT]-NNNNNN` shape and round
example values. `gaps_to_fill` is a
pure function of the marks and the months the statements reach, and
carries the rules that keep a derived mark honest — so it is tested
directly, with no file at all.
"""

from __future__ import annotations

import sys
from datetime import date, datetime
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent))

import derived_marks as dm  # noqa: E402


def _workbook(tmp_path, sheets):
    """Write a workbook in the real one's layout: one sheet per account,
    named by its serial, with the period's START date and its ending
    market value among the columns."""
    openpyxl = pytest.importorskip("openpyxl")
    book = openpyxl.Workbook()
    book.remove(book.active)
    for name, rows in sheets.items():
        sheet = book.create_sheet(name)
        sheet.append(["Date", "BMV", "EMV", "Accrual", "Net Additions"])
        for when, emv in rows:
            sheet.append([when, 0, emv, 0, 0])
    path = tmp_path / "derived-marks.xlsx"
    book.save(path)
    return path


def test_a_sheet_per_account_becomes_month_end_marks(tmp_path):
    path = _workbook(tmp_path, {
        "M000000": [(date(2022, 2, 1), 100.0), (date(2022, 3, 1), 200.0)],
        "T000000": [(date(2022, 2, 1), 5.0)],
    })
    marks = dm.read_workbook(path)
    got = sorted((m.account_external_id, m.as_of, m.market_value) for m in marks)
    assert got == [
        ("SVM-000000", "2022-02-28", 100.0),
        ("SVM-000000", "2022-03-31", 200.0),
        ("SVT-000000", "2022-02-28", 5.0),
    ]


def test_the_sheet_dates_a_period_by_its_start(tmp_path):
    # The value is the period's END, so a row dated the 1st is that
    # month's closing mark — not the previous month's.
    path = _workbook(tmp_path, {"M000000": [(date(2022, 2, 1), 100.0)]})
    assert dm.read_workbook(path)[0].as_of == "2022-02-28"


def test_a_month_end_is_the_months_own_length(tmp_path):
    # February is the month a month-end lands wrong in, and it has two
    # lengths.
    path = _workbook(tmp_path, {"M000000": [
        (date(2022, 2, 1), 100.0),
        (date(2024, 2, 1), 100.0),
        (date(2022, 12, 1), 100.0),
    ]})
    assert [m.as_of for m in dm.read_workbook(path)] == [
        "2022-02-28", "2024-02-29", "2022-12-31"]


def test_sheets_that_are_not_an_account_are_skipped(tmp_path):
    path = _workbook(tmp_path, {
        "M000000": [(date(2022, 2, 1), 100.0)],
        "530000OTHER": [(date(2022, 2, 1), 999.0)],
        "Summary": [(date(2022, 2, 1), 999.0)],
    })
    accounts = {m.account_external_id for m in dm.read_workbook(path)}
    assert accounts == {"SVM-000000"}


def test_blank_and_dashed_cells_are_not_values(tmp_path):
    # A hand-maintained sheet spells "nothing here" with a blank or a
    # dash; neither is a zero.
    path = _workbook(tmp_path, {"M000000": [
        (date(2022, 2, 1), None),
        (date(2022, 3, 1), " -   "),
        (date(2022, 4, 1), 100.0),
    ]})
    assert [(m.as_of, m.market_value) for m in dm.read_workbook(path)] == [
        ("2022-04-30", 100.0)]


def test_a_missing_workbook_is_not_an_error(tmp_path, monkeypatch):
    assert dm.read_workbook(tmp_path / "absent.xlsx") == []
    assert dm.read_workbook(None) == []
    # And is not an error even where there is no reader: the pass runs on
    # every build against a path that usually does not exist, so the absent
    # case must not reach the import.
    monkeypatch.setitem(sys.modules, "openpyxl", None)
    assert dm.read_workbook(tmp_path / "absent.xlsx") == []
    assert dm.read_workbook(None) == []


# ============================================================
# read_stated_periods — every value column of every row
# ============================================================

_FULL_HEADER = ["Date", "BMV", "EMV", "Accrual", "Net Additions",
                "Net Gain", "Income", "Fees"]


def _full_workbook(path, sheets, header=_FULL_HEADER):
    """Write a workbook at ``path``, one sheet per account, under
    ``header`` (all eight columns unless given); each row is given whole,
    so a test states every cell."""
    openpyxl = pytest.importorskip("openpyxl")
    book = openpyxl.Workbook()
    book.remove(book.active)
    for name, rows in sheets.items():
        sheet = book.create_sheet(name)
        sheet.append(header)
        for row in rows:
            sheet.append(row)
    book.save(path)
    return path


def test_a_stated_period_reads_all_seven_values(tmp_path):
    path = _full_workbook(tmp_path / "marks.xlsx", {"M000000": [
        [datetime(2022, 2, 1), 1000.0, 1150.0, 5.0, 100.0, 50.0, 20.0, 10.0],
    ]})
    [p] = dm.read_stated_periods(path)
    assert p == dm.StatedPeriod(
        account_external_id="SVM-000000",
        period_start="2022-02-01", period_end="2022-02-28",
        beginning_value=1000.0, ending_value=1150.0, accrual=5.0,
        net_additions=100.0, net_gain=50.0, income=20.0, fees=10.0,
        cells={"Date": "2022-02-01", "BMV": 1000, "EMV": 1150, "Accrual": 5,
               "Net Additions": 100, "Net Gain": 50, "Income": 20,
               "Fees": 10},
    )


def test_a_stated_period_runs_from_the_rows_date_to_its_month_end(tmp_path):
    path = _full_workbook(tmp_path / "marks.xlsx", {"T000000": [
        [datetime(2024, 2, 1), 100.0, 100.0, 0, 0, 0, 0, 0],
        [datetime(2022, 12, 1), 100.0, 100.0, 0, 0, 0, 0, 0],
    ]})
    assert [(p.account_external_id, p.period_start, p.period_end)
            for p in dm.read_stated_periods(path)] == [
        ("SVT-000000", "2024-02-01", "2024-02-29"),
        ("SVT-000000", "2022-12-01", "2022-12-31"),
    ]


def test_net_additions_keep_their_sign(tmp_path):
    path = _full_workbook(tmp_path / "marks.xlsx", {"M000000": [
        [datetime(2022, 3, 1), 1000.0, 900.0, 0, -100.0, 0, 0, 0],
    ]})
    assert dm.read_stated_periods(path)[0].net_additions == -100.0


def test_blank_dash_and_text_cells_read_as_none(tmp_path):
    path = _full_workbook(tmp_path / "marks.xlsx", {"M000000": [
        [datetime(2022, 2, 1), None, 100.0, " -   ", "n/a", None, "-", 0],
    ]})
    [p] = dm.read_stated_periods(path)
    assert (p.beginning_value, p.accrual, p.net_additions, p.net_gain,
            p.income) == (None, None, None, None, None)
    assert (p.ending_value, p.fees) == (100.0, 0.0)
    # The payload keeps the cells as read, the dash and the text included.
    assert p.cells["Accrual"] == " -   "
    assert p.cells["Net Additions"] == "n/a"
    assert p.cells["BMV"] is None


def test_a_row_that_states_no_value_is_skipped(tmp_path):
    path = _full_workbook(tmp_path / "marks.xlsx", {"M000000": [
        [datetime(2022, 2, 1), None, "-", None, None, "", None, None],
        [datetime(2022, 3, 1), None, None, None, None, None, None, 0],
        ["not a date", 100.0, 100.0, 0, 0, 0, 0, 0],
    ]})
    assert [p.period_start for p in dm.read_stated_periods(path)] == [
        "2022-03-01"]


def test_a_header_without_some_value_columns_reads_them_as_none(tmp_path):
    path = _full_workbook(tmp_path / "marks.xlsx", {"M000000": [
        [datetime(2022, 2, 1), 100.0, 0],
    ]}, header=["Date", "EMV", "Fees"])
    [p] = dm.read_stated_periods(path)
    assert (p.ending_value, p.fees) == (100.0, 0.0)
    assert (p.beginning_value, p.accrual, p.net_additions, p.net_gain,
            p.income) == (None, None, None, None, None)
    assert p.cells == {"Date": "2022-02-01", "EMV": 100, "Fees": 0}


def test_a_sheet_without_a_date_header_states_nothing(tmp_path):
    path = _full_workbook(tmp_path / "marks.xlsx", {"M000000": [
        [datetime(2022, 2, 1), 100.0, 100.0],
    ]}, header=["Day", "BMV", "EMV"])
    assert dm.read_stated_periods(path) == []
    assert dm.read_workbook(path) == []


def test_a_sheet_that_is_not_an_account_is_not_read_for_periods(tmp_path):
    row = [datetime(2022, 2, 1), 100.0, 100.0, 0, 0, 0, 0, 0]
    path = _full_workbook(tmp_path / "marks.xlsx", {
        "R000000": [row], "Summary": [row], "530000OTHER": [row]})
    assert {p.account_external_id for p in dm.read_stated_periods(path)} == {
        "SVR-000000"}


def test_the_marks_of_a_full_workbook_are_its_ending_values(tmp_path):
    # The gap-fill read stays on the ending value alone, whatever the
    # other columns state.
    path = _full_workbook(tmp_path / "marks.xlsx", {"M000000": [
        [datetime(2022, 2, 1), 1000.0, 1150.0, 5.0, 100.0, 50.0, 20.0, 10.0],
        [datetime(2022, 3, 1), 1150.0, None, 0, 0, 0, 0, 0],
    ]})
    assert [(m.account_external_id, m.as_of, m.market_value)
            for m in dm.read_workbook(path)] == [
        ("SVM-000000", "2022-02-28", 1150.0)]


# ============================================================
# gaps_to_fill — the rules that keep a derived mark honest
# ============================================================

def _mark(account, as_of, value=1.0):
    return dm.DerivedMark(account, as_of, value)


def test_fills_only_an_interior_gap():
    covered = {"SVM-000000": {(2022, 1), (2022, 3)}}
    marks = [_mark("SVM-000000", "2022-02-28")]
    assert dm.gaps_to_fill(marks, covered) == marks


def test_never_extends_the_series_at_either_end():
    # Before the first statement and after the last, a derived mark
    # would be inventing a position rather than sourcing a value.
    covered = {"SVM-000000": {(2022, 2), (2022, 3)}}
    marks = [_mark("SVM-000000", "2022-01-31"),
             _mark("SVM-000000", "2022-04-30")]
    assert dm.gaps_to_fill(marks, covered) == []


def test_a_month_the_statements_reach_is_left_alone():
    # The statement is the better record, and two rows for one month
    # would double the account.
    covered = {"SVM-000000": {(2022, 1), (2022, 2), (2022, 3)}}
    assert dm.gaps_to_fill([_mark("SVM-000000", "2022-02-28")], covered) == []


def test_an_account_the_archive_never_covers_is_not_created():
    assert dm.gaps_to_fill([_mark("SVM-000000", "2022-02-28")], {}) == []
    assert dm.gaps_to_fill([_mark("SVM-000000", "2022-02-28")],
                           {"SVM-000001": {(2022, 1), (2022, 3)}}) == []


def test_a_statement_dated_mid_month_still_covers_its_month():
    # Coverage is by month, not by day: a mark at the month's end must
    # not land beside a statement dated the 29th.
    covered = {"SVM-000000": {(2022, 1), (2022, 2), (2022, 3)}}
    assert dm.gaps_to_fill([_mark("SVM-000000", "2022-02-28")], covered) == []


def test_results_are_ordered_so_a_build_replays_the_same_way():
    covered = {"SVM-000000": {(2022, 1), (2022, 5)},
               "SVM-000001": {(2022, 1), (2022, 5)}}
    marks = [_mark("SVM-000001", "2022-03-31"), _mark("SVM-000000", "2022-04-30"),
             _mark("SVM-000000", "2022-02-28")]
    got = [(m.account_external_id, m.as_of)
           for m in dm.gaps_to_fill(marks, covered)]
    assert got == [
        ("SVM-000000", "2022-02-28"),
        ("SVM-000000", "2022-04-30"),
        ("SVM-000001", "2022-03-31"),
    ]
