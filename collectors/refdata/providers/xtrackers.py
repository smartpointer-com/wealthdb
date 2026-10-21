"""
xtrackers: DWS's Xtrackers ETFs, from the constituent export each fund
page links, and the figures the page shows.

- Claim: a UCITS fund (a non-US ISIN) whose name names the issuer
  (Xtrackers, db x-trackers), as brokers write it. The export serves
  UCITS funds; DWS publishes no product list a plain request can read.
- Dates: none. The export is the current constituents.
- Fetch: the export workbook for the fund's ISIN. The workbook states no
  day, so the day of the fetch dates it. For a fund that wants
  characteristics, one more request: the page data the fund page reads
  (the site's pages are a script app), kept as page.json.
- Parse: every line with its ISIN, name, country, currency, exchange,
  type of security and weight. The column "Industry Classification"
  holds the sector; weights are fractions, read as percent. Cash lines
  carry pseudo-ISINs (`_CURRENCYCHF`), read as no ISIN.
- Characteristics: the page's fund key facts state a bond fund's
  effective duration, yield to worst and coupon (the weighted average
  coupon). An equity fund's key facts state no figure: the P/E and the
  dividend yield a page shows are its index's, and the index key facts
  are not read. The page shows no credit-quality or maturity breakdown.
  The page names the fund's asset class (Equities, Fixed Income). A
  figure is dated by the day its key facts state ("As of" a day,
  day/month/year), else by the fetch day. Page data without the fund's
  key facts is no fund page's: where it, or no page data, came,
  meta.json names the characteristics in facts_missing (fundpage.due()).

A swap-replicated Xtrackers fund exports its substitute basket, not the
index: name a proxy with `always` for such a fund.

Terms: README.md, xtrackers.
"""

from __future__ import annotations

import logging
import re
from datetime import date
from pathlib import Path

from collectorkit import bronze

from . import fundpage, table, xlsx
from .base import Parsed, Provider, ProviderError, ident, read_json, read_meta, today_utc, write_meta

EXPORT = "https://etf.dws.com/etfdata/export/GBR/ENG/excel/product/constituent/{isin}/"
PAGE = "https://etf.dws.com/api/pdp/en-gb/etf/{isin}/pdpMetaTagsTealium"
DOC = "constituents.xlsx"
PAGE_DOC = "page.json"
ISSUER = re.compile(r"xtrackers|x-trackers", re.I)
# The figures of the fund's key facts, by the key an item shows, as
# fundpage.label() writes it.
LABELS = {
    "effective duration": "effective_duration",
    "yield to worst": "yield_to_worst",
    "coupon": "weighted_average_coupon",
}
# A day as the page writes it in English: 20/10/2026, day first.
DAY = re.compile(r"\b(\d{2})/(\d{2})/(\d{4})\b")

log = logging.getLogger("refdata.xtrackers")


class Xtrackers(Provider):
    name = "xtrackers"
    serves_funds = True
    facts = frozenset({"characteristics"})

    def claim(self, fund, ctx):
        isin = ident(fund, "isin")
        if not isin or isin.startswith("US") or not ISSUER.search(fund.name or ""):
            return None
        # A fund wanted as a proxy or an underlying wants no
        # characteristics, and its fetch asks for no page.
        return {"ref": fund.ref, "isin": isin, "characteristics": "characteristics" in fund.wants}

    def fetch(self, claim, as_of, outdir: Path, ctx):
        resp = ctx.client.get(EXPORT.format(isin=claim["isin"]))
        if not resp.body.startswith(b"PK"):
            raise ProviderError(f"{resp.url} is not a workbook")
        table.require_header(xlsx.rows(resp.body), f"{resp.url} has no constituents header")
        bronze.atomic_write_bytes(outdir / DOC, resp.body)
        day = today_utc()
        missing = {}
        if claim.get("characteristics"):
            missing = fundpage.keep(ctx, PAGE.format(isin=claim["isin"]), outdir, PAGE_DOC, claim["ref"], fund_page,
                                    log=log, headers={"Accept": "application/json"}, json=True)
        write_meta(outdir, provider=self.name, ref=claim["ref"], isin=claim["isin"], as_of=day.isoformat(),
                   **missing)
        return day

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        day = date.fromisoformat(meta["as_of"])
        out = parse_export((indir / DOC).read_bytes(), meta["ref"], day, provider=self.name)
        if (indir / PAGE_DOC).is_file():
            out.characteristics = characteristics(read_json(indir, PAGE_DOC), meta["ref"], day, provider=self.name)
        return out


def parse_export(data: bytes, ref: str, as_of, *, provider: str) -> Parsed:
    """One Xtrackers constituent export as silver rows for fund `ref`."""
    rows = xlsx.rows(data)
    start = table.require_header(rows, "the export has no constituents header")
    out = table.parse(rows, start, ref, as_of, provider=provider, columns={"sector": ("industry classification",)},
                      fractions=True)
    for line in out.lines:
        line["industry"] = None
    return out


def _dict(value) -> dict:
    return value if isinstance(value, dict) else {}


def _day(disclaimers) -> date | None:
    """The first day a list of disclaimers states."""
    for d in disclaimers if isinstance(disclaimers, list) else ():
        if m := DAY.search(str(_dict(d).get("text") or "")):
            try:
                return date(int(m[3]), int(m[2]), int(m[1]))
            except ValueError:
                continue
    return None


def _key_facts(doc) -> dict:
    """The fund's key facts section of the page data, else {}."""
    return _dict(_dict(_dict(_dict(doc).get("pdpResult")).get("pageSections")).get("keyFacts"))


def fund_page(doc) -> bool:
    """Whether page data holds the fund's key facts: the layout a fund
    page's data has, whichever figures it states."""
    return bool(_key_facts(doc))


def key_facts(doc: dict) -> list:
    """(key, value, day) of every item of the fund's key facts, the day
    its list, else its accordion, states. A key's markup (a footnote) is
    dropped."""
    out = []
    for accordion in _key_facts(doc).get("accordionItems") or []:
        accordion = _dict(accordion)
        for part in [_dict(accordion.get("list"))] + [_dict(b) for b in accordion.get("listBundle") or []]:
            day = _day(part.get("disclaimers")) or _day(accordion.get("disclaimers"))
            for item in part.get("items") or []:
                item = _dict(item)
                out.append((re.sub(r"<[^>]*>", "", str(item.get("key") or "")), item.get("value"), day))
    return out


def asset_class(doc: dict) -> str:
    """The asset class the page's tracking data names, else "": a list of
    several names none."""
    named = _dict(_dict(_dict(doc).get("tealium")).get("attributes")).get("product_category")
    if isinstance(named, list):
        named = named[0] if len(named) == 1 else None
    return named if isinstance(named, str) else ""


def characteristics(doc: dict, ref: str, day: date, *, provider: str) -> list:
    """The characteristics rows of fund `ref` from its page data: the
    figures of its key facts (LABELS), each dated by the day its key facts
    state, else `day`. A figure the fund's asset class has no use for
    (fundpage.NOT_APPLICABLE) is no row."""
    figures = {}
    for key, value, when in key_facts(doc):
        name = LABELS.get(fundpage.label(key))
        got = fundpage.figure(name, *fundpage.number(value)) if name and isinstance(value, str) else None
        if got is not None and got[0] not in figures:
            figures[got[0]] = (got[1], when, {"source": "page", "name": key})
    return fundpage.rows(figures, [], ref, day, provider=provider, skip=fundpage.not_applicable(asset_class(doc)))
