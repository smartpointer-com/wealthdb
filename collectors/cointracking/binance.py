"""Binance public-spot API client for USDT-denominated price
history.

Free, no API key required, geographically generous (api.binance.com
serves most non-US regions; the U.S. endpoint at api.binance.us
has narrower coverage if needed). 1,200 requests/min IP weight on
the public endpoint — orders of magnitude more than we need.

USDT-denominated, NOT USD. The tracking error of USDT/USD around
parity is a few basis points; the schema's `coin_prices` table
is labelled `price_usd` and we store the Binance close in that
column — the small USDT/USD basis is silently absorbed.

Two endpoints used:

  - `/api/v3/exchangeInfo` — every trading pair, its status, and
    its (baseAsset, quoteAsset). Drives the CT-ticker → Binance-
    symbol mapping: filter for `quoteAsset=USDT` pairs and map
    each base asset's ticker to the corresponding pair. Coins
    without a USDT pair (long-tail or delisted) return None and
    get a placeholder `provider_coin_id=NULL` in coin_mapping.

  - `/api/v3/klines?symbol=…&interval=1d&startTime=…&endTime=…
    &limit=1000` — daily OHLC klines in millisecond timestamps.
    Multi-year ranges are chunked because the per-call limit is
    1,000 entries.

Stablecoins (USDT, USDC, DAI, …) can't be self-quoted on Binance
(USDT/USDT doesn't exist). For these, the client emits synthetic
1.0 USD prices for every day in the requested range.

Fiat tickers (USD, EUR, CHF, …) are excluded from lookup before
any API call — they have no crypto USD price.
"""
from __future__ import annotations

import logging
import os
import time
from datetime import date, datetime, timedelta, timezone
from typing import Any

import requests

log = logging.getLogger("cointracking.binance")

BASE_URL = "https://api.binance.com"
USER_AGENT = "wealthdb-cointracking/0.1"

# Binance allows 1,200 requests/min on the public-spot endpoint
# with a per-route weight system. 0.1s pacing = 600 req/min, well
# below the ceiling for any path we use.
RATE_LIMIT_DELAY_S = 0.1

# Maximum daily klines per /api/v3/klines call.
KLINES_MAX_PER_CALL = 1000

PROVIDER = "binance"

# Optional Pro / authenticated key for the higher rate-limit tier.
# Unused on the free public endpoint.
ENV_API_KEY = "BINANCE_API_KEY"

# Sentinel `coin_id` returned by build_mapping for stablecoins.
# ohlcv_historical recognises it and emits synthetic 1.0 prices
# instead of hitting the API.
STABLECOIN_SENTINEL = "__stablecoin__"

# USD-pegged stablecoins. For each of these we emit synthetic
# price=1.0; the small USDT/USD basis (a few bps) absorbs the
# tracking error.
STABLECOINS: set[str] = {
    "USDT", "USDC", "BUSD", "DAI", "TUSD", "USDP", "GUSD",
    "FDUSD", "PYUSD", "USDD", "USDS",
}

# Fiat tickers — held as cash in some portfolios, but have no
# crypto USD price.
FIAT_TICKERS: set[str] = {
    "USD", "EUR", "CHF", "GBP", "JPY", "AUD", "CAD", "NZD",
    "SGD", "HKD", "SEK", "NOK", "DKK", "PLN", "CZK", "HUF",
    "RUB", "INR", "CNY", "KRW", "TWD", "THB", "BRL", "MXN",
    "ZAR", "TRY", "ILS",
}


