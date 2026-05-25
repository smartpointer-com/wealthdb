#!/usr/bin/env python3
"""
fidelity-web-dump download-trigger writer.

In-container fallback for `./fidelity-web-dump download`. The host
wrapper normally handles `download` itself by writing the trigger
file directly (no docker spawn) — this script exists as the
in-container path for the case where someone invokes the
container's entrypoint directly without the wrapper, or runs
download from inside an existing container shell.

Reads CLI flags, writes KEY=VALUE pairs to --trigger-file, exits.
Does NOT do any scraping — the running login.py keep-alive loop
inside the same /data mount picks up the trigger and runs the
scrape against its live Camoufox context.
"""

from __future__ import annotations

import argparse
import logging
import sys
from pathlib import Path


log = logging.getLogger("fidelity-web-dump.download-trigger")


def parse_args(argv):
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--trigger-file", type=Path,
        default=Path("/data/.download-trigger"),
        help=("Path to write the trigger file. Default: "
              "/data/.download-trigger (the path the login.py "
              "keep-alive loop polls)."),
    )
    p.add_argument(
        "--dest", default="/data",
        help=("Bronze root inside the container. Default: /data. "
              "Each trigger spawns a fresh <dest>/<UTC-ts>/ subdir."),
    )
    p.add_argument(
        "--mode", default="all",
        help=("Subset to scrape. Default: all. Other values once "
              "implemented: positions, transactions, documents."),
    )
    p.add_argument(
        "--since", default=None,
        help=("Earliest date (YYYY-MM-DD) for windowed surfaces "
              "(transactions, documents). Default: today - 90d."),
    )
    p.add_argument(
        "--until", default=None,
        help=("Latest date (YYYY-MM-DD). Default: today UTC."),
    )
    p.add_argument(
        "--exclude-accounts", default=None,
        help=("Comma-separated account ids to skip (e.g. the "
              "Fidelity Charitable DAF). Default: empty. The login "
              "container also sources defaults from "
              "/secrets/fidelity-web.env if FIDELITY_WEB_EXCLUDE_"
              "ACCOUNTS is set there."),
    )
    p.add_argument(
        "--dry-run", action="store_true",
        help=("Walk the UI / hit the export buttons in dry-run "
              "mode — confirm landmarks but do not save artefacts."),
    )
    p.add_argument(
        "-v", "--verbose", action="store_true",
        help="DEBUG-level logging on the trigger-writer side.",
    )
    return p.parse_args(argv)


def write_trigger(args):
    args.trigger_file.parent.mkdir(parents=True, exist_ok=True)
    lines = [
        f"dest={args.dest}",
        f"mode={args.mode}",
        f"dry_run={'true' if args.dry_run else 'false'}",
    ]
    if args.since is not None:
        lines.append(f"since={args.since}")
    if args.until is not None:
        lines.append(f"until={args.until}")
    if args.exclude_accounts is not None:
        lines.append(f"exclude_accounts={args.exclude_accounts}")
    body = "\n".join(lines) + "\n"
    args.trigger_file.write_text(body)
    log.info("wrote trigger %s:", args.trigger_file)
    for line in lines:
        log.info("  %s", line)
    log.info(
        "the running login keep-alive loop will pick this up on "
        "its next ~2s poll; bronze lands under %s/<UTC-ts>/.",
        args.dest,
    )


def main(argv):
    args = parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    write_trigger(args)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
