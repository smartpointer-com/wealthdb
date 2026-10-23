"""
amundi: Amundi's ETFs, from the product API its Swiss site's fund pages
read.

- Claim: an ETF with an ISIN whose name names Amundi (or Lyxor, whose
  funds Amundi took over), as brokers write it. No request.
- Dates: none. The API serves the current holdings.
- Fetch: one read-only POST, since the API has no GET form, for the
  fund's name, how it replicates its index, and its holdings, as the API
  answers private investors in Switzerland. For a fund that wants its
  dividend calendar, the same POST asks for the fund's dividend history
  too.
- Parse: every holding with its ISIN, name, weight, quantity, currency,
  sector and country of risk; weights are fractions, read as percent. A
  fund that replicates its index through a swap holds a substitute
  basket: its holdings are basis `collateral`, its replication is
  marked synthetic, and the look-through reads the proxy
  `exposure.proxies` names for it (EXPOSURE.md §5). The API lists an
  index's top ten only, so no index lines are read.
- Dividend calendar: the answer's `historics` state each dividend's
  amount (`dividendAmount`) and its currency (`dividendCurrency`) by one
  date, the ex day, as an epoch millisecond. It reads as the day of its
  nearest UTC midnight, so a day written as midnight in a European time
  zone (the evening before, in UTC) keeps its date. Each is one
  dividend, with no record or pay day, and the points of one ex day are
  one event (base.merge_events). The history lists every dividend of
  the fund, so its events are complete (Parsed.events_complete).
  Where the answer holds no `historics`, a warning says so, and
  meta.json names the calendar in facts_missing. The events are dated
  by the holdings' day.

Terms: README.md, amundi.
"""

from __future__ import annotations

import logging
import re
from datetime import date, datetime, timezone
from pathlib import Path

from collectorkit import bronze

from . import table
from .base import (Parsed, Provider, ProviderError, amount, blank_to_none, dec, event_row, facts_missing, ident,
                   merge_events, percent, read_json, read_meta, row, write_meta)

HOST = "https://www.amundietf.ch"
API = f"{HOST}/mapi/ProductAPI/getProductsData"
# Switzerland, in English, for private investors.
CONTEXT = {"countryCode": "CHE", "languageCode": "en", "userProfileName": "RETAIL"}
CHARACTERISTICS = ["FUND_REPLICATION_METHODOLOGY", "SHARE_MARKETING_NAME"]
FIELDS = ["date", "type", "isin", "name", "weight", "quantity", "currency", "sector", "country", "countryOfRisk"]
# The dividend history a fund that wants its calendar asks for: each
# dividend's amount and currency, by its ex day.
HISTORICS = [{"indicator": "dividendAmount"}, {"indicator": "dividendCurrency"}]
DOC = "products.json"
ISSUER = re.compile(r"\b(?:amundi|lyxor)\b", re.I)
ETF = re.compile(r"ucits|\betf\b", re.I)
# A replication through a swap, as the API words it ("Indirect (Swap
# Based)", "Synthetic").
SWAP = re.compile(r"swap|synthetic|indirect", re.I)
# Half a day, in milliseconds: an epoch millisecond reads as the day of
# its nearest UTC midnight.
HALF_DAY_MS = 12 * 3600 * 1000

log = logging.getLogger("refdata.amundi")


class Amundi(Provider):
    name = "amundi"
    serves_funds = True
    facts = frozenset({"dividend_calendar"})

    def claim(self, fund, ctx):
        """An ETF whose name names the issuer, as brokers write it; the API
        serves ETFs, not Amundi's other funds. No request: the fetch finds
        out whether the API knows the fund."""
        isin, name = ident(fund, "isin"), fund.name or ""
        if not isin or not ISSUER.search(name) or not ETF.search(name):
            return None
        # A fund wanted as a proxy or an underlying wants no calendar, and
        # its fetch asks for no dividend history.
        return {"ref": fund.ref, "isin": isin, "calendar": "dividend_calendar" in fund.wants}

    def fetch(self, claim, as_of, outdir: Path, ctx):
        body = {"context": CONTEXT, "productIds": [claim["isin"]], "productType": "PRODUCT",
                "characteristics": CHARACTERISTICS, "composition": {"compositionFields": FIELDS}}
        if claim.get("calendar"):
            body["historics"] = HISTORICS
        resp = ctx.client.post_json(API, body, headers={"Accept": "application/json"})
        product = _product(resp.json(), claim["isin"])
        if product is None:
            raise ProviderError(f"Amundi's API does not know {claim['isin']}")
        day = _day(product)
        if day is None:
            raise ProviderError(f"Amundi's API lists no dated holdings for {claim['isin']}")
        bronze.atomic_write_bytes(outdir / DOC, resp.body)
        missing = []
        if claim.get("calendar") and not isinstance(product.get("historics"), list):
            log.warning("%s: its answer states no dividend history", claim["ref"])
            missing.append("dividend_calendar")
        write_meta(outdir, provider=self.name, ref=claim["ref"], isin=claim["isin"], as_of=day.isoformat(),
                   **facts_missing(missing))
        return day

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        product = _product(read_json(indir, DOC), meta["isin"])
        if product is None:
            raise ProviderError(f"the answer holds no product {meta['isin']}")
        return parse_product(product, meta["ref"], provider=self.name)

    def probe(self, ctx) -> str:
        resp = ctx.client.post_json(API, {"context": CONTEXT, "productIds": [], "productType": "PRODUCT"},
                                    headers={"Accept": "application/json"})
        return f"Amundi's product API answered {resp.status}"


