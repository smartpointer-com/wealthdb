"""
Reads a performance workbook of month-end account values.

The statement archive has interior gaps — months where an account held
value but no statement covering it survives. Gold carries the account's
earlier month across such a gap, which is honest only while nothing
moves: an account's market value moves every month, so the carried
figure is stale for the whole gap.

The workbook carries a month-end market value per account for
exactly those months, on a basis verified against the custodial
statements. This module reads it, so a gap can be filled with a SOURCED
mark instead of an invented one.

The rules that keep that honest:

* **Only where the archive has nothing.** A month with any real row for
  that account is left alone — the statement is the better record, and
  two rows for one month would double the account.
* **Only INSIDE an account's real coverage.** A derived mark never
  precedes an account's first statement or follows its last: the series
  still begins and ends where the statements do. Filling past the end
  would be inventing a position, not sourcing one.
* **Marked at row level.** Every derived row carries
  :data:`DERIVED_SHA` where a real one carries its statement's sha256,
  so the two are distinguishable in silver, in gold, and in any audit —
  and a statement that later joins the archive simply takes the month
  back.

Each sheet also states the month's flows: beginning value, accrual,
net additions, net gain, income and fees. :func:`read_stated_periods`
reads every row in full, so silver keeps the stated month beside the
ledger's. Those records never create a transaction and never fill a
gap; only the ending value reaches a position row.

The workbook is read through openpyxl, and a missing file is not an
error: the pass yields nothing and the archive stands on its statements
alone.
"""

from __future__ import annotations

import calendar
import re
from dataclasses import dataclass
from datetime import date, datetime, time, timedelta

# Excel serial dates count from this epoch (the 1900 system, including
# its deliberate leap-year bug, which is why day 1 is 1900-01-01).
_EXCEL_EPOCH = date(1899, 12, 30)

# One sheet per account, named by the account's own serial with the
# family letter but without the dash: "M000000" is SVM-000000. A
# sheet whose name is not that shape is not an account, and is
# skipped.
_SHEET_RE = re.compile(r"^(?P<family>[MRT])(?P<serial>\d{6})$")

# The columns of each sheet. A stated period takes all seven value
# columns, each under its silver name. A derived mark takes only the
# ending market value ("EMV"), because it is the only one a position row
# needs, and a statement states it too.
_DATE_HEADER = "Date"
_PERIOD_HEADERS = {
    "BMV": "beginning_value",
    "EMV": "ending_value",
    "Accrual": "accrual",
    "Net Additions": "net_additions",
    "Net Gain": "net_gain",
    "Income": "income",
    "Fees": "fees",
}

# What a derived row carries where a real one carries its statement's
# sha256. Not a hash, so it cannot collide with one, and greppable.
DERIVED_SHA = "derived-workbook-mark"

# The row's description, and so part of its primary key. Distinct from
# every statement-derived description, so a derived mark and a real row
# can never be mistaken for one another — or silently replace one.
DERIVED_DESC = "ACCOUNT VALUE (WORKBOOK MARK)"


@dataclass
class DerivedMark:
    """One month-end account value taken from the workbook."""
    account_external_id: str
    as_of: str                # ISO YYYY-MM-DD, month end
    market_value: float


@dataclass
class StatedPeriod:
    """One account's month as a workbook row states it.

    A value is None where the cell states nothing. ``cells`` keeps the
    row as read, header name to value, with a date as ISO text.
    """
    account_external_id: str
    period_start: str         # ISO YYYY-MM-DD, the row's date
    period_end: str           # ISO YYYY-MM-DD, that month's end
    beginning_value: float | None
    ending_value: float | None
    accrual: float | None
    net_additions: float | None   # signed: in positive, out negative
    net_gain: float | None
    income: float | None
    fees: float | None            # a positive amount, as stated
    cells: dict


def read_workbook(path):
    """Return every ``(account, month-end, value)`` the workbook states.

    Returns ``[]`` when the file is absent, as :func:`read_stated_periods`
    does.
    """
    return marks_of(read_stated_periods(path))


