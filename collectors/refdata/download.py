#!/usr/bin/env python3
"""
refdata downloader: the wanted list in, raw provider files out.

1. Read the wanted list: ask the engine (`wealthdb -r refdata-wanted`),
   or read --wanted FILE.
2. For every wanted fund, ask the providers in refdata.cfg's order; the
   first that claims it serves it, and the inbox serves it as well. Fetch
   its current composition when the newest one had (in gold, or fetched
   by an earlier run) is older than max_age_days and the provider has a
   newer one. With --backfill, also fetch every past date the provider
   can serve inside the days the fund was held, and the last one before
   them, that has no composition within half a month.
3. For every wanted security held directly, ask every enabled security
   provider whose last answer is older than max_age_days; with
   `constituents`, then ask for the securities held through funds whose
   share of net worth reaches `constituent_floor_pct`, the biggest first,
   each only of a provider that can fill a field it lacks.
4. Write each answer's raw files into bronze, one folder per provider,
   fund and date:

       <run>/funds/<provider>/<reference>/<YYYY-MM-DD | current>/
       <run>/securities/<provider>/<key>/

   and the run's manifest, run.json, with the outcome of every request.
   One failed request does not fail the run.

Reads public pages only: no login, a User-Agent that names the tool, one
request a second per host (eight for the SEC, under its limit of ten).

Usage:
    download.py --bronze-dir DIR [--wanted FILE] [--backfill]
                [--provider NAME] [--ref REF] [--limit N] [--dry-run]
"""

from __future__ import annotations

import argparse
import logging
import sys
from dataclasses import dataclass
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

import config
import fetcher
import wanted as wantedmod
from collectorkit import bronze, cli, debugcap
from providers import REGISTRY
from providers.base import Context, ProviderError, Unavailable, today_utc

log = logging.getLogger("refdata.download")

# A past date counts as covered when gold holds a composition this close
# to it: half the month-end cadence.
NEAR_DAYS = 15
# The roles that want a fund's current composition whether or not it is
# held today: it stands in for, widens or lies under a held fund.
CURRENT_ROLES = frozenset({"proxy", "reference", "underlying"})


@dataclass
class Request:
    kind: str          # fund | security
    provider: str
    ref: str
    as_of: date | None = None   # None: the current composition
    claim: object = None
    # Further providers that claim the fund, asked in order when this one
    # cannot serve the current composition: (provider name, claim).
    fallbacks: tuple = ()


def parse_args(argv):
    p = argparse.ArgumentParser(description=__doc__.strip().split("\n\n")[0])
    p.add_argument("--bronze-dir", type=Path, default=None, help="One run dir is created here per invocation.")
    p.add_argument("--wanted", type=Path, default=None,
                   help="Read the wanted list from this file instead of asking the engine.")
    p.add_argument("--backfill", action="store_true",
                   help="Also fetch every past date a provider can serve inside the days a fund was held.")
    p.add_argument("--provider", action="append", default=None,
                   help="Ask only this provider (repeatable); the rest of refdata.cfg's list is skipped.")
    p.add_argument("--ref", action="append", default=None,
                   help="Ask only for this fund reference or security key (repeatable).")
    p.add_argument("--limit", type=int, default=None, help="Fetch at most N files this run.")
    p.add_argument("--no-securities", action="store_true", help="Skip the security lookups.")
    p.add_argument("--check", action="store_true",
                   help="Probe every enabled provider with one request and exit; writes nothing.")
    p.add_argument("--dry-run", action="store_true", help="Plan the requests, print them, fetch nothing.")
    p.add_argument("--debug", action="store_true",
                   help="Trace every HTTP exchange into <run>/screenshots/http-trace.jsonl "
                        "(status, timing, size; no body). prune reclaims it.")
    cli.add_standard_args(p, verb="download", full_history=True)
    return p.parse_args(argv)


def enabled_providers(cfg: config.Config, only):
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


def fetched_before(bronze_dir: Path | None) -> tuple[dict, dict]:
    """What earlier complete runs fetched, which counts as had until gold
    holds it, so a second run before the next load asks for it no more:
    (provider, fund ref, date) of every composition with the day of the
    latest run that fetched it, and the day of each provider's latest
    answer per security."""
    funds, lookups = {}, {}
    if bronze_dir is None or not bronze_dir.is_dir():
        return funds, lookups
    for run in bronze.iter_run_dirs(bronze_dir):
        manifest = bronze.read_manifest(run / "run.json")
        if manifest is None or manifest.get("status") != "complete":
            continue
        day = datetime.fromtimestamp(bronze.parse_run_ts(run.name), timezone.utc).date()
        for e in manifest.get("requests") or []:
            if e.get("status") != "ok":
                continue
            if e.get("kind") == "fund" and e.get("as_of"):
                k = (e["provider"], e["ref"], date.fromisoformat(e["as_of"]))
                funds[k] = max(funds.get(k, day), day)
            elif e.get("kind") == "security":
                k = (e["provider"], e["ref"])
                lookups[k] = max(lookups.get(k, day), day)
    return funds, lookups


