#!/usr/bin/env python3
"""
refdata downloader: the wanted list in, raw provider files out.

1. Read the wanted list: ask the engine (`wealthdb -r refdata-wanted`),
   or read --wanted FILE.
2. For every wanted fund, the inbox serves what it holds. A fund whose
   newest composition had (in gold, or fetched by an earlier run) is older
   than max_age_days wants its current one; with --backfill, it also
   wants every past date the provider can serve inside the days the fund
   was held, and the last one before them, that has no composition within
   half a month. Only such a fund is offered to the providers, in
   refdata.cfg's order: the first that claims it serves it, and the next
   one claims it only should a fetch fail.
3. For every fact a held fund wants beyond its compositions (risk
   metrics, characteristics, money-market figures, its dividend
   calendar) whose newest day had is older than max_age_days, ask the
   providers that answer it in the same order, whichever serves the
   compositions; with --backfill, for the past dates too. A fact a
   provider states only with a composition (line durations) comes with a
   composition request to it, never with a request of its own.
4. For every wanted security held directly, ask every enabled security
   provider whose last answer is older than max_age_days, and every one
   that answers a fact the security wants (its dividend calendar) whose
   newest day had is older than that; with `constituents`, then ask for
   the securities held through funds whose share of net worth reaches
   `constituent_floor_pct`, the biggest first, each only of a provider
   that can fill a field it lacks.
5. Let every bulk provider plan the requests that answer many of those
   securities at once, such as a table of every company's figure for one
   period.
6. Write each answer's raw files into bronze, one folder per request:

       <run>/funds/<provider>/<reference>/<YYYY-MM-DD | current>/
       <run>/securities/<provider>/<key>/
       <run>/bulk/<provider>/<ref>/

   and the run's manifest, run.json, with the outcome of every request.
   One failed request does not fail the run.
7. Say what becomes of every fact wanted: how many funds or securities
   want it, how many a request asks for or a provider had within
   max_age_days, and why each other one stays missing. A dry run prints
   it after the requests; a run logs one line per fact.

Reads public data only, signed in to no account, with a User-Agent that
names the tool and a pace per host (fetcher.py). FINRA's API takes the
client credential its terms require, traded for a token
(providers/finra.py): the id from --client-id or FINRA_CLIENT_ID, the
secret from FINRA_CLIENT_SECRET alone.

Usage:
    download.py --bronze-dir DIR [--wanted FILE] [--backfill] [--provider NAME]
                [--ref REF] [--limit N] [--no-securities] [--client-id ID]
                [--dry-run] [--debug]
    download.py --check [--provider NAME] [--client-id ID]
"""

from __future__ import annotations

import argparse
import logging
import sys
from collections import Counter
from dataclasses import dataclass, field, replace
from datetime import date, datetime, timedelta, timezone
from pathlib import Path
from typing import NamedTuple

import config
import fetcher
import wanted as wantedmod
from collectorkit import bronze, cli, debugcap
from providers import REGISTRY
from providers.base import (Context, ProviderError, Unavailable, complete_runs, folder_name, log_notes, read_meta,
                            today_utc)

log = logging.getLogger("refdata.download")

# A past date counts as covered when gold holds a composition this close
# to it: half the month-end cadence. A fact's day counts only where it is
# held exactly: a filing that states every day of its month would hide
# the month next to it.
NEAR_DAYS = 15
# The roles that want a fund's current composition whether or not it is
# held today: it stands in for, widens or lies under a held fund.
CURRENT_ROLES = frozenset({"proxy", "reference", "underlying"})
# The facts a wanted list names beyond compositions, in the order the
# coverage lists them (adapters/refdata.md §5): a held fund's, which a
# fund provider's `facts` answer, or its compositions (`composition_facts`),
# then the families of figures a security lacks, which a security or bulk
# provider's `answers` answer.
FUND_FACTS = ("risk_metrics", "characteristics", "money_market", "line_durations", "dividend_calendar")
SECURITY_FACTS = ("earnings", "dividends", "financials")
FACTS = FUND_FACTS + SECURITY_FACTS
# The facts a security wants (its `wants`), which a security or bulk
# provider's `answers` answer, each with the wanted list's field that
# holds the newest day gold has it for.
SECURITY_WANTS = {"dividend_calendar": "calendar"}
DEFERRED = "deferred by --limit"

# What a provider's answer can raise without failing the run: its own
# error, an HTTP failure or an answer that is not JSON, or an answer that
# does not parse (ValueError). Anything else is logged with its traceback,
# and the run goes on all the same.
LOOKUP_ERRORS = (ProviderError, fetcher.FetchError, ValueError)


@dataclass
class Request:
    kind: str          # fund | security | bulk
    provider: str
    ref: str
    as_of: date | None = None   # None: the current composition
    claim: object = None
    # The providers standing by, asked in order when this one cannot
    # serve the current composition or fact. Each claims `fund` only then.
    # `fund` carries the identifiers this one's claim adds (described).
    fallbacks: tuple = ()
    fund: object = None
    # The facts a fund request asks for beyond the composition, which the
    # provider's fetch states with it; a security or bulk request's, the
    # facts of the securities' `wants` its claim names.
    facts: tuple = ()
    # Whether the request is for the fund's composition. One asked for
    # facts alone is marked `facts_only` in the manifest, and load keeps
    # only its facts: a filing's lines never replace those another
    # provider serves for the same day.
    compositions: bool = True


