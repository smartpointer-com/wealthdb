#!/usr/bin/env python3
"""cointracking silver loader.

Reads the bronze tree under --bronze-dir (one snapshot dir per
download run, each containing run.json + cu_<id>/trades.csv.zst +
cu_<id>/balance.csv.zst — plain .csv in pre-compression dumps; both
forms resolve, and DuckDB decompresses .csv.zst natively inside
read_csv_auto) and ingests into the DuckDB silver layer:

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
import os
import re
import shutil
import sys
import time
from datetime import date, datetime, timezone
from pathlib import Path

import duckdb

from collectorkit import bronze, cli, compress, silver

from binance import (
    BinanceClient, build_mapping, get_api_key, PROVIDER as PRICE_PROVIDER,
)
from frankfurter import (
    FrankfurterClient, SUPPORTED_FIATS,
    PROVIDER as FX_PROVIDER,
)

log = logging.getLogger("cointracking.load")

MIGRATIONS_DIR = Path(__file__).resolve().parent / "migrations"

# Reconciliation tolerance. CoinTracking exports amounts truncated
# at 8 decimals, so a per-row discrepancy below this means rendering
# imprecision rather than a real balance mismatch.
RECONCILE_ABS_TOL = "0.00000001"

# overview.csv coin column pair pattern. CoinTracking's wide-form
# header has "<SYM> Value in <FIAT>" + "<SYM> Amount" per coin held;
# aggregate columns are "Currencies Total Value in <FIAT>",
# "Coins Total Value in <FIAT>", "Account Total Value in <FIAT>" —
# the multi-word "Total" form is naturally excluded by [A-Z0-9_]+
# (no spaces).
COIN_VALUE_HEADER_RE = re.compile(r'^([A-Z0-9_]+) Value in ([A-Z]+)$')


# Type handler vocabulary — the single source of truth for BOTH the
# replay below AND the unhandled-type guard (warn_unhandled_
# transaction_types). CoinTracking has accumulated a ~19-type
# vocabulary over time; each row is routed by `type` to a buy leg
# (`+buy_amount` on `buy_currency`), a sell leg (`-sell_amount` on
# `sell_currency`), or — for the direction-ambiguous types (Trade,
# Gift / Tip, Gift) — both, with the `buy_amount IS NOT NULL` /
# `sell_amount IS NOT NULL` filter selecting the one populated leg.
#
# A `type` on NEITHER list contributes nothing, silently: an outgoing
# type we forget to list leaves the replay's per-wallet balance too
# high (the coins that left are never subtracted); an incoming one
# leaves it too low. `Other Expense` is CoinTracking's generic
# outgoing-balance type — observed as the sell leg of an exchange
# dust sweep (a periodic conversion of tiny leftover balances, which
# CoinTracking labels "Dust Sweeping" in the Comment column), where
# each swept dust balance is booked as an `Other Expense` sell paired
# with an `Income (non taxable)` buy of the consolidated proceeds.
# That pairing is what first exposed the gap: the buy leg landed while
# the dust never left, stranding each source balance at exactly the
# swept amount. The lists are kept as data so the guard can warn on
# any unrouted leg rather than drop it.
#
# Fee semantics: `fee_amount` on regular rows is informative only;
# `Other Fee` and `Other Expense` rows ARE balance deltas (the fee /
# expense IS the event). See DESIGN.md.
BUY_TYPES = (
    "Trade", "Deposit", "Staking", "Reward / Bonus",
    "Income", "Income (non taxable)",
    "Airdrop", "Airdrop (non taxable)",
    "Gift / Tip", "Gift",
)
SELL_TYPES = (
    "Trade", "Withdrawal", "Other Fee", "Other Expense",
    "Lost", "Stolen", "Spend", "Donation",
    "Gift / Tip", "Gift", "Expense (non taxable)",
)


def _sql_str_list(values: tuple[str, ...]) -> str:
    """Render string constants as a SQL IN-list body: ``'a', 'b'``.
    These are internal constants, never user input; a stray apostrophe
    would be a source-level typo, so we assert rather than escape."""
    for v in values:
        assert "'" not in v, f"type literal must not contain a quote: {v!r}"
    return ", ".join(f"'{v}'" for v in values)


# Aggregate-then-window replay against the `transactions` table.
# The trailing bound `?` parameter is the snapshot_at stamped on
# newly-written rows. The two `{…}` slots are filled once, at import,
# from BUY_TYPES / SELL_TYPES — the SQL body carries no other braces.
REPLAY_SQL_TEMPLATE = f"""
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
      AND type IN ({_sql_str_list(BUY_TYPES)})
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
      AND type IN ({_sql_str_list(SELL_TYPES)})
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
              "subdirs containing run.json + "
              "cu_<id>/{trades,balance,overview}.csv[.zst]. "
              "Default: %(default)s."),
    )
    p.add_argument(
        "--scratch-dir", type=Path, default=None,
        help=("Directory for a working copy of the silver DB. When set, "
              "an existing silver DB is copied here first, the load runs "
              "against the copy, and the finished DB is moved back onto "
              "--silver-db on success. In the container this keeps "
              "DuckDB's per-statement writes off the slow VirtioFS bind "
              "mount — the silver DB reaches it in one bulk move at the "
              "end. Default: open --silver-db in place."),
    )
    p.add_argument(
        "--replay-only", action="store_true",
        help=("Skip bronze ingest; just re-run the holdings replay "
              "against whatever's already in `transactions`. Useful "
              "for iterating on the type-handler rules."),
    )
    p.add_argument(
        "--no-fetch-prices", dest="fetch_prices", action="store_false",
        help=("Skip the post-ingest USD price fill. By DEFAULT, after "
              "ingest, missing USD prices are fetched from the active price "
              "provider (Binance, USDT-denominated) for every coin held in "
              "any portfolio on every day, inserted into coin_prices with ON "
              "CONFLICT DO NOTHING (already-fetched dates untouched); the "
              "previous run's latest priced day is always re-fetched (an "
              "intraday snapshot upgraded to the close). Equivalent to "
              "running `fetch-prices --missing` immediately after load. Pass "
              "this to skip the price fill (e.g. an offline reload)."),
    )
    cli.add_standard_args(p, verb="load")
    return p.parse_args(argv)


