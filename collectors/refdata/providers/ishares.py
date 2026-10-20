"""
ishares: BlackRock's iShares funds, from the holdings and the
characteristics each fund page serves. Two sites:

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
  also its CUSIP or ticker. The claim's `ids` are the identifiers the
  list states for the fund (base.py); a US claim also carries the fund's
  entry in the list.
- Parse: every line with its ISIN (Swiss site) or ticker and CUSIP (US
  site), name, weight, shares, market value, sector, asset class,
  location and market currency. A local ticker of a non-US listing goes
  to the payload (table.py). A bond line adds its duration, modified
  duration, yields to maturity, worst and call, real duration and real
  yield, the exchange rate it was valued at and its accrual date. So a
  composition answers the fund's line durations (`composition_facts`).
- Characteristics: the fund page states the fund's effective duration,
  average yield to maturity, yield to worst, distribution yield, 30-day
  SEC yield, 12-month trailing yield, weighted average maturity and
  coupon, option-adjusted spread (in basis points) and convexity, and an
  equity fund's P/E ratio. A weighted average maturity is kept in years:
  one in days is converted at 365.25 days a year, and one without a unit
  reads as years. Both sites name a figure alike. A figure written as a
  dash or a zero is none, and so is one that does not apply to the
  fund's asset class (an equity fund's spread): the sites write either
  for a figure they do not compute. A US page is a script
  app: its state, HTML-escaped in the page, holds one data point per
  figure and an exposure component with the credit-quality and maturity
  breakdowns. A Swiss page shows one data item per figure. Each figure
  is dated by the day the page states for it, else by the day of the
  site's newest holdings.
- Fetch: for a fund that wants characteristics, a US fetch makes one
  more request, for the page. It also keeps the fund's product-list
  entry, which fills a figure the page lacks; the page's JSON-LD fills
  one neither states. A Swiss fetch keeps the page it reads the holdings
  columns from. Where a fund wants characteristics and the page did not
  come or the files state none, meta.json names them in facts_missing,
  so they stay due.

Terms: README.md, ishares.
"""

from __future__ import annotations

import html
import json
import logging
import re
from datetime import date
from decimal import Decimal
from html.parser import HTMLParser
from pathlib import Path

import fetcher
from collectorkit import bronze

from . import table
from .base import Parsed, Provider, ProviderError, Unavailable, dec, listed, read_json, read_meta, row, write_meta

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
PAGE_DOC = "page.html"
PRODUCT_DOC = "product.json"

# The figures a fund page states, by the name both sites give them: a US
# data point's `name`, a Swiss data item's `col-<name>` class.
NAMES = {
    "modelOad": "effective_duration",
    "fxHedgedYield": "yield_to_maturity",  # the average yield to maturity
    "yieldToWorst": "yield_to_worst",
    "distYield": "distribution_yield",
    "distributionYield": "distribution_yield",
    "thirtyDaySecYield": "sec_yield",
    "twelveMonTrlYld": "trailing_yield",
    "weightedAvgCouponFi": "weighted_average_coupon",
    "weightedAvgLife": "weighted_average_maturity",  # its label: Weighted Avg Maturity
    "optionAdjustedSpread": "option_adjusted_spread",
    "convexity": "convexity",
    "priceEarnings": "price_earnings",
}
# The same figures by the label a page shows, as _label() writes it, for
# a figure under a name NAMES does not hold.
LABELS = {
    "effective duration": "effective_duration",
    "average yield to maturity": "yield_to_maturity",
    "yield to worst": "yield_to_worst",
    "distribution yield": "distribution_yield",
    "30 day sec yield": "sec_yield",
    "12m trailing yield": "trailing_yield",
    "12 month trailing dividend distribution yield": "trailing_yield",
    "weighted avg coupon": "weighted_average_coupon",
    "weighted avg maturity": "weighted_average_maturity",
    "option adjusted spread": "option_adjusted_spread",
    "convexity": "convexity",
    "p/e ratio": "price_earnings",
}
# The breakdowns of a US page's exposure component, by container.
BREAKDOWNS = {"rating": "credit_quality", "maturity": "maturity_breakdown"}
# The figures of a bond portfolio and those of a stock portfolio, and
# the fields that do not apply to a fund by the asset class its page or
# product-list entry states, as _label() writes it.
BOND_FIGURES = frozenset({"effective_duration", "yield_to_maturity", "yield_to_worst", "weighted_average_coupon",
                          "weighted_average_maturity_years", "option_adjusted_spread_bp", "convexity",
                          *BREAKDOWNS.values()})
