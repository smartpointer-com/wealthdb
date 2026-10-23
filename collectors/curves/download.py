#!/usr/bin/env python3
"""
curves downloader: the euro and franc government yield curves.

Pulls the public government curve of two currencies, each from a
built-in map of series, one series per tenor:

- EUR: the ECB's euro area yield curve (dataflow YC) of triple-A rated
  central governments, from the ECB Data Portal. Spot rates, 3M to 30Y,
  business-daily, continuously compounded.
- CHF: the SNB's spot rates of Swiss Confederation bonds, from the SNB
  data portal's warehouse. 1Y to 30Y by half year, business-daily,
  annually compounded.

The ECB also publishes par yields, and both portals publish the fitted
model's parameters. The spot rates are the zero curve itself, so the
engine converts each one exactly to the engine's semiannual compounding
and bootstraps nothing.

A run asks each portal once, for all of its series over the window, and
keeps the raw CSV answer in a UTC-timestamped bronze run directory,
beside a run.json manifest that records what each series is. load.py
projects the answers into the silver `rates` table.

The SNB asks API users to check its latest-update endpoint rather than
poll the data. A run asks it first, and skips the SNB's data when an
earlier complete run already holds that publication for this run's
window and series.

Backfill: the shared --lookback flag bounds the observation window,
from the starting point it names to today. The default is ~90 days and
`all` reaches back 30 years. An early date such as
`--lookback 1950-01-01` fetches each series' whole history, since each
portal answers from its own start.

Usage:
    download.py --bronze-dir <dir> [--series ID|CCY,...]
                [--lookback PRESET|YYYY-MM-DD] [--dry-run] [--debug]
    download.py --check
"""

from __future__ import annotations

import argparse
import json
import logging
import sys
import urllib.error
import urllib.parse
from datetime import date
from pathlib import Path

from collectorkit import bronze, cli, debugcap

# The loader owns the manifest contract and the parsers of both answers.
# The downloader writes each entry from the same field names and counts
# with the same parsers, so the manifest and silver's rows cannot drift.
from load import PARSERS, RATE_MEANING, SNB_CUBE, SNB_FIXED, parse_ecb

log = logging.getLogger("curves.download")

ECB_URL = "https://data-api.ecb.europa.eu/service/data"
SNB_URL = "https://data.snb.ch/api"
USER_AGENT = "wealthdb-curves/1"

# The ECB's euro area yield curve: dataflow YC, business-daily (B), the
# euro area (U2), in euros, by the ECB (4F), from the bonds of the
# triple-A rated central governments (G_N_A), fitted by the Svensson
# model with continuous compounding to yields (SV_C_YM). A series adds
# its data type and maturity: SR_10Y is the ten-year spot rate. Its id is
# the whole key, as the portal's KEY column states it.
ECB_FLOW = "YC"
ECB_KEY = "B.U2.EUR.4F.G_N_A.SV_C_YM"
ECB_TENORS = ("3M", "6M", "9M", *(f"{y}Y" for y in range(1, 31)))


def _snb_series() -> dict[str, str]:
    """The SNB cube's maturity items, 1 to 30 years by half year, each
    with its tenor in the config's vocabulary: J10M0 is 10Y, J01M6 18M."""
    out = {}
    for months in range(12, 361, 6):
        years, rest = divmod(months, 12)
        tenor = f"{years}Y" if rest == 0 else f"{months}M"
        out[f"J{years:02d}M{rest}"] = tenor
    return out


# Series id -> (portal, currency, tenor, basis, compounding). The value is
# in percent. spot: a zero rate, the yield of one payment at the tenor.
# Both portals fit a Nelson-Siegel-Svensson model, which compounds
# continuously. The ECB states the model's rates as they are; the SNB
# states each as the annually compounded rate of the same discount
# factor (README.md, Basis). The manifest records the meaning per series
# and load copies it into each silver row's payload.
#
# A run asks each portal under one key: every ECB id is ECB_FLOW.ECB_KEY
# and one item, every SNB id SNB_CUBE and one item (ecb_url, snb_url). A
# series of another ECB key or SNB cube needs its own request first.
RATE_SERIES: dict[str, tuple[str, str, str, str, str]] = {
    **{f"{ECB_FLOW}.{ECB_KEY}.SR_{tenor}": ("ecb", "EUR", tenor, "spot",
                                            "continuous")
       for tenor in ECB_TENORS},
    **{f"{SNB_CUBE}.{item}": ("snb", "CHF", tenor, "spot", "annual")
       for item, tenor in _snb_series().items()},
}

