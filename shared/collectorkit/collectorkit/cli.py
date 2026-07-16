"""Shared CLI helpers: logging format, common flags, and the unified
date-window contract every collector exposes.

There is exactly ONE window flag, `--lookback`, and it takes either a
named preset (`4w`, `1y`, `all`, …) or an ISO date (`2020-01-01`). It
names a single starting point; the window always runs from there to
today, and everything the source offers inside it — transactions,
documents, snapshots — is fetched. There is no upper-bound flag and no
per-facet window: a collector either covers the window or says so.

`add_standard_args(verb="download")` adds the flag; `resolve_lookback`
turns it into a concrete `(since, until)` pair."""
from __future__ import annotations

import argparse
import logging
import os
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

LOG_FORMAT = "%(asctime)s %(levelname)s %(name)s: %(message)s"


def default_data_root() -> Path:
    """The default wealthdb data root for host-side collectors when no
    --data-dir / --bronze-dir / --silver-db flag is given: the XDG data dir
    ($XDG_DATA_HOME/wealthdb, falling back to ~/.local/share/wealthdb per
    the XDG Base Directory spec). The wrappers normally pass an explicit
    path (which also honours WEALTHDB_DATA_ROOT); this is the bare
    fallback for direct script invocation. Append the collector name,
    e.g. ``cli.default_data_root() / "schwab-api"``.
    """
    xdg = os.environ.get("XDG_DATA_HOME", "").strip()
    base = Path(xdg) if xdg else Path.home() / ".local" / "share"
    return base / "wealthdb"

# The convention for "fetch the recent N days" everywhere. 90 days is
# narrow enough that a forgotten flag doesn't silently trigger a
# multi-year backfill; a fleet-wide --lookback widens uniformly.
DEFAULT_LOOKBACK_DAYS = 90

# Named lookback shortcuts. Value is the number of days from today.
# "all" is ~30 years rather than a literal "no floor" sentinel: it's
# wider than any realistic Swiss / US retail brokerage history, and
# keeps callers using plain ``date`` arithmetic instead of having to
# special-case a date.min throughout the loader code.
LOOKBACK_PRESETS: dict[str, int] = {
    "1w": 7,
    "4w": 28,
    "3m": 90,
    "6m": 180,
    "1y": 365,
    "2y": 730,
    "5y": 1825,
    "all": 365 * 30,
}

LOOKBACK_CHOICES = list(LOOKBACK_PRESETS.keys())


def configure_logging(verbose: bool = False) -> None:
    """Set up root logging with the shared collector format."""
    logging.basicConfig(
        level=logging.DEBUG if verbose else logging.INFO,
        format=LOG_FORMAT,
    )


def add_common_args(parser: argparse.ArgumentParser) -> None:
    """Add the flags every collector CLI shares, on any verb.

    ``add_standard_args`` folds this in for the standard verbs; call it
    directly only for a collector-specific verb outside that set (``explore``,
    cointracking's ``fetch-prices``)."""
    parser.add_argument(
        "-v", "--verbose", action="store_true", help="DEBUG-level logging.",
    )


def _add_force_arg(parser: argparse.ArgumentParser, *,
                   always_rebuilds: bool = False) -> None:
    """The standard ``--force`` flag for `load` commands: rebuild the
    silver DB from scratch (delete it first, then re-ingest all bronze).
    Pair with ``collectorkit.silver.reset(db_path)`` in the loader, called
    before the DB is opened when ``args.force`` is set.

    ``always_rebuilds=True`` is for a loader that has no incremental path —
    every run already deletes and rebuilds, so ``--force`` cannot change the
    outcome. The flag still parses (the fleet passes it everywhere); only the
    help text differs, the same way ``full_history`` handles a ``--lookback``
    that cannot narrow.
    """
    if always_rebuilds:
        help_text = ("Accepted for fleet uniformity; this collector always "
                     "rebuilds the silver DB from bronze (delete + full "
                     "rebuild), so --force is a documented no-op.")
    else:
        help_text = ("Rebuild the silver DB from scratch: delete it, then "
                     "re-ingest all bronze.")
    parser.add_argument("--force", action="store_true", help=help_text)


