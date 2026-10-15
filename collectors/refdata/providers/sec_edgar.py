"""
sec-edgar: what the SEC knows of a US-listed company, from EDGAR.

- Claim: a security whose ticker the SEC's company ticker file lists.
  A security quoted abroad (an ISIN from outside the US, or no ISIN and
  a price in another currency than US dollars) is claimed only where its
  name and the SEC's agree: a ticker abroad may be another company's in
  New York.
- Fetch: the company's submissions file, and its latest reported common
  shares outstanding (the XBRL concept dei:EntityCommonStockSharesOutstanding).
  A company that reports no such concept is no error.
- Parse: the company's name, tickers and exchanges as an identity row;
  its SIC code, its state or country of incorporation and of its
  business address (EDGAR codes, which gold reads through EDGAR's own
  code map) and its shares outstanding as facts.

Terms: README.md, sec-edgar.
"""

from __future__ import annotations

import json
import re
from datetime import date
from pathlib import Path

import fetcher
from collectorkit import bronze

from .base import Parsed, Provider, ident, read_meta, row, today_utc, write_meta
from .sec_nport import SUBMISSIONS, company_tickers

SHARES = "https://data.sec.gov/api/xbrl/companyconcept/CIK{cik:010d}/dei/EntityCommonStockSharesOutstanding.json"
SUB_DOC = "submissions.json"
SHARES_DOC = "shares.json"
DEPOSITARY_SIC = "8880"
DEPOSITARY_NAME = re.compile(r"/ADR/?\s*$", re.IGNORECASE)
FILLER = frozenset({"THE", "INC", "CORP", "CO", "LTD", "PLC", "SA", "AG", "NV", "SE", "AB", "ASA", "SPA", "LIMITED",
                    "CORPORATION", "COMPANY", "GROUP", "HOLDINGS", "HOLDING", "CLASS", "CL", "COM", "ORD", "SHS"})


def compact(ticker: str) -> str:
    """A ticker without its share-class punctuation: brokers, the SEC
    and OpenFIGI write XYZB, XYZ-B, XYZ.B and XYZ/B for one share."""
    return re.sub(r"[./\s-]", "", str(ticker or "").upper())


def depositary_registrant(sub: dict) -> bool:
    """Whether a submissions file is a depositary receipt's registrant:
    EDGAR files one under SIC 8880 or names it COMPANY/ADR."""
    return str(sub.get("sic") or "") == DEPOSITARY_SIC or bool(DEPOSITARY_NAME.search(sub.get("name") or ""))


def first_word(name) -> str:
    """The first word of a name that tells the company: no legal form,
    no filler, two letters at least."""
    for w in re.split(r"[^A-Z0-9]+", str(name or "").upper().replace("'", "")):
        if len(w) >= 2 and w not in FILLER:
            return w
    return ""


def names_agree(a, b) -> bool:
    """Whether two names are one company's: the first telling word of
    each, the one the start of the other (QUELLMARK, QUELLMARK INC); a
    word of two letters only where both are equal."""
    x, y = first_word(a), first_word(b)
    if not (x and y):
        return False
    if min(len(x), len(y)) < 3:
        return x == y
    return x.startswith(y) or y.startswith(x)


class SecEdgar(Provider):
    name = "sec-edgar"
    serves_securities = True
    needs_contact = True
    answers = frozenset({"sector", "country"})

    def _tickers(self, ctx) -> tuple:
        """ticker -> CIK as the SEC writes it, share-class tickers (XYZ-B)
        by their compact form (XYZB) besides, and CIK -> company name."""
        def load():
            doc = company_tickers(ctx)
            exact, classes, titles = {}, {}, {}
            for v in (doc.values() if isinstance(doc, dict) else doc):
                t = str(v.get("ticker") or "").upper()
                if not t:
                    continue
                cik = int(v["cik_str"])
                exact.setdefault(t, cik)
                titles.setdefault(cik, v.get("title") or "")
                if compact(t) != t:
                    classes.setdefault(compact(t), cik)
            return exact, classes, titles
        return ctx.once("edgar_tickers", load)

    def claim_security(self, security, ctx):
        symbol = ident(security, "symbol")
        exact, classes, titles = self._tickers(ctx)
        # The ticker as written first; a share class's compact form only
        # matches a share-class ticker, never a plain one it collides with.
        cik = (exact.get(symbol) or classes.get(compact(symbol))) if symbol else None
        if not cik:
            return None
        isin = ident(security, "isin")
        abroad = not isin.startswith("US") if isin else (security.currency or "USD").upper() != "USD"
        if abroad and security.name and not names_agree(security.name, titles.get(cik)):
            return None
        return {"key": security.key, "symbol": symbol, "cik": cik}

    def fetch_security(self, claim, outdir: Path, ctx):
        sub = ctx.client.get(SUBMISSIONS.format(cik=claim["cik"]))
        sub.json()  # an answer that is not JSON fails the fetch
        bronze.atomic_write_bytes(outdir / SUB_DOC, sub.body)
        try:
            bronze.atomic_write_bytes(outdir / SHARES_DOC, ctx.client.get(SHARES.format(cik=claim["cik"])).body)
        except fetcher.FetchError as e:
            if e.status != 404:
                raise
        write_meta(outdir, provider=self.name, key=claim["key"], cik=claim["cik"], as_of=today_utc().isoformat())

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        sub = json.loads((indir / SUB_DOC).read_text(encoding="utf-8-sig"))
        shares = None
        if (indir / SHARES_DOC).is_file():
            shares = json.loads((indir / SHARES_DOC).read_text(encoding="utf-8-sig"))
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
    out.securities.append(row(
        "securities", security_key=key, as_of=as_of, provider=provider, lei=sub.get("lei") or None,
        ticker=tickers[0] if tickers else None, exchange=exchanges[0] if exchanges else None, name=sub.get("name"),
        security_type=sub.get("entityType"),
        payload={k: v for k, v in {"cik": sub.get("cik"), "tickers": tickers or None, "exchanges": exchanges or None,
                                   "sic_description": sub.get("sicDescription")}.items() if v}))

    def fact(field, value, scheme, **payload):
        if value not in (None, ""):
            out.security_facts.append(row("security_facts", security_key=key, field=field, as_of=as_of,
                                          provider=provider, value=str(value), scheme=scheme, payload=payload))

    fact("sic", sub.get("sic"), "sic", description=sub.get("sicDescription"))
    fact("incorporation", sub.get("stateOfIncorporation"), "edgar")
    # A US address states its state; one abroad states its EDGAR country
    # code apart. A depositary receipt's registrant files from its
    # depositary's address, which says nothing of the company.
    business = (sub.get("addresses") or {}).get("business") or {}
    if not depositary_registrant(sub):
        fact("hq_country", business.get("stateOrCountry") or business.get("countryCode"), "edgar")
    value, end = latest_shares(shares)
    fact("shares_outstanding", value, "count", end=end)
    return out