@dataclass
class Cover:
    """What a run does about one fact one fund or security wants: the
    requests that ask for it, an answer had within max_age, or why it
    stays missing."""
    requests: list = field(default_factory=list)
    had: bool = False
    reason: str = ""


class Fetched(NamedTuple):
    """What earlier complete runs fetched, mostly with the day of the
    latest run that fetched it (fetched_before)."""
    funds: dict      # (provider, fund ref, day of the composition) -> day
    lookups: dict    # (provider, security key) -> day
    facts: dict      # (provider, fund ref, fact) -> day
    fact_days: dict  # (provider, fund ref, fact) -> the days a request for facts alone fetched it for
    bulk: dict       # (provider, bulk ref) -> day
    answered: dict   # (provider, security key) -> day, of a bulk answer whose `answered` names it
    families: dict   # (provider, security key) -> day, of a security request that asked for figures
    wants: dict      # (provider, security key, fact) -> day, of a security request that asked for a
                     # fact the security wants, or a bulk answer that asked for it and states it
    asked: dict      # (provider, security key) -> day, of a security request sent, one that failed too

    @classmethod
    def of(cls, **maps) -> Fetched:
        """The maps given, every other one empty."""
        return cls._make(maps.get(f, {}) for f in cls._fields)


def parse_args(argv):
    p = argparse.ArgumentParser(description=__doc__.strip().split("\n\n")[0])
    p.add_argument("--bronze-dir", type=Path, default=None, help="One run dir is created here per invocation.")
    p.add_argument("--wanted", type=Path, default=None,
                   help="Read the wanted list from this file instead of asking the engine.")
    p.add_argument("--backfill", action="store_true",
                   help="Also fetch every past date a provider can serve inside the days a fund was held.")
    p.add_argument("--provider", action="append", default=None,
                   help="Ask only this provider (repeatable); it must be one refdata.cfg enables.")
    p.add_argument("--ref", action="append", default=None,
                   help="Ask only for this fund reference, security key or bulk ref (repeatable).")
    p.add_argument("--limit", type=int, default=None,
                   help="Make at most N fetch attempts this run; the lookups that find a fund are not counted.")
    p.add_argument("--no-securities", action="store_true", help="Skip the security lookups and the bulk requests.")
    p.add_argument("--client-id", default=None, metavar="ID",
                   help=f"FINRA's API client id, for the finra provider (default: ${config.FINRA_CLIENT_ID}). "
                        f"Its secret comes from ${config.FINRA_CLIENT_SECRET} alone.")
    p.add_argument("--check", action="store_true",
                   help="Ask every enabled provider for its cheapest answer and exit; writes nothing.")
    p.add_argument("--dry-run", action="store_true",
                   help="Plan the requests, print them and what becomes of each fact wanted, fetch nothing.")
    p.add_argument("--debug", action="store_true",
                   help="Trace every HTTP exchange of a run, its plan included, into "
                        "<run>/screenshots/http-trace.jsonl (status, timing, size; no body). prune reclaims it.")
    cli.add_standard_args(p, verb="download", full_history=True)
    return p.parse_args(argv)


def enabled_providers(cfg: config.Config, only=None):
    """The providers a run asks, in refdata.cfg's order; only those
    --provider names, where given. A name refdata.cfg does not enable is
    refused, so a typo never runs nothing."""
    unknown = sorted(set(only or ()) - set(cfg.providers))
    if unknown:
        raise SystemExit(f"--provider {', '.join(unknown)}: not enabled in {cfg.path or 'refdata.cfg'}. "
                         f"Enabled: {', '.join(cfg.providers)}.")
    out = []
    for name in cfg.providers:
        if only and name not in only:
            continue
        p = REGISTRY[name]
        if p.needs_contact and not cfg.contact:
            log.warning("%s: skipped — the SEC asks for a contact address; set `contact` in %s",
                        name, cfg.path)
            continue
        if p.needs_setting and not getattr(cfg, p.needs_setting):
            log.warning("%s: skipped — set `%s` in %s", name, p.needs_setting, cfg.path)
            continue
        out.append(p)
    return out


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


