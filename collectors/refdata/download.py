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
from datetime import date
from itertools import groupby
from pathlib import Path

import config
import fetcher
import wanted as wantedmod
from collectorkit import bronze, cli, debugcap
from ledger import FIGURES, NAMED, Ledger, flat_keys
from providers import REGISTRY
from providers.base import KIND_DIRS, Context, ProviderError, Unavailable, folder_name, log_notes, read_meta, today_utc
from wants import CLAIM_FACTS, COMPOSITION, IDENTITY, ORDER, fact_of, security_order, wants

log = logging.getLogger("refdata.download")

DEFERRED = "deferred by --limit"
LOOKUP_FAILED = "the lookup failed"
# The questions of _Asked a lost fact can hang on.
CLAIM, LIST_DATES, LIST_HISTORY = "claim it", "list its dates", "list its history"

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


class Fetched(Ledger):
    """The ledger stated as one map per kind of answer, each with the day
    of the latest run that fetched it:

    funds      (provider, fund ref, day of the composition)
    facts      (provider, fund ref, fact)
    fact_days  (provider, fund ref, fact) -> the days a request fetched it for
    lookups    (provider, security key)
    families   (provider, security key), of a security request that asked for figures
    wants      (provider, security key, fact), of a request that asked for a fact the
               security wants: a security request, or a bulk answer that names it
    answered   (provider, security key), of a bulk answer whose `answered` names it
    bulk       (provider, bulk ref)
    asked      (provider, security key), of a security request sent, one that failed too"""

    @classmethod
    def of(cls, *, funds=None, facts=None, fact_days=None, lookups=None, families=None, wants=None, answered=None,
           bulk=None, asked=None) -> Fetched:
        """The maps given, every other one empty."""
        out = cls()
        for (p, ref, day), run in (funds or {}).items():
            out.settle(p, "fund", ref, COMPOSITION, day, run)
        for (p, ref, fact), run in (facts or {}).items():
            out.settle(p, "fund", ref, fact, None, run)
        for (p, ref, fact), days in (fact_days or {}).items():
            for day in days:
                out.settle(p, "fund", ref, fact, day, None)
        for m, fact in ((lookups, IDENTITY), (families, FIGURES), (answered, NAMED)):
            for (p, key), run in (m or {}).items():
                out.settle(p, "security", key, fact, None, run)
        for (p, key, fact), run in (wants or {}).items():
            out.settle(p, "security", key, fact, None, run)
        for (p, ref), run in (bulk or {}).items():
            out.settle(p, "bulk", ref, None, None, run)
        out.sent.update(asked or {})
        return out

    def _map(self, kind, keep, with_fact=False) -> dict:
        return {(p, s, *((f,) if with_fact else ())): last for (p, k, s, f) in self.runs
                if k == kind and keep(f) and (last := self.last(p, k, s, f)) is not None}

    @property
    def funds(self) -> dict:
        return {(p, s, d): run for (p, k, s, f), days in self.runs.items() if (k, f) == ("fund", COMPOSITION)
                for d, run in days.items()}

    @property
    def facts(self) -> dict:
        return self._map("fund", lambda f: f != COMPOSITION, True)

    @property
    def fact_days(self) -> dict:
        return {(p, s, f): days for (p, k, s, f) in self.runs if k == "fund" and f != COMPOSITION
                and (days := self.settled(p, k, s, f))}

    @property
    def lookups(self) -> dict:
        return self._map("security", lambda f: f == IDENTITY)

    @property
    def families(self) -> dict:
        return self._map("security", lambda f: f == FIGURES)

    @property
    def wants(self) -> dict:
        return self._map("security", lambda f: f not in (IDENTITY, FIGURES, NAMED), True)

    @property
    def answered(self) -> dict:
        return self._map("security", lambda f: f == NAMED)

    @property
    def bulk(self) -> dict:
        return self._map("bulk", lambda f: True)

    @property
    def asked(self) -> dict:
        return dict(self.sent)


