"""Tests for collectorkit.testpdf, the PDFs tests draw for themselves:
the extractors read back the text where it was drawn. Skips where poppler
is not installed."""
from __future__ import annotations

import shutil

import pytest

from collectorkit import pdftotext, testpdf

needs_poppler = pytest.mark.skipif(shutil.which("pdftotext") is None,
                                   reason="poppler-utils not installed")


def _text(tmp_path, data: bytes) -> str:
    path = tmp_path / "drawn.pdf"
    path.write_bytes(data)
    return pdftotext.layout_text(path)


def test_the_bytes_depend_only_on_the_arguments():
    page = testpdf.runs([(72, 700, "A (b) \\ c")], size=10)
    assert testpdf.pdf([page]) == testpdf.pdf([page])
    assert b"(A \\(b\\) \\\\ c) Tj" in page
    assert testpdf.pdf([page]) != testpdf.pdf([page], page=testpdf.A4)


@needs_poppler
def test_runs_are_read_back_on_their_lines(tmp_path):
    data = testpdf.pdf([testpdf.runs([(72, 700, "LEFT"), (400, 700, "1,00"),
                                      (72, 680, "BELOW")], size=12)])
    lines = [ln.split() for ln in _text(tmp_path, data).splitlines()
             if ln.strip()]
    assert lines == [["LEFT", "1,00"], ["BELOW"]]


@needs_poppler
def test_lines_run_down_the_page_and_pages_follow_each_other(tmp_path):
    data = testpdf.pdf([testpdf.lines(["first", "second"], size=6, leading=8,
                                      x=20, y=800),
                        testpdf.lines(["Café page"], size=6, leading=8,
                                      x=20, y=800)],
                       font="Courier", page=testpdf.A4)
    pages = _text(tmp_path, data).split("\f")
    assert [ln.strip() for ln in pages[0].splitlines() if ln.strip()] == [
        "first", "second"]
    assert pages[1].strip() == "Café page"


def test_from_top_measures_down_from_the_top_edge():
    pages = [[(72, 92, "TOP")], [(20, 42, "NEXT")]]
    assert testpdf.from_top(pages, size=8) == testpdf.pdf(
        [testpdf.runs([(72, 700, "TOP")], size=8),
         testpdf.runs([(20, 750, "NEXT")], size=8)])
    assert testpdf.from_top(pages, size=6, font="Courier",
                            page=testpdf.A4) == testpdf.pdf(
        [testpdf.runs([(72, 750, "TOP")], size=6),
         testpdf.runs([(20, 800, "NEXT")], size=6)],
        font="Courier", page=testpdf.A4)
