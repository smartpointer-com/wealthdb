#!/usr/bin/env python3
"""
FRED FX-rate downloader.

Pulls daily USD-centric foreign-exchange reference rates from the US
Federal Reserve H.10 release via the FRED API — one series per currency
pair (see FX_SERIES) — and writes each series' raw JSON into a
UTC-timestamped bronze run directory plus a run.json manifest. load.py
projects those into the silver `fx_rates` table.

API version: this uses FRED's classic ("v1") endpoint
`https://api.stlouisfed.org/fred/series/observations`, authenticated with
the api_key query parameter. The newer v2 API uses a different
(Authorization: Bearer) scheme and offers no advantage for this
read-only time-series fetch, so v1 is the default. Point --base-url
elsewhere (e.g. a proxy or a future endpoint) if needed.

Backfill: the shared --lookback flag bounds the observation window,
running from the starting point it names to today (default ~90 days;
`--lookback all` or `--lookback 1971-01-01` for a full historic
backfill — FRED clamps to each series' own start).

Usage:
    download.py --bronze-dir <dir> [--api-key KEY]
                [--lookback PRESET|YYYY-MM-DD] [--dry-run]
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from datetime import date, timedelta
from pathlib import Path

from collectorkit import bronze, cli, envfile

# Single source of truth for FRED's no-data / market-holiday sentinels — the
# loader owns the constant (it filters rows on it); the downloader imports it
# so the dated-row count and the silver row filter can never drift apart.
from load import NO_DATA

log = logging.getLogger("fred.download")

DEFAULT_BASE_URL = "https://api.stlouisfed.org/fred/series/observations"

# FRED H.10 series -> (base_iso, quote_iso) in the canonical FX
# convention, where (base, quote, value) means "1 quote = value base"
# (value = base per quote). This is the direction the gold engine and the
# ubs-psn silver use, so a gold adapter maps straight across. A FRED H.10
# value IS "base per quote" for these series — e.g. DEXSZUS = Swiss Francs
# per USD = "1 USD = v CHF", hence base=CHF, quote=USD. Verified live
# against the API and cross-checked against the ubs-psn silver
# (CHF/USD @ ~0.797). The README lists FRED IDs for more currencies; add
# them here with the correct base/quote.
FX_SERIES: dict[str, tuple[str, str]] = {
    "DEXSZUS": ("CHF", "USD"),  # CHF per USD  -> 1 USD = v CHF
    "DEXUSEU": ("USD", "EUR"),  # USD per EUR  -> 1 EUR = v USD
    "DEXJPUS": ("JPY", "USD"),  # JPY per USD
    "DEXUSUK": ("USD", "GBP"),  # USD per GBP
    "DEXCAUS": ("CAD", "USD"),  # CAD per USD
    "DEXUSAL": ("USD", "AUD"),  # USD per AUD
    "DEXCHUS": ("CNY", "USD"),  # CNY per USD
    "DEXHKUS": ("HKD", "USD"),  # HKD per USD
}


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument(
        "--bronze-dir", type=Path, default=None,
        help="One run dir is created here per invocation.",
    )
    p.add_argument(
        "--check", action="store_true",
        help=("Probe the configured credential and exit: resolve the API "
              "key, then ask FRED for a single observation. Exit 0 when the "
              "key is accepted, non-zero when it is missing or rejected. "
              "Writes nothing. This is what `fred login --check` runs — "
              "FRED has no session to mint, so the key IS the session."),
    )
    p.add_argument(
        "--env-file", type=Path, default=None,
        help="KEY=VALUE credentials env file, sourced before the API key is "
             "resolved (also honours the FRED_ENV_FILE env var). The wrapper "
             "already sources <secrets-dir>/fred.env, so this is for running "
             "download.py directly.",
    )
    p.add_argument(
        "--api-key", default=None,
        help="FRED API key. Falls back to the FRED_API_KEY environment "
             "variable (sourced from <secrets>/fred.env by the wrapper).",
    )
    p.add_argument(
        "--base-url", default=DEFAULT_BASE_URL,
        help=f"FRED observations endpoint (default: {DEFAULT_BASE_URL}).",
    )
    p.add_argument(
        "--series", default=None,
        help="Comma-separated subset of the built-in series IDs to fetch "
             "(default: all of them). IDs must be present in FX_SERIES.",
    )
    cli.add_standard_args(p, verb="download")
    p.add_argument(
        "--dry-run", action="store_true",
        help="Fetch and report row counts but write no bronze.",
    )
    p.add_argument(
        "--debug", action="store_true",
        help="Uniform debug-capture gate (default off). fred is a pure "
             "REST/JSON collector and writes no bronze-resident debug "
             "artefacts, so this currently gates nothing extra; it exists "
             "so every collector's `download` shares the flag. See "
             "collectors/README.md.",
    )
    return p.parse_args(argv)


def fetch_observations(base_url: str, series_id: str, api_key: str,
                       start, end, timeout: int = 90) -> dict:
    """GET the raw FRED observations document for one series + window.
    Raises on transport/HTTP errors (the caller logs + skips the series)."""
    params = {
        "series_id": series_id,
        "api_key": api_key,
        "file_type": "json",
        "observation_start": start.isoformat(),
        "observation_end": end.isoformat(),
    }
    url = f"{base_url}?{urllib.parse.urlencode(params)}"
    req = urllib.request.Request(url, headers={"User-Agent": "wealthdb-fred/1"})
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode("utf-8"))


def _source_env_file(explicit: Path | None) -> None:
    """Source an explicit --env-file (or $FRED_ENV_FILE) before credentials
    are resolved, so a direct `download.py` run needs no wrapper. The wrapper
    already sources <secrets-dir>/fred.env; this one is sourced last, so its
    values win. A path that was asked for but is absent is an error, not a
    silent fallback to whatever the environment happened to hold."""
    path = explicit or (Path(os.environ["FRED_ENV_FILE"])
                        if os.environ.get("FRED_ENV_FILE") else None)
    if path is None:
        return
    if not path.is_file():
        raise SystemExit(f"--env-file does not exist: {path}")
    envfile.load_env_file(path, ("FRED_API_KEY",), logger=log)


def _check_credential(base_url: str, api_key: str) -> int:
    """Ask FRED for one observation to prove the key is accepted.

    The fleet's `login --check` contract is "probe the stored session without
    minting a new one". FRED has no session — the API key is the credential —
    so the equivalent is the cheapest possible authenticated read. A key that
    is merely present but rejected is the failure this catches; checking the
    env var alone would not.
    """
    probe_series = next(iter(FX_SERIES))
    today = date.today()
    try:
        doc = fetch_observations(base_url, probe_series, api_key,
                                 today - timedelta(days=7), today, timeout=30)
    except urllib.error.HTTPError as e:
        # FRED answers a bad key with 400 + an error_message body.
        if e.code in (400, 403):
            log.error("credential rejected by FRED (HTTP %d) — check "
                      "FRED_API_KEY in the env file", e.code)
            return 1
        log.error("probe failed: HTTP %d %s", e.code, e.reason)
        return 1
    except (urllib.error.URLError, TimeoutError, OSError) as e:
        log.error("probe could not reach FRED: %s", e)
        return 1
    if "observations" not in doc:
        log.error("unexpected FRED response (no observations): %s",
                  sorted(doc)[:5])
        return 1
    log.info("credential accepted — FRED answered the %s probe", probe_series)
    return 0


def _dated_count(doc: dict) -> int:
    """Number of observations with a real value (a NO_DATA member is FRED's
    no-data / holiday sentinel)."""
    return sum(1 for o in doc.get("observations", [])
               if o.get("value") not in NO_DATA)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)

    if args.debug:
        # TODO(second pass): write bronze-resident debug captures under --debug.
        cli.warn_debug_noop("fred", log)
    _source_env_file(args.env_file)
    api_key = envfile.resolve_credential(args.api_key, "FRED_API_KEY",
                                         "--api-key")
    if args.check:
        return _check_credential(args.base_url, api_key)
    if args.bronze_dir is None:
        raise SystemExit("--bronze-dir is required (or pass --check to probe "
                         "the credential without writing).")
    since, until = cli.resolve_lookback(args)

    if args.series:
        series_ids = [s.strip() for s in args.series.split(",") if s.strip()]
        unknown = [s for s in series_ids if s not in FX_SERIES]
        if unknown:
            raise SystemExit(
                f"unknown series {unknown}: add them to FX_SERIES "
                f"(series_id -> base,quote) in download.py first."
            )
    else:
        series_ids = list(FX_SERIES)

    log.info("FRED FX download: %s … %s, %d series", since, until,
             len(series_ids))

    args.bronze_dir.mkdir(parents=True, exist_ok=True)
    slug = bronze.ts_slug()
    run = None if args.dry_run else bronze.run_dir(args.bronze_dir, slug)
    manifest = {
        "slug": slug,
        "since": since.isoformat(),
        "until": until.isoformat(),
        "base_url": args.base_url,
        "series": {},
    }

    # Drop an "in-progress" manifest up front — this lazily creates the run
    # dir and makes the run's state legible before the fetch loop starts.
    # A walk that crashes mid-fetch thus leaves status="in-progress": load
    # skips such a dump rather than ingesting its partial series, and prune
    # reclaims it as a non-complete dump once quiescent. The terminal write
    # below atomically overwrites this marker with status="complete".
    if run is not None:
        manifest["status"] = "in-progress"
        bronze.atomic_write_json(run / "run.json", manifest)

    total = failures = 0
    for sid in series_ids:
        base, quote = FX_SERIES[sid]
        try:
            doc = fetch_observations(args.base_url, sid, api_key, since, until)
        except urllib.error.HTTPError as e:
            body = (e.read() or b"")[:200].decode("utf-8", "replace")
            log.error("series %s: HTTP %s %s — skipping", sid, e.code, body)
            failures += 1
            continue
        except Exception as e:  # noqa: BLE001 - one bad series must not abort
            log.error("series %s fetch failed: %s — skipping", sid, e)
            failures += 1
            continue
        n = _dated_count(doc)
        total += n
        manifest["series"][sid] = {"base": base, "quote": quote, "rows": n}
        log.info("series %s (%s→%s): %d dated rates", sid, base, quote, n)
        if not args.dry_run:
            bronze.atomic_write_json(run / f"{sid}.json", doc)

    if args.dry_run:
        log.info("dry run: %d rates across %d series, %d failures "
                 "(nothing written)", total, len(manifest["series"]), failures)
        return 0

    manifest["status"] = "complete"
    bronze.atomic_write_json(run / "run.json", manifest)
    log.info("wrote %d series (%d rates, %d failures) to %s",
             len(manifest["series"]), total, failures, run)
    # A run with zero usable series is a failure worth a non-zero exit.
    return 0 if manifest["series"] else 5


if __name__ == "__main__":
    sys.exit(main())