# Each series' run.json entry, less its row count. load reads it back to
# place and describe the rows.
SERIES: dict[str, dict[str, str]] = {
    sid: {"portal": portal, **dict(zip(RATE_MEANING, meaning, strict=True))}
    for sid, (portal, *meaning) in RATE_SERIES.items()
}

# A portal's answer is kept in the run dir under this name.
ANSWER_FILE = "{portal}.csv"


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument(
        "--bronze-dir", type=Path, default=None,
        help="One run dir is created here per invocation.",
    )
    p.add_argument(
        "--check", action="store_true",
        help=("Ask each portal for its cheapest answer and exit: the ECB "
              "for one series' newest observation, the SNB for its latest "
              "update (it asks clients not to poll its data). Exit 0 when "
              "both answer. Writes nothing. This is what `curves login "
              "--check` runs: no portal has a session or a key."),
    )
    p.add_argument(
        "--series", default=None,
        help="Comma-separated subset to fetch: series ids of RATE_SERIES, "
             "or a currency (EUR, CHF) for all of its series. Default: "
             "every series.",
    )
    cli.add_standard_args(p, verb="download")
    p.add_argument(
        "--dry-run", action="store_true",
        help="Fetch and report row counts but write no bronze.",
    )
    p.add_argument(
        "--debug", action="store_true",
        help="Capture debug artefacts into the bronze run dir (default "
             "off): an HTTP trace at <run>/screenshots/http-trace.jsonl, "
             "one line per request with its status, timing, size and "
             "cache headers. `prune` reclaims the trace; `load` never "
             "reads it.",
    )
    return p.parse_args(argv)


# Stands in for callers with no run dir to write into: the `--check`
# probe, which writes nothing. A disabled trace swallows every record().
_NO_TRACE = debugcap.HttpTrace(None, log=log, enabled=False)


def fetch(url: str, timeout: int = 90, *,
          trace: debugcap.HttpTrace = _NO_TRACE) -> bytes:
    """GET one URL of either portal, the ECB's or the SNB's, and return
    its body, through debugcap.traced_get."""
    return debugcap.traced_get(url, user_agent=USER_AGENT, timeout=timeout,
                               trace=trace)


def _item(sid: str) -> str:
    """A series id's last part: its ECB data type and maturity (SR_10Y),
    or its SNB maturity item (J10M0)."""
    return sid.rsplit(".", 1)[1]


def ecb_url(sids: list[str], **params: str) -> str:
    """The ECB's data URL for the series sids, one key with their items
    joined by `+`, answered as CSV with the data alone."""
    keys = "+".join(_item(sid) for sid in sids)
    query = urllib.parse.urlencode(
        {**params, "format": "csvdata", "detail": "dataonly"})
    return f"{ECB_URL}/{ECB_FLOW}/{ECB_KEY}.{keys}?{query}"


def snb_url(sids: list[str], since: date, until: date) -> str:
    """The SNB's data URL for the series sids over the window, each fixed
    dimension at its one item."""
    dims = [f"LAUFZEIT({','.join(_item(sid) for sid in sids)})",
            *(f"{k}({v})" for k, v in SNB_FIXED.items())]
    query = urllib.parse.urlencode({"fromDate": since.isoformat(),
                                    "toDate": until.isoformat(),
                                    "dimSel": ",".join(dims)})
    return f"{SNB_URL}/warehouse/cube/{SNB_CUBE}/data/csv/en?{query}"


SNB_LAST_UPDATE = f"{SNB_URL}/warehouse/cube/{SNB_CUBE}/lastUpdate"


def snb_published(trace: debugcap.HttpTrace) -> str:
    """When the SNB last published the cube, as its latest-update endpoint
    states it (publicSinceDate, e.g. 20260105_0830)."""
    doc = json.loads(fetch(SNB_LAST_UPDATE, timeout=30, trace=trace))
    published = doc.get("publicSinceDate") if isinstance(doc, dict) else None
    if not isinstance(published, str) or not published:
        raise ValueError(f"no publicSinceDate in the SNB's latest update: "
                         f"{str(doc)[:200]}")
    return published


