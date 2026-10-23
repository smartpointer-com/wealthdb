"""
vanguard: the distributions of Vanguard's US ETFs, from each fund's
profile page on Vanguard's US investor site. It answers a fund's
dividend calendar and nothing else: no holdings, no figures.

- Claim: a fund with a US ticker whose name names Vanguard and an ETF,
  as brokers write it. No request.
- Fetch: the fund's profile page (`/investment-products/etfs/profile/
  <ticker>`), kept as page.html, dated by the day of the fetch. A page
  whose script state holds no distributions is refused, and the
  calendar stays due.
- Parse: the state's `distributions.incomeCapitalGains[]` lists each
  distribution of about the last 18 months: its type, amount per share,
  record day, reinvest day, payable day and reinvest price, the days
  mm/dd/yyyy. For an ETF the reinvest day is the ex day. The type names
  the kind: a dividend, a short- or long-term capital gain, a return of
  capital, a special dividend; a type that names none is of kind other.
  Rows of one kind and days are one event, their amounts summed, each
  kept in the payload (base.merge_events). Amounts are in US dollars. A
  list without a row states none: the calendar is answered. The list
  holds every distribution of those 18 months, so its events are
  complete (Parsed.events_complete): load drops what it no longer
  states from its oldest day on, and keeps the older ones.

Vanguard's UCITS ETFs are not read. The UK professional site's fund
page states their distributions, at an address with a page id that no
known source names for an ISIN.

Terms: README.md, vanguard.
"""

from __future__ import annotations

import html
import json
import re
import urllib.parse
from datetime import date
from pathlib import Path

from collectorkit import bronze

from . import table
from .base import (Parsed, Provider, ProviderError, amount, blank_to_none, event_row, merge_events, present, read_bytes,
                   read_meta, today_utc, us_symbol, write_meta)

PAGE = "https://investor.vanguard.com/investment-products/etfs/profile/{ticker}"
PAGE_DOC = "page.html"
ISSUER = re.compile(r"\bvanguard\b", re.I)
ETF = re.compile(r"\betf\b", re.I)
# The key the page's script state lists the distributions under.
ROWS_KEY = re.compile(r'"incomeCapitalGains"\s*:\s*')
# The kind of a distribution, by the words its type holds, the first
# match winning.
KINDS = (("capital gain", "capital_gain"), ("cap gain", "capital_gain"), ("return of capital", "return_of_capital"),
         ("special", "special"), ("dividend", "dividend"), ("income", "dividend"))
# A row's days, by their fields, as the event's columns.
DAYS = (("reInvestDate", "ex_date"), ("recordDate", "record_date"), ("payableDate", "pay_date"))


class Vanguard(Provider):
    name = "vanguard"
    serves_funds = True
    serves_compositions = False
    facts = frozenset({"dividend_calendar"})

    def claim(self, fund, ctx):
        symbol, name = us_symbol(fund), fund.name or ""
        if not symbol or not ISSUER.search(name) or not ETF.search(name):
            return None
        return {"ref": fund.ref, "symbol": symbol}

    def fetch(self, claim, as_of, outdir: Path, ctx):
        resp = ctx.client.get(PAGE.format(ticker=urllib.parse.quote(claim["symbol"].lower(), safe="")))
        if distribution_rows(resp.text()) is None:
            raise ProviderError(f"{resp.url} states no distributions")
        bronze.atomic_write_bytes(outdir / PAGE_DOC, resp.body)
        day = today_utc()
        write_meta(outdir, provider=self.name, ref=claim["ref"], symbol=claim["symbol"], as_of=day.isoformat())
        return day

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        rows = distribution_rows(read_bytes(indir, PAGE_DOC).decode("utf-8", "replace"))
        if rows is None:
            raise ProviderError(f"{indir}: the page states no distributions")
        return Parsed(dividend_events=events(rows, meta["ref"], date.fromisoformat(meta["as_of"]),
                                             provider=self.name), events_complete=True)


def distribution_rows(page: str) -> list | None:
    """The rows of a profile page's `incomeCapitalGains` list, from its
    script state as written or HTML-escaped. None where the page holds no
    such list."""
    decoder = json.JSONDecoder()
    for text in (page, html.unescape(page)):
        for m in ROWS_KEY.finditer(text):
            try:
                value, _ = decoder.raw_decode(text, m.end())
            except ValueError:
                continue
            if isinstance(value, list):
                return [r for r in value if isinstance(r, dict)]
    return None


def _kind(kind_text) -> str:
    """The event kind a distribution's type names (KINDS), else other."""
    words = " ".join(re.sub(r"[^a-z]+", " ", str(kind_text or "").lower()).split())
    return next((kind for word, kind in KINDS if word in words), "other")


def events(rows: list, ref: str, day: date, *, provider: str) -> list:
    """The dividend_events rows of a page's distribution rows, dated
    `day`, in US dollars, the payload of each its type and reinvest
    price. Rows of one kind and the same days are one event, their
    amounts summed and each part kept in the payload (base.merge_events).
    A row that states no day is none."""
    out = []
    for r in rows:
        days = {col: table.event_day(r.get(key)) for key, col in DAYS}
        if not any(days.values()):
            continue
        kind, paid = _kind(r.get("type")), amount(r.get("share"))[0]
        out.append(event_row(ref, provider, day, kind, days, amount=paid, currency="USD" if paid is not None else None,
                             payload=present({"type": blank_to_none(r.get("type")),
                                              "reinvest_price": amount(r.get("reInvestPrice"))[0]})))
    return merge_events(out)
