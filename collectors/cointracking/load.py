#!/usr/bin/env python3
"""cointracking silver loader.

Reads the bronze tree under --bronze-dir (one snapshot dir per
download run, each containing run.json + cu_<id>/trades.csv +
cu_<id>/balance.csv) and ingests into the DuckDB silver layer:

  - `transactions` is fully replaced with the rows from the latest
    processed snapshot. (Every download is a complete dump; we
    don't keep per-snapshot transaction history in silver.)

  - `positions_daily` is INCREMENTALLY upserted via the
    aggregate-then-window replay. The full new time series is
    computed; we find the first as_of_date where new vs existing
    rows differ (changed amount, new row, or removed row) and
    rewrite only from that day onwards. Pre-deviation rows keep
    their original snapshot_at so gold downstream doesn't
    re-process unchanged history.

  - `portfolios` + `wallets` are upserted from run.json + the CSV
    Exchange column.

  - `dump_runs` records each snapshot processed (idempotency gate;
    a re-run skips already-loaded snapshots).

After ingest, the loader reconciles each portfolio's computed
final balance against the balance.csv from /balance_by_exchange.php.
Discrepancies bigger than the 8-decimal CoinTracking export
precision are logged as warnings — not failures.
"""
from __future__ import annotations

import argparse
import json
import logging
import re
import sys
from datetime import datetime, timezone
from pathlib import Path

import duckdb

from collectorkit import cli

log = logging.getLogger("cointracking.load")

MIGRATIONS_DIR = Path(__file__).resolve().parent / "migrations"

# Reconciliation tolerance. CoinTracking exports amounts truncated
# at 8 decimals, so a per-row discrepancy below this means rendering
# imprecision rather than a real balance mismatch.
RECONCILE_ABS_TOL = "0.00000001"


# Aggregate-then-window replay against the `transactions` table.
# Bound parameter at the end is the snapshot_at for newly-written
# rows.
#
# Type handler lists. CoinTracking has accumulated a ~16-type
# vocabulary over time; the set below is the union observed across
# the linked portfolios after running the reconciliation against
# captured balance.csv data. Several types are direction-ambiguous
# (Gift / Tip, Gift) — they appear on both lists, with the
# `buy_amount IS NOT NULL` / `sell_amount IS NOT NULL` filter
# routing each row to exactly one branch based on which column is
# populated.
#
#   BUY side (`+buy_amount` on `buy_currency`):
#     Trade, Deposit, Staking, Reward / Bonus, Income (taxable + non),
#     Airdrop (taxable + non), Gift / Tip, Gift
#
#   SELL side (`-sell_amount` on `sell_currency`):
#     Trade, Withdrawal, Other Fee, Lost, Stolen, Spend, Donation,
#     Gift / Tip, Gift, Expense (non taxable)
#
# Fee semantics: `fee_amount` on regular rows is informative only;
# `Other Fee` rows ARE balance deltas (the fee IS the event).
# See DESIGN.md.
REPLAY_SQL_TEMPLATE = """
WITH deltas AS (
    SELECT
        portfolio_external_id,
        wallet_external_id,
        buy_currency AS instrument,
        CAST(occurred_at AS DATE) AS as_of_date,
        buy_amount AS delta
    FROM transactions
    WHERE buy_amount IS NOT NULL
      AND buy_currency IS NOT NULL
      AND type IN ('Trade', 'Deposit', 'Staking',
                   'Reward / Bonus',
                   'Income', 'Income (non taxable)',
                   'Airdrop', 'Airdrop (non taxable)',
                   'Gift / Tip', 'Gift')
    UNION ALL
    SELECT
        portfolio_external_id,
        wallet_external_id,
        sell_currency AS instrument,
        CAST(occurred_at AS DATE) AS as_of_date,
        -sell_amount AS delta
    FROM transactions
    WHERE sell_amount IS NOT NULL
      AND sell_currency IS NOT NULL
      AND type IN ('Trade', 'Withdrawal', 'Other Fee',
                   'Lost', 'Stolen', 'Spend', 'Donation',
                   'Gift / Tip', 'Gift',
                   'Expense (non taxable)')
),
daily_deltas AS (
    SELECT
        portfolio_external_id,
        wallet_external_id,
        instrument,
        as_of_date,
        SUM(delta) AS daily_delta
    FROM deltas
    GROUP BY portfolio_external_id, wallet_external_id,
             instrument, as_of_date
)
SELECT
    as_of_date,
    portfolio_external_id,
    wallet_external_id,
    instrument AS instrument_external_id,
    SUM(daily_delta) OVER (
        PARTITION BY portfolio_external_id, wallet_external_id, instrument
        ORDER BY as_of_date
        ROWS UNBOUNDED PRECEDING
    ) AS amount,
    ? AS snapshot_at
FROM daily_deltas
"""


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--silver-db", type=Path,
        default=Path("/data/cointracking.duckdb"),
        help="Silver DuckDB path. Default: %(default)s.",
    )
    p.add_argument(
        "--bronze-dir", type=Path, default=Path("/data"),
        help=("Bronze tree root. Snapshots are UTC-timestamped "
              "subdirs containing run.json + cu_<id>/{trades,balance}.csv. "
              "Default: %(default)s."),
    )
    p.add_argument(
        "--replay-only", action="store_true",
        help=("Skip bronze ingest; just re-run the holdings replay "
              "against whatever's already in `transactions`. Useful "
              "for iterating on the type-handler rules."),
    )
    p.add_argument(
        "--force", action="store_true",
        help=("Re-load snapshots already present in `dump_runs`. "
              "Without --force, previously-loaded snapshots are "
              "skipped."),
    )
    cli.add_common_args(p)
    return p.parse_args(argv)