EQUITY_FIGURES = frozenset({"price_earnings"})
NOT_APPLICABLE = {"equity": BOND_FIGURES, "real estate": BOND_FIGURES, "commodity": BOND_FIGURES | EQUITY_FIGURES,
                  "fixed income": EQUITY_FIGURES, "money market": EQUITY_FIGURES}
# A figure as a page writes it: 5.84 yrs, 4.25%, 35 bps.
FIGURE = re.compile(r"(?P<number>[-+]?[\d.,]*\d)\s*(?P<unit>%|bps?|yrs?|years?|days?)?", re.I)
# A product-list entry's key words per figure, as _key() writes keys
# (lower case letters and digits): a key that holds one of a figure's
# words and none of NOT_FIGURE_KEYS states that figure.
PRODUCT_KEYS = (
    ("effective_duration", ("effectiveduration",)),
    ("yield_to_worst", ("yieldtoworst",)),
    ("yield_to_maturity", ("yieldtomaturity", "ytm")),
    ("distribution_yield", ("distributionyield", "distyield")),
    ("sec_yield", ("secyield",)),
    ("trailing_yield", ("trailingyield", "trlyield", "trlyld")),
    ("weighted_average_maturity", ("avgmaturity", "averagematurity")),
    ("weighted_average_coupon", ("avgcoupon", "averagecoupon")),
    ("option_adjusted_spread", ("optionadjustedspread",)),
    ("convexity", ("convexity",)),
    ("price_earnings", ("priceearnings",)),
)
NOT_FIGURE_KEYS = ("asof", "date", "unsub", "benchmark", "index", "real", "monthend", "quarterend")
# The days in a year, to turn a weighted average maturity in days into
# years.
DAYS_A_YEAR = Decimal("365.25")

log = logging.getLogger("refdata.ishares")


class IShares(Provider):
    name = "ishares"
    serves_funds = True
    facts = frozenset({"characteristics"})
    composition_facts = frozenset({"line_durations"})

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
        hit = next(listed(self._index(ctx), fund, "us"), None)
        if hit is None:
            return None
        site, product = hit
        ids = {"isin": product.get("isin")}
        out = {"ref": fund.ref, "site": site, "page": product["productPageUrl"], "name": product.get("fundName"),
               # A fund wanted as a proxy or an underlying wants none, and a
               # US fetch then asks for no page.
               "characteristics": "characteristics" in fund.wants}
        if site == "us":
            ids.update(cusip=product.get("cusip"), symbol=product.get("localExchangeTicker"))
            out["product"] = product
        out["ids"] = {k: str(v).strip().upper() for k, v in ids.items() if v}
        return out

    def _html(self, claim, ctx) -> bytes:
        """A Swiss-site fund page as the site serves it, asked for once per
        run: the holdings columns, the dates and every day's folder take
        it."""
        return ctx.once(("ishares_html", claim["page"]), lambda: ctx.client.get(HOST + claim["page"]).body)

    def _page(self, claim, ctx) -> dict:
        """A Swiss-site fund page's holdings columns and dates."""
        def load():
            return holdings_page(self._html(claim, ctx).decode("utf-8", "replace"))
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
            missing = {}
            if claim["characteristics"]:
                product = claim["product"]
                bronze.atomic_write_json(outdir / PRODUCT_DOC, product)
                try:
                    page = ctx.client.get(HOST + claim["page"]).body
                except fetcher.FetchError as e:
                    log.warning("%s: its fund page did not come: %s", claim["ref"], e)
                    missing = _missing([])
                else:
                    bronze.atomic_write_bytes(outdir / PAGE_DOC, page)
                    missing = _missing(characteristics(page.decode("utf-8", "replace"), product, claim["ref"], day,
                                                       provider=self.name))
            write_meta(outdir, provider=self.name, ref=claim["ref"], site="us", as_of=day.isoformat(), **missing)
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
        html_page = self._html(claim, ctx)
        bronze.atomic_write_bytes(outdir / PAGE_DOC, html_page)
        # The page states today's characteristics whichever day's holdings
        # it serves: a figure it states no day for is the newest day's.
        page_as_of = max(page["dates"])
        missing = _missing(characteristics(html_page.decode("utf-8", "replace"), {}, claim["ref"], page_as_of,
                                           provider=self.name)) if claim["characteristics"] else {}
        write_meta(outdir, provider=self.name, ref=claim["ref"], site="ch", as_of=day.isoformat(),
                   columns=page["columns"], name=claim.get("name"), page_as_of=page_as_of.isoformat(), **missing)
        return day

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        as_of = date.fromisoformat(meta["as_of"])
        if meta["site"] == "us":
            # Decoded as the fetch checked it, so a stray byte costs one
            # character rather than the composition.
            out = parse_csv((indir / CSV_DOC).read_bytes().decode("utf-8-sig", "replace"), meta["ref"], as_of,
                            provider=self.name)
        else:
            out = parse_grid(read_json(indir, JSON_DOC), meta["columns"], meta["ref"], as_of, provider=self.name,
                             fund_name=meta.get("name"))
        page = (indir / PAGE_DOC).read_bytes().decode("utf-8", "replace") if (indir / PAGE_DOC).is_file() else ""
        product = read_json(indir, PRODUCT_DOC) if (indir / PRODUCT_DOC).is_file() else {}
        day = date.fromisoformat(meta.get("page_as_of") or meta["as_of"])
        out.characteristics = characteristics(page, product, meta["ref"], day, provider=self.name)
        return out

    def probe(self, ctx) -> str:
        return f"iShares product lists: {len(self._index(ctx))} identifiers"


