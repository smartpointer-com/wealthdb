"""cointracking USD price fill: crypto from Binance, fiat from Frankfurter.

Fills `coin_prices` for every coin and fiat held in `positions_daily`,
on each day it is held (:func:`fill`). `load` runs the fill after ingest
in 'missing' mode (--no-fetch-prices skips it), and `fetch-prices` runs
it in either mode (fetch_prices.py).

The fill logs under the loader's name, `cointracking.load`, from both
commands.
"""
from __future__ import annotations

import logging
import time
from datetime import date, datetime, timezone

import duckdb

from binance import (
    BinanceClient, build_mapping, get_api_key, PROVIDER as PRICE_PROVIDER,
)
from frankfurter import (
    FrankfurterClient, SUPPORTED_FIATS,
    PROVIDER as FX_PROVIDER,
)

log = logging.getLogger("cointracking.load")


def ensure_coin_mapping(
    conn: duckdb.DuckDBPyConnection,
    client: BinanceClient,
) -> dict[str, str]:
    """Make sure coin_mapping covers every instrument that appears
    in positions_daily with a non-zero amount. Returns the
    {ticker: provider_coin_id} dict for the held coin set; entries
    without a provider match are omitted (logged in build_mapping).

    The mapping is cached in silver.coin_mapping, so a fetch-prices
    run does not call the provider's master-list endpoint every time.
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
    """Return (min, max) as_of_date in positions_daily where `sym` is
    held (amount > 0). None if the coin was never held."""
    row = conn.execute(
        "SELECT MIN(as_of_date), MAX(as_of_date) FROM positions_daily "
        "WHERE instrument_external_id = ? AND amount > 0",
        [sym],
    ).fetchone()
    if not row or row[0] is None:
        return None
    return row[0], row[1]


def _fetch_window(
    conn: duckdb.DuckDBPyConnection, sym: str, source: str, mode: str,
    held_range: tuple[date, date],
) -> tuple[date, date, date | None] | None:
    """(fetch_from, fetch_to, stale_day) for one held instrument.

    'full' fetches the whole held range. 'missing' starts at the earlier
    of the first held day with no price row from `source` and the latest
    priced day. That latest row was an intraday snapshot when written,
    so it is fetched again, and `stale_day` names it for _store_prices
    to replace. None when there is nothing to fetch."""
    min_held, max_held = held_range
    if mode == "full":
        return min_held, max_held, None
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
    """, [source, sym]).fetchone()[0]
    latest_priced = conn.execute("""
        SELECT MAX(as_of_date) FROM coin_prices
        WHERE instrument_external_id = ?
          AND source = ?
    """, [sym, source]).fetchone()[0]
    candidates = [d for d in (first_unpriced, latest_priced) if d is not None]
    if not candidates:
        return None
    return min(candidates), max_held, latest_priced


def _store_prices(
    conn: duckdb.DuckDBPyConnection, sym: str, source: str,
    rows: list[tuple[date, float]], held_range: tuple[date, date],
    stale_day: date | None,
) -> int:
    """Write fetched (day, price_usd) rows for `sym`, keeping only days
    inside the held range. A day that already has a row keeps it (ON
    CONFLICT DO NOTHING), except `stale_day`: its row is deleted first,
    so the fetched close replaces the intraday price stored for it. The
    caller runs this only after a fetch succeeds, so a failed fetch
    leaves the stored price in place. Returns the rows offered."""
    if stale_day is not None:
        conn.execute(
            """DELETE FROM coin_prices
               WHERE instrument_external_id = ?
                 AND source = ?
                 AND as_of_date = ?""",
            [sym, source, stale_day])
    min_held, max_held = held_range
    fetched_at = int(time.time())
    held = [(day, sym, price, source, fetched_at)
            for day, price in rows if min_held <= day <= max_held]
    if held:
        conn.executemany(
            """INSERT INTO coin_prices
               (as_of_date, instrument_external_id, price_usd,
                source, fetched_at)
               VALUES (?, ?, ?, ?, ?)
               ON CONFLICT DO NOTHING""",
            held)
    return len(held)


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
        window = _fetch_window(conn, sym, PRICE_PROVIDER, mode, held_range)
        if window is None:
            continue
        fetch_from, fetch_to, stale_day = window

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
        n = _store_prices(conn, sym, PRICE_PROVIDER, rows, held_range,
                          stale_day)
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

    Source tag stays the same as the data it extends
    ('binance'); from the gold layer's POV the row is
    indistinguishable from a real Binance kline.

    Returns number of rows backfilled."""
    fetched_at = int(time.time())
    n_before = conn.execute("SELECT COUNT(*) FROM coin_prices").fetchone()[0]
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
    """)
    # Count by the table's growth: a fetch earlier in the same second
    # stamps its rows with the same fetched_at.
    n = conn.execute(
        "SELECT COUNT(*) FROM coin_prices").fetchone()[0] - n_before
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
        window = _fetch_window(conn, fiat, FX_PROVIDER, mode, held_range)
        if window is None:
            continue
        fetch_from, fetch_to, stale_day = window

        log.info("FX %s: fetching %s..%s", fiat, fetch_from, fetch_to)
        try:
            rows = client.fetch_fiat_to_usd(fiat, fetch_from, fetch_to)
        except Exception as exc:
            log.error("FX %s fetch failed: %s — skipping", fiat, exc)
            continue

        fiats_fetched += 1
        n = _store_prices(conn, fiat, FX_PROVIDER, rows, held_range, stale_day)
        prices_written += n
        log.info("  %s: wrote %d daily rate(s)", fiat, n)

    return fiats_fetched, prices_written


def fill(conn: duckdb.DuckDBPyConnection, mode: str) -> None:
    """The price fill in `mode` ('missing' or 'full', see
    fetch_coin_prices): USD prices from the crypto provider, then fiat
    rates, then the first-day gaps. `load` runs it in 'missing' mode
    after ingest, `fetch-prices` in either."""
    if mode not in ("missing", "full"):
        raise ValueError(f"unknown fetch mode: {mode!r}")
    log.info("price-fill: %s mode, USD prices from %s + FX rates from %s",
             mode, PRICE_PROVIDER, FX_PROVIDER)
    coins, prices = fetch_coin_prices(
        conn, BinanceClient(api_key=get_api_key()), mode=mode)
    log.info("price-fill: %d coin(s) fetched, %d price "
             "row(s) written", coins, prices)
    fiats, fx_rows = fetch_fx_rates(conn, FrankfurterClient(), mode=mode)
    log.info("price-fill: %d fiat(s) fetched, %d FX "
             "rate row(s) written", fiats, fx_rows)
    backfill_first_day_gaps(conn)
