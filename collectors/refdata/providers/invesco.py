"""
invesco: Invesco's ETFs, from the data API its fund pages read: the
holdings, and the figures a page shows.

- Claim: an ETF whose name names the issuer (Invesco, PowerShares), as
  brokers write it; the API serves ETFs, not Invesco's mutual funds. A
  UCITS fund is asked for by its ISIN; a US fund by its CUSIP, stated or
  read from its US ISIN. Invesco publishes no product list a plain
  request can read.
- Dates: none. The API serves the current holdings.
- Fetch, UCITS: the fund's holdings, its index's constituents and its
  swap counterparties. US: the fund's holdings. For a fund that wants
  characteristics, the answers its page reads its figures from: its
  characteristics and its yields, and for a fund whose characteristics
  are a bond portfolio's, its credit-quality and maturity breakdowns.
  These are fewer requests than one view of the page makes. Each answer
  is kept as it came; one that does not come is a warning.
- Parse: every holding with its ISIN or CUSIP, name, weight and
  currency (US: ticker, units, market value, security type). The index
  constituents are a composition of basis `index`, dated by the index
  file. A fund with swap counterparties holds a substitute basket: its
  holdings are basis `collateral`, the look-through reads the index, and
  each counterparty is a row of role `swap`, with the fund's replication
  marked synthetic.
- Characteristics: a US fund's characteristics state a bond fund's
  effective duration, yields to maturity and worst, years to maturity
  and weighted average coupon (a loan fund's own loan figures), and an
  equity fund's P/E; its yields the 30-day SEC yield, the distribution
  rate (the latest distribution annualized) and the 12-month
  distribution rate (the trailing yield). A UCITS fund's portfolio
  information states a bond fund's effective duration, effective
  maturity and average coupon; its yield information the distribution
  yield and the estimated yield to maturity. A UCITS page states no P/E.
  The forward P/E is a forecast and is not read. The answers state no
  asset class: a fund whose characteristics state a bond figure is a
  bond fund, whose P/E is none. A blank or a zero is none. A breakdown
  is one row per part, named as the answer names it: a US answer by the
  page's label (AA, 0-1 Year), a UCITS one by a key (aA, 5to10Years). A
  figure is dated by the day its answer states for it, else the answer's
  day, else the holdings' day. Where no answer, or none of the API's
  layout (an object), came, meta.json names the characteristics in
  facts_missing (fundpage.due()).

The counterparty entries are read by their field names (a name, and a
weight or exposure in percent); only invented fixtures test that layout.

Terms: README.md, invesco.
"""

from __future__ import annotations

import logging
import re
from datetime import date
from pathlib import Path

import fetcher
from collectorkit import bronze
from collectorkit.parse import iso_date

from . import fundpage, table
from .base import Parsed, Provider, ProviderError, blank_to_none, dec, ident, read_json, read_meta, row, write_meta

API = "https://dng-api.invesco.com/cache/v1/accounts/{locale}/shareclasses/{id}/"
ISSUER = re.compile(r"invesco|powershares", re.I)
ETF = re.compile(r"\betf\b|ucits|powershares|\bqqq\b", re.I)
DOCS = {"fund": "holdings-fund.json", "index": "holdings-index.json", "swaps": "swaps.json"}
JSON = {"Accept": "application/json"}
# The answers a fund page reads its figures from, as kept in bronze.
PAGE_DOCS = {"characteristics": "page-characteristics.json", "yields": "page-yields.json",
             "credit_quality": "page-credit-quality.json", "maturity_breakdown": "page-maturity.json"}
# The figures of the characteristics and yields answers, by key: the US
# site's (unaudited…, bankLoan…, the yields) and the UCITS site's. The
# first key in this order that states a figure wins.
NAMES = {
    "unauditedEffectiveDuration": "effective_duration",
    "effectiveDuration": "effective_duration",
    "unauditedYieldToMaturity": "yield_to_maturity",
    "bankLoanYieldToMaturity": "yield_to_maturity",
    "estimatedYieldToMaturity": "yield_to_maturity",
    "unauditedYieldToWorst": "yield_to_worst",
    "unauditedYearsToMaturity": "weighted_average_maturity",  # in years
    "bankLoanWeightedAverageMaturity": "weighted_average_maturity",
    "effectiveMaturity": "weighted_average_maturity",
    "unauditedWeightedAverageCoupon": "weighted_average_coupon",
    "bankLoanWeightedAverageCoupon": "weighted_average_coupon",
    "averageWeightedCoupon": "weighted_average_coupon",
    "priceToEarningsRatio": "price_earnings",
    "secYield30Day": "sec_yield",
    "distributionYield": "distribution_yield",
    "twelveMonthDistributionRate": "trailing_yield",
}
# The breakdowns a page shows, by the `breakdown` its answer is asked by.
BREAKDOWNS = {"credit_quality": "creditRating", "maturity_breakdown": "effectiveMaturity"}

log = logging.getLogger("refdata.invesco")