def parse_args(argv):
    p = argparse.ArgumentParser(description=__doc__.strip().split("\n\n")[0])
    p.add_argument("--bronze-dir", type=Path, default=None, help="One run dir is created here per invocation.")
    p.add_argument("--wanted", type=Path, default=None,
                   help="Read the wanted list from this file instead of asking the engine.")
    p.add_argument("--backfill", action="store_true",
                   help="Also fetch every past date a provider can serve inside the days a fund was held, "
                        "and a benchmark fund's monthly returns from its history_from on.")
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


def fetched_before(bronze_dir: Path | None) -> Fetched:
    """What earlier complete runs fetched (ledger.py), which counts as
    had until gold holds it."""
    return Fetched.read(bronze_dir)


def _ask(ref: str, provider: str, what: str, call, *args, failed=None):
    """call(*args): a provider's answer about one fund or security, or
    None where it fails, which adds the provider to the set `failed`.
    Its own error is a warning, anything else a traceback; the run goes
    on either way."""
    try:
        return call(*args)
    except LOOKUP_ERRORS as e:
        log.warning("%s: %s could not %s: %s", ref, provider, what, e)
    except Exception:  # noqa: BLE001 - one provider's surprise must not end the run
        log.exception("%s: %s could not %s", ref, provider, what)
    if failed is not None:
        failed.add(provider)
    return None


class _Asked:
    """One fund's questions to the providers, each asked once a run: its
    wants share their claims, newest dates and date lists. The dates are
    those in [since, until], or from the fund's history_from on.
    `failed` holds the providers each question failed on, as a None
    alone does not tell a failure from a provider that does not claim
    the fund."""

    def __init__(self, fund, ctx, since, until):
        self.fund, self.ctx, self.since, self.until, self.answers = fund, ctx, since, until, {}
        self.failed = {}

    def _once(self, p, what, call, *args):
        if (p.name, what) not in self.answers:
            self.answers[(p.name, what)] = _ask(self.fund.ref, p.name, what, call, *args,
                                                failed=self.failed.setdefault(what, set()))
        return self.answers[(p.name, what)]

    def failed_on(self, what, names) -> bool:
        """Whether the question `what` failed on one of the providers `names`."""
        return not self.failed.get(what, set()).isdisjoint(names)

    def claim(self, p):
        return self._once(p, CLAIM, p.claim, self.fund, self.ctx)

    def latest(self, p):
        return self._once(p, "name its newest date", p.latest, self.claim(p), self.ctx)

    def dates(self, p, want=None) -> list:
        """The dates a want's backfill reads: from the fund's
        history_from on, or in [since, until]."""
        if want is not None and want.history:
            return self._once(p, LIST_HISTORY, p.dates, self.claim(p), want.listed_from, self.until, self.ctx) or []
        return self._once(p, LIST_DATES, p.dates, self.claim(p), self.since, self.until, self.ctx) or []


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


def _serves(p, fact) -> bool:
    """Whether a fund provider is asked for a fund's fact in its turn: a
    composition of one that serves compositions in place of the others
    (an additive one serves beside them), another fact of one whose
    `facts` name it."""
    return (p.serves_compositions and not p.additive) if fact == COMPOSITION else fact in p.facts


def _dated(p, fact) -> bool:
    """Whether a fund provider can state the fact for a past day: any
    composition, a fact its `dated_facts` name."""
    return fact == COMPOSITION or fact in p.dated_facts


def _fresh(day, today, max_age) -> bool:
    return day is not None and (today - day).days <= max_age


def _newest(*days):
    return max(filter(None, days), default=None)


