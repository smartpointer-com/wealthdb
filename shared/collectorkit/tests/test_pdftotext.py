"""Tests for collectorkit.pdftotext.

The failure split is tested with `subprocess.run` faked; the real binary is
exercised on a PDF built here, and those tests skip where poppler is not
installed.
"""
from __future__ import annotations

import shutil
import subprocess

import pytest

from collectorkit import pdftotext, testpdf

needs_poppler = pytest.mark.skipif(shutil.which("pdftotext") is None,
                                   reason="poppler-utils not installed")


def _pdf(*runs: tuple[int, int, str]) -> bytes:
    """A one-page PDF printing each (x, y, text) run."""
    return testpdf.pdf([testpdf.runs(runs, size=12)])


def _fake_run(monkeypatch, result=None, raises=None):
    def run(argv, **kwargs):
        if raises is not None:
            raise raises
        return subprocess.CompletedProcess(argv, *result)
    monkeypatch.setattr(pdftotext.subprocess, "run", run)


@needs_poppler
def test_the_printed_columns_survive(tmp_path):
    # Two rows whose left cells differ in width: the right-hand column
    # still starts at one character column on both.
    path = tmp_path / "columns.pdf"
    path.write_bytes(_pdf((72, 700, "A"), (400, 700, "1,00"),
                          (72, 680, "A MUCH LONGER CELL"), (400, 680, "2,00")))
    lines = [ln for ln in pdftotext.layout_text(path).splitlines() if ln.strip()]
    assert len(lines) == 2
    assert lines[0].index("1,00") == lines[1].index("2,00")


@needs_poppler
def test_a_file_that_is_no_pdf_fails_alone(tmp_path):
    path = tmp_path / "broken.pdf"
    path.write_bytes(b"not a pdf")
    with pytest.raises(pdftotext.ExtractionError) as err:
        pdftotext.layout_text(path)
    assert not isinstance(err.value, pdftotext.ToolMissing)


def test_no_binary_is_tool_missing(tmp_path, monkeypatch):
    monkeypatch.setenv("PATH", str(tmp_path))
    with pytest.raises(pdftotext.ToolMissing):
        pdftotext.layout_text(tmp_path / "any.pdf")


def test_a_failed_exit_names_its_code_and_message(monkeypatch):
    _fake_run(monkeypatch, (1, b"", b"Syntax Error: broken xref\n"))
    with pytest.raises(pdftotext.ExtractionError,
                       match="exited 1: Syntax Error: broken xref") as err:
        pdftotext.layout_text("x.pdf")
    assert not isinstance(err.value, pdftotext.ToolMissing)


def test_a_timeout_fails_the_file_not_the_tool(monkeypatch):
    _fake_run(monkeypatch, raises=subprocess.TimeoutExpired("pdftotext", 30))
    with pytest.raises(pdftotext.ExtractionError, match="timed out after 30s") as err:
        pdftotext.layout_text("x.pdf", timeout=30)
    assert not isinstance(err.value, pdftotext.ToolMissing)


def test_an_undecodable_byte_is_replaced_not_fatal(monkeypatch):
    _fake_run(monkeypatch, (0, b"Saldo \xff 1,00\n", b""))
    assert pdftotext.layout_text("x.pdf") == "Saldo � 1,00\n"