def apply_migrations(conn: duckdb.DuckDBPyConnection) -> int:
    for sql_file in sorted(MIGRATIONS_DIR.glob("*.sql")):
        log.debug("applying %s", sql_file.name)
        conn.execute(sql_file.read_text())
    return conn.execute(
        "SELECT MAX(silver_schema_version) FROM schema_meta"
    ).fetchone()[0]


def discover_bronze_snapshots(bronze_dir: Path) -> list[Path]:
    """Return the timestamped subdirs of bronze_dir that contain a
    run.json (chronological)."""
    if not bronze_dir.is_dir():
        return []
    snapshots = []
    for p in sorted(bronze_dir.iterdir()):
        if not p.is_dir():
            continue
        if not re.match(r"\d{8}T\d{6}Z$", p.name):
            continue
        if not (p / "run.json").is_file():
            continue
        snapshots.append(p)
    return snapshots


def parse_run_ts(run_dir: Path) -> int:
    """Parse `YYYYMMDDTHHMMSSZ` directory name into a UTC unix
    timestamp."""
    dt = datetime.strptime(run_dir.name, "%Y%m%dT%H%M%SZ").replace(
        tzinfo=timezone.utc)
    return int(dt.timestamp())


def ingest_portfolios_and_wallets(
    conn: duckdb.DuckDBPyConnection,
    manifest: dict, snapshot_at: int,
) -> None:
    """Upsert portfolio + wallet rows from the run.json manifest +
    the trade CSVs' Exchange column. Wallets are discovered from
    transactions; portfolios come from the manifest."""
    # Portfolios — one row per linked CoinTracking user, keyed on
    # the change_user ID.
    for portfolio in manifest["portfolios"]:
        conn.execute(
            """INSERT INTO portfolios
               (portfolio_external_id, cointracking_user_id, display_name,
                snapshot_at, payload)
               VALUES (?, ?, ?, ?, ?)
               ON CONFLICT (portfolio_external_id) DO UPDATE SET
                 display_name = excluded.display_name,
                 snapshot_at  = excluded.snapshot_at,
                 payload      = excluded.payload""",
            [f"cu_{portfolio['id']}", int(portfolio["id"]),
             portfolio["name"], snapshot_at,
             json.dumps(portfolio)],
        )

    # Wallets — derived from the Exchange column after the
    # transactions table is loaded. Upserted by (portfolio, wallet).
    conn.execute("""
        INSERT INTO wallets
            (portfolio_external_id, wallet_external_id, display_name,
             snapshot_at, payload)
        SELECT DISTINCT
            portfolio_external_id,
            wallet_external_id,
            -- wallet_external_id is namespaced as 'cu_<id>:<wallet>';
            -- display_name strips the namespace prefix.
            SPLIT_PART(wallet_external_id, ':', 2) AS display_name,
            ? AS snapshot_at,
            NULL AS payload
        FROM transactions
        ON CONFLICT (portfolio_external_id, wallet_external_id) DO UPDATE SET
            display_name = excluded.display_name,
            snapshot_at  = excluded.snapshot_at
    """, [snapshot_at])


