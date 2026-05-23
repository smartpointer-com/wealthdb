#!/usr/bin/env python3
"""
Trigger a scrape against an already-running `login` keep-alive
session.

The `login` subcommand holds a CLI-MFA'd Firefox open and polls
a trigger file (`/data/.download-trigger` by default). This
script writes that file with one scrape's config and exits;
the login process picks it up, runs download.walk() against
its live `page`, and goes back to polling.

If no `login` is running, the trigger file just sits on disk
until one starts.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

import landmarks as schwab


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--trigger", default="/data/.download-trigger", type=Path,
        help=("Trigger file the login process polls. Default "
              "%(default)s — matches the path the wrapper's "
              "`login` subcommand passes via --download-trigger."),
    )
    p.add_argument(
        "--dest", default="/data", type=Path,
        help=("Bronze tree root. Default %(default)s."),
    )
    p.add_argument(
        "--mode", choices=("statements", "transactions", "both"),
        default="both",
        help="Scrape mode (default: %(default)s).",
    )
    p.add_argument(
        "--range", dest="date_range",
        choices=tuple(v for v in schwab.DATE_RANGE_VALUES if v != "Custom"),
        default=schwab.DATE_RANGE_DEFAULT,
        help=("Date-range preset for the Statements filter "
              "(default: %(default)s). Pass Last10Years for a "
              "full backfill."),
    )
    p.add_argument(
        "--dry-run", action="store_true",
        help=("Walk pages and enumerate accounts but do NOT click "
              "PDF download buttons. Useful for selector checks."),
    )
    p.add_argument(
        "--with-more-detail", action="store_true",
        help=("On the tx-history pass, also click each row's "
              "'More' link and capture the per-row detail modal "
              "into a sidecar `more-details.json`. Adds ~1 click "
              "per transaction — slow but rich."),
    )
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    config_lines = [
        f"dest={args.dest}",
        f"mode={args.mode}",
        f"date_range={args.date_range}",
        f"dry_run={'true' if args.dry_run else 'false'}",
        f"with_more_detail={'true' if args.with_more_detail else 'false'}",
    ]
    args.trigger.parent.mkdir(parents=True, exist_ok=True)
    args.trigger.write_text("\n".join(config_lines) + "\n", encoding="utf-8")
    print(f"download triggered ({args.trigger}):", file=sys.stderr)
    for line in config_lines:
        print(f"  {line}", file=sys.stderr)
    print(
        "  (the running `login` process will pick this up; bronze "
        "will land under "
        f"{args.dest}/<UTC-ts>/.)",
        file=sys.stderr,
    )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
