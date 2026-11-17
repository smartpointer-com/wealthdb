"""Bronze→silver tests for the equityzen collector's load.py.

Seeds a minimal synthetic EquityZen bronze dump (the buyer-deals
GraphQL envelope in investments.json) and runs the real loader,
asserting the projected offerings + event-sourced positions.
Synthetic deal/company ids + amounts only.
"""
from __future__ import annotations

import dataclasses
import json
import sqlite3
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import load as loader  # noqa: E402
from collectorkit import bronze, extract, pdftotext  # noqa: E402
from collectorkit import loader as frame  # noqa: E402
from collectorkit import silver  # noqa: E402
from pdfs import pdf_bytes, text_page  # noqa: E402

RUN_SLUG = "20240301T000000Z"


def _seed_bronze(root: Path, manifest: dict | None = None) -> Path:
    """A synthetic dump; `manifest` is its run.json, or a finished walk's
    when omitted."""
    run_dir = root / RUN_SLUG
    run_dir.mkdir(parents=True, exist_ok=True)
    (run_dir / "run.json").write_text(
        json.dumps(manifest if manifest is not None else {"status": "complete"}),
        encoding="utf-8")
    investments = {
        "PORTFOLIO": {
            "data": {"buyer": {"buyerDeals": {"edges": [
                {"node": {
                    "deal": {
                        "id": "D1",
                        "company": {
                            "id": "C1",
                            "name": "Acme Inc",
                            "assetClass": "ASSET_COMPANY",
                            "tickerSymbol": "ACME",
                        },
                        "flavor": "DIRECT",
                        "dateStart": "2024-01-01",
                        "sharePrice": 10.0,
                    },
                    "primaryTransaction": {
                        "id": "T1",
                        "transactionDate": "2024-01-15",
                        "sharesPostSplit": 100,
                        "pricePostSplit": 12.0,
                    },
                    "investmentSize": 1200,
                    "dealStage": "ACTIVE",
                }},
            ]}}},
        },
    }
    (run_dir / "investments.json").write_text(json.dumps(investments),
                                              encoding="utf-8")
    return run_dir


def _fresh_db(tmp_path: Path) -> sqlite3.Connection:
    conn = silver.open_db(tmp_path / "equityzen.db")
    silver.apply_migrations(conn, loader.MIGRATIONS_DIR)
    return conn


def _dump_state(db_path: Path) -> dict:
    """Full content snapshot of a silver DB: every loaded table mapped to its
    rows, each table's rows sorted so the comparison is independent of
    insertion order.

    Introspects sqlite_master for the table list, skipping SQLite internals
    and `schema_meta` — the latter's `applied_at` records the wall-clock
    second the migration machinery ran (not loaded data), so it legitimately
    differs between two loads of the same bronze and would mask a true
    equivalence.
    """
    conn = sqlite3.connect(str(db_path))
    try:
        names = [r[0] for r in conn.execute(
            "SELECT name FROM sqlite_master WHERE type='table' "
            "AND name NOT LIKE 'sqlite_%' AND name != 'schema_meta'"
        ).fetchall()]
        # repr() sort keys impose a total order over rows carrying NULLs /
        # mixed column types, which are otherwise unorderable in Python 3.
        return {name: sorted((tuple(r) for r in
                              conn.execute(f"SELECT * FROM {name}").fetchall()),
                             key=repr)
                for name in sorted(names)}
    finally:
        conn.close()


def test_load_offering_and_position(tmp_path):
    run_dir = _seed_bronze(tmp_path / "bronze")
    conn = _fresh_db(tmp_path)
    # One run through the load frame, as `load` loads each pending run.
    spec = dataclasses.replace(loader.SPEC, pending=lambda *_a, **_k: [run_dir])
    offerings, positions, *_ = frame.load_runs(spec, conn, frame.Load(
        args=None, silver_db=None, bronze_dir=run_dir.parent, log=loader.log,
        version=silver.current_schema_version(conn)))
    assert offerings == 1
    assert positions >= 1

    # Offering: deal D1, single-company SPV, ticker carried through.
    off = conn.execute(
        "SELECT deal_external_id, kind, company_name, ticker_symbol, "
        "purchase_price, shares_original FROM offerings").fetchall()
    assert len(off) == 1
    assert off[0]["deal_external_id"] == "D1"
    assert off[0]["kind"] == "spv"             # ASSET_COMPANY → spv
    assert off[0]["company_name"] == "Acme Inc"
    assert off[0]["ticker_symbol"] == "ACME"
    assert float(off[0]["purchase_price"]) == 12.0
    assert float(off[0]["shares_original"]) == 100.0

    # Event-sourced position: the investment event for deal D1.
    pos = conn.execute(
        "SELECT deal_external_id, event_type FROM positions "
        "WHERE deal_external_id = 'D1'").fetchall()
    assert len(pos) >= 1
    assert any(r["event_type"] == "investment" for r in pos)

    assert conn.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0] == 1


