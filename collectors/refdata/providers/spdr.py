"""
spdr: State Street's SPDR ETFs, US and UCITS, from the daily holdings
file each fund page links.

- Claim: the fund's ISIN or CUSIP among the keywords of State Street's
  fund finder (one list for US funds, one for UCITS funds), or its ticker
  among the US funds': a bare ticker could be any exchange's listing of
  another UCITS fund. The fund's page address ends in the file's ticker.
- Dates: none. The file is the current holdings, dated in its preamble.
- Fetch: the holdings workbook, as the page's download link serves it.
- Parse: every holding with its ISIN (UCITS) or CUSIP (US), name,
  weight, shares, currency and sector; a UCITS file adds the trading
  country and the industry. A cash line states no weight and falls into
  the fund's remainder.

Terms: State Street's site terms allow "producing print-outs for your
own personal use" and forbid publishing or redistributing the content
"for other than use for or on behalf of yourself". Its robots.txt leaves
the fund finder and the holdings files open. The file's footer adds: "The
whole or any part of this work may not be reproduced, copied or
transmitted or any of its contents disclosed to third parties without
State Street Investment Management's express written consent."
"""

from __future__ import annotations

from datetime import date
from pathlib import Path

from collectorkit import bronze

from . import table, xlsx
from .base import Parsed, Provider, ProviderError, Unavailable, read_meta, write_meta

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
        # A bare ticker names a US fund only, for a fund with no ISIN of
        # another country.
        isin = (fund.isin or "").strip().upper()
        symbol = fund.symbol if not isin or isin.startswith("US") else None
        for ident, regions in ((fund.isin, None), (fund.cusip, None), (symbol, ("us",))):
            hit = index.get((ident or "").strip().upper()) if ident else None
            if hit is None or (regions and hit[0] not in regions):
                continue
            region, d = hit
            ticker = _file_ticker(region, d.get("fundUri"))
            if ticker:
                return {"ref": fund.ref, "region": region, "ticker": ticker, "name": d.get("fundName")}
        return None

    def fetch(self, claim, as_of, outdir: Path, ctx):
        if as_of is not None:
            raise Unavailable("spdr serves the current holdings only")
        resp = ctx.client.get(FILES[claim["region"]].format(ticker=claim["ticker"]))
        if not resp.body.startswith(b"PK"):
            raise ProviderError(f"{resp.url} is not a workbook")
        outdir.mkdir(parents=True, exist_ok=True)
        bronze.atomic_write_bytes(outdir / DOC, resp.body)
        day = _as_of(xlsx.rows(resp.body))
        write_meta(outdir, provider=self.name, ref=claim["ref"], region=claim["region"], ticker=claim["ticker"],
                   as_of=day.isoformat() if day else None)
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
    pre = table.preamble(rows, start)
    lns = table.lines(rows, start, ref=ref, as_of=as_of)
    out = Parsed(lines=lns)
    out.compositions.append(table.composition(ref, as_of, lns, provider=provider, fund_name=pre.get("fund name")))
    out.funds.append({"fund_ref": ref, "isin": pre.get("isin"), "cusip": None, "symbol": pre.get("ticker symbol"),
                      "name": pre.get("fund name"), "provider": provider, "provider_id": pre.get("ticker symbol"),
                      "payload": {}})
    return out
