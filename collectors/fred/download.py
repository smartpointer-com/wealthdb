#!/usr/bin/env python3
"""
FRED downloader: FX reference rates and US Treasury yields.

Pulls two families of daily series from the FRED API, each from a
built-in map. FX_SERIES holds USD-centric foreign-exchange reference
rates from the US Federal Reserve H.10 release, one series per currency
pair. RATE_SERIES holds the US Treasury yield curve from the H.15
release: the constant-maturity yields from one month to thirty years
plus the three-month bill rate, in percent.

Each series' raw JSON goes into a UTC-timestamped bronze run directory,
beside a run.json manifest that records what each series is.
load.py projects the FX series into the silver `fx_rates` table and the
rates series into `rates`.

API version: this uses FRED's classic ("v1") endpoint
`https://api.stlouisfed.org/fred/series/observations`, authenticated with
the api_key query parameter. The newer v2 API uses a different
(Authorization: Bearer) scheme and offers no advantage for this
read-only time-series fetch, so v1 is the default. Point --base-url
elsewhere (e.g. a proxy or a future endpoint) if needed.

Backfill: the shared --lookback flag bounds the observation window,
from the starting point it names to today. The default is ~90 days and
`all` reaches back 30 years. An early date such as
`--lookback 1950-01-01` fetches each series' whole history, since FRED
clamps the window to each series' own start.

Usage:
    download.py --bronze-dir <dir> [--api-key KEY] [--series ID,...]
                [--lookback PRESET|YYYY-MM-DD] [--dry-run]
"""

from __future__ import annotations

import argparse
import json
import logging
import sys
import urllib.error
import urllib.parse
from datetime import date, timedelta
from pathlib import Path

from collectorkit import bronze, cli, debugcap, envfile

# The loader owns the manifest contract and the dated-observation filter.
# The downloader writes each entry from the same field names and counts
# with the same function, so the manifest and silver's rows cannot drift.
from load import FX_MEANING, RATE_MEANING, dated_observations

log = logging.getLogger("fred.download")

DEFAULT_BASE_URL = "https://api.stlouisfed.org/fred/series/observations"

# FRED H.10 series -> (base_iso, quote_iso) in the canonical FX
# convention, where (base, quote, value) means "1 quote = value base"
# (value = base per quote). This is the direction the gold engine and the
# ubs-psn silver use, so a gold adapter maps straight across. A FRED H.10
# value IS "base per quote" for these series — e.g. DEXSZUS = Swiss Francs
# per USD = "1 USD = v CHF", hence base=CHF, quote=USD. The README lists
# FRED IDs for more currencies; add them here with the correct base/quote.
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

# FRED H.15 series -> (currency, tenor, basis). The value is in percent.
# constant_maturity: a Treasury par yield at a fixed maturity,
# bond-equivalent, semiannual. discount: a bill's secondary-market rate
# on a discount basis. The manifest records the triple per series and
# load copies it into the silver row's payload.
RATE_SERIES: dict[str, tuple[str, str, str]] = {
    "DGS1MO": ("USD", "1M", "constant_maturity"),
    "DGS3MO": ("USD", "3M", "constant_maturity"),
    "DGS6MO": ("USD", "6M", "constant_maturity"),
    "DGS1":   ("USD", "1Y", "constant_maturity"),
    "DGS2":   ("USD", "2Y", "constant_maturity"),
    "DGS3":   ("USD", "3Y", "constant_maturity"),
    "DGS5":   ("USD", "5Y", "constant_maturity"),
    "DGS7":   ("USD", "7Y", "constant_maturity"),
    "DGS10":  ("USD", "10Y", "constant_maturity"),
    "DGS20":  ("USD", "20Y", "constant_maturity"),
    "DGS30":  ("USD", "30Y", "constant_maturity"),
    "DTB3":   ("USD", "3M", "discount"),
}

# Each series' run.json entry, less its row count. load reads it back to
# place and describe the rows. A rates entry names its family. An FX entry
# names none, the shape every FX entry in bronze has.
SERIES: dict[str, dict[str, str]] = {
    **{sid: dict(zip(FX_MEANING, pair, strict=True))
       for sid, pair in FX_SERIES.items()},
    **{sid: {"family": "rates",
             **dict(zip(RATE_MEANING, meaning, strict=True))}
       for sid, meaning in RATE_SERIES.items()},
}


