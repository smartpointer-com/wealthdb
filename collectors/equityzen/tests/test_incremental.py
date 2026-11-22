"""
The standing test of an incremental load: equityzen's fixture runs loaded
one at a time give the silver that one `load --force` of all of them
gives (collectorkit.incremental). Three small runs: an SPV whose shares
are partly tendered, then exited, and a fund with its capital-account
statements and a K-1 as real one-page PDFs. Synthetic data only.
"""
from __future__ import annotations

import json
import shutil
import sqlite3
import sys
from pathlib import Path

import pytest

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


def _k1_silver(tmp_path, dtype="K1") -> tuple[list[str], Path]:
    """One fund run holding a K-1 of document type `dtype`, loaded: the
    load's argv and its silver."""
    k1 = ({"id": "doc-k1", "documentType": dtype}, _K1)
    bronze_dir, db = tmp_path / "bronze", tmp_path / "equityzen.db"
    _run(bronze_dir, "20990301T000000Z", [_fund([k1])], [k1])
    argv = ["--bronze-dir", str(bronze_dir), "--silver-db", str(db)]
    assert load.main(argv) == 0
    return argv, db


def _rows(db: Path, sql: str) -> list[tuple]:
    conn = sqlite3.connect(db)
    try:
        return conn.execute(sql).fetchall()
    finally:
        conn.close()


def test_a_new_k1_reader_derives_the_stored_runs_again(tmp_path, monkeypatch):
    argv, db = _k1_silver(tmp_path)
    read = load.statements.parse_k1
    monkeypatch.setattr(load, "_PARSE_CACHE", {})
    monkeypatch.setattr(load, "K1_GENERATION", "next")
    monkeypatch.setattr(load.statements, "parse_k1", lambda text, boxes: {
        **read(text, boxes), "ending_capital": 1.0})
    assert load.main(argv) == 0
    assert _rows(db, "SELECT ending_capital FROM k1_documents") == [(1.0,)]
    assert _rows(db, "SELECT scope, generation, row_count "
                     "FROM document_derivations") == [("k1", "next", 1)]


def test_a_k1_is_derived_again_from_the_run_its_row_came_from(
        tmp_path, monkeypatch):
    argv, db = _k1_silver(tmp_path)
    k1 = ({"id": "doc-k1", "documentType": "K1"}, _K1)
    _run(tmp_path / "bronze", "20990401T000000Z", [_fund([k1])], [])
    assert load.main(argv) == 0
    read = load.statements.parse_k1
    monkeypatch.setattr(load, "_PARSE_CACHE", {})
    monkeypatch.setattr(load, "K1_GENERATION", "next")
    monkeypatch.setattr(load.statements, "parse_k1", lambda text, boxes: {
        **read(text, boxes), "ending_capital": 1.0})
    assert load.main(argv) == 0
    snapshot_at = bronze.parse_run_ts("20990301T000000Z")
    assert _rows(db, "SELECT ending_capital, snapshot_at "
                     "FROM k1_documents") == [(1.0, snapshot_at)]
    assert _rows(db, "SELECT scope, generation, row_count "
                     "FROM document_derivations") == [("k1", "next", 1)]


_RECORD_SQL = "SELECT * FROM document_derivations"


def test_a_k1_whose_copy_is_gone_keeps_its_row(tmp_path, monkeypatch, caplog):
    argv, db = _k1_silver(tmp_path)
    before = _rows(db, "SELECT * FROM k1_documents"), _rows(db, _RECORD_SQL)
    for blob in (tmp_path / "bronze").rglob("*.pdf"):
        blob.unlink()
    monkeypatch.setattr(load, "K1_GENERATION", "next")
    with caplog.at_level("WARNING"):
        assert load.main(argv) == 0
    assert any("its copy in 20990301T000000Z is gone" in m
               for m in caplog.messages)
    assert (_rows(db, "SELECT * FROM k1_documents"),
            _rows(db, _RECORD_SQL)) == before


def test_a_k1_whose_copy_holds_other_bytes_keeps_its_row(
        tmp_path, monkeypatch, caplog):
    argv, db = _k1_silver(tmp_path)
    before = _rows(db, "SELECT * FROM k1_documents"), _rows(db, _RECORD_SQL)
    for blob in (tmp_path / "bronze").rglob("*.pdf"):
        blob.write_bytes(pdf_bytes(_statement("June 30, 2098", "1,475.00")))
    monkeypatch.setattr(load, "K1_GENERATION", "next")
    with caplog.at_level("WARNING"):
        assert load.main(argv) == 0
    assert any("the copy in 20990301T000000Z holds other bytes" in m
               for m in caplog.messages)
    assert (_rows(db, "SELECT * FROM k1_documents"),
            _rows(db, _RECORD_SQL)) == before


