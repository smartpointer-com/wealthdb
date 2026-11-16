"""The standing test: the fixture dumps loaded one at a time build the
silver one `load --force` builds.

Three tiny synthetic dumps: live holdings, transactions and a Reporting
PDF; a second report and a manual one beside it; then the first report
restated under its number and a PDF that does not parse. Synthetic
portfolio numbers, ISINs and amounts only.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
sys.path.insert(0, str(HERE))

pytest.importorskip("pypdfium2")

import load as loader  # noqa: E402
from collectorkit import incremental  # noqa: E402
from tinypdf import pdf_bytes  # noqa: E402

PORT = "3.111.222.333.01"

REPORT = """\
Reporting as of {date}
Reporting date {date} Portfolio 3.111.222.333.01 "Synthetic Portfolio A"
Liquidity
3a Account CHF 100.00 0.30% Interest 5.00% 100.00
Equity
Switzerland CHF 2.50 Synthetic SMI Fund CH0000000011 1000.00 1200.00 20.00% 57.00% {value}
Real Estate
Switzerland CHF 3.00 Synthetic Property Fund CH0000000044 100.00 110.00 10.00% 4.00% 330.00
"""


def _write_json(path: Path, obj) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(obj), encoding="utf-8")


def _report(date: str, value: str) -> bytes:
    return pdf_bytes(REPORT.format(date=date, value=value).splitlines())


def _dump(root: Path, slug: str, k: int, docs) -> Path:
    d = root / slug
    _write_json(d / "run.json", {"utc": slug, "status": "complete"})
    _write_json(d / "wealth" / "portfolio-inventory.json", {
        "p3a": [{"number": PORT, "name": "Portfolio A", "state": "ACTIVE"}]})
    _write_json(d / "positions" / PORT / "assets.json", {
        "cashAmount": 100.0 + k, "assetsByClasses": {"EQUITIES": [{
            "isin": "CH0000000011", "name": "Synthetic SMI Fund",
            "currencyCode": "CHF", "amount": 2.5, "ratioInChf": 1200.0 + k,
            "acquisitionPrice": 1000.0, "assetPrice": 1200.0}]}})
    _write_json(d / "transactions" / "all.json", {"transactions": {PORT: [
        {"type": "CONTRIBUTION", "amountInChf": 100.0,
         "valueDate": f"2024-01-0{k + 1}", "documentNumber": f"T{k}"}]}})
    _write_json(d / "documents" / "index.json", [
        {"documentNumber": doc_id, "type": "REPORT", "subType": sub}
        for doc_id, sub, _ in docs])
    for doc_id, _, body in docs:
        (d / "documents" / f"{doc_id}.pdf").write_bytes(body)
    return d


def test_dumps_loaded_one_at_a_time_build_the_rebuilt_silver(tmp_path):
    src = tmp_path / "src"
    periodic, manual = "INVESTMENT_REPORTING", "MANUAL_INVESTMENT_REPORTING"
    runs = [
        _dump(src, "20240101T000000Z", 0, [
            ("D1", periodic, _report("31.12.2098", "1200.00"))]),
        _dump(src, "20240401T000000Z", 1, [
            ("D1", periodic, _report("31.12.2098", "1200.00")),
            ("D2", periodic, _report("30.06.2099", "1300.00")),
            ("M1", manual, _report("31.03.2099", "1250.00"))]),
        _dump(src, "20240701T000000Z", 2, [
            ("D1", periodic, _report("31.12.2098", "1201.00")),
            ("D3", periodic, b"%PDF-1.4 does not parse")]),
    ]
    incremental.check(
        loader.main,
        lambda db, bronze_dir: ["--silver-db", str(db),
                                "--bronze-dir", str(bronze_dir)],
        runs, tmp_path / "work")
    # The PDFs were read: three reports, two positions each.
    rows = incremental.silver_rows(tmp_path / "work" / "at-once.db")
    assert sum("report:" in r for r in rows["positions"]) == 6
