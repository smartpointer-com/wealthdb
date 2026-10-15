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
from dataclasses import dataclass, replace
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

# What a provider's answer can raise without failing the run: its own
# error, an HTTP failure or an answer that is not JSON, or an answer that
# does not parse (ValueError). Anything else is logged with its traceback,
# and the run goes on all the same.
LOOKUP_ERRORS = (ProviderError, fetcher.FetchError, ValueError)


@dataclass
class Request:
    kind: str          # fund | security
    provider: str
    ref: str
    as_of: date | None = None   # None: the current composition
    claim: object = None
    # The providers standing by, asked in order when this one cannot
    # serve the current composition. Each claims `fund` only then. `fund`
    # carries the identifiers this one's claim adds (described).
    fallbacks: tuple = ()
    fund: object = None


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
                   help="Ask only for this fund reference or security key (repeatable).")
    p.add_argument("--limit", type=int, default=None,
                   help="Make at most N fetch attempts this run; the lookups that find a fund are not counted.")
    p.add_argument("--no-securities", action="store_true", help="Skip the security lookups.")
    p.add_argument("--check", action="store_true",
                   help="Ask every enabled provider for its cheapest answer and exit; writes nothing.")
    p.add_argument("--dry-run", action="store_true", help="Plan the requests, print them, fetch nothing.")
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


def described(fund: wantedmod.Fund, claim: dict) -> wantedmod.Fund:
    """The fund with its empty identifiers filled from the claim's `ids`.
    A provider standing by can then claim it by the one it reads: N-PORT
    finds a fund named by its ISIN by its ticker."""
    ids = claim.get("ids") or {}
    ids = {k: v for k in ("isin", "cusip", "symbol") if (v := ids.get(k)) and not getattr(fund, k)}
    return replace(fund, **ids) if ids else fund


def plan(want: wantedmod.Wanted, providers, ctx: Context, *, today: date, max_age: int, backfill: bool,
         refs=None, securities=True, constituents=False, floor_pct=0, fetched=None, looked_up=None):
    """The requests a run makes, and the funds that want a composition no
    provider claims.

    The inbox (an additive provider) serves every fund it has a folder
    for. A fund held today, or wanted in one of CURRENT_ROLES, wants its
    current composition once the newest one had is older than max_age. A
    backfill wants every date inside the days the fund was held, and the
    last one before them within max_gap_days: the composition in force
    when the holding began. Only a fund that wants either is offered to
    the other providers: the first that claims it serves it, and those
    after it stand by for its current composition. The current one is
    not asked for where the provider's newest is already had, or where
    the provider served one within max_age, as a report that states a day
    long past."""
    reqs, unclaimed = [], []
    served_days, fetched_days = {}, {}
    for (q, r, d), day in (fetched or {}).items():
        served_days[(q, r)] = max(served_days.get((q, r), day), day)
        fetched_days.setdefault(r, set()).add(d)
    gap = timedelta(days=want.max_gap_days)
    additive = [p for p in providers if p.serves_funds and p.additive]
    others = [p for p in providers if p.serves_funds and not p.additive]
    for fund in want.funds:
        if refs and fund.ref not in refs:
            continue
        have = set(fund.compositions) | fetched_days.get(fund.ref, set())
        until = fund.held_to or today
        since = (fund.held_from or today) - gap
        claimed = False
        for p in additive:
            claim = _ask(fund.ref, p.name, "claim it", p.claim, fund, ctx)
            if claim is None:
                continue
            claimed = True
            for d in _ask(fund.ref, p.name, "list its dates", p.dates, claim, since, until, ctx) or []:
                if d not in have:
                    reqs.append(Request("fund", p.name, fund.ref, d, claim))
                    have.add(d)
        newest = max(have, default=None)
        current = (fund.held_to is None or not CURRENT_ROLES.isdisjoint(fund.roles)) \
            and (newest is None or (today - newest).days > max_age)
        past = backfill and fund.held_from is not None
        if not (current or past):
            continue
        for i, p in enumerate(others):
            claim = _ask(fund.ref, p.name, "claim it", p.claim, fund, ctx)
            if claim is None:
                continue
            claimed = True
            served_on = served_days.get((p.name, fund.ref))
            if current and (served_on is None or (today - served_on).days > max_age):
                latest = _ask(fund.ref, p.name, "name its newest date", p.latest, claim, ctx)
                if latest is None or latest not in have:
                    reqs.append(Request("fund", p.name, fund.ref, None, claim,
                                        tuple(q.name for q in others[i + 1:]), described(fund, claim)))
                    if latest is not None:
                        have.add(latest)
            if past:
                days = _ask(fund.ref, p.name, "list its dates", p.dates, claim, since, until, ctx) or []
                before = [d for d in days if d < fund.held_from]
                for d in [*before[-1:], *(d for d in days if d >= fund.held_from)]:
                    if any(abs((d - h).days) <= NEAR_DAYS for h in have):
                        continue
                    reqs.append(Request("fund", p.name, fund.ref, d, claim))
                    have.add(d)
            break
        if not claimed:
            unclaimed.append(fund.ref)
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
                claim = _ask(sec.key, p.name, "claim it", p.claim_security, sec, ctx)
                if claim is not None:
                    reqs.append(Request("security", p.name, sec.key, None, claim))
    return reqs, unclaimed


def _folder(ref: str) -> str:
    """A fund reference or security key as a folder name: `isin_XX…`, as
    the inbox writes it. A reference with other characters a folder name
    cannot hold gets a short hash of itself, so XYZ-B and XYZ/B never
    share a folder."""
    name = ref.replace(":", "_", 1)
    stem = bronze.safe_stem(name)
    return stem if stem == name else f"{stem}-{bronze.short_token(ref, 8)}"


def request_dir(run: Path, r: Request) -> Path:
    if r.kind == "fund":
        return run / "funds" / r.provider / _folder(r.ref) / (r.as_of.isoformat() if r.as_of else "current")
    return run / "securities" / r.provider / _folder(r.ref)


def execute(reqs, run: Path, ctx: Context, limit=None):
    """Fetch every request into its bronze folder; one entry per request.
    A provider standing by claims the fund only once the ones before it
    failed. `limit` caps the fetch attempts, a standby's included; a
    request it leaves no attempt for is deferred."""
    results, attempts = [], 0
    for r in reqs:
        entry = {"kind": r.kind, "provider": r.provider, "ref": r.ref,
                 "asked": r.as_of.isoformat() if r.as_of else "current"}
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
        cfg = config.load(REGISTRY)
    except config.ConfigError as e:
        log.error("%s", e)
        return 2
    providers = enabled_providers(cfg, args.provider)
    ctx = Context(client=fetcher.Client(cfg.contact), data_dir=args.bronze_dir, config=cfg)
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
    fetched_funds, looked_up = fetched_before(args.bronze_dir)
    reqs, unclaimed = plan(want, providers, ctx, today=today_utc(), max_age=cfg.max_age_days,
                           backfill=args.backfill, refs=set(args.ref or ()), securities=not args.no_securities,
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
    manifest = {"slug": slug, "status": "in-progress", "wanted_as_of": want.as_of.isoformat(),
                "providers": [p.name for p in providers], "unclaimed": unclaimed, "requests": []}
    bronze.atomic_write_json(run / "run.json", manifest)
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