def apply_migrations(conn: duckdb.DuckDBPyConnection) -> int:
    for sql_file in sorted(MIGRATIONS_DIR.glob("*.sql")):
        log.debug("applying %s", sql_file.name)
        conn.execute(sql_file.read_text())
    return conn.execute(
        "SELECT MAX(silver_schema_version) FROM schema_meta"
    ).fetchone()[0]


# run.json status values that mark a run dir as NOT a finished dump.
# "in-progress" is left by a crashed walk; "dry-run" is defensive
# (download --dry-run materialises no run dir, so the value is never
# actually written). A run whose status is any of these is kept out of
# the silver load so partial captures never reach gold; `prune`
# reclaims such dirs.
NON_COMPLETE_STATUSES = ("in-progress", "dry-run")


def discover_bronze_snapshots(bronze_dir: Path) -> list[Path]:
    """Return the timestamped subdirs of bronze_dir that hold a
    completed run.json (chronological). A run.json whose ``status`` is
    ``"in-progress"`` (a crashed walk) or ``"dry-run"`` is skipped so a
    partial dump never reaches silver; a statusless manifest (a dump
    predating the status lifecycle) stays loadable."""
    if not bronze_dir.is_dir():
        return []
    snapshots = []
    for p in sorted(bronze_dir.iterdir()):
        if not p.is_dir():
            continue
        if not bronze.RUN_DIR_RE.match(p.name):
            continue
        run_json = p / "run.json"
        if not run_json.is_file():
            continue
        if bronze.run_status(run_json) in NON_COMPLETE_STATUSES:
            log.info("skipping %s (run.json status not complete)", p.name)
            continue
        snapshots.append(p)
    return snapshots


def parse_run_ts(run_dir: Path) -> int:
    """Parse a `YYYYMMDDTHHMMSSZ` run-dir into a UTC unix timestamp
    (thin Path-adapter over `bronze.parse_run_ts`)."""
    return bronze.parse_run_ts(run_dir.name)


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


def _bronze_csv(run_dir: Path, cu_id: str, kind: str) -> Path | None:
    """On-disk path of a portfolio's `<kind>.csv`, resolving the
    compressed form download writes (`.csv.zst`; plain `.csv` in
    pre-compression runs or after a compression fallback; `.csv.gz`
    for completeness). DuckDB's read_csv_auto decompresses by file
    extension, so the resolved path feeds straight into the staging
    queries. None when no variant exists."""
    return compress.resolve_variant(run_dir / f"cu_{cu_id}" / f"{kind}.csv")


def ingest_transactions(
    conn: duckdb.DuckDBPyConnection,
    manifest: dict, run_dir: Path, snapshot_at: int,
) -> int:
    """Replace `transactions` for the portfolios present in this
    snapshot and re-populate from each one's trades.csv. Returns
    the row count written. Each portfolio's trades.csv is a
    complete dump (CT replays the full trade history on every
    export), so per-portfolio truncate-and-reinsert is safe.
    Portfolios missing from this snapshot keep whatever they were
    last loaded with — relevant when CT's flaky linked-user
    discovery drops one off a refresh."""
    portfolio_ids = [f"cu_{p['id']}" for p in manifest["portfolios"]]
    if portfolio_ids:
        placeholders = ", ".join(["?"] * len(portfolio_ids))
        conn.execute(
            f"DELETE FROM transactions WHERE portfolio_external_id IN ({placeholders})",
            portfolio_ids,
        )

    total = 0
    for portfolio in manifest["portfolios"]:
        cu_id = portfolio["id"]
        trades_csv = _bronze_csv(run_dir, cu_id, "trades")
        if trades_csv is None:
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
            "COALESCE(NULLIF(\"Trade ID\", ''), "
            if has_trade_id else "COALESCE("
        )
        tx_id_expr += (
            "NULLIF(\"Tx-ID\", ''), 'synth')"
            if has_tx_id else "'synth')"
        )
        lpn_expr = "NULLIF(\"LPN\", '')" if "LPN" in cols else "NULL"

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
                {lpn_expr} AS lpn,
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


