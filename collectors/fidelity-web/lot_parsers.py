"""Parsers for the positions page's lot tables.

``parse_open_lots`` reads one page of an open-lot table, the HTML the
page's ``openlots`` query returns. Columns are found by their header
text, not by position, so a reordered or added column does not shift
the others. Every cell also stays in the row's ``cells`` as printed.

``closed_lot_row`` reads one lot of a ``closedlots`` answer, a JSON
object of printed strings, into `closed_lots` columns.
"""

from __future__ import annotations

import re
from datetime import datetime
from html.parser import HTMLParser

# Header text → silver column. A header not listed here is kept in
# `cells` only.
OPEN_LOT_COLUMNS = {
    "acquired": "acquired_date",
    "term": "term",
    "$ total gain/loss": "unrealized_gain_loss",
    "current value": "current_value",
    "quantity": "quantity",
    "average cost basis": "unit_cost",
    "cost basis total": "cost_basis",
}
_NUMERIC = ("unrealized_gain_loss", "current_value", "quantity", "unit_cost",
            "cost_basis")

_NUMBER_RE = re.compile(r"^([+-]?)\$?([0-9][0-9,]*(?:\.[0-9]+)?)$")


def parse_number(text):
    """A printed amount or count as a float: ``+$1,234.56``, ``-$7.00``,
    ``12.345``. None for ``--``, a blank, or anything else."""
    m = _NUMBER_RE.match((text or "").strip().replace(" ", ""))
    if not m:
        return None
    value = float(m.group(2).replace(",", ""))
    return -value if m.group(1) == "-" else value


def parse_lot_date(text):
    """``Dec-05-2024`` → ``2024-12-05``. Text that is not such a date
    is returned as printed, and a blank or ``--`` as None."""
    text = (text or "").strip()
    if not text or text == "--":
        return None
    try:
        return datetime.strptime(text, "%b-%d-%Y").date().isoformat()
    except ValueError:
        return text


class _TableReader(HTMLParser):
    """Collects the header texts and the body rows of the first table."""

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.headers = []
        self.rows = []
        self._section = None
        self._cell = None
        self._row = None

    def handle_starttag(self, tag, attrs):
        if tag in ("thead", "tbody"):
            self._section = tag
        elif tag == "tr" and self._section == "tbody":
            self._row = []
        elif tag in ("th", "td"):
            self._cell = []

    def handle_endtag(self, tag):
        if tag in ("th", "td") and self._cell is not None:
            text = " ".join("".join(self._cell).split())
            if tag == "th" and self._section == "thead":
                self.headers.append(text)
            elif tag == "td" and self._row is not None:
                self._row.append(text)
            self._cell = None
        elif tag == "tr" and self._row is not None:
            if self._row:
                self.rows.append(self._row)
            self._row = None
        elif tag in ("thead", "tbody"):
            self._section = None

    def handle_data(self, data):
        if self._cell is not None:
            self._cell.append(data)


def parse_open_lots(html):
    """The lots on one page of an open-lot table, in print order.

    Each lot is a dict of the silver columns the headers name, plus
    ``cells``: every header mapped to its cell text as printed. Raises
    ValueError when the HTML holds no lot table, or one without an
    Acquired or a Quantity column."""
    reader = _TableReader()
    reader.feed(html or "")
    headers = [h.casefold() for h in reader.headers]
    if "acquired" not in headers or "quantity" not in headers:
        raise ValueError("no open-lot table with Acquired and Quantity "
                         "columns")
    lots = []
    for cells in reader.rows:
        printed = dict(zip(reader.headers, cells, strict=False))
        lot = {"cells": printed}
        for header, text in zip(headers, cells, strict=False):
            column = OPEN_LOT_COLUMNS.get(header)
            if column in _NUMERIC:
                lot[column] = parse_number(text)
            elif column == "acquired_date":
                lot[column] = parse_lot_date(text)
            elif column == "term":
                lot[column] = text.upper() if text not in ("", "--") else None
        lots.append(lot)
    return lots


def closed_lot_row(lot):
    """One lot of a ``closedlots`` answer as `closed_lots` columns.

    The page prints a lot's gain under a short-term or a long-term
    column and ``--`` under the other; the column that holds a figure
    gives ``term``. Both or neither leave ``term`` NULL, and the gain is
    what they hold together."""
    short = parse_number(lot.get("shortTerm"))
    long_ = parse_number(lot.get("longTerm"))
    gains = [g for g in (short, long_) if g is not None]
    term = None
    if short is not None and long_ is None:
        term = "SHORT"
    elif long_ is not None and short is None:
        term = "LONG"
    return {
        "security_name": (lot.get("desc") or "").strip() or None,
        "quantity": parse_number(lot.get("quantity")),
        "acquired_date": _iso_or_printed(lot.get("dateAcquired")),
        "disposed_date": _iso_or_printed(lot.get("dateSold")),
        "proceeds": parse_number(lot.get("proceeds")),
        "cost_basis": parse_number(lot.get("costBasis")),
        "realized_gain_loss": sum(gains) if gains else None,
        "term": term,
    }


def _iso_or_printed(text):
    """An ISO date as given; ``--`` or a blank as None; anything else
    as printed."""
    text = (text or "").strip()
    return text if text and text != "--" else None