def fetched_before(bronze_dir: Path | None) -> Fetched:
    """What earlier complete runs fetched, which counts as had until gold
    holds it, so a second run before the next load asks for it no more:
    every composition (provider, fund ref, date), security answer, fund
    fact and bulk request, each with the day of the latest run that
    fetched it. Apart: the days a request for a fund's facts alone
    fetched each fact for, which are no compositions; the securities a
    bulk answer names in its meta.json's `answered`; the security
    answers whose request asked for families of figures; and the
    facts a security wants that a security request asked for, or a bulk
    request asked for and its answer names the security in. Apart too:
    every security request sent, one that failed included, for a provider
    that waits min_age_days between two (base.py)."""
    out = Fetched.of()

    def seen(into, key, day):
        into[key] = max(into.get(key, day), day)

    for run, manifest in complete_runs(bronze_dir):
        day = datetime.fromtimestamp(bronze.parse_run_ts(run.name), timezone.utc).date()
        for e in manifest.get("requests") or []:
            if e.get("kind") == "security" and e.get("status") in ("ok", "error", "unavailable"):
                seen(out.asked, (e["provider"], e["ref"]), day)
            if e.get("status") != "ok":
                continue
            if e.get("kind") == "fund":
                as_of = date.fromisoformat(e["as_of"]) if e.get("as_of") else None
                if as_of and not e.get("facts_only"):
                    seen(out.funds, (e["provider"], e["ref"], as_of), day)
                for fact in e.get("facts") or ():
                    seen(out.facts, (e["provider"], e["ref"], fact), day)
                    if as_of and e.get("facts_only"):
                        out.fact_days.setdefault((e["provider"], e["ref"], fact), set()).add(as_of)
            elif e.get("kind") == "security":
                seen(out.lookups, (e["provider"], e["ref"]), day)
                if e.get("families"):
                    seen(out.families, (e["provider"], e["ref"]), day)
                for fact in e.get("facts") or ():
                    seen(out.wants, (e["provider"], e["ref"], fact), day)
            elif e.get("kind") == "bulk":
                seen(out.bulk, (e["provider"], e["ref"]), day)
                for key in _answered(run / e["dir"]) if e.get("dir") else ():
                    seen(out.answered, (e["provider"], key), day)
                    for fact in e.get("facts") or ():
                        seen(out.wants, (e["provider"], key, fact), day)
    return out


def _answered(folder: Path) -> set:
    """The security keys a bulk answer names in its meta.json's
    `answered` (base.py); none where it names none."""
    try:
        return flat_keys(read_meta(folder).get("answered"))
    except (ProviderError, OSError, ValueError, AttributeError):
        return set()


def _ask(ref: str, provider: str, what: str, call, *args):
    """call(*args): a provider's answer about one fund or security, or
    None where it fails. Its own error is a warning, anything else a
    traceback; the run goes on either way."""
    try:
        return call(*args)
    except LOOKUP_ERRORS as e:
        log.warning("%s: %s could not %s: %s", ref, provider, what, e)
    except Exception:  # noqa: BLE001 - one provider's surprise must not end the run
        log.exception("%s: %s could not %s", ref, provider, what)
    return None


class _Asked:
    """One fund's questions to the providers, each asked once a run: the
    composition and the fact planning share its claims, newest dates and
    date lists. The dates are those in [since, until]."""

    def __init__(self, fund, ctx, since, until):
        self.fund, self.ctx, self.since, self.until, self.answers = fund, ctx, since, until, {}

    def _once(self, p, what, call, *args):
        if (p.name, what) not in self.answers:
            self.answers[(p.name, what)] = _ask(self.fund.ref, p.name, what, call, *args)
        return self.answers[(p.name, what)]

    def claim(self, p):
        return self._once(p, "claim it", p.claim, self.fund, self.ctx)

    def latest(self, p):
        return self._once(p, "name its newest date", p.latest, self.claim(p), self.ctx)

    def dates(self, p) -> list:
        return self._once(p, "list its dates", p.dates, self.claim(p), self.since, self.until, self.ctx) or []


def described(fund: wantedmod.Fund, claim: dict) -> wantedmod.Fund:
    """The fund with its empty identifiers filled from the claim's `ids`.
    A provider standing by can then claim it by the one it reads: N-PORT
    finds a fund named by its ISIN by its ticker."""
    ids = claim.get("ids") or {}
    ids = {k: v for k in ("isin", "cusip", "symbol") if (v := ids.get(k)) and not getattr(fund, k)}
    return replace(fund, **ids) if ids else fund


def _answers_fact(p, fact, kind="fund") -> bool:
    """Whether a provider answers a fact of a fund (`kind` fund) or of a
    security: a fund's by its `facts`, as a fund provider; a security's (a
    family of figures, a fact the security wants) by its `answers`, as a
    security or bulk provider."""
    if kind == "fund":
        return p.serves_funds and fact in p.facts
    return (p.serves_securities or p.serves_bulk) and fact in p.answers


def _fresh(day, today, max_age) -> bool:
    return day is not None and (today - day).days <= max_age


def _past_days(listed, held_from, have, near=NEAR_DAYS):
    """The dates of `listed` a backfill asks for: every one from held_from
    on, and the last one before it, the one in force on the first day
    held; none with a day of `have` within `near` days. have gains each."""
    before = [d for d in listed if d < held_from]
    for d in [*before[-1:], *(d for d in listed if d >= held_from)]:
        if any(abs((d - h).days) <= near for h in have):
            continue
        have.add(d)
        yield d


def plan(want: wantedmod.Wanted, providers, ctx: Context, *, today: date, max_age: int, backfill: bool,
         refs=None, securities=True, constituents=False, floor_pct=0, earlier: Fetched | None = None):
    """The requests a run makes, the funds' then the securities' and the
    bulk ones; the funds that want a composition no provider claims; and
    what becomes of every fact wanted, as fact -> ref or key -> Cover.
    `earlier` is what earlier runs fetched (fetched_before)."""
    cover, earlier = {}, earlier or Fetched.of()
    reqs, unclaimed = _fund_requests(want, providers, ctx, today=today, max_age=max_age, backfill=backfill,
                                     refs=refs, earlier=earlier, cover=cover)
    if securities:
        secs = security_order(want.securities, constituents, floor_pct)
        asks = _asks(providers, ctx)
        own = _security_requests(secs, providers, ctx, today=today, max_age=max_age, refs=refs, earlier=earlier,
                                 asks=asks)
        bulk = _bulk_requests(secs, providers, ctx, today=today, max_age=max_age, refs=refs, earlier=earlier)
        reqs += own + bulk
        _cover_securities(secs, providers, own + bulk, today=today, max_age=max_age, refs=refs, earlier=earlier,
                          cover=cover, asks=asks)
    return reqs, unclaimed, cover


