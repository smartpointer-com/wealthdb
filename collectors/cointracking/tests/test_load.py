"""Bronze→silver tests for the cointracking collector's load.py.

cointracking's silver is DuckDB. These tests seed a minimal
synthetic bronze run (a per-portfolio 13-column trades.csv) and run
the transaction ingest, asserting the projected silver
`transactions` rows. Synthetic portfolio ids / wallets / amounts
only.
"""
from __future__ import annotations

import json
import sys
from datetime import date, datetime
from decimal import Decimal
from pathlib import Path

import duckdb
import pytest

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import load as loader  # noqa: E402

CU = "1"

# The trades.csv export header (DuckDB auto-renames the duplicate
# "Cur." columns to Cur._1 / Cur._2).
TRADES_HEADER = (
    '"Type","Buy","Cur.","Sell","Cur.","Fee","Cur.","Exchange",'
    '"Group","Comment","Date","LPN","Tx-ID"'
)
TRADE_ROW = (
    '"Trade","0.5","BTC","15000","USD","10","USD","ExchangeA",'
    '"","","2024-01-15 10:00:00","","TXID1"'
)


@pytest.fixture(autouse=True)
def _no_timezone_env(monkeypatch):
    # The wrapper forwards a deployment's zones in this variable; keep
    # them out of the tests' synthetic portfolios.
    monkeypatch.delenv(loader.TIMEZONES_ENV, raising=False)


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


def _row(type_, *, buy="", buy_cur="", sell="", sell_cur="", fee="",
         fee_cur="", exchange="ExchangeA", group="", comment="",
         date="2024-01-15 10:00:00", tx_id="") -> str:
    """One trades.csv row (all fields quoted). Positional layout
    matches TRADES_HEADER: Type, Buy, Cur.(buy), Sell, Cur.(sell), Fee,
    Cur.(fee), Exchange, Group, Comment, Date, LPN, Tx-ID."""
    fields = [type_, buy, buy_cur, sell, sell_cur, fee, fee_cur,
              exchange, group, comment, date, "", tx_id]
    return ",".join(f'"{f}"' for f in fields)


def _seed_bronze_rows(root: Path, rows: list[str]) -> tuple[Path, dict]:
    """Materialise a bronze run whose cu_<CU> trades.csv holds `rows`."""
    run_dir = root / "20240201T000000Z"
    (run_dir / f"cu_{CU}").mkdir(parents=True, exist_ok=True)
    (run_dir / f"cu_{CU}" / "trades.csv").write_text(
        TRADES_HEADER + "\n" + "\n".join(rows) + "\n", encoding="utf-8")
    manifest = {"portfolios": [{"id": CU, "name": "test"}]}
    return run_dir, manifest


def _latest_positions(conn) -> dict[tuple[str, str], Decimal]:
    """Latest (wallet, instrument) → amount from positions_daily."""
    return {
        (w, i): amt for w, i, amt in conn.execute("""
            SELECT wallet, instrument, amount FROM (
                SELECT
                    SUBSTR(wallet_external_id,
                           STRPOS(wallet_external_id, ':') + 1) AS wallet,
                    instrument_external_id AS instrument,
                    amount,
                    ROW_NUMBER() OVER (
                        PARTITION BY wallet_external_id, instrument_external_id
                        ORDER BY as_of_date DESC) AS rn
                FROM positions_daily)
            WHERE rn = 1
        """).fetchall()
    }


def test_dust_sweep_other_expense_zeroes_the_swept_balance(tmp_path):
    # An exchange dust sweep (CoinTracking labels it "Dust Sweeping")
    # books an `Other Expense` sell of each swept dust balance paired
    # with an `Income (non taxable)` buy of the consolidated proceeds.
    # The sell leg is a real outgoing delta: if `Other Expense` is
    # unrouted the dust never leaves and the replay's per-wallet
    # balance stays stranded at the swept amount. Regression for that
    # gap (the buy leg was always handled). Amounts are synthetic.
    rows = [
        _row("Deposit", buy="0.01230000", buy_cur="ETH",
             date="2024-01-01 00:00:00"),
        _row("Other Expense", sell="0.01230000", sell_cur="ETH",
             comment="Dust Sweeping", date="2024-02-01 00:00:00"),
        _row("Income (non taxable)", buy="0.50", buy_cur="USD",
             comment="Dust Sweeping", date="2024-02-01 00:00:00"),
    ]
    run_dir, manifest = _seed_bronze_rows(tmp_path / "bronze", rows)
    conn = _fresh_db(tmp_path)
    loader.ingest_transactions(conn, manifest, run_dir, snapshot_at=1706745600,
                               timezones={})
    loader.upsert_positions_daily(conn, snapshot_at=1706745600)

    latest = _latest_positions(conn)
    # Dust fully swept out — ETH back to zero, not stranded at 0.01230000.
    assert latest[("ExchangeA", "ETH")] == 0
    # Consolidated proceeds landed.
    assert latest[("ExchangeA", "USD")] == Decimal("0.50")


