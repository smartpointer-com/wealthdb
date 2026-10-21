"""The reference records: what the demo's funds hold and what its
securities are.

They depend on the catalogue (catalogue/funds.json), the calendar and
the seed alone, never on the simulation, so a composition is the same
whatever the household traded. Each fund has one composition per
month-end from its first month to the as-of, minus its gaps; its weights
are the catalogue's targets jittered by keyed randomness and
renormalised to their total. The security facts are restated every
quarter-end, as a lookup refreshed on a schedule would.

A bond line states its durations and yields as an issuer file does.
They follow from the bond's terms and the dollar curve on the day
(demohouse/rates.py, demohouse/bonds.py): a bond yields the curve at its
remaining life plus its spread. A fund the catalogue marks with `risk`
states its rate and credit-spread sensitivity every quarter-end, as a
filing does, and its duration, yields, credit quality and maturity
bands every month-end, as its page does. Every fund with a cash payout
states its distribution yield. A money-market fund states its yields
and the maturity of its holdings every month-end, as its N-MFP filing
does. A fund that publishes its page but not its holdings states the
page's figures every month-end, and no composition.

Every row is dated by the day it describes, and fetched that day: the
rows of a month-end are the same in a full build and in an append that
reaches past it. The companies' reported figures (demohouse/filings.py)
are the exception: each is fetched on the day it is filed.
"""

import collections
import datetime as dt
import json
import re
from decimal import Decimal, localcontext

from . import dates, filings, keyed, rates
from . import bonds as bondmath
from .money import PRECISE, ZERO, D, compound, text
from .spec import CATALOGUE_DIR, name_on

FUNDS_PATH = CATALOGUE_DIR / "funds.json"
_ISIN = re.compile(r"^[A-Z]{2}[A-Z0-9]{9}[0-9]$")
JITTER = "0.05"

# The silver tables the reference records fill.
TABLES = ("compositions", "lines", "attributes", "counterparties", "securities", "security_facts",
          "fund_risk", "characteristics", "period_facts")

# The tenors a filing states a fund's rate sensitivity at, in years
# (canonical.RiskTenors), and the currency of its credit-spread rows.
RISK_TENORS = {t: rates.tenor_years(t) for t in ("3M", "1Y", "5Y", "10Y", "30Y")}
NO_CURRENCY = "XXX"
# The credit-quality labels a fund page states, best first. A line no
# agency rates, cash among them, is Not Rated.
RATINGS = ("AAA", "AA", "A", "BBB", "BB", "B")
NOT_RATED = "Not Rated"
INVESTMENT_GRADE = RATINGS[:4]

# Inflation priced into the curve, in percent: an inflation-linked bond's
# real yield is its nominal yield less this.
BREAKEVEN = Decimal("2.3")
# A floating note resets its coupon at these month-ends.
RESET_MONTHS = (1, 4, 7, 10)
# A sized fund grows by this a year, and its size jitters by this each
# month-end.
GROWTH = Decimal("0.06")
SIZE_JITTER = "0.01"
# A fund page's distribution yield jitters by this around the payout
# rate the instruments catalogue gives, and its other stated figures by
# PAGE_JITTER around their targets.
YIELD_JITTER = "0.03"
PAGE_JITTER = "0.02"
# The maturity bands a bond fund page states, each to its upper bound in
# years, and the band of its cash.
MATURITY_BANDS = (("0-1 Years", 1), ("1-3 Years", 3), ("3-5 Years", 5), ("5-7 Years", 7), ("7-10 Years", 10),
                  ("10-20 Years", 20), ("20+ Years", None))
CASH_BAND = "Cash and/or Derivatives"


def load():
    return json.loads(FUNDS_PATH.read_text())


def securities(cat, instruments):
    """The catalogue's securities by id. One that is also a demo
    instrument takes its name, currency and ticker from it, its ISIN when
    it has one, and a bond's coupon, maturity, spread and rating when it
    has them, so the two catalogues cannot disagree; the entry in
    funds.json adds what a fund's line states beside them."""
    out = {}
    for s in cat["securities"]:
        inst = instruments.get(s["id"])
        if inst:
            s = {**s, "name": inst["name"], "currency": inst["currency"], "ticker": inst.get("symbol"),
                 "isin": inst.get("isin") or s.get("isin")}
            if "rate" in inst.get("coupon", {}):
                s["coupon"] = str(D(inst["coupon"]["rate"]) * 100)
            if "maturity" in inst:
                s["maturity"] = inst["maturity"]
            if "spread" in inst.get("model", {}):
                s["spread"] = inst["model"]["spread"]
            if "rating" in inst:
                s["rating"] = inst["rating"]
        out[s["id"]] = s
    return out