def ingest_portfolio_prices(
    conn: duckdb.DuckDBPyConnection,
    manifest: dict, run_dir: Path, snapshot_at: int,
) -> None:
    """Parse each portfolio's overview.csv (one row per day, wide-form
    pairs of `<SYM> Value in <FIAT>` + `<SYM> Amount` per held coin)
    into portfolio_prices. Price = value / amount; rows where amount
    is 0/empty are dropped (the division-by-zero guard).

    The quote currency varies between portfolios (CT's per-portfolio
    "main fiat" setting — EUR for some users/portfolios, USD for
    others) and is extracted from each column header at parse time.

    The whole overview lands in a single insert per portfolio: the
    wide coin pairs are folded into one (date, sym, fiat, value,
    amount) stream, so the price division / filters and the conflict
    resolution run once for the portfolio rather than once per coin
    column (the former shape issued a separate insert per pair, each
    re-offering the full history to the growing conflict index).

    A NOT EXISTS anti-join drops rows whose price key already exists
    before the insert runs; the ON CONFLICT DO NOTHING clause backs
    it up. Market prices don't change retroactively, so re-loading a
    fresh bronze snapshot adds new dates without touching the old
    rows' snapshot_at — gold doesn't reprocess unchanged history, and
    the anti-join spares the write path from re-offering ~all of an
    already-loaded history to the conflict index. Corruption-recovery
    re-fetch goes through fetch-prices, not load."""
    for portfolio in manifest["portfolios"]:
        cu_id = portfolio["id"]
        portfolio_id = f"cu_{cu_id}"
        overview_csv = _bronze_csv(run_dir, cu_id, "overview")
        if overview_csv is None:
            log.warning("cu_%s: no overview.csv to ingest", cu_id)
            continue

        # Stage the CSV (all_varchar=true so empty cells parse cleanly
        # and we control numeric promotion).
        conn.execute("""
            CREATE OR REPLACE TEMP TABLE raw_overview AS
            SELECT * FROM read_csv_auto(?, header=true, all_varchar=true)
        """, [str(overview_csv)])

        # Header → coin-column-pair list. The regex filter
        # (uppercase symbol + uppercase fiat, no spaces) keeps
        # only the coin column pairs and naturally excludes the
        # multi-word aggregate columns ("Currencies Total Value
        # in <fiat>", etc.) plus the Date column.
        cols = [c[0] for c in conn.execute("DESCRIBE raw_overview").fetchall()]
        pairs: list[tuple[str, str]] = []  # [(sym, fiat)]
        for c in cols:
            m = COIN_VALUE_HEADER_RE.match(c)
            if m:
                sym, fiat = m.group(1), m.group(2)
                amount_col = f"{sym} Amount"
                if amount_col in cols:
                    pairs.append((sym, fiat))

        if not pairs:
            log.warning("cu_%s: overview.csv has no recognisable coin "
                        "column pairs (%d cols total)", cu_id, len(cols))
            continue

        # Fold the wide pairs into one long stream with a UNION ALL
        # branch per coin column pair. sym/fiat are bound as query
        # parameters; the value/amount column identifiers are
        # interpolated, but COIN_VALUE_HEADER_RE constrains them to
        # [A-Z0-9_]+ / [A-Z]+ (no quotes, no spaces), so they can't
        # break out of the double-quoted identifier.
        branches: list[str] = []
        params: dict[str, object] = {
            "portfolio": portfolio_id, "snap": snapshot_at,
        }
        for i, (sym, fiat) in enumerate(pairs):
            value_col = f"{sym} Value in {fiat}"
            amount_col = f"{sym} Amount"
            branches.append(
                f'SELECT "Date" AS d, $sym{i} AS sym, $fiat{i} AS fiat, '
                f'"{value_col}" AS value_raw, "{amount_col}" AS amount_raw '
                f'FROM raw_overview'
            )
            params[f"sym{i}"] = sym
            params[f"fiat{i}"] = fiat
        union_sql = "\n                UNION ALL\n                ".join(branches)

        # The Date column has one special value: "now" — a current-
        # moment snapshot above the close-of-day rows; drop it. The
        # TRY_CAST + amount > 0 filter is the division-by-zero guard:
        # a row only yields a price when the holding was non-zero that
        # day. CoinTracking emits the Date in a per-portfolio locale
        # (`YYYY/MM/DD` for some, `DD.MM.YYYY` for others); the
        # COALESCE over TRY_STRPTIME parses either without knowing
        # which a given portfolio uses.
        conn.execute(f"""
            INSERT INTO portfolio_prices (
                as_of_date, portfolio_external_id,
                instrument_external_id, quote_currency,
                price, snapshot_at
            )
            WITH staged AS (
                SELECT
                    COALESCE(
                        TRY_STRPTIME(d, '%Y/%m/%d')::DATE,
                        TRY_STRPTIME(d, '%d.%m.%Y')::DATE
                    ) AS as_of_date,
                    sym, fiat,
                    TRY_CAST(value_raw  AS DECIMAL(38, 18)) AS value_dec,
                    TRY_CAST(amount_raw AS DECIMAL(38, 18)) AS amount_dec
                FROM (
                    {union_sql}
                )
                WHERE d != 'now'
            )
            SELECT
                s.as_of_date,
                $portfolio AS portfolio_external_id,
                s.sym      AS instrument_external_id,
                s.fiat     AS quote_currency,
                s.value_dec / s.amount_dec AS price,
                $snap      AS snapshot_at
            FROM staged s
            WHERE s.amount_dec IS NOT NULL
              AND s.amount_dec > 0
              AND s.value_dec IS NOT NULL
              AND NOT EXISTS (
                  SELECT 1 FROM portfolio_prices pp
                  WHERE pp.portfolio_external_id  = $portfolio
                    AND pp.as_of_date             = s.as_of_date
                    AND pp.instrument_external_id = s.sym
                    AND pp.quote_currency         = s.fiat
              )
            ON CONFLICT DO NOTHING
        """, params)

        # Diagnostic: log the (coin, fiat) pair count + total
        # portfolio_prices for this portfolio.
        n_rows_for_portfolio = conn.execute(
            "SELECT COUNT(*) FROM portfolio_prices "
            "WHERE portfolio_external_id = ?",
            [portfolio_id]
        ).fetchone()[0]
        # Distinct fiats observed — if more than 1, the CT
        # main-fiat preference was likely changed at some point
        # and we'd want to flag it (the gold layer needs to know).
        fiats = sorted({fiat for _, fiat in pairs})
        log.info("cu_%s: portfolio_prices %d row(s) total; %d coin "
                 "pair(s) ingested; quote=%s",
                 cu_id, n_rows_for_portfolio, len(pairs),
                 fiats[0] if len(fiats) == 1 else fiats)

    conn.execute("DROP TABLE IF EXISTS raw_overview")


