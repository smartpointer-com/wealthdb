"""Date / number parsing helpers shared by silver loaders.

Each collector emits silver records stamped at second-resolution Unix
epoch (UTC). Sources state dates in their own formats; this module
covers the formats two or more collectors share."""
from __future__ import annotations

import re
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


def epoch_day(d) -> int:
    """Unix seconds UTC at midnight of the given date or datetime."""
    return int(datetime(d.year, d.month, d.day, tzinfo=timezone.utc).timestamp())


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


def iso_timestamp_to_epoch(value: str | None) -> int | None:
    """Unix seconds of an ISO-8601 timestamp, or None when absent or
    unparseable. A trailing ``Z`` reads as UTC, and so does a timestamp
    without an offset. Any other offset is honoured."""
    if not value or not isinstance(value, str):
        return None
    if value.endswith("Z"):
        value = value[:-1] + "+00:00"
    try:
        dt = datetime.fromisoformat(value)
    except ValueError:
        return None
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return int(dt.timestamp())


def date_in_name(name: str, pattern) -> int | None:
    """Unix seconds at midnight UTC of the first date `pattern` finds in
    `name`, or None when there is none or it names no real day.

    `pattern` is a regex (string or compiled) with the named groups
    ``y``, ``m`` and ``d``, so each caller states its own file-name
    layout."""
    m = re.search(pattern, name)
    if not m:
        return None
    try:
        return epoch_day(date(int(m["y"]), int(m["m"]), int(m["d"])))
    except ValueError:
        return None
