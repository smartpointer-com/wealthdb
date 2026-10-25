"""
gleif: who issued a security, from the Global LEI Foundation's public
API.

- Claim: a security with an ISIN.
- Fetch: the LEI records the API maps that ISIN to
  (`lei-records?filter[isin]=…`), as served. A US equity ISIN the API
  maps to a company outside the US is a depositary receipt's: for it the
  ISINs of that company's LEI are fetched too (`lei-records/{lei}/isins`,
  up to ISIN_PAGES pages), so gold can find the share it stands for.
- Parse: the issuer's LEI and its name in Latin script (_latin_name);
  its legal country (the jurisdiction, else the legal address) and its
  headquarters country as facts; its category (GENERAL, FUND, …) in the
  payload, with the legal name where the name differs from it; the
  issuer's other ISINs as one `issuer_isins` fact, space-separated. An
  ISIN the API does not know is still a lookup, so it is not asked again
  before max_age_days.
- Constituents: asked for a missing country, and for a missing sector
  of a stock under a US ISIN, which may be a receipt.

Terms: README.md, gleif.
"""

from __future__ import annotations

import json
import logging
import unicodedata
from datetime import date
from pathlib import Path

import fetcher
from collectorkit import bronze

from .base import Parsed, Provider, ident, present, read_json, read_meta, row, today_utc, write_meta

API = "https://api.gleif.org/api/v1/lei-records"
DOC = "lei-records.json"
ISINS_DOC = "isins.json"
ISIN_PAGE_SIZE = 200
ISIN_PAGES = 5
HEADERS = {"Accept": "application/vnd.api+json"}
# The names a record may give in Latin script for a legal name in
# another, in the order they are preferred, as (type, English only):
# the legal name in English (otherNames), the way brokers and fund
# files print it; then GLEIF's ASCII transliterations
# (transliteratedOtherNames), the one the registrant prefers first; then
# the legal name in any other language (otherNames), or in none stated.
LATIN_NAME_TYPES = (("ALTERNATIVE_LANGUAGE_LEGAL_NAME", True), ("PREFERRED_ASCII_TRANSLITERATED_LEGAL_NAME", False),
                    ("AUTO_ASCII_TRANSLITERATED_LEGAL_NAME", False), ("ALTERNATIVE_LANGUAGE_LEGAL_NAME", False))

log = logging.getLogger("refdata.gleif")


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
        return ("sector" in security.missing and security.asset_class == "public_equity"
                and ident(security, "isin").startswith("US"))

    def claim_security(self, security, ctx):
        isin = ident(security, "isin")
        if not isin:
            return None
        return {"key": security.key, "isin": isin,
                "equity": (security.asset_class or "public_equity") == "public_equity"}

    def fetch_security(self, claim, outdir: Path, ctx):
        resp = ctx.client.get(API, params={"filter[isin]": claim["isin"]}, headers=HEADERS)
        doc = resp.json()
        bronze.atomic_write_bytes(outdir / DOC, resp.body)
        lei, country = _issuer(doc)
        if claim.get("equity") and claim["isin"].startswith("US") and lei and country and country != "US":
            try:
                isins = _lei_isins(lei, ctx)
            except fetcher.FetchError as e:
                log.warning("%s: the ISINs of its issuer did not come: %s", claim["key"], e)
            else:
                bronze.atomic_write_bytes(outdir / ISINS_DOC, json.dumps({"lei": lei, "isins": isins}).encode())
        write_meta(outdir, provider=self.name, key=claim["key"], isin=claim["isin"],
                   as_of=today_utc().isoformat())

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        as_of = date.fromisoformat(meta["as_of"])
        out = parse_records(read_json(indir, DOC), meta["key"], meta["isin"], as_of, provider=self.name)
        if (indir / ISINS_DOC).is_file():
            out.security_facts += issuer_isins(read_json(indir, ISINS_DOC), meta["key"], meta["isin"], as_of,
                                               provider=self.name)
        return out

    def probe(self, ctx) -> str:
        resp = ctx.client.get(API, params={"page[size]": "1"}, headers=HEADERS)
        return f"GLEIF API answered {resp.status}"


def _lei_isins(lei: str, ctx) -> list:
    """The ISINs GLEIF maps to an LEI, up to ISIN_PAGES pages."""
    isins = []
    for page in range(1, ISIN_PAGES + 1):
        got = ctx.client.get(f"{API}/{lei}/isins", headers=HEADERS,
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
    return [row("security_facts", security_key=key, field="issuer_isins", as_of=as_of, provider=provider,
                value=" ".join(others), scheme="isin", payload={"lei": doc.get("lei")})]


def _country(value) -> str | None:
    """A GLEIF jurisdiction or address country as its ISO country: a
    subdivision (US-DE) counts as its country."""
    v = (value or "").strip().upper()
    return v.split("-", 1)[0] if v else None


def _latin(text: str) -> bool:
    """Whether every letter of `text` is in Latin script: below the Greek
    block (ª and º among them), or named LATIN. A full-width letter is
    not: gold reads names in ASCII form, and a full-width one would read
    as no name at all."""
    return all(ch < "\u0370" or ("LATIN" in (n := unicodedata.name(ch, "")) and not n.startswith("FULLWIDTH"))
               for ch in text if ch.isalpha())


def _english(language) -> bool:
    """Whether a language tag (en, en-US) names English."""
    tag = (language or "").strip().lower()
    return tag == "en" or tag.startswith("en-")


def _latin_name(entity: dict):
    """The issuer's name in Latin script, and its legal name where the
    two differ: a legal name in another script reads as the first of the
    record's other names in LATIN_NAME_TYPES order (English where the
    rung asks for it) that has a letter and is in Latin script, else as
    itself."""
    legal = (entity.get("legalName") or {}).get("name")
    if not legal or _latin(legal):
        return legal, None
    others = [*(entity.get("transliteratedOtherNames") or []), *(entity.get("otherNames") or [])]
    for kind, english in LATIN_NAME_TYPES:
        for other in others:
            name = (other.get("name") or "").strip()
            if (other.get("type") == kind and (not english or _english(other.get("language")))
                    and any(ch.isalpha() for ch in name) and _latin(name)):
                return name, legal
    return legal, None


def parse_records(doc: dict, key: str, isin: str, as_of: date, *, provider: str) -> Parsed:
    """The LEI records of one ISIN as silver rows for security `key`. The
    first record is the issuer; an empty answer is a lookup that found
    nothing."""
    records = doc.get("data") or []
    out = Parsed()
    if not records:
        out.securities.append(row("securities", security_key=key, as_of=as_of, provider=provider, isin=isin,
                                  payload={"found": False}))
        return out
    lei, legal_country = _issuer(doc)
    entity = (records[0].get("attributes") or {}).get("entity") or {}
    hq = _country((entity.get("headquartersAddress") or {}).get("country"))
    name, legal_name = _latin_name(entity)
    out.securities.append(row(
        "securities", security_key=key, as_of=as_of, provider=provider, isin=isin, lei=lei, name=name,
        payload=present({"category": entity.get("category"), "jurisdiction": entity.get("jurisdiction"),
                         "status": entity.get("status"), "records": len(records), "legal_name": legal_name})))
    for field, value in (("legal_country", legal_country), ("hq_country", hq)):
        if value:
            out.security_facts.append(row("security_facts", security_key=key, field=field, as_of=as_of,
                                          provider=provider, value=value, scheme="iso3166", payload={"lei": lei}))
    return out