def warn_unhandled_transaction_types(
    conn: duckdb.DuckDBPyConnection,
) -> list[tuple[str, int, int]]:
    """Warn (loudly) about any transaction whose populated leg the
    replay silently drops because its `type` is on neither BUY_TYPES
    nor SELL_TYPES.

    The replay routes rows by `type`; a leg whose type is unlisted
    contributes nothing to positions_daily, so the affected wallet's
    balance reads high (a dropped sell) or low (a dropped buy) with no
    error raised — exactly how the `Other Expense` dust-sweep leg
    slipped through before it was added to SELL_TYPES. CoinTracking
    keeps growing its vocabulary (margin, lending, derivatives, …), so
    this is a standing guard against the next unlisted type, not a
    one-off. Returns the (type, dropped_buy_legs, dropped_sell_legs)
    rows it warned on — empty when coverage is complete."""
    rows = conn.execute(f"""
        SELECT
            type,
            COUNT(*) FILTER (
                WHERE buy_amount IS NOT NULL AND buy_currency IS NOT NULL
                  AND type NOT IN ({_sql_str_list(BUY_TYPES)})
            ) AS dropped_buy_legs,
            COUNT(*) FILTER (
                WHERE sell_amount IS NOT NULL AND sell_currency IS NOT NULL
                  AND type NOT IN ({_sql_str_list(SELL_TYPES)})
            ) AS dropped_sell_legs
        FROM transactions
        GROUP BY type
        HAVING dropped_buy_legs > 0 OR dropped_sell_legs > 0
        ORDER BY type
    """).fetchall()
    for typ, n_buy, n_sell in rows:
        log.warning(
            "unrouted transaction type %r: %d buy leg(s) + %d sell "
            "leg(s) dropped from the replay — affected wallet balances "
            "will read high/low until a handler is added to "
            "BUY_TYPES / SELL_TYPES in load.py", typ, n_buy, n_sell)
    return rows


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
        balance_csv = _bronze_csv(run_dir, cu_id, "balance")
        if balance_csv is None:
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

        # Issues exist — log each one with wallet + instrument +
        # amounts so a human reviewer can act on them. These values
        # are private to the deployment; never copy a log line into
        # a tracked file or shared dump.
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


