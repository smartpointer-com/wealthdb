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
  address the finder states, kept as page.html. For a fund that wants
  its dividend calendar, the region's distributions workbook, asked for
  once a run whichever number of funds want it.
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
- Dividend calendar: State Street lists every distribution of a
  region's funds in one workbook. The US one (sheet "dividend") states
  per distribution its ex, record and payable days (mm/dd/yyyy) and the
  dividend and the short- and long-term capital gains it pays, in US
  dollars: the dividend is one event, the two gains together one
  capital gain, and a row that pays neither is one event of kind other.
  The EMEA one (sheets "Open Fund" and "Closed Fund") states the
  announcement, ex, record and payment days (dd/mm/yyyy) and the
  distribution with its currency: one dividend per row. A day stored as
  a date cell reads from its serial. A fund's rows are those of its
  CUSIP, else of its ticker (US), or of its ISIN (EMEA); the fetch keeps
  them with their sheet's header as distributions.json. A fund without
  a row has paid none, and its calendar is answered. The rows list the
  fund's whole history, so its events are complete
  (Parsed.events_complete); the rows of one id are one event
  (base.merge_events). Where the workbook did not come or is in no known
  layout, meta.json names the calendar in facts_missing. The events are
  dated by the holdings file's day.

Terms: README.md, spdr.
"""

from __future__ import annotations

import json
import logging
import re
from datetime import date, timedelta
from pathlib import Path

import fetcher
from collectorkit import bronze

from . import fundpage, table, xlsx
from .base import (Parsed, Provider, ProviderError, amount, blank_to_none, event_row, facts_missing, ident, kinds_paid,
                   listed, merge_events, present, read_json, read_meta, write_meta)

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
# The workbook of every distribution of a region's funds, and the sheets
# it lists them on.
DISTRIBUTIONS = {
    "us": ("https://www.ssga.com/library-content/products/fund-data/etfs/us/spdr-etf-historical-distributions.xlsx",
           ("dividend",)),
    "emea": ("https://www.ssga.com/library-content/products/fund-data/etfs/emea/"
             "spdr-etf-historical-distributions-emea-en.xlsx", ("Open Fund", "Closed Fund")),
}
DISTRIBUTIONS_DOC = "distributions.json"
# A distributions sheet's days, by the headers a column shows, as
# _header() writes them.
EVENT_DAYS = (("ex_date", ("ex date",)), ("record_date", ("record date",)),
              ("pay_date", ("payable date", "payment date")), ("declared_date", ("announcement date",)))
# A US sheet's amounts, by the kind of event each pays: the two gains
# together are one capital gain.
US_PARTS = (("dividend", "dividend"), ("short term capital gain", "capital_gain"),
            ("long term capital gain", "capital_gain"))
# The amount an EMEA sheet's row pays, and its currency.
EMEA_AMOUNT, EMEA_CURRENCY = "distribution", "currency"
# The columns an event reads; the payload keeps the others.
READ_COLUMNS = frozenset({*(w for _, words in EVENT_DAYS for w in words), *(n for n, _ in US_PARTS), EMEA_AMOUNT,
                          EMEA_CURRENCY})
# A date cell's serial counts the days since this one.
SERIAL_EPOCH = date(1899, 12, 30)

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


def _ids(fund, entry: dict) -> dict:
    """The fund's ISIN and CUSIP: its own, else those the keywords of its
    finder entry name; a US ISIN names its CUSIP."""
    words = [w.strip().upper() for w in (entry.get("keywords") or "").split(",")]
    isin = ident(fund, "isin") or next((w for w in words if table.ISIN_RE.match(w)), "")
    cusip = ident(fund, "cusip") or (isin[2:11] if isin.startswith("US") else "") or next(
        (w for w in words if table.CUSIP_RE.match(w) and any(ch.isdigit() for ch in w)), "")
    return {"isin": isin, "cusip": cusip}


class Spdr(Provider):
    name = "spdr"
    serves_funds = True
    facts = frozenset({"characteristics", "dividend_calendar"})

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
                # A fund wanted as a proxy or an underlying wants neither,
                # and its fetch asks for no page or distributions.
                return {"ref": fund.ref, "region": region, "ticker": ticker, "name": d.get("fundName"),
                        "page": d["fundUri"], **_ids(fund, d), "characteristics": "characteristics" in fund.wants,
                        "calendar": "dividend_calendar" in fund.wants}
        return None

    def _distributions(self, region: str, ctx) -> dict:
        """A region's distributions workbook, asked for once a run: its URL
        and, per sheet of DISTRIBUTIONS it holds, the sheet's name, header
        row and the rows below it. A workbook that holds none of them is
        an error."""
        def load():
            url, names = DISTRIBUTIONS[region]
            resp = ctx.client.get(url)
            if not resp.body.startswith(b"PK"):
                raise ProviderError(f"{resp.url} is not a workbook")
            sheets, errors = [], []
            for name in names:
                try:
                    rows = xlsx.rows(resp.body, sheet=name)
                except xlsx.XlsxError as e:
                    errors.append(e)
                    continue
                start = distributions_header(rows)
                if start is None:
                    raise ProviderError(f"{resp.url}: its sheet {name!r} has no distributions header")
                sheets.append({"sheet": name, "header": rows[start], "rows": rows[start + 1:]})
            if not sheets:
                raise ProviderError(f"{resp.url}: {errors[0]}")
            return {"url": resp.url, "sheets": sheets}
        return ctx.once(("spdr_distributions", region), load)

    def _keep_distributions(self, claim, outdir: Path, ctx) -> bool:
        """Keeps the claimed fund's rows of its region's distributions
        workbook in outdir; whether the workbook came in a layout this
        provider reads."""
        try:
            book = self._distributions(claim["region"], ctx)
        except (fetcher.FetchError, ProviderError) as e:
            log.warning("%s: its distributions workbook did not come: %s", claim["ref"], e)
            return False
        bronze.atomic_write_json(outdir / DISTRIBUTIONS_DOC, {
            "url": book["url"],
            "sheets": [{**sheet, "rows": fund_rows(sheet, claim)} for sheet in book["sheets"]]})
        return True

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
        missing = []
        if claim.get("characteristics"):
            missing += fundpage.keep(ctx, HOST + claim["page"], outdir, PAGE_DOC, claim["ref"], fund_page, log=log)
        if claim.get("calendar") and not self._keep_distributions(claim, outdir, ctx):
            missing.append("dividend_calendar")
        write_meta(outdir, provider=self.name, ref=claim["ref"], region=claim["region"], ticker=claim["ticker"],
                   as_of=day.isoformat(), **facts_missing(missing))
        return day

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        day = date.fromisoformat(meta["as_of"])
        out = parse_holdings((indir / DOC).read_bytes(), meta["ref"], provider=self.name)
        if (indir / PAGE_DOC).is_file():
            out.characteristics = characteristics((indir / PAGE_DOC).read_bytes().decode("utf-8", "replace"),
                                                  meta["ref"], day, provider=self.name)
        if (indir / DISTRIBUTIONS_DOC).is_file():
            out.dividend_events = distribution_events(read_json(indir, DISTRIBUTIONS_DOC), meta["ref"], day,
                                                      emea=meta["region"] == "emea", provider=self.name)
            out.events_complete = True
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


def _header(cell) -> str:
    """A distributions sheet's header as EVENT_DAYS and US_PARTS name it:
    lower case words, without marks or a unit (EX-DATE is ex date,
    DIVIDEND ($) dividend)."""
    return " ".join(re.sub(r"[^a-z]+", " ", str(cell or "").lower()).split())


def _columns(header: list) -> dict:
    """A sheet's column per header (_header()), the first of a name
    winning."""
    out = {}
    for i, h in enumerate(header):
        out.setdefault(_header(h), i)
    return out


def distributions_header(rows: list) -> int | None:
    """The index of a distributions sheet's header row: the first that
    names an ex day and a record day."""
    return next((i for i, r in enumerate(rows) if {"ex date", "record date"} <= {_header(c) for c in r}), None)


def _cell(r: list, col: dict, name: str) -> str:
    i = col.get(name)
    return str(r[i]).strip() if i is not None and i < len(r) else ""


def fund_rows(sheet: dict, claim: dict) -> list:
    """A distributions sheet's rows of the claimed fund: on an EMEA sheet
    those of its ISIN; on the US sheet those of its CUSIP, or, where the
    fund's or the row's CUSIP is unknown, those of its ticker."""
    col = _columns(sheet["header"])
    out = []
    for r in sheet["rows"]:
        if claim["region"] == "emea":
            mine = bool(claim.get("isin")) and _cell(r, col, "isin").upper() == claim["isin"]
        elif claim.get("cusip") and _cell(r, col, "cusip"):
            mine = _cell(r, col, "cusip").upper() == claim["cusip"]
        else:
            mine = _cell(r, col, "ticker").upper() == claim["ticker"].upper()
        if mine:
            out.append(r)
    return out


def _day(text: str, *, day_first: bool) -> date | None:
    """A day as a distributions sheet writes it: mm/dd/yyyy on the US
    sheet, dd/mm/yyyy on the EMEA ones, or a date cell's serial."""
    if re.fullmatch(r"\d{5}(?:\.0+)?", text):
        return SERIAL_EPOCH + timedelta(days=int(float(text)))
    if table.SLASH_DAY.fullmatch(text):
        return table.slash_day(text, day_first=day_first)
    return table.parse_day(text)