def ingest_transactions(
    conn: duckdb.DuckDBPyConnection,
    manifest: dict, run_dir: Path, snapshot_at: int,
) -> int:
    """Truncate `transactions` and re-populate from every portfolio's
    trades.csv. Returns the row count written. Each snapshot is a
    complete dump; we don't keep per-snapshot history in
    transactions. (positions_daily is the time-series store.)"""
    conn.execute("DELETE FROM transactions")

    total = 0
    for portfolio in manifest["portfolios"]:
        cu_id = portfolio["id"]
        trades_csv = run_dir / f"cu_{cu_id}" / "trades.csv"
        if not trades_csv.is_file():
            log.warning("no trades.csv for cu_%s — skipping", cu_id)
            continue

        # Load the CSV into a temp table first so we can map columns.
        # all_varchar=true so numeric columns with '' (empty) values
        # parse cleanly; TRY_CAST below promotes them to DECIMAL.
        conn.execute("""
            CREATE OR REPLACE TEMP TABLE raw AS
            SELECT * FROM read_csv_auto(?, header=true, all_varchar=true)
        """, [str(trades_csv)])

        # Verify the expected column names are present. The blob
        # CSV header is:
        #   "Type","Buy","Cur.","Sell","Cur.","Fee","Cur.","Exchange",
        #   "Group","Comment","Trade ID","Imported From","Add Date",
        #   "Date","From Address","To Address","Tx Hash",
        #   "Sell From Address","Sell To Address"
        # DuckDB auto-renames duplicate "Cur." columns to Cur., Cur._1,
        # Cur._2 in declaration order — buy / sell / fee respectively.
        cols = set(c[0] for c in conn.execute(
            "DESCRIBE raw").fetchall())
        required = {"Type", "Buy", "Cur.", "Sell", "Cur._1", "Fee",
                    "Cur._2", "Exchange", "Date"}
        missing = required - cols
        if missing:
            raise RuntimeError(
                f"trades.csv for cu_{cu_id} is missing expected "
                f"columns: {sorted(missing)}. Did the export mode "
                f"get set to 'Extended with additional columns'?"
            )

        # Project into the transactions schema. transaction_external_id
        # is synthesized as `cu_<id>:r<row>:<Trade ID or Tx-ID or
        # empty>` — guarantees uniqueness even when CoinTracking's
        # Trade ID isn't (the row-number prefix is deterministic
        # given the CSV's row order, so re-loads produce the same
        # IDs).
        has_trade_id = "Trade ID" in cols
        has_tx_id = "Tx-ID" in cols
        tx_id_expr = (
            f"COALESCE(NULLIF(\"Trade ID\", ''), "
            if has_trade_id else "COALESCE("
        )
        tx_id_expr += (
            f"NULLIF(\"Tx-ID\", ''), 'synth')"
            if has_tx_id else "'synth')"
        )

        n_before = conn.execute(
            "SELECT COUNT(*) FROM transactions").fetchone()[0]
        conn.execute(f"""
            INSERT INTO transactions (
                transaction_external_id,
                portfolio_external_id,
                wallet_external_id,
                snapshot_at,
                occurred_at,
                type,
                buy_amount, buy_currency,
                sell_amount, sell_currency,
                fee_amount, fee_currency,
                comment, lpn, payload
            )
            SELECT
                'cu_{cu_id}:r' || ROW_NUMBER() OVER (ORDER BY "Date") ||
                    ':' || {tx_id_expr} AS transaction_external_id,
                'cu_{cu_id}' AS portfolio_external_id,
                'cu_{cu_id}:' || "Exchange" AS wallet_external_id,
                {snapshot_at} AS snapshot_at,
                CAST("Date" AS TIMESTAMP) AS occurred_at,
                "Type" AS type,
                TRY_CAST(NULLIF("Buy", '') AS DECIMAL(38, 18)),
                NULLIF("Cur.", ''),
                TRY_CAST(NULLIF("Sell", '') AS DECIMAL(38, 18)),
                NULLIF("Cur._1", ''),
                TRY_CAST(NULLIF("Fee", '') AS DECIMAL(38, 18)),
                NULLIF("Cur._2", ''),
                NULLIF("Comment", ''),
                {("NULLIF(\"LPN\", '')" if "LPN" in cols else "NULL")} AS lpn,
                NULL AS payload
            FROM raw
        """)
        loaded = conn.execute(
            "SELECT COUNT(*) FROM transactions").fetchone()[0] - n_before
        log.info("  cu_%s: %d transactions loaded", cu_id, loaded)
        total += loaded

    conn.execute("DROP TABLE IF EXISTS raw")
    return total