LOOKBACK_METAVAR = "PRESET|YYYY-MM-DD"


def lookback_value(raw: str) -> str:
    """argparse ``type`` for ``--lookback``: a named preset or an ISO date.

    Returns the raw string unchanged — :func:`resolve_lookback` turns it
    into a date. Keeping the raw form on the Namespace means a warning can
    echo what was actually typed (``--lookback 4w``, not the date it
    resolved to), and it defers "what is today" to resolve time.
    """
    if raw in LOOKBACK_PRESETS:
        return raw
    try:
        date.fromisoformat(raw)
    except ValueError:
        raise argparse.ArgumentTypeError(
            f"{raw!r} is neither a named preset "
            f"({', '.join(LOOKBACK_CHOICES)}) nor an ISO date (YYYY-MM-DD)"
        ) from None
    return raw


def _add_lookback_arg(parser: argparse.ArgumentParser) -> None:
    """The one date-window flag, for a collector that can bound its fetch. A
    full-history collector gets :func:`_add_full_download_lookback_arg`
    instead — same flag, help text that says it cannot narrow. Reached via
    ``add_standard_args(verb="download")``.

    The default is deliberately handled in :func:`resolve_lookback` rather
    than as an argparse ``default=``, so a collector can still tell "user
    passed nothing" apart from "user passed the default".
    """
    parser.add_argument(
        "--lookback", type=lookback_value, default=None,
        metavar=LOOKBACK_METAVAR,
        help=("How far back to fetch: a named preset "
              f"({', '.join(LOOKBACK_CHOICES)}) or an ISO date "
              "(YYYY-MM-DD). The window runs from there to today and "
              "covers everything the source offers in it — transactions, "
              "documents, snapshots alike. 'all' reaches back 30 years. "
              f"Default: the last {DEFAULT_LOOKBACK_DAYS} days."),
    )


def _today_utc() -> date:
    return datetime.now(timezone.utc).date()


def lookback_start(value: str, *, today: date) -> date:
    """The concrete start date a validated ``--lookback`` value names."""
    if value in LOOKBACK_PRESETS:
        return today - timedelta(days=LOOKBACK_PRESETS[value])
    return date.fromisoformat(value)


def resolve_lookback(args: argparse.Namespace, *,
                     default_days: int = DEFAULT_LOOKBACK_DAYS,
                     ) -> tuple[date, date]:
    """Translate ``--lookback`` into the concrete ``(since, until)`` window.

    ``until`` is always today (UTC): the window runs from the requested
    starting point to now, and there is no flag to pull it back. ``since``
    is the ``--lookback`` value resolved against today, or
    ``today - default_days`` when the flag is absent.

    ``--lookback all`` evaluates to ``today − 30 years`` (wider than any
    realistic Swiss / US retail brokerage history) so loaders never have to
    deal with a sentinel ``date.min``.
    """
    today = _today_utc()
    if args.lookback is None:
        return today - timedelta(days=default_days), today
    since = lookback_start(args.lookback, today=today)
    if since > today:
        raise SystemExit(
            f"--lookback {args.lookback} starts after today ({since} > {today})"
        )
    return since, today


def _add_full_download_lookback_arg(parser: argparse.ArgumentParser) -> None:
    """``--lookback`` on a collector that always fetches its full
    history and structurally cannot honour a narrower window.

    A fleet orchestrator forwards ``--lookback`` to every collector's
    ``download``; the ones that can bound their fetch get
    :func:`_add_lookback_arg` + :func:`resolve_lookback`. The ones that
    always pull everything — a passive SPA capture with no server-side date
    filter, a full-history export whose downstream replay needs every row,
    an SFTP drop of whatever the server has queued — use this instead.
    ``--lookback`` is a *lower bound* ("fetch at least this far back"), and
    a full download trivially satisfies any window, so the flag is
    validated (for a clean error on a typo) but only drives a warning via
    :func:`warn_lookback_ignored`; the download is unaffected.
    """
    parser.add_argument(
        "--lookback", type=lookback_value, default=None,
        metavar=LOOKBACK_METAVAR,
        help=("Accepted for fleet uniformity. This collector "
              "always downloads its full history (a superset of any "
              "window); --lookback cannot narrow that, so it is logged "
              "and otherwise ignored."),
    )