def _past_days(listed, held_from, have, near):
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
         refs=None, securities=True, constituents=False, floor_pct=0, earlier: Ledger | None = None,
         unplanned=None):
    """The requests a run makes, the funds' then the securities' and the
    bulk ones; the funds that want a composition no provider claims; and
    what becomes of every fact wanted, as fact -> ref or key -> Cover.
    `earlier` is what earlier runs fetched (fetched_before). The set
    `unplanned` gains (fund ref, fact) for a fact a failed lookup left
    with a request missing.

    Each want (wants.py) is routed by its fact: a fund's to the first
    provider that claims the fund (_Planner.first), a security's to every
    provider that answers it, in one request per provider
    (_Planner.every). Then every bulk provider plans its own requests."""
    run = _Planner(providers, ctx, today=today, max_age=max_age, backfill=backfill, ledger=earlier or Ledger(),
                   unplanned=set() if unplanned is None else unplanned)
    subjects = [list(ws) for _, ws in groupby(wants(want, today=today, refs=refs, securities=securities,
                                                    constituents=constituents, floor_pct=floor_pct),
                                              key=lambda w: id(w.subject))]
    for ws in subjects:
        if ws[0].fact.route == "first":
            run.first(ws)
    if securities:
        run.asks = _asks(providers, ctx)
        every = [ws for ws in subjects if ws[0].fact.route == "all"]
        for ws in every:
            run.every(ws)
        run.bulk(security_order(want.securities, constituents, floor_pct), refs)
        run.cover_securities([w for ws in every for w in ws if w.counted])
    return run.reqs, run.unclaimed, run.cover


