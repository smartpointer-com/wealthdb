"""
invesco: Invesco's ETFs, from the data API its fund pages read.

- Claim: an ETF whose name names the issuer (Invesco, PowerShares), as
  brokers write it; the API serves ETFs, not Invesco's mutual funds. A
  UCITS fund is asked for by its ISIN; a US fund by its CUSIP, stated or
  read from its US ISIN. Invesco publishes no product list a plain
  request can read.
- Dates: none. The API serves the current holdings.
- Fetch, UCITS: the fund's holdings, its index's constituents and its
  swap counterparties. US: the fund's holdings.
- Parse: every holding with its ISIN or CUSIP, name, weight and
  currency (US: ticker, units, market value, security type). The index
  constituents are a composition of basis `index`, dated by the index
  file. A fund with swap counterparties holds a substitute basket: its
  holdings are basis `collateral`, the look-through reads the index, and
  each counterparty is a row of role `swap`, with the fund's replication
  marked synthetic.

The counterparty entries are read by their field names (a name, and a
weight or exposure in percent); only invented fixtures test that layout.

Terms: Invesco's terms grant "a limited, non-exclusive and revocable
license to access and make personal use of the Website", and forbid
using "devices (including software) that are designed to provide
repeated automated access to the Website" and "any data mining, robots,
or similar data gathering and extraction tools". Off until refdata.cfg
names it.
"""

from __future__ import annotations

import json
import re
from datetime import date
from pathlib import Path

import fetcher
from collectorkit import bronze

from . import table
from .base import Parsed, Provider, ProviderError, Unavailable, blank_to_none, dec, read_meta, write_meta

API = "https://dng-api.invesco.com/cache/v1/accounts/{locale}/shareclasses/{id}/"
ISSUER = re.compile(r"invesco|powershares", re.I)
ETF = re.compile(r"\betf\b|ucits|powershares|\bqqq\b", re.I)
DOCS = {"fund": "holdings-fund.json", "index": "holdings-index.json", "swaps": "swaps.json"}


class Invesco(Provider):
    name = "invesco"
    serves_funds = True

    def claim(self, fund, ctx):
        if not (ISSUER.search(fund.name or "") and ETF.search(fund.name or "")):
            return None
        isin = (fund.isin or (fund.ref[5:] if fund.ref.startswith("isin:") else "") or "").upper()
        cusip = (fund.cusip or (fund.ref[6:] if fund.ref.startswith("cusip:") else "") or "").upper()
        if not cusip and isin.startswith("US") and len(isin) == 12:
            cusip = isin[2:11]
        if cusip:
            return {"ref": fund.ref, "site": "us", "id": cusip}
        if isin:
            return {"ref": fund.ref, "site": "ucits", "id": isin}
        return None

    def fetch(self, claim, as_of, outdir: Path, ctx):
        if as_of is not None:
            raise Unavailable("invesco serves the current holdings only")
        headers = {"Accept": "application/json"}
        if claim["site"] == "us":
            base = API.format(locale="en_US", id=claim["id"])
            asks = {"fund": (base + "holdings/fund", {"idType": "cusip", "productType": "ETF"})}
        else:
            base = API.format(locale="en_GB", id=claim["id"])
            asks = {"fund": (base + "holdings/fund", {"idType": "isin"}),
                    "index": (base + "holdings/index", {"idType": "isin"}),
                    "swaps": (base + "swaps", {"idType": "isin", "variationType": "counterParties"})}
        got = {}
        for kind, (url, params) in asks.items():
            try:
                resp = ctx.client.get(url, params=params, headers=headers)
            except fetcher.FetchError as e:
                if kind != "fund" and e.status in (404, 500):
                    continue
                raise
            try:
                got[kind] = (resp.body, json.loads(resp.body))
            except ValueError as e:
                raise ProviderError(f"{resp.url} is not JSON: {e}") from e
        fund_doc = got["fund"][1]
        day = _day(fund_doc)
        if day is None or not isinstance(fund_doc.get("holdings"), list):
            raise ProviderError(f"{claim['id']}: the holdings answer states no holdings or no date")
        outdir.mkdir(parents=True, exist_ok=True)
        for kind, (body, _) in got.items():
            bronze.atomic_write_bytes(outdir / DOCS[kind], body)
        write_meta(outdir, provider=self.name, ref=claim["ref"], site=claim["site"], as_of=day.isoformat(),
                   files=sorted(got))
        return day

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        docs = {k: json.loads((indir / DOCS[k]).read_text(encoding="utf-8")) for k in meta["files"]}
        return parse_docs(docs, meta["ref"], provider=self.name)