def warn_lookback_ignored(lookback: str | None, log: logging.Logger, *,
                          what: str = "its full history") -> None:
    """Warn that a full-download collector (see
    :func:`add_standard_args` with ``full_history=True``) cannot narrow to the
    requested
    ``--lookback``. No-op when ``lookback`` is None. Worded to hold on every
    path — a real download, a ``--dry-run`` walk, or a session probe — since
    it states the collector's nature, not that bytes were fetched this run."""
    if lookback:
        log.warning(
            "--lookback %s cannot narrow this collector — it always fetches "
            "%s (a superset of any window); the flag has no effect.",
            lookback, what)


# ----------------------------------------------------------------------
# The standard argument group
# ----------------------------------------------------------------------
#
# ``add_standard_args`` is the single entry point every collector entry
# script wires in. Defining the standard set in one place makes uniform
# acceptance *structural* — a standard optional flag parses cleanly on every
# collector, it never triggers an argparse "unrecognized arguments" exit 2 —
# rather than a matter of per-collector discipline. It folds the per-concept
# adders above so a collector never wires the standard flags by hand. Only
# ``add_common_args`` stays public, for the collector-specific verbs outside
# STANDARD_VERBS (``explore``, cointracking's ``fetch-prices``).

STANDARD_VERBS = ("login", "download", "load", "prune")


def add_standard_args(parser: argparse.ArgumentParser, *, verb: str,
                      full_history: bool = False,
                      always_rebuilds: bool = False) -> None:
    """Add the standard optional flags that ``verb`` owns to ``parser``.

    Every entry script calls this once so the standard vocabulary is
    accepted uniformly. The set per verb:

      ``download``  ``-v/--verbose`` + ``--lookback``. A full-history
                    collector (``full_history=True``) gets the *same* flag —
                    it parses cleanly everywhere — with help text saying it
                    cannot narrow; :func:`resolve_standard` emits the runtime
                    warning. Pass the same ``full_history`` to both.
      ``load``      ``-v/--verbose`` + ``--force`` (delete the silver DB,
                    then rebuild it from all bronze). ``always_rebuilds=True``
                    marks a loader with no incremental path, where ``--force``
                    parses but cannot change the outcome.
      ``login``     ``-v/--verbose``.
      ``prune``     ``-v/--verbose`` (the shared prune parser adds its own
                    ``--bronze-dir`` / ``--dry-run`` / ``--min-age-hours``).
    """
    if verb not in STANDARD_VERBS:
        raise ValueError(f"unknown standard verb: {verb!r}")
    add_common_args(parser)
    if verb == "download":
        if full_history:
            _add_full_download_lookback_arg(parser)
        else:
            _add_lookback_arg(parser)
    elif verb == "load":
        _add_force_arg(parser, always_rebuilds=always_rebuilds)


def resolve_standard(args: argparse.Namespace, *, verb: str,
                     full_history: bool = False,
                     log: logging.Logger | None = None,
                     what: str = "its full history",
                     ) -> tuple[date | None, date | None]:
    """Resolve the standard ``download`` window added by
    :func:`add_standard_args`.

    Returns ``(since, until)``.

    - **Bounded** (``full_history=False``): delegates to
      :func:`resolve_lookback`.
    - **Full history** (``full_history=True``): the flag was accepted for
      fleet uniformity but cannot narrow the fetch. Returns
      ``(None, None)`` and, given a ``log``, warns via
      :func:`warn_lookback_ignored` — neither a silent ignore nor an
      argparse reject.

    For any verb other than ``download`` this is a no-op returning
    ``(None, None)`` (a ``load`` / ``login`` / ``prune`` parser has no
    window).
    """
    if verb != "download":
        return None, None
    if not full_history:
        return resolve_lookback(args)
    if log is not None:
        warn_lookback_ignored(args.lookback, log, what=what)
    return None, None