class _Planner:
    """One plan's state: the requests so far, the funds no provider
    claims, and the coverage, fact -> ref or key -> Cover."""

    def __init__(self, providers, ctx, *, today, max_age, backfill, ledger, unplanned):
        self.providers, self.ctx, self.today, self.max_age, self.backfill = providers, ctx, today, max_age, backfill
        self.ledger, self.unplanned, self.asks = ledger, unplanned, {}
        self.reqs, self.unclaimed, self.cover = [], [], {}
        self.funds = [p for p in providers if p.serves_funds]
        self.rank = {p.name: i for i, p in enumerate(self.funds)}

    def fresh(self, day, max_age=None) -> bool:
        return _fresh(day, self.today, self.max_age if max_age is None else max_age)

    def first(self, ws):
        """A fund's requests, its composition's (ws[0]) then its facts';
        the fund is unclaimed where it wants a composition no provider
        claims, a failed claim aside."""
        fund, composition = ws[0].subject, ws[0]
        asked = _Asked(fund, self.ctx, composition.listed_from, composition.until)
        own = []
        have = {*composition.have, *self.ledger.held("fund", fund.ref, COMPOSITION)}
        lacks = self._first(composition, asked, own, have)
        for w in ws[1:]:
            self._first(w, asked, own, have)
        self.reqs += own
        if lacks and not asked.failed_on(CLAIM, {p.name for p in self.funds}):
            self.unclaimed.append(fund.ref)

    def _first(self, w, asked, own, have) -> bool:
        """Add to own, the fund's requests, those for one want, record in
        the coverage what becomes of it where the want counts there, and
        tell whether no provider claims the fund for it.

        The inbox (an additive provider) serves every composition it has a
        folder for. A current want is due once the newest day had is older
        than max_age: for a composition, of gold's days, an earlier run's
        and the inbox's; for another fact, of gold's. A backfill wants the
        past days from the want's first day on.

        The providers that serve the fact (_serves) are asked in
        refdata.cfg's order. The first that claims the fund is asked for
        its current answer, and those after it stand by. Where a request
        to it for the current day is planned, that request asks for the
        fact too. Else it is not asked where an earlier run fetched the
        fact from it within max_age, or where the newest day it names is
        one known: for a composition, any day had; for another fact, a
        day gold holds it for, a day a request fetched it for, or the day
        of a composition fetched from the provider, but for a series
        fact, as a composition parsed before the collector read the fact
        states none. A backfill asks it, where it states the fact for
        past days (_dated), for every date it lists in the want's span,
        and the last one before them, with no day known within the
        fact's near_days; nor the newest one when the current answer is
        asked for.

        A fact that no provider's `facts` name, but some provider's
        `composition_facts` do, gets no request of its own (_stated).

        A failed claim of a fact due or of past days, or a failed date
        list, adds the fact to `unplanned`: a request that should exist
        does not."""
        fund, fact = w.subject, w.fact.name
        composition = fact == COMPOSITION
        claimed = composition and self._additive(asked, own, have)
        due = w.current and not self.fresh(max(have if composition else w.have, default=None))
        c = Cover(had=w.current and not due)
        if w.counted:
            self.cover.setdefault(fact, {})[fund.ref] = c
        past = self.backfill and w.since is not None
        if not (due or past):
            return False
        serving = [p for p in self.funds if _serves(p, fact)]
        if not serving:
            if due and not composition:
                self._stated(w, c, asked, own)
            return not claimed
        for i, p in enumerate(serving):
            claim = asked.claim(p)
            if claim is None:
                continue
            claimed = True
            known = have if composition else {
                *w.have, *self.ledger.settled(p.name, "fund", fund.ref, fact),
                *(() if w.fact.series else self.ledger.settled(p.name, "fund", fund.ref, COMPOSITION))}
            if due:
                joins = any(r.provider == p.name and r.as_of is None for r in own)
                c.had = not joins and (self.fresh(self.ledger.last(p.name, "fund", fund.ref, fact))
                                       or asked.latest(p) in known)
                if not c.had:
                    c.requests.append(self._asking(own, w, p, None, claim, serving[i + 1:]))
                    # The current answer is the newest day's, where the
                    # provider names one: no past request asks for it again.
                    if asked.latest(p) is not None:
                        known.add(asked.latest(p))
            if past and _dated(p, fact):
                for d in _past_days(asked.dates(p, w), w.since, known, w.fact.near_days):
                    self._asking(own, w, p, d, claim, ())
                if not composition and asked.failed_on(LIST_HISTORY if w.history else LIST_DATES, {p.name}):
                    self.unplanned.add((fund.ref, fact))
            break
        else:
            failed = [p for p in serving if asked.failed_on(CLAIM, {p.name})]
            c.reason = LOOKUP_FAILED if failed else "no provider claims it"
            if not composition and failed and (due or (past and any(_dated(p, fact) for p in failed))):
                self.unplanned.add((fund.ref, fact))
        return not claimed

    def _additive(self, asked, own, have) -> bool:
        """Add to own every composition an additive provider that claims
        the fund holds and none had has; whether one claims it."""
        claimed = False
        for p in self.funds:
            claim = asked.claim(p) if p.additive else None
            if claim is None:
                continue
            claimed = True
            for d in asked.dates(p):
                if d not in have:
                    own.append(Request("fund", p.name, asked.fund.ref, d, claim))
                    have.add(d)
        return claimed

    def _stated(self, w, c, asked, own):
        """Record what becomes of a due fact that only compositions state
        (`composition_facts`): each of the fund's composition requests to
        such a provider asks for it, and it is had where an earlier run
        fetched a composition from one within max_age."""
        fact, ref = w.fact.name, w.subject.ref
        stating = {p.name for p in self.funds if fact in p.composition_facts}
        c.requests = [r for r in own if r.compositions and r.provider in stating]
        c.had = not c.requests and any(self.fresh(self.ledger.last(q, "fund", ref, COMPOSITION)) for q in stating)
        if not (c.requests or c.had):
            c.reason = _unanswered(fact, self.funds)
            if asked.failed_on(CLAIM, stating):
                c.reason = LOOKUP_FAILED
                self.unplanned.add((ref, fact))

    def _asking(self, own, w, p, as_of, claim, standbys) -> Request:
        """The fund's request to p for as_of, which asks for the want's
        fact too: the one already planned, else a new one, for the
        composition, or for facts alone. The providers standing by join
        its fallbacks, in refdata.cfg's order; one that serves no
        compositions stands by on a request for facts alone only."""
        fund, fact = w.subject, w.fact.name
        r = next((r for r in own if r.provider == p.name and r.as_of == as_of), None)
        if r is None:
            r = Request("fund", p.name, fund.ref, as_of, claim, fund=described(fund, claim) if as_of is None else None,
                        compositions=fact == COMPOSITION)
            own.append(r)
        if fact != COMPOSITION and fact not in r.facts:
            r.facts += (fact,)
        joining = [q.name for q in standbys if not r.compositions or q.serves_compositions]
        r.fallbacks = tuple(sorted({*r.fallbacks, *joining}, key=self.rank.__getitem__))
        return r

    def every(self, ws):
        """A security's requests, ws[0] its lookup, then the facts it
        lacks and wants: one to every security provider that wants it,
        can claim it, and has not answered for it within max_age, here or
        in an earlier run.

        For a security held directly that lacks a family of figures the
        provider answers, only an earlier run's request that asked for
        families counts: an answer without them does not, nor the day of a
        lookup that gold holds. Once asked, a company that files no figures
        waits max_age like any lookup.

        A fact the security wants that the provider answers is due once
        the newest of gold's day for it and the day an earlier run's
        request to the provider asked for it is older than max_age. The
        provider claims the security with only its due facts in `wants`,
        and the request asks for those its claim names in `facts`. A
        claim that names none makes a request only where the lookup is
        due. A provider that answers nothing but such facts is asked only
        where one is due.

        A provider that waits min_age_days between two requests for one
        security reads max_age as at least that, and is not asked where an
        earlier run sent it a request within those days, one that failed
        too (_waits)."""
        lookup_want, *rest = ws
        sec = lookup_want.subject
        families = {w.fact.name for w in rest if w.fact.asked_by == "families"}
        claimed = [w for w in rest if w.fact.asked_by == "facts"]
        for p in self.providers:
            if not p.serves_securities or (not lookup_want.current and not p.wants_constituent(sec)):
                continue
            if _waits(p, sec.key, self.ledger, self.today, self.asks):
                continue
            age = max(self.max_age, p.min_age_days)
            due = tuple(w.fact.name for w in claimed if w.fact.name in p.answers and not self.fresh(
                _newest(max(w.have, default=None), self.ledger.last(p.name, "security", sec.key, w.fact.name)), age))
            figures = lookup_want.current and bool(p.answers & families)
            last = self.ledger.last(p.name, "security", sec.key, FIGURES) if figures else _newest(
                lookup_want.by.get(p.name), self.ledger.last(p.name, "security", sec.key, IDENTITY))
            wants_only = bool(p.answers) and p.answers.issubset(CLAIM_FACTS)
            lookup = not wants_only and not self.fresh(last, age)
            if not (lookup or due):
                continue
            claim = _ask(sec.key, p.name, CLAIM, p.claim_security, replace(sec, wants=due), self.ctx)
            facts = tuple(f for f in due if f in _claim_facts(claim))
            if claim is not None and (lookup or facts):
                self.reqs.append(Request("security", p.name, sec.key, None, claim, facts=facts))

    def bulk(self, secs, refs):
        """The bulk requests: each bulk provider plans them for the
        securities a run asks for, in security_order, given the refs
        earlier runs fetched from it, the day of each, and max_age. A
        request asks for the facts of the securities' `wants` its claim
        names in `facts` and the provider answers. --ref keeps only the
        bulk refs it names."""
        for p in self.providers:
            if not p.serves_bulk:
                continue
            before = self.ledger.subjects(p.name, "bulk")
            planned = _ask("bulk", p.name, "plan its requests", p.plan_bulk, secs, self.ctx, self.today, before,
                           self.max_age) or []
            self.reqs += [Request("bulk", p.name, ref, None, claim,
                                  facts=tuple(f for f in _claim_facts(claim) if f in p.answers))
                          for ref, claim in planned if not refs or ref in refs]

    def cover_securities(self, ws):
        """Record in the coverage what becomes of each family of figures
        a security lacks and each fact it wants: the requests of a
        provider that answers it which name the security and ask for it
        (_asks_for); a fact wanted that gold holds within max_age; an
        earlier run's answer within max_age, for a family from a security
        request to that provider that asked for families (a plain lookup
        does not count) or a bulk answer whose `answered` names the
        security, for a fact wanted from a request that asked for it,
        within the provider's min_age_days where they are more; or why
        none is asked, a provider that waits min_age_days after a request
        that failed too."""
        naming = {}
        for r in self.reqs:
            if r.kind != "fund":
                for key in ({r.ref} if r.kind == "security" else bulk_keys(r.claim)):
                    naming.setdefault(key, []).append(r)
        by_name = {p.name: p for p in self.providers}
        for w in ws:
            sec, fact = w.subject, w.fact.name
            wanted = w.fact.asked_by == "facts"
            self.cover.setdefault(fact, {})[sec.key] = c = Cover(
                had=wanted and self.fresh(max(w.have, default=None)))
            if c.had:
                continue
            answering = [p for p in self.providers if _answers_fact(p, fact, "security")]
            if not answering:
                c.reason = _unanswered(fact, self.providers, "security")
                continue
            c.requests = [r for r in naming.get(sec.key, ()) if fact in by_name[r.provider].answers
                          and _asks_for(r, fact)]
            if wanted:
                c.had = any(self.fresh(self.ledger.last(p.name, "security", sec.key, fact),
                                       max(self.max_age, p.min_age_days)) for p in answering)
            else:
                c.had = any((p.serves_securities and self.fresh(self.ledger.last(p.name, "security", sec.key, FIGURES)))
                            or (p.serves_bulk and self.fresh(self.ledger.last(p.name, "security", sec.key, NAMED)))
                            for p in answering)
            if not (c.requests or c.had):
                c.reason = next((f"{p.name} waits {p.min_age_days} days after a request" for p in answering
                                 if p.serves_securities and _waits(p, sec.key, self.ledger, self.today, self.asks)),
                                "no provider claims it")


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