def ensure_coin_mapping(
    conn: duckdb.DuckDBPyConnection,
    client: BinanceClient,
) -> dict[str, str]:
    """Make sure coin_mapping covers every instrument that appears
    in positions_daily with a non-zero amount. Returns the
    {ticker: provider_coin_id} dict for the held coin set; entries
    without a provider match are omitted (logged in build_mapping).

    The mapping is cached in silver.coin_mapping so we don't hit
    the provider's master-list endpoint on every fetch-prices run.
    Updated lazily — only when a new ticker shows up that isn't
    already cached for the active provider."""
    held = [r[0] for r in conn.execute(
        "SELECT DISTINCT instrument_external_id FROM positions_daily "
        "WHERE amount > 0 ORDER BY 1"
    ).fetchall()]

    cached: dict[str, str | None] = {}
    for sym, cid in conn.execute(
        "SELECT instrument_external_id, provider_coin_id "
        "FROM coin_mapping WHERE provider = ?",
        [PRICE_PROVIDER],
    ).fetchall():
        cached[sym] = cid

    missing = [s for s in held if s not in cached]
    if missing:
        log.info("looking up %s ids for %d new ticker(s)",
                 PRICE_PROVIDER, len(missing))
        fresh = build_mapping(client, missing)
        now_ts = int(time.time())
        for sym in missing:
            cid = fresh.get(sym)  # None if no match (or fiat)
            conn.execute(
                """INSERT INTO coin_mapping
                   (instrument_external_id, provider, provider_coin_id,
                    payload, mapped_at)
                   VALUES (?, ?, ?, NULL, ?)""",
                [sym, PRICE_PROVIDER, cid, now_ts],
            )
            cached[sym] = cid

    return {s: cid for s, cid in cached.items() if cid and s in held}


def coin_date_range(
    conn: duckdb.DuckDBPyConnection, sym: str,
) -> tuple[date, date] | None:
    """Return (min, max) as_of_date in positions_daily where the
    user held `sym` (amount > 0). None if the coin was never held."""
    row = conn.execute(
        "SELECT MIN(as_of_date), MAX(as_of_date) FROM positions_daily "
        "WHERE instrument_external_id = ? AND amount > 0",
        [sym],
    ).fetchone()
    if not row or row[0] is None:
        return None
    return row[0], row[1]


