"""
Unit tests for pdf_readers.py: the extraction each kind is read with,
the extraction cache, the lazy 1099 identity read and the runner.

pdfplumber is stood in for by a module that serves invented pages and
counts what is read, so no real PDF is needed. Every value is invented.
"""

from __future__ import annotations

import json
import sys
import types
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import pdf_readers  # noqa: E402

_STATEMENT = """\
INVESTMENT REPORT
January 1, 2026 - March 31, 2026
Account # 100-000001
Holdings
PLACEHOLDER FUND 100% $1,000.00 10.000 $100.00 $1,000.00
Total Market Value $1,000.00
"""

# A Giving Account statement whose two pools reconcile to its totals.
_DAF_STATEMENT = """\
Account Summary:
Beginning Value as of April 1, 2025 $ 90,000.00
Ending Value as of June 30, 2025 $ 100,000.00
GivingAccount Pool Holdings:
Units Price per Unit Total Value Total Value
June 30, 2025 June 30, 2025 April 1, 2025 June 30, 2025
Example Growth 1,000.000 $ 60.00 $ 54,000.00 $ 60,000.00
Example Income 2,000.000 $ 20.00 $ 36,000.00 $ 40,000.00
Total Value $ 90,000.00 $ 100,000.00
Transaction Detail:
"""


def _words(text, top):
    out, x0 = [], 20.0
    for token in text.split():
        out.append({"text": token, "x0": x0, "x1": x0 + 5 * len(token),
                    "top": top, "bottom": top + 8.0})
        x0 += 5 * len(token) + 3
    return out


_1099_PAGES = [
    ["2025 TAX REPORTING STATEMENT",
     "PLACEHOLDER HOLDER Account No. 100-000001",
     "02/15/2026 9000000000 Pages 1 of 3"],
    ["FORM 1099-B* Copy B for Recipient"],
    ["Form 1099-OID 2025 Original Issue Discount"],
]


@pytest.fixture
def plumber(monkeypatch):
    """A stand-in pdfplumber. ``reads`` counts each page method call."""
    reads = []

    class _Page:
        def __init__(self, i, lines):
            self.i, self.lines = i, lines

        def extract_text(self):
            reads.append(("text", self.i))
            return "\n".join(self.lines)

        def extract_words(self):
            reads.append(("words", self.i))
            return [w for k, line in enumerate(self.lines)
                    for w in _words(line, 100.0 + 12 * k)]

    class _Doc:
        def __init__(self, path):
            name = Path(path).name
            if name.startswith("STATEMENT_"):
                pages = [_DAF_STATEMENT.splitlines()]
            elif name.startswith("Statement") or name.endswith(" Statement.pdf"):
                pages = [_STATEMENT.splitlines()]
            else:
                pages = _1099_PAGES
            self.pages = [_Page(i, lines) for i, lines in enumerate(pages)]

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    fake = types.ModuleType("pdfplumber")
    fake.open = _Doc
    monkeypatch.setitem(sys.modules, "pdfplumber", fake)
    return reads


def test_a_statement_is_extracted_then_parsed(plumber, tmp_path):
    doc = pdf_readers.parse_statement_pdf(tmp_path / "Statement3312026.pdf")
    assert doc["period_end"] == "2026-03-31"
    assert doc["accounts"][0]["holdings"][0]["quantity"] == 10.0
    assert doc["path"] == str(tmp_path / "Statement3312026.pdf")


def test_a_cached_extraction_is_parsed_without_opening_the_pdf(
        plumber, tmp_path):
    path = tmp_path / "Statement3312026.pdf"
    first = pdf_readers.parse_statement_pdf(
        path, sha="ab" * 32, cache_dir=tmp_path / "extract")
    plumber.clear()
    again = pdf_readers.parse_statement_pdf(
        path, sha="ab" * 32, cache_dir=tmp_path / "extract")
    assert again == first
    assert plumber == []
    assert len(list((tmp_path / "extract").iterdir())) == 1


def test_a_supplied_statement_is_read_with_its_signature(plumber, tmp_path):
    path = tmp_path / "Placeholder 3.26 Statement.pdf"
    doc = pdf_readers.parse_supplied_statement_pdf(
        path, expected_signature="INVESTMENT REPORT")
    assert "_error" not in doc
    assert doc["period_end"] == "2026-03-31"
    assert pdf_readers.parse_supplied_statement_pdf(
        path, expected_signature="ANOTHER REGISTRATION") == {
        "_error": "signature-mismatch",
        "expected_signature": "ANOTHER REGISTRATION", "path": str(path)}


def test_a_daf_statement_is_extracted_then_parsed(plumber, tmp_path):
    doc = pdf_readers.parse_daf_statement_pdf(
        tmp_path / "STATEMENT_2098_06_30.pdf")
    assert (doc["as_of_date"], doc["reconciled"]) == ("2025-06-30", True)
    assert [p["market_value"] for p in doc["pools"]] == [60000.0, 40000.0]


def test_the_1099_identity_reads_only_the_pages_it_needs(plumber, tmp_path):
    ident = pdf_readers.read_consolidated_1099_identity(
        tmp_path / "Consolidated_Form_1099.pdf")
    assert (ident["tax_year"], ident["account_external_id"],
            ident["prepared"]) == (2025, "100000001", "2026-02-15")
    assert plumber == [("words", 0)]


def test_the_1099_is_parsed_from_every_pages_words(plumber, tmp_path):
    doc = pdf_readers.parse_consolidated_1099_pdf(
        tmp_path / "Consolidated_Form_1099.pdf")
    assert doc["tax_year"] == 2025 and doc["lots"] == []
    assert {("words", i) for i in range(3)} <= set(plumber)


def test_the_runner_prints_what_one_kind_reads(plumber, tmp_path):
    out = tmp_path / "out.json"
    assert pdf_readers.main(["statement", str(tmp_path / "Statement1.pdf"),
                             "--json-out", str(out)]) == 0
    [doc] = json.loads(out.read_text())
    assert doc["period_end"] == "2026-03-31"


@pytest.mark.parametrize("kind, name, key, value", [
    ("supplied", "Placeholder 3.26 Statement.pdf", "period_end", "2026-03-31"),
    ("daf", "STATEMENT_2098_06_30.pdf", "as_of_date", "2025-06-30"),
])
def test_the_runner_reads_each_statement_kind(plumber, tmp_path, kind, name,
                                              key, value):
    out = tmp_path / "out.json"
    assert pdf_readers.main([kind, str(tmp_path / name),
                             "--json-out", str(out)]) == 0
    [doc] = json.loads(out.read_text())
    assert doc[key] == value


def test_the_runner_refuses_an_unknown_kind(capsys):
    assert pdf_readers.main(["nonsense", "x.pdf"]) == 2
    assert "usage: pdf_readers.py" in capsys.readouterr().err