def _us_paid(r: list, col: dict) -> dict:
    """kind -> (amount, currency, payload) of a US row: one per kind it
    pays an amount of (US_PARTS), in US dollars, the two gains summed into
    one capital gain (base.kinds_paid); one of kind other, of no amount,
    where it pays none."""
    parts = {name: amount(_cell(r, col, name))[0] for name, _ in US_PARTS}
    gains = {"short_term": parts["short term capital gain"], "long_term": parts["long term capital gain"]}
    return {kind: (paid, "USD", gains if kind == "capital_gain" else {})
            for kind, paid in kinds_paid(parts, US_PARTS).items()} or {"other": (None, None, {})}


def _emea_paid(r: list, col: dict) -> dict:
    """kind -> (amount, currency, payload) of an EMEA row: one dividend, in
    the currency its row names, else the one its amount names."""
    paid, code = amount(_cell(r, col, EMEA_AMOUNT))
    currency = (blank_to_none(_cell(r, col, EMEA_CURRENCY)) or code) if paid else None
    return {"dividend": (paid, currency, {})}


def distribution_events(doc: dict, ref: str, day: date, *, emea: bool, provider: str) -> list:
    """The dividend_events rows of a fund's rows of a distributions
    workbook (distributions.json), dated `day`: per US row an event per
    kind it pays (_us_paid), per EMEA row a dividend (_emea_paid). A row
    that states no ex, record or pay day is none, and so is one whose
    cells an earlier row repeats, on its sheet or another: a
    distribution the workbook lists twice. The rows of one id are one
    event (base.merge_events). The payload keeps the row's other cells
    and its sheet."""
    out, seen = [], set()
    for sheet in doc.get("sheets") or []:
        col = _columns(sheet["header"])
        for r in sheet["rows"]:
            cells = tuple(sorted((h, _cell(r, col, h)) for h in col if h))
            if cells in seen:
                continue
            seen.add(cells)
            days = {f: next(filter(None, (_day(_cell(r, col, w), day_first=emea) for w in words)), None)
                    for f, words in EVENT_DAYS}
            if not (days["ex_date"] or days["record_date"] or days["pay_date"]):
                continue
            payload = {"sheet": sheet["sheet"],
                       **{h.replace(" ", "_"): _cell(r, col, h) for h in col if h and h not in READ_COLUMNS}}
            for kind, (paid, currency, extra) in (_emea_paid if emea else _us_paid)(r, col).items():
                out.append(event_row(ref, provider, day, kind, days, amount=paid, currency=currency,
                                     payload=present({**payload, **extra})))
    return merge_events(out)