def snb_held(bronze_dir: Path, published: str, since: date,
             sids: list[str]) -> str | None:
    """The newest complete run that holds the SNB answer this run would
    fetch: one asked of the same publication, over a window from `since`
    or earlier, for every series of sids, whose answer held a row (an
    answer the parser read). None when no run does."""
    want = set(sids)
    for run in reversed(list(bronze.iter_run_dirs(bronze_dir))):
        m = bronze.read_manifest(run / "run.json") or {}
        snb = (m.get("portals") or {}).get("snb") or {}
        held = {sid: e.get("rows") for sid, e in (m.get("series") or {}).items()
                if isinstance(e, dict) and e.get("portal") == "snb"}
        if (m.get("status") == bronze.COMPLETE_STATUS and snb.get("file")
                and snb.get("published") == published
                and isinstance(m.get("since"), str)
                and m["since"] <= since.isoformat() and want <= held.keys()
                and any(held.values())):
            return run.name
    return None


def ask_ecb(sids: list[str], since: date, until: date, *, bronze_dir: Path,
            trace: debugcap.HttpTrace) -> tuple[dict, bytes]:
    """Ask the ECB for the series sids over the window. Returns the
    manifest's entry for the portal and its raw answer. bronze_dir is
    unused: the ECB states no publication to compare, and a run asks it
    every time."""
    url = ecb_url(sids, startPeriod=since.isoformat(),
                  endPeriod=until.isoformat())
    try:
        body = fetch(url, trace=trace)
    except urllib.error.HTTPError as e:
        # SDMX answers a window with no observation with 404; the portal
        # also answers it with an empty 200.
        if e.code != 404:
            raise
        body = b""
    return {"url": url, "file": ANSWER_FILE.format(portal="ecb")}, body


def ask_snb(sids: list[str], since: date, until: date, *, bronze_dir: Path,
            trace: debugcap.HttpTrace) -> tuple[dict, bytes | None]:
    """Ask the SNB for the series sids over the window, once its latest
    update says a run in bronze_dir does not hold them already. Returns
    the manifest's entry for the portal and its raw answer, or None for
    an answer unchanged since a run that holds it."""
    published = snb_published(trace)
    held = snb_held(bronze_dir, published, since, sids)
    if held is not None:
        return {"published": published, "unchanged_since": held}, None
    url = snb_url(sids, since, until)
    return {"url": url, "file": ANSWER_FILE.format(portal="snb"),
            "published": published}, fetch(url, trace=trace)


# How a run asks each portal.
ASK = {"ecb": ask_ecb, "snb": ask_snb}


def _probe_ecb() -> str:
    """The newest observation of the ECB's ten-year rate, for the log."""
    answer = parse_ecb(fetch(ecb_url([f"{ECB_FLOW}.{ECB_KEY}.SR_10Y"],
                                     lastNObservations="1"),
                             timeout=30).decode("utf-8"))
    if not answer:
        raise ValueError("answered no observation")
    sid, obs = next(iter(answer.items()))
    return "{} on {} is {}".format(sid, *obs[-1])


def _probe_snb() -> str:
    """The SNB's latest update of the cube, for the log. The SNB asks
    clients to check it rather than poll the data, and it names the cube
    the data requests ask for."""
    return f"{SNB_CUBE} published {snb_published(_NO_TRACE)}"


# Each portal's cheapest answer, which `--check` asks for.
PROBES = {"ecb": _probe_ecb, "snb": _probe_snb}


def check() -> int:
    """Ask each portal for its cheapest answer (PROBES) and report it.
    0 when both answer. Writes nothing."""
    ok = True
    for portal, probe in PROBES.items():
        try:
            log.info("%s: %s", portal, probe())
        except urllib.error.HTTPError as e:
            log.error("%s: probe failed: HTTP %d %s", portal, e.code, e.reason)
            ok = False
        except (OSError, ValueError, KeyError) as e:
            log.error("%s: probe failed: %s", portal, e)
            ok = False
    return 0 if ok else 1


def _describe(sid: str) -> str:
    """One series' meaning for the log: "EUR 10Y spot continuous"."""
    return " ".join(RATE_SERIES[sid][1:])


