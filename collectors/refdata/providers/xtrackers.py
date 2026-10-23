"""
xtrackers: DWS's Xtrackers ETFs, from the constituent export each fund
page links, and the figures the page shows.

- Claim: a UCITS fund (a non-US ISIN) whose name names the issuer
  (Xtrackers, db x-trackers), as brokers write it. The export serves
  UCITS funds; DWS publishes no product list a plain request can read.
- Dates: none. The export is the current constituents.
- Fetch: the export workbook for the fund's ISIN. The workbook states no
  day, so the day of the fetch dates it. For a fund that wants
  characteristics or its dividend calendar, one more request: the page
  data the fund page reads (the site's pages are a script app), kept as
  page.json. The one answer serves both.
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
- Dividend calendar: the page data's table `fundinformation-dividends`
  lists the class's distributions, one per row: its year, dividend per
  share, dividend yield, record day, ex day and pay day, each day
  dd/mm/yyyy with an ISO sort value beside it. Each row is one dividend,
  in the class currency the key facts state, kept as stated even where
  its record day precedes its ex day. A fund page without the table
  answers the calendar only where its key facts name an accumulating
  class, which pays none. Else, and where no fund page came, meta.json
  names the calendar in facts_missing, with a warning. The table lists
  the class's whole history, so its events are complete
  (Parsed.events_complete); the rows of one id are one event
  (base.merge_events). The events are dated by the fetch day.

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
from collectorkit.parse import iso_date

from . import fundpage, table, xlsx
from .base import (Parsed, Provider, ProviderError, amount, event_row, facts_missing, ident, merge_events, present,
                   read_json, read_meta, today_utc, write_meta)

EXPORT = "https://etf.dws.com/etfdata/export/GBR/ENG/excel/product/constituent/{isin}/"
PAGE = "https://etf.dws.com/api/pdp/en-gb/etf/{isin}/pdpMetaTagsTealium"
DOC = "constituents.xlsx"
PAGE_DOC = "page.json"
JSON = {"Accept": "application/json"}
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
# The page data's table of a class's distributions, and its columns.
DIVIDENDS = "fundinformation-dividends"
DIVIDEND_COLUMNS = {"year": "column_0", "amount": "column_1", "dividend_yield": "column_2",
                    "record_date": "column_3", "ex_date": "column_4", "pay_date": "column_5"}
# The key facts that name a class's currency, the first stated winning,
# and its income treatment, as fundpage.label() writes them.
CURRENCY_KEYS = ("share class currency", "fund currency", "currency")
INCOME_KEY = "income treatment"
# An income treatment that pays no distribution.
ACCUMULATING = re.compile(r"capitali[sz]|accumulat", re.I)

log = logging.getLogger("refdata.xtrackers")


class Xtrackers(Provider):
    name = "xtrackers"
    serves_funds = True
    facts = frozenset({"characteristics", "dividend_calendar"})

    def claim(self, fund, ctx):
        isin = ident(fund, "isin")
        if not isin or isin.startswith("US") or not ISSUER.search(fund.name or ""):
            return None
        # A fund wanted as a proxy or an underlying wants neither, and its
        # fetch asks for no page data.
        return {"ref": fund.ref, "isin": isin, "characteristics": "characteristics" in fund.wants,
                "calendar": "dividend_calendar" in fund.wants}

    def fetch(self, claim, as_of, outdir: Path, ctx):
        resp = ctx.client.get(EXPORT.format(isin=claim["isin"]))
        if not resp.body.startswith(b"PK"):
            raise ProviderError(f"{resp.url} is not a workbook")
        table.require_header(xlsx.rows(resp.body), f"{resp.url} has no constituents header")
        bronze.atomic_write_bytes(outdir / DOC, resp.body)
        day = today_utc()
        missing = self._page(claim, outdir, ctx) if claim.get("characteristics") or claim.get("calendar") else []
        write_meta(outdir, provider=self.name, ref=claim["ref"], isin=claim["isin"], as_of=day.isoformat(),
                   **facts_missing(missing))
        return day

    def _page(self, claim, outdir: Path, ctx) -> list:
        """Asks for a fund's page data and keeps it; the facts the fund
        wants that it leaves missing: those of a page that did not come or
        holds no key facts (fundpage.due(), its warning), and the calendar
        where a fund page states no dividends (answers_calendar())."""
        got = fundpage.fetch(ctx, PAGE.format(isin=claim["isin"]), claim["ref"], log=log, headers=JSON, json=True)
        if got is not None:
            bronze.atomic_write_bytes(outdir / PAGE_DOC, got[0])
        known = got is not None and fund_page(got[1])
        due = fundpage.due(got is not None, known, claim["ref"], log=log)
        missing = due if claim.get("characteristics") else []
        if claim.get("calendar") and not (known and answers_calendar(got[1], claim["ref"])):
            missing.append("dividend_calendar")
        return missing

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        day = date.fromisoformat(meta["as_of"])
        out = parse_export((indir / DOC).read_bytes(), meta["ref"], day, provider=self.name)
        if (indir / PAGE_DOC).is_file():
            doc = read_json(indir, PAGE_DOC)
            out.characteristics = characteristics(doc, meta["ref"], day, provider=self.name)
            out.dividend_events = dividend_events(doc, meta["ref"], day, provider=self.name)
            out.events_complete = dividend_rows(doc) is not None
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
        m = DAY.search(str(_dict(d).get("text") or ""))
        if m and (day := table.slash_day(m[0], day_first=True)):
            return day
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


def dividend_rows(doc) -> list | None:
    """The rows of the page data's dividends table (DIVIDENDS), wherever
    the page data holds it: the `values` of the `table` of the item of
    that id. None where it holds no such table."""
    todo = [doc]
    while todo:
        v = todo.pop()
        if isinstance(v, dict):
            values = _dict(v.get("table")).get("values")
            if v.get("id") == DIVIDENDS and isinstance(values, list):
                return [r for r in values if isinstance(r, dict)]
            todo.extend(v.values())
        elif isinstance(v, list):
            todo.extend(v)
    return None


def _key_fact(doc, keys) -> str | None:
    """The text of the first of the key facts `keys` names (as
    fundpage.label() writes a key) that the page data states."""
    facts = {fundpage.label(k): v for k, v, _ in reversed(key_facts(doc)) if isinstance(v, str) and v.strip()}
    return next((facts[k].strip() for k in keys if k in facts), None)


def answers_calendar(doc, ref: str) -> bool:
    """Whether a fund's page data answers its calendar: it holds the
    dividends table, or its key facts name an accumulating class, which
    pays none. A page without either is a warning."""
    if dividend_rows(doc) is not None or ACCUMULATING.search(_key_fact(doc, (INCOME_KEY,)) or ""):
        return True
    log.warning("%s: its page data states no dividends table", ref)
    return False


def _cell_text(cell) -> str:
    """The text a table cell shows: a dict's value, else the cell."""
    if isinstance(cell, dict):
        cell = next((cell[k] for k in ("value", "text", "displayValue") if cell.get(k) not in (None, "")), "")
    return str(cell if cell is not None else "").strip()