def fund_ref(inst):
    """The reference gold builds for an instrument (canonical.FundRef):
    isin:, else symbol:. The demo's instruments carry no CUSIP."""
    isin = (inst.get("isin") or "").strip().upper()
    if _ISIN.match(isin):
        return "isin:" + isin
    symbol = (inst.get("symbol") or "").strip().upper()
    return "symbol:" + symbol if symbol else None


def month_ends(first, as_of, gaps=()):
    """The month-ends from `first` to as_of, both inclusive, skipping the
    gap months (inclusive "YYYY-MM" pairs)."""
    def in_gap(day):
        ym = f"{day:%Y-%m}"
        return any(lo <= ym <= hi for lo, hi in gaps)

    day = dates.month_end(first.year, first.month)
    while day <= as_of:
        if not in_gap(day):
            yield day
        nxt = day + dt.timedelta(days=1)
        day = dates.month_end(nxt.year, nxt.month)


def quarter_ends(first, as_of):
    for day in month_ends(first, as_of):
        if day.month % 3 == 0:
            yield day


def _weights(seed, fund, day, basis, targets):
    """The targets jittered and renormalised to their total, in percent,
    four decimals, in the catalogue's order."""
    raw = {sid: Decimal(w) * keyed.lognormal_factor(keyed.rng(seed, "fund-weight", fund, basis, day.isoformat(), sid),
                                                     JITTER)
           for sid, w in targets.items()}
    total = sum(Decimal(w) for w in targets.values())
    scale = total / sum(raw.values())
    return {sid: Decimal(text(w * scale)) for sid, w in raw.items()}


# ---- bonds ----------------------------------------------------------------


def _next_reset(day):
    """The first reset month-end after the month of `day`."""
    month = dt.date(day.year, day.month, 1)
    while True:
        month = dates.add_months(month, 1)
        if month.month in RESET_MONTHS:
            return dates.month_end(month.year, month.month)


# What a bond line states on a day (its line columns), and the effective
# duration and the life, in years, that place its rate risk on the tenors.
Bond = collections.namedtuple("Bond", "cols duration life")


def bond(s, day, curve):
    """What a bond line states on `day`, from its terms and the day's
    curve. A line yields the curve at its remaining life plus its
    spread. A floating note's coupon is the three-month rate plus its
    spread, and its duration runs to its next reset. A pass-through
    yields and lasts over its average life, not its final maturity. A
    callable bond worth more than par is worst at its call date. An
    inflation-linked bond's real yield is its yield less the breakeven,
    and its durations are real ones."""
    spread = D(s.get("spread", "0"))
    cols = {"maturity": s["maturity"], "coupon_kind": s.get("coupon_kind", "Fixed"), "is_default": 0, "in_arrears": 0,
            "paid_in_kind": 0}
    if cols["coupon_kind"] == "Floating":
        ytm = bondmath.curve_yield(curve, RISK_TENORS["3M"], spread)
        life = dates.years(day, _next_reset(day))
        cols |= {"coupon": text(ytm), "yield_to_maturity": text(ytm), "yield_to_worst": text(ytm),
                 "duration": text(life), "modified_duration": text(life)}
        return Bond(cols, Decimal(cols["duration"]), life)
    frequency = Decimal(s.get("frequency", 2))
    coupon = D(s["coupon"])
    life = D(s["life"]) if "life" in s else bondmath.life(day, s["maturity"])
    ytm = bondmath.curve_yield(curve, life, spread)
    if s.get("inflation_linked"):
        real = max(ytm - BREAKEVEN, bondmath.MIN_YIELD)
        modified = bondmath.modified_duration(coupon, real, life, frequency)
        cols |= {"real_duration": text(modified), "real_yield": text(real)}
    else:
        modified = bondmath.modified_duration(coupon, ytm, life, frequency)
    duration, worst = modified, ytm
    if "call" in s:
        to_call = bondmath.life(day, s["call"])
        ytc = bondmath.yield_at(bondmath.price(coupon, ytm, life, frequency), coupon, to_call, frequency)
        cols["yield_to_call"] = text(ytc)
        if ytc < ytm:
            worst, life = ytc, to_call
            duration = bondmath.modified_duration(coupon, ytc, to_call, frequency)
    cols |= {"coupon": text(coupon), "yield_to_maturity": text(ytm), "yield_to_worst": text(worst),
             "duration": text(duration), "modified_duration": text(modified)}
    return Bond(cols, Decimal(cols["duration"]), life)


