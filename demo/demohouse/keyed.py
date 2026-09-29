"""Keyed randomness.

Every stochastic choice draws from a generator seeded by a hash of
(seed, stream, keys) — never from one sequential generator. A draw
therefore depends only on the seed, the stream and the draw's own keys
(a day, an account, a month), so appending days at the end cannot
perturb any earlier day. Python's built-in
hash() is salted per process and is never used.

Normal variates come from an inverse-CDF approximation evaluated in
Decimal arithmetic, whose ln and sqrt are correctly rounded, so a draw
is the same on every platform rather than depending on the host libm.
"""

import hashlib
import random
from decimal import Decimal, localcontext

_ONE = Decimal(1)
_HALF = Decimal("0.5")

# Acklam's rational approximation of the inverse normal CDF (relative
# error below 1.2e-9), split into its central and tail regions.
_A = [Decimal(s) for s in (
    "-39.69683028665376", "220.9460984245205", "-275.9285104469687",
    "138.3577518672690", "-30.66479806614716", "2.506628277459239")]
_B = [Decimal(s) for s in (
    "-54.47609879822406", "161.5858368580409", "-155.6989798598866",
    "66.80131188771972", "-13.28068155288572")]
_C = [Decimal(s) for s in (
    "-0.007784894002430293", "-0.3223964580411365", "-2.400758277161838",
    "-2.549732539343734", "4.374664141464968", "2.938163982698783")]
_D = [Decimal(s) for s in (
    "0.007784695709041462", "0.3224671290700398", "2.445134137142996",
    "3.754408661907416")]
_P_LOW = Decimal("0.02425")
_P_HIGH = _ONE - _P_LOW


def rng(seed, stream, *keys):
    """A random.Random seeded by (seed, stream, keys) through blake2b."""
    material = "|".join([str(seed), str(stream), *(str(k) for k in keys)])
    digest = hashlib.blake2b(material.encode(), digest_size=16).digest()
    return random.Random(int.from_bytes(digest, "big"))


def uniform(r):
    """A uniform draw in (0, 1) as an exact Decimal (never 0)."""
    u = r.random()
    while u == 0.0:
        u = r.random()
    return Decimal(u)


def normal(r):
    """A standard normal draw as a Decimal, platform-independent."""
    p = uniform(r)
    with localcontext() as ctx:
        ctx.prec = 28
        if p < _P_LOW:
            q = (-2 * p.ln()).sqrt()
            return _tail(q)
        if p > _P_HIGH:
            q = (-2 * (_ONE - p).ln()).sqrt()
            return -_tail(q)
        q = p - _HALF
        t = q * q
        num = (((((_A[0] * t + _A[1]) * t + _A[2]) * t + _A[3]) * t + _A[4]) * t + _A[5]) * q
        den = ((((_B[0] * t + _B[1]) * t + _B[2]) * t + _B[3]) * t + _B[4]) * t + _ONE
        return num / den


def _tail(q):
    num = ((((_C[0] * q + _C[1]) * q + _C[2]) * q + _C[3]) * q + _C[4]) * q + _C[5]
    den = (((_D[0] * q + _D[1]) * q + _D[2]) * q + _D[3]) * q + _ONE
    return num / den


def lognormal_factor(r, sigma):
    """exp(sigma * z - sigma^2 / 2): a mean-one multiplicative jitter."""
    sigma = Decimal(str(sigma))
    with localcontext() as ctx:
        ctx.prec = 28
        return (sigma * normal(r) - sigma * sigma / 2).exp()


def chance(r, p):
    """True with probability p (a float or Decimal in [0, 1])."""
    return uniform(r) < Decimal(str(p))


def pick(r, items, weights=None):
    """One item, uniformly or by integer-or-Decimal weights."""
    if not items:
        raise ValueError("pick from an empty list")
    if weights is None:
        return items[r.randrange(len(items))]
    total = sum(Decimal(str(w)) for w in weights)
    x = uniform(r) * total
    acc = Decimal(0)
    for item, w in zip(items, weights, strict=True):
        acc += Decimal(str(w))
        if x < acc:
            return item
    return items[-1]