def select(spec: str | None) -> list[str]:
    """The series a --series value names, in RATE_SERIES order: each a
    series id, or a currency for every series of it. Every series when
    none is given."""
    if not spec:
        return list(RATE_SERIES)
    tokens = [s.strip() for s in spec.split(",") if s.strip()]
    currencies = {meaning[1] for meaning in RATE_SERIES.values()}
    unknown = [s for s in tokens if s not in RATE_SERIES and s not in currencies]
    if unknown:
        raise SystemExit(
            f"unknown series {unknown}: name a series id of RATE_SERIES or "
            f"one of the currencies {sorted(currencies)}; add a series to "
            f"RATE_SERIES (series_id -> portal,currency,tenor,basis,"
            f"compounding) in download.py first.")
    picked = set(tokens)
    return [sid for sid, meaning in RATE_SERIES.items()
            if sid in picked or meaning[1] in picked]


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)
    if args.check:
        return check()
    if args.bronze_dir is None:
        raise SystemExit("--bronze-dir is required (or pass --check to ask "
                         "each portal without writing).")
    since, until = cli.resolve_lookback(args)
    series_ids = select(args.series)
    by_portal: dict[str, list[str]] = {}
    for sid in series_ids:
        by_portal.setdefault(RATE_SERIES[sid][0], []).append(sid)

    log.info("curves download: %s … %s, %d series", since, until,
             len(series_ids))

    slug = bronze.ts_slug()
    run = None if args.dry_run else bronze.run_dir(args.bronze_dir, slug)
    manifest = {
        "slug": slug,
        "since": since.isoformat(),
        "until": until.isoformat(),
        "portals": {},
        "series": {},
    }

    # Drop an "in-progress" manifest up front; the write makes the run dir
    # (a dry run makes none). A walk that crashes mid-fetch thus leaves
    # status="in-progress": load skips such a dump,
    # and prune reclaims it once quiescent. The terminal write below
    # atomically overwrites this marker with status="complete".
    if run is not None:
        manifest["status"] = "in-progress"
        bronze.atomic_write_json(run / "run.json", manifest)

    # One trace for the whole run. A dry run creates no run dir, so
    # HttpTrace(None) makes the capture an explicit no-op.
    trace = debugcap.HttpTrace(run, log=log, enabled=args.debug)

    total = failures = unchanged = 0
    for portal, sids in by_portal.items():
        try:
            entry, body = ASK[portal](sids, since, until,
                                      bronze_dir=args.bronze_dir, trace=trace)
            answer = None if body is None else PARSERS[portal](
                body.decode("utf-8"))
        except urllib.error.HTTPError as e:
            body = (e.read() or b"")[:200].decode("utf-8", "replace")
            log.error("%s: HTTP %s %s — skipping", portal, e.code, body)
            failures += 1
            continue
        except Exception as e:  # noqa: BLE001 - one bad portal must not abort
            log.error("%s: fetch or parse failed: %s — skipping", portal, e)
            failures += 1
            continue
        manifest["portals"][portal] = entry
        if answer is None:
            log.info("%s: publication %s unchanged since run %s; not asked "
                     "again", portal, entry["published"],
                     entry["unchanged_since"])
            unchanged += 1
            continue
        for sid in sids:
            n = len(answer.get(sid, []))
            total += n
            manifest["series"][sid] = {**SERIES[sid], "rows": n}
            log.info("series %s (%s): %d dated observations", sid,
                     _describe(sid), n)
        if run is not None:
            bronze.atomic_write_bytes(run / entry["file"], body)

    # A run that fetched nothing and found nothing unchanged is a failure
    # worth a non-zero exit, a dry run included.
    rc = 0 if manifest["series"] or unchanged else 5
    if run is None:
        log.info("dry run: %d observations across %d series, %d failures "
                 "(nothing written)", total, len(manifest["series"]), failures)
        return rc

    manifest["status"] = bronze.COMPLETE_STATUS
    bronze.atomic_write_json(run / "run.json", manifest)
    log.info("wrote %d series (%d observations, %d failures) to %s",
             len(manifest["series"]), total, failures, run)
    return rc


if __name__ == "__main__":
    sys.exit(main())