def _risk(ref, at, nav, weights, bonds, securities):
    """A filing's rate and credit-spread rows for one fund-day, from its
    net assets and its bond lines' weights and figures. A line's
    DV01 is its value times its duration times one basis point; its
    DV100 is a hundred of those less the convexity a hundred basis
    points earn back. Both sit on the two tenors around the line's life,
    in proportion to how near it lies to each. Government lines carry no
    credit spread."""
    zero = {t: ZERO for t in RISK_TENORS}
    dv01 = {}
    dv100 = {}
    spread = {"ig": dict(zero), "hy": dict(zero)}
    with localcontext(PRECISE):
        for sid, b in bonds.items():
            s = securities[sid]
            value = nav * weights[sid] / 100
            line01 = value * b.duration / 10000
            line100 = 100 * line01 - value * b.duration * b.duration / 20000
            ccy = s["currency"]
            dv01.setdefault(ccy, dict(zero))
            dv100.setdefault(ccy, dict(zero))
            for tenor, share in rates.weights(list(RISK_TENORS.items()), b.life).items():
                dv01[ccy][tenor] += line01 * share
                dv100[ccy][tenor] += line100 * share
                if s.get("sector") != "Government":
                    grade = "ig" if s["rating"] in INVESTMENT_GRADE else "hy"
                    spread[grade][tenor] += line01 * share
    out = []
    for ccy in sorted(dv01):
        for tenor in RISK_TENORS:
            out.append({"fund_ref": ref, "as_of": at, "currency": ccy, "tenor": tenor,
                        "dv01": text(dv01[ccy][tenor]), "dv100": text(dv100[ccy][tenor]), "run_at": at,
                        "payload": "{}"})
    for tenor in RISK_TENORS:
        out.append({"fund_ref": ref, "as_of": at, "currency": NO_CURRENCY, "tenor": tenor,
                    "credit_ig_dv01": text(spread["ig"][tenor]), "credit_hy_dv01": text(spread["hy"][tenor]),
                    "run_at": at, "payload": "{}"})
    return out


# ---- funds ----------------------------------------------------------------


def _line(ref, at, basis, n, **fields):
    """Line `n` of a composition; a column it does not name is null."""
    return {"fund_ref": ref, "as_of": at, "basis": basis, "line_no": n, "payload": "{}", **fields}


def _lines(ref, at, basis, weights, securities, remainder, bonds):
    rows = []
    for n, (sid, w) in enumerate(weights.items(), start=1):
        s = securities[sid]
        rows.append(_line(ref, at, basis, n, isin=s.get("isin"), ticker=s.get("ticker"), name=s["name"],
                          asset_type=s.get("asset_type", "Equity"), weight=text(w), currency=s["currency"],
                          country=s.get("country"), sector=s.get("sector"),
                          **(bonds[sid].cols if sid in bonds else {})))
    left = Decimal(100) - sum(weights.values(), ZERO)
    if not remainder and left != 0:
        rows.append(_line(ref, at, basis, len(rows) + 1, name="USD Cash", asset_type="Cash", weight=text(left),
                          currency="USD"))
    return rows


def _size(seed, fund, day):
    """A sized fund's net assets on `day`: its catalogue size at its first
    month-end, grown and jittered."""
    grown = compound(GROWTH, dates.years(dates.parse(fund["from"]), day))
    r = keyed.rng(seed, "fund-size", fund["instrument"], day.isoformat())
    with localcontext(PRECISE):
        return D(fund["net_assets"]) * grown * keyed.lognormal_factor(r, SIZE_JITTER)


def _figure(ref, at, provider, field, value, label=""):
    return {"fund_ref": ref, "as_of": at, "provider": provider, "field": field, "label": label,
            "value": text(value), "run_at": at, "payload": "{}"}


def _band(years):
    """The maturity band a fund page puts `years` to maturity in."""
    for label, top in MATURITY_BANDS:
        if top is None or years < top:
            return label
    raise AssertionError("the last band is open")


