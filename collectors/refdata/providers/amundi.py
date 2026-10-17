"""
amundi: Amundi's ETFs, from the product API its Swiss site's fund pages
read.

- Claim: an ETF with an ISIN whose name names Amundi (or Lyxor, whose
  funds Amundi took over), as brokers write it. No request.
- Dates: none. The API serves the current holdings.
- Fetch: one read-only POST, since the API has no GET form, for the
  fund's name, how it replicates its index, and its holdings, as the API
  answers private investors in Switzerland.
- Parse: every holding with its ISIN, name, weight, quantity, currency,
  sector and country of risk; weights are fractions, read as percent. A
  fund that replicates its index through a swap holds a substitute
  basket: its holdings are basis `collateral`, its replication is
  marked synthetic, and the look-through reads the proxy
  `exposure.proxies` names for it (EXPOSURE.md §5). The API lists an
  index's top ten only, so no index lines are read.

Terms: README.md, amundi.
"""

from __future__ import annotations

import re
from datetime import date
from pathlib import Path

from collectorkit import bronze

from . import table
from .base import (Parsed, Provider, ProviderError, blank_to_none, dec, ident, percent, read_json, read_meta, row,
                   write_meta)

HOST = "https://www.amundietf.ch"
API = f"{HOST}/mapi/ProductAPI/getProductsData"
# Switzerland, in English, for private investors.
CONTEXT = {"countryCode": "CHE", "languageCode": "en", "userProfileName": "RETAIL"}
CHARACTERISTICS = ["FUND_REPLICATION_METHODOLOGY", "SHARE_MARKETING_NAME"]
FIELDS = ["date", "type", "isin", "name", "weight", "quantity", "currency", "sector", "country", "countryOfRisk"]
DOC = "products.json"
ISSUER = re.compile(r"\b(?:amundi|lyxor)\b", re.I)
ETF = re.compile(r"ucits|\betf\b", re.I)
# A replication through a swap, as the API words it ("Indirect (Swap
# Based)", "Synthetic").
SWAP = re.compile(r"swap|synthetic|indirect", re.I)


class Amundi(Provider):
    name = "amundi"
    serves_funds = True

    def claim(self, fund, ctx):
        """An ETF whose name names the issuer, as brokers write it; the API
        serves ETFs, not Amundi's other funds. No request: the fetch finds
        out whether the API knows the fund."""
        isin, name = ident(fund, "isin"), fund.name or ""
        if not isin or not ISSUER.search(name) or not ETF.search(name):
            return None
        return {"ref": fund.ref, "isin": isin}

    def fetch(self, claim, as_of, outdir: Path, ctx):
        body = {"context": CONTEXT, "productIds": [claim["isin"]], "productType": "PRODUCT",
                "characteristics": CHARACTERISTICS, "composition": {"compositionFields": FIELDS}}
        resp = ctx.client.post_json(API, body, headers={"Accept": "application/json"})
        product = _product(resp.json(), claim["isin"])
        if product is None:
            raise ProviderError(f"Amundi's API does not know {claim['isin']}")
        day = _day(product)
        if day is None:
            raise ProviderError(f"Amundi's API lists no dated holdings for {claim['isin']}")
        bronze.atomic_write_bytes(outdir / DOC, resp.body)
        write_meta(outdir, provider=self.name, ref=claim["ref"], isin=claim["isin"], as_of=day.isoformat())
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
    return out
