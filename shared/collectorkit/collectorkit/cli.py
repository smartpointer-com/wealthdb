"""Shared CLI helpers: logging format, common flags, and the unified
date-window contract every bounded-data collector exposes.

Every collector that has any kind of date-bounded surface accepts the
same vocabulary — `--since`, `--until`, `--lookback`, plus the
`--documents-{since,until}` pair for collectors that scrape a document
archive separately from transactions. `add_lookback_args` adds the
flags; `resolve_lookback` returns the concrete `(since, until,
documents_since, documents_until)` dates with consistent defaulting
and precedence."""
from __future__ import annotations

import argparse
import logging
import os
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

LOG_FORMAT = "%(asctime)s %(levelname)s %(name)s: %(message)s"


def default_data_root() -> Path:
    """The default wealthdb data root for host-side collectors when no
    --data-dir / --dest / --silver-db flag is given: the XDG data dir
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
# multi-year backfill; wealthdb-refresh --lookback widens uniformly.
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
    """Add the flags every collector CLI shares."""
    parser.add_argument(
        "-v", "--verbose", action="store_true", help="DEBUG-level logging.",
    )


def add_force_arg(parser: argparse.ArgumentParser) -> None:
    """Add the standard ``--force`` flag for `load` commands: rebuild the
    silver DB from scratch (delete it first, then re-ingest all bronze).
    Pair with ``collectorkit.silver.reset(db_path)`` in the loader, called
    before the DB is opened when ``args.force`` is set."""
    parser.add_argument(
        "--force", action="store_true",
        help="Rebuild the silver DB from scratch: delete it, then "
             "re-ingest all bronze.",
    )


def add_lookback_args(parser: argparse.ArgumentParser, *,
                      has_documents: bool = True) -> None:
    """Add the unified date-window flags.

    Adds:
      --since YYYY-MM-DD          earliest transaction/activity date
      --until YYYY-MM-DD          latest (default: today UTC)
      --lookback {3m,6m,1y,2y,5y,all}
                                   named window shortcut

    With ``has_documents=True``, also adds:
      --documents-since YYYY-MM-DD  (default: same as --since)
      --documents-until YYYY-MM-DD  (default: same as --until)

    Defaults are deliberately handled in ``resolve_lookback`` rather
    than as argparse ``default=``: each collector that wraps these
    flags through to wealthdb-refresh needs to be able to detect
    "user passed nothing" so the shortcut precedence stays sane.
    """
    parser.add_argument(
        "--since", type=date.fromisoformat, default=None,
        help=("Earliest transaction/activity date (YYYY-MM-DD). "
              f"Default: {DEFAULT_LOOKBACK_DAYS} days before --until. "
              "For a one-off backfill pass an older date explicitly; "
              "--lookback offers named shortcuts."),
    )
    parser.add_argument(
        "--until", type=date.fromisoformat, default=None,
        help=("Latest transaction/activity date (YYYY-MM-DD, "
              "inclusive). Default: today UTC."),
    )
    parser.add_argument(
        "--lookback", choices=LOOKBACK_CHOICES, default=None,
        help=("Named window shortcut. Sets --since (and "
              "--documents-since if applicable) to today - X. "
              "'all' lifts the lower bound entirely. An explicit "
              "--since wins over --lookback."),
    )
    if has_documents:
        parser.add_argument(
            "--documents-since", type=date.fromisoformat, default=None,
            help=("Earliest document date (YYYY-MM-DD). Default: "
                  "same as --since."),
        )
        parser.add_argument(
            "--documents-until", type=date.fromisoformat, default=None,
            help=("Latest document date (YYYY-MM-DD, inclusive). "
                  "Default: same as --until."),
        )


def _today_utc() -> date:
    return datetime.now(timezone.utc).date()


def resolve_lookback(args: argparse.Namespace, *,
                     default_days: int = DEFAULT_LOOKBACK_DAYS,
                     has_documents: bool = True,
                     ) -> tuple[date, date, date | None, date | None]:
    """Translate the date-window args added by :func:`add_lookback_args`
    into concrete dates.

    Returns ``(since, until, documents_since, documents_until)``. With
    ``has_documents=False`` the last two are ``None``.

    Precedence for the transaction window:
      1. explicit ``--since`` wins
      2. ``--lookback`` shortcut
      3. ``until - default_days``

    Documents inherit ``--since`` unless ``--documents-since`` is set
    explicitly. ``--lookback all`` evaluates to ``until − 30 years``
    (wider than any realistic Swiss / US retail brokerage history)
    so loaders never have to deal with a sentinel ``date.min``.
    """
    today = _today_utc()
    until = args.until or today

    if args.since is not None:
        since: date = args.since
    elif args.lookback is not None:
        since = until - timedelta(days=LOOKBACK_PRESETS[args.lookback])
    else:
        since = until - timedelta(days=default_days)

    if since > until:
        raise SystemExit(f"--since {since} is after --until {until}")

    if not has_documents:
        return since, until, None, None

    documents_until = args.documents_until or until
    if args.documents_since is not None:
        documents_since: date = args.documents_since
    elif args.lookback is not None:
        documents_since = documents_until - timedelta(
            days=LOOKBACK_PRESETS[args.lookback])
    else:
        documents_since = since

    if documents_since > documents_until:
        raise SystemExit(
            f"--documents-since {documents_since} is after "
            f"--documents-until {documents_until}"
        )

    return since, until, documents_since, documents_until
