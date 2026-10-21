"""
spdr: State Street's SPDR ETFs, US and UCITS, from the daily holdings
file each fund page links and the figures the page states.

- Claim: the fund's ISIN or CUSIP among the keywords of State Street's
  fund finder (one list for US funds, one for UCITS funds), or its ticker
  among the US funds': a bare ticker could be any exchange's listing of
  another UCITS fund. The fund's page address ends in the file's ticker.
- Dates: none. The file is the current holdings, dated in its preamble.
- Fetch: the holdings workbook, as the page's download link serves it.
  A workbook with no holdings header or no day is refused. For a fund
  that wants characteristics, one more request: the fund page, at the
  address the finder states, kept as page.html.
- Parse: every holding with its ISIN (UCITS) or CUSIP (US), name,
  weight, shares, currency and sector; a UCITS file adds the trading
  country and the industry. A cash line states no weight and falls into
  the fund's remainder.
- Characteristics: the page's "Fund Characteristics" table states a
  bond fund's average coupon, average maturity in years, yields to
  maturity and worst, option-adjusted (UCITS: effective) duration,
  option-adjusted spread in basis points and convexity; its "Yields"
  table any fund's 30-day SEC yield (US) and distribution yield. An
  equity fund's table states its P/E only as a forecast for its next
  fiscal year (FY1), not the trailing P/E price_earnings holds, so no
  P/E is read and an equity fund's page fills its yields only. The
  "Index" tables are the benchmark's and are not read. The page's chart
  data, JSON in hidden inputs, holds the fund's credit-quality breakdown
  and maturity ladder, and its quick info names the fund's asset class.
  A UCITS page writes a decimal comma: the page names its decimal point
  and thousands mark, and its figures are read through them. A figure
  is dated by the day its table's heading states, a breakdown by its own
  day, else by the holdings file's day. A page that holds no table of
  the fund's own figures is no fund page: where it, or no page, came,
  meta.json names the characteristics in facts_missing
  (fundpage.due()).

Terms: README.md, spdr.
"""

from __future__ import annotations

import json
import logging
import re
from datetime import date
from pathlib import Path

from collectorkit import bronze

from . import fundpage, table, xlsx
from .base import Parsed, Provider, ProviderError, listed, read_meta, write_meta

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
HOST = "https://www.ssga.com"
DOC = "holdings.xlsx"
PAGE_DOC = "page.html"

# The tables of a page that state the fund's own figures, by heading, as
# fundpage.label() writes it.
SECTIONS = ("fund characteristics", "yields")
# The figures of those tables, by the label a row shows, as
# fundpage.label() writes it.
LABELS = {
    "average coupon": "weighted_average_coupon",
    "average maturity in years": "weighted_average_maturity",
    "yield to maturity": "yield_to_maturity",
    "average yield to worst": "yield_to_worst",
    "yield to worst": "yield_to_worst",
    "option adjusted duration": "effective_duration",
    "effective duration": "effective_duration",
    "option adjusted spread": "option_adjusted_spread",
    "effective convexity": "convexity",
    "convexity": "convexity",
    "30 day sec yield": "sec_yield",
    "fund distribution yield": "distribution_yield",
    "distribution yield": "distribution_yield",
}
# The hidden inputs that hold a breakdown's chart data, by field: the US
# and the UCITS pages name the credit-quality one apart.
BREAKDOWNS = {"credit_quality": ("quality-breakdown-fund", "fund-quality-breakdown"),
              "maturity_breakdown": ("fund-maturity-ladder",)}

