"""Parsers for Schwab year-end tax documents: the 1099 Composite, the
Year-End Summary and the Gain/Loss Report.

Each kind reads its document in a module of its own: `form_1099b` the
1099-B section of the 1099 Composite's XML or CSV copy, and
`realized_reports` the realized lots of the Year-End Summary and the
Gain/Loss Report, PDFs both. This module holds the wrappers the loader
calls, which take a file's path.

No PII lives in these modules — they parse whatever the bronze file
holds. Real names / numbers land only in the *local* silver DB, never
in tracked source or tests; the test fixtures are wholly synthetic.
"""
from __future__ import annotations

from pathlib import Path

import extraction
import form_1099b
import realized_reports


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