# What a provider's lookup can raise without failing the run: its own
# error, an HTTP failure, or an answer that does not parse (ValueError,
# JSON's included).
LOOKUP_ERRORS = (ProviderError, fetcher.FetchError, ValueError)


def _claim(p, fund, ctx):
    try:
        return p.claim(fund, ctx)
    except LOOKUP_ERRORS as e:
        log.warning("%s: %s could not claim it: %s", fund.ref, p.name, e)
        return None


def _dates(p, claim, fund, since, until, ctx):
    try:
        return p.dates(claim, since, until, ctx) or []
    except LOOKUP_ERRORS as e:
        log.warning("%s: %s could not list its dates: %s", fund.ref, p.name, e)
        return []


def plan(want: wantedmod.Wanted, providers, ctx: Context, *, today: date, max_age: int, backfill: bool,
         refs=None, securities=True, constituents=False, floor_pct=0, fetched=None, looked_up=None):
    """The requests a run makes, and the funds no provider claims.

    The first provider that claims a fund serves it, and the next ones
    stand by for its current composition should it fail; an additive one
    (the inbox) serves it as well. A fund held today, or wanted in one of
    CURRENT_ROLES, wants its current composition once the newest one had
    is older than max_age, unless the provider's newest is already had or
    the provider served one within max_age, as a report that states a day
    long past. A backfill wants every date inside the days the fund was held,
    and the last one before them within max_gap_days: the composition in
    force when the holding began."""
    reqs, unclaimed = [], []
    fetched = fetched or {}
    served_days = {}
    for (q, r, _), day in fetched.items():
        served_days[(q, r)] = max(served_days.get((q, r), day), day)
    gap = timedelta(days=want.max_gap_days)
    for fund in want.funds:
        if refs and fund.ref not in refs:
            continue
        served, standby = [], []
        for p in providers:
            if not p.serves_funds:
                continue
            claim = _claim(p, fund, ctx)
            if claim is None:
                continue
            if p.additive or not any(not q.additive for q, _ in served):
                served.append((p, claim))
            else:
                standby.append((p.name, claim))
        if not served:
            unclaimed.append(fund.ref)
        have = set(fund.compositions) | {d for (_, r, d) in fetched if r == fund.ref}
        current = fund.held_to is None or not CURRENT_ROLES.isdisjoint(fund.roles)
        until = fund.held_to or today
        since = (fund.held_from or today) - gap
        for p, claim in served:
            if p.additive:
                for d in _dates(p, claim, fund, since, until, ctx):
                    if d not in have:
                        reqs.append(Request("fund", p.name, fund.ref, d, claim))
                        have.add(d)
                continue
            newest = max(have) if have else None
            served_on = served_days.get((p.name, fund.ref))
            if current and (newest is None or (today - newest).days > max_age) \
                    and (served_on is None or (today - served_on).days > max_age):
                try:
                    latest = p.latest(claim, ctx)
                except LOOKUP_ERRORS as e:
                    log.warning("%s: %s could not name its newest date: %s", fund.ref, p.name, e)
                    latest = None
                if latest is None or latest not in have:
                    reqs.append(Request("fund", p.name, fund.ref, None, claim, tuple(standby)))
                    if latest is not None:
                        have.add(latest)
            if backfill and fund.held_from is not None:
                days = _dates(p, claim, fund, since, until, ctx)
                before = [d for d in days if d < fund.held_from]
                for d in [*before[-1:], *(d for d in days if d >= fund.held_from)]:
                    if any(abs((d - h).days) <= NEAR_DAYS for h in have):
                        continue
                    reqs.append(Request("fund", p.name, fund.ref, d, claim))
                    have.add(d)
    if securities:
        for sec in security_order(want.securities, constituents, floor_pct):
            if refs and sec.key not in refs:
                continue
            for p in providers:
                if not p.serves_securities or (sec.scope == "constituent" and not p.wants_constituent(sec)):
                    continue
                last = max(filter(None, (sec.lookups.get(p.name), (looked_up or {}).get((p.name, sec.key)))),
                           default=None)
                if last is not None and (today - last).days <= max_age:
                    continue
                try:
                    claim = p.claim_security(sec, ctx)
                except LOOKUP_ERRORS as e:
                    log.warning("%s: %s could not claim it: %s", sec.key, p.name, e)
                    continue
                if claim is not None:
                    reqs.append(Request("security", p.name, sec.key, None, claim))
    return reqs, unclaimed


def request_dir(run: Path, r: Request) -> Path:
    leaf = r.as_of.isoformat() if r.as_of else "current"
    if r.kind == "fund":
        return run / "funds" / r.provider / bronze.safe_stem(r.ref) / leaf
    return run / "securities" / r.provider / bronze.safe_stem(r.ref)


