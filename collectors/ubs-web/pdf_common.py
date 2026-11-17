"""What every UBS document reader shares: dates, numbers, the IBAN
shape, a bold header's doubled characters, and a document's text.

A reader module imports this one and `collectorkit.words`, and nothing
that extracts a PDF (see pdf_parsers.py).
"""
from __future__ import annotations

import re
from datetime import date, datetime, timezone

from collectorkit.words import Extracted


def document_text(extracted: Extracted, max_pages: int | None = None) -> str:
    """The text of a document's first `max_pages` pages, or of all of
    them, one page after another."""
    return "\n".join(p.text for p in extracted.pages[:max_pages])


def to_unix(d: date) -> int:
    return int(datetime(d.year, d.month, d.day, tzinfo=timezone.utc).timestamp())


# A date as the documents spell it out: "7 March 2024".
LONG_DATE = r"(?P<day>\d{1,2})\s+(?P<month>[A-Z][a-z]+)\s+(?P<year>\d{4})"

# The documents are issued in English throughout the archive. The month
# names are spelled out here rather than handed to strptime("%d %B %Y"),
# whose names come from the process locale: the loader runs in a
# container whose locale is whatever the base image sets, and a date that
# parses on one machine and not another would silently drop rows. Keyed
# in lower case, as UBS varies the casing within one archive.
_MONTHS = {
    name: n for n, name in enumerate(
        ("january", "february", "march", "april", "may", "june", "july",
         "august", "september", "october", "november", "december"), start=1)
}


def long_date(m: re.Match) -> date | None:
    """The date a match's `day`, `month` and `year` groups spell out; None
    for a month name this table does not know."""
    month = _MONTHS.get(m["month"].lower())
    return date(int(m["year"]), month, int(m["day"])) if month else None


def long_date_to_unix(m: re.Match) -> int | None:
    """`long_date` as Unix seconds UTC midnight."""
    d = long_date(m)
    return to_unix(d) if d else None



# An ISIN: country code, nine alphanumerics, check digit.
ISIN_SHAPE = r"[A-Z]{2}[A-Z0-9]{9}\d"


def dmy_to_date(dmy: str) -> date | None:
    """DD.MM.YYYY → date; None when not a date."""
    try:
        d, mo, y = (int(x) for x in dmy.split("."))
        return date(y, mo, d)
    except ValueError:
        return None


def dmy_to_unix(dmy: str) -> int | None:
    """DD.MM.YYYY → Unix seconds UTC midnight; None when not a date."""
    d = dmy_to_date(dmy)
    return to_unix(d) if d else None


def dmy_to_iso(dmy: str) -> str | None:
    """DD.MM.YYYY → 'YYYY-MM-DD'; None when not a date."""
    d = dmy_to_date(dmy)
    return d.isoformat() if d else None


def to_float(s: str | None) -> float | None:
    """Parse a UBS-formatted number ('1 234.56', '1'234.56', '-1234')."""
    if s is None:
        return None
    s = s.replace(" ", "").replace("'", "").replace(",", "")
    if not s:
        return None
    try:
        return float(s)
    except ValueError:
        return None



# An IBAN as the statements and advices print it, spaced or not.
IBAN_RE = re.compile(r"CH\d{2}(?:[ ]?[A-Z0-9]){17}")


def undouble_bold(line: str) -> str:
    """pdfplumber renders some bold headers as every-character-
    doubled ('MMaattuurriittyy nnoottiiccee',
    'AAss aatt 3311..0033..22002233'). Collapse the line — but only
    when the WHOLE line passes the doubled test. A line that mixes
    doubled and non-doubled tokens isn't bold-rendered, and a
    legitimate all-same-digit field ('0000') would otherwise be
    silently shortened to '00'."""
    if not line:
        return line
    flat = line.replace(" ", "")
    if len(flat) < 2 or len(flat) % 2 != 0:
        return line
    for i in range(0, len(flat), 2):
        if flat[i] != flat[i + 1]:
            return line
    parts = []
    for token in line.split(" "):
        if not token:
            parts.append(token)
            continue
        if len(token) % 2 != 0:
            return line
        parts.append("".join(token[i] for i in range(0, len(token), 2)))
    return " ".join(parts)
