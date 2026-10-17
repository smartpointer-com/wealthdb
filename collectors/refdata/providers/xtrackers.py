"""
xtrackers: DWS's Xtrackers ETFs, from the constituent export each fund
page links.

- Claim: a UCITS fund (a non-US ISIN) whose name names the issuer
  (Xtrackers, db x-trackers), as brokers write it. The export serves
  UCITS funds; DWS publishes no product list a plain request can read.
- Dates: none. The export is the current constituents.
- Fetch: the export workbook for the fund's ISIN. The workbook states no
  day, so the day of the fetch dates it.
- Parse: every line with its ISIN, name, country, currency, exchange,
  type of security and weight. The column "Industry Classification"
  holds the sector; weights are fractions, read as percent. Cash lines
  carry pseudo-ISINs (`_CURRENCYCHF`), read as no ISIN.

A swap-replicated Xtrackers fund exports its substitute basket, not the
index: name a proxy with `always` for such a fund.

Terms: README.md, xtrackers.
"""

from __future__ import annotations

import re
from datetime import date
from pathlib import Path

from collectorkit import bronze

from . import table, xlsx
from .base import Parsed, Provider, ProviderError, ident, read_meta, today_utc, write_meta

EXPORT = "https://etf.dws.com/etfdata/export/GBR/ENG/excel/product/constituent/{isin}/"
DOC = "constituents.xlsx"
ISSUER = re.compile(r"xtrackers|x-trackers", re.I)


class Xtrackers(Provider):
    name = "xtrackers"
    serves_funds = True

    def claim(self, fund, ctx):
        isin = ident(fund, "isin")
        if not isin or isin.startswith("US") or not ISSUER.search(fund.name or ""):
            return None
        return {"ref": fund.ref, "isin": isin}

    def fetch(self, claim, as_of, outdir: Path, ctx):
        resp = ctx.client.get(EXPORT.format(isin=claim["isin"]))
        if not resp.body.startswith(b"PK"):
            raise ProviderError(f"{resp.url} is not a workbook")
        table.require_header(xlsx.rows(resp.body), f"{resp.url} has no constituents header")
        bronze.atomic_write_bytes(outdir / DOC, resp.body)
        day = today_utc()
        write_meta(outdir, provider=self.name, ref=claim["ref"], isin=claim["isin"], as_of=day.isoformat())
        return day

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        return parse_export((indir / DOC).read_bytes(), meta["ref"], date.fromisoformat(meta["as_of"]),
                            provider=self.name)


def parse_export(data: bytes, ref: str, as_of, *, provider: str) -> Parsed:
    """One Xtrackers constituent export as silver rows for fund `ref`."""
    rows = xlsx.rows(data)
    start = table.require_header(rows, "the export has no constituents header")
    out = table.parse(rows, start, ref, as_of, provider=provider, columns={"sector": ("industry classification",)},
                      fractions=True)
    for line in out.lines:
        line["industry"] = None
    return out