def _fund_requests(want, providers, ctx, *, today, max_age, backfill, refs, earlier, cover):
    """The fund requests a run makes, and the funds that want a
    composition no provider claims: each fund's composition requests, then
    its facts' (_fact_requests)."""
    reqs, unclaimed = [], []
    served_days, fetched_days, provider_days = {}, {}, {}
    for (q, r, d), day in earlier.funds.items():
        provider_days.setdefault((q, r), set()).add(d)
        served_days[(q, r)] = max(served_days.get((q, r), day), day)
        fetched_days.setdefault(r, set()).add(d)
    gap = timedelta(days=want.max_gap_days)
    funds = [p for p in providers if p.serves_funds]
    rank = {p.name: i for i, p in enumerate(funds)}
    for fund in want.funds:
        if refs and fund.ref not in refs:
            continue
        asked = _Asked(fund, ctx, (fund.held_from or today) - gap, fund.held_to or today)
        own, lacks = _composition_requests(fund, funds, asked, today=today, max_age=max_age, backfill=backfill,
                                           have=set(fund.compositions) | fetched_days.get(fund.ref, set()),
                                           served_days=served_days)
        _fact_requests(fund, funds, asked, own, rank, today=today, max_age=max_age, backfill=backfill,
                       earlier=earlier, provider_days=provider_days, served_days=served_days, cover=cover)
        reqs += own
        if lacks:
            unclaimed.append(fund.ref)
    return reqs, unclaimed


def _composition_requests(fund, providers, asked, *, today, max_age, backfill, have, served_days):
    """A fund's composition requests, and whether it wants a composition
    no provider claims.

    The inbox (an additive provider) serves every fund it has a folder
    for. A fund held today, or wanted in one of CURRENT_ROLES, wants its
    current composition once the newest one had is older than max_age. A
    backfill wants every date inside the days the fund was held, and the
    last one before them within max_gap_days: the composition in force
    when the holding began. Only a fund that wants either is offered to
    the other providers that serve compositions: the first that claims it
    serves it, and those after it stand by for its current composition.
    The current one is not asked for where the provider's newest is
    already had, or where the provider served one within max_age, as a
    report that states a day long past."""
    own, claimed = [], False
    for p in providers:
        claim = asked.claim(p) if p.additive else None
        if claim is None:
            continue
        claimed = True
        for d in asked.dates(p):
            if d not in have:
                own.append(Request("fund", p.name, fund.ref, d, claim))
                have.add(d)
    newest = max(have, default=None)
    current = (fund.held_to is None or not CURRENT_ROLES.isdisjoint(fund.roles)) \
        and (newest is None or (today - newest).days > max_age)
    past = backfill and fund.held_from is not None
    if not (current or past):
        return own, False
    others = [p for p in providers if not p.additive and p.serves_compositions]
    for i, p in enumerate(others):
        claim = asked.claim(p)
        if claim is None:
            continue
        claimed = True
        served_on = served_days.get((p.name, fund.ref))
        if current and (served_on is None or (today - served_on).days > max_age):
            latest = asked.latest(p)
            if latest is None or latest not in have:
                own.append(Request("fund", p.name, fund.ref, None, claim,
                                   tuple(q.name for q in others[i + 1:]), described(fund, claim)))
                if latest is not None:
                    have.add(latest)
        if past:
            own += [Request("fund", p.name, fund.ref, d, claim)
                    for d in _past_days(asked.dates(p), fund.held_from, have)]
        break
    return own, not claimed