def _bond_figures(ref, day, provider, weights, bonds, securities, ter):
    """A bond fund page's figures: the effective duration over the whole
    fund (cash counts as zero), the yield to maturity over its bonds, the
    SEC yield as that yield less the expense ratio, and the share of each
    credit-quality label and of each maturity band. A line no rating
    covers, and the cash the weights leave, is Not Rated, so the shares
    sum to 100. A bond's band is its years to maturity, a pass-through's
    its average life; the cash is a band of its own."""
    at = dates.epoch(day)
    with localcontext(PRECISE):
        duration = sum(weights[sid] * b.duration for sid, b in bonds.items()) / 100
        ytm = (sum(weights[sid] * Decimal(b.cols["yield_to_maturity"]) for sid, b in bonds.items())
               / sum(weights[sid] for sid in bonds))
    left = Decimal(100) - sum(weights.values(), ZERO)
    quality = dict.fromkeys((*RATINGS, NOT_RATED), ZERO)
    quality[NOT_RATED] = left
    maturity = dict.fromkeys((label for label, _ in MATURITY_BANDS), ZERO)
    for sid, w in weights.items():
        s = securities[sid]
        quality[s.get("rating", NOT_RATED)] += w
        if sid in bonds:
            years = D(s["life"]) if "life" in s else dates.years(day, dates.parse(s["maturity"]))
            maturity[_band(years)] += w
    maturity[CASH_BAND] = left
    out = [_figure(ref, at, provider, "effective_duration", duration),
           _figure(ref, at, provider, "yield_to_maturity", ytm),
           _figure(ref, at, provider, "sec_yield", ytm - D(ter))]
    out += [_figure(ref, at, provider, "credit_quality", share, label) for label, share in quality.items() if share]
    out += [_figure(ref, at, provider, "maturity_breakdown", share, label) for label, share in maturity.items() if share]
    return out


def _money_market(seed, fund, inst, curve, as_of, provider):
    """A money-market fund's figures every month-end, as its N-MFP filing
    states them: the seven-day yield before and after expenses, in
    percent, and the weighted average maturity and life of its holdings,
    in days."""
    ref, terms = fund_ref(inst), inst["money_market"]
    out = []
    for day in month_ends(dates.parse(fund["from"]), as_of):
        at, points = dates.epoch(day), rates.on(curve, day)
        r = keyed.rng(seed, "money-market", fund["instrument"], day.isoformat())
        out += [_figure(ref, at, provider, "seven_day_gross_yield", points[terms["tenor"]]),
                _figure(ref, at, provider, "seven_day_yield", bondmath.money_market_yield(points, terms)),
                _figure(ref, at, provider, "weighted_average_maturity",
                        (D(fund["maturity_days"]) * keyed.lognormal_factor(r, PAGE_JITTER)).quantize(Decimal(1))),
                _figure(ref, at, provider, "weighted_average_life",
                        (D(fund["life_days"]) * keyed.lognormal_factor(r, PAGE_JITTER)).quantize(Decimal(1)))]
    return out


def _page(seed, fund, inst, curve, as_of, provider):
    """The figures every month-end of a bond fund that publishes its page
    but not its holdings: its duration, its average life and the maturity
    bands of its targets, jittered; its yield to maturity, the curve at
    its average life plus its spread; its yield to worst, less what its
    calls cost; the SEC yield, that less its expenses; and its
    distribution yield, near its payout. Its credit is the config's, as a
    fund sliced by rating (yield.rating_funds)."""
    ref = fund_ref(inst)
    payout = D(inst["income"]["yield"]) * 100
    out = []
    for day in month_ends(dates.parse(fund["from"]), as_of):
        at = dates.epoch(day)
        r = keyed.rng(seed, "fund-page", fund["instrument"], day.isoformat())
        ytm = bondmath.curve_yield(rates.on(curve, day), D(fund["life"]), D(fund["spread"]))
        ytw = ytm - D(fund["call_drag"])
        duration = D(fund["duration"]) * keyed.lognormal_factor(r, PAGE_JITTER)
        out += [_figure(ref, at, provider, "effective_duration", duration),
                _figure(ref, at, provider, "weighted_average_maturity_years",
                        D(fund["life"]) * keyed.lognormal_factor(r, PAGE_JITTER)),
                _figure(ref, at, provider, "yield_to_maturity", ytm),
                _figure(ref, at, provider, "yield_to_worst", ytw),
                _figure(ref, at, provider, "sec_yield", ytw - D(fund["ter"])),
                _figure(ref, at, provider, "distribution_yield", payout * keyed.lognormal_factor(r, YIELD_JITTER))]
        bands = _weights(seed, fund["instrument"], day, "maturity", fund["maturity_breakdown"])
        bands[CASH_BAND] = Decimal(100) - sum(bands.values(), ZERO)
        out += [_figure(ref, at, provider, "maturity_breakdown", share, label) for label, share in bands.items()]
    return out


