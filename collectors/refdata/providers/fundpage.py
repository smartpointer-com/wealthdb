"""
fundpage: the rules an issuer's fund page is read by, whichever site
serves it. A page states figures for the fund as a whole (its duration,
its yields, its P/E) and its breakdowns by credit quality and maturity;
each issuer's provider reads its own layout, and these rules turn what
it found into `characteristics` rows:

- A figure is a number. A dash, a blank, "N/A" or a zero is none: the
  sites write one of them for a figure they do not compute.
- A weighted average maturity is kept in years, one stated in days
  converted; an option-adjusted spread in basis points, one stated in
  percent converted.
- A figure the fund's asset class has no use for is none, as an equity
  fund's spread (NOT_APPLICABLE).
- A breakdown is one row per part, its label as the page writes it and
  its value the part's share in percent. A part of no share is no row.
- A page that does not come is a warning, never a failed fetch. Its
  characteristics stay due: meta.json names them in facts_missing. So
  do those of a page that came in no layout the provider knows, which
  is a warning too, so that a change of layout shows. A page in a known
  layout answers the characteristics, whatever figures it states: a
  figure it does not state is one the fund's page has none of. iShares
  checks no layout: its pages answer the characteristics only where a
  figure is found (missing()).
"""

from __future__ import annotations

import re
from decimal import Decimal
from html.parser import HTMLParser

import fetcher
from collectorkit import bronze

from .base import dec, row

# The breakdowns a page states, one row per part.
BREAKDOWN_FIELDS = ("credit_quality", "maturity_breakdown")
# The figures of a bond portfolio and those of a stock portfolio: a
# fund of the other asset class has no use for them.
BOND_FIGURES = frozenset({"effective_duration", "yield_to_maturity", "yield_to_worst", "weighted_average_coupon",
                          "weighted_average_maturity_years", "option_adjusted_spread_bp", "convexity",
                          *BREAKDOWN_FIELDS})
EQUITY_FIGURES = frozenset({"price_earnings"})
# The fields that do not apply to a fund by the asset class its page
# states, in each site's words, as label() writes them.
NOT_APPLICABLE = {"equity": BOND_FIGURES, "equities": BOND_FIGURES, "real estate": BOND_FIGURES,
                  "commodity": BOND_FIGURES | EQUITY_FIGURES, "commodities": BOND_FIGURES | EQUITY_FIGURES,
                  "fixed income": EQUITY_FIGURES, "money market": EQUITY_FIGURES}
# The HTML tags that hold no content and so open no element.
VOID_TAGS = frozenset({"area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "source", "track",
                       "wbr"})
# A figure as a page writes it: 5.84 yrs, 4.25%, 35 bps.
FIGURE = re.compile(r"(?P<number>[-+]?[\d.,]*\d)\s*(?P<unit>%|bps?|yrs?|years?|days?)?", re.I)
# The days in a year, to turn a weighted average maturity in days into
# years.
DAYS_A_YEAR = Decimal("365.25")


def label(text) -> str:
    """A label as a provider's LABELS hold it: lower case, without a
    trailing "as of" (an iShares US page names a figure's second
    appearance so)."""
    s = " ".join(str(text or "").lower().split())
    return re.sub(r"\s*\bas of$", "", s).rstrip(": ")


def number(text):
    """(number, unit) of a figure as a page writes it (6.12 yrs, 4.01%,
    35 bps, 1’234), or (None, "") where the text is no figure."""
    m = FIGURE.fullmatch(str(text).replace("’", "").replace("'", "").strip()) if text is not None else None
    return (m["number"], (m["unit"] or "").lower()) if m else (None, "")


def figure(name: str, number, unit: str):
    """(field, value) of figure `name`, or None where `number` is no
    decimal or is zero. A weighted average maturity is kept in years, one
    stated in days converted to four decimals: gold reads
    weighted_average_maturity, in days, as a money-market figure. A spread
    in percent is turned into basis points."""
    value = dec(number)
    if value is None or Decimal(value) == 0:
        return None
    if name == "weighted_average_maturity":
        if unit.startswith("day"):
            value = dec((Decimal(value) / DAYS_A_YEAR).quantize(Decimal("0.0001")))
        return "weighted_average_maturity_years", value
    if name == "option_adjusted_spread":
        return "option_adjusted_spread_bp", dec(Decimal(value) * 100) if unit == "%" else value
    return name, value