log = logging.getLogger("refdata.spdr")


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
    facts = frozenset({"characteristics"})

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
        for region, d in listed(self._index(ctx), fund, "us"):
            ticker = _file_ticker(region, d.get("fundUri"))
            if ticker:
                # A fund wanted as a proxy or an underlying wants no
                # characteristics, and its fetch asks for no page.
                return {"ref": fund.ref, "region": region, "ticker": ticker, "name": d.get("fundName"),
                        "page": d["fundUri"], "characteristics": "characteristics" in fund.wants}
        return None

    def fetch(self, claim, as_of, outdir: Path, ctx):
        resp = ctx.client.get(FILES[claim["region"]].format(ticker=claim["ticker"]))
        if not resp.body.startswith(b"PK"):
            raise ProviderError(f"{resp.url} is not a workbook")
        rows = xlsx.rows(resp.body)
        start = table.require_header(rows, f"{resp.url} has no holdings header")
        day = _as_of(table.preamble(rows, start))
        if day is None:
            raise ProviderError(f"{resp.url} states no holdings date")
        bronze.atomic_write_bytes(outdir / DOC, resp.body)
        missing = {}
        if claim.get("characteristics"):
            missing = fundpage.keep(ctx, HOST + claim["page"], outdir, PAGE_DOC, claim["ref"], fund_page, log=log)
        write_meta(outdir, provider=self.name, ref=claim["ref"], region=claim["region"], ticker=claim["ticker"],
                   as_of=day.isoformat(), **missing)
        return day

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        out = parse_holdings((indir / DOC).read_bytes(), meta["ref"], provider=self.name)
        if (indir / PAGE_DOC).is_file():
            out.characteristics = characteristics((indir / PAGE_DOC).read_bytes().decode("utf-8", "replace"),
                                                  meta["ref"], date.fromisoformat(meta["as_of"]), provider=self.name)
        return out

    def probe(self, ctx) -> str:
        return f"SPDR fund finders: {len(self._index(ctx))} identifiers"


def _as_of(pre: dict) -> date | None:
    """The holdings day a workbook's preamble states."""
    for key in ("holdings as of", "as of", "holdings"):
        if d := table.parse_day(pre.get(key)):
            return d
    for line in pre.get("_lines", []):
        if d := table.parse_day(line):
            return d
    return None


def parse_holdings(data: bytes, ref: str, *, provider: str) -> Parsed:
    """One SPDR holdings workbook as silver rows for fund `ref`."""
    rows = xlsx.rows(data)
    start = table.require_header(rows, "the workbook has no holdings header")
    pre = table.preamble(rows, start)
    as_of = _as_of(pre)
    if as_of is None:
        raise ProviderError("the workbook states no holdings date")
    return table.parse(rows, start, ref, as_of, provider=provider, fund_name=pre.get("fund name"))


class _Page(fundpage.TagStack):
    """A fund page's key-value tables and hidden inputs. A table belongs
    to the `comp-title` heading above it: the heading's text, without its
    `date` span, and that span's text. A table row is the text of its
    `label` header cell, without the cell's `info` tooltip, and of its
    `data` cell. A hidden input is its id and value."""
    TEXT = ("date", "title", "label", "data")

    def __init__(self):
        super().__init__()
        self.sections, self.inputs = [], {}

    def start(self, tag, classes, attrs):
        if tag == "input" and attrs.get("type") == "hidden" and attrs.get("id"):
            self.inputs[attrs["id"]] = attrs.get("value") or ""
        if tag == "h2" and "comp-title" in classes:
            self.sections.append({"title": [], "date": [], "rows": []})
            return "title"
        if tag in ("script", "style", "svg", "button") or (tag == "span" and "info" in classes):
            return "skip"
        if tag == "span" and "date" in classes:
            return "date"
        if tag == "tr" and self.sections:
            self.sections[-1]["rows"].append({"label": [], "data": []})
        elif tag == "th" and "label" in classes:
            return "label"
        elif tag == "td" and "data" in classes:
            return "data"
        return None

    def handle_data(self, data):
        parts = self.parts()
        if not self.sections or "skip" in parts:
            return
        section = self.sections[-1]
        part = next((k for k in self.TEXT if k in parts), None)
        if part in ("date", "title") and "title" in parts:
            section[part].append(data)
        elif part in ("label", "data") and section["rows"]:
            section["rows"][-1][part].append(data)


def _text(chunks) -> str:
    return " ".join("".join(chunks).split())