def fetch_coin_prices(
    conn: duckdb.DuckDBPyConnection,
    client: BinanceClient,
    mode: str,
) -> tuple[int, int]:
    """Drive the price-provider fetch loop. `mode` is one of:

      - 'missing': fetch (held, unpriced) gaps per coin. Always
        re-fetches the latest priced day too — the price stored
        for "today" on any previous run was an INTRADAY snapshot,
        not a close, so the next run upgrades it to the close
        price. Used by `load` (price fill is default; --no-fetch-prices
        to skip) and `fetch-prices --missing`.
      - 'full':    drop every coin_prices row sourced from the
        active provider, then re-fetch the full held range per
        coin. Used by `fetch-prices` (no flag) for corruption
        recovery (the provider occasionally returns bad values
        for individual days).

    Returns (coins_fetched, prices_written)."""
    if mode not in ("missing", "full"):
        raise ValueError(f"unknown fetch mode: {mode!r}")

    mapping = ensure_coin_mapping(conn, client)
    if not mapping:
        log.info("no held coins have a %s mapping; nothing to fetch",
                 PRICE_PROVIDER)
        return 0, 0

    if mode == "full":
        log.warning("fetch-prices full re-fetch: TRUNCATE coin_prices "
                    "for provider=%s (corruption-recovery mode)",
                    PRICE_PROVIDER)
        conn.execute(
            "DELETE FROM coin_prices WHERE source = ?",
            [PRICE_PROVIDER])

    coins_fetched = 0
    prices_written = 0
    for sym in sorted(mapping.keys()):
        coin_id = mapping[sym]
        held_range = coin_date_range(conn, sym)
        if not held_range:
            continue
        min_held, max_held = held_range

        if mode == "missing":
            # Earliest day in the held range with no price row yet.
            first_unpriced = conn.execute("""
                SELECT MIN(pd.as_of_date)
                FROM positions_daily pd
                LEFT JOIN coin_prices cp
                  ON cp.instrument_external_id = pd.instrument_external_id
                 AND cp.as_of_date = pd.as_of_date
                 AND cp.source = ?
                WHERE pd.instrument_external_id = ?
                  AND pd.amount > 0
                  AND cp.as_of_date IS NULL
            """, [PRICE_PROVIDER, sym]).fetchone()[0]
            # Latest priced day — this row was an intraday/live
            # snapshot when first written; it needs to be re-fetched
            # so the next run upgrades it to the close.
            latest_priced = conn.execute("""
                SELECT MAX(as_of_date) FROM coin_prices
                WHERE instrument_external_id = ?
                  AND source = ?
            """, [sym, PRICE_PROVIDER]).fetchone()[0]
            # If neither exists, this coin isn't in the held set;
            # skip (defensive — coin_date_range already caught it).
            candidates = [d for d in (first_unpriced, latest_priced)
                          if d is not None]
            if not candidates:
                continue
            fetch_from = min(candidates)
            # Drop the latest priced day so the INSERT below
            # actually writes the close price (ON CONFLICT DO
            # NOTHING would otherwise preserve the stale-live row).
            if latest_priced is not None:
                conn.execute(
                    """DELETE FROM coin_prices
                       WHERE instrument_external_id = ?
                         AND source = ?
                         AND as_of_date = ?""",
                    [sym, PRICE_PROVIDER, latest_priced],
                )
            fetch_to = max_held
        else:  # full
            fetch_from = min_held
            fetch_to = max_held

        # Convert to unix seconds, 00:00 UTC. Pad by 1 day on each
        # side so day-boundary rounding doesn't lose anything.
        from_ts = int(datetime.combine(
            fetch_from, datetime.min.time(), tzinfo=timezone.utc
        ).timestamp()) - 86400
        to_ts = int(datetime.combine(
            fetch_to, datetime.min.time(), tzinfo=timezone.utc
        ).timestamp()) + 86400

        log.info("%s (%s): fetching %s..%s",
                 sym, coin_id, fetch_from, fetch_to)
        try:
            rows = client.ohlcv_historical(coin_id, from_ts, to_ts)
        except Exception as exc:
            log.error("%s (%s): fetch failed: %s — skipping coin",
                      sym, coin_id, exc)
            continue

        coins_fetched += 1
        fetched_at = int(time.time())
        # ON CONFLICT DO NOTHING for 'missing' — preserves existing
        # rows. For 'full' we already truncated above, so conflict
        # shouldn't fire, but DO NOTHING is the safer fallback.
        n = 0
        for day, price in rows:
            if not (min_held <= day <= max_held):
                continue  # only days the position was actually held
            conn.execute(
                """INSERT INTO coin_prices
                   (as_of_date, instrument_external_id, price_usd,
                    source, fetched_at)
                   VALUES (?, ?, ?, ?, ?)
                   ON CONFLICT DO NOTHING""",
                [day, sym, price, PRICE_PROVIDER, fetched_at],
            )
            n += 1
        prices_written += n
        log.info("  %s: wrote %d daily price(s)", sym, n)

    return coins_fetched, prices_written


def backfill_first_day_gaps(
    conn: duckdb.DuckDBPyConnection,
    max_gap_days: int = 365,
) -> int:
    """For (instrument, day) pairs where positions_daily has amount
    > 0 but coin_prices has no row, AND coin_prices has a later
    row for the same instrument within `max_gap_days`, insert a
    backfilled row using the earliest-later price.

    Use case: first-day-held edge cases where Binance's USDT pair
    was listed AFTER a portfolio's first purchase of that coin.
    The price won't be exact for the gap day but is a sound
    approximation when the listing gap is short relative to the
    coin's price stability.

    Source tag stays the same as the data we're extending
    ('binance'); from the gold layer's POV the row is
    indistinguishable from a real Binance kline.

    Returns number of rows backfilled."""
    fetched_at = int(time.time())
    conn.execute(f"""
        WITH gaps AS (
            SELECT DISTINCT
                pd.instrument_external_id AS instr,
                pd.as_of_date AS gap_date
            FROM positions_daily pd
            LEFT JOIN coin_prices cp
              ON cp.instrument_external_id = pd.instrument_external_id
             AND cp.as_of_date = pd.as_of_date
            WHERE pd.amount > 0
              AND cp.as_of_date IS NULL
        ),
        first_later AS (
            SELECT g.instr, g.gap_date,
                   MIN(cp.as_of_date) AS next_priced
            FROM gaps g
            JOIN coin_prices cp
              ON cp.instrument_external_id = g.instr
             AND cp.as_of_date > g.gap_date
            GROUP BY 1, 2
        )
        INSERT INTO coin_prices (
            as_of_date, instrument_external_id, price_usd,
            source, fetched_at
        )
        SELECT
            fl.gap_date AS as_of_date,
            fl.instr    AS instrument_external_id,
            cp.price_usd,
            cp.source,
            {fetched_at} AS fetched_at
        FROM first_later fl
        JOIN coin_prices cp
          ON cp.instrument_external_id = fl.instr
         AND cp.as_of_date = fl.next_priced
        WHERE DATEDIFF('day', fl.gap_date, fl.next_priced) <= {max_gap_days}
        ON CONFLICT DO NOTHING
    """).fetchall()
    # DuckDB's INSERT reports no rowcount; count the rows stamped
    # with this fetched_at instead.
    n = conn.execute("""
        SELECT COUNT(*)
        FROM coin_prices
        WHERE fetched_at = ?
    """, [fetched_at]).fetchone()[0]
    if n:
        log.info("backfill: %d first-day gap(s) filled within %d-day "
                 "window", n, max_gap_days)
    return n


