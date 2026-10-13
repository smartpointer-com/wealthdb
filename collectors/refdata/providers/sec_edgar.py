"""
sec-edgar: what the SEC knows of a US-listed company, from EDGAR.

- Claim: a security whose ticker the SEC's company ticker file lists.
- Fetch: the company's submissions file, and its latest reported common
  shares outstanding (the XBRL concept dei:EntityCommonStockSharesOutstanding).
  A company that reports no such concept is no error.
- Parse: the company's name, tickers and exchanges as an identity row;
  its SIC code, its state or country of incorporation and of its
  business address (EDGAR codes, which gold reads through EDGAR's own
  code map) and its shares outstanding as facts.

Terms: as sec-nport. "Information presented on sec.gov is considered
public information and may be copied or further distributed by users of
the web site without the SEC's permission." The SEC requires a contact
in the User-Agent and at most ten requests a second.
"""

from __future__ import annotations

import json
import re
from datetime import date
from pathlib import Path

import fetcher
from collectorkit import bronze

from .base import Parsed, Provider, ProviderError, read_meta, today_utc, write_meta
from .sec_nport import COMPANY_TICKERS, SUBMISSIONS

def compact(ticker: str) -> str:
    """A ticker without its share-class punctuation: brokers, the SEC
    and OpenFIGI write XYZB, XYZ-B, XYZ.B and XYZ/B for one share."""
    return re.sub(r"[./\s-]", "", str(ticker or "").upper())


SHARES = "https://data.sec.gov/api/xbrl/companyconcept/CIK{cik:010d}/dei/EntityCommonStockSharesOutstanding.json"
SUB_DOC = "submissions.json"
SHARES_DOC = "shares.json"


class SecEdgar(Provider):
    name = "sec-edgar"
    serves_securities = True
    needs_contact = True
    answers = frozenset({"sector", "country"})

    def _tickers(self, ctx) -> tuple:
        """ticker -> CIK as the SEC writes it, and share-class tickers
        (XYZ-B) by their compact form (XYZB) besides."""
        def load():
            doc = ctx.client.get(COMPANY_TICKERS).json()
            exact, classes = {}, {}
            for v in (doc.values() if isinstance(doc, dict) else doc):
                t = str(v.get("ticker") or "").upper()
                if not t:
                    continue
                exact.setdefault(t, int(v["cik_str"]))
                if compact(t) != t:
                    classes.setdefault(compact(t), int(v["cik_str"]))
            return exact, classes
        return ctx.once("edgar_tickers", load)

    def claim_security(self, security, ctx):
        key = security.key
        symbol = (security.symbol or (key[7:] if key.startswith("symbol:") else "") or "").strip().upper()
        exact, classes = self._tickers(ctx)
        # The ticker as written first; a share class's compact form only
        # matches a share-class ticker, never a plain one it collides with.
        cik = (exact.get(symbol) or classes.get(compact(symbol))) if symbol else None
        return {"key": key, "symbol": symbol, "cik": cik} if cik else None

    def fetch_security(self, claim, outdir: Path, ctx):
        sub = ctx.client.get(SUBMISSIONS.format(cik=claim["cik"]))
        try:
            sub.json()
        except ValueError as e:
            raise ProviderError(f"{sub.url} is not JSON: {e}") from e
        outdir.mkdir(parents=True, exist_ok=True)
        bronze.atomic_write_bytes(outdir / SUB_DOC, sub.body)
        try:
            bronze.atomic_write_bytes(outdir / SHARES_DOC, ctx.client.get(SHARES.format(cik=claim["cik"])).body)
        except fetcher.FetchError as e:
            if e.status != 404:
                raise
        write_meta(outdir, provider=self.name, key=claim["key"], cik=claim["cik"], as_of=today_utc().isoformat())

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        sub = json.loads((indir / SUB_DOC).read_text(encoding="utf-8"))
        shares = None
        if (indir / SHARES_DOC).is_file():
            shares = json.loads((indir / SHARES_DOC).read_text(encoding="utf-8"))
        return parse_company(sub, shares, meta["key"], date.fromisoformat(meta["as_of"]), provider=self.name)

    def probe(self, ctx) -> str:
        return f"SEC company ticker file: {len(self._tickers(ctx)[0])} tickers"


def latest_shares(doc: dict | None):
    """The latest reported shares outstanding and the day they stand for;
    where two filings state the same day, the later filing wins. The API
    carries the company-wide value only, so a company that reports its
    share classes one by one states none."""
    if not doc:
        return None, None
    facts = [f for unit in (doc.get("units") or {}).values() for f in unit
             if f.get("end") and f.get("val") is not None]
    if not facts:
        return None, None
    last = max(facts, key=lambda f: (f["end"], f.get("filed") or "", f.get("accn") or ""))
    return last["val"], last["end"]


def parse_company(sub: dict, shares: dict | None, key: str, as_of: date, *, provider: str) -> Parsed:
    """One company's submissions file, and its shares outstanding, as
    silver rows for security `key`."""
    tickers = sub.get("tickers") or []
    exchanges = sub.get("exchanges") or []
    out = Parsed()
    out.securities.append({
        "security_key": key, "as_of": as_of, "provider": provider, "isin": None, "cusip": None, "figi": None,
        "lei": sub.get("lei") or None, "ticker": tickers[0] if tickers else None,
        "exchange": exchanges[0] if exchanges else None, "name": sub.get("name"), "security_type": sub.get("entityType"),
        "listing_currency": None,
        "payload": {k: v for k, v in {"cik": sub.get("cik"), "tickers": tickers or None,
                                      "exchanges": exchanges or None,
                                      "sic_description": sub.get("sicDescription")}.items() if v},
    })

    def fact(field, value, scheme, **payload):
        if value not in (None, ""):
            out.security_facts.append({"security_key": key, "field": field, "as_of": as_of, "provider": provider,
                                       "value": str(value), "scheme": scheme, "payload": payload})

    fact("sic", sub.get("sic"), "sic", description=sub.get("sicDescription"))
    fact("incorporation", sub.get("stateOfIncorporation"), "edgar")
    business = (sub.get("addresses") or {}).get("business") or {}
    fact("hq_country", business.get("stateOrCountry"), "edgar")
    value, end = latest_shares(shares)
    fact("shares_outstanding", value, "count", end=end)
    return out
