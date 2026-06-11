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


def _seed_bronze(root: Path) -> Path:
    run_dir = root / RUN_SLUG
    run_dir.mkdir(parents=True, exist_ok=True)
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


def test_load_offering_and_position(tmp_path):
    run_dir = _seed_bronze(tmp_path / "bronze")
    conn = _fresh_db(tmp_path)
    stats = loader.load_run(conn, run_dir, force=False)
    assert stats["skipped"] is False
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


def test_skips_already_loaded(tmp_path):
    run_dir = _seed_bronze(tmp_path / "bronze")
    conn = _fresh_db(tmp_path)
    loader.load_run(conn, run_dir, force=False)
    # Second load without --force is a no-op (dump_runs guard).
    again = loader.load_run(conn, run_dir, force=False)
    assert again["skipped"] is True
    assert conn.execute("SELECT COUNT(*) FROM offerings").fetchone()[0] == 1