class Invesco(Provider):
    name = "invesco"
    serves_funds = True
    facts = frozenset({"characteristics"})

    def claim(self, fund, ctx):
        if not (ISSUER.search(fund.name or "") and ETF.search(fund.name or "")):
            return None
        isin, cusip = ident(fund, "isin"), ident(fund, "cusip")
        if not cusip and isin.startswith("US") and len(isin) == 12:
            cusip = isin[2:11]
        # A fund wanted as a proxy or an underlying wants no
        # characteristics, and its fetch asks for no page answer.
        wants = "characteristics" in fund.wants
        if cusip:
            return {"ref": fund.ref, "site": "us", "id": cusip, "characteristics": wants}
        if isin:
            return {"ref": fund.ref, "site": "ucits", "id": isin, "characteristics": wants}
        return None

    def fetch(self, claim, as_of, outdir: Path, ctx):
        if claim["site"] == "us":
            base = API.format(locale="en_US", id=claim["id"])
            asks = {"fund": (base + "holdings/fund", {"idType": "cusip", "productType": "ETF"})}
        else:
            base = API.format(locale="en_GB", id=claim["id"])
            asks = {"fund": (base + "holdings/fund", {"idType": "isin"}),
                    "index": (base + "holdings/index", {"idType": "isin"}),
                    "swaps": (base + "swaps", {"idType": "isin", "variationType": "counterParties"})}
        got = {}
        for kind, (url, params) in asks.items():
            try:
                resp = ctx.client.get(url, params=params, headers=JSON)
            except fetcher.FetchError as e:
                if kind != "fund" and e.status in (404, 500):
                    continue
                raise
            got[kind] = (resp.body, resp.json())
        fund_doc = got["fund"][1]
        day = _day(fund_doc)
        if day is None or not isinstance(fund_doc.get("holdings"), list):
            raise ProviderError(f"{claim['id']}: the holdings answer states no holdings or no date")
        for kind, (body, _) in got.items():
            bronze.atomic_write_bytes(outdir / DOCS[kind], body)
        pages, missing = {}, {}
        if claim.get("characteristics"):
            pages = self._pages(claim, outdir, ctx)
            missing = fundpage.due(bool(pages), any(isinstance(d, dict) for d in pages.values()), claim["ref"],
                                   log=log, what="its page's answers")
        write_meta(outdir, provider=self.name, ref=claim["ref"], site=claim["site"], as_of=day.isoformat(),
                   files=sorted(got), pages=sorted(pages), **missing)
        return day

    def _pages(self, claim, outdir: Path, ctx) -> dict:
        """The page answers that came, by name, each kept in outdir: those
        of the figures, then a bond fund's breakdowns."""
        figures, parts = page_asks(claim)
        got = self._ask(figures, claim, outdir, ctx)
        if _bond(page_figures(got)):
            got.update(self._ask(parts, claim, outdir, ctx))
        return got

    def _ask(self, asks: dict, claim, outdir: Path, ctx) -> dict:
        """name -> decoded answer of each ask that came, kept in outdir."""
        got = {}
        for name, (url, params) in asks.items():
            answer = fundpage.fetch(ctx, url, claim["ref"], log=log, what=f"its page's {name.replace('_', ' ')}",
                                    params=params, headers=JSON, json=True)
            if answer is not None:
                bronze.atomic_write_bytes(outdir / PAGE_DOCS[name], answer[0])
                got[name] = answer[1]
        return got

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        out = parse_docs({k: read_json(indir, DOCS[k]) for k in meta["files"]}, meta["ref"], provider=self.name)
        pages = {k: read_json(indir, PAGE_DOCS[k]) for k in meta.get("pages", ())}
        out.characteristics = characteristics(pages, meta["ref"], date.fromisoformat(meta["as_of"]),
                                              provider=self.name)
        return out


def _day(doc: dict) -> date | None:
    return next(filter(None, (iso_date((doc or {}).get(k)) for k in ("effectiveBusinessDate", "effectiveDate"))), None)


def _iso(value) -> str | None:
    day = table.parse_day(value) if blank_to_none(value) else None
    return day.isoformat() if day else None


def _first(entry: dict, *words):
    """The value of the first key that holds one of the words, any case."""
    for k, v in entry.items():
        if any(w in k.lower() for w in words) and v not in (None, ""):
            return v
    return None


def _lines(holdings: list, ref: str, as_of: date, basis: str) -> list:
    out = []
    for h in holdings:
        isin = blank_to_none(h.get("isin"))
        out.append(row(
            "lines", fund_ref=ref, as_of=as_of, basis=basis, line_no=len(out) + 1, isin=isin.upper() if isin else None,
            cusip=blank_to_none(h.get("cusip")), ticker=blank_to_none(h.get("ticker")),
            name=blank_to_none(h.get("name") or h.get("issuerName")), asset_type=blank_to_none(h.get("securityTypeName")),
            weight=dec(h.get("weight", h.get("percentageOfTotalNetAssets"))), quantity=dec(h.get("units")),
            market_value=dec(h.get("marketValueBase")), currency=blank_to_none(h.get("currency")),
            maturity=_iso(h.get("maturityDate")), coupon=dec(h.get("coupon")),
            payload={k: v for k, v in h.items() if k not in (
                "isin", "cusip", "ticker", "name", "issuerName", "securityTypeName", "weight",
                "percentageOfTotalNetAssets", "units", "marketValueBase", "currency", "maturityDate", "coupon")
                and v not in (None, "")}))
    return out


