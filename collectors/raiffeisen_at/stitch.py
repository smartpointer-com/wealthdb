"""Stitch supplied transaction listings and the live history into one ledger.

Pure: no database, no files. load.py gathers the inputs — per account, the
live history with the days each download certified, and the parsed listings
that passed their own checks — and writes back what this returns.

Every booking day of an account has one source of truth (DESIGN.md §I):

1. **live**, when a download certified the day: the day lies in a run's
   window, before the run's own (partial) day, and the run fetched its whole
   history back to there;
2. else the **highest-precedence accepted listing** covering it — a listing
   certifies its `Ausgabe Datum ab` up to the day before it was printed. On a
   day live saw only partly (a download's own day, or the oldest day a cut-
   short walk returned) the listing tops live up: it contributes the postings
   live lacks and the day's closing balance;
3. else nobody.

A listing is accepted when it agrees with the live history and every listing
already accepted on every day they share (X1: the same postings — or, on a
partly seen day, a superset — and the same closing balance) and connects to
them (X2: the closing balance is continuous wherever it meets another
source's days). Listings are tried in precedence order, over and over until a
pass accepts nothing, so one that only connects through another listing still
stitches; one that disagrees is rejected whole; one that never connects is
rejected with the gap it would need filled. Which accepted listing speaks for
a day is settled by precedence once acceptance is done.
"""
from __future__ import annotations

import bisect
from collections import Counter
from collections.abc import Iterator
from dataclasses import dataclass, field
from datetime import date, timedelta

from listing_parser import Listing

LIVE = "live"
_DAY = timedelta(days=1)


@dataclass(frozen=True)
class Window:
    """Days a download covered: `start` is the first day it certifies,
    `end` its own, partial day."""
    start: date
    end: date


def days_between(start: date, end: date) -> Iterator[date]:
    d = start
    while d <= end:
        yield d
        d += _DAY


def day_ranges(days) -> list[tuple[date, date]]:
    """Days → sorted, contiguous (first, last) ranges."""
    out: list[list[date]] = []
    for d in sorted(days):
        if out and d == out[-1][1] + _DAY:
            out[-1][1] = d
        else:
            out.append([d, d])
    return [(a, b) for a, b in out]


# ============================================================
# The two kinds of source
# ============================================================

@dataclass
class LiveView:
    """One account's live history: the download windows, the postings (amount
    cents per booking day) and the closing-balance series."""
    txn_windows: list[Window] = field(default_factory=list)
    balance_windows: list[Window] = field(default_factory=list)
    postings: dict[date, Counter] = field(default_factory=dict)
    balances: dict[date, int] = field(default_factory=dict)
    name: str = LIVE

    def __post_init__(self):
        self.certified = {d for w in self.txn_windows
                          for d in days_between(w.start, w.end - _DAY)}
        self.balance_certified = {d for w in self.balance_windows
                                  for d in days_between(w.start, w.end - _DAY)}
        self._balance_runs = day_ranges(self.balance_certified)
        self._balance_days = sorted(self.balances)

    def postings_on(self, d: date) -> Counter:
        return self.postings.get(d, Counter())

    def certifies(self, d: date) -> bool:
        return d in self.certified

    def closing(self, d: date) -> int | None:
        """The closing balance at the end of `d`: the latest balance on or
        before it within the same certified stretch. The day before a stretch
        is known too when the stretch's first day is a certified booking day:
        its balance less its postings. None otherwise."""
        for start, end in self._balance_runs:
            if start <= d <= end:
                i = bisect.bisect_right(self._balance_days, d)
                if i and self._balance_days[i - 1] >= start:
                    return self.balances[self._balance_days[i - 1]]
                return None
            if d == start - _DAY and start in self.balances \
                    and start in self.certified:
                return self.balances[start] - sum(
                    amount * n for amount, n in self.postings_on(start).items())
        return None


@dataclass
class Candidate:
    """One parsed listing that passed its own checks, for one account."""
    key: str                    # the file's sha256
    name: str                   # for messages
    listing: Listing

    def __post_init__(self):
        lst = self.listing
        self.start = lst.coverage_start
        self.print_day = lst.print_day
        self._postings = lst.postings_by_day()
        self._balances = lst.closing_balances()
        self._balance_days = sorted(self._balances)
        self._opening = lst.opening_balance()

    @property
    def precedence(self):
        """Listings with booking text first, then the newest print, then the
        file hash — content-derived, so arrival order never matters."""
        return (not self.listing.shows_text,
                -self.listing.printed_at.timestamp(), self.key)

    def certifies(self, d: date) -> bool:
        return self.start <= d < self.print_day

    def certified_days(self) -> Iterator[date]:
        return days_between(self.start, self.print_day - _DAY)

    def postings_on(self, d: date) -> Counter:
        return self._postings.get(d, Counter())

    def closing(self, d: date) -> int | None:
        """The closing balance at the end of `d`, from the day before the
        coverage starts (the opening balance) to the day before the print."""
        if not (self.start - _DAY <= d < self.print_day):
            return None
        i = bisect.bisect_right(self._balance_days, d)
        if i:
            return self._balances[self._balance_days[i - 1]]
        return self._opening


# ============================================================
# The stitch
# ============================================================

@dataclass
class Outcome:
    status: str                              # 'accepted' | 'rejected'
    reason: str | None = None
    owned: list[tuple[date, date]] = field(default_factory=list)


