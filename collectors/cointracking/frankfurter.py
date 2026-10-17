"""Frankfurter ECB-rate client for FX coverage in coin_prices.

Frankfurter (https://api.frankfurter.app/) wraps the European
Central Bank's daily reference rates with a clean REST surface.
Free, no API key, no signup, no rate-limit announced (in practice
a single call per fiat × held-range chunk is all the collector needs).

Used to fill the FX coverage gap for non-USD fiat cash balances
(e.g. EUR). For those instruments, `price_usd` is
populated with the ECB closing reference rate for that day —
inserted into the same `coin_prices` table the crypto path uses,
under `source='frankfurter'`.

ECB publishes weekday-only rates; the client forward-fills across
weekends and ECB holidays (typical FX convention: the last
business day's close applies through the next non-business day),
so the returned series covers every calendar day.

The public-interface signatures (`FrankfurterClient`,
`fetch_fiat_to_usd`) mirror the shape of the crypto clients so
load.py and fetch_prices.py can call them with the same
ergonomics.
"""
from __future__ import annotations

import logging
import time
from datetime import date, timedelta
from typing import Any

import requests

log = logging.getLogger("cointracking.frankfurter")

BASE_URL = "https://api.frankfurter.app"
USER_AGENT = "wealthdb-cointracking/0.1"
RATE_LIMIT_DELAY_S = 0.2  # Conservative; service has no published cap.

PROVIDER = "frankfurter"

# The fiat tickers this client converts. binance.py excludes the
# same set, plus USD, from its lookup; frankfurter is the destination
# for them. USD is trivially 1.0 and is excluded here, so it costs no
# API call.
SUPPORTED_FIATS: set[str] = {
    "EUR", "CHF", "GBP", "JPY", "AUD", "CAD", "NZD",
    "SEK", "NOK", "DKK", "PLN", "CZK", "HUF",
    "INR", "CNY", "KRW", "SGD", "HKD", "MXN",
    "BRL", "ZAR", "TRY", "ILS", "THB", "RUB", "TWD",
}


class FrankfurterClient:
    """Polite synchronous client. No auth, very light rate-limiter."""

    def __init__(self) -> None:
        self.session = requests.Session()
        self.session.headers.update({
            "User-Agent": USER_AGENT,
            "Accept": "application/json",
        })
        self._last_call_at = 0.0

    def _request(self, path: str, params: dict | None = None) -> Any:
        wait = (self._last_call_at + RATE_LIMIT_DELAY_S) - time.monotonic()
        if wait > 0:
            time.sleep(wait)
        r = self.session.get(BASE_URL + path, params=params, timeout=60)
        self._last_call_at = time.monotonic()
        r.raise_for_status()
        return r.json()

    def fetch_fiat_to_usd(
        self, fiat: str, from_date: date, to_date: date,
    ) -> list[tuple[date, float]]:
        """Daily `<fiat>` → USD rates for [from_date, to_date].
        Returns [(as_of_date, usd_per_one_fiat), …] ascending,
        forward-filled across weekends / ECB holidays so every
        calendar day in the range carries a rate."""
        fiat_u = fiat.upper()
        if fiat_u == "USD":
            # Trivial: 1 USD = 1 USD. Synthesise the full day range.
            return [
                (from_date + timedelta(days=i), 1.0)
                for i in range((to_date - from_date).days + 1)
            ]
        if fiat_u not in SUPPORTED_FIATS:
            log.warning("fiat %s not in Frankfurter supported set; "
                        "skipping", fiat)
            return []

        # Frankfurter uses /YYYY-MM-DD..YYYY-MM-DD?from=<base>&to=USD.
        # The result rate is "USD per 1 base" — exactly the
        # price_usd of one unit of the base fiat.
        #
        # The query starts 10 days early, so a published rate exists
        # to forward-fill from when the requested from_date is itself
        # a weekend / ECB holiday.
        padded_from = from_date - timedelta(days=10)
        data = self._request(
            f"/{padded_from.isoformat()}..{to_date.isoformat()}",
            {"from": fiat_u, "to": "USD"},
        )
        rates = data.get("rates", {})
        # Build the published-rate map first.
        published: dict[date, float] = {}
        for day_str, m in rates.items():
            day = date.fromisoformat(day_str)
            usd_rate = m.get("USD")
            if usd_rate is None:
                continue
            published[day] = float(usd_rate)
        if not published:
            return []

        # Forward-fill across the FULL requested range: walk day by
        # day, carrying the last-known rate through weekends / ECB
        # holidays. Standard FX convention — Friday's close applies
        # through the weekend and stays in force until Monday
        # publishes the next rate.
        result: list[tuple[date, float]] = []
        last_known: float | None = None
        cur = padded_from
        while cur <= to_date:
            if cur in published:
                last_known = published[cur]
            if last_known is not None and cur >= from_date:
                result.append((cur, last_known))
            cur += timedelta(days=1)
        return result