def test_warn_unhandled_types_flags_unrouted_leg(tmp_path):
    # A populated leg whose type is on neither list must be reported,
    # not silently dropped — this is the guard that would have caught
    # the `Other Expense` gap at load time.
    rows = [
        _row("Trade", buy="0.5", buy_cur="BTC", sell="15000",
             sell_cur="USD", date="2024-01-15 10:00:00"),
        _row("Margin Trade", sell="1.0", sell_cur="BTC",
             date="2024-01-16 10:00:00"),
    ]
    run_dir, manifest = _seed_bronze_rows(tmp_path / "bronze", rows)
    conn = _fresh_db(tmp_path)
    loader.ingest_transactions(conn, manifest, run_dir, snapshot_at=1705312800,
                               timezones={})
    # (type, dropped_buy_legs, dropped_sell_legs)
    assert loader.warn_unhandled_transaction_types(conn) == [
        ("Margin Trade", 0, 1)]


def test_warn_unhandled_types_silent_when_covered(tmp_path):
    # Every type routed (incl. the dust-sweep pair) → no warnings.
    # Amounts are synthetic.
    rows = [
        _row("Other Expense", sell="0.02000000", sell_cur="EUR",
             comment="Dust Sweeping", date="2024-02-01 00:00:00"),
        _row("Income (non taxable)", buy="0.05000000", buy_cur="USD",
             comment="Dust Sweeping", date="2024-02-01 00:00:00"),
    ]
    run_dir, manifest = _seed_bronze_rows(tmp_path / "bronze", rows)
    conn = _fresh_db(tmp_path)
    loader.ingest_transactions(conn, manifest, run_dir, snapshot_at=1706745600,
                               timezones={})
    assert loader.warn_unhandled_transaction_types(conn) == []


def test_ingest_transactions(tmp_path):
    run_dir, manifest = _seed_bronze(tmp_path / "bronze")
    conn = _fresh_db(tmp_path)
    n = loader.ingest_transactions(conn, manifest, run_dir,
                                   snapshot_at=1705312800, timezones={})
    assert n == 1

    row = conn.execute(
        "SELECT portfolio_external_id, wallet_external_id, type, "
        "buy_amount, buy_currency, sell_amount, sell_currency, "
        "fee_amount, fee_currency FROM transactions").fetchall()
    assert len(row) == 1
    (portfolio, wallet, typ, buy, buy_ccy, sell, sell_ccy,
     fee, fee_ccy) = row[0]
    assert portfolio == "cu_1"
    assert wallet == "cu_1:ExchangeA"
    assert typ == "Trade"
    assert float(buy) == 0.5
    assert buy_ccy == "BTC"
    assert float(sell) == 15000.0
    assert sell_ccy == "USD"
    assert float(fee) == 10.0
    assert fee_ccy == "USD"


def _ids(tmp_path: Path, name: str, rows: list[str]) -> list[tuple[str, str]]:
    """Load `rows` into a fresh silver; return its (type,
    transaction_external_id) pairs, sorted."""
    run_dir, manifest = _seed_bronze_rows(tmp_path / name, rows)
    conn = duckdb.connect(str(tmp_path / f"{name}.duckdb"))
    loader.apply_migrations(conn)
    loader.ingest_transactions(conn, manifest, run_dir, snapshot_at=1706745600,
                               timezones={})
    return sorted(conn.execute(
        "SELECT type, transaction_external_id FROM transactions").fetchall())