def fetch_fx_rates(
    conn: duckdb.DuckDBPyConnection,
    client: FrankfurterClient,
    mode: str,
) -> tuple[int, int]:
    """Fill USD-equivalent prices for any FIAT cash balance held in
    positions_daily — EUR, CHF, etc. Inserted into coin_prices with
    source='frankfurter'; same conflict-resolution semantics as
    the crypto fetcher (missing = ON CONFLICT DO NOTHING +
    re-fetch latest; full = TRUNCATE + re-insert).

    ECB doesn't publish weekend rates; the client forward-fills
    across weekends and ECB holidays (standard FX-rate
    convention), so every held day carries a rate.

    Returns (fiats_fetched, prices_written)."""
    if mode not in ("missing", "full"):
        raise ValueError(f"unknown fetch mode: {mode!r}")

    # Which fiat tickers actually appear in positions_daily?
    held_fiats = [
        r[0] for r in conn.execute("""
            SELECT DISTINCT instrument_external_id
            FROM positions_daily
            WHERE amount > 0
              AND instrument_external_id IN (
                  SELECT UNNEST(?::VARCHAR[])
              )
            ORDER BY 1
        """, [sorted(SUPPORTED_FIATS)]).fetchall()
    ]
    if not held_fiats:
        return 0, 0

    if mode == "full":
        log.warning("fetch FX full re-fetch: TRUNCATE coin_prices "
                    "for provider=%s", FX_PROVIDER)
        conn.execute(
            "DELETE FROM coin_prices WHERE source = ?", [FX_PROVIDER])

    fiats_fetched = 0
    prices_written = 0
    for fiat in held_fiats:
        held_range = coin_date_range(conn, fiat)
        if not held_range:
            continue
        min_held, max_held = held_range

        if mode == "missing":
            first_unpriced = conn.execute("""
                SELECT MIN(pd.as_of_date)
                FROM positions_daily pd
                LEFT JOIN coin_prices cp
                  ON cp.instrument_external_id = pd.instrument_external_id
                 AND cp.as_of_date = pd.as_of_date
                 AND cp.source = ?
                WHERE pd.instrument_external_id = ?
                  AND pd.amount > 0
                  AND cp.as_of_date IS NULL
            """, [FX_PROVIDER, fiat]).fetchone()[0]
            latest_priced = conn.execute("""
                SELECT MAX(as_of_date) FROM coin_prices
                WHERE instrument_external_id = ? AND source = ?
            """, [fiat, FX_PROVIDER]).fetchone()[0]
            candidates = [d for d in (first_unpriced, latest_priced)
                          if d is not None]
            if not candidates:
                continue
            fetch_from = min(candidates)
            if latest_priced is not None:
                conn.execute(
                    """DELETE FROM coin_prices
                       WHERE instrument_external_id = ?
                         AND source = ?
                         AND as_of_date = ?""",
                    [fiat, FX_PROVIDER, latest_priced])
            fetch_to = max_held
        else:
            fetch_from = min_held
            fetch_to = max_held

        log.info("FX %s: fetching %s..%s", fiat, fetch_from, fetch_to)
        try:
            rows = client.fetch_fiat_to_usd(fiat, fetch_from, fetch_to)
        except Exception as exc:
            log.error("FX %s fetch failed: %s — skipping", fiat, exc)
            continue

        fiats_fetched += 1
        fetched_at = int(time.time())
        n = 0
        for day, rate in rows:
            if not (min_held <= day <= max_held):
                continue
            conn.execute(
                """INSERT INTO coin_prices
                   (as_of_date, instrument_external_id, price_usd,
                    source, fetched_at)
                   VALUES (?, ?, ?, ?, ?)
                   ON CONFLICT DO NOTHING""",
                [day, fiat, rate, FX_PROVIDER, fetched_at])
            n += 1
        prices_written += n
        log.info("  %s: wrote %d daily rate(s)", fiat, n)

    return fiats_fetched, prices_written


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
        log.info("  already loaded; skipping (use --force to rebuild)")
        return

    manifest = json.loads((run_dir / "run.json").read_text())

    n_tx = ingest_transactions(conn, manifest, run_dir, snapshot_at)
    log.info("  total transactions loaded: %d", n_tx)

    ingest_portfolios_and_wallets(conn, manifest, snapshot_at)

    cutoff, n_rewritten = upsert_positions_daily(conn, snapshot_at)

    warn_unhandled_transaction_types(conn)

    ingest_portfolio_prices(conn, manifest, run_dir, snapshot_at)

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