def _families(claim):
    """The families of figures a security or bulk request's claim asks
    for (its `families`), or None where the claim does not say."""
    return claim.get("families") if isinstance(claim, dict) else None


def _claim_facts(claim) -> tuple:
    """The facts of a security's `wants` a security or bulk request's
    claim asks for (its `facts`); none where it does not say."""
    facts = claim.get("facts") if isinstance(claim, dict) else None
    return tuple(f for f in facts or () if f in CLAIM_FACTS)


def _asks_for(r, fact) -> bool:
    """Whether a security or bulk request asks for a fact a security wants
    (the request's `facts` name it, from its claim) or a family of
    figures it lacks (its claim's `families` names it). A security
    request whose claim does not name families asks for whatever its
    provider answers; a bulk claim names its families (base.py)."""
    if fact in CLAIM_FACTS:
        return fact in r.facts
    families = _families(r.claim)
    return fact in families if families is not None else r.kind == "security"


def _asks(providers, ctx) -> dict:
    """provider -> security key -> the day of its last request, as each
    provider that waits min_age_days keeps it (Provider.asked); None
    where its record does not read, a warning: it is asked nothing."""
    return {p.name: _ask("plan", p.name, "read the days of its requests", p.asked, ctx)
            for p in providers if p.min_age_days}


