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
(demohouse/rates.py): a bond yields the curve at its remaining life
plus its spread. A fund the catalogue marks with `risk` states its rate
and credit-spread sensitivity every quarter-end, as a filing does, and
its duration, yields and credit quality every month-end, as its page
does. Every fund with a cash payout states its distribution yield.

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
# The lowest yield a line states, in percent, and the shortest life, in
# years, a bond line's figures run over.
MIN_YIELD = Decimal("0.05")
MIN_LIFE = Decimal("0.01")
# A floating note resets its coupon at these month-ends.
RESET_MONTHS = (1, 4, 7, 10)
# A sized fund grows by this a year, and its size jitters by this each
# month-end.
GROWTH = Decimal("0.06")
SIZE_JITTER = "0.01"
# A fund page's distribution yield jitters by this around the payout
# rate the instruments catalogue gives.
YIELD_JITTER = "0.03"


def load():
    return json.loads(FUNDS_PATH.read_text())


def securities(cat, instruments):
    """The catalogue's securities by id. One that is also a demo
    instrument takes its name, currency and ticker from it, its ISIN when
    it has one, and its coupon when it pays one, so the two catalogues
    cannot disagree; the entry in funds.json adds what a fund's line
    states beside them."""
    out = {}
    for s in cat["securities"]:
        inst = instruments.get(s["id"])
        if inst:
            s = {**s, "name": inst["name"], "currency": inst["currency"], "ticker": inst.get("symbol"),
                 "isin": inst.get("isin") or s.get("isin")}
            if "coupon" in inst:
                s["coupon"] = str(D(inst["coupon"]["rate"]) * 100)
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


def _discount(ytm, life, frequency):
    """(per-period yield, the discount factor over `life` years)."""
    with localcontext(PRECISE):
        y = ytm / 100 / frequency
        return y, (-(life * frequency) * (1 + y).ln()).exp()


def _price(coupon, ytm, life, frequency):
    """The price per 100 of par of a bond that pays `coupon` percent a
    year for `life` more years, at a yield of `ytm` percent."""
    with localcontext(PRECISE):
        y, v = _discount(ytm, life, frequency)
        c = coupon / 100 / frequency
        return 100 * (c / y * (1 - v) + v)


def _modified_duration(coupon, ytm, life, frequency):
    """The modified duration in years, from the closed form of the
    Macaulay duration."""
    with localcontext(PRECISE):
        y, v = _discount(ytm, life, frequency)
        c = coupon / 100 / frequency
        n = life * frequency
        macaulay = n if c == 0 else (1 + y) / y - (1 + y + n * (c - y)) / (c * (1 / v - 1) + y)
        return macaulay / frequency / (1 + y)


def _yield_at(price, coupon, life, frequency):
    """The yield in percent at which the bond is worth `price`: the price
    falls as the yield rises, so a bisection finds it."""
    lo, hi = MIN_YIELD, Decimal(30)
    with localcontext(PRECISE):
        for _ in range(48):
            mid = (lo + hi) / 2
            if _price(coupon, mid, life, frequency) > price:
                lo = mid
            else:
                hi = mid
        return (lo + hi) / 2


def _next_reset(day):
    """The first reset month-end after the month of `day`."""
    month = dt.date(day.year, day.month, 1)
    while True:
        month = dates.add_months(month, 1)
        if month.month in RESET_MONTHS:
            return dates.month_end(month.year, month.month)


def _life(day, until):
    return max(dates.years(day, dates.parse(until)), MIN_LIFE)


def _yield(curve, life, spread):
    """The curve at `life` years plus `spread`, in percent."""
    with localcontext(PRECISE):
        return max(rates.at(curve, life) + spread, MIN_YIELD)


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
        ytm = _yield(curve, RISK_TENORS["3M"], spread)
        life = dates.years(day, _next_reset(day))
        cols |= {"coupon": text(ytm), "yield_to_maturity": text(ytm), "yield_to_worst": text(ytm),
                 "duration": text(life), "modified_duration": text(life)}
        return Bond(cols, Decimal(cols["duration"]), life)
    frequency = Decimal(s.get("frequency", 2))
    coupon = D(s["coupon"])
    life = D(s["life"]) if "life" in s else _life(day, s["maturity"])
    ytm = _yield(curve, life, spread)
    if s.get("inflation_linked"):
        real = max(ytm - BREAKEVEN, MIN_YIELD)
        modified = _modified_duration(coupon, real, life, frequency)
        cols |= {"real_duration": text(modified), "real_yield": text(real)}
    else:
        modified = _modified_duration(coupon, ytm, life, frequency)
    duration, worst = modified, ytm
    if "call" in s:
        to_call = _life(day, s["call"])
        ytc = _yield_at(_price(coupon, ytm, life, frequency), coupon, to_call, frequency)
        cols["yield_to_call"] = text(ytc)
        if ytc < ytm:
            worst, life = ytc, to_call
            duration = _modified_duration(coupon, ytc, to_call, frequency)
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


def _bond_figures(ref, at, provider, weights, bonds, securities, ter):
    """A bond fund page's figures: the effective duration over the whole
    fund (cash counts as zero), the yield to maturity over its bonds, the
    SEC yield as that yield less the expense ratio, and the share of each
    credit-quality label. A line no rating covers, and the cash the
    weights leave, is Not Rated, so the shares sum to 100."""
    with localcontext(PRECISE):
        duration = sum(weights[sid] * b.duration for sid, b in bonds.items()) / 100
        ytm = (sum(weights[sid] * Decimal(b.cols["yield_to_maturity"]) for sid, b in bonds.items())
               / sum(weights[sid] for sid in bonds))
    quality = dict.fromkeys((*RATINGS, NOT_RATED), ZERO)
    quality[NOT_RATED] = Decimal(100) - sum(weights.values(), ZERO)
    for sid, w in weights.items():
        quality[securities[sid].get("rating", NOT_RATED)] += w
    out = [_figure(ref, at, provider, "effective_duration", duration),
           _figure(ref, at, provider, "yield_to_maturity", ytm),
           _figure(ref, at, provider, "sec_yield", ytm - D(ter))]
    out += [_figure(ref, at, provider, "credit_quality", share, label) for label, share in quality.items() if share]
    return out


def rows(inputs, seed, as_of):
    """Every reference row dated on or before as_of, per table, and every
    company figure filed by then. A column a row does not name is null."""
    cat = load()
    listed = securities(cat, inputs.instruments)
    curve = rates.curve(seed, inputs.fx_start, as_of)
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
                    out["characteristics"] += _bond_figures(ref, at, cat["provider"], weights, bonds, listed,
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
    facts = []
    for f in cat["facts"]:
        inst = inputs.instruments[f["instrument"]]
        facts.append((f, inst, fund_ref(inst), listed.get(f["instrument"], {}).get("isin")))
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
