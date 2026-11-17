"""Unit tests for statements.py — the per-LP capital-statement parser.

Synthetic text blocks in both source phrasings (the quarterly report's
"Partner's Capital Statement" section and the dedicated year-end
"Limited Partner's Capital Statement" PDF). All figures synthetic.
"""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import statements  # noqa: E402
from collectorkit import pdftotext  # noqa: E402

QUARTERLY = """\
                 Example Fund of Funds, LP
              Partner's Capital Statement (Unaudited)

Beginning Balance                                  $    91,000 $   41,000 $        —
Called capital                                              —      60,000     202,000
Net change in unrealized gain/(loss)                     (800)      7,700       7,700
Distributions                                               —        (990)       (990)
Capital account balance at September 30, 2025      $    88,800 $   88,800 $    88,800

                                                     As of September 30, 2025
Committed capital                                  $   540,000
Paid in capital, since inception                       202,000
Unfunded commitment                                    338,000
Distributions, since inception                            (990)
"""

DEDICATED = """\
                 Example Fund of Funds, LP and
                 Example Fund of Funds QP, LP
              Limited Partner's Capital Statement (Unaudited)

Beginning Capital Account Balance                        $    88,800 $    41,000 $         —
Capital Contributions                                         60,000     202,000     338,000
Net Change in Unrealized Gains (Losses) on Investments         2,600       9,300       9,300
Capital Account Balance, December 31, 2025               $   233,300 $   233,300 $   233,300

                                    Commitment Summary
Capital Commitment                                       $   540,000
Capital Called Through December 31, 2025                 $   338,000
Distributions Through December 31, 2025                  $      (770)
"""


def test_parse_quarterly_phrasing():
    p = statements.parse_partner_capital_statement(QUARTERLY)
    assert p == {"ending_capital_cents": 8880000,
                 "contributed_cents": 20200000,
                 "distributions_cents": 99000}


def test_parse_dedicated_phrasing():
    p = statements.parse_partner_capital_statement(DEDICATED)
    assert p == {"ending_capital_cents": 23330000,
                 "contributed_cents": 33800000,
                 "distributions_cents": 77000}


def test_parse_rejects_non_statement_text():
    assert statements.parse_partner_capital_statement("Schedule of Investments\n") is None
    assert statements.parse_partner_capital_statement("") is None


def test_cents():
    assert statements._cents("1,234.56") == 123456
    assert statements._cents("(273)") == -27300
    assert statements._cents("—") is None
    assert statements._cents("") is None


# What `pdftotext -layout` gives for a two-page report: each page ends in
# a form feed, and the text holds characters beyond ASCII.
LAYOUT_TEXT = QUARTERLY + "\f" + DEDICATED + "  Total — €\n\f"


def _layout_text(calls, text=LAYOUT_TEXT):
    def layout_text(path, *, timeout=None):
        calls.append(Path(path).name)
        return text
    return layout_text


def test_pdf_text_is_the_layout_text_and_extracts_each_document_once(
        tmp_path, monkeypatch, cache_dir):
    calls = []
    monkeypatch.setattr(pdftotext, "layout_text", _layout_text(calls))
    report = tmp_path / "report.pdf"
    report.write_bytes(b"%PDF-1.4 synthetic")
    assert statements.pdf_text(report) == LAYOUT_TEXT
    # A copy of the same bytes (a run's hardlink, a later load) is read
    # from the cache, and gives the same text to the byte.
    again = tmp_path / "again.pdf"
    again.write_bytes(report.read_bytes())
    assert statements.pdf_text(again) == LAYOUT_TEXT
    assert calls == ["report.pdf"]
    assert len(list((cache_dir / "extract").glob("*.json.gz"))) == 1


def test_pdf_text_of_a_document_pdftotext_cannot_read_is_empty(
        tmp_path, monkeypatch):
    calls = []

    def fail(path, *, timeout=None):
        calls.append(Path(path).name)
        raise pdftotext.ExtractionError("encrypted")
    monkeypatch.setattr(pdftotext, "layout_text", fail)
    report = tmp_path / "report.pdf"
    report.write_bytes(b"%PDF-1.4 synthetic")
    assert statements.pdf_text(report) == ""
    # A failure is not kept: the next load tries again.
    assert statements.pdf_text(report) == ""
    assert calls == ["report.pdf", "report.pdf"]