def _missing(rows: list) -> dict:
    """meta.json's facts_missing for a fund that wants characteristics:
    none where its files state some; else the planner keeps them due."""
    return {} if rows else {"facts_missing": ["characteristics"]}


def _csv_day(rows: list) -> date | None:
    """The day a US-site CSV's preamble states ("Fund Holdings as of")."""
    for r in rows[:10]:
        if len(r) >= 2 and table._norm(r[0]) in ("fund holdings as of", "holdings as of"):
            return table.parse_day(r[1])
    return None


def parse_csv(text: str, ref: str, as_of: date, *, provider: str) -> Parsed:
    """A US-site holdings CSV as silver rows for fund `ref`."""
    rows = table.csv_rows(text)
    start = table.require_header(rows, "the holdings file has no header row")
    name = rows[0][0].strip() if rows and rows[0] else None
    return table.parse(rows, start, ref, as_of, provider=provider, fund_name=name)


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
    return table.parse(rows, 0, ref, as_of, provider=provider, fund_name=fund_name)


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
        thead = part.find("</thead>", tbl)
        head = part[tbl:thead if thead > 0 else tbl + 20000]
        columns = [" ".join(html.unescape(re.sub(r"<[^>]+>", " ", th)).split())
                   for th in re.findall(r"<th[^>]*>(.*?)</th>", head, re.S)]
    days = set()
    for sel in re.findall(r'<select class="date-dropdown">(.*?)</select>', part, re.S):
        for v in re.findall(r'<option[^>]*value="(\d{8})"', sel):
            days.add(date(int(v[:4]), int(v[4:6]), int(v[6:])))
    return {"columns": columns, "dates": sorted(days)}


class _Items(HTMLParser):
    """The data items of a Swiss-site fund page: per `product-data-item`
    div, the name its `col-<name>` class gives and the text of its
    caption, its as-of-date and its data. A button's text (a tooltip's
    icon) is no caption."""
    VOID = frozenset({"area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "source", "track",
                      "wbr"})
    PARTS = ("caption", "as-of-date", "data")

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.items, self._open, self._item = [], [], None

    def handle_starttag(self, tag, attrs):
        if tag in self.VOID:
            return
        classes = (dict(attrs).get("class") or "").split()
        part = None
        if tag == "div" and "product-data-item" in classes:
            self._item = {"name": next((c[4:] for c in classes if c.startswith("col-")), ""),
                          **{k: [] for k in self.PARTS}}
            self.items.append(self._item)
            part = "item"
        elif tag in ("button", "script", "style"):
            part = "skip"
        elif self._item is not None:
            part = next((k for k in self.PARTS if k in classes), None)
        self._open.append((tag, part))

    def handle_endtag(self, tag):
        for i in range(len(self._open) - 1, -1, -1):
            if self._open[i][0] == tag:
                if any(part == "item" for _, part in self._open[i:]):
                    self._item = None
                del self._open[i:]
                return

    def handle_data(self, data):
        parts = [part for _, part in self._open if part]
        if self._item is not None and "skip" not in parts:
            part = next((k for k in reversed(parts) if k in self.PARTS), None)
            if part:
                self._item[part].append(data)