class TagStack(HTMLParser):
    """An HTML parser that keeps a page's open elements, each with the part
    a reader's start() names for it, else None. A void tag opens no
    element. An end tag closes its element and every element opened
    inside it, as a browser closes an element whose end tag is missing."""

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self._open = []

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        part = self.start(tag, (attrs.get("class") or "").split(), attrs)
        if tag not in VOID_TAGS:
            self._open.append((tag, part))

    def start(self, tag: str, classes: list, attrs: dict):
        """The part the element a start tag opens is, else None."""
        return None

    def handle_endtag(self, tag):
        for i in range(len(self._open) - 1, -1, -1):
            if self._open[i][0] == tag:
                self.closed({part for _, part in self._open[i:]})
                del self._open[i:]
                return

    def closed(self, parts: set) -> None:
        """Called with the parts of the elements an end tag closes."""

    def parts(self) -> list:
        """The parts of the open elements, outermost first."""
        return [part for _, part in self._open if part]


def not_applicable(asset_class) -> frozenset:
    """The fields a fund of `asset_class`, as a page names it, has no use
    for (NOT_APPLICABLE); none for a class no site word names."""
    return NOT_APPLICABLE.get(label(asset_class), frozenset())


def parts(field: str, pairs, day) -> list:
    """A breakdown's parts, (field, label, value, day) per (label, share)
    pair: its label as the page writes it, spaces collapsed, its share a
    decimal. A part of no label, of no share or of a zero share is none,
    and so is one whose label came before."""
    out, seen = [], set()
    for name, share in pairs:
        name, value = " ".join(str(name or "").split()), dec(share)
        if name and value is not None and Decimal(value) != 0 and name not in seen:
            seen.add(name)
            out.append((field, name, value, day))
    return out


def rows(figures: dict, breakdowns: list, ref: str, day, *, provider: str, skip=frozenset()) -> list:
    """The characteristics rows of fund `ref`: per figure, field -> (value,
    day, payload), one row dated by its own day, else `day`; then the
    breakdown parts (parts()). A figure or a breakdown whose field `skip`
    names is no row."""
    out = [row("characteristics", fund_ref=ref, as_of=when or day, provider=provider, field=field, value=value,
               payload=payload)
           for field, (value, when, payload) in figures.items() if field not in skip]
    out += [row("characteristics", fund_ref=ref, as_of=when or day, provider=provider, field=field, label=name,
                value=value)
            for field, name, value, when in breakdowns if field not in skip]
    return out


def missing(answered) -> dict:
    """meta.json's facts_missing for a fund that wants characteristics:
    none where its pages answered them; else the planner keeps them due."""
    return {} if answered else {"facts_missing": ["characteristics"]}


def due(came: bool, known: bool, ref: str, *, log, what="its fund page") -> dict:
    """meta.json's facts_missing after a fund's page was asked for: the
    characteristics stay due where the page did not come, or came in no
    layout the provider knows (a warning). A page in a known layout
    answers them."""
    if came and not known:
        log.warning("%s: %s matched no known layout", ref, what)
    return missing(came and known)


def fetch(ctx, url: str, ref: str, *, log, what="its fund page", params=None, headers=None, json=False):
    """(body, content) of a page a fund's characteristics come from: its
    decoded JSON with `json`, else its text. None where it did not come,
    with a warning: the holdings stand without it. With `json`, an answer
    that is not JSON did not come."""
    try:
        resp = ctx.client.get(url, params=params, headers=headers)
        content = resp.json() if json else resp.text()
    except fetcher.FetchError as e:
        log.warning("%s: %s did not come: %s", ref, what, e)
        return None
    return resp.body, content


def keep(ctx, url: str, outdir, name: str, ref: str, known, *, log, headers=None, json=False) -> dict:
    """Asks for a fund's page, keeps it in outdir as `name` where it came,
    and returns meta.json's facts_missing (due()). `known(content)` says
    whether the page (fetch()'s content) is in a layout the provider
    knows."""
    got = fetch(ctx, url, ref, log=log, headers=headers, json=json)
    if got is not None:
        bronze.atomic_write_bytes(outdir / name, got[0])
    return due(got is not None, got is not None and known(got[1]), ref, log=log)