def _fact_requests(fund, providers, asked, own, rank, *, today, max_age, backfill, earlier, provider_days,
                   served_days, cover):
    """Add to own, a fund's requests, those for the facts it wants beyond
    compositions, and record in cover what becomes of each while the fund
    is held.

    A held fund's fact is due once gold's newest day for it is older than
    max_age. The providers whose `facts` name it are asked in refdata.cfg's
    order, whichever serves the compositions. The first that claims the
    fund is asked for its current answer, and those after it that answer
    the fact stand by. Where the composition planning asks the claimant
    for the same day, that request asks for the fact too. Else the
    claimant is not asked where an earlier run fetched the fact from it
    within max_age, or where the newest day it names is one gold holds
    the fact for or one an earlier run fetched from it: a composition's
    day, or a day a request for facts alone fetched the fact for. A
    backfill asks the claimant, where it states the fact for past days
    (`dated_facts`), for every date it lists inside the days held, and
    the last one before them, that is none of those days, nor the newest
    one when the current answer is asked for.

    A fact that no provider's `facts` name, but some provider's
    `composition_facts` do, gets no request of its own. A due one is
    asked for by each of the fund's composition requests to such a
    provider, and had where an earlier run fetched a composition from one
    within max_age."""
    held = fund.held_to is None
    past = backfill and fund.held_from is not None
    for fact in fund.wants:
        days = fund.facts.get(fact, ())
        due = held and not _fresh(max(days, default=None), today, max_age)
        c = Cover(had=held and not due)
        if held:
            cover.setdefault(fact, {})[fund.ref] = c
        if not (due or past):
            continue
        answering = [p for p in providers if fact in p.facts]
        if not answering:
            if due:
                stating = {p.name for p in providers if fact in p.composition_facts}
                c.requests = [r for r in own if r.compositions and r.provider in stating]
                c.had = not c.requests and any(_fresh(served_days.get((q, fund.ref)), today, max_age)
                                               for q in stating)
                if not (c.requests or c.had):
                    c.reason = _unanswered(fact, providers)
            continue
        for i, p in enumerate(answering):
            claim = asked.claim(p)
            if claim is None:
                continue
            known = {*days, *provider_days.get((p.name, fund.ref), ()),
                     *earlier.fact_days.get((p.name, fund.ref, fact), ())}
            if due:
                joins = any(r.provider == p.name and r.as_of is None for r in own)
                c.had = not joins and (_fresh(earlier.facts.get((p.name, fund.ref, fact)), today, max_age)
                                       or asked.latest(p) in known)
                if not c.had:
                    c.requests.append(_asking(own, fund, p, None, claim, fact, answering[i + 1:], rank))
                    # The current answer is the newest day's, where the
                    # provider names one: no past request asks for it again.
                    if asked.latest(p) is not None:
                        known.add(asked.latest(p))
            if past and fact in p.dated_facts:
                for d in _past_days(asked.dates(p), fund.held_from, known, near=0):
                    _asking(own, fund, p, d, claim, fact, (), rank)
            break
        else:
            c.reason = "no provider claims it"


def _asking(own, fund, p, as_of, claim, fact, standbys, rank) -> Request:
    """The fund's request to p for as_of, which asks for fact too: the one
    already planned, a composition's or another fact's, else a new one
    for facts alone. The providers standing by for the fact join its
    fallbacks, in refdata.cfg's order; one that serves no compositions
    stands by on a request for facts alone only."""
    r = next((r for r in own if r.provider == p.name and r.as_of == as_of), None)
    if r is None:
        r = Request("fund", p.name, fund.ref, as_of, claim, fund=described(fund, claim) if as_of is None else None,
                    compositions=False)
        own.append(r)
    if fact not in r.facts:
        r.facts += (fact,)
    joining = [q.name for q in standbys if not r.compositions or q.serves_compositions]
    r.fallbacks = tuple(sorted({*r.fallbacks, *joining}, key=rank.__getitem__))
    return r


def _facts_missing(folder: Path) -> set:
    """The facts a fetch that succeeded names as missing in its meta.json
    (base.py): they stay due, as if the run had not asked."""
    try:
        return set(read_meta(folder).get("facts_missing") or ())
    except (ProviderError, OSError, ValueError):
        return set()


def _unanswered(fact, providers, kind="fund") -> str:
    """Why no provider of the run is asked for a fact of a fund (`kind`
    fund) or of a security: a fund's comes only with a composition from
    the registered providers whose `composition_facts` name it; the
    registered providers that answer it are off, as refdata.cfg does not
    enable them or --provider leaves them out; or none answers it."""
    if kind == "fund":
        stating = [name for name, p in REGISTRY.items() if fact in p.composition_facts]
        if stating:
            return f"comes only with a composition from: {', '.join(stating)}"
    on = {p.name for p in providers}
    off = [name for name, p in REGISTRY.items() if name not in on and _answers_fact(p, fact, kind)]
    return f"provider off: {', '.join(off)}" if off else "no enabled provider answers it"


def _last_answer(sec, p, lookups):
    """The day p last answered for a security: in gold, or in an earlier
    run."""
    return max(filter(None, (sec.lookups.get(p.name), lookups.get((p.name, sec.key)))), default=None)


def _wanted_day(sec, fact, fetched):
    """The newest day a fact the security wants was had: gold's day for
    it (SECURITY_WANTS), or the day an earlier run fetched it."""
    return max(filter(None, (getattr(sec, SECURITY_WANTS[fact]), fetched)), default=None)


def _families(claim):
    """The families of figures a security or bulk request's claim asks
    for (its `families`), or None where the claim does not say."""
    return claim.get("families") if isinstance(claim, dict) else None


def _claim_facts(claim) -> tuple:
    """The facts of a security's `wants` a security or bulk request's
    claim asks for (its `facts`); none where it does not say."""
    facts = claim.get("facts") if isinstance(claim, dict) else None
    return tuple(f for f in facts or () if f in SECURITY_WANTS)


def _asks_for(r, fact) -> bool:
    """Whether a security or bulk request asks for a fact a security wants
    (the request's `facts` name it, from its claim) or a family of
    figures it lacks (its claim's `families` names it). A security
    request whose claim does not name families asks for whatever its
    provider answers; a bulk claim names its families (base.py)."""
    if fact in SECURITY_WANTS:
        return fact in r.facts
    families = _families(r.claim)
    return fact in families if families is not None else r.kind == "security"