def marks_of(periods):
    """The marks among stated `periods`. A mark is a stated period's
    ending value, so a row whose EMV cell states nothing is no mark."""
    return [DerivedMark(p.account_external_id, p.period_end, p.ending_value)
            for p in periods if p.ending_value is not None]


def read_stated_periods(path):
    """Return every month the workbook states, all seven values read.

    Returns ``[]`` when the file is absent — the archive then stands on
    its statements alone. A row becomes a record when its date parses
    and at least one value does. This is a hand-maintained sheet, and a
    blank, a dash or text in a value cell is how it spells "nothing
    here": such a cell reads as None.
    """
    if not path or not path.is_file():
        return []

    # Imported here, below the guard: the pass runs on every build against a
    # workbook that is usually absent, so the common path must not need a
    # reader at all — as with the collector's other heavy dependencies.
    import openpyxl

    book = openpyxl.load_workbook(str(path), read_only=True, data_only=True)
    try:
        out = []
        for name in book.sheetnames:
            m = _SHEET_RE.match(name.strip())
            if not m:
                continue
            account = f"SV{m['family']}-{m['serial']}"
            out.extend(_read_period_sheet(book[name], account))
        return out
    finally:
        book.close()


def _header(rows):
    try:
        return [str(c).strip() if c is not None else "" for c in next(rows)]
    except StopIteration:
        return None


def _read_period_sheet(sheet, account):
    rows = sheet.iter_rows(values_only=True)
    header = _header(rows)
    if header is None or _DATE_HEADER not in header:
        return []
    date_col = header.index(_DATE_HEADER)
    columns = {field: header.index(name)
               for name, field in _PERIOD_HEADERS.items() if name in header}
    out = []
    for row in rows:
        if len(row) <= date_col:
            continue
        when = _as_date(row[date_col])
        if when is None:
            continue
        values = {field: _as_number(row[col]) if col < len(row) else None
                  for field, col in columns.items()}
        if all(v is None for v in values.values()):
            continue
        cells = {name: _as_json(cell)
                 for name, cell in zip(header, row, strict=False) if name}
        # The sheet dates a period by its START; its values close at the
        # period's END, so the record ends at that month's end.
        out.append(StatedPeriod(
            account_external_id=account,
            period_start=when.isoformat(),
            period_end=_month_end(when).isoformat(),
            cells=cells,
            **{field: values.get(field) for field in _PERIOD_HEADERS.values()},
        ))
    return out


def _as_json(cell):
    """A cell as JSON can hold it: a date as ISO text, a midnight
    datetime as its date alone."""
    if isinstance(cell, datetime):
        if cell.time() == time():
            return cell.date().isoformat()
        return cell.isoformat()
    if isinstance(cell, (date, time)):
        return cell.isoformat()
    return cell


def _as_date(cell):
    """A workbook date cell, as either a datetime or an Excel serial."""
    if hasattr(cell, "date"):
        return cell.date()
    if isinstance(cell, date):
        return cell
    try:
        serial = int(str(cell).strip())
    except (TypeError, ValueError):
        return None
    if serial <= 0:
        return None
    return _EXCEL_EPOCH + timedelta(days=serial)


def _as_number(cell):
    if isinstance(cell, (int, float)) and not isinstance(cell, bool):
        return float(cell)
    try:
        return float(str(cell).strip().replace(",", ""))
    except (TypeError, ValueError):
        return None


def _month_end(when):
    return date(when.year, when.month,
                calendar.monthrange(when.year, when.month)[1])


def gaps_to_fill(marks, covered):
    """The subset of ``marks`` that fills an interior gap.

    ``covered`` maps an account to the set of ``(year, month)`` pairs the
    statements already reach. A mark is kept only when its month is not
    among them AND it falls strictly inside that account's covered span,
    so the series is never extended at either end.
    """
    span = {account: (min(months), max(months))
            for account, months in covered.items() if months}
    out = []
    for mark in marks:
        months = covered.get(mark.account_external_id)
        if not months:
            continue
        key = (int(mark.as_of[:4]), int(mark.as_of[5:7]))
        if key in months:
            continue
        first, last = span[mark.account_external_id]
        if first < key < last:
            out.append(mark)
    return sorted(out, key=lambda m: (m.account_external_id, m.as_of))
