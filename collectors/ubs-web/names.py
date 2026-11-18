"""The walk's pure name and text rules: the gap list of a manifest, the
date format the period popover takes, a download's file extension, a
document link without its api key, and the portfolio and period an
export body states. The walk in `download.py` drives the page.
"""

from __future__ import annotations

from datetime import date
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

import landmarks as ubs  # local module


def _windows(gaps: list[tuple[date, date]]) -> list[str]:
    """Un-exported windows as `YYYY-MM-DD..YYYY-MM-DD` strings, for the
    manifest and the log. Always written, empty list included, so a
    reader can tell a run that covered everything from one taken before
    the field existed."""
    return [f"{a.isoformat()}..{b.isoformat()}" for a, b in gaps]


def _fmt_dmy(d: date) -> str:
    return d.strftime("%d.%m.%Y")


def _suggest_extension(suggested: str | None, fmt: str) -> str:
    """Pick a sane file extension. UBS suggests filenames; trust them
    when they end in something obvious, otherwise fall back to fmt."""
    if suggested:
        ext = Path(suggested).suffix.lstrip(".").lower()
        if ext:
            return ext
    return {"csv": "csv", "mt940": "mt940", "pdf": "pdf"}.get(fmt, "bin")


def _strip_apikey(href: str) -> str:
    """Don't persist the apikey query param in run.json — it is a
    long-lived tenant secret. Token + Accept are enough to re-fetch
    by re-running download.py (which gets a fresh apikey from the
    rendered DOM)."""
    s = urlsplit(href)
    qs = parse_qs(s.query)
    qs.pop("apikey", None)
    flat = "&".join(f"{k}={v[0]}" for k, v in qs.items())
    return f"{s.scheme}://{s.netloc}{s.path}" + (f"?{flat}" if flat else "")


def _exported_portfolio(text: str) -> str | None:
    """The portfolio an export says it is for, from its first row.

    The file is named for this rather than for the scope asked of the
    surface, so a dump never claims a portfolio it does not hold."""
    rows = text.splitlines()
    if len(rows) < 2:
        return None
    cells = rows[1].split(";")
    return cells[2].strip() if len(cells) > 2 and cells[2].strip() else None


def _export_window(text: str) -> tuple[str, str] | None:
    """The period the export's own footer states, or None when absent."""
    for line in reversed(text.splitlines()):
        if line.startswith(ubs.TXN_EXPORT_FOOTER_PREFIX):
            m = ubs.TXN_EXPORT_FOOTER_RE.search(line)
            if m:
                return m.group(1), m.group(2)
    return None
