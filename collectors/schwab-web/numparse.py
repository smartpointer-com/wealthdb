"""One money/number-string parser for the schwab-web silver loader, and
the small value helpers the tax-form kinds share.

Schwab renders monetary and numeric cells in a handful of shapes across
its statement PDFs, tax forms and web exports: bare numbers (``1234.56``),
thousands-separated (``1,234.56``), dollar-prefixed (``$1,234.56``),
leading-minus negatives (``-$5.00``), and parenthesised negatives
(``($1.00)``). Blank / ``None`` / non-numeric input yields ``None``.

This function is the single implementation the surface-specific helpers
(``load._parse_money``, :func:`to_float`, ``stmt_tokens._parse_number``
and ``stmt_2025``'s cash-summary reader) delegate to.
Each surface enables only the shapes its source actually emits, via the
keyword flags — the defaults handle the plain ``(neg)`` / comma / number
case shared by all of them:

* ``dollar`` — also strip a leading ``$`` after sign handling.
* ``leading_minus`` — treat a leading ``-`` as negative, and strip any
  surrounding ``()`` / ``-`` characters generically (rather than peeling
  exactly one paren from each end).
* ``dollar_first`` — the tax-form ordering: strip ``$`` and commas
  *before* detecting a parenthesised negative, so ``($5)`` (a stray
  dollar inside the parens) is left non-numeric and returns ``None``.

The result is advisory: silver keeps the raw string in its JSON payload;
these floats feed ordering / arithmetic indexes only.
"""
from __future__ import annotations

from collections import Counter


def parse_amount(
    s,
    *,
    dollar: bool = False,
    leading_minus: bool = False,
    dollar_first: bool = False,
) -> float | None:
    if s is None:
        return None
    txt = str(s).strip()
    if dollar_first:
        txt = txt.lstrip("$").replace(",", "")
        if not txt:
            return None
        neg = txt.startswith("(") and txt.endswith(")")
        txt = txt.strip("()")
    else:
        if not txt:
            return None
        neg = txt.startswith("(") and txt.endswith(")")
        if leading_minus:
            neg = neg or txt.startswith("-")
            txt = txt.strip("()-")
        elif neg:
            txt = txt[1:-1]
        if dollar:
            txt = txt.lstrip("$")
        txt = txt.replace(",", "")
    try:
        v = float(txt)
    except ValueError:
        return None
    return -v if neg else v


def to_float(s) -> float | None:
    """Parse a tax-form numeric cell to float. Returns None for
    blank / non-numeric input. Strips $ and thousands commas."""
    return parse_amount(s, dollar_first=True)


def modal_year(iso_dates: list[str | None]) -> int | None:
    """The dominant calendar year across a list of ISO dates — a tax
    form's year derived from its *content* (every lot on a 1099-B for
    tax year N was sold in year N). Used when no explicit tax year is
    printed, or as a cross-check."""
    years = Counter()
    for d in iso_dates:
        if d and len(d) >= 4 and d[:4].isdigit():
            years[int(d[:4])] += 1
    if not years:
        return None
    return years.most_common(1)[0][0]
