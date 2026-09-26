"""Money as the sources print it for display, parsed to numbers.

A bank's export, a portal's JSON and a statement's CSV all tend to carry
the display form of an amount: thousands separators, a leading dollar
sign, and accounting parentheses for a negative. This module reads that
form. A source that prints money another way — a German listing's
``1.234,56-``, a statement token with its own sign rules — keeps its own
reader beside its parser.

Imported as ``from collectorkit import money``; it is deliberately not
re-exported from the package ``__init__``, which sits inside every
parser fingerprint (``srcfp``) and must not move for a load-side helper.
"""
from __future__ import annotations


def parse_money(raw) -> float | None:
    """A signed amount, or None when there is none.

    A native number (``int`` or ``float``) passes through as a float.
    Anything else is read as text: surrounding whitespace, commas and ``$``
    signs are dropped, a value wrapped in parentheses is negative, and a
    leading minus is the number's own. Blank or unparseable text is None.
    """
    if raw is None:
        return None
    if isinstance(raw, (int, float)):
        return float(raw)
    s = str(raw).strip()
    if not s:
        return None
    neg = s.startswith("(") and s.endswith(")")
    s = s.strip("()").replace(",", "").replace("$", "").strip()
    if not s:
        return None
    try:
        val = float(s)
    except ValueError:
        return None
    return -val if neg else val


def to_cents(value: float | None) -> int | None:
    """A float amount in minor units, rounded half to even like
    ``round``; None stays None."""
    return None if value is None else int(round(value * 100))


def parse_money_cents(raw) -> int | None:
    """:func:`parse_money` in minor units (cents)."""
    return to_cents(parse_money(raw))