class BinanceClient:
    """Polite synchronous client. Single connection, rate-limited,
    retries 418/429 with exponential backoff."""

    def __init__(self, api_key: str | None = None) -> None:
        self.session = requests.Session()
        self.session.headers.update({
            "User-Agent": USER_AGENT,
            "Accept": "application/json",
        })
        if api_key:
            self.session.headers["X-MBX-APIKEY"] = api_key
            log.info("Binance API key active")
        self._last_call_at = 0.0

    def _request(self, path: str, params: dict | None = None) -> Any:
        wait = (self._last_call_at + RATE_LIMIT_DELAY_S) - time.monotonic()
        if wait > 0:
            time.sleep(wait)
        for attempt in range(5):
            r = self.session.get(BASE_URL + path, params=params, timeout=60)
            self._last_call_at = time.monotonic()
            # 429 = rate-limit warning; 418 = IP banned briefly for
            # repeated 429s. Both warrant a longer backoff.
            if r.status_code in (418, 429):
                wait_s = 30 * (2 ** attempt)
                log.warning("Binance %d (attempt %d/5); backing off %ds",
                            r.status_code, attempt + 1, wait_s)
                time.sleep(wait_s)
                continue
            r.raise_for_status()
            return r.json()
        raise RuntimeError(
            f"Binance: gave up on {path} after 5 retries on 418/429")

    def exchange_info(self) -> dict:
        """Full symbol metadata. Symbols field has {symbol, status,
        baseAsset, quoteAsset, …}. Cached for the lifetime of the
        client; call once per process."""
        return self._request("/api/v3/exchangeInfo")

    def ohlcv_historical(
        self, coin_id: str, from_ts: int, to_ts: int,
    ) -> list[tuple[date, float]]:
        """Returns [(as_of_date, close_price), …] sorted ascending.

        `coin_id` is either:
          - A Binance symbol like 'BTCUSDT' — real API call, chunked
            in KLINES_MAX_PER_CALL-day windows.
          - STABLECOIN_SENTINEL — synthetic 1.0 USD prices for every
            day in the range (no API call).
        """
        from_date = datetime.fromtimestamp(
            from_ts, tz=timezone.utc).date()
        to_date = datetime.fromtimestamp(
            to_ts, tz=timezone.utc).date()
        if from_date > to_date:
            return []

        if coin_id == STABLECOIN_SENTINEL:
            rows: list[tuple[date, float]] = []
            cur = from_date
            while cur <= to_date:
                rows.append((cur, 1.0))
                cur += timedelta(days=1)
            return rows

        rows = []
        start_ms = int(datetime.combine(
            from_date, datetime.min.time(), tzinfo=timezone.utc
        ).timestamp() * 1000)
        end_ms = int(datetime.combine(
            to_date + timedelta(days=1), datetime.min.time(),
            tzinfo=timezone.utc,
        ).timestamp() * 1000)

        while start_ms < end_ms:
            data = self._request("/api/v3/klines", {
                "symbol": coin_id,
                "interval": "1d",
                "startTime": str(start_ms),
                "endTime": str(end_ms),
                "limit": str(KLINES_MAX_PER_CALL),
            })
            if not data:
                break
            for kline in data:
                # kline = [open_time_ms, open, high, low, close,
                #          volume, close_time_ms, …]
                open_time_ms = kline[0]
                close_price = float(kline[4])
                day = datetime.fromtimestamp(
                    open_time_ms / 1000, tz=timezone.utc).date()
                rows.append((day, close_price))
            # Advance to the day after the last returned kline so
            # the next iteration doesn't re-fetch the boundary day.
            last_open_ms = data[-1][0]
            start_ms = last_open_ms + 86400 * 1000
        return rows


def build_mapping(
    client: BinanceClient, instruments: list[str],
) -> dict[str, str | None]:
    """For each ticker in `instruments`, return either:

      - the Binance USDT-pair symbol (e.g. 'BTCUSDT') if listed
      - STABLECOIN_SENTINEL for stablecoins (synthetic 1.0 prices)
      - None for tickers without a USDT pair (skipped during fetch)

    Fiat tickers are dropped entirely before any API call.
    """
    crypto_targets = [s for s in instruments
                      if s.upper() not in FIAT_TICKERS]
    skipped_fiat = [s for s in instruments
                    if s.upper() in FIAT_TICKERS]
    if skipped_fiat:
        log.info("skipping %d fiat ticker(s) (no crypto USD price): %s",
                 len(skipped_fiat), sorted(skipped_fiat))

    if not crypto_targets:
        return {}

    log.info("fetching /api/v3/exchangeInfo (Binance symbol list)")
    info = client.exchange_info()

    # baseAsset → first matching USDT pair. Binance has at most one
    # per base asset, so order doesn't matter.
    usdt_pairs: dict[str, str] = {}
    for s in info.get("symbols", []):
        if s.get("quoteAsset") != "USDT":
            continue
        base = s.get("baseAsset", "").upper()
        if base and base not in usdt_pairs:
            usdt_pairs[base] = s["symbol"]

    mapping: dict[str, str | None] = {}
    for sym in crypto_targets:
        sym_u = sym.upper()
        if sym_u in STABLECOINS:
            mapping[sym] = STABLECOIN_SENTINEL
            log.debug("mapped %s → STABLECOIN (synthetic 1.0 prices)",
                      sym)
            continue
        pair = usdt_pairs.get(sym_u)
        if pair:
            mapping[sym] = pair
            log.debug("mapped %s → %s", sym, pair)
        else:
            log.warning("no Binance USDT pair for %s — USD prices "
                        "will be missing for this coin", sym)
            mapping[sym] = None
    return mapping


def get_api_key() -> str | None:
    """Read BINANCE_API_KEY from env. Returns None for the public
    endpoint (no key required)."""
    return os.environ.get(ENV_API_KEY) or None