def test_transaction_ids_follow_row_content_not_row_order(tmp_path):
    # Rows sharing a timestamp get the same id whatever order the
    # export lists them in; exact duplicates are told apart by a copy
    # number; an amended row gets a new id while its siblings keep
    # theirs.
    same_second = "2024-01-15 10:00:00"
    a = _row("Deposit", buy="1", buy_cur="BTC", date=same_second)
    b = _row("Staking", buy="0.01", buy_cur="BTC", date=same_second)
    c = _row("Withdrawal", sell="0.5", sell_cur="BTC", date=same_second)
    first = _ids(tmp_path, "first", [a, b, c, b])
    assert _ids(tmp_path, "second", [b, c, b, a]) == first

    ids = dict(first)
    staking = [tid for typ, tid in first if typ == "Staking"]
    base = staking[0]
    assert base.startswith(f"cu_{CU}:")
    assert staking == [base, base + ":2"]

    amended = dict(_ids(tmp_path, "amended", [
        a, _row("Staking", buy="0.02", buy_cur="BTC", date=same_second), c]))
    assert amended["Deposit"] == ids["Deposit"]
    assert amended["Withdrawal"] == ids["Withdrawal"]
    assert amended["Staking"] not in staking


def _transactions(conn) -> list[tuple]:
    return conn.execute(
        "SELECT * FROM transactions ORDER BY transaction_external_id"
    ).fetchall()


def test_ingest_transactions_compressed_converges(tmp_path):
    # Convergence gate for bronze compression: the same bronze content
    # as trades.csv.zst (the form download now writes, and the form
    # the recompress sweep leaves behind) must produce silver rows
    # identical to the plain-CSV load. DuckDB decompresses .csv.zst
    # natively inside read_csv_auto; load.py only resolves the path.
    from collectorkit import compress

    run_plain, manifest = _seed_bronze(tmp_path / "plain")
    conn_plain = duckdb.connect(str(tmp_path / "plain.duckdb"))
    loader.apply_migrations(conn_plain)
    loader.ingest_transactions(conn_plain, manifest, run_plain,
                               snapshot_at=1705312800, timezones={})

    run_zst, manifest = _seed_bronze(tmp_path / "zst")
    compress.compress_file(run_zst / f"cu_{CU}" / "trades.csv")
    assert not (run_zst / f"cu_{CU}" / "trades.csv").exists()
    conn_zst = duckdb.connect(str(tmp_path / "zst.duckdb"))
    loader.apply_migrations(conn_zst)
    n = loader.ingest_transactions(conn_zst, manifest, run_zst,
                                   snapshot_at=1705312800, timezones={})

    assert n == 1
    assert _transactions(conn_zst) == _transactions(conn_plain)


def test_bronze_csv_resolution_prefers_plain(tmp_path):
    # When a compressed twin and the plain original coexist (a
    # recompress sweep interrupted between verify and unlink), the
    # original is authoritative.
    from collectorkit import compress

    run_dir, _ = _seed_bronze(tmp_path / "bronze")
    plain = run_dir / f"cu_{CU}" / "trades.csv"
    compress.compress_file(plain, remove_original=False)
    assert loader._bronze_csv(run_dir, CU, "trades") == plain
    plain.unlink()
    assert loader._bronze_csv(run_dir, CU, "trades") == \
        run_dir / f"cu_{CU}" / "trades.csv.zst"
    assert loader._bronze_csv(run_dir, CU, "overview") is None


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
    loader.ingest_transactions(conn, manifest, run_dir, snapshot_at=1705312800,
                               timezones={})
    loader.ingest_transactions(conn, manifest, run_dir, snapshot_at=1705312800,
                               timezones={})
    assert conn.execute(
        "SELECT COUNT(*) FROM transactions").fetchone()[0] == 1


# overview.csv header: the Date column, two coin pairs (each a
# "<SYM> Value in <FIAT>" + "<SYM> Amount" pair), and one multi-word
# aggregate column that the coin-pair regex must exclude.
OVERVIEW_HEADER = (
    '"Date","BTC Value in USD","BTC Amount",'
    '"ETH Value in USD","ETH Amount","Account Total Value in USD"'
)


def _seed_overview(root: Path, slug: str, rows: list[str]) -> tuple[Path, dict]:
    """Materialise a bronze run whose cu_<CU> overview.csv holds `rows`
    under OVERVIEW_HEADER (all fields already quoted)."""
    run_dir = root / slug
    (run_dir / f"cu_{CU}").mkdir(parents=True, exist_ok=True)
    (run_dir / f"cu_{CU}" / "overview.csv").write_text(
        OVERVIEW_HEADER + "\n" + "\n".join(rows) + "\n", encoding="utf-8")
    manifest = {"portfolios": [{"id": CU, "name": "test"}]}
    return run_dir, manifest


def _prices(conn) -> dict[tuple[str, str, str], tuple[Decimal, int]]:
    """(as_of_date, instrument, quote) → (price, snapshot_at) from
    portfolio_prices."""
    return {
        (str(d), instr, q): (price, snap)
        for d, instr, q, price, snap in conn.execute(
            "SELECT as_of_date, instrument_external_id, quote_currency, "
            "price, snapshot_at FROM portfolio_prices"
        ).fetchall()
    }