def _load_all(tmp_path: Path) -> sqlite3.Connection:
    """`load` over tmp_path/bronze through the CLI entry point, which picks
    the pending runs; the silver DB, opened."""
    db = tmp_path / "equityzen.db"
    assert loader.main(["--bronze-dir", str(tmp_path / "bronze"),
                        "--silver-db", str(db)]) == 0
    return silver.open_db(db)


def test_skips_already_loaded(tmp_path):
    _seed_bronze(tmp_path / "bronze")
    _load_all(tmp_path).close()
    # A second load of the same run is a no-op (dump_runs guard).
    conn = _load_all(tmp_path)
    assert conn.execute("SELECT COUNT(*) FROM offerings").fetchone()[0] == 1
    assert conn.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0] == 1


def test_skips_a_dump_whose_walk_never_finished(tmp_path):
    _seed_bronze(tmp_path / "bronze", {"status": "in-progress"})
    conn = _load_all(tmp_path)
    assert conn.execute("SELECT COUNT(*) FROM offerings").fetchone()[0] == 0
    assert conn.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0] == 0


def test_skips_a_dump_without_run_json(tmp_path):
    run_dir = _seed_bronze(tmp_path / "bronze")
    (run_dir / "run.json").unlink()
    conn = _load_all(tmp_path)
    assert conn.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0] == 0


def test_a_slug_that_names_no_real_instant_stops_no_load(tmp_path):
    # Month 13 matches the run-dir pattern but is no date: it is skipped,
    # and the runs after it still load.
    bad = _seed_bronze(tmp_path / "bronze")
    bad.rename(bad.with_name("20231301T000000Z"))
    _seed_bronze(tmp_path / "bronze")
    conn = _load_all(tmp_path)
    assert conn.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0] == 1


def test_loads_a_statusless_manifest(tmp_path):
    # A manifest without a status field was written at the end of a
    # finished walk, so it is complete.
    _seed_bronze(tmp_path / "bronze", {"source": "equityzen"})
    conn = _load_all(tmp_path)
    assert conn.execute("SELECT COUNT(*) FROM offerings").fetchone()[0] == 1


def test_force_rebuild_equals_incremental(tmp_path):
    """For UNCHANGED bronze, `load --force` (rebuild the silver from all
    bronze) reproduces the plain incremental load exactly. Drives the real
    CLI entry point (`load.main`), where the frame's rebuild happens;
    going through `load_run` alone would bypass it.
    """
    bronze_root = tmp_path / "bronze"
    _seed_bronze(bronze_root)
    db = tmp_path / "equityzen.db"

    # Plain incremental load into a fresh DB, then snapshot its full state.
    assert loader.main(["--bronze-dir", str(bronze_root),
                        "--silver-db", str(db)]) == 0
    incremental = _dump_state(db)
    # Guard against a vacuous pass: the load must have populated the DB.
    assert incremental["offerings"], "incremental load produced no offerings"
    assert incremental["positions"], "incremental load produced no positions"
    assert incremental["dump_runs"], "incremental load recorded no dump run"

    # Force-rebuild over the SAME bronze (re-ingest every run).
    assert loader.main(["--bronze-dir", str(bronze_root),
                        "--silver-db", str(db), "--force"]) == 0

    assert _dump_state(db) == incremental


# ---- document extraction -------------------------------------------------

_STATEMENT = [text_page(
    "STATEMENT OF CAPITAL ACCOUNT\n"
    "For the Quarter Ended June 30, 2098\n"
    "Beginning Net Capital Account Balance     1,000.00\n"
    "Capital Contributions     500.00\n"
    "Profit (Loss)     (25.00)\n"
    "Net Ending Capital Account Balance     1,475.00\n")]

# A K-1 whose ending capital prints off its caption's line, so the text
# leaves it empty and the word boxes are read.
_K1 = [[(36, 66, "Schedule K-1"),
        (231, 90, "For calendar year 2098"),
        (337, 106, "1 Ordinary business income (loss)"),
        (454, 106, "14 Self-employment earnings (loss)"),
        (112, 622, "Partner's Capital Account Analysis"),
        (54, 634, "Beginning capital account $"), (266, 634, "1,000."),
        (54, 694, "Ending capital account $"), (266, 690, "1,475.")]]