def upsert_positions_daily(
    conn: duckdb.DuckDBPyConnection, snapshot_at: int,
) -> tuple[str | None, int]:
    """Incremental upsert: compute the full new positions_daily,
    find the first as_of_date where new differs from existing, and
    rewrite only from that cutoff onwards. Older rows keep their
    original snapshot_at so the gold layer doesn't re-process
    unchanged history.

    Returns (cutoff_date, rows_written). cutoff_date is None when
    new and existing match exactly (no-op load)."""
    # Build the new positions in a temp table.
    conn.execute(
        f"CREATE OR REPLACE TEMP TABLE positions_daily_new AS "
        f"{REPLAY_SQL_TEMPLATE}",
        [snapshot_at],
    )

    # Find the first day where new and existing differ. FULL OUTER
    # JOIN + IS DISTINCT FROM catches all three cases: changed
    # amounts, rows that newly appear, and rows that vanish.
    cutoff = conn.execute("""
        SELECT MIN(as_of_date) FROM (
            SELECT COALESCE(n.as_of_date, o.as_of_date) AS as_of_date
            FROM positions_daily_new n
            FULL OUTER JOIN positions_daily o
              ON  n.as_of_date             = o.as_of_date
              AND n.portfolio_external_id  = o.portfolio_external_id
              AND n.wallet_external_id     = o.wallet_external_id
              AND n.instrument_external_id = o.instrument_external_id
            WHERE n.amount IS DISTINCT FROM o.amount
        )
    """).fetchone()[0]

    if cutoff is None:
        log.info("positions_daily: no changes (load was a no-op)")
        conn.execute("DROP TABLE positions_daily_new")
        return None, 0

    # Rewrite from cutoff onwards. Older rows keep their original
    # snapshot_at — gold won't reprocess them.
    conn.execute(
        "DELETE FROM positions_daily WHERE as_of_date >= ?", [cutoff])
    conn.execute(
        "INSERT INTO positions_daily "
        "SELECT * FROM positions_daily_new WHERE as_of_date >= ?",
        [cutoff])
    rewritten = conn.execute(
        "SELECT COUNT(*) FROM positions_daily WHERE as_of_date >= ?",
        [cutoff]).fetchone()[0]

    log.info("positions_daily: first deviation %s; rewrote %d rows "
             "from there forward (rows before that day kept their "
             "original snapshot_at)", cutoff, rewritten)
    conn.execute("DROP TABLE positions_daily_new")
    return str(cutoff), rewritten