def test_a_k1_that_reads_as_neither_loses_its_row_and_record(tmp_path,
                                                            monkeypatch):
    # A document typed K1 reads as a K-1 whatever its text says, so only an
    # untyped one that its text made a K-1 can read as neither.
    argv, db = _k1_silver(tmp_path, dtype=None)
    assert _rows(db, "SELECT document_external_id "
                     "FROM k1_documents") == [("doc-k1",)]
    neither = {"kind": None, "text": "",
               "cap": load.statements.parse_capital_account("")}
    monkeypatch.setattr(load, "K1_GENERATION", "next")
    monkeypatch.setattr(load, "_parse_pdf", lambda blob, chash: neither)
    assert load.main(argv) == 0
    assert _rows(db, "SELECT * FROM k1_documents") == []
    assert _rows(db, "SELECT * FROM document_derivations") == []
    assert _rows(db, "SELECT * FROM capital_account_statements") == []


def test_a_silver_from_before_the_records_derives_its_k1s_once(tmp_path):
    argv, db = _k1_silver(tmp_path)
    before = _rows(db, "SELECT * FROM k1_documents")
    conn = sqlite3.connect(db)
    conn.executescript("DROP TABLE document_derivations; "
                       "DELETE FROM schema_meta WHERE silver_schema_version = 3;")
    conn.close()
    assert load.main(argv) == 0
    assert _rows(db, "SELECT * FROM k1_documents") == before
    assert _rows(db, "SELECT scope, generation, row_count "
                     "FROM document_derivations") == [
        ("k1", load.K1_GENERATION, 1)]


def _reader_reads_the_k1_as_a_statement(monkeypatch) -> None:
    """A new K-1 generation whose classifier reads the fixture K-1 as a
    capital-account statement, with a year-end NAV."""
    classify, parse = (load.statements.classify,
                       load.statements.parse_capital_account)

    def is_k1(text):
        return "SCHEDULE K-1" in text.upper()

    monkeypatch.setattr(load, "_PARSE_CACHE", {})
    monkeypatch.setattr(load, "K1_GENERATION", "next")
    monkeypatch.setattr(load.statements, "classify", lambda text: (
        "capital_account" if is_k1(text) else classify(text)))
    monkeypatch.setattr(load.statements, "parse_capital_account", lambda text: {
        **parse(text), "period_end": "2098-12-31", "ending_nav": 1600.0,
    } if is_k1(text) else parse(text))


@pytest.mark.parametrize("layout", ["k1_run_is_latest", "listed_later",
                                    "new_run_after_the_change",
                                    "an_older_k1_sorts_later"])
def test_a_k1_that_reads_as_a_statement_now_gives_the_force_silver(
        tmp_path, monkeypatch, layout):
    q1 = ({"id": "doc-s1", "documentType": "CAPITAL_ACCOUNT_STATEMENT"},
          _statement("June 30, 2098", "1,475.00"))
    k1 = ({"id": "doc-k1", "documentType": "K1"}, _K1)
    k1s = [k1]
    if layout == "an_older_k1_sorts_later":
        # A second K-1 of the fund, other bytes, that only the earlier run
        # lists: its row is that run's, and its id sorts after doc-k1's.
        k1s.append(({"id": "doc-k2", "documentType": "K1"},
                    [[(x, y, s.replace("2098", "2097")) for x, y, s in _K1[0]]]))
    bronze_dir = tmp_path / "bronze"
    _run(bronze_dir, "20990101T000000Z", [_spv("ACTIVE", []), _fund([q1])],
         [q1])
    _run(bronze_dir, "20990301T000000Z", [_fund([q1, *k1s])], [q1, *k1s])
    if layout == "listed_later":
        _run(bronze_dir, "20990401T000000Z", [_fund([q1, k1])], [])
    if layout == "an_older_k1_sorts_later":
        _run(bronze_dir, "20990401T000000Z", [_fund([q1, k1])], [q1, k1])
    incremental_db, force_db = tmp_path / "a.db", tmp_path / "b.db"
    argv = ["--bronze-dir", str(bronze_dir), "--silver-db", str(incremental_db)]
    assert load.main(argv) == 0
    assert _rows(incremental_db, "SELECT document_external_id FROM "
                                 "k1_documents ORDER BY 1") == [
        (doc["id"],) for doc, _ in k1s]

    _reader_reads_the_k1_as_a_statement(monkeypatch)
    if layout == "new_run_after_the_change":
        shutil.copytree(bronze_dir / "20990301T000000Z",
                        bronze_dir / "20990501T000000Z")
    assert load.main(argv) == 0
    assert load.main(["--bronze-dir", str(bronze_dir), "--silver-db",
                      str(force_db), "--force"]) == 0

    assert _rows(force_db, "SELECT document_external_id FROM "
                           "capital_account_statements ORDER BY 1") == [
        (doc["id"],) for doc, _ in [*k1s, q1]]
    if layout != "listed_later":
        assert ("doc-k1",) in _rows(force_db, "SELECT event_external_id "
                                    "FROM positions WHERE event_type = "
                                    "'statement'")
    assert (incremental.silver_rows(incremental_db)
            == incremental.silver_rows(force_db))