def _product(doc: dict, isin: str) -> dict | None:
    """The answer's product for the ISIN asked for."""
    return next((p for p in doc.get("products") or [] if str(p.get("productId", "")).upper() == isin), None)


def _day(product: dict) -> date | None:
    days = [str(((c or {}).get("compositionCharacteristics") or {}).get("date") or "")[:10]
            for c in (product.get("composition") or {}).get("compositionData") or []]
    days = [d for d in days if d]
    return date.fromisoformat(max(days)) if days else None


def _percent(item: dict, c: dict) -> str | None:
    """A holding's weight in percent: the API states a fraction."""
    return percent(item.get("weight") if item.get("weight") is not None else c.get("weight"))


def parse_product(product: dict, ref: str, *, provider: str) -> Parsed:
    """One Amundi fund's holdings as silver rows for fund `ref`."""
    as_of = _day(product)
    if as_of is None:
        raise ProviderError("the answer lists no holdings with a date")
    characteristics = product.get("characteristics") or {}
    swap = bool(SWAP.search(str(characteristics.get("FUND_REPLICATION_METHODOLOGY") or "")))
    basis = "collateral" if swap else "holdings"
    out = Parsed()
    for item in (product.get("composition") or {}).get("compositionData") or []:
        c = item.get("compositionCharacteristics") or {}
        out.lines.append(row(
            "lines", fund_ref=ref, as_of=as_of, basis=basis, line_no=len(out.lines) + 1,
            isin=blank_to_none(c.get("isin")), name=blank_to_none(c.get("name")), asset_type=blank_to_none(c.get("type")),
            weight=_percent(item, c), quantity=dec(c.get("quantity")), currency=blank_to_none(c.get("currency")),
            country=blank_to_none(c.get("country")), risk_country=blank_to_none(c.get("countryOfRisk")),
            sector=blank_to_none(c.get("sector"))))
    out.compositions.append(table.composition(ref, as_of, out.lines, provider=provider, basis=basis,
                                              fund_name=characteristics.get("SHARE_MARKETING_NAME"),
                                              currency=(product.get("currencies") or {}).get("CURRENCY")))
    if swap:
        out.attributes.append(row("attributes", fund_ref=ref, as_of=as_of, replication="synthetic",
                                  payload={"replication": characteristics.get("FUND_REPLICATION_METHODOLOGY")}))
    out.dividend_events = dividend_events(product, ref, as_of, provider=provider)
    out.events_complete = isinstance(product.get("historics"), list)
    return out


def _ex_day(ms) -> date | None:
    """The day an epoch millisecond names: that of its nearest UTC
    midnight."""
    if isinstance(ms, bool) or not isinstance(ms, int | float):
        return None
    try:
        return datetime.fromtimestamp((ms + HALF_DAY_MS) / 1000, tz=timezone.utc).date()
    except (OverflowError, OSError, ValueError):
        return None


def _series(product: dict, indicator: str) -> list:
    """The (day, value) points of one of the answer's historics."""
    entry = next((h for h in product.get("historics") or [] if isinstance(h, dict) and h.get("indicator") == indicator),
                 {})
    return [(_ex_day(p.get("date")), p.get("data")) for p in entry.get("historicalData") or [] if isinstance(p, dict)]


def dividend_events(product: dict, ref: str, day: date, *, provider: str) -> list:
    """The dividend_events rows of the answer's dividend history, dated
    `day`: one dividend per amount, by its ex day, in the currency stated
    for the same day. A point of no day is none; an amount of zero is
    none; the points of one ex day are one event (base.merge_events)."""
    currencies = {d: blank_to_none(v) for d, v in _series(product, "dividendCurrency") if d}
    out = []
    for ex, value in _series(product, "dividendAmount"):
        paid = amount(value)[0]
        if ex is None or paid is None:
            continue
        out.append(event_row(ref, provider, day, "dividend", {"ex_date": ex}, amount=paid,
                             currency=currencies.get(ex)))
    return merge_events(out)
