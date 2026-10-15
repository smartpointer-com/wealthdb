"""
ishares: BlackRock's iShares funds, from the holdings each fund page
serves. Two sites:

- us: US-listed iShares ETFs. The page's holdings file
  (`latest-holdings.csv`) is a CSV with a preamble that states the day.
  Equity lines carry tickers and no ISIN; bond lines add CUSIP and ISIN.
  The site serves the current holdings only.
- ch: iShares UCITS and Swiss funds, from the Swiss site. The holdings
  are a JSON grid without a header; the fund page's holdings table names
  the columns, and its date list names the days the site serves: the
  latest, the last month end and the last year end. `--backfill` fetches
  those.

- Claim: the fund's ISIN in either site's product list; on the US site
  also its CUSIP or ticker.
- Parse: every line with its ISIN (Swiss site) or ticker and CUSIP (US
  site), name, weight, shares, market value, sector, asset class,
  location and market currency. A local ticker of a non-US listing goes
  to the payload (table.py).

Terms: README.md, ishares.
"""

from __future__ import annotations

import html
import json
import re
from datetime import date
from pathlib import Path

from collectorkit import bronze

from . import table
from .base import Parsed, Provider, ProviderError, Unavailable, ident, read_meta, us_symbol, write_meta

HOST = "https://www.ishares.com"
SCREENERS = {
    "us": ("/us/product-screener/product-screener-v3.1.jsn",
           "/templatedata/config/product-screener-v3/data/en/us-ishares/ishares-product-screener-backend-config"),
    "ch": ("/ch/individual/en/product-screener/product-screener-v3.1.jsn",
           "/templatedata/config/product-screener-v3/data/en/ch/product-screener/"
           "ishares-product-screener-backend-config"),
}
# The holdings component of a Swiss-site fund page.
CH_HOLDINGS = "1495092304805"
CSV_DOC = "holdings.csv"
JSON_DOC = "holdings.json"


class IShares(Provider):
    name = "ishares"
    serves_funds = True

    def _index(self, ctx) -> dict:
        """identifier -> (site, product), from both product lists: the US
        site's ISINs, CUSIPs and tickers, then the Swiss site's ISINs. An
        identifier both lists name keeps the US entry."""
        def load():
            out = {}
            for site, (path, dcr) in SCREENERS.items():
                doc = ctx.client.get(HOST + path, params={"dcrPath": dcr, "siteEntryPassthrough": "true"}).json()
                for product in (doc or {}).values():
                    if not isinstance(product, dict) or not product.get("productPageUrl"):
                        continue
                    ids = [product.get("isin")]
                    if site == "us":
                        ids += [product.get("cusip"), product.get("localExchangeTicker")]
                    for i in (str(i).strip().upper() for i in ids if i):
                        out.setdefault(i, (site, product))
            return out
        return ctx.once("ishares_index", load)

    def claim(self, fund, ctx):
        index = self._index(ctx)
        # A ticker matches a US fund only (base.us_symbol).
        for i, us_only in ((ident(fund, "isin"), False), (ident(fund, "cusip"), False), (us_symbol(fund), True)):
            hit = index.get(i) if i else None
            if hit is None or (us_only and hit[0] != "us"):
                continue
            site, product = hit
            return {"ref": fund.ref, "site": site, "page": product["productPageUrl"],
                    "name": product.get("fundName")}
        return None

    def _page(self, claim, ctx) -> dict:
        """A Swiss-site fund page's holdings columns and dates."""
        def load():
            return holdings_page(ctx.client.get(HOST + claim["page"]).text())
        return ctx.once(("ishares_page", claim["page"]), load)

    def dates(self, claim, since, until, ctx):
        if claim["site"] != "ch":
            return None
        return [d for d in self._page(claim, ctx)["dates"] if since <= d <= until]

    def latest(self, claim, ctx):
        if claim["site"] != "ch":
            return None
        days = self._page(claim, ctx)["dates"]
        return max(days) if days else None

    def fetch(self, claim, as_of, outdir: Path, ctx):
        if claim["site"] == "us":
            resp = ctx.client.get(HOST + claim["page"] + "/latest-holdings.csv")
            rows = table.csv_rows(resp.text("utf-8-sig"))
            day = _csv_day(rows)
            if day is None or table.header_index(rows) is None:
                raise ProviderError(f"{resp.url} is not a holdings file")
            bronze.atomic_write_bytes(outdir / CSV_DOC, resp.body)
            write_meta(outdir, provider=self.name, ref=claim["ref"], site="us", as_of=day.isoformat())
            return day
        page = self._page(claim, ctx)
        if not page["columns"] or not page["dates"]:
            raise ProviderError(f"{claim['page']}: the page names no holdings columns or dates")
        day = as_of or max(page["dates"])
        if day not in page["dates"]:
            raise Unavailable(f"the site serves no holdings for {day}")
        params = {"tab": "all", "fileType": "json"}
        if as_of is not None:
            params["asOfDate"] = day.strftime("%Y%m%d")
        resp = ctx.client.get(f"{HOST}{claim['page']}/{CH_HOLDINGS}.ajax", params=params)
        resp.json()  # an answer that is not JSON fails the fetch
        bronze.atomic_write_bytes(outdir / JSON_DOC, resp.body)
        write_meta(outdir, provider=self.name, ref=claim["ref"], site="ch", as_of=day.isoformat(),
                   columns=page["columns"], name=claim.get("name"))
        return day

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        as_of = date.fromisoformat(meta["as_of"])
        if meta["site"] == "us":
            return parse_csv((indir / CSV_DOC).read_bytes().decode("utf-8-sig"), meta["ref"], as_of,
                             provider=self.name)
        doc = json.loads((indir / JSON_DOC).read_bytes().decode("utf-8-sig"))
        return parse_grid(doc, meta["columns"], meta["ref"], as_of, provider=self.name, fund_name=meta.get("name"))

    def probe(self, ctx) -> str:
        return f"iShares product lists: {len(self._index(ctx))} identifiers"


