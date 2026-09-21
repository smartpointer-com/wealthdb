"""
Unit tests for derived_marks.py — the advisor workbook that fills the
months the statement archive misses.

`read_workbook` is exercised against a workbook this module builds, so
the fixture is synthetic end to end: all-zero account serials in the
`SV[MRT]-NNNNNN` shape and round example values. `gaps_to_fill` is a
pure function of the marks and the months the statements reach, and
carries the rules that keep a derived mark honest — so it is tested
directly, with no file at all.
"""

from __future__ import annotations

import sys
from datetime import date
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent))

import derived_marks as dm  # noqa: E402


def _workbook(tmp_path, sheets):
    """Write a workbook shaped like the advisor's: one sheet per account,
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