def reconcile_balances(
    conn: duckdb.DuckDBPyConnection,
    manifest: dict, run_dir: Path,
) -> None:
    """Compare each portfolio's computed final balance (from
    positions_daily) against the balance.csv from
    /balance_by_exchange.php. Discrepancies bigger than the
    8-decimal export precision are logged as warnings — not
    failures.

    CoinTracking's UI filters zero-balance entries from
    /balance_by_exchange.php; our replay retains them. So:
      - rows in both → must agree within tolerance
      - rows only in CT → coverage gap (warn)
      - rows only in mine with abs(amount) > tol → unreported
        position (warn); zero rows only in mine are fine."""
    for portfolio in manifest["portfolios"]:
        cu_id = portfolio["id"]
        balance_csv = run_dir / f"cu_{cu_id}" / "balance.csv"
        if not balance_csv.is_file():
            log.warning("cu_%s: no balance.csv to reconcile against", cu_id)
            continue

        # Full outer join in SQL; status column classifies each pair.
        rows = conn.execute("""
            WITH mine AS (
                SELECT
                    -- Strip the 'cu_<id>:' prefix to match CT's
                    -- bare Exchange column.
                    SUBSTR(wallet_external_id,
                           STRPOS(wallet_external_id, ':') + 1) AS wallet,
                    instrument_external_id AS instrument,
                    amount,
                    ROW_NUMBER() OVER (
                        PARTITION BY wallet_external_id,
                                     instrument_external_id
                        ORDER BY as_of_date DESC
                    ) AS rn
                FROM positions_daily
                WHERE portfolio_external_id = ?
            ),
            mine_latest AS (
                SELECT wallet, instrument, amount FROM mine WHERE rn = 1
            ),
            ct AS (
                SELECT
                    "Exchange" AS wallet,
                    "Currency" AS instrument,
                    CAST("Amount" AS DECIMAL(38, 18)) AS amount
                FROM read_csv_auto(?, header=true, all_varchar=true)
            )
            SELECT
                COALESCE(m.wallet, c.wallet) AS wallet,
                COALESCE(m.instrument, c.instrument) AS instrument,
                m.amount AS mine_amount,
                c.amount AS ct_amount,
                CASE
                    WHEN m.amount IS NULL THEN 'only_ct'
                    WHEN c.amount IS NULL THEN 'only_mine'
                    WHEN ABS(m.amount - c.amount) >
                         CAST(? AS DECIMAL(38, 18)) THEN 'mismatch'
                    ELSE 'match'
                END AS status
            FROM mine_latest m
            FULL OUTER JOIN ct c
              ON m.wallet = c.wallet
             AND m.instrument = c.instrument
        """, [f"cu_{cu_id}", str(balance_csv),
              RECONCILE_ABS_TOL]).fetchall()

        if not rows:
            log.info("cu_%s: no reconciliation data (balance.csv may "
                     "be empty)", cu_id)
            continue

        from decimal import Decimal
        tol = Decimal(RECONCILE_ABS_TOL)
        n_match = 0
        n_only_mine_zero = 0
        only_ct: list[tuple] = []
        only_mine_nonzero: list[tuple] = []
        mismatch: list[tuple] = []
        for wallet, instrument, mine_v, ct_v, status in rows:
            if status == "match":
                n_match += 1
            elif status == "only_ct":
                only_ct.append((wallet, instrument, ct_v))
            elif status == "only_mine":
                amt = Decimal(str(mine_v)) if mine_v is not None else Decimal(0)
                if abs(amt) > tol:
                    only_mine_nonzero.append((wallet, instrument, mine_v))
                else:
                    n_only_mine_zero += 1
            elif status == "mismatch":
                mismatch.append((wallet, instrument, mine_v, ct_v))

        problems = len(only_ct) + len(only_mine_nonzero) + len(mismatch)
        if problems == 0:
            log.info("cu_%s: reconciliation OK — %d pair(s) match, "
                     "%d zero-balance pair(s) only in replay (CT "
                     "UI filters those)",
                     cu_id, n_match, n_only_mine_zero)
            continue

        # Issues exist — log each one so the operator has actionable
        # info (wallet, instrument, amounts). These values are
        # operator-private; never copy a log line into a tracked
        # file or shared dump.
        log.warning(
            "cu_%s: reconciliation issues — %d match, %d zero-"
            "only-mine, %d mismatch beyond ±%s, %d CT-has-I-don't, "
            "%d non-zero-only-mine",
            cu_id, n_match, n_only_mine_zero, len(mismatch), tol,
            len(only_ct), len(only_mine_nonzero),
        )
        for wallet, instrument, mine_v, ct_v in mismatch:
            diff = abs(Decimal(str(mine_v)) - Decimal(str(ct_v)))
            log.warning("  mismatch: wallet=%s instrument=%s "
                        "replay=%s ct=%s diff=%s",
                        wallet, instrument, mine_v, ct_v, diff)
        for wallet, instrument, ct_v in only_ct:
            log.warning("  CT-only: wallet=%s instrument=%s "
                        "ct_amount=%s (replay produces nothing)",
                        wallet, instrument, ct_v)
        for wallet, instrument, mine_v in only_mine_nonzero:
            log.warning("  non-zero replay-only: wallet=%s "
                        "instrument=%s replay_amount=%s "
                        "(CT balance.csv has no entry)",
                        wallet, instrument, mine_v)


