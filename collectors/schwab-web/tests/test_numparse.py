"""Input/output tables for the money/number-string parser.

``numparse.parse_amount`` covers four call sites, each with its own input
dialect selected by flags:

  * ``load._parse_money``                    (money strings; leading $, -,
                                              and (parens) negatives)
  * ``tax_form_parsers._to_float``           (1099 cells; $ + commas
                                              stripped before paren check)
  * ``pdf_parsers._parse_number``            (bare numbers; paren negatives)
  * the cash-summary reader in ``pdf_parsers``
                                             (cash cells; $ + paren negatives)

The table pins the expected output for every input SHAPE each dialect
handles: negatives, paren-negatives, leading $, thousands commas,
blank/None, plus the edge cases where the four dialects disagree.
"""

from __future__ import annotations

import math
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from numparse import parse_amount  # noqa: E402


# Config each call site passes to the consolidated parser.
_CFG = {
    "money": dict(dollar=True, leading_minus=True),   # load._parse_money
    "to_float": dict(dollar_first=True),              # _to_float
    "number": dict(),                                 # _parse_number
    "cash": dict(dollar=True),                        # cash summary
}


def _byte_same(a, b):
    """Byte-identity for the parser's return domain (None | float),
    treating +0.0 vs -0.0 and NaN signs as distinct."""
    if a is None or b is None:
        return a is b
    if math.isnan(a) and math.isnan(b):
        return math.copysign(1.0, a) == math.copysign(1.0, b)
    return repr(a) == repr(b)


# (input, money, to_float, number, cash) — expected float | None.
# Where the four dialects disagree the columns differ (that is the point
# of the pin).
_TABLE = [
    # input           money      to_float   number     cash
    (None,            None,      None,      None,      None),
    ('',              None,      None,      None,      None),
    ('   ',           None,      None,      None,      None),
    ('0',             0.0,       0.0,       0.0,       0.0),
    ('0.00',          0.0,       0.0,       0.0,       0.0),
    ('1234.56',       1234.56,   1234.56,   1234.56,   1234.56),
    ('1,234.56',      1234.56,   1234.56,   1234.56,   1234.56),
    ('1,000,000',     1000000.0, 1000000.0, 1000000.0, 1000000.0),
    ('$1,234.56',     1234.56,   1234.56,   None,      1234.56),
    ('$1234.56',      1234.56,   1234.56,   None,      1234.56),
    ('-5.00',         -5.0,      -5.0,      -5.0,      -5.0),
    ('-$5.00',        -5.0,      None,      None,      None),
    ('(1.00)',        -1.0,      -1.0,      -1.0,      -1.0),
    ('(1,000.00)',    -1000.0,   -1000.0,   -1000.0,   -1000.0),
    ('($1.00)',       -1.0,      None,      None,      -1.0),
    ('($1,000.00)',   -1000.0,   None,      None,      -1000.0),
    ('$(5)',          None,      -5.0,      None,      None),
    ('(-5)',          -5.0,      5.0,       5.0,       5.0),
    ('N/A',           None,      None,      None,      None),
    ('abc',           None,      None,      None,      None),
    ('50%',           None,      None,      None,      None),
    ('$',             None,      None,      None,      None),
    ('()',            None,      None,      None,      None),
    ('1.5e3',         1500.0,    1500.0,    1500.0,    1500.0),
    ('+5',            5.0,       5.0,       5.0,       5.0),
    ('.5',            0.5,       0.5,       0.5,       0.5),
    ('$.5',           0.5,       0.5,       None,      0.5),
    ('  $5.00 ',      5.0,       5.0,       None,      5.0),
]


def test_hardcoded_shapes():
    cols = ("money", "to_float", "number", "cash")
    for row in _TABLE:
        inp, expected = row[0], dict(zip(cols, row[1:], strict=True))
        for name in cols:
            got = parse_amount(inp, **_CFG[name])
            assert _byte_same(got, expected[name]), (
                f"parse_amount({inp!r}, {_CFG[name]}) -> {got!r}, "
                f"expected {expected[name]!r} (shape={name})"
            )