def test_ingest_portfolio_prices_divides_filters_and_excludes_aggregate(
        tmp_path):
    # The folded price ingest: price = value / amount per coin pair,
    # with the multi-word aggregate column excluded, the "now" row
    # skipped, and the division-by-zero guard dropping rows whose
    # amount is 0 or whose value/amount is empty. Both Date locales
    # (YYYY/MM/DD and DD.MM.YYYY) parse. Amounts are synthetic.
    rows = [
        # "now" snapshot above the close-of-day rows — always skipped.
        '"now","9","0.5","9","9","9"',
        # Both coins priced: BTC 10000/0.5=20000, ETH 3000/10=300.
        '"2024/01/01","10000","0.5","3000","10","13000"',
        # BTC value empty → BTC dropped; ETH 6000/10=600.
        '"2024/01/02","","","6000","10","6000"',
        # BTC amount 0 → division-by-zero guard drops BTC; ETH 2000/5=400.
        '"2024/01/03","5000","0","2000","5","2000"',
        # DD.MM.YYYY locale row: BTC 1000/2=500; ETH empty → dropped.
        '"15.01.2024","1000","2","","","1000"',
    ]
    run_dir, manifest = _seed_overview(tmp_path / "bronze", "20240201T000000Z",
                                       rows)
    conn = _fresh_db(tmp_path)
    loader.ingest_portfolio_prices(conn, manifest, run_dir,
                                   snapshot_at=1706745600)

    prices = _prices(conn)
    assert prices == {
        ("2024-01-01", "BTC", "USD"): (Decimal("20000"), 1706745600),
        ("2024-01-01", "ETH", "USD"): (Decimal("300"), 1706745600),
        ("2024-01-02", "ETH", "USD"): (Decimal("600"), 1706745600),
        ("2024-01-03", "ETH", "USD"): (Decimal("400"), 1706745600),
        ("2024-01-15", "BTC", "USD"): (Decimal("500"), 1706745600),
    }


def test_ingest_portfolio_prices_first_writer_wins_across_snapshots(tmp_path):
    # Re-ingesting a later snapshot must leave already-present price
    # keys untouched (original value + snapshot_at kept) and only add
    # genuinely new dates — the anti-join + ON CONFLICT DO NOTHING
    # first-writer-wins semantics gold relies on to skip unchanged
    # history. Amounts are synthetic.
    conn = _fresh_db(tmp_path)
    run1, manifest = _seed_overview(tmp_path / "b1", "20240201T000000Z",
                                    ['"2024/01/01","10000","0.5","","","10000"'])
    loader.ingest_portfolio_prices(conn, manifest, run1, snapshot_at=100)

    # Later snapshot: same date with a different BTC value (must NOT
    # overwrite) plus a brand-new date (must land).
    run2, manifest = _seed_overview(
        tmp_path / "b2", "20240202T000000Z",
        ['"2024/01/01","11000","0.5","","","11000"',
         '"2024/01/02","12000","0.5","","","12000"'])
    loader.ingest_portfolio_prices(conn, manifest, run2, snapshot_at=200)

    prices = _prices(conn)
    # 2024-01-01 keeps the first snapshot's value (20000) and stamp.
    assert prices[("2024-01-01", "BTC", "USD")] == (Decimal("20000"), 100)
    # 2024-01-02 is new: second snapshot's value (24000) and stamp.
    assert prices[("2024-01-02", "BTC", "USD")] == (Decimal("24000"), 200)


def test_stage_work_db_seeds_from_existing_target(tmp_path):
    # With a scratch dir, an existing silver DB is copied in so the
    # incremental load sees prior state; without one, the silver DB is
    # opened in place.
    silver = tmp_path / "silver" / "cointracking.duckdb"
    silver.parent.mkdir()
    silver.write_bytes(b"EXISTING-STATE")
    scratch = tmp_path / "scratch"

    work, promote = loader._stage_work_db(silver, scratch)
    assert promote is True
    assert work == scratch / silver.name
    assert work.read_bytes() == b"EXISTING-STATE"  # prior state carried in

    # No scratch dir → open in place, nothing to promote.
    assert loader._stage_work_db(silver, None) == (silver, False)