def process_snapshot(
    conn: duckdb.DuckDBPyConnection, run_dir: Path, force: bool,
) -> None:
    """Ingest one bronze snapshot dir into silver."""
    snapshot_at = parse_run_ts(run_dir)
    log.info("snapshot %s (snapshot_at=%d)", run_dir.name, snapshot_at)

    # Idempotency gate.
    already = conn.execute(
        "SELECT 1 FROM dump_runs WHERE snapshot_at = ?",
        [snapshot_at]).fetchone()
    if already and not force:
        log.info("  already loaded; skipping (use --force to re-load)")
        return

    manifest = json.loads((run_dir / "run.json").read_text())

    n_tx = ingest_transactions(conn, manifest, run_dir, snapshot_at)
    log.info("  total transactions loaded: %d", n_tx)

    ingest_portfolios_and_wallets(conn, manifest, snapshot_at)

    cutoff, n_rewritten = upsert_positions_daily(conn, snapshot_at)

    reconcile_balances(conn, manifest, run_dir)

    # Record the snapshot. Replace existing on --force.
    conn.execute(
        "INSERT INTO dump_runs (snapshot_at, silver_schema_version, "
        "run_dir, payload) VALUES (?, ?, ?, ?) "
        "ON CONFLICT (snapshot_at) DO UPDATE SET "
        "  silver_schema_version = excluded.silver_schema_version, "
        "  run_dir               = excluded.run_dir, "
        "  payload               = excluded.payload",
        [snapshot_at, 1, str(run_dir),
         json.dumps({"cutoff": cutoff, "rewritten": n_rewritten})],
    )


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    args.silver_db.parent.mkdir(parents=True, exist_ok=True)
    conn = duckdb.connect(str(args.silver_db))
    try:
        version = apply_migrations(conn)
        log.info("silver schema at version %d (%s)",
                 version, args.silver_db)

        if args.replay_only:
            tx_count = conn.execute(
                "SELECT COUNT(*) FROM transactions").fetchone()[0]
            if tx_count == 0:
                log.info("0 transactions in silver — nothing to replay")
                return 0
            snapshot_at = conn.execute(
                "SELECT COALESCE(MAX(snapshot_at), 0) FROM dump_runs"
            ).fetchone()[0]
            upsert_positions_daily(conn, snapshot_at)
            return 0

        snapshots = discover_bronze_snapshots(args.bronze_dir)
        if not snapshots:
            log.info("no bronze snapshots under %s; nothing to do",
                     args.bronze_dir)
            return 0

        log.info("found %d bronze snapshot(s) to consider",
                 len(snapshots))
        for snap in snapshots:
            try:
                process_snapshot(conn, snap, args.force)
            except Exception as exc:
                log.error("snapshot %s failed: %s", snap.name, exc)
                # Continue with the next snapshot — partial progress
                # is better than a full rollback.
                continue

        log.info("done")
    finally:
        conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
