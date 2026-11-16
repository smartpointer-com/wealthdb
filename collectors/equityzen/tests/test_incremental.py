"""
The standing test of an incremental load: equityzen's fixture runs loaded
one at a time give the silver that one `load --force` of all of them
gives (collectorkit.incremental). Three small runs: an SPV whose shares
are partly tendered, then exited, and a fund with its capital-account
statements and a K-1 as real one-page PDFs. Synthetic data only.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402
from collectorkit import bronze, incremental  # noqa: E402
from pdfs import pdf_bytes, text_page  # noqa: E402


def _statement(quarter: str, ending: str) -> list:
    return [text_page(
        "STATEMENT OF CAPITAL ACCOUNT\n"
        f"For the Quarter Ended {quarter}\n"
        "Beginning Net Capital Account Balance     1,000.00\n"
        "Capital Contributions     500.00\n"
        "Profit (Loss)     (25.00)\n"
        f"Net Ending Capital Account Balance     {ending}\n")]


_K1 = [[(36, 66, "Schedule K-1"),
        (231, 90, "For calendar year 2098"),
        (337, 106, "1 Ordinary business income (loss)"),
        (454, 106, "14 Self-employment earnings (loss)"),
        (112, 622, "Partner's Capital Account Analysis"),
        (54, 634, "Beginning capital account $"), (266, 634, "1,000."),
        (54, 694, "Ending capital account $"), (266, 694, "1,475.")]]


def _spv(stage: str, tenders: list[dict]) -> dict:
    return {"deal": {"id": "D1", "company": {
                "id": "C1", "name": "Example Co", "assetClass": "ASSET_COMPANY"},
                     "dateStart": "2098-01-01", "sharePrice": 10.0},
            "primaryTransaction": {"id": "T1", "transactionDate": "2098-01-15",
                                   "sharesPostSplit": 100, "pricePostSplit": 12.0,
                                   "distributedTransactions": tenders},
            "investmentSize": 1200, "dealStage": stage,
            "dateExit": "2099-06-30" if stage == "EXITED" else None}


def _fund(docs: list[tuple[dict, list]]) -> dict:
    return {"deal": {"id": "D2", "company": {
                "id": "C2", "name": "Example Fund", "assetClass": "ASSET_MULTI_COMPANY_FUND"},
                     "dateStart": "2098-02-01"},
            "primaryTransaction": {"id": "T2", "transactionDate": "2098-02-15",
                                   "sharesPostSplit": 1000, "pricePostSplit": 1.0},
            "investmentSize": 1000, "dealStage": "ACTIVE",
            "documents": [doc for doc, _ in docs]}


def _run(root: Path, name: str, nodes: list[dict],
         docs: list[tuple[dict, list]]) -> Path:
    run = root / name
    run.mkdir(parents=True)
    (run / "run.json").write_text(json.dumps({"status": "complete"}))
    (run / "investments.json").write_text(json.dumps({"PORTFOLIO": {"data": {
        "buyer": {"buyerDeals": {"edges": [{"node": n} for n in nodes]}}}}}))
    for doc, pages in docs:
        blob = (run / "documents" / bronze.short_token("D2")
                / (bronze.short_token(doc["id"]) + ".pdf"))
        blob.parent.mkdir(parents=True, exist_ok=True)
        blob.write_bytes(pdf_bytes(pages))
    return run


def test_runs_loaded_one_at_a_time_build_the_silver_of_a_force_load(tmp_path):
    q1 = ({"id": "doc-s1", "documentType": "CAPITAL_ACCOUNT_STATEMENT"},
          _statement("June 30, 2098", "1,475.00"))
    q2 = ({"id": "doc-s2", "documentType": "CAPITAL_ACCOUNT_STATEMENT"},
          _statement("September 30, 2098", "1,520.00"))
    k1 = ({"id": "doc-k1", "documentType": "K1"}, _K1)
    tender = {"id": "T9", "transactionDate": "2098-09-01", "sharesPostSplit": 40,
              "pricePostSplit": 15.0, "value": 600.0}
    root = tmp_path / "runs"
    runs = [
        _run(root, "20990101T000000Z", [_spv("ACTIVE", []), _fund([q1])], [q1]),
        _run(root, "20990301T000000Z",
             [_spv("ACTIVE", [tender]), _fund([q1, q2, k1])], [q1, q2, k1]),
        _run(root, "20990701T000000Z", [_spv("EXITED", [tender])], []),
    ]
    incremental.check(
        load.main,
        lambda db, bronze_dir: ["--bronze-dir", str(bronze_dir),
                                "--silver-db", str(db)],
        runs, tmp_path / "work")