def _seed_complete_run(root: Path, slug: str) -> Path:
    """A loadable bronze snapshot: run.json (complete) + a one-row
    trades.csv, enough to drive main() end to end."""
    run_dir = root / slug
    (run_dir / f"cu_{CU}").mkdir(parents=True, exist_ok=True)
    (run_dir / f"cu_{CU}" / "trades.csv").write_text(
        TRADES_HEADER + "\n" + TRADE_ROW + "\n", encoding="utf-8")
    (run_dir / "run.json").write_text(json.dumps(
        {"portfolios": [{"id": CU, "name": "test"}], "status": "complete"}),
        encoding="utf-8")
    return run_dir


def test_scratch_dir_promotes_finished_db_and_cleans_up(tmp_path):
    # Driving main() with --scratch-dir builds the DB under scratch and
    # moves it onto --silver-db, leaving no scratch copy or temp behind.
    bronze = tmp_path / "bronze"
    _seed_complete_run(bronze, "20240115T100000Z")
    silver = tmp_path / "silver" / "cointracking.duckdb"
    scratch = tmp_path / "scratch"

    rc = loader.main([
        "--bronze-dir", str(bronze),
        "--silver-db", str(silver),
        "--scratch-dir", str(scratch),
        "--force",
        "--no-fetch-prices",  # price fill is default-on; this test is offline
    ])

    assert rc == 0
    assert silver.exists()                                # promoted onto target
    assert not (scratch / silver.name).exists()           # scratch copy cleared
    assert not silver.with_name(silver.name + ".tmp").exists()  # no temp left
    conn = duckdb.connect(str(silver))
    assert conn.execute(
        "SELECT COUNT(*) FROM transactions").fetchone()[0] == 1


def test_scratch_dir_cleans_temp_when_promote_fails(tmp_path, monkeypatch):
    # If the promote raises between the copy and the atomic rename, the
    # staging temp beside the target must not linger and the scratch copy
    # must still be cleared — and the target is never a partial file (the
    # rename never ran, so it stays absent here).
    bronze = tmp_path / "bronze"
    _seed_complete_run(bronze, "20240115T100000Z")
    silver = tmp_path / "silver" / "cointracking.duckdb"
    scratch = tmp_path / "scratch"

    def boom(src, dst):
        raise OSError("simulated crash mid-promote")
    monkeypatch.setattr(loader.os, "replace", boom)

    with pytest.raises(OSError):
        loader.main([
            "--bronze-dir", str(bronze),
            "--silver-db", str(silver),
            "--scratch-dir", str(scratch),
            "--force",
            "--no-fetch-prices",  # price fill is default-on; this test is offline
        ])

    assert not silver.with_name(silver.name + ".tmp").exists()  # temp cleaned
    assert not (scratch / silver.name).exists()                 # scratch cleaned
    assert not silver.exists()                                  # target untouched


# Tables excluded from the force-rebuild equivalence dump below.
# `schema_meta` is migration bookkeeping, not silver data: its
# `applied_at` column defaults to CURRENT_TIMESTAMP, so it records the
# wall-clock instant each load applied the migrations and necessarily
# differs between two runs. Every genuine data table instead stamps its
# rows with the snapshot's deterministic run timestamp, so those match
# exactly.
_NON_DATA_TABLES = {"schema_meta"}


def _dump_silver_state(silver: Path) -> dict[str, list[tuple]]:
    """Every base data table's full row set, each deterministically
    sorted, keyed by table name — a structure two loads can be compared
    on. A fresh read connection keeps connection-scoped staging temps out
    of scope, so only the persistent silver tables are dumped."""
    conn = duckdb.connect(str(silver))
    try:
        tables = [r[0] for r in conn.execute(
            "SELECT table_name FROM information_schema.tables "
            "WHERE table_schema = 'main' AND table_type = 'BASE TABLE' "
            "ORDER BY table_name").fetchall()]
        state: dict[str, list[tuple]] = {}
        for t in tables:
            if t in _NON_DATA_TABLES:
                continue
            rows = conn.execute(f'SELECT * FROM "{t}"').fetchall()
            # Sort on a string projection of each row so NULL / JSON /
            # Decimal cells order without type-comparison errors; the
            # rows themselves keep their native typed values for the
            # equality assertion, making the comparison exact.
            rows.sort(key=lambda r: [str(c) for c in r])
            state[t] = rows
        return state
    finally:
        conn.close()


