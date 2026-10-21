"""The US dollar yield curve.

The `fx` source publishes it beside its exchange rates: one value per
business day and tenor, in percent, from the first day of the FX history
to the as-of. Three factors set the curve, as in a Nelson-Siegel fit: a
level (the long end), a slope (the short end less the long end) and a
curvature (the belly). The slope follows a slow four-year cycle, so the
history holds inverted spells and upward-sloping ones. Each factor adds a
mean-reverting walk of keyed draws, one per business day.

The curve depends on the calendar and the seed alone, never on the
simulation, so a full build and an append state the same values. Every
computation is Decimal, whose exp is correctly rounded, so a value is
the same on every platform.
"""

import json
from decimal import Decimal, localcontext

from . import dates, keyed
from .money import PRECISE, text

# The source that publishes the curve, and the currency it belongs to.
SOURCE = "fx"
CURRENCY = "USD"

# The published tenors and their length in months.
TENOR_MONTHS = {"1M": 1, "3M": 3, "6M": 6, "1Y": 12, "2Y": 24, "5Y": 60, "10Y": 120, "30Y": 360}

# The factors' decay, in years: a tenor's slope loading is (1 - e^-x) / x,
# and its curvature loading that less e^-x, at x = tenor / DECAY_YEARS.
DECAY_YEARS = Decimal(2)
LEVEL = Decimal("3.9")
CURVATURE = Decimal("-0.3")
# The slope's cycle: its amplitude in percentage points, its period in
# days, and where in the cycle the series starts (0.5 is the most
# inverted point).
SLOPE_AMPLITUDE = Decimal("1.5")
SLOPE_PERIOD_DAYS = Decimal(1461)
SLOPE_PHASE = Decimal("0.55")
# Each factor's walk: its daily pull back to zero and its daily volatility,
# in percentage points.
WALKS = {"level": (Decimal("0.01"), Decimal("0.05")),
         "slope": (Decimal("0.01"), Decimal("0.03")),
         "curvature": (Decimal("0.02"), Decimal("0.03"))}
# Published values stay inside a plausible band.
FLOOR, CAP = Decimal(0), Decimal(6)


def series_id(tenor):
    """The series that publishes `tenor`: USD-3M."""
    return f"{CURRENCY}-{tenor}"


def tenor_years(tenor):
    """A tenor's length in years: 3M is 0.25."""
    with localcontext(PRECISE):
        return Decimal(TENOR_MONTHS[tenor]) / 12


def _loadings():
    """{tenor: (slope, curvature)}: how much of each factor a tenor
    carries. The level loads one on every tenor."""
    out = {}
    with localcontext(PRECISE):
        for tenor in TENOR_MONTHS:
            x = tenor_years(tenor) / DECAY_YEARS
            decay = (-x).exp()
            slope = (1 - decay) / x
            out[tenor] = (slope, slope - decay)
    return out


def _cycle(offset_days):
    """The slope's cycle `offset_days` into the series: a triangle wave
    between -SLOPE_AMPLITUDE and +SLOPE_AMPLITUDE."""
    with localcontext(PRECISE):
        u = (SLOPE_PHASE + Decimal(offset_days) / SLOPE_PERIOD_DAYS) % 1
        return SLOPE_AMPLITUDE * (1 - 4 * abs(u - Decimal("0.5")))


def curve(seed, start, until):
    """{day: {tenor: percent}} for every business day from start to until,
    at full precision and inside the band."""
    loads = _loadings()
    walk = dict.fromkeys(WALKS, Decimal(0))
    out = {}
    for day in dates.days(start, until):
        if not dates.is_business(day):
            continue
        with localcontext(PRECISE):
            for name, (pull, sigma) in WALKS.items():
                z = keyed.normal(keyed.rng(seed, "rates", name, day.isoformat()))
                walk[name] = walk[name] * (1 - pull) + sigma * z
            level = LEVEL + walk["level"]
            slope = _cycle((day - start).days) + walk["slope"]
            bend = CURVATURE + walk["curvature"]
            out[day] = {t: min(max(level + slope * s + bend * c, FLOOR), CAP) for t, (s, c) in loads.items()}
    return out


def on(points, day):
    """The curve in force on `day`: the latest business day's on or before
    it."""
    day = dates.prev_business(day)
    if day not in points:
        raise KeyError(f"no curve on or before {day}")
    return points[day]


def weights(knots, x):
    """{key: weight}: `x` placed on the two (key, position) knots around
    it, in proportion to how near it lies to each, and wholly on the
    nearer end beyond the first or the last. The knots ascend by
    position."""
    if x <= knots[0][1]:
        return {knots[0][0]: Decimal(1)}
    for (k0, p0), (k1, p1) in zip(knots, knots[1:], strict=False):
        if x <= p1:
            with localcontext(PRECISE):
                w1 = (x - p0) / (p1 - p0)
                return {k0: 1 - w1, k1: w1}
    return {knots[-1][0]: Decimal(1)}


def at(curve_on_day, horizon):
    """The curve's rate at `horizon` years: linear between the published
    tenors, flat beyond the shortest and the longest."""
    knots = sorted(((t, tenor_years(t)) for t in curve_on_day), key=lambda k: k[1])
    with localcontext(PRECISE):
        return sum(curve_on_day[t] * w for t, w in weights(knots, horizon).items())


def rows(points_by_day):
    """The rates rows of the publishing source for a curve() result, two
    decimals as the Treasury publishes them. The payload says what the
    series is, as the fred collector's map does."""
    out = []
    for day, points in points_by_day.items():
        stamp = dates.epoch(day)
        for tenor, rate in points.items():
            out.append({"as_of": stamp, "series_id": series_id(tenor), "value": text(rate, 2),
                        "payload": json.dumps({"currency": CURRENCY, "tenor": tenor, "basis": "constant_maturity"},
                                              separators=(",", ":"))})
    return out


def curves_config():
    """The config's `yield.curves` entry naming every published series."""
    return {CURRENCY: {t: f"{SOURCE}:{series_id(t)}" for t in TENOR_MONTHS}}
