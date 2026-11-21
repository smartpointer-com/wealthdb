"""A one-page PDF whose text is the given lines, for loader tests.

Courier text, one line per row, top to bottom, so pypdf and PDFium both
read each line back as written (collectorkit.testpdf). No real document
is used.
"""
from __future__ import annotations

from collectorkit import testpdf


def pdf_bytes(lines: list[str]) -> bytes:
    """The bytes of a one-page PDF that shows `lines`."""
    return testpdf.pdf([testpdf.lines(lines, size=6, leading=8, x=20, y=800)],
                       font="Courier", page=testpdf.A4)