def _asks(providers, ctx) -> dict:
    """provider -> security key -> the day of its last request, as each
    provider that waits min_age_days keeps it (Provider.asked); None
    where its record does not read, a warning: it is asked nothing."""
    return {p.name: _ask("plan", p.name, "read the days of its requests", p.asked, ctx)
            for p in providers if p.min_age_days}


def _waits(p, sec, earlier, today, asks) -> bool:
    """Whether p waits before it is asked about the security again: it
    waits min_age_days between two requests, and an earlier run sent one
    within them, by its manifest (Fetched.asked) or by the provider's own
    record (_asks), which counts a run that died too. A provider whose
    record does not read waits."""
    if not p.min_age_days:
        return False
    days = asks.get(p.name)
    return days is None or any(_fresh(d, today, p.min_age_days)
                               for d in (earlier.asked.get((p.name, sec.key)), days.get(sec.key)))


def _security_requests(secs, providers, ctx, *, today, max_age, refs, earlier, asks):
    """The security requests a run makes, in security_order: each
    security of every security provider that wants it, can claim it, and
    has not answered for it within max_age, here or in an earlier run.

    For a security held directly that lacks a family of figures the
    provider answers, only an earlier run's request that asked for
    families counts: an answer without them does not, nor the day of a
    lookup that gold holds. Once asked, a company that files no figures
    waits max_age like any lookup.

    A fact the security wants (SECURITY_WANTS) that the provider answers
    is due once the newest of gold's day for it and the day an earlier
    run's request to the provider asked for it is older than max_age.
    The provider claims the security with only its due facts in `wants`,
    and the request asks for those its claim names in `facts`. A claim
    that names none makes a request only where the lookup is due. A
    provider that answers nothing but such facts is asked only where one
    is due.

    A provider that waits min_age_days between two requests for one
    security reads max_age as at least that, and is not asked where an
    earlier run sent it a request within those days, one that failed
    too (_waits)."""
    reqs = []
    for sec in secs:
        if refs and sec.key not in refs:
            continue
        for p in providers:
            if not p.serves_securities or (sec.scope == "constituent" and not p.wants_constituent(sec)):
                continue
            if _waits(p, sec, earlier, today, asks):
                continue
            age = max(max_age, p.min_age_days)
            due = tuple(f for f in sec.wants if f in SECURITY_WANTS and f in p.answers and not _fresh(
                _wanted_day(sec, f, earlier.wants.get((p.name, sec.key, f))), today, age))
            figures = sec.scope == "direct" and bool(p.answers.intersection(sec.missing, SECURITY_FACTS))
            last = earlier.families.get((p.name, sec.key)) if figures else _last_answer(sec, p, earlier.lookups)
            wants_only = bool(p.answers) and p.answers.issubset(SECURITY_WANTS)
            lookup = not wants_only and not _fresh(last, today, age)
            if not (lookup or due):
                continue
            claim = _ask(sec.key, p.name, "claim it", p.claim_security, replace(sec, wants=due), ctx)
            facts = tuple(f for f in due if f in _claim_facts(claim))
            if claim is not None and (lookup or facts):
                reqs.append(Request("security", p.name, sec.key, None, claim, facts=facts))
    return reqs


def flat_keys(keys) -> set:
    """Security keys as base.py's bulk contract states them: a list, or
    lists (or one key) by another identifier, such as a CIK."""
    if isinstance(keys, dict):
        keys = [k for v in keys.values() for k in ([v] if isinstance(v, str) else v)]
    return set(keys or ())


def bulk_keys(claim) -> set:
    """The security keys a bulk request's claim names, in its `keys`."""
    return flat_keys(claim.get("keys") if isinstance(claim, dict) else None)


def _bulk_requests(secs, providers, ctx, *, today, max_age, refs, earlier):
    """The bulk requests a run makes: each bulk provider plans them for the
    securities a run asks for, in security_order, given the refs earlier
    runs fetched from it, the day of each, and max_age. A request asks
    for the facts of the securities' `wants` its claim names in `facts`
    and the provider answers. --ref keeps only the bulk refs it names."""
    reqs = []
    for p in providers:
        if not p.serves_bulk:
            continue
        before = {ref: day for (q, ref), day in earlier.bulk.items() if q == p.name}
        planned = _ask("bulk", p.name, "plan its requests", p.plan_bulk, secs, ctx, today, before, max_age) or []
        reqs += [Request("bulk", p.name, ref, None, claim, facts=tuple(f for f in _claim_facts(claim) if f in p.answers))
                 for ref, claim in planned if not refs or ref in refs]
    return reqs


