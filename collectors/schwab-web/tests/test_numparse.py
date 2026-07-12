"""Equivalence tests for the consolidated money/number-string parser.

Before P10, four near-identical parsers lived in the collector:

  * ``load._parse_money``                    (money strings; leading $, -,
                                              and (parens) negatives)
  * ``tax_form_parsers._to_float``           (1099 cells; $ + commas
                                              stripped before paren check)
  * ``pdf_parsers._parse_number``            (bare numbers; paren negatives)
  * ``pdf_parsers._parse_cash_summary_new``'s nested ``_clean``
                                             (cash cells; $ + paren negatives)

They collapsed into ``numparse.parse_amount`` with three flags. These
tests pin the consolidated parser to the ORIGINAL behaviour two ways:

1. hard-coded expected outputs for every input SHAPE each parser handles
   (negatives, paren-negatives, leading $, thousands commas, blank/None,
   plus the divergent edge cases where the four originals disagreed), and
2. a fuzz cross-check against verbatim inline copies of the four originals
   over an exhaustive small-alphabet battery + random strings, asserting
   byte-identical output (NaN-sign aware).
"""

from __future__ import annotations

import itertools
import math
import random
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from numparse import parse_amount  # noqa: E402


# ------------------------------------------------------------
# Verbatim copies of the four pre-consolidation originals.
# ------------------------------------------------------------

def _orig_parse_money(s):  # load._parse_money
    if s is None:
        return None
    txt = str(s).strip()
    if not txt:
        return None
    neg = txt.startswith("-") or (txt.startswith("(") and txt.endswith(")"))
    txt = txt.strip("()-").lstrip("$").replace(",", "")
    try:
        v = float(txt)
    except ValueError:
        return None
    return -v if neg else v


def _orig_to_float(s):  # tax_form_parsers._to_float
    if s is None:
        return None
    txt = str(s).strip().lstrip("$").replace(",", "")
    if not txt:
        return None
    neg = txt.startswith("(") and txt.endswith(")")
    txt = txt.strip("()")
    try:
        v = float(txt)
    except ValueError:
        return None
    return -v if neg else v


def _orig_parse_number(s):  # pdf_parsers._parse_number
    if s is None:
        return None
    s = s.strip()
    if not s:
        return None
    neg = s.startswith("(") and s.endswith(")")
    if neg:
        s = s[1:-1]
    s = s.replace(",", "")
    try:
        v = float(s)
    except ValueError:
        return None
    return -v if neg else v


def _orig_clean(s):  # pdf_parsers._parse_cash_summary_new._clean
    if s is None:
        return None
    s = s.strip()
    neg = (s.startswith("(") and s.endswith(")")) or (
        s.startswith("($") and s.endswith(")")
    )
    if neg:
        s = s[1:-1]
    s = s.lstrip("$")
    s = s.lstrip("$").replace(",", "")
    try:
        v = float(s)
    except ValueError:
        return None
    return -v if neg else v


# Config each call site passes to the consolidated parser.
_CFG = {
    "money": dict(dollar=True, leading_minus=True),   # load._parse_money
    "to_float": dict(dollar_first=True),              # _to_float
    "number": dict(),                                 # _parse_number
    "clean": dict(dollar=True),                       # _clean
}
_ORIG = {
    "money": _orig_parse_money,
    "to_float": _orig_to_float,
    "number": _orig_parse_number,
    "clean": _orig_clean,
}


def _byte_same(a, b):
    """Byte-identity for the parser's return domain (None | float),
    treating +0.0 vs -0.0 and NaN signs as distinct."""
    if a is None or b is None:
        return a is b
    if math.isnan(a) and math.isnan(b):
        return math.copysign(1.0, a) == math.copysign(1.0, b)
    return repr(a) == repr(b)


# ------------------------------------------------------------
# 1. Hard-coded expected outputs, per shape, per original.
# ------------------------------------------------------------

# (input, money, to_float, number, clean) — expected float | None.
# Derived by hand from the ORIGINAL implementations above; where the
# four disagree the columns differ (that is the point of the pin).
_TABLE = [
    # input           money      to_float   number     clean
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
    cols = ("money", "to_float", "number", "clean")
    for row in _TABLE:
        inp, expected = row[0], dict(zip(cols, row[1:]))
        for name in cols:
            got = parse_amount(inp, **_CFG[name])
            assert _byte_same(got, expected[name]), (
                f"parse_amount({inp!r}, {_CFG[name]}) -> {got!r}, "
                f"expected {expected[name]!r} (shape={name})"
            )
            # and the pin genuinely matches the original impl too
            assert _byte_same(_ORIG[name](inp), expected[name]), (
                f"table wrong for {name}({inp!r})"
            )


# ------------------------------------------------------------
# 2. Fuzz: consolidated == original, byte for byte.
# ------------------------------------------------------------

def _battery():
    alph = ["", "$", "-", "(", ")", "1", "2", ".", ",", "0",
            "5", " ", "%", "$-", "($", ")-"]
    for combo in itertools.product(alph, repeat=3):
        yield "".join(combo)
    rng = random.Random(1729)
    chars = "$-().,0123456789 %e"
    for _ in range(30000):
        n = rng.randint(0, 7)
        yield "".join(rng.choice(chars) for _ in range(n))
    yield None


def test_fuzz_equivalence_all_originals():
    for x in _battery():
        for name, orig in _ORIG.items():
            expected = orig(x)
            got = parse_amount(x, **_CFG[name])
            assert _byte_same(got, expected), (
                f"{name}: parse_amount({x!r}) -> {got!r} != {expected!r}"
            )