def _label(text) -> str:
    """A label as LABELS holds it: lower case, without a trailing "as of"
    (a US page names a figure's second appearance so)."""
    s = " ".join(str(text or "").lower().split())
    return re.sub(r"\s*\bas of$", "", s).rstrip(": ")


def _number(text):
    """(number, unit) of a figure as a page writes it (6.12 yrs, 4.01%,
    35 bps, 1’234), or (None, "") where the text is no figure."""
    m = FIGURE.fullmatch(str(text).replace("’", "").replace("'", "").strip()) if text is not None else None
    return (m["number"], (m["unit"] or "").lower()) if m else (None, "")


def _figure(name: str, number, unit: str):
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


def _keep(out: dict, name: str, got) -> None:
    """Puts figure `name` into `out`, unless `out` holds it already; one
    that states a day replaces one that does not."""
    if got is not None and (name not in out or (out[name][2] is None and got[2] is not None)):
        out[name] = got


def _json_objects(text: str):
    """Every JSON object in a text that no other one holds, in order."""
    decoder = json.JSONDecoder()
    i = text.find("{")
    while i >= 0:
        try:
            obj, end = decoder.raw_decode(text, i)
        except ValueError:
            end = i + 1
        else:
            yield obj
        i = text.find("{", end)


def _dicts(value):
    """Every dict a decoded JSON value holds, itself first, in order."""
    todo = [value]
    while todo:
        v = todo.pop()
        if isinstance(v, dict):
            yield v
            todo.extend(reversed(list(v.values())))
        elif isinstance(v, list):
            todo.extend(reversed(v))


def page_state(page: str) -> dict:
    """What a page's script state holds (a US page's, HTML-escaped in the
    page): its data points (dicts with a name and a formattedValue), its
    exposure component, and the PropertyValues of its JSON-LD."""
    out = {"points": [], "exposure": None, "properties": []}
    for obj in _json_objects(html.unescape(page)):
        for d in _dicts(obj):
            if "formattedValue" in d and "name" in d:
                out["points"].append(d)
            elif d.get("componentId") == "exposureBreakdowns":
                out["exposure"] = out["exposure"] or d
            elif d.get("@type") == "PropertyValue":
                out["properties"].append(d)
    return out


def point_figures(points: list) -> dict:
    """figure -> (field, value, day, payload) for each figure a US page's
    data points state: the raw `value` where it is a number, else the
    formattedValue, whose unit counts either way. A figure's day is its
    asOfDate. A point named for a figure (NAMES) wins over one only
    labelled so: a benchmark's point can carry the fund's label."""
    named, labelled = {}, {}
    for p in points:
        name = NAMES.get(str(p.get("name")))
        out, name = (named, name) if name else (labelled, LABELS.get(_label(p.get("label"))))
        if name is None:
            continue
        number, unit = _number(p.get("formattedValue"))
        if dec(p.get("value")) is not None:
            number = p["value"]
        got = _figure(name, number, unit)
        if got is not None:
            day = table.parse_day(p.get("asOfDate")) or table.parse_day(p.get("formattedAsOfDate"))
            _keep(out, name, (*got, day, {"source": "page", "name": p.get("name")}))
    return {**labelled, **named}


def page_items(page: str) -> list:
    """A Swiss page's data items: per item its name and the text of its
    caption, as-of-date and data."""
    parser = _Items()
    parser.feed(page)
    parser.close()
    return [{"name": item["name"], **{k: " ".join("".join(item[k]).split()) for k in _Items.PARTS}}
            for item in parser.items]


def item_figures(items: list) -> dict:
    """figure -> (field, value, day, payload) for each figure a Swiss
    page's data items (page_items) state. An item named for a figure (its
    `col-<name>` in NAMES) wins over one only captioned so. A dash or an
    item without data is no figure."""
    named, labelled = {}, {}
    for item in items:
        caption, when, data = (item[k] for k in _Items.PARTS)
        name = NAMES.get(item["name"])
        out, name = (named, name) if name else (labelled, LABELS.get(_label(caption)))
        got = _figure(name, *_number(data)) if name else None
        if got is not None:
            _keep(out, name, (*got, table.parse_day(when), {"source": "page", "name": item["name"] or caption}))
    return {**labelled, **named}