def _cover_securities(secs, providers, reqs, *, today, max_age, refs, earlier, cover, asks):
    """Record in cover what becomes of each family of figures a security
    lacks (SECURITY_FACTS) and each fact it wants (SECURITY_WANTS): the
    requests of a provider that answers it which name the security and
    ask for it (_asks_for); a fact wanted that gold holds within max_age;
    an earlier run's answer within max_age, for a family from a security
    request to that provider that asked for families (a plain lookup does
    not count) or a bulk answer whose `answered` names the security,
    for a fact wanted from a request that asked for it, within the
    provider's min_age_days where they are more; or why none is asked,
    a provider that waits min_age_days after a request that failed
    too."""
    naming = {}
    for r in reqs:
        for key in ({r.ref} if r.kind == "security" else bulk_keys(r.claim)):
            naming.setdefault(key, []).append(r)
    by_name = {p.name: p for p in providers}
    for sec in secs:
        if refs and sec.key not in refs:
            continue
        for fact in (*(f for f in sec.missing if f in SECURITY_FACTS), *(f for f in sec.wants if f in SECURITY_WANTS)):
            wanted = fact in SECURITY_WANTS
            cover.setdefault(fact, {})[sec.key] = c = Cover(
                had=wanted and _fresh(getattr(sec, SECURITY_WANTS[fact]), today, max_age))
            if c.had:
                continue
            answering = [p for p in providers if _answers_fact(p, fact, "security")]
            if not answering:
                c.reason = _unanswered(fact, providers, "security")
                continue
            c.requests = [r for r in naming.get(sec.key, ()) if fact in by_name[r.provider].answers
                          and _asks_for(r, fact)]
            if wanted:
                c.had = any(_fresh(earlier.wants.get((p.name, sec.key, fact)), today, max(max_age, p.min_age_days))
                            for p in answering)
            else:
                c.had = any((p.serves_securities and _fresh(earlier.families.get((p.name, sec.key)), today, max_age))
                            or (p.serves_bulk and _fresh(earlier.answered.get((p.name, sec.key)), today, max_age))
                            for p in answering)
            if not (c.requests or c.had):
                c.reason = next((f"{p.name} waits {p.min_age_days} days after a request" for p in answering
                                 if p.serves_securities and _waits(p, sec, earlier, today, asks)),
                                "no provider claims it")


def tally(cover, deferred=frozenset()) -> list:
    """Per fact wanted, in FACTS order: (fact, how many funds or
    securities want it, how many a request asks for it, how many had it
    within max_age, each one left as (ref or key, reason)). A request
    whose id is in `deferred` asks for nothing."""
    out = []
    for fact in sorted(cover, key=lambda f: (FACTS.index(f) if f in FACTS else len(FACTS), f)):
        planned = had = 0
        left = []
        for ref, c in cover[fact].items():
            if any(id(r) not in deferred for r in c.requests):
                planned += 1
            elif c.had:
                had += 1
            else:
                left.append((ref, DEFERRED if c.requests else c.reason))
        out.append((fact, len(cover[fact]), planned, had, left))
    return out


def coverage_lines(cover, deferred=frozenset()) -> list:
    """The dry run's `facts` section: per fact, its counts, then one line
    per fund or security left, `<fact>\\t<ref or key>\\t<reason>`."""
    out = []
    for fact, wanted, planned, had, left in tally(cover, deferred):
        out.append(f"{fact}\t{wanted} wanted, {planned} planned, {had} had")
        out += [f"{fact}\t{ref}\t{reason}" for ref, reason in left]
    return ["facts", *out] if out else []


def log_coverage(cover, deferred=frozenset()) -> None:
    """A run's counts, one log line per fact, those left by reason."""
    for fact, wanted, planned, had, left in tally(cover, deferred):
        reasons = Counter(reason for _, reason in left)
        log.info("facts: %s: %d wanted, %d planned, %d had%s", fact, wanted, planned, had,
                 "".join(f", {n} {reason}" for reason, n in reasons.items()))


def request_dir(run: Path, r: Request) -> Path:
    if r.kind == "fund":
        return run / "funds" / r.provider / folder_name(r.ref) / (r.as_of.isoformat() if r.as_of else "current")
    return run / ("bulk" if r.kind == "bulk" else "securities") / r.provider / folder_name(r.ref)


def execute(reqs, run: Path, ctx: Context, limit=None):
    """Fetch every request into its bronze folder; one entry per request.
    A provider standing by claims the fund only once the ones before it
    failed. `limit` caps the fetch attempts, a standby's included; a
    request it leaves no attempt for is deferred. A request's entry names
    the facts it asks for, once one serves it those its provider answers
    and its meta.json does not name missing; a security request's the
    families of figures its claim asks for too."""
    results, attempts = [], 0
    for r in reqs:
        entry = {"kind": r.kind, "provider": r.provider, "ref": r.ref,
                 "asked": r.as_of.isoformat() if r.as_of else "current"}
        if r.facts:
            entry["facts"] = list(r.facts)
        if r.kind == "fund" and not r.compositions:
            entry["facts_only"] = True
        if r.kind == "security" and _families(r.claim):
            entry["families"] = list(_families(r.claim))
        # The first that serves the request is recorded; the others'
        # errors too.
        tried = []
        for name in (r.provider, *r.fallbacks):
            if limit is not None and attempts >= limit:
                break
            claim = r.claim if name == r.provider else _ask(r.ref, name, "claim it", REGISTRY[name].claim, r.fund, ctx)
            if claim is None:
                continue
            attempts += 1
            status, error = _fetch(Request(r.kind, name, r.ref, r.as_of, claim), run, ctx, entry)
            if status == "ok":
                entry["provider"] = name
                if r.facts:
                    missing = _facts_missing(run / entry["dir"])
                    entry["facts"] = [f for f in r.facts if _answers_fact(REGISTRY[name], f, r.kind) and f not in missing]
                break
            tried.append({"provider": name, "status": status, "error": error})
        if tried and entry.get("status") != "ok":
            entry["status"], entry["error"] = tried[0]["status"], tried[0]["error"]
        entry.setdefault("status", "deferred")
        if len(tried) > 1 or (tried and entry["status"] == "ok"):
            entry["tried"] = tried
        results.append(entry)
    return results