def _day(doc: dict) -> date | None:
    for key in ("effectiveBusinessDate", "effectiveDate"):
        v = (doc or {}).get(key)
        if v:
            try:
                return date.fromisoformat(str(v)[:10])
            except ValueError:
                continue
    return None


def _iso(value) -> str | None:
    day = table.parse_day(value) if blank_to_none(value) else None
    return day.isoformat() if day else None


def _first(entry: dict, *words):
    """The value of the first key that holds one of the words, any case."""
    for k, v in entry.items():
        if any(w in k.lower() for w in words) and v not in (None, ""):
            return v
    return None


def _lines(holdings: list, ref: str, as_of: date, basis: str) -> list:
    out = []
    for h in holdings:
        isin = blank_to_none(h.get("isin"))
        out.append({
            "fund_ref": ref, "as_of": as_of, "basis": basis, "line_no": len(out) + 1,
            "isin": isin.upper() if isin else None, "cusip": blank_to_none(h.get("cusip")), "figi": None,
            "ticker": blank_to_none(h.get("ticker")), "exchange": None,
            "name": blank_to_none(h.get("name") or h.get("issuerName")),
            "asset_type": blank_to_none(h.get("securityTypeName")),
            "weight": dec(h.get("weight", h.get("percentageOfTotalNetAssets"))),
            "quantity": dec(h.get("units")), "market_value": dec(h.get("marketValueBase")),
            "currency": blank_to_none(h.get("currency")), "country": None, "risk_country": None, "sector": None,
            "industry": None, "maturity": _iso(h.get("maturityDate")), "coupon": dec(h.get("coupon")),
            "long_currency": None, "long_amount": None, "short_currency": None, "short_amount": None,
            "payload": {k: v for k, v in h.items() if k not in (
                "isin", "cusip", "ticker", "name", "issuerName", "securityTypeName", "weight",
                "percentageOfTotalNetAssets", "units", "marketValueBase", "currency", "maturityDate", "coupon")
                and v not in (None, "")},
        })
    return out


def _composition(ref, as_of, lns, basis, provider):
    weights = [float(x["weight"]) for x in lns if x["weight"] is not None]
    return {"fund_ref": ref, "as_of": as_of, "basis": basis, "provider": provider, "source_kind": "daily_file",
            "fund_name": None, "currency": None, "net_assets": None, "line_count": len(lns),
            "weight_sum": dec(round(sum(weights), 8)) if weights else None, "units_outstanding": None,
            "payload": {}}


def parse_docs(docs: dict, ref: str, *, provider: str) -> Parsed:
    """An Invesco fund's holdings, index and swap answers as silver rows
    for fund `ref`."""
    out = Parsed()
    fund_doc = docs["fund"]
    as_of = _day(fund_doc)
    if as_of is None:
        raise ProviderError("the holdings answer states no date")
    swaps = (docs.get("swaps") or {}).get("swapsCounterParties") or []
    basis = "collateral" if swaps else "holdings"
    lns = _lines(fund_doc.get("holdings") or [], ref, as_of, basis)
    out.lines += lns
    out.compositions.append(_composition(ref, as_of, lns, basis, provider))
    index_doc = docs.get("index")
    if index_doc and index_doc.get("holdings"):
        index_day = _day(index_doc) or as_of
        idx = _lines(index_doc["holdings"], ref, index_day, "index")
        out.lines += idx
        out.compositions.append(_composition(ref, index_day, idx, "index", provider))
    for cp in swaps:
        name = _first(cp, "name", "counterparty")
        if not name:
            continue
        out.counterparties.append({"fund_ref": ref, "as_of": as_of, "role": "swap", "name": str(name),
                                   "lei": blank_to_none(_first(cp, "lei")),
                                   "share": dec(_first(cp, "weight", "exposure", "percent")), "payload": cp})
    if swaps:
        out.attributes.append({"fund_ref": ref, "as_of": as_of, "domicile": None, "legal_form": None,
                               "ucits": None, "replication": "synthetic", "distribution": None, "ter": None,
                               "hedged_currency": None, "lending": None, "index_name": None, "depositary": None,
                               "payload": {}})
    return out