def _cell_day(cell) -> date | None:
    """The day a table cell states: its ISO sortValue, else its text, day
    first (19/08/2026)."""
    if isinstance(cell, dict) and (day := iso_date(str(cell.get("sortValue") or ""))):
        return day
    text = _cell_text(cell)
    return table.slash_day(text, day_first=True) if DAY.fullmatch(text) else iso_date(text)


def _cell_amount(cell) -> tuple:
    """(amount, currency) of the dividend per share a cell states
    (base.amount): its number, and the currency code it names, if any."""
    return amount(_cell_text(cell))


def dividend_events(doc, ref: str, day: date, *, provider: str) -> list:
    """The dividend_events rows of the page data's dividends table, dated
    `day`: one dividend per row, in the currency its amount names, else
    the class currency the key facts state. A row's days are kept as
    stated, a record day before the ex day too; a row that states none is
    no event, and the rows of one id are one event (base.merge_events).
    The payload keeps the year and the dividend yield."""
    currency = _key_fact(doc, CURRENCY_KEYS)
    out = []
    for r in dividend_rows(doc) or ():
        cells = {name: r.get(column) for name, column in DIVIDEND_COLUMNS.items()}
        days = {k: _cell_day(cells[k]) for k in ("ex_date", "record_date", "pay_date")}
        if not any(days.values()):
            continue
        paid, code = _cell_amount(cells["amount"])
        out.append(event_row(ref, provider, day, "dividend", days, amount=paid,
                             currency=(code or currency) if paid else None,
                             payload=present({"year": _cell_text(cells["year"]),
                                              "dividend_yield": _cell_text(cells["dividend_yield"])})))
    return merge_events(out)
