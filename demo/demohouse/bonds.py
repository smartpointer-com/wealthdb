"""Bond arithmetic: prices, yields, durations and coupon dates.

A bond yields the dollar curve at its remaining life plus its spread
(demohouse/rates.py), so a fund's lines (demohouse/reference.py) and a
bond the household holds (demohouse/market.py) price off one curve. The
demo has no other curve, so a euro bond's spread is below zero.

A price is clean, per 100 of face: the book adds the interest accrued
since the last coupon. A fixed coupon or a zero discounts at its coupon
frequency, a zero twice a year, and a callable bond to the worse of its
maturity and its call. A discount note's price is its face less the
discount to maturity on ACT/360. A floating note resets its coupon at
every coupon date to a curve tenor plus its spread, so it trades near
par: it is off par only by what its coupon and the market rate differ
until the next reset. A money-market fund earns the curve at its tenor
less its expenses.

Every computation is Decimal, so a value is the same on every platform.
"""

import datetime as dt
from decimal import Decimal, localcontext

from . import dates, rates
from .money import PRECISE, D

# The lowest yield a bond states, in percent, and the shortest life, in
# years, its figures run over.
MIN_YIELD = Decimal("0.05")
MIN_LIFE = Decimal("0.01")
# A zero compounds twice a year, as the street quotes it.
ZERO_FREQUENCY = Decimal(2)
# Days a year of a discount note's quote, and of its yield's.
DISCOUNT_DAYS = Decimal(360)
YEAR_DAYS = Decimal(365)
# The lowest seven-day yield a money-market fund earns, in percent.
MIN_MONEY_YIELD = Decimal("0.01")


def _discount(ytm, life, frequency):
    """(per-period yield, the discount factor over `life` years)."""
    with localcontext(PRECISE):
        y = ytm / 100 / frequency
        return y, (-(life * frequency) * (1 + y).ln()).exp()


def price(coupon, ytm, life, frequency):
    """The price per 100 of par of a bond that pays `coupon` percent a
    year for `life` more years, at a yield of `ytm` percent."""
    with localcontext(PRECISE):
        y, v = _discount(ytm, life, frequency)
        c = coupon / 100 / frequency
        return 100 * (c / y * (1 - v) + v)


def modified_duration(coupon, ytm, life, frequency):
    """The modified duration in years, from the closed form of the
    Macaulay duration."""
    with localcontext(PRECISE):
        y, v = _discount(ytm, life, frequency)
        c = coupon / 100 / frequency
        n = life * frequency
        macaulay = n if c == 0 else (1 + y) / y - (1 + y + n * (c - y)) / (c * (1 / v - 1) + y)
        return macaulay / frequency / (1 + y)


def yield_at(value, coupon, life, frequency):
    """The yield in percent at which the bond is worth `value`: the
    price falls as the yield rises, so a bisection finds it."""
    lo, hi = MIN_YIELD, Decimal(30)
    with localcontext(PRECISE):
        for _ in range(48):
            mid = (lo + hi) / 2
            if price(coupon, mid, life, frequency) > value:
                lo = mid
            else:
                hi = mid
        return (lo + hi) / 2


def life(day, until):
    """The years from `day` to `until` (a date or an ISO date), at least
    MIN_LIFE."""
    end = dates.parse(until) if isinstance(until, str) else until
    return max(dates.years(day, end), MIN_LIFE)


def curve_yield(curve_on_day, years, spread):
    """The curve at `years` plus `spread`, in percent."""
    with localcontext(PRECISE):
        return max(rates.at(curve_on_day, years) + spread, MIN_YIELD)


# ---- coupons --------------------------------------------------------------


def coupon_dates(coupon, year):
    """A coupon's dates in `year`, in order."""
    return [dates.clamp_day(year, m, coupon["day"]) for m in sorted(coupon["months"])]


def last_coupon(coupon, day):
    """The latest coupon date on or before `day`."""
    for year in (day.year, day.year - 1):
        for d in reversed(coupon_dates(coupon, year)):
            if d <= day:
                return d
    raise ValueError(f"no coupon date within a year before {day}")


def next_coupon(coupon, day):
    """The first coupon date after `day`."""
    for year in (day.year, day.year + 1):
        for d in coupon_dates(coupon, year):
            if d > day:
                return d
    raise ValueError(f"no coupon date within a year after {day}")


def accrual_fraction(coupon, day):
    """Share of a year since the bond's last coupon date (actual/365)."""
    return D((day - last_coupon(coupon, day)).days) / YEAR_DAYS


def frequency(coupon):
    """The coupon payments a year: a zero's compounding where it pays
    none."""
    return Decimal(len(coupon["months"])) if coupon else ZERO_FREQUENCY


# ---- prices off the curve -------------------------------------------------


def fixed_price(inst, day, curve_on_day):
    """A fixed-coupon bond's or a zero's price per 100 on `day`: its
    coupons and face discounted at the curve at its life plus its
    spread. A bond callable at par is worth the lower of that and the
    same to its call date, as the market prices it to worst."""
    coupon = inst.get("coupon")
    rate = D(coupon["rate"]) * 100 if coupon else Decimal(0)
    spread = D(inst["model"]["spread"])
    ends = [inst["maturity"]] + ([inst["call"]] if inst.get("call") and dates.parse(inst["call"]) > day else [])
    return min(price(rate, curve_yield(curve_on_day, life(day, end), spread), life(day, end), frequency(coupon))
               for end in ends)


def discount_price(inst, day, curve_on_day):
    """A discount note's price per 100 on `day`: its face less the
    discount at the curve at its life plus its spread, on ACT/360."""
    days = (dates.parse(inst["maturity"]) - day).days
    if days <= 0:
        return Decimal(100)
    with localcontext(PRECISE):
        rate = curve_yield(curve_on_day, D(days) / YEAR_DAYS, D(inst["model"]["spread"]))
        return 100 * (1 - rate / 100 * days / DISCOUNT_DAYS)


def floating_price(inst, day, coupon_pct, curve_on_day):
    """A floating note's clean price per 100 on `day`: par and the coupon
    still to accrue until its next reset, discounted over that time at the
    market's rate plus its spread. It is off par by about the difference
    of the two rates over the time to the reset."""
    coupon = inst["coupon"]
    reset = min(next_coupon(coupon, day), dates.parse(inst["maturity"]))
    with localcontext(PRECISE):
        t = D((reset - day).days) / YEAR_DAYS
        market = curve_yield(curve_on_day, t, D(coupon["spread"]))
        return 100 * (1 + coupon_pct / 100 * t) / (1 + market / 100 * t)


def money_market_yield(curve_on_day, terms):
    """A money-market fund's seven-day yield on a day, in percent: the
    curve at its tenor less its expenses, at least MIN_MONEY_YIELD."""
    return max(curve_on_day[terms["tenor"]] - D(terms["expense"]), MIN_MONEY_YIELD)


def redemption_day(inst):
    """The day a bond pays back its face: its maturity, or the next
    business day."""
    return dates.next_business(dates.parse(inst["maturity"]))


def issue_dates(first_maturity, weeks, issues):
    """The (issued, maturity) days of a ladder of discount notes: each
    issue matures `weeks` after it is issued, on the day the next one is
    issued."""
    term = dt.timedelta(weeks=weeks)
    first = dates.parse(first_maturity)
    return [(first + term * (n - 1), first + term * n) for n in range(issues)]
