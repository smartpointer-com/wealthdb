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

import pytest

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
                "instrument": {"symbol": "VTI", "cusip": "999999999",
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
    assert pos[0]["instrument_key"] == "999999999"
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


# ============================================================
# Position cost columns (migration 0005)
# ============================================================
#
# average_cost is `averagePrice` as sent; unrealized_gain_loss is the
# open P/L of the side the position is held on. Invented figures only.

_COST_POSITIONS = {
    # Long equity. averageLongPrice is a different average and must not
    # reach the column.
    "999999999": {
        "instrument": {"symbol": "VTI", "cusip": "999999999",
                       "assetType": "EQUITY"},
        "longQuantity": 10, "shortQuantity": 0,
        "marketValue": 2500.00,
        "averagePrice": 200.00, "averageLongPrice": 600.00,
        "longOpenProfitLoss": 500.00, "shortOpenProfitLoss": 0,
    },
    # Short option: per-share price, P/L from the short side.
    "SPX   990119C09990000": {
        "instrument": {"symbol": "SPX   990119C09990000",
                       "assetType": "OPTION"},
        "longQuantity": 0, "shortQuantity": 2,
        "marketValue": -300.00,
        "averagePrice": 2.50,
        "longOpenProfitLoss": 0, "shortOpenProfitLoss": 200.00,
    },
    # Bond: averagePrice per 100 of par, kept as sent.
    "912796ZZ9": {
        "instrument": {"symbol": "912796ZZ9", "cusip": "912796ZZ9",
                       "assetType": "FIXED_INCOME"},
        "longQuantity": 10000,
        "marketValue": 9950.00,
        "averagePrice": 99.25,
        "longOpenProfitLoss": 25.00,
    },
    # Zero average cost: stored as 0, not NULL.
    "QQQ": {
        "instrument": {"symbol": "QQQ", "assetType": "EQUITY"},
        "longQuantity": 1,
        "marketValue": 100.00,
        "averagePrice": 0,
        "longOpenProfitLoss": 100.00,
    },
    # Neither field sent: both columns NULL.
    "SPY": {
        "instrument": {"symbol": "SPY", "assetType": "EQUITY"},
        "longQuantity": 1,
        "marketValue": 100.00,
    },
    # Short with only the long-side P/L sent: not stated, so NULL.
    "IWM": {
        "instrument": {"symbol": "IWM", "assetType": "EQUITY"},
        "shortQuantity": 1,
        "marketValue": -100.00,
        "averagePrice": 110.00,
        "longOpenProfitLoss": 0,
    },
}

_COST_EXPECTED = {
    "999999999": (200.00, 500.00),
    "SPX   990119C09990000": (2.50, 200.00),
    "912796ZZ9": (99.25, 25.00),
    "QQQ": (0, 100.00),
    "SPY": (None, None),
    "IWM": (110.00, None),
}


def _seed_cost_positions(root: Path) -> Path:
    dump = root / RUN_SLUG
    dump.mkdir(parents=True, exist_ok=True)
    (dump / "account_numbers.json").write_text(json.dumps([
        {"accountNumber": ACCT_PLAIN, "hashValue": ACCT_HASH},
    ]), encoding="utf-8")
    (dump / "accounts_positions.json").write_text(json.dumps([
        {"securitiesAccount": {
            "accountNumber": ACCT_PLAIN,
            "type": "MARGIN",
            "positions": list(_COST_POSITIONS.values()),
        }},
    ]), encoding="utf-8")
    return dump


def _cost_columns(conn) -> dict:
    return {
        r[0]: (r[1], r[2]) for r in conn.execute(
            "SELECT instrument_key, average_cost, unrealized_gain_loss "
            "FROM positions")
    }


def test_load_writes_position_cost_columns(tmp_path):
    conn = _fresh_db(tmp_path)
    loader.load_dump(conn, _seed_cost_positions(tmp_path / "bronze"))
    assert _cost_columns(conn) == _COST_EXPECTED
    # The payload keeps the raw object, averageLongPrice included.
    payload = json.loads(conn.execute(
        "SELECT payload FROM positions WHERE instrument_key = '999999999'"
    ).fetchone()[0])
    assert payload["averageLongPrice"] == 600.00


def test_migration_0005_backfills_rows_loaded_before_it(tmp_path):
    """A silver DB at schema 0004 holds positions with payload only. The
    migration fills both columns from payload by the loader's rule."""
    pre = tmp_path / "migrations-0004"
    pre.mkdir()
    for f in sorted(loader.MIGRATIONS_DIR.glob("000[1-4]_*.sql")):
        (pre / f.name).write_text(f.read_text(encoding="utf-8"),
                                  encoding="utf-8")
    conn = loader.open_db(tmp_path / "schwab-api.db")
    assert silver.apply_migrations(conn, pre) == 4
    with conn:
        conn.executemany(
            "INSERT INTO positions"
            "(snapshot_at, account_external_id, instrument_key, payload) "
            "VALUES (?, ?, ?, ?)",
            [(1, ACCT_HASH, key, loader.canonical_json(pos))
             for key, pos in _COST_POSITIONS.items()],
        )

    assert silver.apply_migrations(conn, loader.MIGRATIONS_DIR) == 5
    assert _cost_columns(conn) == _COST_EXPECTED


# ============================================================
# Bronze compression convergence
# ============================================================
#
# The hard invariant for zstd bronze compression: a `load` of a
# compressed bronze tree (.json.zst) must produce byte-identical silver
# to a `load` of the same tree uncompressed. schwab-api's loader stores
# no file hashes / sizes — read_json just resolves the on-disk variant
# and json.load()s the decompressed bytes — so convergence reduces to
# "the same parsed object comes back". Synthetic hashes / example
# tickers only; never real Schwab data.

# Bronze-derived silver tables. dump_runs (records the run_dir absolute
# path, which differs between two scratch trees) and schema_meta
# (migration timestamps) are deliberately excluded from the comparison.
_BRONZE_TABLES = (
    "accounts", "user_preference", "account_balances",
    "positions", "open_orders", "transactions", "instruments",
)


def _seed_full_bronze(root: Path, slug: str = RUN_SLUG) -> Path:
    """A complete synthetic dump exercising every load read-path:
    account_numbers, accounts_positions (positions + a balance),
    user_preference (matching account → nickname/preference_type + the
    per-request streamer noise the loader strips), a transactions file
    (a TRADE with a FIXED_INCOME transferItem so
    load_synthesized_instruments runs too), open_orders, and the
    optional instruments.json (so load_instruments runs)."""
    dump = root / slug
    dump.mkdir(parents=True, exist_ok=True)
    (dump / "account_numbers.json").write_text(json.dumps([
        {"accountNumber": ACCT_PLAIN, "hashValue": ACCT_HASH},
    ]), encoding="utf-8")
    (dump / "user_preference.json").write_text(json.dumps({
        "accounts": [{"accountNumber": ACCT_PLAIN, "type": "BROKERAGE",
                      "nickName": "Synthetic Brokerage"}],
        "streamerInfo": [{"schwabClientCorrelId": "per-request-noise"}],
    }), encoding="utf-8")
    (dump / "accounts_positions.json").write_text(json.dumps([
        {"securitiesAccount": {
            "accountNumber": ACCT_PLAIN,
            "type": "MARGIN",
            "positions": [{
                "instrument": {"symbol": "VTI", "cusip": "999999999",
                               "assetType": "EQUITY"},
                "longQuantity": 10,
                "marketValue": 2500.00,
            }],
            "currentBalances": {"liquidationValue": 2500.00},
        }},
    ]), encoding="utf-8")
    (dump / "transactions_000.json").write_text(json.dumps({
        "account_hash": ACCT_HASH,
        "window_start": "2024-01-01",
        "window_end": "2024-03-31",
        "transactions": [{
            "activityId": "990000000001",
            "time": "2024-02-01T12:00:00+0000",
            "accountNumber": ACCT_PLAIN,
            "type": "TRADE",
            "transferItems": [{
                "instrument": {
                    "symbol": "912796ZZ9",       # synthetic T-bill CUSIP
                    "cusip": "912796ZZ9",
                    "assetType": "FIXED_INCOME",
                    "maturityDate": "2024-06-15T00:00:00+0000",
                    "variableRate": 0,
                },
            }],
        }],
    }), encoding="utf-8")
    (dump / "open_orders.json").write_text(json.dumps({
        "window_start": "2024-01-01", "window_end": "2024-03-31",
        "orders": [{
            "accountNumber": ACCT_PLAIN,
            "orderId": 111222333,
            "status": "WORKING",
        }],
    }), encoding="utf-8")
    (dump / "instruments.json").write_text(json.dumps({
        "instruments": [{
            "symbol": "VTI", "cusip": "999999999", "assetType": "EQUITY",
            "description": "VANGUARD TOTAL STOCK MARKET ETF",
        }],
    }), encoding="utf-8")
    return dump


# The six compressible data artefacts; run.json is never in this set.
_DATA_ARTEFACTS = (
    "account_numbers.json", "user_preference.json",
    "accounts_positions.json", "transactions_000.json",
    "open_orders.json", "instruments.json",
)


def _dump_tables(db_path: Path) -> dict:
    """Every bronze-derived table as a set of rows (each table is
    PK-backed, so rows are unique and set comparison is exact +
    order-independent)."""
    c = sqlite3.connect(str(db_path))
    try:
        return {t: set(c.execute(f"SELECT * FROM {t}").fetchall())
                for t in _BRONZE_TABLES}
    finally:
        c.close()


def test_compressed_bronze_converges_to_identical_silver(tmp_path):
    from collectorkit import compress

    # 1) Load the plain tree.
    bronze_plain = tmp_path / "plain"
    _seed_full_bronze(bronze_plain)
    db_plain = tmp_path / "plain.db"
    assert loader.main(["--silver-db", str(db_plain),
                        "--bronze-dir", str(bronze_plain)]) == 0
    plain = _dump_tables(db_plain)

    # Sanity: the seed actually populated every table (a convergence of
    # two empty databases would be a vacuous pass).
    assert plain["positions"] and plain["transactions"]
    assert plain["accounts"] and plain["open_orders"]
    assert plain["account_balances"] and plain["user_preference"]
    assert len(plain["instruments"]) == 2   # VTI (API) + synthesized bond

    # 2) A fresh tree with every data artefact compressed IN PLACE — the
    #    form download now writes. This dump has no run.json, so only
    #    data artefacts exist to touch.
    bronze_zst = tmp_path / "zst"
    dump = _seed_full_bronze(bronze_zst)
    for name in _DATA_ARTEFACTS:
        compress.compress_file(dump / name)
        assert not (dump / name).exists()
        assert (dump / (name + ".zst")).is_file()

    db_zst = tmp_path / "zst.db"
    assert loader.main(["--silver-db", str(db_zst),
                        "--bronze-dir", str(bronze_zst)]) == 0
    zst = _dump_tables(db_zst)

    # Every bronze-derived table byte-identical between the two loads.
    assert zst == plain


def test_transactions_coexisting_plain_and_zst_ingests_once(tmp_path):
    """A recompress interrupted between verify and unlink leaves a plain
    transactions_NNN.json next to its .zst twin. transaction_files keys
    on the logical name and resolves one variant (plain wins), so the
    window is ingested exactly once — not doubled (which would PK-collide
    on activity_id) nor skipped."""
    from collectorkit import compress

    bronze = tmp_path / "bronze"
    dump = _seed_full_bronze(bronze)
    txn = dump / "transactions_000.json"
    compress.compress_file(txn, remove_original=False)   # keep both
    assert txn.exists()
    assert (dump / "transactions_000.json.zst").is_file()

    # Plain wins; the resolver yields exactly one on-disk path.
    assert loader.transaction_files(dump) == [txn]

    db = tmp_path / "silver.db"
    assert loader.main(["--silver-db", str(db),
                        "--bronze-dir", str(bronze)]) == 0
    c = sqlite3.connect(str(db))
    try:
        assert c.execute(
            "SELECT COUNT(*) FROM transactions").fetchone()[0] == 1
    finally:
        c.close()


def test_transaction_files_resolves_zst_only(tmp_path):
    """With only the .zst twin on disk, transaction_files resolves it and
    read_json (via the logical name) decompresses it transparently."""
    from collectorkit import compress

    dump = _seed_full_bronze(tmp_path / "bronze")
    txn = dump / "transactions_000.json"
    compress.compress_file(txn)                          # removes plain
    assert not txn.exists()

    assert loader.transaction_files(dump) == [
        dump / "transactions_000.json.zst"]
    # read_json takes the LOGICAL name and resolves the .zst variant.
    data = loader.read_json(txn)
    assert data["transactions"][0]["activityId"] == "990000000001"


def test_read_json_missing_artefact_raises(tmp_path):
    with pytest.raises(FileNotFoundError):
        loader.read_json(tmp_path / "nope.json")


def test_logical_bronze_path_strips_compression_suffix():
    assert loader._logical_bronze_path(Path(
        "d/accounts_positions.json.zst")) == Path("d/accounts_positions.json")
    assert loader._logical_bronze_path(
        Path("d/transactions_000.json.gz")) == Path("d/transactions_000.json")
    # Plain paths pass through unchanged.
    assert loader._logical_bronze_path(
        Path("d/run.json")) == Path("d/run.json")


def test_recompress_compresses_data_leaves_run_json(tmp_path):
    """The recompress sweep converts the six data artefacts to .json.zst
    inside a complete dump and leaves run.json byte-identical (it is
    excluded from the patterns), and silver reloads to the same rows."""
    from collectorkit import recompress as rc_engine
    import recompress as rc_mod  # the collector's thin wrapper

    bronze = tmp_path / "bronze"
    dump = _seed_full_bronze(bronze)
    (dump / "run.json").write_text(
        json.dumps({"status": "complete"}), encoding="utf-8")
    run_json_before = (dump / "run.json").read_bytes()

    # Load pre-sweep for the convergence comparison.
    db_before = tmp_path / "before.db"
    assert loader.main(["--silver-db", str(db_before),
                        "--bronze-dir", str(bronze)]) == 0
    before = _dump_tables(db_before)

    # Sweep with no age guard (the seeded dump is "fresh").
    assert rc_engine.run(rc_mod.CONFIG, bronze,
                         dry_run=False, min_age_hours=0) == 0

    for name in _DATA_ARTEFACTS:
        assert (dump / (name + ".zst")).is_file()
        assert not (dump / name).exists()
    # run.json untouched — byte-identical, never compressed.
    assert (dump / "run.json").read_bytes() == run_json_before
    assert not (dump / "run.json.zst").exists()

    # Convergence: a fresh load of the swept tree yields identical silver.
    db_after = tmp_path / "after.db"
    assert loader.main(["--silver-db", str(db_after),
                        "--bronze-dir", str(bronze)]) == 0
    assert _dump_tables(db_after) == before