def _stage_work_db(
    silver_db: Path, scratch_dir: Path | None,
) -> tuple[Path, bool]:
    """Pick the path DuckDB actually opens for the load.

    Without --scratch-dir the silver DB is opened in place. With it, an
    existing silver DB is copied into scratch_dir so the incremental
    load sees prior state, and the returned flag tells the caller to
    move the finished copy back onto silver_db afterwards.

    Returns (work_db, promote): work_db is the path to open, promote is
    True when it must be copied back to silver_db on success."""
    if scratch_dir is None:
        return silver_db, False
    scratch_dir.mkdir(parents=True, exist_ok=True)
    work_db = scratch_dir / silver_db.name
    if work_db.resolve() == silver_db.resolve():
        # Scratch resolves to the silver location — nothing to stage.
        return silver_db, False
    if silver_db.exists():
        shutil.copy2(silver_db, work_db)
    elif work_db.exists():
        # No source to seed from; drop a stale scratch copy so the load
        # starts from a clean migration rather than leftover state.
        work_db.unlink()
    return work_db, True


def _silver_tmp(silver_db: Path) -> Path:
    """The staging path a promote copies to before its atomic rename —
    a sibling of the silver DB so the rename stays on one filesystem."""
    return silver_db.with_name(silver_db.name + ".tmp")


def _promote_work_db(work_db: Path, silver_db: Path) -> None:
    """Move the finished scratch DB onto the silver path. The copy lands
    in a sibling temp first, then an atomic rename swaps it in, so the
    silver target is only ever the previous complete DB or the new
    complete one — never a half-written file. The caller clears the temp
    (it survives here only if the rename never ran)."""
    tmp = _silver_tmp(silver_db)
    shutil.copy2(work_db, tmp)
    os.replace(tmp, silver_db)


def run_load(conn: duckdb.DuckDBPyConnection, args: argparse.Namespace) -> int:
    """Apply migrations and ingest bronze into the open silver
    connection, honouring --replay-only and the default price fill
    (--no-fetch-prices to skip). Returns the process exit code."""
    version = apply_migrations(conn)
    log.info("silver schema at version %d (%s)", version, args.silver_db)

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
        warn_unhandled_transaction_types(conn)
        return 0

    snapshots = discover_bronze_snapshots(args.bronze_dir)
    if not snapshots:
        log.info("no bronze snapshots under %s; nothing to do",
                 args.bronze_dir)
        return 0

    log.info("found %d bronze snapshot(s) to consider", len(snapshots))
    for snap in snapshots:
        try:
            process_snapshot(conn, snap, args.force)
        except Exception as exc:
            log.error("snapshot %s failed: %s", snap.name, exc)
            # Continue with the next snapshot — partial progress
            # is better than a full rollback.
            continue

    if args.fetch_prices:
        log.info("price-fill: filling missing USD prices "
                 "from %s + FX rates from %s",
                 PRICE_PROVIDER, FX_PROVIDER)
        client = BinanceClient(api_key=get_api_key())
        coins, prices = fetch_coin_prices(conn, client, mode="missing")
        log.info("price-fill: %d coin(s) fetched, %d price "
                 "row(s) written", coins, prices)
        fx_client = FrankfurterClient()
        fiats, fx_rows = fetch_fx_rates(conn, fx_client, mode="missing")
        log.info("price-fill: %d fiat(s) fetched, %d FX "
                 "rate row(s) written", fiats, fx_rows)
        backfill_first_day_gaps(conn)

    log.info("done")
    return 0


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    args.silver_db.parent.mkdir(parents=True, exist_ok=True)
    # --force = delete the silver DB, then rebuild from all bronze (the
    # fleet-wide meaning). Reset before staging so the work DB seeds empty
    # and every snapshot re-ingests; silver.reset clears the DuckDB file and
    # its .wal sidecar.
    if args.force:
        silver.reset(args.silver_db)
    work_db, promote = _stage_work_db(args.silver_db, args.scratch_dir)
    try:
        conn = duckdb.connect(str(work_db))
        # The only silver here that is DuckDB rather than SQLite, so it
        # never passed through collectorkit's own open_db. _promote_work_db
        # copies the mode along with the bytes.
        silver.own_only(work_db)
        try:
            rc = run_load(conn, args)
        finally:
            conn.close()
        if promote:
            # Only reached on a clean run — a hard failure propagates
            # before this and leaves the original silver DB untouched.
            _promote_work_db(work_db, args.silver_db)
        return rc
    finally:
        # Clear scratch artefacts on every exit path: the working copy,
        # and the promote's staging temp should it linger (a successful
        # rename consumes it; a promote that raised between the copy and
        # the rename leaves it behind). The silver target is never
        # touched here.
        if promote:
            work_db.unlink(missing_ok=True)
            _silver_tmp(args.silver_db).unlink(missing_ok=True)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
