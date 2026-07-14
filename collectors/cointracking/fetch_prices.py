#!/usr/bin/env python3
"""cointracking USD price fetcher.

Two modes:

  - Default (no flag): FULL re-fetch. Drops every existing
    coin_prices row from the active provider, then re-fetches the
    complete held range for every coin. Use after suspected
    provider data-quality issues (occasional bad values).

  - --missing: fetch only (held, unpriced) gaps + the previous
    run's latest priced day (always re-fetched because it was an
    intraday snapshot when first stored). Identical to the price
    fill `load` runs by default. Useful when load was run with
    --no-fetch-prices and prices need to be brought up to date
    without re-running the bronze ingest.

The "held" set comes from positions_daily.amount > 0 — every coin
held on at least one day across any portfolio. Prices outside
the held range are not fetched.

Provider: Binance public-spot (api.binance.com, no key, no signup).
USDT-denominated; the small USDT/USD basis is absorbed into the
stored price_usd values. Stablecoins (USDT, USDC, DAI, …) are
emitted as synthetic 1.0. See binance.py for the polite-client
details.
"""
from __future__ import annotations

import argparse
import logging
import sys
from pathlib import Path

import duckdb

from collectorkit import cli

from binance import BinanceClient, get_api_key
from frankfurter import FrankfurterClient
from load import (
    MIGRATIONS_DIR, apply_migrations, fetch_coin_prices, fetch_fx_rates,
    backfill_first_day_gaps,
)

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
    try:
        # Make sure the schema is current (in case fetch-prices is
        # run against a silver DB that pre-dates migration 0002).
        apply_migrations(conn)

        client = BinanceClient(api_key=get_api_key())
        mode = "missing" if args.missing else "full"
        coins, prices = fetch_coin_prices(conn, client, mode=mode)
        log.info("crypto: %s mode, %d coin(s) fetched, %d price row(s) written",
                 mode, coins, prices)
        fx_client = FrankfurterClient()
        fiats, fx_rows = fetch_fx_rates(conn, fx_client, mode=mode)
        log.info("fx:     %s mode, %d fiat(s) fetched, %d rate row(s) written",
                 mode, fiats, fx_rows)
        backfill_first_day_gaps(conn)
    finally:
        conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