def parse_docs(docs: dict, ref: str, *, provider: str) -> Parsed:
    """An Invesco fund's holdings, index and swap answers as silver rows
    for fund `ref`."""
    out = Parsed()
    fund_doc = docs["fund"]
    as_of = _day(fund_doc)
    if as_of is None:
        raise ProviderError("the holdings answer states no date")
    swaps = (docs.get("swaps") or {}).get("swapsCounterParties") or []
    basis = "collateral" if swaps else "holdings"
    lns = _lines(fund_doc.get("holdings") or [], ref, as_of, basis)
    out.lines += lns
    out.compositions.append(table.composition(ref, as_of, lns, provider=provider, basis=basis))
    index_doc = docs.get("index")
    if index_doc and index_doc.get("holdings"):
        index_day = _day(index_doc) or as_of
        idx = _lines(index_doc["holdings"], ref, index_day, "index")
        out.lines += idx
        out.compositions.append(table.composition(ref, index_day, idx, provider=provider, basis="index"))
    for cp in swaps:
        name = _first(cp, "name", "counterparty")
        if not name:
            continue
        out.counterparties.append(row("counterparties", fund_ref=ref, as_of=as_of, role="swap", name=str(name),
                                      lei=blank_to_none(_first(cp, "lei")),
                                      share=dec(_first(cp, "weight", "exposure", "percent")), payload=cp))
    if swaps:
        out.attributes.append(row("attributes", fund_ref=ref, as_of=as_of, replication="synthetic"))
    return out


def page_asks(claim: dict) -> tuple:
    """The answers a fund page reads its figures from, as name -> (url,
    params): those of its characteristics and yields, and those of its
    breakdowns. A US fund is asked for by CUSIP, a UCITS fund by ISIN."""
    if claim["site"] == "us":
        base, by = API.format(locale="en_US", id=claim["id"]), {"idType": "cusip", "productType": "ETF"}
        figures = {"characteristics": (base.rstrip("/"), {"expand": "nav", "variationType": "fundCharacteristics",
                                                          **by}),
                   "yields": (base.rstrip("/"), {"expand": "nav", "variationType": "yieldInformation",
                                                 "managementFeeWaiver": "0.0", **by})}
    else:
        base, by = API.format(locale="en_GB", id=claim["id"]), {"idType": "isin", "productCode": "ETF"}
        figures = {"characteristics": (base + "characteristics",
                                       {"idType": "isin", "variationType": "portfolioInformation"}),
                   "yields": (base + "characteristics", {"idType": "isin", "variationType": "yieldInformation"})}
    parts = {field: (base + "weightedHoldings/fund", {**by, "breakdown": breakdown})
             for field, breakdown in BREAKDOWNS.items()}
    return figures, parts


def _answer(docs: dict, name: str) -> dict:
    doc = docs.get(name)
    return doc if isinstance(doc, dict) else {}


def page_figures(docs: dict) -> dict:
    """field -> (value, day, payload) of each figure the characteristics
    and yields answers state (NAMES), the characteristics' first. A
    figure is dated by its `<key>EffectiveDate`, else its answer's day."""
    out = {}
    for doc in (_answer(docs, "characteristics"), _answer(docs, "yields")):
        for key, name in NAMES.items():
            got = fundpage.figure(name, doc.get(key), "")
            if got is not None and got[0] not in out:
                out[got[0]] = (got[1], iso_date(doc.get(key + "EffectiveDate")) or _day(doc),
                               {"source": "page", "name": key})
    return out


def _bond(figures: dict) -> bool:
    """Whether a fund's figures are a bond portfolio's: the answers name
    no asset class."""
    return bool(fundpage.BOND_FIGURES & figures.keys())


def page_breakdowns(docs: dict) -> list:
    """The parts of the breakdown answers: per holding weight its name and
    value, dated by its answer's day."""
    out = []
    for field in BREAKDOWNS:
        doc = _answer(docs, field)
        weights = doc.get("holdingWeights") or []
        out += fundpage.parts(field, [(w.get("name"), w.get("value")) for w in weights if isinstance(w, dict)],
                              _day(doc))
    return out


def characteristics(docs: dict, ref: str, day: date, *, provider: str) -> list:
    """The characteristics rows of fund `ref` from its page answers (name
    -> decoded answer), each dated by the day its answer states, else
    `day`, the holdings' day. A bond fund's P/E is no row."""
    figures = page_figures(docs)
    return fundpage.rows(figures, page_breakdowns(docs), ref, day, provider=provider,
                         skip=fundpage.EQUITY_FIGURES if _bond(figures) else frozenset())