def _endpoint(value: str) -> str:
    """argparse ``type`` for --base-url: an http(s) URL with a host.

    Some malformed values make urllib raise an error that quotes the whole
    request URL, api_key included. Every malformed value is refused here,
    before any request, and the message never echoes it."""
    try:
        parts = urllib.parse.urlsplit(value)
        _ = parts.port  # raises ValueError on a malformed port
    except ValueError:  # also an unbalanced IPv6 bracket
        parts = None
    if (parts is None or parts.scheme not in ("http", "https")
            or not parts.hostname
            or any(c <= " " or c == "\x7f" for c in value)):
        raise argparse.ArgumentTypeError("not an http(s) URL with a host")
    return value


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
        help="Credentials env file, sourced as bash before the API key is "
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
        "--base-url", type=_endpoint, default=DEFAULT_BASE_URL,
        help="FRED observations endpoint, an http(s) URL "
             f"(default: {DEFAULT_BASE_URL}).",
    )
    p.add_argument(
        "--series", default=None,
        help="Comma-separated subset of the built-in series IDs to fetch "
             "(default: every series of both families). Each ID must be "
             "in FX_SERIES or RATE_SERIES; one list may mix the two.",
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
             "one line per FRED request with its status, timing, size and "
             "rate-limit headers — the shape of the exchange, which the "
             "observations documents beside it do not record. The api_key "
             "query parameter is redacted. `prune` reclaims the trace; "
             "`load` never reads it.",
    )
    return p.parse_args(argv)


# Stands in for callers with no run dir to write into — the `--check` probe,
# which writes nothing at all by contract. A disabled trace swallows every
# record(), so the fetch path needs no None-guard.
_NO_TRACE = debugcap.HttpTrace(None, log=log, enabled=False)


def fetch_observations(base_url: str, series_id: str, api_key: str,
                       start, end, timeout: int = 90, *,
                       trace: debugcap.HttpTrace = _NO_TRACE) -> dict:
    """GET the raw FRED observations document for one series + window,
    through debugcap.traced_get (the caller logs + skips a series that
    raises). The api_key travels in the query string; the trace masks
    it."""
    params = {
        "series_id": series_id,
        "api_key": api_key,
        "file_type": "json",
        "observation_start": start.isoformat(),
        "observation_end": end.isoformat(),
    }
    url = f"{base_url}?{urllib.parse.urlencode(params)}"
    body = debugcap.traced_get(url, user_agent="wealthdb-fred/1",
                               timeout=timeout, trace=trace)
    return json.loads(body.decode("utf-8"))


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
    except OSError as e:
        # URLError, TimeoutError and the socket/TLS errors.
        log.error("probe could not reach FRED: %s", e)
        return 1
    except ValueError as e:
        log.error("unexpected FRED response (not JSON): %s", e)
        return 1
    if not isinstance(doc, dict) or "observations" not in doc:
        log.error("unexpected FRED response (no observations): %s",
                  sorted(doc)[:5] if isinstance(doc, dict)
                  else type(doc).__name__)
        return 1
    log.info("credential accepted — FRED answered the %s probe", probe_series)
    return 0


def _describe(sid: str) -> str:
    """One series' meaning for the log: "CHF→USD" for an FX series,
    "USD 10Y constant_maturity" for a rates series."""
    if sid in FX_SERIES:
        return "{}→{}".format(*FX_SERIES[sid])
    return " ".join(RATE_SERIES[sid])


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)

    # A direct `download.py` run needs no wrapper: the named file is
    # sourced through bash, as the wrapper sources it, and its values win.
    envfile.source_named_env_file(args.env_file, "FRED_ENV_FILE")
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
        unknown = [s for s in series_ids if s not in SERIES]
        if unknown:
            raise SystemExit(
                f"unknown series {unknown}: add them to FX_SERIES "
                f"(series_id -> base,quote) or RATE_SERIES "
                f"(series_id -> currency,tenor,basis) in download.py first."
            )
    else:
        series_ids = list(SERIES)

    log.info("FRED download: %s … %s, %d series", since, until,
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

    # One trace for the whole fetch loop. A dry-run creates no run dir, so
    # there is nowhere for a capture to land: HttpTrace(None) makes that an
    # explicit no-op instead of a write against a directory that isn't there.
    trace = debugcap.HttpTrace(run, log=log, enabled=args.debug)

    total = failures = 0
    for sid in series_ids:
        try:
            doc = fetch_observations(args.base_url, sid, api_key, since, until,
                                     trace=trace)
        except urllib.error.HTTPError as e:
            body = (e.read() or b"")[:200].decode("utf-8", "replace")
            log.error("series %s: HTTP %s %s — skipping", sid, e.code, body)
            failures += 1
            continue
        except Exception as e:  # noqa: BLE001 - one bad series must not abort
            log.error("series %s fetch failed: %s — skipping", sid, e)
            failures += 1
            continue
        n = len(dated_observations(doc))
        total += n
        manifest["series"][sid] = {**SERIES[sid], "rows": n}
        log.info("series %s (%s): %d dated observations", sid,
                 _describe(sid), n)
        if not args.dry_run:
            bronze.atomic_write_json(run / f"{sid}.json", doc)

    # A run with zero usable series is a failure worth a non-zero exit, a
    # dry run included.
    rc = 0 if manifest["series"] else 5
    if args.dry_run:
        log.info("dry run: %d observations across %d series, %d failures "
                 "(nothing written)", total, len(manifest["series"]), failures)
        return rc

    manifest["status"] = "complete"
    bronze.atomic_write_json(run / "run.json", manifest)
    log.info("wrote %d series (%d observations, %d failures) to %s",
             len(manifest["series"]), total, failures, run)
    return rc


if __name__ == "__main__":
    sys.exit(main())
