"""Parsers for Schwab year-end tax documents: the 1099 Composite, the
Year-End Summary and the Gain/Loss Report.

Each kind reads its document in a module of its own: `form_1099b` the
1099-B section of the 1099 Composite's XML or CSV copy, and
`realized_reports` the realized lots of the Year-End Summary and the
Gain/Loss Report, PDFs both. This module keeps the names the loader
and the tests import, and the wrappers that take a file's path.

No PII lives in these modules — they parse whatever the bronze file
holds. Real names / numbers land only in the *local* silver DB, never
in tracked source or tests; the test fixtures are wholly synthetic.
"""
from __future__ import annotations

from pathlib import Path
from typing import NamedTuple

import extraction
import form_1099b
import realized_reports
from form_1099b import parse_1099b_csv, parse_1099b_xml  # noqa: F401
from realized_reports import (  # noqa: F401
    _YES_COLUMN_SLACK,
    _YES_LAYOUT_FIELDS,
    _same_row,
    _yes_lot_amounts,
    _yes_subtitle,
    parse_gain_loss_report_text,
    parse_year_end_summary_text,
)


def parse_1099b(path, fmt: str | None = None) -> dict:
    """Parse a 1099 Composite file (XML preferred, CSV fallback).

    `fmt` is 'xml' / 'csv'; inferred from the extension when omitted.
    Returns {"tax_year": int|None, "lots": [row, ...]}."""
    p = Path(path)
    fmt = (fmt or p.suffix.lstrip(".")).lower()
    text = p.read_text(encoding="utf-8", errors="replace")
    return form_1099b.parse(text, fmt)


def parse_realized_report_pdf(path, kind: str) -> dict:
    """Parse a realized-lot report PDF: `kind` is 'year_end_summary' (a
    Year-End Summary or 1099 Composite PDF) or 'gain_loss_report'."""
    extracted = extraction.pdfium(path, words=kind == "year_end_summary")
    return realized_reports.parse(extracted, kind)


# A page's words with the box's bottom before its top, both in PDFium's
# bottom-up frame: the shape collectorkit's word tests compare
# `collectorkit.words.Word` and its geometry against.

class _Word(NamedTuple):
    text: str
    x0: float
    x1: float
    bottom: float
    top: float

    @property
    def mid(self) -> float:
        return (self.bottom + self.top) / 2


def _pdf_words(path) -> list[list[_Word]]:
    """Every page's words, as :data:`extraction.WORDS` reads them."""
    return [[_Word(w.text, w.x0, w.x1, w.bottom, w.top) for w in page.words()]
            for page in extraction.pdfium(path, words=True).pages]
