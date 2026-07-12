"""One money/number-string parser for the schwab-web silver loader.

Schwab renders monetary and numeric cells in a handful of shapes across
its statement PDFs, tax forms and web exports: bare numbers (``1234.56``),
thousands-separated (``1,234.56``), dollar-prefixed (``$1,234.56``),
leading-minus negatives (``-$5.00``), and parenthesised negatives
(``($1.00)``). Blank / ``None`` / non-numeric input yields ``None``.

This function is the single implementation the surface-specific helpers
(``load._parse_money``, ``tax_form_parsers._to_float``,
``pdf_parsers._parse_number`` and the cash-summary cleaner) delegate to.
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
