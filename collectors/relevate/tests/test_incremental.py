"""The standing test: the fixture dumps loaded one at a time build the
silver one `load --force` builds.

Three tiny synthetic dumps: a report and a credit note, a second report
beside the first, then a restatement of the first report under another
valuation date and a PDF that does not parse.
Synthetic ids, ISINs and amounts only.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
sys.path.insert(0, str(HERE))

import load as loader  # noqa: E402
from collectorkit import bronze, incremental  # noqa: E402
from tinypdf import pdf_bytes  # noqa: E402

pytest.importorskip("pypdf")

ACC = "9999.999999.9"

REPORT = """\
  Reference no.                                       9999.999999.9
  Valuation date                                      {date}

Portfolio Detail
  Asset Class         Security                                          ISIN              Currency      Units     Performance      Allocation     Allocation in CHF

  Stocks
                       Acme Index Equity Fund                            XX0000000001        USD       1'234.567        5.20             {share}         50'000.00

  Liquidity
   Liquidity CHF                                                                              CHF                                           7.00          7'000.00

Performance
"""

CREDIT_NOTE = """\
Gutschriftsanzeige Seite 1/1
Betrag:
Valuta:
CHF {amount}
{date}
PensFree
9999.999999.9
"""


def _write_json(path: Path, obj) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(obj), encoding="utf-8")


def _dump(root: Path, slug: str, cash: float, docs) -> Path:
    d = root / slug
    _write_json(d / "run.json", {"tool": "relevate.download", "mode": "all",
                                 "status": "complete", "files": ["a"]})
    _write_json(d / "accounts" / "investment-overview.json", {"portfolios": [{
        "externalId": ACC, "id": 100, "product": {"key": "FZPF"},
        "currency": {"currencyCode": "CHF"}, "cashBalance": cash}]})
    _write_json(d / "portfolios" / bronze.short_token(ACC) / "modelportfolio.json",
                {"positions": [{"security": {"id": 1, "isin": "XX0000000001",
                                             "name": "Acme Index Equity Fund"},
                                "allocation": 0.6}]})
    for doc_id, _, body in docs:
        (d / "documents").mkdir(parents=True, exist_ok=True)
        (d / "documents" / f"{doc_id}.pdf").write_bytes(body)
    _write_json(d / "documents" / "index.json", {"documents": [
        {"id": doc_id, "fileName": name} for doc_id, name, _ in docs]})
    return d


def _report(date: str, share: str) -> bytes:
    return pdf_bytes(REPORT.format(date=date, share=share).splitlines())


def _credit_note(amount: str, date: str) -> bytes:
    return pdf_bytes(CREDIT_NOTE.format(amount=amount, date=date).splitlines())


def test_dumps_loaded_one_at_a_time_build_the_rebuilt_silver(tmp_path):
    src = tmp_path / "src"
    q1 = ("11", "Quartalsbericht Q1.pdf")
    runs = [
        _dump(src, "20240101T000000Z", 1.0, [
            (*q1, _report("31.03.2099", "45.00")),
            ("21", "Gutschriftsanzeige 1.pdf", _credit_note("100.00", "15.01.2099"))]),
        _dump(src, "20240401T000000Z", 2.0, [
            (*q1, _report("31.03.2099", "45.00")),
            ("12", "Quartalsbericht Q2.pdf", _report("30.06.2099", "44.00"))]),
        _dump(src, "20240701T000000Z", 3.0, [
            (*q1, _report("30.04.2099", "46.00")),
            ("13", "Quartalsbericht Q3.pdf", b"%PDF-1.4 does not parse")]),
    ]
    incremental.check(
        loader.main,
        lambda db, bronze_dir: ["--silver-db", str(db),
                                "--bronze-dir", str(bronze_dir)],
        runs, tmp_path / "work")
    # The PDFs were read: both passes wrote rows.
    rows = incremental.silver_rows(tmp_path / "work" / "at-once.db")
    assert len(rows["historical_position_snapshots"]) == 2
    assert len(rows["transactions"]) == 1
