"""Date / number parsing helpers shared by silver loaders.

Each collector emits silver records stamped at second-resolution Unix
epoch (UTC). Sources hand us dates in their own formats — this module
covers the formats two or more collectors share."""
from __future__ import annotations

from datetime import date, datetime, timezone


def iso_date(value) -> date | None:
    """The date a ``YYYY-MM-DD…`` string starts with, or None when the
    value is not such a string. A trailing time is ignored."""
    if not isinstance(value, str) or len(value) < 10:
        return None
    try:
        return date.fromisoformat(value[:10])
    except ValueError:
        return None


def iso_date_to_epoch(s: str | None) -> int | None:
    """Parse a `YYYY-MM-DD` date (with optional trailing time) to Unix
    epoch seconds at midnight UTC. Returns None for falsy / unparseable
    input.

    Tolerates a trailing time component by slicing to the first 10
    characters before `strptime`, so callers can hand in either a bare
    date or a full ISO timestamp without pre-trimming."""
    if not s:
        return None
    try:
        dt = datetime.strptime(s[:10], "%Y-%m-%d").replace(tzinfo=timezone.utc)
        return int(dt.timestamp())
    except (TypeError, ValueError):
        return None