def _fund_run(root: Path, name: str) -> Path:
    """A run holding a fund with one capital-account statement and one
    K-1, as real one-page PDFs."""
    docs = [({"id": "doc-s1", "documentType": "CAPITAL_ACCOUNT_STATEMENT"},
             _STATEMENT),
            ({"id": "doc-k1", "documentType": "K1"}, _K1)]
    node = {"deal": {"id": "D2", "company": {
                "id": "C2", "name": "Example Fund",
                "assetClass": "ASSET_MULTI_COMPANY_FUND"},
                     "dateStart": "2098-02-01"},
            "primaryTransaction": {"id": "T2", "transactionDate": "2098-02-15",
                                   "sharesPostSplit": 1000,
                                   "pricePostSplit": 1.0},
            "investmentSize": 1000, "dealStage": "ACTIVE",
            "documents": [doc for doc, _ in docs]}
    run = root / name
    run.mkdir(parents=True)
    (run / "run.json").write_text(json.dumps({"status": "complete"}))
    (run / "investments.json").write_text(json.dumps({"PORTFOLIO": {"data": {
        "buyer": {"buyerDeals": {"edges": [{"node": node}]}}}}}))
    for doc, pages in docs:
        blob = (run / "documents" / bronze.short_token("D2")
                / (bronze.short_token(doc["id"]) + ".pdf"))
        blob.parent.mkdir(parents=True, exist_ok=True)
        blob.write_bytes(pdf_bytes(pages))
    return run


def test_the_cached_text_is_the_pdftotext_layout_text(tmp_path, cache_dir):
    # What the parsers read through the cache, on a miss and on a hit, is
    # byte for byte the text `pdftotext -layout` prints.
    run = _fund_run(tmp_path, RUN_SLUG)
    for pdf in sorted((run / "documents").rglob("*.pdf")):
        sha = bronze.sha256_file(pdf)[0]
        direct = pdftotext.layout_text(pdf, timeout=120)
        assert direct.strip()
        miss = loader._extracted(loader._LAYOUT, pdf, sha)
        assert list(extract.default_dir("equityzen").glob(f"{sha}.*"))
        hit = loader._extracted(loader._LAYOUT, pdf, sha)
        assert miss.text() == hit.text() == direct
        boxes = loader._extracted(loader._BBOX, pdf, sha)
        assert boxes == loader._extracted(loader._BBOX, pdf, sha) == (
            extract.pdftotext_bbox(pdf))


def test_each_document_is_extracted_once(tmp_path, monkeypatch):
    # A later run holds the same bytes, and a later load reads them again:
    # pdftotext runs once per document and engine, in the first load.
    calls = []
    layout_text, bbox_xhtml = pdftotext.layout_text, extract._bbox_xhtml

    def counted(kind, real):
        def run(pdf, timeout=None):
            calls.append((kind, Path(pdf).name))
            return real(pdf, timeout=timeout)
        return run

    monkeypatch.setattr(pdftotext, "layout_text", counted("layout", layout_text))
    monkeypatch.setattr(extract, "_bbox_xhtml", counted("bbox", bbox_xhtml))
    bronze_root, db = tmp_path / "bronze", tmp_path / "equityzen.db"
    argv = ["--bronze-dir", str(bronze_root), "--silver-db", str(db)]
    for name in ("20990101T000000Z", "20990201T000000Z"):
        monkeypatch.setattr(loader, "_PARSE_CACHE", {})   # a new process
        _fund_run(bronze_root, name)
        assert loader.main(argv) == 0
    k1 = bronze.short_token("doc-k1") + ".pdf"
    assert sorted(calls) == sorted([
        ("layout", bronze.short_token("doc-s1") + ".pdf"),
        ("layout", k1), ("bbox", k1)])
    conn = silver.open_db(db)
    assert [tuple(r) for r in conn.execute(
        "SELECT beginning_capital, ending_capital FROM k1_documents")] == [
            (1000.0, 1475.0)]
    conn.close()


def test_the_load_sweeps_extractions_no_extractor_reads(tmp_path, cache_dir):
    # An entry under a key neither extractor has (an older engine or code)
    # goes; the current keys' entries and other files stay.
    directory = extract.default_dir("equityzen")
    directory.mkdir(parents=True)
    keep = {extract.extraction_key(e) for e in (loader._LAYOUT, loader._BBOX)}
    stale = directory / f"{'a' * 64}.{'0' * 32}.json.gz"
    kept = [directory / f"{'a' * 64}.{key}.json.gz" for key in keep]
    other = directory / "notes.txt"
    for path in (stale, other, *kept):
        path.write_bytes(b"")
    (tmp_path / "bronze").mkdir()
    _load_all(tmp_path).close()
    assert sorted(directory.iterdir()) == sorted([other, *kept])
