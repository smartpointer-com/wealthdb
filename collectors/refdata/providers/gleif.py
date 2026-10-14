"""
gleif: who issued a security, from the Global LEI Foundation's public
API.

- Claim: a security with an ISIN.
- Fetch: the LEI records the API maps that ISIN to
  (`lei-records?filter[isin]=…`), as served. A US equity ISIN the API
  maps to a company outside the US is a depositary receipt's: for it the
  ISINs of that company's LEI are fetched too (`lei-records/{lei}/isins`,
  up to ISIN_PAGES pages), so gold can find the share it stands for.
- Parse: the issuer's LEI and legal name; its legal country (the
  jurisdiction, else the legal address) and its headquarters country as
  facts; its category (GENERAL, FUND, …) in the payload; the issuer's
  other ISINs as one `issuer_isins` fact, space-separated. An ISIN the
  API does not know is still a lookup, so it is not asked again before
  max_age_days.
- Constituents: asked for a missing country, and for a missing sector
  of a stock under a US ISIN, which may be a receipt.

Terms: "The data available through the Access Service are provided
under the CC0 licence" (GLEIF's LEI data terms of use, §II.3).
"""

from __future__ import annotations

import json
import logging
from datetime import date
from pathlib import Path

import fetcher
from collectorkit import bronze

from .base import Parsed, Provider, ProviderError, read_meta, today_utc, write_meta

API = "https://api.gleif.org/api/v1/lei-records"
DOC = "lei-records.json"
ISINS_DOC = "isins.json"
ISIN_PAGE_SIZE = 200
ISIN_PAGES = 5

log = logging.getLogger("refdata.gleif")


def _isin(security) -> str | None:
    if security.isin:
        return security.isin.strip().upper()
    if security.key.startswith("isin:"):
        return security.key[5:].upper()
    return None


class Gleif(Provider):
    name = "gleif"
    serves_securities = True
    answers = frozenset({"country"})

    def wants_constituent(self, security) -> bool:
        """A constituent is asked for its country, and a stock under a US
        ISIN for its sector too: a receipt's ISIN leads to its issuer
        abroad, whose own share states one."""
        if super().wants_constituent(security):
            return True
        isin = _isin(security) or ""
        return "sector" in security.missing and security.asset_class == "public_equity" and isin.startswith("US")

    def claim_security(self, security, ctx):
        isin = _isin(security)
        if not isin:
            return None
        return {"key": security.key, "isin": isin,
                "equity": (security.asset_class or "public_equity") == "public_equity"}

    def fetch_security(self, claim, outdir: Path, ctx):
        resp = ctx.client.get(API, params={"filter[isin]": claim["isin"]},
                              headers={"Accept": "application/vnd.api+json"})
        try:
            doc = resp.json()
        except ValueError as e:
            raise ProviderError(f"{resp.url} is not JSON: {e}") from e
        outdir.mkdir(parents=True, exist_ok=True)
        bronze.atomic_write_bytes(outdir / DOC, resp.body)
        lei, country = _issuer(doc)
        if claim.get("equity") and claim["isin"].startswith("US") and lei and country and country != "US":
            try:
                isins = _lei_isins(lei, ctx)
            except (fetcher.FetchError, ValueError) as e:
                log.warning("%s: the ISINs of its issuer did not come: %s", claim["key"], e)
            else:
                bronze.atomic_write_bytes(outdir / ISINS_DOC, json.dumps({"lei": lei, "isins": isins}).encode())
        write_meta(outdir, provider=self.name, key=claim["key"], isin=claim["isin"],
                   as_of=today_utc().isoformat())

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        doc = json.loads((indir / DOC).read_text(encoding="utf-8"))
        out = parse_records(doc, meta["key"], meta["isin"], date.fromisoformat(meta["as_of"]), provider=self.name)
        if (indir / ISINS_DOC).is_file():
            isins = json.loads((indir / ISINS_DOC).read_text(encoding="utf-8"))
            out.security_facts += issuer_isins(isins, meta["key"], meta["isin"], date.fromisoformat(meta["as_of"]),
                                               provider=self.name)
        return out

    def probe(self, ctx) -> str:
        resp = ctx.client.get(API, params={"page[size]": "1"}, headers={"Accept": "application/vnd.api+json"})
        return f"GLEIF API answered {resp.status}"