def test_force_rebuild_equals_incremental(tmp_path):
    # `load --force` deletes the silver DB and rebuilds it from all bronze
    # (silver.reset runs in main() before the work DB is staged, so the
    # load seeds empty and every snapshot re-ingests). For UNCHANGED
    # bronze that rebuild must reproduce a plain incremental load exactly
    # across every data table — the guarantee that makes --force a safe
    # repair rather than a state-altering operation. --no-fetch-prices
    # keeps both loads offline (the post-ingest USD price fill is
    # default-on and would otherwise reach the network).
    bronze = tmp_path / "bronze"
    _seed_complete_run(bronze, "20240115T100000Z")
    silver = tmp_path / "silver" / "cointracking.duckdb"

    argv = ["--bronze-dir", str(bronze), "--silver-db", str(silver),
            "--no-fetch-prices"]

    # Plain incremental load into a fresh silver DB, then capture state.
    assert loader.main(argv) == 0
    incremental = _dump_silver_state(silver)
    # Guard against a vacuous pass: the load must actually have populated
    # silver, so the equivalence below compares real state, not two empties.
    assert incremental["transactions"], "incremental load wrote no transactions"

    # Force rebuild over the SAME unchanged bronze, then re-capture.
    assert loader.main(argv + ["--force"]) == 0
    rebuilt = _dump_silver_state(silver)

    assert rebuilt == incremental


# ---- Group, Tx-ID and the export's local time ------------------------

def _seed_export(root: Path, slug: str, portfolios: dict[str, list[str]],
                 header: str = TRADES_HEADER) -> Path:
    """A complete bronze run: run.json plus one trades.csv per
    portfolio id in `portfolios`, holding that portfolio's rows."""
    run_dir = root / slug
    for cu, rows in portfolios.items():
        (run_dir / f"cu_{cu}").mkdir(parents=True, exist_ok=True)
        (run_dir / f"cu_{cu}" / "trades.csv").write_text(
            header + "\n" + "\n".join(rows) + "\n", encoding="utf-8")
    (run_dir / "run.json").write_text(json.dumps({
        "portfolios": [{"id": cu, "name": f"test {cu}"} for cu in portfolios],
        "status": "complete",
    }), encoding="utf-8")
    return run_dir


def _ingest(tmp_path: Path, portfolios: dict[str, list[str]],
            timezones: dict[str, str], header: str = TRADES_HEADER,
            name: str = "silver") -> duckdb.DuckDBPyConnection:
    """Ingest one export into a fresh silver and return the connection."""
    run_dir = _seed_export(tmp_path / f"{name}-bronze", "20240201T000000Z",
                           portfolios, header)
    manifest = json.loads((run_dir / "run.json").read_text())
    conn = duckdb.connect(str(tmp_path / f"{name}.duckdb"))
    loader.apply_migrations(conn)
    loader.ingest_transactions(conn, manifest, run_dir, snapshot_at=1706745600,
                               timezones=timezones)
    return conn


def _times(conn) -> dict[tuple[str, str], str]:
    """(portfolio, occurred_local) → occurred_at as text."""
    return {(p, local): str(at) for p, local, at in conn.execute(
        "SELECT portfolio_external_id, occurred_local, occurred_at "
        "FROM transactions").fetchall()}


def test_group_and_tx_id_land_as_columns(tmp_path):
    # A populated Group / Tx-ID is kept as printed; a blank one is NULL.
    conn = _ingest(tmp_path, {CU: [
        _row("Trade", buy="1", buy_cur="ETH", sell="0.05", sell_cur="BTC",
             group="GroupA", tx_id="TX-0001", date="2024-01-15 10:00:00"),
        _row("Deposit", buy="1", buy_cur="BTC", date="2024-01-14 10:00:00"),
    ]}, timezones={})
    assert sorted(conn.execute(
        "SELECT type, trade_group, tx_id FROM transactions").fetchall()) == [
        ("Deposit", None, None),
        ("Trade", "GroupA", "TX-0001"),
    ]


def test_export_without_group_and_tx_id_columns_loads(tmp_path):
    # An export whose header lacks Group, LPN and Tx-ID still loads;
    # the three columns are NULL.
    header = ('"Type","Buy","Cur.","Sell","Cur.","Fee","Cur.","Exchange",'
              '"Comment","Date"')
    row = '"Deposit","1","BTC","","","","","ExchangeA","","2024-01-15 10:00:00"'
    conn = _ingest(tmp_path, {CU: [row]}, timezones={}, header=header)
    assert conn.execute(
        "SELECT lpn, trade_group, tx_id, occurred_local, occurred_at "
        "FROM transactions").fetchall() == [
        (None, None, None, "2024-01-15 10:00:00",
         datetime(2024, 1, 15, 10, 0)),
    ]


