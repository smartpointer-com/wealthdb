"""Minimal one-page PDFs for the equityzen loader tests: text placed at
given points, which pdftotext reads as any other PDF. Synthetic data only."""
from __future__ import annotations

from collectorkit import testpdf


def pdf_bytes(pages: list[list[tuple[float, float, str]]]) -> bytes:
    """A minimal PDF in one font, Helvetica at 8 points: per page, (x, y,
    text) triples, y measured down from the top of a letter page to the
    baseline. pdftotext reads it as any other PDF."""
    return testpdf.from_top(pages, size=8)


def text_page(text: str) -> list[tuple[float, float, str]]:
    """`text` as one PDF page that `pdftotext -layout` gives back line by
    line: each run of characters between two or more spaces is placed at
    its column."""
    out = []
    for i, line in enumerate(text.splitlines()):
        col = 0
        for part in line.split("  "):
            if part.strip():
                out.append((40 + 4.6 * col, 60 + 12 * i, part.strip()))
            col += len(part) + 2
    return out

