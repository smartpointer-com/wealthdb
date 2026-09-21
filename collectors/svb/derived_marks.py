"""
Month-end account values from the advisor's performance workbook.

The statement archive has interior gaps — months where an account held
value but no statement covering it survives. Carry-forward is honest
across such a gap only while nothing else moves; here it is not enough,
because gold ends an account's series at the first later snapshot of its
source that re-covers every account seen alongside it. A month the
archive misses for one account but covers for its peers therefore reads
as that account's CLOSURE, and its value drops to zero until the next
statement brings it back.

The advisor's workbook carries a month-end market value per account for
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

The workbook is read through openpyxl, and a missing file is not an
error: the pass yields nothing and the archive stands on its statements
alone.
"""

from __future__ import annotations

import calendar
import re
from dataclasses import dataclass
from datetime import date, timedelta

# Excel serial dates count from this epoch (the 1900 system, including
# its deliberate leap-year bug, which is why day 1 is 1900-01-01).
_EXCEL_EPOCH = date(1899, 12, 30)

# One sheet per account, named by the account's own serial with the
# family letter but without the dash: "M000000" is SVM-000000. A
# sheet whose name is not that shape is not an account, and is
# skipped.
_SHEET_RE = re.compile(r"^(?P<family>[MRT])(?P<serial>\d{6})$")

# The columns read from each sheet. The workbook also carries beginning
# value, net additions, gain, income and fees; only the ending market
# value is taken, because it is the only one a position row needs and
# the only one verified against the statements.
_DATE_HEADER = "Date"
_VALUE_HEADER = "EMV"

# What a derived row carries where a real one carries its statement's
# sha256. Not a hash, so it cannot collide with one, and greppable.
DERIVED_SHA = "derived-advisor-mark"

# The row's description, and so part of its primary key. Distinct from
# every statement-derived description, so a derived mark and a real row
# can never be mistaken for one another — or silently replace one.
DERIVED_DESC = "ACCOUNT VALUE (ADVISOR MARK)"


@dataclass
class DerivedMark:
    """One month-end account value taken from the workbook."""
    account_external_id: str
    as_of: str                # ISO YYYY-MM-DD, month end
    market_value: float


def read_workbook(path):
    """Return every ``(account, month-end, value)`` the workbook states.

    Returns ``[]`` when the file is absent — the archive then stands on
    its statements alone. Rows whose date or value will not parse are
    skipped: this is a hand-maintained sheet, and a blank or a dash in a
    cell is how it spells "nothing here".
    """
    if not path or not path.is_file():
        return []

    # Imported here, below the guard: the pass runs on every build against a
    # workbook that is usually absent, so the common path must not need a
    # reader at all — as with the collector's other heavy dependencies.
    import openpyxl

    book = openpyxl.load_workbook(str(path), read_only=True, data_only=True)
    try:
        marks = []
        for name in book.sheetnames:
            m = _SHEET_RE.match(name.strip())
            if not m:
                continue
            account = f"SV{m['family']}-{m['serial']}"
            marks.extend(_read_sheet(book[name], account))
        return marks
    finally:
        book.close()


def _read_sheet(sheet, account):
    rows = sheet.iter_rows(values_only=True)
    try:
        header = [str(c).strip() if c is not None else "" for c in next(rows)]
    except StopIteration:
        return []
    try:
        date_col = header.index(_DATE_HEADER)
        value_col = header.index(_VALUE_HEADER)
    except ValueError:
        return []
    out = []
    for row in rows:
        if len(row) <= max(date_col, value_col):
            continue
        when = _as_date(row[date_col])
        value = _as_number(row[value_col])
        if when is None or value is None:
            continue
        # The sheet dates a period by its START; the value is the
        # period's END, so the mark belongs at that month's end.
        out.append(DerivedMark(account, _month_end(when).isoformat(), value))
    return out


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
