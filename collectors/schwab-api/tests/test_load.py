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
from collectorkit import incremental, silver  # noqa: E402

RUN_SLUG = "20240401T000000Z"
ACCT_PLAIN = "12345678"
ACCT_HASH = "HASH00000000000000000001"


def _mark_complete(dump: Path) -> Path:
    """Write the terminal manifest a finished download leaves."""
    (dump / "run.json").write_text(json.dumps({"status": "complete"}),
                                   encoding="utf-8")
    return dump


def _seed_bronze(root: Path, slug: str = RUN_SLUG) -> Path:
    dump = root / slug
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
    return _mark_complete(dump)


def _load(db: Path, bronze: Path, *extra: str) -> int:
    return loader.main(["--silver-db", str(db), "--bronze-dir", str(bronze),
                        *extra])


def _open(db: Path) -> sqlite3.Connection:
    conn = sqlite3.connect(str(db))
    conn.row_factory = sqlite3.Row  # column-name access in assertions
    return conn


def _count(conn: sqlite3.Connection, table: str) -> int:
    return conn.execute(f"SELECT COUNT(*) FROM {table}").fetchone()[0]


def test_load_accounts_and_positions(tmp_path, caplog):
    _seed_bronze(tmp_path / "bronze")
    db = tmp_path / "schwab-api.db"
    with caplog.at_level("INFO", logger="schwab-api.load"):
        assert _load(db, tmp_path / "bronze") == 0
    assert (f"loaded run {RUN_SLUG}: 1 accounts, 0 user preferences, "
            "1 positions, 1 balances, 0 transactions, 0 open orders, "
            "0 instruments") in caplog.text
    conn = _open(db)

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


def test_skips_already_loaded(tmp_path, caplog):
    _seed_bronze(tmp_path / "bronze")
    db = tmp_path / "schwab-api.db"
    assert _load(db, tmp_path / "bronze") == 0
    with caplog.at_level("INFO", logger="schwab-api.load"):
        assert _load(db, tmp_path / "bronze") == 0
    assert f"{RUN_SLUG}: skipped (already loaded)" in caplog.text
    assert _count(_open(db), "positions") == 1


@pytest.mark.parametrize("status", ["in-progress", "dry-run"])
def test_skips_non_complete_dump(tmp_path, status):
    """A crashed download leaves status="in-progress" over partial
    artefacts; the dump stays pending instead of loading half a snapshot."""
    dump = _seed_bronze(tmp_path / "bronze")
    (dump / "run.json").write_text(json.dumps({"status": status}),
                                   encoding="utf-8")
    db = tmp_path / "schwab-api.db"
    assert _load(db, tmp_path / "bronze") == 0
    conn = _open(db)
    assert _count(conn, "accounts") == 0
    assert _count(conn, "dump_runs") == 0

    # Once the download finishes, the same dump loads.
    _mark_complete(dump)
    assert _load(db, tmp_path / "bronze") == 0
    assert _count(conn, "dump_runs") == 1


def test_a_dir_with_no_run_json_is_neither_loaded_nor_recorded(tmp_path):
    """A `download --debug` that dies at the token refresh can leave a run
    dir holding only its HTTP trace. prune reclaims it, so load skips it
    and goes on to the other dumps."""
    bronze = tmp_path / "bronze"
    _seed_bronze(bronze)
    shell = bronze / "20240402T000000Z"
    (shell / "screenshots").mkdir(parents=True)
    (shell / "screenshots" / "http-trace.jsonl").write_text("{}\n")

    db = tmp_path / "silver.db"
    assert loader.main(["--silver-db", str(db),
                        "--bronze-dir", str(bronze)]) == 0
    c = sqlite3.connect(str(db))
    try:
        assert c.execute("SELECT snapshot_at FROM dump_runs").fetchall() == [
            (loader.bronze.parse_run_ts(RUN_SLUG),)]
    finally:
        c.close()


