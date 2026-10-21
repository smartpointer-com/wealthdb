"""Prices and exchange rates.

Four factors drive every quoted instrument: a stock market, a bond
market, gold and crypto. Each draws one daily log-return from keyed
randomness, so a price on a day is the same in every run. An instrument
follows its factor with a beta and adds noise of its own; a blend
follows a weighted mix. Stock, bond and gold prices move on business
days only; crypto and exchange rates move every day.

Exchange rates are USD per unit of foreign currency, a slow
mean-reverting random walk around their starting level.

Instruments of type `nav` have no market; their value is set by the
events that mark them (a fund statement, an appraisal). A `stable` one
keeps its price, as a money-market fund keeps its dollar. A bond of
type `curve`, `discount` or `floating` prices off the day's dollar
curve (demohouse/bonds.py), on business days: a fixed coupon or a zero,
a discount note, a floating note.
"""

import datetime as dt
from decimal import Decimal, localcontext

from . import bonds, dates, keyed, rates
from .money import PRECISE, D, q8, q10

# Annual drift and volatility of each factor. Crypto also jumps: on a
# jump day its return gains a draw with the stated volatility.
FACTORS = {
    "market": {"mu": "0.07", "sigma": "0.16", "calendar": "business"},
    "bond": {"mu": "0.03", "sigma": "0.05", "calendar": "business"},
    "gold": {"mu": "0.05", "sigma": "0.14", "calendar": "business"},
    "crypto": {"mu": "0.30", "sigma": "0.60", "calendar": "daily",
               "jump_chance": "0.01", "jump_sigma": "0.08"},
}
FX_SIGMA = Decimal("0.07")
FX_REVERSION = Decimal("0.002")
_TRADING_DAYS = {"business": Decimal(252), "daily": Decimal(365)}
# The price models the curve sets, and a floating coupon's precision, in
# percent.
CURVE_MODELS = ("curve", "discount", "floating")
_COUPON_PLACES = Decimal("0.001")