def execute(reqs, run: Path, ctx: Context, limit=None):
    """Fetch every request into its bronze folder; one entry per request."""
    results = []
    fetched = 0
    for r in reqs:
        entry = {"kind": r.kind, "provider": r.provider, "ref": r.ref,
                 "asked": r.as_of.isoformat() if r.as_of else "current"}
        if limit is not None and fetched >= limit:
            entry["status"] = "deferred"
            results.append(entry)
            continue
        # The provider that claimed the request, then each standing by:
        # the first that serves it is recorded; the others' errors too.
        tried = []
        for name, claim in ((r.provider, r.claim), *r.fallbacks):
            status, error = _fetch(Request(r.kind, name, r.ref, r.as_of, claim), run, ctx, entry)
            if status == "ok":
                entry["provider"] = name
                fetched += 1
                break
            tried.append({"provider": name, "status": status, "error": error})
        else:
            entry["status"], entry["error"] = tried[0]["status"], tried[0]["error"]
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
            entry["as_of"] = got.isoformat() if got else None
        else:
            p.fetch_security(r.claim, out, ctx)
        entry["status"] = "ok"
        entry["dir"] = str(out.relative_to(run))
        return "ok", None
    except Unavailable as e:
        return "unavailable", str(e)
    except (ProviderError, fetcher.FetchError, ValueError) as e:
        log.warning("%s %s (%s): %s", r.provider, r.ref, entry["asked"], e)
        return "error", str(e)
    except Exception as e:  # noqa: BLE001 - one bad request must not abort the run
        log.exception("%s %s (%s) failed", r.provider, r.ref, entry["asked"])
        return "error", f"{type(e).__name__}: {e}"


def probe(providers, ctx) -> int:
    rc = 0
    for p in providers:
        try:
            log.info("%s: %s", p.name, p.probe(ctx))
        except (ProviderError, fetcher.FetchError) as e:
            log.error("%s: probe failed: %s", p.name, e)
            rc = 1
    return rc


def main(argv=None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)
    cli.resolve_standard(args, verb="download", full_history=True, log=log,
                         what="what the wanted list asks for")
    try:
        cfg = config.load(REGISTRY)
    except config.ConfigError as e:
        log.error("%s", e)
        return 2
    providers = enabled_providers(cfg, args.provider)
    data_dir = args.bronze_dir
    ctx = Context(client=fetcher.Client(cfg.contact), contact=cfg.contact, data_dir=data_dir, config=cfg)
    if args.check:
        return probe(providers, ctx)
    if args.bronze_dir is None:
        raise SystemExit("--bronze-dir is required (or pass --check to probe the providers).")
    try:
        want = wantedmod.from_file(args.wanted) if args.wanted else wantedmod.from_engine(config.engine_command(cfg))
    except wantedmod.WantedError as e:
        log.error("%s", e)
        return 3
    today = today_utc()
    fetched_funds, looked_up = fetched_before(args.bronze_dir)
    reqs, unclaimed = plan(want, providers, ctx, today=today, max_age=cfg.max_age_days, backfill=args.backfill,
                           refs=set(args.ref or ()), securities=not args.no_securities,
                           constituents=cfg.constituents, floor_pct=cfg.constituent_floor_pct,
                           fetched=fetched_funds, looked_up=looked_up)
    log.info("wanted: %d funds, %d securities; %d requests; %d funds no provider claims",
             len(want.funds), len(want.securities), len(reqs), len(unclaimed))
    for ref in unclaimed:
        log.info("unclaimed: %s", ref)
    if args.dry_run:
        for r in reqs:
            print(f"{r.kind}\t{r.provider}\t{r.ref}\t{r.as_of.isoformat() if r.as_of else 'current'}")
        return 0
    args.bronze_dir.mkdir(parents=True, exist_ok=True)
    slug = bronze.ts_slug()
    run = bronze.run_dir(args.bronze_dir, slug)
    manifest = {"slug": slug, "status": "in-progress", "wanted_as_of": want.as_of.isoformat(),
                "providers": [p.name for p in providers], "unclaimed": unclaimed, "requests": []}
    bronze.atomic_write_json(run / "run.json", manifest)
    ctx.client.trace = debugcap.HttpTrace(run, log=log, enabled=args.debug)
    manifest["requests"] = execute(reqs, run, ctx, args.limit)
    manifest["status"] = "complete"
    bronze.atomic_write_json(run / "run.json", manifest)
    by = {}
    for e in manifest["requests"]:
        by[e["status"]] = by.get(e["status"], 0) + 1
    log.info("run %s: %s", slug, ", ".join(f"{n} {s}" for s, n in sorted(by.items())) or "nothing to fetch")
    return 0


if __name__ == "__main__":
    sys.exit(main())
