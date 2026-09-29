"""Dates and timestamps.

A snapshot is stamped at the UTC midnight of its day and holds that
day's closing state. A transaction is stamped at noon UTC plus its
sequence number within the account and day, so it sorts after the
snapshot instant of its own day and every report's UTC day bucket puts
it on the right date. Markets trade Monday to Friday; the calendar has
no holidays.
"""

import datetime as dt

DAY = 86400
_NOON = 12 * 3600


def parse(s):
    return dt.date.fromisoformat(s)


def epoch(day):
    """Unix seconds at the UTC midnight that starts `day`."""
    return (day - dt.date(1970, 1, 1)).days * DAY


def txn_time(day, seq):
    return epoch(day) + _NOON + seq


def days(start, end):
    """Every date from start to end, both inclusive."""
    d = start
    one = dt.timedelta(days=1)
    while d <= end:
        yield d
        d += one


def is_business(day):
    return day.weekday() < 5


def next_business(day):
    """`day` itself when it is a business day, else the next one."""
    while not is_business(day):
        day += dt.timedelta(days=1)
    return day


def prev_business(day):
    while not is_business(day):
        day -= dt.timedelta(days=1)
    return day


def nth_business(year, month, n):
    """The n-th business day of the month (1-based)."""
    d = dt.date(year, month, 1)
    count = 0
    while True:
        if is_business(d):
            count += 1
            if count == n:
                return d
        d += dt.timedelta(days=1)


def month_end(year, month):
    if month == 12:
        return dt.date(year, 12, 31)
    return dt.date(year, month + 1, 1) - dt.timedelta(days=1)


def last_business(year, month):
    return prev_business(month_end(year, month))


def clamp_day(year, month, day):
    """The date (year, month, day), or the month's last day if shorter."""
    return min(dt.date(year, month, 1) + dt.timedelta(days=day - 1), month_end(year, month))


def add_months(day, n):
    m = day.month - 1 + n
    return clamp_day(day.year + m // 12, m % 12 + 1, day.day)