class Market:
    """Price and FX series, advanced one day at a time."""

    def __init__(self, seed, instruments, fx_start, fx_levels, curve):
        self.seed = seed
        # {day: {tenor: percent}}, every business day (demohouse/rates.py).
        self.curve = curve
        self._first = min(curve)
        self.instruments = {i["id"]: i for i in instruments}
        self.day = None
        self.factor_ret = {}
        self.prices = {}
        self.fx_start = fx_start
        self.fx_levels = {c: D(v) for c, v in fx_levels.items()}
        with localcontext(PRECISE):
            self._fx_log0 = {c: v.ln() for c, v in self.fx_levels.items()}
        self._fx_log = dict(self._fx_log0)
        self.fx_rates = {c: q10(v) for c, v in self.fx_levels.items()}
        self._splits = {}
        for i in instruments:
            if i.get("model", {}).get("type") in (None, "nav", *CURVE_MODELS):
                continue
            self.prices[i["id"]] = D(i["price"])

    def add_split(self, instrument, day, ratio):
        self._splits[(instrument, day)] = D(ratio)

    def advance(self, day):
        """Move every series to `day` (called once per day, in order)."""
        if self.day is not None and day != self.day + dt.timedelta(days=1):
            raise ValueError(f"market advanced from {self.day} to {day}")
        first = self.day is None
        self.day = day
        if first or dates.is_business(day):
            self._price_off_curve(day)
        if first:
            return
        if day > self.fx_start:
            self._advance_fx(day)
        self.factor_ret = {name: self._factor(name, f, day) for name, f in FACTORS.items()}
        for iid, inst in self.instruments.items():
            model = inst.get("model", {})
            if model.get("type") in (None, "nav", "stable", *CURVE_MODELS):
                continue
            listed = inst.get("listed")
            if listed and day <= dates.parse(listed):
                continue  # not trading yet; it lists at its catalogue price
            r = self._instrument_return(iid, inst, model, day)
            if r is None:
                continue
            with localcontext(PRECISE):
                price = self.prices[iid] * r.exp()
            ratio = self._splits.get((iid, day))
            if ratio:
                price = price / ratio
            self.prices[iid] = q8(price)

    def price(self, instrument):
        return self.prices[instrument]

    def curve_on(self, day):
        """The curve in force on `day`; before the first day it is
        published, that day's, as a floating note's coupon set before the
        history starts reads it."""
        return rates.on(self.curve, max(day, self._first))

    def coupon_rate(self, instrument, day):
        """The coupon rate, a fraction a year, of the period that holds
        `day`: a fixed coupon's own; a floating one's the curve's tenor on
        the period's first day plus its spread, in the precision a
        coupon notice states."""
        coupon = self.instruments[instrument]["coupon"]
        if "rate" in coupon:
            return D(coupon["rate"])
        start = bonds.last_coupon(coupon, day)
        with localcontext(PRECISE):
            pct = self.curve_on(start)[coupon["floating"]] + D(coupon["spread"])
        return pct.quantize(_COUPON_PLACES) / 100

    def usd_per(self, ccy):
        """USD value of one unit of `ccy` on the current day."""
        if ccy == "USD":
            return Decimal(1)
        return self.fx_rates[ccy]

    def to_usd(self, amount, ccy):
        with localcontext(PRECISE):
            return D(amount) * self.usd_per(ccy)

    def from_usd(self, amount, ccy):
        with localcontext(PRECISE):
            return D(amount) / self.usd_per(ccy)

    # ---- internals -----------------------------------------------------

    def _price_off_curve(self, day):
        """Price every bond the curve sets, from its issue until it
        matures; a matured one stays at its face."""
        points = None
        for iid, inst in self.instruments.items():
            kind = inst.get("model", {}).get("type")
            if kind not in CURVE_MODELS or ("issued" in inst and day < dates.parse(inst["issued"])):
                continue
            points = points or self.curve_on(day)
            if day >= dates.parse(inst["maturity"]):
                price = Decimal(100)
            elif kind == "curve":
                price = bonds.fixed_price(inst, day, points)
            elif kind == "discount":
                price = bonds.discount_price(inst, day, points)
            else:
                price = bonds.floating_price(inst, day, self.coupon_rate(iid, day) * 100, points)
            self.prices[iid] = q8(price)

    def _factor(self, name, f, day):
        if f["calendar"] == "business" and not dates.is_business(day):
            return None
        n = _TRADING_DAYS[f["calendar"]]
        mu, sigma = D(f["mu"]), D(f["sigma"])
        r = keyed.rng(self.seed, "factor", name, day.isoformat())
        with localcontext(PRECISE):
            ret = (mu - sigma * sigma / 2) / n + sigma / n.sqrt() * keyed.normal(r)
            if "jump_chance" in f and keyed.chance(r, f["jump_chance"]):
                ret += D(f["jump_sigma"]) * keyed.normal(r)
        return ret

    def _instrument_return(self, iid, inst, model, day):
        kind = model["type"]
        if kind == "equity":
            base = self.factor_ret["market"]
            weights = {"market": D(model["beta"])}
        elif kind == "blend":
            base = self.factor_ret["market"]
            weights = {k: D(v) for k, v in model["weights"].items()}
        elif kind == "bond":
            base = self.factor_ret["bond"]
            weights = {"bond": Decimal(1)}
        elif kind == "gold":
            base = self.factor_ret["gold"]
            weights = {"gold": Decimal(1)}
        elif kind == "crypto":
            base = self.factor_ret["crypto"]
            weights = {"crypto": D(model["beta"])}
        else:
            raise ValueError(f"{iid}: unknown price model {kind!r}")
        if base is None:
            return None  # not a trading day for this instrument
        calendar = FACTORS[next(iter(weights))]["calendar"]
        n = _TRADING_DAYS[calendar]
        idio = D(model.get("idio", "0"))
        alpha = D(model.get("alpha", "0"))
        r = keyed.rng(self.seed, "idio", iid, day.isoformat())
        with localcontext(PRECISE):
            ret = sum(w * self.factor_ret[k] for k, w in weights.items())
            ret += alpha / n - idio * idio / (2 * n) + idio / n.sqrt() * keyed.normal(r)
        return ret

    def _advance_fx(self, day):
        for ccy in sorted(self._fx_log):
            r = keyed.rng(self.seed, "fx", ccy, day.isoformat())
            with localcontext(PRECISE):
                x = self._fx_log[ccy]
                x += FX_REVERSION * (self._fx_log0[ccy] - x) + FX_SIGMA / Decimal(365).sqrt() * keyed.normal(r)
                self._fx_log[ccy] = x
                self.fx_rates[ccy] = q10(x.exp())