def rows(inputs, seed, as_of, curve):
    """Every reference row dated on or before as_of, per table, and every
    company figure filed by then, its bonds priced off `curve`, the dollar
    curve to as_of (demohouse/rates.py). A column a row does not name is
    null."""
    cat = load()
    listed = securities(cat, inputs.instruments)
    out = {t: [] for t in TABLES}
    for fund in cat["funds"]:
        inst = inputs.instruments[fund["instrument"]]
        ref = fund_ref(inst)
        # An index lists no cash; its weights leave what they leave.
        bases = [(fund.get("basis", "holdings"), fund["lines"], fund.get("remainder", False))]
        if "index" in fund:
            bases.append(("index", fund["index"], True))
        a = fund.get("attributes", {})
        payout = inst.get("income", {}).get("yield")
        for day in month_ends(dates.parse(fund["from"]), as_of, fund.get("gaps", [])):
            at = dates.epoch(day)
            nav = _size(seed, fund, day) if "net_assets" in fund else None
            for basis, targets, remainder in bases:
                weights = _weights(seed, fund["instrument"], day, basis, targets)
                bonds = {sid: bond(listed[sid], day, rates.on(curve, day))
                         for sid in weights if "maturity" in listed[sid]}
                lines = _lines(ref, at, basis, weights, listed, remainder, bonds)
                out["lines"] += lines
                out["compositions"].append({
                    "fund_ref": ref, "as_of": at, "basis": basis, "provider": cat["provider"],
                    "source_kind": "daily_file", "fund_name": name_on(inst, day), "currency": inst["currency"],
                    "net_assets": None if nav is None else text(nav, 2),
                    "line_count": len(lines), "weight_sum": text(sum((Decimal(r["weight"]) for r in lines), ZERO)),
                    "run_at": at, "payload": "{}"})
                if fund.get("risk") and basis == "holdings":
                    out["characteristics"] += _bond_figures(ref, day, cat["provider"], weights, bonds, listed,
                                                            a["ter"])
                    if day.month % 3 == 0:
                        out["fund_risk"] += _risk(ref, at, nav, weights, bonds, listed)
            if payout:
                r = keyed.rng(seed, "fund-yield", fund["instrument"], day.isoformat())
                out["characteristics"].append(_figure(ref, at, cat["provider"], "distribution_yield",
                                                      D(payout) * 100 * keyed.lognormal_factor(r, YIELD_JITTER)))
            out["attributes"].append({
                "fund_ref": ref, "as_of": at, "domicile": a.get("domicile"), "legal_form": a.get("legal_form"),
                "replication": a.get("replication"), "ter": a.get("ter"), "hedged_currency": a.get("hedged_currency"),
                "index_name": a.get("index_name"), "run_at": at, "payload": "{}"})
            for cp in fund.get("counterparties", []):
                out["counterparties"].append({
                    "fund_ref": ref, "as_of": at, "role": cp["role"], "name": cp["name"], "share": cp["share"],
                    "run_at": at, "payload": "{}"})
    for fund in cat["money_market"]:
        out["characteristics"] += _money_market(seed, fund, inputs.instruments[fund["instrument"]], curve, as_of,
                                                cat["provider"])
    for fund in cat["pages"]:
        out["characteristics"] += _page(seed, fund, inputs.instruments[fund["instrument"]], curve, as_of,
                                        cat["provider"])
    facts = []
    for f in cat["facts"]:
        # A ladder's fact holds for every note of it.
        ids = ([iid for iid, i in inputs.instruments.items() if i.get("ladder") == f["ladder"]]
               if "ladder" in f else [f["instrument"]])
        for iid in ids:
            inst = inputs.instruments[iid]
            facts.append((f, inst, fund_ref(inst), listed.get(iid, {}).get("isin")))
    for day in quarter_ends(inputs.history_start, as_of):
        at = dates.epoch(day)
        for f, inst, key, isin in facts:
            if "figi" in f:
                out["securities"].append({
                    "security_key": key, "as_of": at, "provider": cat["provider"], "isin": isin, "figi": f["figi"],
                    "ticker": inst.get("symbol"), "name": name_on(inst, day), "security_type": "Common Stock",
                    "listing_currency": inst["currency"], "run_at": at, "payload": "{}"})
            for field in ("sector", "hq_country", "legal_country", "market_cap"):
                if field in f:
                    out["security_facts"].append({
                        "security_key": key, "field": field, "as_of": at, "provider": cat["provider"],
                        "value": f[field], "scheme": "label" if field == "sector" else None,
                        "run_at": at, "payload": "{}"})
    for f, inst, key, _ in facts:
        if "filings" in f:
            out["period_facts"] += filings.rows(inputs, seed, as_of, f, inst, key, cat["provider"])
    return out