def _waits(p, key, ledger, today, asks) -> bool:
    """Whether p waits before it is asked about the security again: it
    waits min_age_days between two requests, and an earlier run sent one
    within them, by its manifest (Ledger.last_sent) or by the provider's
    own record (_asks), which counts a run that died too. A provider
    whose record does not read waits."""
    if not p.min_age_days:
        return False
    days = asks.get(p.name)
    return days is None or any(_fresh(d, today, p.min_age_days) for d in (ledger.last_sent(p.name, key), days.get(key)))


def bulk_keys(claim) -> set:
    """The security keys a bulk request's claim names, in its `keys`."""
    return flat_keys(claim.get("keys") if isinstance(claim, dict) else None)


def tally(cover, deferred=frozenset()) -> list:
    """Per fact wanted, in wants.ORDER: (fact, how many funds or
    securities want it, how many a request asks for it, how many had it
    within max_age, each one left as (ref or key, reason)). A request
    whose id is in `deferred` asks for nothing."""
    out = []
    for fact in sorted(cover, key=lambda f: (ORDER.index(f) if f in ORDER else len(ORDER), f)):
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


def _failed(entry) -> bool:
    """Whether a request met an error, on its provider or one standing
    by, and not only providers with nothing to serve."""
    return "error" in (entry["status"], *(t["status"] for t in entry.get("tried", ())))