def test_occurred_at_is_local_date_converted_from_the_portfolio_zone(
        tmp_path):
    # Portfolio 1 has a zone: its Date reads as local time there and
    # converts to UTC, at the winter and the summer offset. Portfolio 2
    # has none and reads as UTC. occurred_local keeps the text as
    # exported for both.
    winter, summer = "2024-01-15 00:30:00", "2024-07-15 12:00:00"
    rows = [_row("Deposit", buy="1", buy_cur="BTC", date=winter),
            _row("Deposit", buy="2", buy_cur="BTC", date=summer)]
    conn = _ingest(tmp_path, {"1": rows, "2": rows},
                   timezones={"cu_1": "Europe/Zurich"})
    assert _times(conn) == {
        ("cu_1", winter): "2024-01-14 23:30:00",
        ("cu_1", summer): "2024-07-15 10:00:00",
        ("cu_2", winter): winter,
        ("cu_2", summer): summer,
    }


@pytest.mark.parametrize("zone, local, utc", [
    # Wall times a spring-forward change skips read with the offset in
    # force before the change.
    ("Europe/Zurich", "2024-03-31 02:30:00", "2024-03-31 01:30:00"),
    ("America/New_York", "2024-03-10 02:30:00", "2024-03-10 07:30:00"),
    # Wall times a fall-back change repeats read as the later one.
    ("Europe/Zurich", "2024-10-27 02:30:00", "2024-10-27 01:30:00"),
    ("America/New_York", "2024-11-03 01:30:00", "2024-11-03 06:30:00"),
    # Either side of a change, the offset of that side.
    ("Europe/Zurich", "2024-03-31 01:59:59", "2024-03-31 00:59:59"),
    ("Europe/Zurich", "2024-03-31 03:00:00", "2024-03-31 01:00:00"),
])
def test_occurred_at_on_a_daylight_saving_change(tmp_path, zone, local, utc):
    conn = _ingest(tmp_path, {CU: [_row("Staking", buy="1", buy_cur="ETH",
                                        date=local)]},
                   timezones={f"cu_{CU}": zone})
    assert _times(conn) == {(f"cu_{CU}", local): utc}


def test_transaction_ids_do_not_depend_on_the_timezone(tmp_path):
    rows = [_row("Deposit", buy="1", buy_cur="BTC", date="2024-01-15 00:30:00"),
            _row("Trade", buy="1", buy_cur="ETH", sell="0.05", sell_cur="BTC",
                 group="GroupA", tx_id="TX-0001",
                 date="2024-01-15 00:30:00")]

    def ids(name, timezones):
        conn = _ingest(tmp_path, {CU: rows}, timezones, name=name)
        return sorted(r[0] for r in conn.execute(
            "SELECT transaction_external_id FROM transactions").fetchall())

    assert ids("utc", {}) == ids("tokyo", {f"cu_{CU}": "Asia/Tokyo"})


def test_parse_timezones_items_env_and_errors(monkeypatch):
    zones = {"cu_1": "Europe/Zurich", "cu_2": "America/New_York"}
    assert loader.parse_args([
        "--timezone", "cu_1=Europe/Zurich",
        "--timezone", "cu_2=America/New_York",
    ]).timezones == zones
    # No flag: the items come from the environment variable.
    monkeypatch.setenv(loader.TIMEZONES_ENV,
                       " cu_1=Europe/Zurich\tcu_2=America/New_York ")
    assert loader.parse_args([]).timezones == zones
    # A flag replaces the environment variable entirely.
    assert loader.parse_args(["--timezone", "cu_3=UTC"]).timezones == {
        "cu_3": "UTC"}
    monkeypatch.delenv(loader.TIMEZONES_ENV)
    assert loader.parse_args([]).timezones == {}
    # The same zone twice is fine.
    assert loader.parse_timezones(["cu_1=UTC", "cu_1=UTC"]) == {"cu_1": "UTC"}


@pytest.mark.parametrize("items, message", [
    (["1=Europe/Zurich"], "is not cu_<id>=<zone>"),
    (["cu_1"], "is not cu_<id>=<zone>"),
    (["cu_1=Europe/Nowhere"], "unknown timezone"),
    (["cu_1=UTC", "cu_1=Europe/Zurich"], "two timezones"),
])
def test_parse_timezones_rejects(items, message, capsys):
    with pytest.raises(ValueError, match=message):
        loader.parse_timezones(items)
    argv = [a for item in items for a in ("--timezone", item)]
    with pytest.raises(SystemExit) as exc:
        loader.parse_args(argv)
    assert exc.value.code == 2
    assert message in capsys.readouterr().err


