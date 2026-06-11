"""Bronze→silver tests for the schwab-api collector's load.py.

Seeds a minimal synthetic Schwab brokerage-API dump (account_numbers
+ accounts_positions) and runs the real loader, asserting the
projected silver rows. Synthetic account hashes / symbols only.
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

RUN_SLUG = "20240401T000000Z"
ACCT_PLAIN = "12345678"
ACCT_HASH = "HASH00000000000000000001"


def _seed_bronze(root: Path) -> Path:
    dump = root / RUN_SLUG
    dump.mkdir(parents=True, exist_ok=True)
    (dump / "account_numbers.json").write_text(json.dumps([
        {"accountNumber": ACCT_PLAIN, "hashValue": ACCT_HASH},
    ]), encoding="utf-8")
    (dump / "accounts_positions.json").write_text(json.dumps([
        {"securitiesAccount": {
            "accountNumber": ACCT_PLAIN,
            "type": "MARGIN",
            "positions": [{
                "instrument": {"symbol": "VTI", "cusip": "922908769",
                               "assetType": "EQUITY"},
                "longQuantity": 10,
                "marketValue": 2500.00,
            }],
            "currentBalances": {"liquidationValue": 2500.00},
        }},
    ]), encoding="utf-8")
    return dump


def _fresh_db(tmp_path: Path) -> sqlite3.Connection:
    conn = loader.open_db(tmp_path / "schwab-api.db")
    conn.row_factory = sqlite3.Row  # column-name access in assertions
    silver.apply_migrations(conn, loader.MIGRATIONS_DIR)
    return conn


def test_load_accounts_and_positions(tmp_path):
    dump = _seed_bronze(tmp_path / "bronze")
    conn = _fresh_db(tmp_path)
    stats = loader.load_dump(conn, dump)
    assert stats["skipped"] is False
    assert stats["accounts"] == 1
    assert stats["positions"] == 1

    # Account keyed by the hash (plaintext number never stored as the key).
    acct = conn.execute(
        "SELECT account_external_id FROM accounts").fetchall()
    assert len(acct) == 1
    assert acct[0]["account_external_id"] == ACCT_HASH

    # Position: keyed by cusip (preferred over symbol), account = hash,
    # full position JSON in payload.
    pos = conn.execute(
        "SELECT account_external_id, instrument_key, payload "
        "FROM positions").fetchall()
    assert len(pos) == 1
    assert pos[0]["account_external_id"] == ACCT_HASH
    assert pos[0]["instrument_key"] == "922908769"
    payload = json.loads(pos[0]["payload"])
    assert payload["marketValue"] == 2500.0

    assert conn.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0] == 1


def test_skips_already_loaded(tmp_path):
    dump = _seed_bronze(tmp_path / "bronze")
    conn = _fresh_db(tmp_path)
    loader.load_dump(conn, dump)
    again = loader.load_dump(conn, dump)
    assert again["skipped"] is True
    assert conn.execute("SELECT COUNT(*) FROM positions").fetchone()[0] == 1