def lost_funds(cover, reqs, results, unplanned=frozenset()) -> dict:
    """ref -> what a failure cost it this run, sorted, by the --ref that
    recovers it: a fund's ref, a security's key, a bulk request's ref.

    A want is lost where a request that asked for it met an error and
    none answered it: a fund's composition, each fact a request asks
    for, and each row of the coverage, a security's among them. So is
    one a failed lookup left with a request missing (`unplanned`),
    whatever else came. A fund that lost a fact is named by its facts
    alone, its composition only where it lost nothing else: a rerun for
    its facts asks for the composition too. A series fact counts by
    date: a filing's months are its own, one missing month leaves a gap
    in every window over it, and only a backfill asks a past date again.
    A provider with nothing to state loses nothing, and a rerun would
    not change its answer. A request --limit deferred has not failed."""
    entries = {id(r): e for r, e in zip(reqs, results, strict=True)}
    asking = [(r, r.ref, f) for r in reqs if r.kind == "fund"
              for f in ((COMPOSITION,) if r.compositions else ()) + r.facts]
    asking += [(r, ref, fact) for fact, wanting in cover.items() for ref, c in wanting.items() for r in c.requests]
    got, lost = set(), {}
    for r, ref, fact in asking:
        e = entries[id(r)]
        key = (ref, fact, r.as_of if fact_of("fund" if r.kind == "fund" else "security", fact).series else None)
        if e["status"] == "ok" and _answered(r, e, fact):
            got.add(key)
        elif _failed(e):
            lost.setdefault(key, set()).add(r.ref)
    out = {}
    for key, refs in lost.items():
        if key not in got:
            for ref in refs:
                out.setdefault(ref, set()).add(key[1])
    for ref, fact in unplanned:
        out.setdefault(ref, set()).add(fact)
    return {ref: sorted(facts - {COMPOSITION} or facts) for ref, facts in sorted(out.items())}


def _answered(r, entry, fact) -> bool:
    """Whether a request that served answered a fact it asked for: one its
    `facts` name by its entry's facts, as the fetch states them; the
    composition by serving; a fact a fund's compositions state by its
    provider's `composition_facts`; a family of figures by serving."""
    if fact in r.facts:
        return fact in entry["facts"]
    if r.kind == "fund" and fact != COMPOSITION:
        return fact in REGISTRY[entry["provider"]].composition_facts
    return True


def log_lost(lost, backfill) -> None:
    """One closing line per ref a failure cost a want, with the rerun
    that recovers it. The exit code stays 0: one timeout must not fail a
    nightly."""
    flag = "--backfill " if backfill else ""
    for ref, facts in lost.items():
        log.warning("lost: %s: this run failed on %s; rerun with %s--ref %s", ref, ", ".join(facts), flag, ref)


def request_dir(run: Path, r: Request) -> Path:
    folder = run / KIND_DIRS[r.kind] / r.provider / folder_name(r.ref)
    if r.kind == "fund":
        return folder / (r.as_of.isoformat() if r.as_of else "current")
    return folder


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
            unasked = set()
            claim = r.claim if name == r.provider else _ask(r.ref, name, CLAIM, REGISTRY[name].claim, r.fund, ctx,
                                                            failed=unasked)
            if unasked:
                tried.append({"provider": name, "status": "error", "error": "its claim failed"})
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
    unplanned = set()
    reqs, unclaimed, cover = plan(want, providers, ctx, today=today_utc(), max_age=cfg.max_age_days,
                                  backfill=args.backfill, refs=set(args.ref or ()),
                                  securities=not args.no_securities, constituents=cfg.constituents,
                                  floor_pct=cfg.constituent_floor_pct, earlier=earlier, unplanned=unplanned)
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
    log_lost(lost_funds(cover, reqs, manifest["requests"], unplanned), args.backfill)
    return 0


if __name__ == "__main__":
    sys.exit(main())
