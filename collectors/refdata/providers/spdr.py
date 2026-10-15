"""
spdr: State Street's SPDR ETFs, US and UCITS, from the daily holdings
file each fund page links.

- Claim: the fund's ISIN or CUSIP among the keywords of State Street's
  fund finder (one list for US funds, one for UCITS funds), or its ticker
  among the US funds': a bare ticker could be any exchange's listing of
  another UCITS fund. The fund's page address ends in the file's ticker.
- Dates: none. The file is the current holdings, dated in its preamble.
- Fetch: the holdings workbook, as the page's download link serves it.
  A workbook with no holdings header or no day is refused.
- Parse: every holding with its ISIN (UCITS) or CUSIP (US), name,
  weight, shares, currency and sector; a UCITS file adds the trading
  country and the industry. A cash line states no weight and falls into
  the fund's remainder.

Terms: README.md, spdr.
"""

from __future__ import annotations

from datetime import date
from pathlib import Path

from collectorkit import bronze

from . import table, xlsx
from .base import Parsed, Provider, ProviderError, ident, read_meta, us_symbol, write_meta

FINDER = "https://www.ssga.com/bin/v1/ssmp/fund/fundfinder"
FINDERS = {
    "us": {"country": "us", "language": "en", "role": "intermediary", "product": "etfs", "ui": "fund-finder"},
    "emea": {"country": "ch", "language": "en_gb", "role": "intermediary", "product": "etfs", "ui": "fund-finder"},
}
FILES = {
    "us": "https://www.ssga.com/us/en/intermediary/library-content/products/fund-data/etfs/us/"
          "holdings-daily-us-en-{ticker}.xlsx",
    "emea": "https://www.ssga.com/library-content/products/fund-data/etfs/emea/holdings-daily-emea-en-{ticker}.xlsx",
}
DOC = "holdings.xlsx"


def _datas(doc: dict) -> list:
    """Every fund entry of a fund finder answer, across its lists."""
    funds = ((doc.get("data") or {}).get("funds")) or {}
    out = []
    for block in funds.values():
        out += (block or {}).get("datas") or []
    return out


def _file_ticker(region: str, uri: str) -> str | None:
    """The ticker a fund's holdings file is named by: the end of its page
    address, one word for a US fund (`…-qval`), two for a UCITS one
    (`…-qeur-gy`)."""
    parts = (uri or "").rstrip("/").rsplit("/", 1)[-1].split("-")
    n = 1 if region == "us" else 2
    return "-".join(parts[-n:]).lower() if len(parts) > n else None


class Spdr(Provider):
    name = "spdr"
    serves_funds = True

    def _index(self, ctx) -> dict:
        """identifier or ticker -> (region, fund entry), from both finders."""
        def load():
            out = {}
            for region, params in FINDERS.items():
                for d in _datas(ctx.client.get(FINDER, params=params).json()):
                    for word in (w.strip().upper() for w in (d.get("keywords") or "").split(",")):
                        # A US fund's ticker wins over a UCITS listing's.
                        if word and (word not in out or region == "us"):
                            out[word] = (region, d)
            return out
        return ctx.once("spdr_index", load)

    def claim(self, fund, ctx):
        index = self._index(ctx)
        # A ticker matches a US fund only (base.us_symbol).
        for i, us_only in ((ident(fund, "isin"), False), (ident(fund, "cusip"), False), (us_symbol(fund), True)):
            hit = index.get(i) if i else None
            if hit is None or (us_only and hit[0] != "us"):
                continue
            region, d = hit
            ticker = _file_ticker(region, d.get("fundUri"))
            if ticker:
                return {"ref": fund.ref, "region": region, "ticker": ticker, "name": d.get("fundName")}
        return None

    def fetch(self, claim, as_of, outdir: Path, ctx):
        resp = ctx.client.get(FILES[claim["region"]].format(ticker=claim["ticker"]))
        if not resp.body.startswith(b"PK"):
            raise ProviderError(f"{resp.url} is not a workbook")
        rows = xlsx.rows(resp.body)
        if table.header_index(rows) is None:
            raise ProviderError(f"{resp.url} has no holdings header")
        day = _as_of(rows)
        if day is None:
            raise ProviderError(f"{resp.url} states no holdings date")
        bronze.atomic_write_bytes(outdir / DOC, resp.body)
        write_meta(outdir, provider=self.name, ref=claim["ref"], region=claim["region"], ticker=claim["ticker"],
                   as_of=day.isoformat())
        return day

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        return parse_holdings((indir / DOC).read_bytes(), meta["ref"], provider=self.name)

    def probe(self, ctx) -> str:
        return f"SPDR fund finders: {len(self._index(ctx))} identifiers"


def _as_of(rows) -> date | None:
    start = table.header_index(rows)
    pre = table.preamble(rows, start if start is not None else len(rows))
    for key in ("holdings as of", "as of", "holdings"):
        if key in pre and table.parse_day(pre[key]):
            return table.parse_day(pre[key])
    for line in pre.get("_lines", []):
        if table.parse_day(line):
            return table.parse_day(line)
    return None


def parse_holdings(data: bytes, ref: str, *, provider: str) -> Parsed:
    """One SPDR holdings workbook as silver rows for fund `ref`."""
    rows = xlsx.rows(data)
    start = table.header_index(rows)
    if start is None:
        raise ProviderError("the workbook has no holdings header")
    as_of = _as_of(rows)
    if as_of is None:
        raise ProviderError("the workbook states no holdings date")
    lns = table.lines(rows, start, ref=ref, as_of=as_of)
    name = table.preamble(rows, start).get("fund name")
    return Parsed(lines=lns, compositions=[table.composition(ref, as_of, lns, provider=provider, fund_name=name)])