def _silver_argv(tmp_path: Path) -> list[str]:
    return ["--bronze-dir", str(tmp_path / "bronze"),
            "--silver-db", str(tmp_path / "silver" / "cointracking.duckdb"),
            "--no-fetch-prices"]


def _query(tmp_path: Path, sql: str) -> list[tuple]:
    conn = duckdb.connect(str(tmp_path / "silver" / "cointracking.duckdb"))
    try:
        return conn.execute(sql).fetchall()
    finally:
        conn.close()


def _schema_version() -> int:
    conn = duckdb.connect()
    try:
        return loader.apply_migrations(conn)
    finally:
        conn.close()


def test_load_ingests_the_newest_export_again_when_a_timezone_changes(
        tmp_path, caplog):
    # A zone set after the export was loaded applies at the next load,
    # without a new download. A load with nothing changed reads nothing
    # again.
    _seed_export(tmp_path / "bronze", "20240201T000000Z", {CU: [
        _row("Deposit", buy="1", buy_cur="BTC", date="2024-01-15 00:30:00")]})
    argv = _silver_argv(tmp_path)
    zoned = argv + ["--timezone", f"cu_{CU}=Europe/Zurich"]
    state_sql = ("SELECT p.timezone, t.occurred_at, d.as_of_date, "
                 "r.silver_schema_version "
                 "FROM portfolios p, transactions t, positions_daily d, "
                 "dump_runs r")

    assert loader.main(argv) == 0
    assert _query(tmp_path, state_sql) == [
        ("UTC", datetime(2024, 1, 15, 0, 30), date(2024, 1, 15),
         _schema_version())]

    caplog.set_level("INFO", logger="cointracking.load")
    assert loader.main(zoned) == 0
    assert "again" in caplog.text
    # The deposit moves to the UTC day before; the replay follows it.
    assert _query(tmp_path, state_sql) == [
        ("Europe/Zurich", datetime(2024, 1, 14, 23, 30), date(2024, 1, 14),
         _schema_version())]

    caplog.clear()
    assert loader.main(zoned) == 0
    assert "again" not in caplog.text


def test_load_backfills_rows_loaded_under_an_older_schema(tmp_path):
    # Silver at schema v2 with the export already loaded, as a load
    # before v3 left it. The migration adds the columns: the portfolio
    # reads as UTC, which is the zone that load applied, and the
    # transaction holds NULL. The next load ingests the export again
    # and fills them.
    silver = tmp_path / "silver" / "cointracking.duckdb"
    silver.parent.mkdir()
    run_dir = _seed_export(tmp_path / "bronze", "20240201T000000Z", {CU: [
        _row("Trade", buy="1", buy_cur="ETH", sell="0.05", sell_cur="BTC",
             group="GroupA", tx_id="TX-0001", date="2024-01-15 10:00:00")]})
    conn = duckdb.connect(str(silver))
    for name in ("0001_initial.sql", "0002_prices.sql"):
        conn.execute((loader.MIGRATIONS_DIR / name).read_text())
    conn.execute("INSERT INTO portfolios VALUES (?, ?, 'test', 1, NULL)",
                 [f"cu_{CU}", int(CU)])
    conn.execute(
        "INSERT INTO transactions (transaction_external_id, "
        "portfolio_external_id, wallet_external_id, snapshot_at, "
        "occurred_at, type) VALUES (?, ?, ?, 1, ?, 'Trade')",
        [f"cu_{CU}:0000000000000000", f"cu_{CU}", f"cu_{CU}:ExchangeA",
         datetime(2024, 1, 15, 10, 0)])
    conn.execute("INSERT INTO dump_runs VALUES (?, 1, ?, NULL)",
                 [loader.parse_run_ts(run_dir), str(run_dir)])
    loader.apply_migrations(conn)
    assert conn.execute(
        "SELECT p.timezone, t.trade_group, t.tx_id, t.occurred_local "
        "FROM portfolios p, transactions t").fetchall() == [
        ("UTC", None, None, None)]
    conn.close()

    assert loader.main(_silver_argv(tmp_path)) == 0
    assert _query(tmp_path,
                  "SELECT t.trade_group, t.tx_id, t.occurred_local, "
                  "t.occurred_at, r.silver_schema_version "
                  "FROM transactions t, dump_runs r") == [
        ("GroupA", "TX-0001", "2024-01-15 10:00:00",
         datetime(2024, 1, 15, 10, 0), _schema_version())]
