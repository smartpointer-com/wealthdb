"""
Reading conventions the SVB statement families share.

The brokerage statements and the deposit / mortgage ones are different
documents with different layouts and, since one carries a text layer
and the others do not, different extraction stacks. What they do share
is how the bank writes a number and a date, because one bank printed
them all:

* **Parentheses mean negative.** ``($1,234.56)`` is a debit. The
  deposit ledger also signs inline (``$-1,234.56``); both are read.
  Getting this wrong does not fail, it silently reverses a movement,
  which is why it lives in one place with one set of tests rather than
  once per parser.
* **Two-digit years**, pivoted the conventional way, with whichever
  separator: the brokerage statements date a row ``MM/DD/YY``, the
  deposit ledger ``MM-DD`` over the statement's own year, the mortgage
  statement its ``Statement Date: MM/DD/YY`` header. A colon or a dot
  is accepted as a separator too — OCR reads a hyphen as either often
  enough, and a date is not where that should cost a row.

Each parser still owns the SHAPE it accepts — what a money column may
look like in its own layout is a property of that layout, and the two
deliberately differ. This module is only what a token means once one
has been found.
"""

from __future__ import annotations

import re
from datetime import date

# Two-digit years below this belong to the 2000s. Nothing in these
# archives predates the 1970s, so the conventional pivot is safe and a
# year that would fall the wrong side of it would be a misread anyway.
YEAR_PIVOT = 70

_SEPARATOR = r"[-/.:]"
_SHORT_DATE_RE = re.compile(
    rf"^(?P<month>\d{{1,2}}){_SEPARATOR}(?P<day>\d{{1,2}}){_SEPARATOR}"
    rf"(?P<year>\d{{2}})$")
# A token carrying no year of its own, for the ``year=`` path below.
_DAY_ONLY_RE = re.compile(rf"^\d{{1,2}}{_SEPARATOR}\d{{1,2}}$")


def parse_money(token):
    """One money or numeric token as a float, or ``None``.

    Strips ``$`` and thousands commas, drops a trailing ``%`` (the
    brokerage ledger prints a margin rate in the description), and maps
    a surrounding pair of parentheses to a negative sign. A lone sign
    is not a number.

    The parenthesis pair must be matched: a token missing its closing
    one is malformed, and reading it as positive would turn a debit
    into a credit. ``None`` instead lets the caller's own arithmetic
    refuse the row.
    """
    if token is None:
        return None
    t = token.strip()
    negative = False
    if t.startswith("(") and t.endswith(")"):
        negative = True
        t = t[1:-1]
    t = t.replace("$", "").replace(",", "").rstrip("%").strip()
    if t.startswith("-"):
        negative = True
        t = t[1:]
    if not t or t in ("-", "+"):
        return None
    try:
        value = float(t)
    except ValueError:
        return None
    return -value if negative else value


def iso_from_short_date(token, year=None):
    """``MM/DD/YY`` or ``MM-DD-YY`` → ISO ``YYYY-MM-DD``, or ``None``
    when the token is not one or names no real day.

    ``year`` supplies the century-full year for a token that carries no
    year of its own — a deposit ledger dates a row ``MM-DD`` and leaves
    the year to the statement's period.
    """
    if token is None:
        return None
    t = token.strip()
    if year is not None and _DAY_ONLY_RE.match(t):
        t = f"{t}-{year % 100:02d}"
    m = _SHORT_DATE_RE.match(t)
    if not m:
        return None
    yy = int(m["year"])
    full = 1900 + yy if yy >= YEAR_PIVOT else 2000 + yy
    try:
        return date(full, int(m["month"]), int(m["day"])).isoformat()
    except ValueError:
        return None
