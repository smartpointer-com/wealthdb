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


def _seed_with_txns(root: Path, slug: str,
                    window_start: str, window_end: str,
                    txn_specs: list[tuple[str, str]]) -> Path:
    """Seed a synthetic dump with one transaction file.

    `txn_specs` is a list of (activityId, iso_time) — the helper fills
    in the rest with minimal valid JOURNAL data."""
    dump = root / slug
    dump.mkdir(parents=True, exist_ok=True)
    (dump / "account_numbers.json").write_text(json.dumps([
        {"accountNumber": ACCT_PLAIN, "hashValue": ACCT_HASH},
    ]), encoding="utf-8")
    (dump / "accounts_positions.json").write_text(json.dumps([
        {"securitiesAccount": {
            "accountNumber": ACCT_PLAIN,
            "type": "CASH",
            "currentBalances": {"liquidationValue": 0.0},
        }},
    ]), encoding="utf-8")
    (dump / "transactions_000.json").write_text(json.dumps({
        "account_hash": ACCT_HASH,
        "window_start": window_start,
        "window_end": window_end,
        "transactions": [
            {
                "activityId": aid,
                "time": t,
                "accountNumber": ACCT_PLAIN,
                "type": "JOURNAL",
                "subAccount": "CASH",
                "tradeDate": t,
                "settlementDate": t,
                "status": "VALID",
                "netAmount": 0,
                "description": "synthetic test journal",
                "transferItems": [],
            } for aid, t in txn_specs
        ],
    }), encoding="utf-8")
    return dump


def test_txn_outside_declared_window_does_not_pk_collide(tmp_path):
    """Reproduces the failure where Schwab returns a transaction whose
    `time` falls before the dump's declared window_start (e.g. a JOURNAL
    posted on day D but with an underlying event time D-2). The window
    DELETE in load_transactions would miss the prior dump's row, and a
    naive INSERT would PK-collide on activity_id."""
    bronze = tmp_path / "bronze"
    conn = _fresh_db(tmp_path)

    # Dump 1: wide window covering the txn's event time.
    dump1 = _seed_with_txns(
        bronze, "20260601T000000Z",
        window_start="2026-06-01", window_end="2026-06-07",
        txn_specs=[("990000000001", "2026-06-06T12:00:00+0000")],
    )
    stats1 = loader.load_dump(conn, dump1)
    assert stats1["transactions"] == 1

    # Dump 2: a narrower, later window. Schwab still returns the same
    # activity_id (posted in-window) but its `time` is 2026-06-06, before
    # the new window_start of 2026-06-08. Without the fix this raises
    # sqlite3.IntegrityError on the activity_id PK.
    dump2 = _seed_with_txns(
        bronze, "20260615T000000Z",
        window_start="2026-06-08", window_end="2026-06-15",
        txn_specs=[("990000000001", "2026-06-06T12:00:00+0000")],
    )
    stats2 = loader.load_dump(conn, dump2)
    assert stats2["transactions"] == 1

    # Exactly one row for that activity_id; the second load replaced it.
    rows = conn.execute(
        "SELECT activity_id, timestamp FROM transactions "
        "WHERE activity_id = '990000000001'"
    ).fetchall()
    assert len(rows) == 1
    assert rows[0]["activity_id"] == "990000000001"
