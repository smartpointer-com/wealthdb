#!/usr/bin/env python3
"""cointracking USD price fetcher.

Two modes:

  - Default (no flag): FULL re-fetch. Drops every existing
    coin_prices row from the active provider and the Frankfurter FX
    rows, then re-fetches the complete held range for every coin.
    Use after suspected provider data-quality issues (occasional bad
    values).

  - --missing: fetch only (held, unpriced) gaps + the previous
    run's latest priced day (always re-fetched because it was an
    intraday snapshot when first stored). The price fill `load`
    runs by default (prices.fill). Useful when load was run with
    --no-fetch-prices and prices need to be brought up to date
    without re-running the bronze ingest.

The "held" set comes from positions_daily.amount > 0 — every coin
held on at least one day across any portfolio. Prices outside
the held range are not fetched.

Providers: crypto from Binance public-spot (api.binance.com, no
key, no signup), USDT-denominated; the small USDT/USD basis is
absorbed into the stored price_usd values. Stablecoins (USDT, USDC,
DAI, …) are emitted as synthetic 1.0. See binance.py for the
polite-client details. Fiat from the ECB's reference rates via
Frankfurter (frankfurter.py). Either mode ends by backfilling a held
day before a coin's first price from the next price
(`backfill_first_day_gaps`).
"""
from __future__ import annotations

import argparse
import logging
import sys
from pathlib import Path

import duckdb

from collectorkit import cli, silver

import prices
from load import apply_migrations

log = logging.getLogger("cointracking.fetch_prices")


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
        "--missing", action="store_true",
        help=("Fetch only (held, unpriced) gaps. Without this flag, "
              "the full held range is re-fetched (corruption-recovery "
              "default)."),
    )
    cli.add_common_args(p)
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    if not args.silver_db.is_file():
        log.error("silver DB not found at %s — run `cointracking load` "
                  "first", args.silver_db)
        return 1

    conn = duckdb.connect(str(args.silver_db))
    silver.own_only(args.silver_db)
    try:
        # Bring the schema up to date, so an older silver gains the
        # price tables.
        apply_migrations(conn)
        prices.fill(conn, "missing" if args.missing else "full")
    finally:
        conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