def _csv_day(rows: list) -> date | None:
    """The day a US-site CSV's preamble states ("Fund Holdings as of")."""
    for r in rows[:10]:
        if len(r) >= 2 and table._norm(r[0]) in ("fund holdings as of", "holdings as of"):
            return table.parse_day(r[1])
    return None


def parse_csv(text: str, ref: str, as_of: date, *, provider: str) -> Parsed:
    """A US-site holdings CSV as silver rows for fund `ref`."""
    rows = table.csv_rows(text)
    start = table.header_index(rows)
    if start is None:
        raise ProviderError("the holdings file has no header row")
    lns = table.lines(rows, start, ref=ref, as_of=as_of)
    name = rows[0][0].strip() if rows and rows[0] else None
    return Parsed(lines=lns, compositions=[table.composition(ref, as_of, lns, provider=provider, fund_name=name)])


def parse_grid(doc: dict, columns: list, ref: str, as_of: date, *, provider: str, fund_name=None) -> Parsed:
    """A Swiss-site holdings grid, read through the column names its fund
    page states, as silver rows for fund `ref`."""
    grid = doc.get("aaData")
    if not isinstance(grid, list):
        raise ProviderError("the holdings JSON has no aaData grid")
    rows = [list(columns)]
    for r in grid:
        rows.append([("" if v is None else str(v.get("raw", v.get("display", "")) if isinstance(v, dict) else v))
                     for v in r])
    if table.header_index(rows) != 0:
        raise ProviderError(f"the page's holdings columns name no holding and weight: {columns}")
    lns = table.lines(rows, 0, ref=ref, as_of=as_of)
    return Parsed(lines=lns, compositions=[table.composition(ref, as_of, lns, provider=provider, fund_name=fund_name)])


def holdings_page(page: str) -> dict:
    """The holdings table's column names and the days its date list
    offers, from a Swiss-site fund page."""
    start = page.find(f'id="c{CH_HOLDINGS}"')
    if start < 0:
        return {"columns": [], "dates": []}
    end = page.find("<!-- COMPONENT", start + 50)
    part = page[start:end if end > 0 else len(page)]
    tbl = part.find('id="allHoldingsTable"')
    columns = []
    if tbl >= 0:
        head = part[tbl:part.find("</thead>", tbl) if part.find("</thead>", tbl) > 0 else tbl + 20000]
        columns = [" ".join(html.unescape(re.sub(r"<[^>]+>", " ", th)).split())
                   for th in re.findall(r"<th[^>]*>(.*?)</th>", head, re.S)]
    days = set()
    for sel in re.findall(r'<select class="date-dropdown">(.*?)</select>', part, re.S):
        for v in re.findall(r'<option[^>]*value="(\d{8})"', sel):
            days.add(date(int(v[:4]), int(v[4:6]), int(v[6:])))
    return {"columns": columns, "dates": sorted(days)}
