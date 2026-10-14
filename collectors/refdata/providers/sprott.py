"""
sprott: Sprott's US ETFs, from the holdings table each fund page embeds.

- Claim: the fund's ticker among the fund pages the site's home page
  links (`/{ticker}-sprott-…-etf/`).
- Dates: none. The page shows the current holdings, dated above the
  table ("As of MM/DD/YYYY").
- Fetch: the fund page, as served. The holdings download is a CSV
  written into the page as a `data:` link.
- Parse: every holding with its name, weight, shares and market value,
  and its ticker where it names a US listing. A ticker with an exchange
  code after it (`QRV CN`) is a listing abroad and moves to the payload,
  as the shared table rule does with other local tickers. The SEDOL
  column is not read. The cash line is cash.

Terms: the site links no terms of use (checked 2026-10-14); its privacy
policy covers personal data only. The holdings are the fund's own
publication, read once a run at one request a second.
"""

from __future__ import annotations

import csv
import html
import io
import re
import urllib.parse
from datetime import date, datetime
from pathlib import Path

from collectorkit import bronze

from . import table
from .base import Parsed, Provider, ProviderError, Unavailable, read_meta, write_meta

HOST = "https://sprottetfs.com"
DOC = "fund.html"
PAGE = re.compile(r'href="(/([a-z]{3,5})-sprott-[a-z0-9-]+/)"')
DATA = re.compile(r'href="data:application/csv;charset=utf-8,([^"]*)"')
AS_OF = re.compile(r"As of (\d{2}/\d{2}/\d{4})")
COLUMNS = {"name": ("security",), "ticker": ("symbol",)}


class Sprott(Provider):
    name = "sprott"
    serves_funds = True

    def _pages(self, ctx) -> dict:
        """ticker -> fund page path, from the home page."""
        def load():
            body = ctx.client.get(HOST + "/").body.decode("utf-8", "replace")
            return {t.upper(): path for path, t in PAGE.findall(body)}
        return ctx.once("sprott_pages", load)

    def claim(self, fund, ctx):
        symbol = (fund.symbol or (fund.ref[7:] if fund.ref.startswith("symbol:") else "") or "").strip().upper()
        isin = (fund.isin or "").strip().upper()
        if not symbol or (isin and not isin.startswith("US")):
            return None
        path = self._pages(ctx).get(symbol)
        return {"ref": fund.ref, "ticker": symbol, "page": path} if path else None

    def fetch(self, claim, as_of, outdir: Path, ctx):
        if as_of is not None:
            raise Unavailable("sprott serves the current holdings only")
        resp = ctx.client.get(HOST + claim["page"])
        page = resp.body.decode("utf-8", "replace")
        if not DATA.search(page):
            raise ProviderError(f"{resp.url} embeds no holdings table")
        day = _as_of(page)
        if day is None:
            raise ProviderError(f"{resp.url} states no holdings date")
        outdir.mkdir(parents=True, exist_ok=True)
        bronze.atomic_write_bytes(outdir / DOC, resp.body)
        write_meta(outdir, provider=self.name, ref=claim["ref"], ticker=claim["ticker"], as_of=day.isoformat())
        return day

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        return parse_page((indir / DOC).read_bytes().decode("utf-8", "replace"), meta["ref"], provider=self.name)

    def probe(self, ctx) -> str:
        return f"Sprott home page: {len(self._pages(ctx))} fund pages"


def _as_of(page: str) -> date | None:
    m = AS_OF.search(page)
    return datetime.strptime(m.group(1), "%m/%d/%Y").date() if m else None


def parse_page(page: str, ref: str, *, provider: str) -> Parsed:
    """One Sprott fund page's holdings table as silver rows for `ref`."""
    m = DATA.search(page)
    if not m:
        raise ProviderError("the page embeds no holdings table")
    as_of = _as_of(page)
    if as_of is None:
        raise ProviderError("the page states no holdings date")
    rows = list(csv.reader(io.StringIO(urllib.parse.unquote(html.unescape(m.group(1))))))
    start = table.header_index(rows, COLUMNS)
    if start is None:
        raise ProviderError("the holdings table has no header")
    lns = table.lines(rows, start, ref=ref, as_of=as_of, columns=COLUMNS)
    for ln in lns:
        if ln["ticker"] and " " in ln["ticker"]:
            ln["payload"]["local_ticker"], ln["ticker"] = ln["ticker"], None
        if (ln["name"] or "").lower().startswith("cash"):
            ln["asset_type"] = "Cash"
    out = Parsed(lines=lns)
    out.compositions.append(table.composition(ref, as_of, lns, provider=provider))
    out.funds.append({"fund_ref": ref, "isin": None, "cusip": None, "symbol": None, "name": None,
                      "provider": provider, "provider_id": None, "payload": {}})
    return out
