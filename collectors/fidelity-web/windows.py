"""The activity backfill's windows: how a requested range is cut into
exports, clamped to what the page serves, and how a refused window past
the retention floor is told apart from a gap. Pure date arithmetic; the
walk in `download.py` drives the page.
"""

from __future__ import annotations

import logging
from datetime import timedelta

from landmarks import (
    ACTIVITY_OUTSIDE_RETENTION, ACTIVITY_RANGE_REFUSED,
    ACTIVITY_RETENTION_FLOOR_DAYS, MAX_ACTIVITY_WINDOW_DAYS,
)

# The walk's own logger: a record reads the same whichever module
# writes it.
log = logging.getLogger("fidelity-web.download")


def make_activity_windows(since_date, until_date):
    """Bisect ``[since_date, until_date]`` into
    ``MAX_ACTIVITY_WINDOW_DAYS``-day windows (oldest first) so each
    fits under Fidelity's per-export cap on the Custom range tab.
    Returns a list of ``(start, end)`` date tuples."""
    if until_date < since_date:
        return []
    windows = []
    cursor = since_date
    while cursor <= until_date:
        end = min(
            cursor + timedelta(days=MAX_ACTIVITY_WINDOW_DAYS - 1),
            until_date,
        )
        windows.append((cursor, end))
        cursor = end + timedelta(days=1)
    return windows


def mark_outside_retention(results):
    """Tell the windows of an activity backfill that lie past Fidelity's
    retention apart from its gaps.

    The backfill starts at ``ACTIVITY_RETENTION_FLOOR_DAYS``, an assumed
    floor, so its first windows can lie before what Fidelity serves, and
    the page refuses their Custom range. A refused window that ends
    before the first window the page accepted is outside retention: no
    run can fetch it. It is recorded with the status
    ``ACTIVITY_OUTSIDE_RETENTION`` and no ``ok`` flag, which keeps it
    out of the phase's gaps. A refused window after an accepted one
    stays a gap. So does every refused window of a run in which the
    page accepted none, because nothing then shows where retention
    starts."""
    accepted = [r["window"][0] for r in results
                if r.get("window") and r.get("error") != ACTIVITY_RANGE_REFUSED]
    if not accepted:
        return results
    first = min(accepted)
    out = [
        {"window": r["window"], "status": ACTIVITY_OUTSIDE_RETENTION}
        if r.get("error") == ACTIVITY_RANGE_REFUSED and r.get("window")
        and r["window"][1] < first else r
        for r in results
    ]
    outside = sum(r.get("status") == ACTIVITY_OUTSIDE_RETENTION for r in out)
    if outside:
        log.info("activity backfill: %d window(s) before %s lie outside "
                 "Fidelity's retention; not counted as gaps", outside, first)
    return out


def clamp_activity_window(since_date, until_date, fmin, fmax, today):
    """The window a backfill will actually walk.

    ``fmin``/``fmax`` are whatever the Custom tab published in its
    date inputs' ``min``/``max`` attributes, either of which may be
    None. The current generation of the panel publishes NEITHER — it
    enforces retention through validation messages instead — so an
    absent ``fmin`` is the normal case rather than a broken probe, and
    ``ACTIVITY_RETENTION_FLOOR_DAYS`` stands in for it: generous
    enough to reach everything Fidelity has ever served here, short
    enough that ``--lookback all`` stays a walk rather than a siege of
    hundreds of identical empty exports.

    The returned range may be inverted, which is how a request that
    falls entirely outside the available window reports itself.
    """
    if fmin is None:
        floor = today - timedelta(days=ACTIVITY_RETENTION_FLOOR_DAYS)
        if since_date < floor:
            log.info(
                "activity backfill: the panel published no date "
                "bounds, so the requested since=%s is clamped to the "
                "assumed retention floor %s (%d days)",
                since_date.isoformat(), floor.isoformat(),
                ACTIVITY_RETENTION_FLOOR_DAYS,
            )
            since_date = floor
    elif since_date < fmin:
        log.info(
            "activity backfill: clamping requested since=%s up to "
            "Fidelity's earliest available %s",
            since_date.isoformat(), fmin.isoformat(),
        )
        since_date = fmin
    if fmax and until_date > fmax:
        log.info(
            "activity backfill: clamping requested until=%s down to "
            "Fidelity's latest available %s",
            until_date.isoformat(), fmax.isoformat(),
        )
        until_date = fmax
    return since_date, until_date