def _lei_isins(lei: str, ctx) -> list:
    """The ISINs GLEIF maps to an LEI, up to ISIN_PAGES pages."""
    isins = []
    for page in range(1, ISIN_PAGES + 1):
        got = ctx.client.get(f"{API}/{lei}/isins", headers={"Accept": "application/vnd.api+json"},
                             params={"page[size]": str(ISIN_PAGE_SIZE), "page[number]": str(page)}).json()
        rows = got.get("data") or []
        isins += [(r.get("attributes") or {}).get("isin") for r in rows]
        if len(rows) < ISIN_PAGE_SIZE:
            break
    return isins


def _issuer(doc: dict):
    """The first LEI record's LEI and legal country."""
    records = doc.get("data") or []
    if not records:
        return None, None
    attrs = records[0].get("attributes") or {}
    entity = attrs.get("entity") or {}
    lei = attrs.get("lei") or records[0].get("id")
    return lei, _country(entity.get("jurisdiction")) or _country((entity.get("legalAddress") or {}).get("country"))


def issuer_isins(doc: dict, key: str, isin: str, as_of: date, *, provider: str) -> list:
    """The other ISINs of a receipt's issuer as one `issuer_isins` fact,
    sorted and space-separated (a security has one fact per field and
    day)."""
    others = sorted({str(o or "").strip().upper() for o in doc.get("isins") or []} - {isin, ""})
    if not others:
        return []
    return [{"security_key": key, "field": "issuer_isins", "as_of": as_of, "provider": provider,
             "value": " ".join(others), "scheme": "isin", "payload": {"lei": doc.get("lei")}}]


def _country(value) -> str | None:
    """A GLEIF jurisdiction or address country as its ISO country: a
    subdivision (US-DE) counts as its country."""
    v = (value or "").strip().upper()
    return v.split("-", 1)[0] if v else None


def parse_records(doc: dict, key: str, isin: str, as_of: date, *, provider: str) -> Parsed:
    """The LEI records of one ISIN as silver rows for security `key`. The
    first record is the issuer; an empty answer is a lookup that found
    nothing."""
    records = doc.get("data") or []
    out = Parsed()
    if not records:
        out.securities.append({"security_key": key, "as_of": as_of, "provider": provider, "isin": isin,
                               "cusip": None, "figi": None, "lei": None, "ticker": None, "exchange": None,
                               "name": None, "security_type": None, "listing_currency": None,
                               "payload": {"found": False}})
        return out
    lei, legal = _issuer(doc)
    entity = (records[0].get("attributes") or {}).get("entity") or {}
    hq = _country((entity.get("headquartersAddress") or {}).get("country"))
    out.securities.append({
        "security_key": key, "as_of": as_of, "provider": provider, "isin": isin, "cusip": None, "figi": None,
        "lei": lei, "ticker": None, "exchange": None, "name": (entity.get("legalName") or {}).get("name"),
        "security_type": None, "listing_currency": None,
        "payload": {k: v for k, v in {"category": entity.get("category"),
                                      "jurisdiction": entity.get("jurisdiction"),
                                      "status": entity.get("status"), "records": len(records)}.items() if v},
    })
    for field, value in (("legal_country", legal), ("hq_country", hq)):
        if value:
            out.security_facts.append({"security_key": key, "field": field, "as_of": as_of, "provider": provider,
                                       "value": value, "scheme": "iso3166", "payload": {"lei": lei}})
    return out
