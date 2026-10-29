"""Bronze→silver tests for the equityzen collector's load.py.

Seeds a minimal synthetic EquityZen bronze dump (the buyer-deals
GraphQL envelope in investments.json) and runs the real loader,
asserting the projected offerings + event-sourced positions.
Synthetic deal/company ids + amounts only.
"""
from __future__ import annotations

import json
import sqlite3
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import load as loader  # noqa: E402
from collectorkit import silver  # noqa: E402

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
    conn = loader.open_db(tmp_path / "equityzen.db")
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
    stats = loader.load_run(conn, run_dir)
    assert stats["offerings"] == 1
    assert stats["positions"] >= 1

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
    return loader.open_db(db)


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
    """For UNCHANGED bronze, `load --force` — delete the silver DB, then
    rebuild it from all bronze — reproduces the plain incremental load
    exactly. Drives the real CLI entry point (`load.main`) because the
    --force reset happens there, before the DB is opened; going through
    `load_run` alone would bypass it.
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

    # Force-rebuild over the SAME bronze (reset + re-ingest every run).
    assert loader.main(["--bronze-dir", str(bronze_root),
                        "--silver-db", str(db), "--force"]) == 0

    assert _dump_state(db) == incremental