def _display(value):
    """A value as a product list states it: the text it shows (`d`), else
    its raw value (`r`)."""
    if isinstance(value, dict):
        value = next((value[k] for k in ("d", "r") if value.get(k) not in (None, "")), None)
    return None if value is None else str(value)


def _key(key) -> str:
    return re.sub(r"[^a-z0-9]", "", str(key).lower())


def product_figures(product: dict) -> dict:
    """figure -> (field, value, day, payload) for each figure a
    product-list entry states (PRODUCT_KEYS), dated by the day its
    `<key>AsOf` or `<key>AsOfDate` states."""
    keys = {_key(k): v for k, v in product.items()}
    out = {}
    for key, value in product.items():
        k = _key(key)
        if any(w in k for w in NOT_FIGURE_KEYS):
            continue
        name = next((n for n, words in PRODUCT_KEYS if any(w in k for w in words)), None)
        got = _figure(name, *_number(_display(value))) if name and name not in out else None
        if got is not None:
            when = _display(keys.get(k + "asof") or keys.get(k + "asofdate"))
            out[name] = (*got, table.parse_day(when), {"source": "product", "name": key})
    return out


def property_figures(properties: list) -> dict:
    """figure -> (field, value, day, payload) for each figure a page's
    JSON-LD states as a PropertyValue, by its name. It states no day."""
    out = {}
    for p in properties:
        name = LABELS.get(_label(p.get("name")))
        got = _figure(name, *_number(p.get("value"))) if name else None
        if got is not None:
            _keep(out, name, (*got, None, {"source": "json-ld", "name": p.get("name")}))
    return out


def asset_class(state: dict, items: list, product: dict) -> str:
    """The asset class a fund's page (a US data point, a Swiss data item)
    or product-list entry states, as _label() writes it, else ""."""
    stated = [p.get("value") or p.get("formattedValue") for p in state["points"] if p.get("name") == "assetClass"]
    stated += [i["data"] for i in items if i["name"] == "assetClass"]
    stated.append(_display(product.get("aladdinAssetClass")))
    return next((_label(s) for s in stated if isinstance(s, str) and _label(s) not in ("", "-")), "")


def _point(points: dict, name: str):
    """The value of a breakdown container's data point `name`."""
    return (points.get(name) or {}).get("value")


def breakdowns(exposure: dict, ref: str, day: date, *, provider: str) -> list:
    """The characteristics rows of a US page's exposure component: per
    container BREAKDOWNS names, one row per part, its label the
    container's `type` and its value the `fund` share, dated by the
    container's asOf. A part of no share is no row."""
    maps = next((d["containersByNameMap"] for d in _dicts(exposure)
                 if isinstance(d.get("containersByNameMap"), dict)), {})
    out = []
    for container, field in BREAKDOWNS.items():
        points = (maps.get(container) or {}).get("dataPointsByNameMap") or {}
        labels, shares = _point(points, "type"), _point(points, "fund")
        if not isinstance(labels, list) or not isinstance(shares, list):
            continue
        when = table.parse_day(_point(points, "asOf")) or day
        seen = set()
        for label, share in zip(labels, shares, strict=False):
            label, value = " ".join(str(label or "").split()), dec(share)
            if label and value is not None and Decimal(value) != 0 and label not in seen:
                seen.add(label)
                out.append(row("characteristics", fund_ref=ref, as_of=when, provider=provider, field=field,
                               label=label, value=value))
    return out


def characteristics(page: str, product: dict, ref: str, day: date, *, provider: str) -> list:
    """The characteristics rows of fund `ref`. A figure comes from the
    page (a US page's data points, a Swiss page's data items), else the
    product-list entry, else the page's JSON-LD; the breakdowns from a US
    page's exposure component. A figure is dated by the day stated for
    it, else `day`; a figure none states, or one the fund's asset class
    has no use for (NOT_APPLICABLE), is no row."""
    state, items = page_state(page), page_items(page)
    figures = point_figures(state["points"])
    for more in (item_figures(items), product_figures(product), property_figures(state["properties"])):
        for name, got in more.items():
            figures.setdefault(name, got)
    out = [row("characteristics", fund_ref=ref, as_of=when or day, provider=provider, field=field, value=value,
               payload=payload)
           for field, value, when, payload in figures.values()]
    if state["exposure"] is not None:
        out += breakdowns(state["exposure"], ref, day, provider=provider)
    skip = NOT_APPLICABLE.get(asset_class(state, items, product), frozenset())
    return [r for r in out if r["field"] not in skip]
