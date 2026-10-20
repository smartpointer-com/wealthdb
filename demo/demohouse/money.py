"""Decimal helpers.

Money is Decimal throughout and never float. Amounts round half-even to
cents when a transaction books them; balances and values are written with
four decimals, gold's DECIMAL(28,4); quantities and prices with eight,
gold's DECIMAL(28,8); exchange rates with ten, DECIMAL(20,10).
"""

from decimal import ROUND_HALF_EVEN, Context, Decimal, localcontext

# The context every computation runs in: Decimal's default, its 28
# digits pinned, so a value never depends on the caller's context.
PRECISE = Context(prec=28)

ZERO = Decimal(0)
CENT = Decimal("0.01")
_Q4 = Decimal("0.0001")
_Q8 = Decimal("0.00000001")
_Q10 = Decimal("0.0000000001")


def D(x):
    """A Decimal from an int, a string or a Decimal (never a float)."""
    if isinstance(x, float):
        raise TypeError("money is never a float")
    return x if isinstance(x, Decimal) else Decimal(str(x))


def cents(x):
    return D(x).quantize(CENT, rounding=ROUND_HALF_EVEN)


def q4(x):
    return D(x).quantize(_Q4, rounding=ROUND_HALF_EVEN)


def q8(x):
    return D(x).quantize(_Q8, rounding=ROUND_HALF_EVEN)


def q10(x):
    return D(x).quantize(_Q10, rounding=ROUND_HALF_EVEN)


def text(x, places=4):
    """A decimal string for silver: fixed places, no exponent, no -0."""
    q = {2: CENT, 4: _Q4, 8: _Q8, 10: _Q10}[places]
    v = D(x).quantize(q, rounding=ROUND_HALF_EVEN)
    if v == 0:
        v = abs(v)
    return format(v, "f")


def compound(rate, years):
    """(1 + rate) ** years, for any real number of years, through
    Decimal's correctly rounded exp and ln."""
    with localcontext(PRECISE):
        return (D(years) * (1 + D(rate)).ln()).exp()


def mul(*xs):
    """A product at working precision, before any rounding."""
    with localcontext(PRECISE):
        out = Decimal(1)
        for x in xs:
            out *= D(x)
        return out