def _contains(big: Counter, small: Counter) -> bool:
    return all(big[k] >= n for k, n in small.items())


def _agrees(cand: Candidate, src: LiveView | Candidate, d: date) -> bool:
    """The postings on `d` agree: equal where both certify the day, a
    superset on the side that does where only one does."""
    mine, theirs = cand.postings_on(d), src.postings_on(d)
    mine_full, theirs_full = cand.certifies(d), src.certifies(d)
    if mine_full and theirs_full:
        return mine == theirs
    if mine_full:
        return _contains(mine, theirs)
    if theirs_full:
        return _contains(theirs, mine)
    return True


def _disagreement(cand: Candidate, live: LiveView, accepted: list[Candidate],
                  owner_of: dict[date, str],
                  sources: dict[str, LiveView | Candidate]) -> str | None:
    """X1: the first day this listing disagrees with the live history or a
    listing already accepted, or None."""
    for d in days_between(cand.start, cand.print_day):
        owner = owner_of.get(d)
        checks = [sources[owner]] if owner is not None else []
        if owner != LIVE and live.postings_on(d):
            checks.append(live)                  # what live saw of the day
        for src in checks:
            if not _agrees(cand, src, d):
                return f"postings on {d} differ from {src.name}"
    for src in [live, *accepted]:
        for d in days_between(cand.start - _DAY, cand.print_day - _DAY):
            mine, theirs = cand.closing(d), src.closing(d)
            if mine is not None and theirs is not None and mine != theirs:
                return f"the closing balance on {d} differs from {src.name}"
    return None


def _connection(cand: Candidate, new_days: list[date],
                owner_of: dict[date, str],
                sources: dict[str, LiveView | Candidate]) -> str | None:
    """X2: where the days this listing would take meet days another source
    holds, the closing balance must be continuous. Returns None when it
    connects, 'island' when it touches nothing, else the problem."""
    anchored = False
    for first, last in day_ranges(new_days):
        for boundary, neighbour in ((first - _DAY, first - _DAY),
                                    (last, last + _DAY)):
            owner = owner_of.get(neighbour)
            if owner is None:
                continue
            mine, theirs = cand.closing(boundary), sources[owner].closing(boundary)
            if mine is None or theirs is None:
                return (f"the balance where it meets {sources[owner].name} "
                        f"on {boundary} cannot be verified")
            if mine != theirs:
                return (f"the balance is not continuous where it meets "
                        f"{sources[owner].name} on {boundary}")
            anchored = True
    if anchored or not new_days:
        return None
    return "island"


def _gap(cand: Candidate, owner_of: dict[date, str]) -> str:
    before = [d for d in owner_of if d < cand.start]
    after = [d for d in owner_of if d > cand.print_day]
    if after and (not before or min(after) - cand.print_day
                  <= cand.start - max(before)):
        return f"{cand.print_day} to {min(after) - _DAY}"
    if before:
        return f"{max(before) + _DAY} to {cand.start - _DAY}"
    return "the rest of the ledger"


def stitch(live: LiveView, candidates: list[Candidate]) -> dict[str, Outcome]:
    """Accept or reject each listing of one account and assign it the days it
    speaks for. The result depends only on the set of listings and the live
    history."""
    owner_of: dict[date, str] = {d: LIVE for d in live.certified}
    sources: dict[str, LiveView | Candidate] = {LIVE: live}
    accepted: list[Candidate] = []
    outcomes: dict[str, Outcome] = {}
    pending = sorted(candidates, key=lambda c: c.precedence)

    def accept(cand: Candidate, new_days: list[date]) -> None:
        for d in new_days:
            owner_of[d] = cand.key
        sources[cand.key] = cand
        accepted.append(cand)
        outcomes[cand.key] = Outcome("accepted")

    # With no certified live history to anchor on, the first listing that
    # agrees with what live did see seeds the ledger.
    while not owner_of and pending:
        seed = pending.pop(0)
        problem = _disagreement(seed, live, accepted, owner_of, sources)
        if problem:
            outcomes[seed.key] = Outcome("rejected", problem)
        else:
            accept(seed, list(seed.certified_days()))

    progressed = True
    while progressed and pending:
        progressed = False
        for cand in list(pending):
            problem = _disagreement(cand, live, accepted, owner_of, sources)
            new_days = [d for d in cand.certified_days() if d not in owner_of]
            if problem is None:
                problem = _connection(cand, new_days, owner_of, sources)
            if problem == "island":
                continue
            pending.remove(cand)
            progressed = True
            if problem:
                outcomes[cand.key] = Outcome("rejected", problem)
            else:
                accept(cand, new_days)
    for cand in pending:
        outcomes[cand.key] = Outcome(
            "rejected", "does not connect to the ledger: nothing covers "
            + _gap(cand, owner_of))

    # Accepted listings agree on every day they share, so which one speaks
    # for a day is precedence alone — not the order they were accepted in.
    speaker: dict[date, str] = {}
    for cand in sorted(accepted, key=lambda c: c.precedence):
        for d in cand.certified_days():
            if d not in live.certified:
                speaker.setdefault(d, cand.key)
    for cand in accepted:
        outcomes[cand.key].owned = day_ranges(
            d for d, key in speaker.items() if key == cand.key)
    return outcomes