def page_tables(page: str) -> tuple:
    """(tables, inputs) of a fund page: per table its heading, the day the
    heading states and its rows as (label, data); and the page's hidden
    inputs by id."""
    parser = _Page()
    parser.feed(page)
    parser.close()
    tables = [(_text(s["title"]), table.parse_day(_text(s["date"])),
               [(_text(r["label"]), _text(r["data"])) for r in s["rows"] if _text(r["label"]) and _text(r["data"])])
              for s in parser.sections]
    return tables, parser.inputs


def _plain(text, point: str | None, mark: str | None) -> str:
    """A figure as the page writes it, with its thousands mark dropped and
    a point for its decimal point: on a page of decimal commas and spaced
    thousands, 6,35 reads 6.35 and 1 045 reads 1045. A page that names no
    decimal point has its figures read as written, so a comma that may be
    either mark is none (base.dec())."""
    s = " ".join(str(text or "").split())
    if not point:
        return s
    if mark and not mark.strip():
        s = re.sub(r"(?<=\d) (?=\d)", "", s)
    elif mark and mark != point:
        s = s.replace(mark, "")
    return s.replace(point, ".")


def _input(inputs: dict, key: str) -> dict:
    """The JSON a hidden input holds, else {}."""
    try:
        doc = json.loads(inputs.get(key) or "{}")
    except ValueError:
        return {}
    return doc if isinstance(doc, dict) else {}


def _value(entry, *keys):
    """The value at entry[key][key]…, else None."""
    for k in keys:
        entry = entry.get(k) if isinstance(entry, dict) else None
    return entry


def table_figures(tables: list, point: str, mark: str) -> dict:
    """field -> (value, day, payload) of each figure the fund's own tables
    (SECTIONS) state, dated by the day their heading states. The first
    row that states a figure wins."""
    out = {}
    for heading, day, rows in tables:
        if fundpage.label(heading) not in SECTIONS:
            continue
        for name, data in rows:
            field = LABELS.get(fundpage.label(name))
            got = fundpage.figure(field, *fundpage.number(_plain(data, point, mark))) if field else None
            if got is not None and got[0] not in out:
                out[got[0]] = (got[1], day, {"source": "page", "name": name})
    return out


def breakdowns(inputs: dict, point: str, mark: str) -> list:
    """The parts of the fund's breakdowns its chart data (BREAKDOWNS)
    states: per part its name and its share, the raw `originalValue`
    where it is given, dated by the chart's day. A field's first input
    that holds parts is read, and no other."""
    out = []
    for field, keys in BREAKDOWNS.items():
        doc = next((d for d in (_input(inputs, k) for k in keys) if d.get("attrArray")), {})
        day = table.parse_day(doc.get("asOfDateSimple")) or table.parse_day(doc.get("asOfDate"))
        pairs = [(_value(a, "name", "value"),
                  _value(a, "weight", "originalValue") or _plain(_value(a, "weight", "value"), point, mark))
                 for a in doc.get("attrArray") or []]
        out += fundpage.parts(field, pairs, day)
    return out


def asset_class(inputs: dict) -> str:
    """The asset class the page's quick info states, else ""."""
    return _value(_input(inputs, "fund-quick-info"), "attrs", "fund-assetclass", "value") or ""


def fund_page(page: str) -> bool:
    """Whether a page holds a table of the fund's own figures (SECTIONS):
    the layout a fund page has, whichever figures it states."""
    return any(fundpage.label(heading) in SECTIONS for heading, _, _ in page_tables(page)[0])


def characteristics(page: str, ref: str, day: date, *, provider: str) -> list:
    """The characteristics rows of fund `ref` from its page: the figures
    of its own tables and its breakdowns, each dated by the day the page
    states for it, else `day`. A figure the fund's asset class has no use
    for (fundpage.NOT_APPLICABLE) is no row."""
    tables, inputs = page_tables(page)
    point, mark = inputs.get("decimal-point"), inputs.get("thousands-sep")
    return fundpage.rows(table_figures(tables, point, mark), breakdowns(inputs, point, mark), ref, day,
                         provider=provider, skip=fundpage.not_applicable(asset_class(inputs)))