def _fetch(r: Request, run: Path, ctx: Context, entry: dict):
    """One provider's attempt at one request; fills entry on success and
    returns (status, error)."""
    out = request_dir(run, r)
    p = REGISTRY[r.provider]
    try:
        if r.kind == "fund":
            got = p.fetch(r.claim, r.as_of, out, ctx)
            if got is None:
                raise ProviderError("the files fetched state no day")
            entry["as_of"] = got.isoformat()
        elif r.kind == "bulk":
            p.fetch_bulk(r.claim, out, ctx)
        else:
            p.fetch_security(r.claim, out, ctx)
        entry["status"] = "ok"
        entry["dir"] = str(out.relative_to(run))
        return "ok", None
    except Unavailable as e:
        return "unavailable", str(e)
    except LOOKUP_ERRORS as e:
        log.warning("%s %s (%s): %s", r.provider, r.ref, entry["asked"], e)
        return "error", str(e)
    except Exception as e:  # noqa: BLE001 - one bad request must not abort the run
        log.exception("%s %s (%s) failed", r.provider, r.ref, entry["asked"])
        return "error", f"{type(e).__name__}: {e}"


def probe(providers, ctx) -> int:
    """Ask every provider for its cheapest answer; 1 where any fails."""
    rc = 0
    for p in providers:
        try:
            log.info("%s: %s", p.name, p.probe(ctx))
        except LOOKUP_ERRORS as e:
            log.error("%s: probe failed: %s", p.name, e)
            rc = 1
        except Exception:  # noqa: BLE001 - one provider's surprise must not hide the others' answers
            log.exception("%s: probe failed", p.name)
            rc = 1
    return rc


def main(argv=None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)
    cli.resolve_standard(args, verb="download", full_history=True, log=log,
                         what="what the wanted list asks for")
    try:
        cfg = config.with_finra_credential(config.load(REGISTRY), args.client_id)
    except config.ConfigError as e:
        log.error("%s", e)
        return 2
    providers = enabled_providers(cfg, args.provider)
    ctx = Context(client=fetcher.Client(cfg.contact), data_dir=args.bronze_dir, config=cfg,
                  providers=frozenset(p.name for p in providers))
    if args.check:
        return probe(providers, ctx)
    if args.bronze_dir is None:
        raise SystemExit("--bronze-dir is required (or pass --check to probe the providers).")
    try:
        want = wantedmod.from_file(args.wanted) if args.wanted else wantedmod.from_engine(config.engine_command(cfg))
    except wantedmod.WantedError as e:
        log.error("%s", e)
        return 3
    if not args.dry_run:
        # The run dir comes before the plan, so --debug traces the
        # requests the plan makes as well.
        args.bronze_dir.mkdir(parents=True, exist_ok=True)
        slug = bronze.ts_slug()
        run = bronze.run_dir(args.bronze_dir, slug)
        bronze.atomic_write_json(run / "run.json", {"slug": slug, "status": "in-progress"})
        ctx.client.trace = debugcap.HttpTrace(run, log=log, enabled=args.debug)
    earlier = fetched_before(args.bronze_dir)
    reqs, unclaimed, cover = plan(want, providers, ctx, today=today_utc(), max_age=cfg.max_age_days,
                                  backfill=args.backfill, refs=set(args.ref or ()),
                                  securities=not args.no_securities, constituents=cfg.constituents,
                                  floor_pct=cfg.constituent_floor_pct, earlier=earlier)
    log.info("wanted: %d funds, %d securities; %d requests; %d funds no provider claims",
             len(want.funds), len(want.securities), len(reqs), len(unclaimed))
    for ref in unclaimed:
        log.info("unclaimed: %s", ref)
    if args.dry_run:
        for r in reqs:
            print(f"{r.kind}\t{r.provider}\t{r.ref}\t{r.as_of.isoformat() if r.as_of else 'current'}")
        # Each request counts as one attempt here: a standby's is not
        # known before a fetch fails.
        deferred = {id(r) for r in reqs[max(args.limit, 0):]} if args.limit is not None else set()
        for line in coverage_lines(cover, deferred):
            print(line)
        return 0
    manifest = {"slug": slug, "status": "in-progress", "wanted_as_of": want.as_of.isoformat(),
                "providers": [p.name for p in providers], "unclaimed": unclaimed, "requests": []}
    bronze.atomic_write_json(run / "run.json", manifest)
    manifest["requests"] = execute(reqs, run, ctx, args.limit)
    manifest["status"] = "complete"
    bronze.atomic_write_json(run / "run.json", manifest)
    by = Counter(e["status"] for e in manifest["requests"])
    log.info("run %s: %s", slug, ", ".join(f"{n} {s}" for s, n in sorted(by.items())) or "nothing to fetch")
    log_coverage(cover, {id(r) for r, e in zip(reqs, manifest["requests"], strict=True) if e["status"] == "deferred"})
    log_notes(providers, log)
    return 0


if __name__ == "__main__":
    sys.exit(main())
