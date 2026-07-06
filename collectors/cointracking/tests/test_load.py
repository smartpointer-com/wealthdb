"""Bronze→silver tests for the cointracking collector's load.py.

cointracking's silver is DuckDB. These tests seed a minimal
synthetic bronze run (a per-portfolio 19-column trades.csv) and run
the transaction ingest, asserting the projected silver
`transactions` rows. Synthetic portfolio ids / wallets / amounts
only.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

import duckdb

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import load as loader  # noqa: E402

CU = "1"

# The "Extended with additional columns" trades.csv header (DuckDB
# auto-renames the duplicate "Cur." columns to Cur._1 / Cur._2).
TRADES_HEADER = (
    '"Type","Buy","Cur.","Sell","Cur.","Fee","Cur.","Exchange",'
    '"Group","Comment","Trade ID","Imported From","Add Date","Date",'
    '"From Address","To Address","Tx Hash","Sell From Address",'
    '"Sell To Address"'
)
TRADE_ROW = (
    '"Trade","0.5","BTC","15000","USD","10","USD","Kraken",'
    '"","","TID1","","","2024-01-15 10:00:00","","","","",""'
)


def _seed_bronze(root: Path) -> tuple[Path, dict]:
    run_dir = root / "20240115T100000Z"
    (run_dir / f"cu_{CU}").mkdir(parents=True, exist_ok=True)
    (run_dir / f"cu_{CU}" / "trades.csv").write_text(
        TRADES_HEADER + "\n" + TRADE_ROW + "\n", encoding="utf-8")
    manifest = {"portfolios": [{"id": CU, "name": "test"}]}
    return run_dir, manifest


def _fresh_db(tmp_path: Path) -> duckdb.DuckDBPyConnection:
    conn = duckdb.connect(str(tmp_path / "cointracking.duckdb"))
    loader.apply_migrations(conn)
    return conn


def test_ingest_transactions(tmp_path):
    run_dir, manifest = _seed_bronze(tmp_path / "bronze")
    conn = _fresh_db(tmp_path)
    n = loader.ingest_transactions(conn, manifest, run_dir,
                                   snapshot_at=1705312800)
    assert n == 1

    row = conn.execute(
        "SELECT portfolio_external_id, wallet_external_id, type, "
        "buy_amount, buy_currency, sell_amount, sell_currency, "
        "fee_amount, fee_currency FROM transactions").fetchall()
    assert len(row) == 1
    (portfolio, wallet, typ, buy, buy_ccy, sell, sell_ccy,
     fee, fee_ccy) = row[0]
    assert portfolio == "cu_1"
    assert wallet == "cu_1:Kraken"
    assert typ == "Trade"
    assert float(buy) == 0.5
    assert buy_ccy == "BTC"
    assert float(sell) == 15000.0
    assert sell_ccy == "USD"
    assert float(fee) == 10.0
    assert fee_ccy == "USD"


def _seed_run(root: Path, slug: str, status: str | None) -> Path:
    """Materialise a run dir with a run.json carrying `status`
    (omitted entirely when None) plus a cu_<id>/ so it looks real."""
    run_dir = root / slug
    (run_dir / f"cu_{CU}").mkdir(parents=True, exist_ok=True)
    meta: dict = {"portfolios": [{"id": CU, "name": "test"}]}
    if status is not None:
        meta["status"] = status
    (run_dir / "run.json").write_text(json.dumps(meta), encoding="utf-8")
    return run_dir


def test_discover_skips_in_progress_keeps_complete_and_legacy(tmp_path):
    # The load-guard paired with the in-progress marker: a crashed walk
    # leaves run.json={"status":"in-progress"}, which discover_bronze_
    # snapshots must skip so a partial dump never reaches silver. A
    # complete dump and a statusless (pre-lifecycle) manifest stay
    # loadable.
    bronze = tmp_path / "bronze"
    _seed_run(bronze, "20240114T100000Z", status=None)          # legacy
    _seed_run(bronze, "20240115T100000Z", status="complete")    # complete
    _seed_run(bronze, "20240116T100000Z", status="in-progress")  # crashed
    _seed_run(bronze, "20240117T100000Z", status="dry-run")      # shell

    names = {p.name for p in loader.discover_bronze_snapshots(bronze)}
    assert names == {"20240114T100000Z", "20240115T100000Z"}


def test_ingest_replaces_per_portfolio(tmp_path):
    # A second ingest of the same portfolio replaces (not duplicates)
    # — each trades.csv is a complete replay of that portfolio.
    run_dir, manifest = _seed_bronze(tmp_path / "bronze")
    conn = _fresh_db(tmp_path)
    loader.ingest_transactions(conn, manifest, run_dir, snapshot_at=1705312800)
    loader.ingest_transactions(conn, manifest, run_dir, snapshot_at=1705312800)
    assert conn.execute(
        "SELECT COUNT(*) FROM transactions").fetchone()[0] == 1
