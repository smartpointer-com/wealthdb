"""
The wanted list read as wants: one per subject (a fund or a security)
and fact it asks for, with the days it is wanted for and the days gold
holds it. The planner (download.py) reads a wanted list through these
alone: a fund's roles, the days it was held, its history_from, a
security's missing fields, its calendar day and its lookups each have
their one reader here.
"""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass, field
from datetime import date, timedelta

from wanted import SECURITY_WANTS

COMPOSITION, IDENTITY = "composition", "identity"
# The roles that want a fund's current composition whether or not it is
# held today: it stands in for, widens or lies under a held fund, or
# makes up a benchmark.
CURRENT_ROLES = frozenset({"proxy", "reference", "underlying", "benchmark"})


@dataclass(frozen=True)
class Fact:
    """A fact a subject can want.

    route: `first`, the first provider that claims the subject serves it
    and those after it stand by; or `all`, every provider that answers it
    is asked. near_days: a past day counts as had where one had lies this
    close. series: each day is an answer of its own, read from the fund's
    history_from on; a composition states none of it. asked_by: the field
    of a security claim that names it, `facts` or `families`; none for
    the lookup itself."""
    name: str
    subject: str        # fund | security
    route: str
    near_days: int = 0
    series: bool = False
    asked_by: str = "facts"


# In the order the coverage lists them (adapters/refdata.md §5): a fund's
# composition and the facts beyond it, which a fund provider's `facts`
# answer, or its compositions (`composition_facts`); then a security's
# lookup, the families of figures it lacks and the facts it wants, which
# a security or bulk provider's `answers` answer.
FACTS = (
    Fact(COMPOSITION, "fund", "first", near_days=15),
    Fact("risk_metrics", "fund", "first"),
    Fact("monthly_returns", "fund", "first", series=True),
    Fact("characteristics", "fund", "first"),
    Fact("money_market", "fund", "first"),
    Fact("line_durations", "fund", "first"),
    Fact("dividend_calendar", "fund", "first"),
    Fact(IDENTITY, "security", "all", asked_by=""),
    Fact("earnings", "security", "all", asked_by="families"),
    Fact("dividends", "security", "all", asked_by="families"),
    Fact("financials", "security", "all", asked_by="families"),
    Fact("dividend_calendar", "security", "all"),
)
ORDER = tuple(dict.fromkeys(f.name for f in FACTS))
_BY = {(f.subject, f.name): f for f in FACTS}
# A security's facts by the field of a claim that names them.
FAMILIES = tuple(f.name for f in FACTS if f.subject == "security" and f.asked_by == "families")
CLAIM_FACTS = frozenset(f.name for f in FACTS if f.subject == "security" and f.asked_by == "facts")


def fact_of(subject: str, name: str) -> Fact:
    """The fact `name` of a subject kind; a name the table does not know
    reads as a fact routed like the others of its subject."""
    return _BY.get((subject, name)) or Fact(name, subject, "first" if subject == "fund" else "all")


@dataclass(frozen=True)
class Want:
    """One fact one subject wants.

    current: wanted today, so its current answer is due once the newest
    day had is older than max_age. For a security: held directly, so a
    lookup asks for its figures, and a constituent is asked only of a
    provider that answers a field it lacks. counted: the coverage counts
    it. since: the first past day a backfill wants, None for none;
    listed_from and until: the days whose dates a backfill reads, and
    history whether those start at the fund's history_from. have: the
    days gold holds it for. by: provider -> the day gold has its last
    answer of, for a security's lookup."""
    subject: object
    fact: Fact
    current: bool = True
    counted: bool = True
    since: date | None = None
    listed_from: date | None = None
    until: date | None = None
    history: bool = False
    have: frozenset = frozenset()
    by: Mapping = field(default_factory=dict)


def security_order(securities, constituents: bool, floor_pct: float = 0) -> list:
    """The securities a run asks for, in the order it asks: every one
    held directly, then, where refdata.cfg's `constituents` allows, the
    securities held through funds whose share of net worth reaches
    floor_pct, the biggest first, so a run cut short by --limit has asked
    for what weighs most. A share counts by its size, so a negative net
    worth drops none; an unknown one passes. A scope this collector does
    not know is skipped."""
    direct = [s for s in securities if s.scope == "direct"]
    through = [s for s in securities if s.scope == "constituent"
               and (s.share_pct is None or abs(s.share_pct) >= floor_pct)] if constituents else []
    return direct + sorted(through, key=lambda s: (-abs(s.share_pct or 0), s.key))


def fund_wants(fund, today: date, gap_days: int) -> list:
    """A fund's wants: its composition, then each fact of its `wants`.

    A fund held today, or wanted in one of CURRENT_ROLES, wants its
    current composition; a held fund its current facts, which the
    coverage counts. A backfill wants the past days from the first day
    held on, and lists the dates from max_gap_days before it, for the
    one in force that day. A series fact reads from history_from on,
    where the fund states one."""
    until = fund.held_to or today
    listed_from = (fund.held_from or today) - timedelta(days=gap_days)
    held = fund.held_to is None
    out = [Want(fund, fact_of("fund", COMPOSITION), current=held or not CURRENT_ROLES.isdisjoint(fund.roles),
                counted=False, since=fund.held_from, listed_from=listed_from, until=until,
                have=frozenset(fund.compositions))]
    for name in fund.wants:
        f = fact_of("fund", name)
        history = f.series and fund.history_from is not None
        out.append(Want(fund, f, current=held, counted=held,
                        since=fund.history_from if history else fund.held_from,
                        listed_from=fund.history_from if history else listed_from, until=until, history=history,
                        have=frozenset(fund.facts.get(name, ()))))
    return out


def security_wants(sec) -> list:
    """A security's wants: its lookup, then each family of figures it
    lacks, then each fact of its `wants` this collector knows."""
    direct = sec.scope == "direct"
    out = [Want(sec, fact_of("security", IDENTITY), current=direct, counted=False, by=sec.lookups)]
    out += [Want(sec, _BY[("security", f)], current=direct) for f in sec.missing if f in FAMILIES]
    out += [Want(sec, _BY[("security", f)], current=direct,
                 have=frozenset(filter(None, (getattr(sec, SECURITY_WANTS[f]),)))) for f in sec.wants if f in CLAIM_FACTS]
    return out


def wants(w, *, today: date, refs=None, securities=True, constituents=False, floor_pct=0) -> list:
    """Every want of a wanted list, a subject's together: the funds' in
    the list's order, then the securities' in security_order. Only the
    subjects `refs` names, where given."""
    out = [x for fund in w.funds if not refs or fund.ref in refs for x in fund_wants(fund, today, w.max_gap_days)]
    if securities:
        out += [x for sec in security_order(w.securities, constituents, floor_pct) if not refs or sec.key in refs
                for x in security_wants(sec)]
    return out