def test_a_legacy_dump_without_run_json_still_loads(tmp_path):
    """A dump that holds the terminal open_orders.json but no manifest is
    complete to prune, so load reads it."""
    dump = _seed_bronze(tmp_path / "bronze")
    (dump / "run.json").unlink()
    (dump / "open_orders.json").write_text(json.dumps({"orders": []}),
                                           encoding="utf-8")
    db = tmp_path / "schwab-api.db"
    assert _load(db, tmp_path / "bronze") == 0
    assert _count(_open(db), "positions") == 1


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
    return _mark_complete(dump)


def test_txn_outside_declared_window_does_not_pk_collide(tmp_path):
    """Reproduces the failure where Schwab returns a transaction whose
    `time` falls before the dump's declared window_start (e.g. a JOURNAL
    posted on day D but with an underlying event time D-2). The window
    DELETE in load_transactions would miss the prior dump's row, and a
    naive INSERT would PK-collide on activity_id."""
    bronze = tmp_path / "bronze"
    db = tmp_path / "schwab-api.db"

    # Dump 1: wide window covering the txn's event time.
    _seed_with_txns(
        bronze, "20260601T000000Z",
        window_start="2026-06-01", window_end="2026-06-07",
        txn_specs=[("990000000001", "2026-06-06T12:00:00+0000")],
    )
    assert _load(db, bronze) == 0

    # Dump 2: a narrower, later window. Schwab still returns the same
    # activity_id (posted in-window) but its `time` is 2026-06-06, before
    # the new window_start of 2026-06-08. Without the fix this raises
    # sqlite3.IntegrityError on the activity_id PK.
    _seed_with_txns(
        bronze, "20260615T000000Z",
        window_start="2026-06-08", window_end="2026-06-15",
        txn_specs=[("990000000001", "2026-06-06T12:00:00+0000")],
    )
    assert _load(db, bronze) == 0

    # Exactly one row for that activity_id; the second load replaced it.
    rows = _open(db).execute(
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
    return _mark_complete(dump)


def _cost_columns(conn) -> dict:
    return {
        r[0]: (r[1], r[2]) for r in conn.execute(
            "SELECT instrument_key, average_cost, unrealized_gain_loss "
            "FROM positions")
    }


def test_load_writes_position_cost_columns(tmp_path):
    _seed_cost_positions(tmp_path / "bronze")
    db = tmp_path / "schwab-api.db"
    assert _load(db, tmp_path / "bronze") == 0
    conn = _open(db)
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

    assert silver.apply_migrations(conn, loader.MIGRATIONS_DIR) == 6
    assert _cost_columns(conn) == _COST_EXPECTED


# ============================================================
# Promoted position fields and transaction legs (migration 0006)
# ============================================================
#
# Codes verbatim, numbers as decimal text with the payload's digits,
# the maturity as the day it names. Invented figures only.

def _numbers_position(symbol: str, value) -> dict:
    return {"instrument": {"symbol": symbol, "assetType": "EQUITY"},
            "longQuantity": value, "marketValue": value}


# A number the payload states, and the text the column holds for it.
_NUMBERS = {
    "INT": (10, "10"),
    "FLOAT": (2500.0, "2500.0"),
    "LONGFLOAT": (0.30000000000000004, "0.30000000000000004"),
    "SMALL": (1e-05, "0.00001"),
    "SMALLER": (-2.5e-07, "-0.00000025"),
    "LARGE": (1.2345e+16, "12345000000000000"),
    "NEGZERO": (-0.0, "-0.0"),
    "BIGINT": (12345678901234567890, "12345678901234567890"),
    "TEXT": ("10", None),
    "BOOL": (True, None),
    "NULL": (None, None),
}

_FIELD_POSITIONS = [
    {"instrument": {"symbol": "912796ZZ9", "cusip": "912796ZZ9",
                    "assetType": "FIXED_INCOME", "description": "US T-BILL",
                    "maturityDate": "2099-09-09T05:00:00.000+00:00",
                    "variableRate": 4.125},
     "longQuantity": 10000, "shortQuantity": 0, "marketValue": 9950.5},
    {"instrument": {"symbol": "VTI", "cusip": "", "type": "EXCHANGE_TRADED_FUND",
                    "assetType": "COLLECTIVE_INVESTMENT",
                    "maturityDate": "2099-02-30", "variableRate": 0}},
    {"instrument": {"symbol": "BADDAY", "maturityDate": "2099-9-09T00:00:00"}},
    {"instrument": {"symbol": "NOINST", "description": 5}, "shortQuantity": 2},
    *(_numbers_position(sym, v) for sym, (v, _) in _NUMBERS.items()),
]

_FIELD_COLUMNS = ("instrument_key, asset_type, instrument_type, cusip, symbol, "
                  "description, maturity_date, variable_rate, long_quantity, "
                  "short_quantity, market_value")

_FIELD_EXPECTED = {
    ("912796ZZ9", "FIXED_INCOME", None, "912796ZZ9", "912796ZZ9", "US T-BILL",
     4092595200, "4.125", "10000", "0", "9950.5"),
    ("VTI", "COLLECTIVE_INVESTMENT", "EXCHANGE_TRADED_FUND", "", "VTI", None,
     None, "0", None, None, None),
    ("BADDAY", None, None, None, "BADDAY", None, None, None, None, None, None),
    ("NOINST", None, None, None, "NOINST", None, None, None, None, "2", None),
    *((sym, "EQUITY", None, None, sym, None, None, None, text, None, text)
      for sym, (_, text) in _NUMBERS.items()),
}

_LEG_TXN = {
    "activityId": "990000000001", "time": "2024-02-01T12:00:00+0000",
    "type": "TRADE", "netAmount": -1505.0,
    "transferItems": [
        {"instrument": {"assetType": "EQUITY", "cusip": "999999999",
                        "symbol": "VTI"},
         "amount": 10, "cost": -1500.0, "price": 150.0},
        {"instrument": {"assetType": "CURRENCY", "symbol": "CURRENCY_USD"},
         "amount": -1505.0, "cost": -1.5e-05},
        {"instrument": None, "amount": "3", "price": None},
    ],
}

_LEG_EXPECTED = {
    ("990000000001", 0, "EQUITY", "999999999", "VTI", "10", "-1500.0", "150.0"),
    ("990000000001", 1, "CURRENCY", None, "CURRENCY_USD", "-1505.0",
     "-0.000015", None),
    ("990000000001", 2, None, None, None, None, None, None),
}


def _seed_field_bronze(root: Path) -> Path:
    dump = _seed_bronze(root)
    (dump / "accounts_positions.json").write_text(json.dumps([
        {"securitiesAccount": {"accountNumber": ACCT_PLAIN, "type": "MARGIN",
                               "positions": _FIELD_POSITIONS}},
    ]), encoding="utf-8")
    (dump / "transactions_000.json").write_text(json.dumps({
        "account_hash": ACCT_HASH, "window_start": "2024-01-01",
        "window_end": "2024-03-31", "transactions": [
            _LEG_TXN,
            {"activityId": "990000000002", "time": "2024-02-02T12:00:00+0000",
             "type": "JOURNAL"},
        ]}), encoding="utf-8")
    return dump


def _rows(conn, sql: str) -> set:
    return {tuple(r) for r in conn.execute(sql)}


def test_load_writes_the_promoted_position_fields(tmp_path):
    _seed_field_bronze(tmp_path / "bronze")
    db = tmp_path / "schwab-api.db"
    assert _load(db, tmp_path / "bronze") == 0
    assert _rows(_open(db), f"SELECT {_FIELD_COLUMNS} FROM positions"
                 ) == _FIELD_EXPECTED


def test_load_writes_every_leg_at_its_place(tmp_path):
    _seed_field_bronze(tmp_path / "bronze")
    db = tmp_path / "schwab-api.db"
    assert _load(db, tmp_path / "bronze") == 0
    assert _rows(_open(db), "SELECT * FROM transaction_legs") == _LEG_EXPECTED


def test_a_replaced_window_replaces_its_legs(tmp_path):
    bronze = tmp_path / "bronze"
    db = tmp_path / "schwab-api.db"
    _seed_field_bronze(bronze)
    assert _load(db, bronze) == 0
    # The later dump states the window again without the trade.
    _seed_with_txns(bronze, "20240402T000000Z", window_start="2024-01-01",
                    window_end="2024-03-31",
                    txn_specs=[("990000000003", "2024-02-03T12:00:00+0000")])
    assert _load(db, bronze) == 0
    assert _count(_open(db), "transaction_legs") == 0


def test_migration_0006_fills_what_the_loader_writes(tmp_path):
    """A silver at schema 0005, holding the rows a load writes, gets from
    the migration exactly the fields a fresh load of the same bronze
    writes."""
    _seed_field_bronze(tmp_path / "bronze")
    fresh = tmp_path / "fresh.db"
    assert _load(fresh, tmp_path / "bronze") == 0

    pre = tmp_path / "migrations-0005"
    pre.mkdir()
    for f in sorted(loader.MIGRATIONS_DIR.glob("000[1-5]_*.sql")):
        (pre / f.name).write_text(f.read_text(encoding="utf-8"),
                                  encoding="utf-8")
    old = tmp_path / "old.db"
    conn = loader.open_db(old)
    assert silver.apply_migrations(conn, pre) == 5
    conn.execute("ATTACH DATABASE ? AS fresh", (str(fresh),))
    with conn:
        for table in _BRONZE_TABLES:
            if table == "transaction_legs":
                continue
            cols = ", ".join(r[1] for r in conn.execute(
                f"PRAGMA main.table_info({table})"))
            conn.execute(f"INSERT INTO main.{table} ({cols}) "
                         f"SELECT {cols} FROM fresh.{table}")
    conn.execute("DETACH DATABASE fresh")
    assert silver.apply_migrations(conn, loader.MIGRATIONS_DIR) == 6
    conn.close()
    assert _dump_tables(old) == _dump_tables(fresh)


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
    "positions", "open_orders", "transactions", "transaction_legs",
    "instruments",
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


# ============================================================
# The load frame
# ============================================================

def test_runs_loaded_one_at_a_time_build_what_force_builds(tmp_path):
    # The later run states the first one's window again: its journal
    # replaces the trade at the same activity id, and a row joins.
    fixture = tmp_path / "fixture"
    runs = [
        _seed_full_bronze(fixture),
        _seed_with_txns(fixture, "20240402T000000Z",
                        window_start="2024-01-15", window_end="2024-04-02",
                        txn_specs=[("990000000001", "2024-02-01T12:00:00+0000"),
                                   ("990000000002", "2024-03-01T12:00:00+0000")]),
    ]
    incremental.check(loader.main, lambda db, bronze: [
        "--silver-db", str(db), "--bronze-dir", str(bronze)], runs,
        tmp_path / "work")


def test_a_missing_bronze_dir_ends_the_load(tmp_path):
    with pytest.raises(SystemExit, match="Bronze directory not found"):
        _load(tmp_path / "schwab-api.db", tmp_path / "nope")


def test_force_rebuilds_from_every_dump(tmp_path):
    bronze = tmp_path / "bronze"
    _seed_full_bronze(bronze)
    db = tmp_path / "schwab-api.db"
    assert _load(db, bronze) == 0
    before = _dump_tables(db)
    conn = _open(db)
    conn.execute("DELETE FROM positions")
    conn.commit()
    conn.close()
    assert _load(db, bronze, "--force") == 0
    assert _dump_tables(db) == before
